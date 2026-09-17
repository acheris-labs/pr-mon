package notify_test

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/acheris-labs/pr-mon/internal/config"
	"github.com/acheris-labs/pr-mon/internal/models"
	"github.com/acheris-labs/pr-mon/internal/notify"
	"github.com/acheris-labs/pr-mon/internal/readiness"
	"github.com/acheris-labs/pr-mon/internal/testfixtures"
	"github.com/acheris-labs/pr-mon/internal/tracker"
)

var vars = map[string]string{"PR_NUM": "12", "PR_STATE": "READY", "PR_URL": "https://x/12"}

func TestRender(t *testing.T) {
	got := notify.Render("PR {{PR_NUM}} is {{ PR_STATE }}: {{PR_URL}}", vars)
	if want := "PR 12 is READY: https://x/12"; got != want {
		t.Errorf("render = %q, want %q", got, want)
	}
	if got := notify.Render("{{PR_TYPO}} {{PR_NUM}}", vars); got != "{{PR_TYPO}} 12" {
		t.Errorf("unknown placeholders should be left alone: %q", got)
	}
	values := map[string]string{"PR_TITLE": "{{PR_NUM}} $(rm -rf ~)", "PR_NUM": "1"}
	if got := notify.Render("{{PR_TITLE}}", values); got != "{{PR_NUM}} $(rm -rf ~)" {
		t.Errorf("substituted values must not be rescanned: %q", got)
	}
}

func TestUnknownPlaceholders(t *testing.T) {
	got := notify.UnknownPlaceholders("{{PR_NUM}} {{ nope }} {{PR_TYPO}} {{nope}}")
	if want := []string{"nope", "PR_TYPO"}; !reflect.DeepEqual(got, want) {
		t.Errorf("unknown = %v, want %v", got, want)
	}
	if got := notify.UnknownPlaceholders("{{PR_REPO}}#{{PR_NUM}}"); len(got) != 0 {
		t.Errorf("unknown = %v, want none", got)
	}
}

func TestSampleVariablesCoverEveryName(t *testing.T) {
	sample := notify.SampleVariables("acme/web")
	names := []string{}
	for name := range sample {
		names = append(names, name)
	}
	sort.Strings(names)
	want := append([]string{}, notify.VariableNames...)
	sort.Strings(want)
	if !reflect.DeepEqual(names, want) {
		t.Errorf("sample = %v, want %v", names, want)
	}
	if sample["PR_REPO"] != "acme/web" ||
		sample["PR_URL"] != "https://github.com/acme/web/pull/123" ||
		sample["PR_REASON"] != "" {
		t.Errorf("sample = %+v", sample)
	}
}

func TestPRVariables(t *testing.T) {
	pr := readiness.Assess(testfixtures.PR(7, func(p *models.PullRequest) { p.Title = "Fix it" }))
	got := notify.PRVariables("acme/api", pr, "FAILING", "")
	want := map[string]string{
		"PR_REPO": "acme/api", "PR_NUM": "7", "PR_TITLE": "Fix it", "PR_AUTHOR": "alice",
		"PR_BRANCH": "feature-7", "PR_TARGET": "main", "PR_STATE": "FAILING",
		"PR_URL": "https://github.com/acme/api/pull/7", "PR_REASON": "",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("variables = %+v, want %+v", got, want)
	}
	failed := notify.PRVariables("acme/api", pr, "MERGE_FAILED", "no permission")
	if failed["PR_REASON"] != "no permission" {
		t.Errorf("reason = %q", failed["PR_REASON"])
	}
}

func TestSelect(t *testing.T) {
	repo := testfixtures.Repo("acme/api", []models.PullRequest{
		readiness.Assess(testfixtures.PR(1)),
		readiness.Assess(testfixtures.PR(2, testfixtures.Draft)),
	})
	selected := func(changes []tracker.Change, settings config.NotifyConfig) []string {
		found := []string{}
		for _, notification := range notify.Select(repo, changes, settings) {
			found = append(found, notification.State)
		}
		return found
	}
	defaults := config.NewNotifyConfig()
	newOnly := config.NotifyConfig{Events: []string{"NEW"}}
	drafts := config.NotifyConfig{Events: []string{"NEW"}, IncludeDrafts: true}

	opened := []tracker.Change{{Number: 1, New: models.StatusReady}}
	if got := selected(opened, defaults); len(got) != 0 {
		t.Errorf("NEW should be quiet unless selected: %v", got)
	}
	if got := selected(opened, newOnly); !reflect.DeepEqual(got, []string{"NEW"}) {
		t.Errorf("states = %v", got)
	}
	became := []tracker.Change{{Number: 1, Old: models.StatusPending, New: models.StatusReady}}
	if got := selected(became, defaults); !reflect.DeepEqual(got, []string{"READY"}) {
		t.Errorf("states = %v", got)
	}
	behind := []tracker.Change{{Number: 1, Old: models.StatusReady, New: models.StatusBehind}}
	if got := selected(behind, defaults); len(got) != 0 {
		t.Errorf("unselected statuses should be quiet: %v", got)
	}
	moved := []tracker.Change{{Number: 1, Old: models.StatusFailing, New: models.StatusConflict}}
	if got := selected(moved, defaults); !reflect.DeepEqual(got, []string{"CONFLICT"}) {
		t.Errorf("states = %v", got)
	}
	draft := []tracker.Change{{Number: 2, New: models.StatusDraft}}
	if got := selected(draft, newOnly); len(got) != 0 {
		t.Errorf("drafts should be skipped: %v", got)
	}
	if got := selected(draft, drafts); !reflect.DeepEqual(got, []string{"NEW"}) {
		t.Errorf("states = %v", got)
	}
	found := notify.Select(repo, became, defaults)
	if len(found) != 1 || found[0].Repo != "acme/api" || found[0].PR.Number != 1 {
		t.Errorf("notifications = %+v", found)
	}
}

func TestDetectDesktopNotifier(t *testing.T) {
	cases := []struct {
		present []string
		want    string
	}{
		{[]string{"terminal-notifier", "osascript", "notify-send"}, "terminal-notifier"},
		{[]string{"osascript", "notify-send"}, "osascript"},
		{[]string{"notify-send"}, "notify-send"},
		{nil, ""},
	}
	for _, test := range cases {
		t.Run(strings.Join(test.present, ","), func(t *testing.T) {
			dir := t.TempDir()
			for _, tool := range test.present {
				if err := os.WriteFile(filepath.Join(dir, tool), []byte("#!/bin/sh\n"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("PATH", dir)
			want := ""
			if test.want != "" {
				want = filepath.Join(dir, test.want)
			}
			if got := notify.DetectDesktopNotifier(); got != want {
				t.Errorf("notifier = %q, want %q", got, want)
			}
		})
	}
}

func TestDesktopArgv(t *testing.T) {
	sample := notify.SampleVariables("owner/repo")
	got := notify.DesktopArgv("/x/terminal-notifier", "-hi", sample)
	want := []string{"/x/terminal-notifier", "-title", "pr-mon", "-subtitle", "owner/repo",
		"-message", "-hi", "-open", sample["PR_URL"]}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("argv = %v, want %v", got, want)
	}
	got = notify.DesktopArgv("/usr/bin/osascript", `say "x"`, sample)
	want = []string{"/usr/bin/osascript",
		"-e", "on run argv",
		"-e", "display notification (item 1 of argv) with title (item 2 of argv)",
		"-e", "end run",
		`say "x"`, "pr-mon"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("argv = %v, want %v", got, want)
	}
	got = notify.DesktopArgv("/usr/bin/notify-send", "-hi", sample)
	if want := []string{"/usr/bin/notify-send", "--", "pr-mon", "-hi"}; !reflect.DeepEqual(got, want) {
		t.Errorf("argv = %v, want %v", got, want)
	}
}

func TestScriptArgv(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	got, err := notify.ScriptArgv(`~/bin/send --to 'two words' ~x $HOME`)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{home + "/bin/send", "--to", "two words", home + "x", "$HOME"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("argv = %v, want %v", got, want)
	}
	for _, command := range []string{"", "   ", "send 'unterminated"} {
		if _, err := notify.ScriptArgv(command); err == nil {
			t.Errorf("%q should be rejected", command)
		}
	}
}

// script writes an executable shell script and returns its path.
func script(t *testing.T, dir, body string) string {
	t.Helper()
	path := filepath.Join(dir, "s")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunPassesStdinEnvAndHome(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	path := script(t, dir, "cat > \""+out+"\"\necho \"$PR_NUM|$PWD\" >> \""+out+"\"\n")
	stdin := "hello\nworld"
	if problem := notify.Run(context.Background(), []string{path}, &stdin,
		map[string]string{"PR_NUM": "12"}, notify.CommandTimeout); problem != "" {
		t.Fatalf("problem = %q", problem)
	}
	text, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	home, _ := os.UserHomeDir()
	if want := "hello\nworld12|" + home + "\n"; string(text) != want {
		t.Errorf("output = %q, want %q", text, want)
	}
}

func TestRunReportsFailures(t *testing.T) {
	dir := t.TempDir()
	empty := ""
	cases := []struct {
		name string
		body string
		want string
	}{
		{"nonzero exit", "echo 'first problem' >&2\necho second >&2\nexit 3\n",
			"s exited with code 3: first problem"},
		{"nonzero exit without stderr", "exit 4\n", "s exited with code 4"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			path := script(t, t.TempDir(), test.body)
			got := notify.Run(context.Background(), []string{path}, &empty, nil, notify.CommandTimeout)
			if got != test.want {
				t.Errorf("problem = %q, want %q", got, test.want)
			}
		})
	}
	missing := filepath.Join(dir, "nope")
	if got := notify.Run(context.Background(), []string{missing}, &empty, nil,
		notify.CommandTimeout); !strings.Contains(got, "nope") {
		t.Errorf("problem = %q, want it to name the command", got)
	}
	plain := filepath.Join(dir, "plain")
	if err := os.WriteFile(plain, []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := notify.Run(context.Background(), []string{plain}, &empty, nil,
		notify.CommandTimeout); !strings.Contains(got, "plain") {
		t.Errorf("problem = %q, want it to name the command", got)
	}
}

func TestRunIgnoringStdinIsFine(t *testing.T) {
	path := script(t, t.TempDir(), "exit 0\n")
	big := strings.Repeat("x", 1_000_000)
	if problem := notify.Run(context.Background(), []string{path}, &big, nil,
		notify.CommandTimeout); problem != "" {
		t.Errorf("problem = %q", problem)
	}
}

func TestRunTimeoutKillsTheProcessGroup(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "marker")
	// A child that outlives its parent unless the whole group is killed.
	path := script(t, dir, "(sleep 5; touch \""+marker+"\") &\nsleep 5\n")
	empty := ""
	start := time.Now()
	problem := notify.Run(context.Background(), []string{path}, &empty, nil, 200*time.Millisecond)
	if !strings.Contains(problem, "timed out") {
		t.Errorf("problem = %q, want a timeout", problem)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("took %s, want the timeout to end it", elapsed)
	}
	time.Sleep(700 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Error("the script's child survived the timeout")
	}
}

func TestDeliver(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	path := script(t, dir, "cat > \""+out+"\"\n")
	settings := config.NewNotifyConfig()
	settings.Message = "{{PR_REPO}}#{{PR_NUM}} is {{PR_STATE}}"
	settings.ScriptEnabled = true
	settings.Script = path
	settings.DesktopEnabled = true
	variables := notify.PRVariables("acme/api", testfixtures.PR(12), "READY", "")

	results := notify.Deliver(context.Background(), settings, "", variables)
	if len(results) != 1 || results[0].Channel != "script" || results[0].Error != "" {
		t.Fatalf("results = %+v, want the script only", results)
	}
	text, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(text) != "acme/api#12 is READY" {
		t.Errorf("message = %q", text)
	}

	// With a notifier, both channels run; echo stands in for the desktop tool.
	notifier := filepath.Join(t.TempDir(), "notify-send")
	if err := os.WriteFile(notifier, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	results = notify.Deliver(context.Background(), settings, notifier, variables)
	if len(results) != 2 || results[0].Channel != "script" || results[1].Channel != "desktop" {
		t.Errorf("results = %+v", results)
	}
	for _, result := range results {
		if result.Error != "" {
			t.Errorf("%s error = %q", result.Channel, result.Error)
		}
	}

	// Nothing enabled sends nothing; a bad command is reported, not run.
	if got := notify.Deliver(context.Background(), config.NewNotifyConfig(), notifier,
		variables); len(got) != 0 {
		t.Errorf("results = %+v, want none", got)
	}
	bad := settings
	bad.Script = "send 'unterminated"
	bad.DesktopEnabled = false
	got := notify.Deliver(context.Background(), bad, "", variables)
	if len(got) != 1 || !strings.Contains(got[0].Error, "Bad script command") {
		t.Errorf("results = %+v", got)
	}
}

func TestNotificationFormAndPreview(t *testing.T) {
	form := notify.NotificationForm()
	if len(form.Events) != len(config.EventNames) || form.Events[0].Name != "READY" ||
		form.Events[0].Label != "Ready (mergeable)" {
		t.Errorf("events = %+v", form.Events)
	}
	if !strings.Contains(form.ScriptHelp, "stdin") {
		t.Errorf("script help = %q", form.ScriptHelp)
	}
	if !form.Defaults.Equal(config.NewNotifyConfig()) {
		t.Errorf("defaults = %+v", form.Defaults)
	}
	preview := notify.PreviewMessage("acme/api", "{{PR_REPO}} {{NOPE}}")
	if preview.Text != "acme/api {{NOPE}}" ||
		!reflect.DeepEqual(preview.Unknown, []string{"NOPE"}) {
		t.Errorf("preview = %+v", preview)
	}
}
