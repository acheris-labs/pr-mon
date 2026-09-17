// Command gen-fixtures writes protocol-fixtures/: sample protocol messages that
// both the Go tests and the Swift client's tests read, so the two implementations
// are checked against the same bytes. Run it with `make fixtures`.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/acheris-labs/pr-mon/internal/actions"
	"github.com/acheris-labs/pr-mon/internal/config"
	"github.com/acheris-labs/pr-mon/internal/daemon"
	"github.com/acheris-labs/pr-mon/internal/models"
	"github.com/acheris-labs/pr-mon/internal/notify"
	"github.com/acheris-labs/pr-mon/internal/protocol"
	"github.com/acheris-labs/pr-mon/internal/readiness"
	"github.com/acheris-labs/pr-mon/internal/service"
	"github.com/acheris-labs/pr-mon/internal/testfixtures"
)

const (
	uid          = 501
	checkStarted = "2026-09-15T01:30:00Z"
	armedAt      = "2026-09-17T10:00:00+00:00"
	lastUpdate   = "2026-09-17T12:00:00+00:00"
)

var armed = models.ArmedMerge{Method: models.MergeSquash, DeleteBranch: true, ArmedAt: armedAt}

// state is a backend in a representative state: every status, an armed merge,
// a repo that failed to load, and GitHub's own auto-merge.
func state() protocol.Snapshot {
	api := testfixtures.Repo("acme/api", []models.PullRequest{
		pr(1, func(p *models.PullRequest) { p.Title = "Ready to go 🚀" }),
		pr(2, func(p *models.PullRequest) {
			testfixtures.Pending(p)
			p.Checks = []models.Check{{Name: "ci", State: "PENDING", StartedAt: models.Ptr(checkStarted)}}
			p.ChecksTotal = 1
		}),
		pr(3, func(p *models.PullRequest) {
			testfixtures.Failing(p)
			p.Checks = []models.Check{
				{Name: "lint", State: "FAILURE", StartedAt: models.Ptr(checkStarted)},
				{Name: "deploy", State: "SUCCESS", StartedAt: models.Ptr("2026-09-15T01:00:00Z")},
			}
			p.ChecksTotal = 5
		}),
		pr(4, testfixtures.Conflict),
		pr(5, func(p *models.PullRequest) {
			testfixtures.Behind(p)
			p.ReviewDecision = models.Ptr("REVIEW_REQUIRED")
		}),
		pr(6, func(p *models.PullRequest) {
			testfixtures.Draft(p)
			p.Title = "WIP with a line separator"
		}),
		pr(7, func(p *models.PullRequest) {
			p.Mergeable = "UNKNOWN"
			p.MergeState = "UNKNOWN"
			p.CheckState = nil
		}),
		pr(8, func(p *models.PullRequest) {
			testfixtures.Pending(p)
			p.AutoMerge = &models.AutoMerge{Method: models.MergeRebase, EnabledBy: "carol"}
			p.HeadRefID = nil
		}),
	}, func(repo *models.Repo) { repo.PRTotal = 60 })

	rich := testfixtures.Repo("Textualize/rich", []models.PullRequest{
		pr(41, testfixtures.Pending),
		pr(42, testfixtures.Pending),
	}, func(repo *models.Repo) {
		repo.MergeMethods = []models.MergeMethod{models.MergeSquash}
		repo.DeleteBranchOnMerge = true
		repo.AutoMergeAllowed = false
	})

	settings := config.NewNotifyConfig()
	settings.ScriptEnabled = true
	settings.Script = "~/bin/im"

	pid := 4242
	return protocol.Snapshot{
		Config: config.Config{
			Repos:         []string{"acme/api", "acme/web", "Textualize/rich"},
			PollInterval:  60,
			Notifications: map[string]config.NotifyConfig{"acme/api": settings},
		},
		Repos: map[string]models.Repo{
			"acme/api":        actions.WithActions(api, nil),
			"Textualize/rich": actions.WithActions(rich, map[int]models.ArmedMerge{42: armed}),
		},
		Errors:    map[string]string{"acme/web": "Repository acme/web not found"},
		Unseen:    map[string][]int{"acme/api": {1, 5}, "acme/web": {}, "Textualize/rich": {}},
		Collapsed: []string{"textualize"},
		Armed: map[string]protocol.ArmedByNumber{
			"acme/api": {}, "acme/web": {}, "Textualize/rich": {"42": armed},
		},
		Status: service.Status{
			Connected:  true,
			PID:        &pid,
			Version:    "0.0.0-fixture",
			Notifier:   models.Ptr("/opt/homebrew/bin/terminal-notifier"),
			LastUpdate: models.Ptr(lastUpdate),
			Warnings:   []string{"Ignoring unreadable config: example"},
		},
	}
}

func pr(number int, mutate ...func(*models.PullRequest)) models.PullRequest {
	titled := append([]func(*models.PullRequest){
		func(p *models.PullRequest) { p.Title = fmt.Sprintf("PR %d", number) },
	}, mutate...)
	return readiness.Assess(testfixtures.PR(number, titled...))
}

// files is every fixture: name to content.
func files() map[string]any {
	snapshot := state()
	api := snapshot.Repos["acme/api"]
	repoUpdate := protocol.RepoUpdate{
		Name:   "acme/api",
		Repo:   &api,
		Unseen: snapshot.Unseen["acme/api"],
		Armed:  snapshot.Armed["acme/api"],
	}
	pid := *snapshot.Status.PID
	squash, merge := models.MergeSquash, models.MergeCommit
	fixtures := map[string]any{
		"hello-response.json": protocol.Response{ID: 1, OK: true, Result: protocol.Hello{
			Protocol: protocol.Version,
			Version:  snapshot.Status.Version,
			PID:      &pid,
			Notifier: snapshot.Status.Notifier,
		}},
		"snapshot-response.json": protocol.Response{ID: 2, OK: true, Result: snapshot},
		"error-response.json": protocol.Response{
			ID: 3, OK: false, Error: "acme/api#99 is not an open PR"},
		"notification-form-response.json": protocol.Response{
			ID: 4, OK: true, Result: notify.NotificationForm()},
		"preview-notification-response.json": protocol.Response{ID: 5, OK: true,
			Result: notify.PreviewMessage("acme/api", "{{PR_REPO}}#{{PR_NUM}} {{PR_OOPS}}")},
		"event-repos.json":     protocol.EventMessage{Event: "repos", Data: snapshot},
		"event-repo.json":      protocol.EventMessage{Event: "repo", Data: repoUpdate},
		"event-seen.json":      protocol.EventMessage{Event: "seen", Data: protocol.SeenUpdate{Name: "acme/api", Unseen: snapshot.Unseen["acme/api"]}},
		"event-collapsed.json": protocol.EventMessage{Event: "collapsed", Data: protocol.CollapsedUpdate{Collapsed: snapshot.Collapsed}},
		"event-config.json":    protocol.EventMessage{Event: "config", Data: protocol.ConfigUpdate{Config: snapshot.Config}},
		"event-status.json":    protocol.EventMessage{Event: "status", Data: protocol.StatusUpdate{Status: snapshot.Status}},
		"event-toast.json": protocol.EventMessage{Event: "toast", Data: protocol.Toast{
			Message: "Merged acme/api#1 (squash)", Severity: "information"}},
		"requests.json":     requests(squash, merge),
		"socket-paths.json": socketPaths(),
	}
	return fixtures
}

func requests(squash, merge models.MergeMethod) []map[string]any {
	perform := func(id int, action models.Action) map[string]any {
		return map[string]any{"id": id, "op": "perform",
			"args": map[string]any{"repo": "acme/api", "number": 1, "action": action}}
	}
	settings := config.NewNotifyConfig()
	withScript := settings
	withScript.Script = "im"
	return []map[string]any{
		{"id": 1, "op": "hello", "args": map[string]any{}},
		{"id": 2, "op": "snapshot", "args": map[string]any{}},
		{"id": 3, "op": "mark_seen", "args": map[string]any{"name": "acme/api", "number": 1}},
		{"id": 4, "op": "refresh_all", "args": map[string]any{}},
		perform(5, models.Action{Kind: "merge", Method: &squash, DeleteBranch: true}),
		perform(6, models.Action{Kind: "arm_merge", Method: &merge}),
		perform(7, models.Action{Kind: "disarm_merge"}),
		perform(8, models.Action{Kind: "update"}),
		{"id": 9, "op": "add_repo", "args": map[string]any{"name": "acme/web"}},
		{"id": 10, "op": "remove_repo", "args": map[string]any{"name": "acme/web"}},
		{"id": 11, "op": "set_collapsed", "args": map[string]any{"owner": "acme", "collapsed": true}},
		{"id": 12, "op": "save_notifications",
			"args": map[string]any{"repo": "acme/api", "settings": withScript}},
		{"id": 13, "op": "send_test",
			"args": map[string]any{"repo": "acme/api", "settings": settings}},
		{"id": 14, "op": "notification_form", "args": map[string]any{}},
		{"id": 15, "op": "preview_notification",
			"args": map[string]any{"repo": "acme/api", "message": "{{PR_NUM}}"}},
		{"id": 16, "op": "set_poll_interval", "args": map[string]any{"seconds": 120}},
	}
}

// socketPaths shows both socket locations, including the /tmp fallback.
func socketPaths() map[string]any {
	cases := []map[string]string{}
	for _, directory := range []string{
		"/Users/alice/.local/state/pr-mon",
		"/Users/alice/very-long-directory-name/very-long-directory-name/" +
			"very-long-directory-name/very-long-directory-name/pr-mon",
	} {
		cases = append(cases, map[string]string{
			"state_directory": directory,
			"socket":          socketFor(directory),
		})
	}
	return map[string]any{"protocol": protocol.Version, "uid": uid, "cases": cases}
}

// socketFor computes a path as the daemon would for the fixture's uid.
func socketFor(directory string) string {
	path := daemon.Paths{Directory: directory}.Socket()
	if filepath.Dir(path) == directory {
		return path
	}
	// The fallback carries this process's uid; the fixture states its own.
	return filepath.Join("/tmp", "pr-mon-"+strconv.Itoa(uid), filepath.Base(path))
}

func main() {
	root, err := os.Getwd()
	if err != nil {
		fail(err)
	}
	directory := filepath.Join(root, "protocol-fixtures")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		fail(err)
	}
	written := map[string]bool{}
	for name, content := range files() {
		text, err := json.MarshalIndent(content, "", "  ")
		if err != nil {
			fail(err)
		}
		if err := os.WriteFile(filepath.Join(directory, name), append(text, '\n'), 0o644); err != nil {
			fail(err)
		}
		written[name] = true
	}
	stale, err := filepath.Glob(filepath.Join(directory, "*.json"))
	if err != nil {
		fail(err)
	}
	for _, path := range stale {
		if !written[filepath.Base(path)] {
			os.Remove(path)
		}
	}
	fmt.Printf("wrote %d fixtures to %s\n", len(written), directory)
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "gen-fixtures: %v\n", err)
	os.Exit(1)
}
