package service_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/acheris-labs/pr-mon/internal/config"
	"github.com/acheris-labs/pr-mon/internal/github"
	"github.com/acheris-labs/pr-mon/internal/models"
	"github.com/acheris-labs/pr-mon/internal/notify"
	"github.com/acheris-labs/pr-mon/internal/readiness"
	"github.com/acheris-labs/pr-mon/internal/service"
	"github.com/acheris-labs/pr-mon/internal/testfixtures"
)

// fakeGitHub stands in for the API: it replays repos and records calls.
type fakeGitHub struct {
	mutex       sync.Mutex
	repos       map[string]models.Repo
	fetchErr    map[string]error
	calls       []string
	mergeHeads  []string
	mergeErrs   []error // consumed in order, then mergeErr
	mergeErr    error
	deleteErr   error
	mergeGate   chan struct{}
	fetchCounts map[string]int
	// States of PRs outside the fake repos, by "owner/repo#n"; a PR in a fake
	// repo is open. Anything else is not found.
	states map[string]string
	// What the conditional requests answer: the repo's current ETags, and each
	// PR's updated_at.
	listETags map[string]string
	baseETags map[string]string
	updatedAt map[string]string
}

func newFakeGitHub(repos ...models.Repo) *fakeGitHub {
	fake := &fakeGitHub{
		repos:       map[string]models.Repo{},
		fetchErr:    map[string]error{},
		fetchCounts: map[string]int{},
		states:      map[string]string{},
		listETags:   map[string]string{},
		baseETags:   map[string]string{},
		updatedAt:   map[string]string{},
	}
	for _, repo := range repos {
		fake.repos[repo.Name] = repo
	}
	return fake
}

func (f *fakeGitHub) setRepo(repo models.Repo) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.repos[repo.Name] = repo
}

func (f *fakeGitHub) record(call string) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.calls = append(f.calls, call)
}

func (f *fakeGitHub) callsMatching(prefix string) []string {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	found := []string{}
	for _, call := range f.calls {
		if len(call) >= len(prefix) && call[:len(prefix)] == prefix {
			found = append(found, call)
		}
	}
	return found
}

func (f *fakeGitHub) FetchRepo(_ context.Context, name string) (models.Repo, error) {
	f.mutex.Lock()
	f.calls = append(f.calls, "fetch "+name)
	f.fetchCounts[name]++
	err := f.fetchErr[name]
	repo, found := f.repos[name]
	f.mutex.Unlock()
	if err != nil {
		return models.Repo{}, err
	}
	if !found {
		return models.Repo{}, &github.NotFoundError{Message: "Repository " + name + " not found"}
	}
	return repo, nil
}

func (f *fakeGitHub) Merge(_ context.Context, prID string, method models.MergeMethod, head string) error {
	f.mutex.Lock()
	f.calls = append(f.calls, fmt.Sprintf("merge %s %s", prID, method))
	f.mergeHeads = append(f.mergeHeads, head)
	var err error
	if len(f.mergeErrs) > 0 {
		err, f.mergeErrs = f.mergeErrs[0], f.mergeErrs[1:]
	} else {
		err = f.mergeErr
	}
	gate := f.mergeGate
	f.mutex.Unlock()
	if gate != nil {
		<-gate
	}
	return err
}

func (f *fakeGitHub) UpdateBranch(_ context.Context, prID string) error {
	f.record("update " + prID)
	return nil
}

func (f *fakeGitHub) EnableAutoMerge(_ context.Context, prID string, method models.MergeMethod) error {
	f.record(fmt.Sprintf("auto_on %s %s", prID, method))
	return nil
}

func (f *fakeGitHub) DisableAutoMerge(_ context.Context, prID string) error {
	f.record("auto_off " + prID)
	return nil
}

func (f *fakeGitHub) DeleteBranch(_ context.Context, refID string) error {
	f.record("delete " + refID)
	f.mutex.Lock()
	defer f.mutex.Unlock()
	return f.deleteErr
}

// ListOpenPRs answers from the fake repo, and reports 304 when the caller's
// ETag matches the one the repo has now.
func (f *fakeGitHub) ListOpenPRs(_ context.Context, name, etag string) (github.OpenPRs, error) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.calls = append(f.calls, "list "+name)
	if err := f.fetchErr[name]; err != nil {
		return github.OpenPRs{}, err
	}
	repo, found := f.repos[name]
	if !found {
		return github.OpenPRs{}, &github.NotFoundError{Message: "Repository " + name + " not found"}
	}
	current := f.listETags[name]
	if current == "" {
		current = "etag-0"
	}
	if etag == current {
		return github.OpenPRs{Changed: false, ETag: current}, nil
	}
	listed := github.OpenPRs{Changed: true, ETag: current, Updated: map[int]string{}}
	for _, pr := range repo.PRs {
		listed.Numbers = append(listed.Numbers, pr.Number)
		stamp := f.updatedAt[models.PRKey(name, pr.Number)]
		if stamp == "" {
			stamp = testfixtures.CreatedAt
		}
		listed.Updated[pr.Number] = stamp
	}
	return listed, nil
}

func (f *fakeGitHub) BaseMoved(_ context.Context, name, etag string) (bool, string, error) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.calls = append(f.calls, "base "+name)
	current := f.baseETags[name]
	if current == "" {
		current = "base-0"
	}
	return etag != current, current, nil
}

func (f *fakeGitHub) FetchPRs(_ context.Context, name string, numbers []int) ([]models.PullRequest, error) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.calls = append(f.calls, fmt.Sprintf("fetch %s %v", name, numbers))
	wanted := map[int]bool{}
	for _, number := range numbers {
		wanted[number] = true
	}
	found := []models.PullRequest{}
	for _, pr := range f.repos[name].PRs {
		if wanted[pr.Number] {
			found = append(found, pr)
		}
	}
	return found, nil
}

// touch marks a PR as changed since the last look, the way GitHub would.
func (f *fakeGitHub) touch(repo string, number int, stamp string) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.updatedAt[models.PRKey(repo, number)] = stamp
	f.listETags[repo] = "etag-" + stamp
}

// basePushed makes the next BaseMoved report new commits on the base branch.
func (f *fakeGitHub) basePushed(repo, sha string) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.baseETags[repo] = "base-" + sha
}

func (f *fakeGitHub) setState(key, state string) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.states[key] = state
}

func (f *fakeGitHub) LookupPRs(_ context.Context, repo string, numbers []int) ([]models.PRRef, error) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.calls = append(f.calls, fmt.Sprintf("lookup %s %v", repo, numbers))
	refs := []models.PRRef{}
	for _, number := range numbers {
		key := models.PRKey(repo, number)
		ref := models.PRRef{Repo: repo, Number: number, Title: "Elsewhere",
			URL: "https://github.com/" + repo, State: f.states[key]}
		for _, pr := range f.repos[repo].PRs {
			if pr.Number == number && ref.State == "" {
				ref.Title, ref.State = pr.Title, models.PROpen
			}
		}
		if ref.State == "" {
			return nil, &github.NotFoundError{Message: key + " not found"}
		}
		refs = append(refs, ref)
	}
	return refs, nil
}

// harness is a monitor with temp files, a fake GitHub and recorded events.
type harness struct {
	t          *testing.T
	monitor    *service.Monitor
	client     *fakeGitHub
	configPath string
	statePath  string
	mutex      sync.Mutex
	events     []service.Event
	deliveries []map[string]string
}

func start(t *testing.T, client *fakeGitHub, repos []string,
	notifications map[string]config.NotifyConfig) *harness {
	t.Helper()
	return startWith(t, client, repos, notifications, nil)
}

// startWith is start with the saved config adjusted, for the poll intervals.
func startWith(t *testing.T, client *fakeGitHub, repos []string,
	notifications map[string]config.NotifyConfig, adjust func(*config.Config)) *harness {
	t.Helper()
	dir := t.TempDir()
	h := &harness{
		t:          t,
		client:     client,
		configPath: filepath.Join(dir, "config.toml"),
		statePath:  filepath.Join(dir, "state.json"),
	}
	saved := config.New()
	saved.Repos = repos
	if notifications != nil {
		saved.Notifications = notifications
	}
	if adjust != nil {
		adjust(&saved)
	}
	if err := config.Save(h.configPath, saved); err != nil {
		t.Fatal(err)
	}
	deliver := func(_ context.Context, settings config.NotifyConfig, notifier string,
		variables map[string]string) []notify.Result {
		h.mutex.Lock()
		defer h.mutex.Unlock()
		h.deliveries = append(h.deliveries, variables)
		if !settings.ScriptEnabled && !settings.DesktopEnabled {
			return nil
		}
		return []notify.Result{{Channel: "script"}}
	}
	h.monitor = service.New(client, h.configPath, h.statePath, service.Options{
		Version:  "1.2.3",
		Notifier: "/x/tn",
		Deliver:  deliver,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	h.monitor.AddListener(func(event service.Event) {
		h.mutex.Lock()
		defer h.mutex.Unlock()
		h.events = append(h.events, event)
	})
	h.monitor.Start(context.Background())
	h.monitor.WaitIdle()
	t.Cleanup(h.monitor.Stop)
	return h
}

func (h *harness) poll() {
	h.monitor.PollOnce(context.Background())
	h.monitor.WaitIdle()
}

func (h *harness) toasts() []string {
	h.mutex.Lock()
	defer h.mutex.Unlock()
	found := []string{}
	for _, event := range h.events {
		if event.Kind == "toast" {
			found = append(found, event.Message)
		}
	}
	return found
}

func (h *harness) kinds() []string {
	h.mutex.Lock()
	defer h.mutex.Unlock()
	found := []string{}
	for _, event := range h.events {
		found = append(found, event.Kind)
	}
	return found
}

func (h *harness) sent() []map[string]string {
	h.mutex.Lock()
	defer h.mutex.Unlock()
	return append([]map[string]string{}, h.deliveries...)
}

func (h *harness) hasToast(substring string) bool {
	for _, message := range h.toasts() {
		if contains(message, substring) {
			return true
		}
	}
	return false
}

func contains(text, substring string) bool {
	return len(substring) == 0 || (len(text) >= len(substring) &&
		func() bool {
			for i := 0; i+len(substring) <= len(text); i++ {
				if text[i:i+len(substring)] == substring {
					return true
				}
			}
			return false
		}())
}

// waitFor blocks until condition holds, or the test fails.
func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	for range 200 {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition never became true")
}

func repoWith(name string, prs ...models.PullRequest) models.Repo {
	assessed := []models.PullRequest{}
	for _, pr := range prs {
		assessed = append(assessed, readiness.Assess(pr))
	}
	return testfixtures.Repo(name, assessed)
}

func TestPollLoadsRepos(t *testing.T) {
	client := newFakeGitHub(repoWith("acme/api", testfixtures.PR(1)))
	h := start(t, client, []string{"acme/api"}, nil)
	repo, found := h.monitor.Repo("acme/api")
	if !found || len(repo.PRs) != 1 {
		t.Fatalf("repo = %+v, found = %v", repo, found)
	}
	if repo.PRs[0].Actions == nil || len(repo.PRs[0].Actions) == 0 {
		t.Error("PRs should arrive with their action menus filled in")
	}
	if h.monitor.Status().LastUpdate == nil {
		t.Error("status should record the update")
	}
	if got := h.monitor.Unseen("acme/api"); len(got) != 1 {
		t.Errorf("unseen = %v, want the new PR", got)
	}
}

func TestFetchErrorKeepsLastData(t *testing.T) {
	client := newFakeGitHub(repoWith("acme/api", testfixtures.PR(1)))
	h := start(t, client, []string{"acme/api"}, nil)
	client.mutex.Lock()
	client.fetchErr["acme/api"] = &github.Error{Message: "GitHub API error 500"}
	client.mutex.Unlock()
	h.poll()
	if got := h.monitor.Errors()["acme/api"]; got == "" {
		t.Error("the error should be recorded")
	}
	if repo, found := h.monitor.Repo("acme/api"); !found || len(repo.PRs) != 1 {
		t.Error("the last good data should be kept")
	}
}

func TestRateLimitPausesPolling(t *testing.T) {
	client := newFakeGitHub(repoWith("acme/api", testfixtures.PR(1)))
	h := start(t, client, []string{"acme/api"}, nil)
	reset := time.Now().UTC().Add(time.Hour)
	client.mutex.Lock()
	client.fetchErr["acme/api"] = &github.RateLimitError{Message: "rate limited", ResetAt: reset}
	before := len(client.calls)
	client.mutex.Unlock()
	h.poll()
	if h.monitor.Status().RateLimitedUntil == nil {
		t.Error("status should show the pause")
	}
	client.mutex.Lock()
	during := len(client.calls)
	client.mutex.Unlock()
	h.poll() // still paused, so no further fetch
	client.mutex.Lock()
	after := len(client.calls)
	client.mutex.Unlock()
	if during <= before || after != during {
		t.Errorf("fetches: before=%d during=%d after=%d, want the pause to hold", before, during, after)
	}
}

func TestNotificationsWaitForTheFirstLoad(t *testing.T) {
	settings := config.NewNotifyConfig()
	settings.ScriptEnabled = true
	settings.Script = "im"
	client := newFakeGitHub(repoWith("acme/api", testfixtures.PR(1, testfixtures.Pending)))
	h := start(t, client, []string{"acme/api"},
		map[string]config.NotifyConfig{"acme/api": settings})
	if got := h.sent(); len(got) != 0 {
		t.Fatalf("sent %v on the first load, want none", got)
	}
	client.setRepo(repoWith("acme/api", testfixtures.PR(1)))
	h.poll()
	sent := h.sent()
	if len(sent) != 1 || sent[0]["PR_STATE"] != "READY" || sent[0]["PR_NUM"] != "1" {
		t.Fatalf("sent = %v", sent)
	}
}

func TestAddAndRemoveRepo(t *testing.T) {
	client := newFakeGitHub(repoWith("acme/api", testfixtures.PR(1)))
	h := start(t, client, []string{"acme/api"}, nil)
	client.setRepo(repoWith("Acme/New", testfixtures.PR(3)))

	name, err := h.monitor.AddRepo(context.Background(), "acme/new")
	if err == nil {
		t.Fatalf("adding an unknown spelling should fail until GitHub is asked: %q", name)
	}
	name, err = h.monitor.AddRepo(context.Background(), "Acme/New")
	if err != nil {
		t.Fatal(err)
	}
	if name != "Acme/New" {
		t.Errorf("name = %q, want GitHub's spelling", name)
	}
	if _, err := h.monitor.AddRepo(context.Background(), "acme/api"); err == nil {
		t.Error("adding a monitored repo again should fail")
	}
	saved, _ := config.Load(h.configPath)
	if len(saved.Repos) != 2 {
		t.Errorf("saved repos = %v", saved.Repos)
	}

	if err := h.monitor.RemoveRepo("Acme/New"); err != nil {
		t.Fatal(err)
	}
	if _, found := h.monitor.Repo("Acme/New"); found {
		t.Error("the repo should be gone")
	}
	if err := h.monitor.RemoveRepo("gone/repo"); err == nil {
		t.Error("removing an unmonitored repo should fail")
	}
	saved, _ = config.Load(h.configPath)
	if len(saved.Repos) != 1 || saved.Repos[0] != "acme/api" {
		t.Errorf("saved repos = %v", saved.Repos)
	}
}

func TestMarkSeenAndCollapsed(t *testing.T) {
	client := newFakeGitHub(repoWith("acme/api", testfixtures.PR(1)))
	h := start(t, client, []string{"acme/api"}, nil)
	h.monitor.MarkSeen("acme/api", 1)
	if got := h.monitor.Unseen("acme/api"); len(got) != 0 {
		t.Errorf("unseen = %v", got)
	}
	h.monitor.SetCollapsed("acme", true)
	if got := h.monitor.Collapsed(); len(got) != 1 || got[0] != "acme" {
		t.Errorf("collapsed = %v", got)
	}
	h.monitor.SetCollapsed("acme", false)
	if got := h.monitor.Collapsed(); len(got) != 0 {
		t.Errorf("collapsed = %v", got)
	}
}

func TestAlertOpensACollapsedGroup(t *testing.T) {
	client := newFakeGitHub(repoWith("acme/api", testfixtures.PR(1)))
	h := start(t, client, []string{"acme/api"}, nil)
	h.monitor.SetCollapsed("acme", true)
	client.setRepo(repoWith("acme/api", testfixtures.PR(1, testfixtures.Failing)))
	h.poll()
	if got := h.monitor.Collapsed(); len(got) != 0 {
		t.Errorf("collapsed = %v, want the group opened by the new alert", got)
	}
}

func TestPerformActions(t *testing.T) {
	client := newFakeGitHub(repoWith("acme/api", testfixtures.PR(1)))
	h := start(t, client, []string{"acme/api"}, nil)
	ctx := context.Background()
	squash := models.MergeSquash

	if err := h.monitor.Perform(ctx, "acme/api", 1,
		models.Action{Kind: "merge", Method: &squash, DeleteBranch: true}); err != nil {
		t.Fatal(err)
	}
	h.monitor.WaitIdle()
	if got := client.callsMatching("merge "); len(got) != 1 || got[0] != "merge PR_1 SQUASH" {
		t.Errorf("merge calls = %v", got)
	}
	if got := client.callsMatching("delete "); len(got) != 1 {
		t.Errorf("delete calls = %v, want the branch deleted", got)
	}
	if !h.hasToast("Merged acme/api#1 (squash)") {
		t.Errorf("toasts = %v", h.toasts())
	}

	if err := h.monitor.Perform(ctx, "acme/api", 1, models.Action{Kind: "update"}); err != nil {
		t.Fatal(err)
	}
	if got := client.callsMatching("update "); len(got) != 1 {
		t.Errorf("update calls = %v", got)
	}
	if err := h.monitor.Perform(ctx, "acme/api", 1,
		models.Action{Kind: "auto_merge_on", Method: &squash}); err != nil {
		t.Fatal(err)
	}
	if got := client.callsMatching("auto_on "); len(got) != 1 {
		t.Errorf("auto-merge calls = %v", got)
	}
	if err := h.monitor.Perform(ctx, "acme/api", 99, models.Action{Kind: "update"}); err == nil {
		t.Error("acting on an unknown PR should fail")
	}
}

func TestMergeFailureIsAToast(t *testing.T) {
	client := newFakeGitHub(repoWith("acme/api", testfixtures.PR(1)))
	client.mergeErr = &github.Error{Message: "not allowed"}
	h := start(t, client, []string{"acme/api"}, nil)
	squash := models.MergeSquash
	if err := h.monitor.Perform(context.Background(), "acme/api", 1,
		models.Action{Kind: "merge", Method: &squash}); err != nil {
		t.Fatalf("GitHub failures are toasts, not errors: %v", err)
	}
	if !h.hasToast("Merge failed for acme/api#1: not allowed") {
		t.Errorf("toasts = %v", h.toasts())
	}
}

func TestSetPollInterval(t *testing.T) {
	client := newFakeGitHub(repoWith("acme/api", testfixtures.PR(1)))
	h := start(t, client, []string{"acme/api"}, nil)
	if err := h.monitor.SetPollInterval(120); err != nil {
		t.Fatal(err)
	}
	if h.monitor.Config().PollInterval != 120 {
		t.Errorf("interval = %d", h.monitor.Config().PollInterval)
	}
	saved, _ := config.Load(h.configPath)
	if saved.PollInterval != 120 {
		t.Errorf("saved interval = %d", saved.PollInterval)
	}
	for _, bad := range []int{5, 4000} {
		if err := h.monitor.SetPollInterval(bad); err == nil {
			t.Errorf("%d should be rejected", bad)
		}
	}
}

func TestSaveNotificationsAndTest(t *testing.T) {
	client := newFakeGitHub(repoWith("acme/api", testfixtures.PR(1)))
	h := start(t, client, []string{"acme/api"}, nil)
	settings := config.NewNotifyConfig()
	settings.ScriptEnabled = true
	settings.Script = "im"
	if err := h.monitor.SaveNotifications("acme/api", settings); err != nil {
		t.Fatal(err)
	}
	saved, _ := config.Load(h.configPath)
	if !saved.Notifications["acme/api"].Equal(settings) {
		t.Errorf("saved = %+v", saved.Notifications)
	}
	if err := h.monitor.SaveNotifications("gone/repo", settings); err == nil {
		t.Error("settings for an unmonitored repo should be refused")
	}
	h.monitor.SendTest("acme/api", settings)
	h.monitor.WaitIdle()
	sent := h.sent()
	if len(sent) != 1 || sent[0]["PR_REPO"] != "acme/api" {
		t.Errorf("sent = %v", sent)
	}
	if !h.hasToast("Test script notification sent") {
		t.Errorf("toasts = %v", h.toasts())
	}
	h.monitor.SendTest("acme/api", config.NewNotifyConfig())
	h.monitor.WaitIdle()
	if !h.hasToast("Nothing to send") {
		t.Errorf("toasts = %v", h.toasts())
	}
}

func TestEventsAreEmitted(t *testing.T) {
	client := newFakeGitHub(repoWith("acme/api", testfixtures.PR(1)))
	h := start(t, client, []string{"acme/api"}, nil)
	wanted := map[string]bool{"repo": false, "status": false}
	for _, kind := range h.kinds() {
		if _, tracked := wanted[kind]; tracked {
			wanted[kind] = true
		}
	}
	for kind, seen := range wanted {
		if !seen {
			t.Errorf("no %q event; kinds = %v", kind, h.kinds())
		}
	}
}
