package service_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/acheris-labs/pr-mon/internal/actions"
	"github.com/acheris-labs/pr-mon/internal/config"
	"github.com/acheris-labs/pr-mon/internal/github"
	"github.com/acheris-labs/pr-mon/internal/models"
	"github.com/acheris-labs/pr-mon/internal/readiness"
	"github.com/acheris-labs/pr-mon/internal/service"
	"github.com/acheris-labs/pr-mon/internal/state"
	"github.com/acheris-labs/pr-mon/internal/testfixtures"
	"github.com/acheris-labs/pr-mon/internal/twoway"
	"github.com/acheris-labs/pr-mon/internal/twoway/handler"
)

// configureTwoWay gives the saved config a usable [two_way] table.
func configureTwoWay(saved *config.Config) {
	saved.TwoWay = &config.TwoWayConfig{Command: "/usr/local/bin/pr-mon-handler --verbose"}
}

// startTwoWay starts a harness watching acme/api with [two_way] configured,
// the fake standing in for the handler.
func startTwoWay(t *testing.T, client *fakeGitHub,
	notifications map[string]config.NotifyConfig) *harness {
	t.Helper()
	return launch(t, client, []string{"acme/api"}, notifications, configureTwoWay,
		func(h *harness) {
			h.twoWay = &testfixtures.TwoWay{Taps: make(chan twoway.Tap), ReceiveErrs: make(chan error)}
		})
}

// twoWaySettings sends acme/api's chosen events over the two-way channel only.
func twoWaySettings(events ...string) map[string]config.NotifyConfig {
	settings := config.NewNotifyConfig()
	settings.TwoWayEnabled = true
	if len(events) > 0 {
		settings.Events = events
	}
	return map[string]config.NotifyConfig{"acme/api": settings}
}

// messages is what state.json tracks now.
func (h *harness) messages() map[string]state.Message {
	loaded, warning := state.Load(h.statePath)
	if warning != "" {
		h.t.Fatal(warning)
	}
	return loaded.Messages
}

func (h *harness) posts() []twoway.Message {
	h.twoWay.Lock()
	defer h.twoWay.Unlock()
	return append([]twoway.Message{}, h.twoWay.Posts...)
}

func (h *harness) deletes() []string {
	h.twoWay.Lock()
	defer h.twoWay.Unlock()
	return append([]string{}, h.twoWay.Deletes...)
}

func (h *harness) edit(id string) (twoway.Message, bool) {
	h.twoWay.Lock()
	defer h.twoWay.Unlock()
	message, found := h.twoWay.Edits[id]
	return message, found
}

// becomes makes acme/api's only PR look like this from the next poll on.
func becomes(h *harness, mutate ...func(*models.PullRequest)) {
	h.client.setRepo(repoWith("acme/api", testfixtures.PR(1, mutate...)))
	h.poll()
}

// readyMessage starts a two-way harness whose PR 1 went READY and was posted
// about as message "1".
func readyMessage(t *testing.T, notifications map[string]config.NotifyConfig) *harness {
	t.Helper()
	client := newFakeGitHub(repoWith("acme/api", testfixtures.PR(1, testfixtures.Pending)))
	h := startTwoWay(t, client, notifications)
	becomes(h)
	if got := h.posts(); len(got) != 1 || got[0].Status != "READY" {
		t.Fatalf("posts = %+v, want one READY message", got)
	}
	return h
}

func kinds(buttons []state.Button) []string {
	found := []string{}
	for _, button := range buttons {
		found = append(found, button.Kind)
	}
	return found
}

func failedActionsCheck(pr *models.PullRequest) {
	pr.Checks = []models.Check{{Name: "ci", State: "FAILURE",
		StartedAt: models.Ptr(testfixtures.CheckStarted), RunID: models.Ptr(7)}}
}

func canBypass(pr *models.PullRequest) { pr.CanBypass = true }

func TestButtonsFor(t *testing.T) {
	all := []models.MergeMethod{models.MergeSquash, models.MergeCommit, models.MergeRebase}
	cases := []struct {
		name    string
		event   string
		methods []models.MergeMethod
		mutate  []func(*models.PullRequest)
		want    []state.Button
	}{
		{"ready, one method", "READY", []models.MergeMethod{models.MergeSquash}, nil,
			[]state.Button{{Kind: "merge", Method: "SQUASH", DeleteBranch: true}, {Kind: "dismiss"}}},
		{"ready, every method", "READY", all, nil, []state.Button{
			{Kind: "merge", Method: "SQUASH", DeleteBranch: true},
			{Kind: "merge", Method: "MERGE", DeleteBranch: true},
			{Kind: "merge", Method: "REBASE", DeleteBranch: true},
			{Kind: "dismiss"}}},
		{"failing, may bypass", "FAILING", all,
			[]func(*models.PullRequest){testfixtures.Failing, failedActionsCheck, canBypass},
			[]state.Button{{Kind: "rerun_checks"},
				{Kind: "force_merge", Method: "SQUASH", DeleteBranch: true}, {Kind: "dismiss"}}},
		{"failing, no bypass", "FAILING", all,
			[]func(*models.PullRequest){testfixtures.Failing, failedActionsCheck},
			[]state.Button{{Kind: "rerun_checks"}, {Kind: "dismiss"}}},
		{"blocked, even with bypass", "BLOCKED", all,
			[]func(*models.PullRequest){func(pr *models.PullRequest) { pr.MergeState = "BLOCKED" }, canBypass},
			[]state.Button{{Kind: "dismiss"}}},
		{"conflict", "CONFLICT", all, []func(*models.PullRequest){testfixtures.Conflict},
			[]state.Button{{Kind: "dismiss"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pr := readiness.Assess(testfixtures.PR(1, c.mutate...))
			if string(pr.Status) != c.event {
				t.Fatalf("status = %s, want %s", pr.Status, c.event)
			}
			repo := actions.WithActions(testfixtures.Repo("acme/api", []models.PullRequest{pr},
				func(repo *models.Repo) { repo.MergeMethods = c.methods }), nil)
			got := service.ButtonsFor(repo, repo.PRs[0], c.event)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("buttons = %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestPostsOnSelectedEvent(t *testing.T) {
	h := readyMessage(t, twoWaySettings())
	post := h.posts()[0]
	if !strings.Contains(post.Title, "acme/api #1") {
		t.Errorf("title = %q", post.Title)
	}
	labels := []string{}
	for _, row := range post.Buttons {
		for _, button := range row {
			labels = append(labels, button.Label)
			if button.URL != "" && button.URL != "https://github.com/acme/api/pull/1" {
				t.Errorf("link = %q, want the PR", button.URL)
			}
			if button.URL == "" && len(button.Key) != 8 {
				t.Errorf("key %q, want 8 characters", button.Key)
			}
		}
	}
	want := []string{"Squash & merge", "Merge commit", "Rebase & merge", "Open", "Dismiss"}
	if !reflect.DeepEqual(labels, want) {
		t.Errorf("labels = %v, want %v", labels, want)
	}
	messages := h.messages()
	message, found := messages["1"]
	if !found || len(messages) != 1 {
		t.Fatalf("messages = %+v, want message 1", messages)
	}
	if message.Repo != "acme/api" || message.Number != 1 || message.Event != "READY" ||
		message.Status != "READY" || message.HeadSha != testfixtures.HeadSha || message.SentAt == "" {
		t.Errorf("message = %+v", message)
	}
	if got := kinds(sortedButtons(message.Keys)); len(got) != 4 {
		t.Errorf("keys = %+v, want three merges and dismiss", message.Keys)
	}
	for _, row := range post.Buttons {
		for _, button := range row {
			if _, known := message.Keys[button.Key]; button.URL == "" && !known {
				t.Errorf("button %q has key %q, which isn't recorded", button.Label, button.Key)
			}
		}
	}
}

// sortedButtons is the recorded buttons, in no particular order.
func sortedButtons(keys map[string]state.Button) []state.Button {
	found := []state.Button{}
	for _, button := range keys {
		found = append(found, button)
	}
	return found
}

func TestMergedIsNotSent(t *testing.T) {
	client := newFakeGitHub(repoWith("acme/api", testfixtures.PR(1, testfixtures.Pending)))
	h := startTwoWay(t, client, twoWaySettings())
	arm(t, h, 1, false)
	becomes(h)
	h.monitor.WaitIdle()
	if len(client.callsMatching("merge ")) != 1 {
		t.Fatalf("calls = %v, want the merge", client.callsMatching(""))
	}
	for _, post := range h.posts() {
		if post.Status == "MERGED" {
			t.Errorf("posted %+v, but MERGED isn't sent over two-way", post)
		}
	}
}

func TestMutedIsNotSent(t *testing.T) {
	client := newFakeGitHub(repoWith("acme/api", testfixtures.PR(1, testfixtures.Pending)))
	h := startTwoWay(t, client, twoWaySettings())
	mute(t, h, 1, "mute")
	becomes(h)
	if got := h.posts(); len(got) != 0 {
		t.Errorf("posts = %+v, want none for a muted PR", got)
	}
}

func TestDisabledRepoIsNotSent(t *testing.T) {
	settings := twoWaySettings()
	disabled := settings["acme/api"]
	disabled.TwoWayEnabled = false
	settings["acme/api"] = disabled
	client := newFakeGitHub(repoWith("acme/api", testfixtures.PR(1, testfixtures.Pending)))
	h := startTwoWay(t, client, settings)
	becomes(h)
	if got := h.posts(); len(got) != 0 {
		t.Errorf("posts = %+v, want none with the switch off", got)
	}
}

func TestNewEventSupersedes(t *testing.T) {
	h := readyMessage(t, twoWaySettings())
	becomes(h, testfixtures.Failing)
	posts := h.posts()
	if len(posts) != 2 || posts[1].Status != "FAILING" {
		t.Fatalf("posts = %+v, want a FAILING message", posts)
	}
	if got := h.deletes(); !reflect.DeepEqual(got, []string{"1"}) {
		t.Errorf("deletes = %v, want the READY message", got)
	}
	messages := h.messages()
	if _, found := messages["2"]; !found || len(messages) != 1 || messages["2"].Status != "FAILING" {
		t.Errorf("messages = %+v, want only the FAILING one", messages)
	}
}

func TestStatusMovedDeletes(t *testing.T) {
	h := readyMessage(t, twoWaySettings("READY", "FAILING"))
	becomes(h, testfixtures.Conflict)
	if got := h.posts(); len(got) != 1 {
		t.Errorf("posts = %+v, want nothing new for an unselected event", got)
	}
	if got := h.deletes(); !reflect.DeepEqual(got, []string{"1"}) {
		t.Errorf("deletes = %v, want the READY message", got)
	}
	if got := h.messages(); len(got) != 0 {
		t.Errorf("messages = %+v, want none", got)
	}
}

func TestFirstLoadDeletesMovedMessages(t *testing.T) {
	h := readyMessage(t, twoWaySettings())
	h.client.setRepo(repoWith("acme/api", testfixtures.PR(1, testfixtures.Failing)))
	h.restart()
	if got := h.posts(); len(got) != 1 {
		t.Errorf("posts = %+v, want nothing on the first load", got)
	}
	if got := h.deletes(); !reflect.DeepEqual(got, []string{"1"}) {
		t.Errorf("deletes = %v, want the READY message", got)
	}
	if got := h.messages(); len(got) != 0 {
		t.Errorf("messages = %+v, want none", got)
	}
}

func TestFirstLoadKeepsCurrentMessages(t *testing.T) {
	h := readyMessage(t, twoWaySettings())
	h.restart()
	if got := h.deletes(); len(got) != 0 {
		t.Errorf("deletes = %v, want none: the PR is still READY", got)
	}
	if _, found := h.messages()["1"]; !found {
		t.Error("the message should still be tracked")
	}
}

func TestClosedPRDeletes(t *testing.T) {
	h := readyMessage(t, twoWaySettings())
	h.client.setRepo(repoWith("acme/api", testfixtures.PR(2, testfixtures.Pending)))
	h.poll()
	if got := h.deletes(); !reflect.DeepEqual(got, []string{"1"}) {
		t.Errorf("deletes = %v, want the closed PR's message", got)
	}
	if got := h.messages(); len(got) != 0 {
		t.Errorf("messages = %+v, want none", got)
	}
}

func TestTruncatedListKeepsMessage(t *testing.T) {
	h := readyMessage(t, twoWaySettings())
	truncated := repoWith("acme/api", testfixtures.PR(3, testfixtures.Pending))
	truncated.PRTotal = 80
	h.client.setRepo(truncated)
	h.poll()
	if got := h.deletes(); len(got) != 0 {
		t.Errorf("deletes = %v, want none: the PR may be beyond the cut-off", got)
	}
	if _, found := h.messages()["1"]; !found {
		t.Error("the message should still be tracked")
	}
}

func TestTooOldFallsBackToEdit(t *testing.T) {
	h := readyMessage(t, twoWaySettings())
	h.twoWay.Lock()
	h.twoWay.DeleteErr = fmt.Errorf("telegram: %w", twoway.ErrTooOld)
	h.twoWay.Unlock()
	h.client.setRepo(repoWith("acme/api"))
	h.poll()
	edit, found := h.edit("1")
	if !found {
		t.Fatal("a message too old to delete should be edited")
	}
	if len(edit.Buttons) != 0 {
		t.Errorf("buttons = %+v, want none", edit.Buttons)
	}
	if !strings.Contains(edit.Title, "acme/api #1") ||
		!strings.Contains(strings.Join(edit.Lines, "\n"), "PR closed") {
		t.Errorf("edit = %+v, want the message's text and why it's done", edit)
	}
	if got := h.messages(); len(got) != 0 {
		t.Errorf("messages = %+v, want it forgotten", got)
	}
	if h.hasToast("notification failed") {
		t.Errorf("toasts = %v, want no failure", h.toasts())
	}
}

func TestGoneMessageIsForgotten(t *testing.T) {
	h := readyMessage(t, twoWaySettings())
	h.twoWay.Lock()
	h.twoWay.DeleteErr = fmt.Errorf("telegram: %w", twoway.ErrGone)
	h.twoWay.Unlock()
	h.client.setRepo(repoWith("acme/api"))
	h.poll()
	if _, found := h.edit("1"); found {
		t.Error("a message already gone needs no edit")
	}
	if got := h.messages(); len(got) != 0 {
		t.Errorf("messages = %+v, want it forgotten", got)
	}
	if h.hasToast("notification failed") {
		t.Errorf("toasts = %v, want no failure", h.toasts())
	}
}

func TestDeleteFailureIsReported(t *testing.T) {
	h := readyMessage(t, twoWaySettings())
	h.twoWay.Lock()
	h.twoWay.DeleteErr = errors.New("AWS is down")
	h.twoWay.Unlock()
	h.client.setRepo(repoWith("acme/api"))
	h.poll()
	if !h.hasToast("Fake notification failed: AWS is down") {
		t.Errorf("toasts = %v", h.toasts())
	}
}

func TestPostFailureKeepsPreviousMessage(t *testing.T) {
	h := readyMessage(t, twoWaySettings())
	h.twoWay.Lock()
	h.twoWay.PostErr = errors.New("AWS is down")
	h.twoWay.Unlock()
	becomes(h, testfixtures.Failing)
	if got := h.deletes(); len(got) != 0 {
		t.Errorf("deletes = %v, want the READY message kept", got)
	}
	if _, found := h.messages()["1"]; !found {
		t.Error("the READY message should still be tracked")
	}
	if !h.hasToast("Fake notification failed: AWS is down") {
		t.Errorf("toasts = %v", h.toasts())
	}
}

func TestRemoveRepoDeletesMessages(t *testing.T) {
	h := readyMessage(t, twoWaySettings())
	if err := h.monitor.RemoveRepo("acme/api"); err != nil {
		t.Fatal(err)
	}
	h.monitor.WaitIdle()
	if got := h.deletes(); !reflect.DeepEqual(got, []string{"1"}) {
		t.Errorf("deletes = %v, want the repo's message", got)
	}
	if got := h.messages(); len(got) != 0 {
		t.Errorf("messages = %+v, want none", got)
	}
}

func TestSendTestPostsSample(t *testing.T) {
	client := newFakeGitHub(repoWith("acme/api", testfixtures.PR(1)))
	h := startTwoWay(t, client, nil)
	h.monitor.SendTest("acme/api", twoWaySettings()["acme/api"])
	h.monitor.WaitIdle()
	posts := h.posts()
	if len(posts) != 1 || posts[0].Status != "TEST" {
		t.Fatalf("posts = %+v, want a TEST message", posts)
	}
	if buttons := posts[0].Buttons; len(buttons) != 1 || len(buttons[0]) != 1 ||
		buttons[0][0].Label != "Dismiss" {
		t.Errorf("buttons = %+v, want Dismiss only", buttons)
	}
	if h.hasToast("Nothing to send") {
		t.Errorf("toasts = %v: two-way counts", h.toasts())
	}
	if !h.hasToast("Test Fake notification sent") {
		t.Errorf("toasts = %v", h.toasts())
	}
	message, found := h.messages()["1"]
	if !found || message.Number != 0 || message.Repo != "acme/api" {
		t.Fatalf("messages = %+v, want the sample tracked with number 0", h.messages())
	}
	h.poll()
	if got := h.deletes(); len(got) != 0 {
		t.Errorf("deletes = %v: cleanup should leave the sample alone", got)
	}
}

func TestNothingToSendNamesTwoWay(t *testing.T) {
	client := newFakeGitHub(repoWith("acme/api", testfixtures.PR(1)))
	h := start(t, client, []string{"acme/api"}, nil)
	h.monitor.SendTest("acme/api", config.NewNotifyConfig())
	h.monitor.WaitIdle()
	if !h.hasToast("Nothing to send: enable Script, Desktop or Two-way notifications") {
		t.Errorf("toasts = %v", h.toasts())
	}
}

func TestMergeFailedPosts(t *testing.T) {
	client := newFakeGitHub(repoWith("acme/api", testfixtures.PR(1)))
	client.mergeErr = &github.Error{Message: "Pull request is not mergeable"}
	h := startTwoWay(t, client, twoWaySettings())
	arm(t, h, 1, false)
	h.monitor.WaitIdle()
	var failed *twoway.Message
	for _, post := range h.posts() {
		if post.Status == "MERGE_FAILED" {
			failed = &post
		}
	}
	if failed == nil {
		t.Fatalf("posts = %+v, want MERGE_FAILED", h.posts())
	}
	if !strings.Contains(strings.Join(failed.Lines, "\n"), "Pull request is not mergeable") {
		t.Errorf("lines = %v, want the reason", failed.Lines)
	}
}

func TestStartWarnsOnBadSetup(t *testing.T) {
	cases := []struct {
		name     string
		adjust   func(*config.Config)
		buildErr error
		want     string
	}{
		{"no command", func(saved *config.Config) {
			saved.TwoWay = &config.TwoWayConfig{}
		}, nil, "[two_way] command is missing"},
		{"builder fails", configureTwoWay, errors.New("boom"), "boom"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			client := newFakeGitHub(repoWith("acme/api", testfixtures.PR(1)))
			h := launch(t, client, []string{"acme/api"}, nil, c.adjust, func(h *harness) {
				h.twoWay = &testfixtures.TwoWay{}
				h.buildErr = c.buildErr
			})
			h.wantOffWarning(c.want)
		})
	}
}

// wantOffWarning checks two-way is off with a warning containing want.
func (h *harness) wantOffWarning(want string) {
	h.t.Helper()
	status := h.monitor.Status()
	if status.TwoWay != nil {
		h.t.Errorf("two_way = %q, want null", *status.TwoWay)
	}
	for _, warning := range status.Warnings {
		if strings.HasPrefix(warning, "Two-way notifications are off: ") &&
			strings.Contains(warning, want) {
			return
		}
	}
	h.t.Errorf("warnings = %v, want one mentioning %q", status.Warnings, want)
}

func TestUnsplittableCommandWarns(t *testing.T) {
	client := newFakeGitHub(repoWith("acme/api", testfixtures.PR(1)))
	built := false
	h := launch(t, client, []string{"acme/api"}, nil, func(saved *config.Config) {
		saved.TwoWay = &config.TwoWayConfig{Command: "'unclosed"}
	}, func(h *harness) {
		h.twoWay = &testfixtures.TwoWay{}
		h.onBuild = func() { built = true }
	})
	h.wantOffWarning("[two_way] command: ")
	if built {
		t.Error("the builder ran for a command that doesn't split")
	}
}

func TestLegacyTableWarns(t *testing.T) {
	client := newFakeGitHub(repoWith("acme/api", testfixtures.PR(1)))
	h := launch(t, client, []string{"acme/api"}, nil, nil, func(h *harness) {
		h.twoWay = &testfixtures.TwoWay{}
		h.legacyTwoWay = true
	})
	h.wantOffWarning("[two_way] now takes a handler command")
}

func TestStartNamesTheChannel(t *testing.T) {
	client := newFakeGitHub(repoWith("acme/api", testfixtures.PR(1)))
	h := startTwoWay(t, client, nil)
	status := h.monitor.Status()
	if status.TwoWay == nil || *status.TwoWay != "Fake" {
		t.Errorf("two_way = %v, want Fake", status.TwoWay)
	}
	if len(status.Warnings) != 0 {
		t.Errorf("warnings = %v", status.Warnings)
	}
}

func TestNoTwoWayTableMeansNoChannel(t *testing.T) {
	client := newFakeGitHub(repoWith("acme/api", testfixtures.PR(1)))
	h := start(t, client, []string{"acme/api"}, nil)
	if status := h.monitor.Status(); status.TwoWay != nil || len(status.Warnings) != 0 {
		t.Errorf("status = %+v, want no channel and no warning", status)
	}
}

func TestDefaultBuilderRunsTheHandler(t *testing.T) {
	reported := make(chan twoway.Availability, 4)
	channel, err := service.DefaultTwoWay("1.2.3", slog.New(slog.NewTextHandler(io.Discard, nil)))(
		context.Background(), config.TwoWayConfig{Command: "/nonexistent/pr-mon-handler"},
		func(now twoway.Availability) { reported <- now })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { channel.(io.Closer).Close() })
	if _, ok := channel.(*handler.Runner); !ok {
		t.Fatalf("channel is %T, want *handler.Runner", channel)
	}
	select {
	case now := <-reported:
		if now.Ready || now.Why == "" {
			t.Errorf("availability = %+v, want not ready, with a reason", now)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the runner never reported that the handler couldn't start")
	}
}

// severity is the severity of the toast with this message, or "" if none was shown.
func (h *harness) severity(message string) string {
	h.mutex.Lock()
	defer h.mutex.Unlock()
	for _, event := range h.events {
		if event.Kind == "toast" && event.Message == message {
			return event.Severity
		}
	}
	return ""
}

func TestTwoWayStatusFollowsAvailability(t *testing.T) {
	client := newFakeGitHub(repoWith("acme/api", testfixtures.PR(1)))
	h := startTwoWay(t, client, nil)
	if status := h.monitor.Status(); status.TwoWay == nil || *status.TwoWay != "Fake" {
		t.Fatalf("two_way = %v, want Fake", status.TwoWay)
	}
	before := h.count("status")

	h.onChange(twoway.Availability{Why: "handler exited (exit status 1)"})
	status := h.monitor.Status()
	if status.TwoWay != nil {
		t.Errorf("two_way = %q, want null while the handler is down", *status.TwoWay)
	}
	want := []string{"Two-way notifications are off: handler exited (exit status 1)"}
	if !reflect.DeepEqual(status.Warnings, want) {
		t.Errorf("warnings = %v, want %v", status.Warnings, want)
	}
	if got := h.count("status"); got != before+1 {
		t.Errorf("status events = %d, want %d", got, before+1)
	}

	h.onChange(twoway.Availability{Why: "handler exited (exit status 2)"})
	want = []string{"Two-way notifications are off: handler exited (exit status 2)"}
	if got := h.monitor.Status().Warnings; !reflect.DeepEqual(got, want) {
		t.Errorf("warnings = %v, want only the latest reason %v", got, want)
	}

	h.onChange(twoway.Availability{Ready: true, Name: "Fake"})
	status = h.monitor.Status()
	if status.TwoWay == nil || *status.TwoWay != "Fake" || len(status.Warnings) != 0 {
		t.Errorf("status = %+v, want Fake and no warning once ready", status)
	}
	if got := h.count("status"); got != before+3 {
		t.Errorf("status events = %d, want %d", got, before+3)
	}
}

func TestPostWhileUnavailableIsQuiet(t *testing.T) {
	client := newFakeGitHub(repoWith("acme/api", testfixtures.PR(1, testfixtures.Pending)))
	h := startTwoWay(t, client, twoWaySettings())
	h.twoWay.SetUnavailable(true)
	becomes(h)
	if got := h.posts(); len(got) != 0 {
		t.Errorf("posts = %+v, want none while the handler is down", got)
	}
	if got := h.toasts(); len(got) != 0 {
		t.Errorf("toasts = %v, want none: the status warning says it", got)
	}
}

func TestRetirementWaitsForTheHandler(t *testing.T) {
	h := readyMessage(t, twoWaySettings())
	h.twoWay.SetUnavailable(true)
	h.client.setRepo(repoWith("acme/api"))
	h.poll()
	message, found := h.messages()["1"]
	if !found || message.Retiring != "PR closed" || len(message.Keys) != 0 {
		t.Fatalf("messages = %+v, want message 1 waiting to be retired, with no keys", h.messages())
	}
	if h.hasToast("notification failed") {
		t.Errorf("toasts = %v, want none", h.toasts())
	}
	h.poll()
	if _, found := h.messages()["1"]; !found {
		t.Fatal("a poll while the handler is down should keep the message")
	}

	h.twoWay.SetUnavailable(false)
	h.onChange(twoway.Availability{Ready: true, Name: "Fake"})
	h.monitor.WaitIdle()
	if got := h.deletes(); !reflect.DeepEqual(got, []string{"1"}) {
		t.Errorf("deletes = %v, want message 1 once the handler is back", got)
	}
	if got := h.messages(); len(got) != 0 {
		t.Errorf("messages = %+v, want none", got)
	}
}

func TestSendTestWhileUnavailable(t *testing.T) {
	client := newFakeGitHub(repoWith("acme/api", testfixtures.PR(1)))
	h := startTwoWay(t, client, nil)
	h.twoWay.SetUnavailable(true)
	h.monitor.SendTest("acme/api", twoWaySettings()["acme/api"])
	h.monitor.WaitIdle()
	want := "Test two-way notification not sent: the handler isn't running"
	if got := h.severity(want); got != "warning" {
		t.Errorf("toasts = %v, want a warning %q", h.toasts(), want)
	}
	if h.hasToast("notification failed") {
		t.Errorf("toasts = %v, want no failure toast", h.toasts())
	}
}

func TestStopClosesTheChannel(t *testing.T) {
	client := newFakeGitHub(repoWith("acme/api", testfixtures.PR(1)))
	h := startTwoWay(t, client, nil)
	h.monitor.Stop()
	h.twoWay.Lock()
	closed := h.twoWay.Closed
	h.twoWay.Unlock()
	if !closed {
		t.Error("Stop should close the channel")
	}
	h.boot() // for the cleanup's Stop
}

func TestRetirementWaitingAtStartIsRetried(t *testing.T) {
	h := readyMessage(t, twoWaySettings())
	h.twoWay.SetUnavailable(true)
	h.client.setRepo(repoWith("acme/api"))
	h.poll()
	if got := h.messages()["1"].Retiring; got != "PR closed" {
		t.Fatalf("retiring = %q, want PR closed", got)
	}
	h.twoWay.SetUnavailable(false)
	h.restart()
	if got := h.deletes(); !reflect.DeepEqual(got, []string{"1"}) {
		t.Errorf("deletes = %v, want message 1 once a ready channel starts", got)
	}
	if got := h.messages(); len(got) != 0 {
		t.Errorf("messages = %+v, want none", got)
	}
}

// A handler that reports ready between refusing a delete and the refusal
// coming back must not leave the message waiting for the next ready report.
func TestRetirementRefusedAsTheHandlerComesBackIsRetried(t *testing.T) {
	h := readyMessage(t, twoWaySettings())
	h.twoWay.SetUnavailable(true)
	h.twoWay.Lock()
	refused := false
	h.twoWay.DeleteHook = func(_ context.Context, _ string, err error) error {
		if errors.Is(err, twoway.ErrUnavailable) && !refused {
			refused = true
			h.twoWay.SetUnavailable(false)
			h.onChange(twoway.Availability{Ready: true, Name: "Fake"})
		}
		return err
	}
	h.twoWay.Unlock()

	h.client.setRepo(repoWith("acme/api"))
	h.poll()
	if got := h.deletes(); !reflect.DeepEqual(got, []string{"1"}) {
		t.Errorf("deletes = %v, want message 1 retried once the handler is ready", got)
	}
	if got := h.messages(); len(got) != 0 {
		t.Errorf("messages = %+v, want none", got)
	}
}

// A delete cut short by Stop keeps the message, to retire after the restart.
func TestRetirementInFlightAtStopIsKept(t *testing.T) {
	h := readyMessage(t, twoWaySettings())
	entered := make(chan struct{})
	h.twoWay.Lock()
	h.twoWay.DeleteHook = func(ctx context.Context, _ string, _ error) error {
		close(entered)
		<-ctx.Done()
		return ctx.Err()
	}
	h.twoWay.Unlock()

	h.client.setRepo(repoWith("acme/api"))
	h.monitor.PollOnce(context.Background())
	<-entered
	h.monitor.Stop()
	if h.hasToast("notification failed") {
		t.Errorf("toasts = %v, want none", h.toasts())
	}
	if got := h.messages()["1"]; got.Retiring != "PR closed" {
		t.Errorf("messages = %+v, want message 1 waiting to be retired", h.messages())
	}

	h.twoWay.Lock()
	h.twoWay.DeleteHook = nil
	h.twoWay.Deletes = nil
	h.twoWay.Unlock()
	h.boot()
	if got := h.deletes(); !reflect.DeepEqual(got, []string{"1"}) {
		t.Errorf("deletes = %v, want message 1 after the restart", got)
	}
	if got := h.messages(); len(got) != 0 {
		t.Errorf("messages = %+v, want none", got)
	}
}
