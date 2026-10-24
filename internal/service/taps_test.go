package service_test

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acheris-labs/pr-mon/internal/models"
	"github.com/acheris-labs/pr-mon/internal/state"
	"github.com/acheris-labs/pr-mon/internal/testfixtures"
	"github.com/acheris-labs/pr-mon/internal/twoway"
)

const movedHead = "fff1234def5678abc1234def5678abc1234def56"

func (h *harness) acked() []twoway.Tap {
	h.twoWay.Lock()
	defer h.twoWay.Unlock()
	return append([]twoway.Tap{}, h.twoWay.Acked...)
}

// send delivers a tap without waiting for it to be handled.
func (h *harness) send(id, key string) {
	h.t.Helper()
	tap := twoway.Tap{MessageID: id, Key: key, Receipt: fmt.Sprintf("%s/%s", id, key)}
	select {
	case h.twoWay.Taps <- tap:
	case <-time.After(2 * time.Second):
		h.t.Fatal("nothing is receiving taps")
	}
}

// tap presses a button and waits until the backend has handled it.
func (h *harness) tap(id, key string) {
	h.t.Helper()
	before := len(h.acked())
	h.send(id, key)
	waitFor(h.t, func() bool { return len(h.acked()) > before })
	h.monitor.WaitIdle()
}

// keyOf is the live key of message id's first button of this kind.
func keyOf(t *testing.T, h *harness, id, kind string) string {
	t.Helper()
	message, found := h.messages()[id]
	if !found {
		t.Fatalf("message %s isn't tracked", id)
	}
	keys := []string{}
	for key := range message.Keys {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		button := message.Keys[key]
		if button.Kind == kind && (kind != "merge" || button.Method == "SQUASH") {
			return key
		}
	}
	t.Fatalf("message %s has no %s button: %+v", id, kind, message.Keys)
	return ""
}

func labels(message twoway.Message) []string {
	found := []string{}
	for _, row := range message.Buttons {
		for _, button := range row {
			found = append(found, button.Label)
		}
	}
	return found
}

// edited is the last edit of message id, which must have this line.
func edited(t *testing.T, h *harness, id, line string) twoway.Message {
	t.Helper()
	message, found := h.edit(id)
	if !found {
		t.Fatalf("message %s wasn't edited", id)
	}
	if !slices.Contains(message.Lines, line) {
		t.Fatalf("lines = %q, want %q", message.Lines, line)
	}
	return message
}

// rekeyed checks that message id is still tracked, that the tapped key is
// gone, and that the buttons shown carry exactly the keys now live.
func rekeyed(t *testing.T, h *harness, id, old string, shown twoway.Message) state.Message {
	t.Helper()
	message, found := h.messages()[id]
	if !found {
		t.Fatalf("message %s should still be tracked", id)
	}
	if _, live := message.Keys[old]; live {
		t.Errorf("the tapped key %s is still live", old)
	}
	showing := []string{}
	for _, row := range shown.Buttons {
		for _, button := range row {
			if button.Key != "" {
				showing = append(showing, button.Key)
			}
		}
	}
	live := []string{}
	for key := range message.Keys {
		live = append(live, key)
	}
	slices.Sort(showing)
	slices.Sort(live)
	if len(live) == 0 || !reflect.DeepEqual(showing, live) {
		t.Errorf("buttons show keys %v, live keys are %v", showing, live)
	}
	return message
}

func (h *harness) merges() []string { return h.client.callsMatching("merge ") }

func (h *harness) mergeHeads() []string {
	h.client.mutex.Lock()
	defer h.client.mutex.Unlock()
	return append([]string{}, h.client.mergeHeads...)
}

// failingMessage starts a two-way harness whose PR 1 went FAILING, with an
// Actions run to re-run and branch protection the user may bypass, and was
// posted about as message "1".
func failingMessage(t *testing.T) *harness {
	t.Helper()
	client := newFakeGitHub(repoWith("acme/api", testfixtures.PR(1, testfixtures.Pending)))
	h := startTwoWay(t, client, twoWaySettings())
	becomes(h, testfixtures.Failing, failedActionsCheck, canBypass)
	if got := h.posts(); len(got) != 1 || got[0].Status != "FAILING" {
		t.Fatalf("posts = %+v, want one FAILING message", got)
	}
	return h
}

func TestTapMergeMergesAndDeletes(t *testing.T) {
	h := readyMessage(t, twoWaySettings())
	key := keyOf(t, h, "1", "merge")
	h.tap("1", key)
	if got := h.merges(); !reflect.DeepEqual(got, []string{"merge PR_1 SQUASH"}) {
		t.Fatalf("merges = %v, want one squash", got)
	}
	if got := h.mergeHeads(); !reflect.DeepEqual(got, []string{testfixtures.HeadSha}) {
		t.Errorf("expected heads = %v, want the recorded head", got)
	}
	if got := h.client.callsMatching("delete "); len(got) != 1 {
		t.Errorf("branch deletes = %v, want one", got)
	}
	if got := h.deletes(); !reflect.DeepEqual(got, []string{"1"}) {
		t.Errorf("deletes = %v, want message 1", got)
	}
	if got := h.messages(); len(got) != 0 {
		t.Errorf("messages = %+v, want none tracked", got)
	}
	if got := h.acked(); len(got) != 1 || got[0].Key != key {
		t.Errorf("acked = %+v, want the tap", got)
	}
}

func TestTapTwiceRunsOnce(t *testing.T) {
	h := readyMessage(t, twoWaySettings())
	gate := make(chan struct{})
	var release sync.Once
	open := func() { release.Do(func() { close(gate) }) }
	t.Cleanup(open) // before Stop, should the test fail with the merge held
	h.client.mutex.Lock()
	h.client.mergeGate = gate
	h.client.mutex.Unlock()

	key := keyOf(t, h, "1", "merge")
	h.send("1", key)
	h.send("1", key)
	// One tap is stuck in the merge; the other must be handled meanwhile.
	waitFor(t, func() bool { return len(h.acked()) == 1 })
	if _, found := h.edit("1"); found {
		t.Error("the second tap shouldn't touch a message being acted on")
	}
	open()
	waitFor(t, func() bool { return len(h.acked()) == 2 })
	h.monitor.WaitIdle()
	if got := h.merges(); len(got) != 1 {
		t.Errorf("merges = %v, want exactly one", got)
	}
	if got := h.deletes(); !reflect.DeepEqual(got, []string{"1"}) {
		t.Errorf("deletes = %v, want message 1 once", got)
	}
}

func TestTapHeadMovedRefuses(t *testing.T) {
	h := readyMessage(t, twoWaySettings())
	key := keyOf(t, h, "1", "merge")
	// A commit lands; the backend hasn't polled since.
	h.client.setRepo(repoWith("acme/api", testfixtures.PR(1, func(pr *models.PullRequest) {
		pr.HeadSha = models.Ptr(movedHead)
	})))
	h.tap("1", key)
	if got := h.merges(); len(got) != 0 {
		t.Fatalf("merges = %v, want none", got)
	}
	shown := edited(t, h, "1", "The PR changed since this message")
	if got := labels(shown); !slices.Contains(got, "Squash & merge") {
		t.Errorf("labels = %v, want the current buttons", got)
	}
	if message := rekeyed(t, h, "1", key, shown); message.HeadSha != movedHead {
		t.Errorf("head = %s, want the head the buttons were built from", message.HeadSha)
	}
}

func TestTapNoLongerAvailable(t *testing.T) {
	h := failingMessage(t)
	key := keyOf(t, h, "1", "rerun_checks")
	// Still failing, but no longer through a GitHub Actions run.
	h.client.setRepo(repoWith("acme/api", testfixtures.PR(1, testfixtures.Failing, canBypass,
		func(pr *models.PullRequest) {
			pr.Checks = []models.Check{testfixtures.CheckRun("external", "FAILURE")}
		})))
	h.tap("1", key)
	if got := h.client.callsMatching("rerun "); len(got) != 0 {
		t.Fatalf("reruns = %v, want none", got)
	}
	shown := edited(t, h, "1", "No longer available")
	if got := labels(shown); !reflect.DeepEqual(got, []string{"Force merge", "Open", "Dismiss"}) {
		t.Errorf("labels = %v, want the buttons still on offer", got)
	}
	rekeyed(t, h, "1", key, shown)
}

func TestTapMergeFailureKeepsMessage(t *testing.T) {
	h := readyMessage(t, twoWaySettings())
	h.client.mutex.Lock()
	h.client.mergeErr = errors.New("Required status check is expected")
	h.client.mutex.Unlock()
	key := keyOf(t, h, "1", "merge")
	h.tap("1", key)
	if got := h.merges(); len(got) != 1 {
		t.Fatalf("merges = %v, want one attempt", got)
	}
	shown := edited(t, h, "1", "⚠️ Required status check is expected")
	if got := labels(shown); !slices.Contains(got, "Squash & merge") {
		t.Errorf("labels = %v, want the buttons back", got)
	}
	rekeyed(t, h, "1", key, shown)
	if got := h.deletes(); len(got) != 0 {
		t.Errorf("deletes = %v, want none", got)
	}
}

// askForceMerge taps Force merge on a failing message and checks the question.
func askForceMerge(t *testing.T, h *harness) {
	t.Helper()
	key := keyOf(t, h, "1", "force_merge")
	h.tap("1", key)
	if got := h.merges(); len(got) != 0 {
		t.Fatalf("merges = %v, want none before confirming", got)
	}
	shown := edited(t, h, "1", "Force merge #1?")
	// Cancel first: a second tap where Force merge or Re-run was mustn't confirm.
	if got := labels(shown); !reflect.DeepEqual(got, []string{"Cancel", "Confirm"}) {
		t.Errorf("labels = %v, want Cancel then Confirm", got)
	}
	message := rekeyed(t, h, "1", key, shown)
	if message.ConfirmAskedAt == "" {
		t.Error("the message should be confirming")
	}
	confirm := message.Keys[keyOf(t, h, "1", "confirm_force_merge")]
	if confirm != (state.Button{Kind: "confirm_force_merge", Method: "SQUASH", DeleteBranch: true}) {
		t.Errorf("confirm = %+v, want the force merge's method and branch choice", confirm)
	}
}

func TestForceMergeAsksFirst(t *testing.T) {
	h := failingMessage(t)
	askForceMerge(t, h)
}

func TestConfirmForceMerge(t *testing.T) {
	h := failingMessage(t)
	askForceMerge(t, h)
	h.monitor.SetConfirmGuard(0)
	h.twoWay.Lock()
	h.twoWay.DeleteErr = fmt.Errorf("telegram: %w", twoway.ErrTooOld)
	h.twoWay.Unlock()
	h.tap("1", keyOf(t, h, "1", "confirm_force_merge"))
	if got := h.merges(); !reflect.DeepEqual(got, []string{"merge PR_1 SQUASH"}) {
		t.Fatalf("merges = %v, want one squash", got)
	}
	if got := h.mergeHeads(); !reflect.DeepEqual(got, []string{testfixtures.HeadSha}) {
		t.Errorf("expected heads = %v, want the recorded head", got)
	}
	if got := h.deletes(); !reflect.DeepEqual(got, []string{"1"}) {
		t.Errorf("deletes = %v, want message 1", got)
	}
	if shown := edited(t, h, "1", "✅ Merged"); len(shown.Buttons) != 0 {
		t.Errorf("buttons = %+v, want none on a message too old to delete", shown.Buttons)
	}
	if got := h.messages(); len(got) != 0 {
		t.Errorf("messages = %+v, want none tracked", got)
	}
}

func TestCancelRestores(t *testing.T) {
	h := failingMessage(t)
	askForceMerge(t, h)
	key := keyOf(t, h, "1", "cancel")
	h.tap("1", key)
	if got := h.merges(); len(got) != 0 {
		t.Fatalf("merges = %v, want none", got)
	}
	shown, _ := h.edit("1")
	want := []string{"Re-run failed checks", "Force merge", "Open", "Dismiss"}
	if got := labels(shown); !reflect.DeepEqual(got, want) {
		t.Errorf("labels = %v, want %v", got, want)
	}
	if slices.Contains(shown.Lines, "Force merge #1?") {
		t.Errorf("lines = %q, want the question gone", shown.Lines)
	}
	if message := rekeyed(t, h, "1", key, shown); message.ConfirmAskedAt != "" {
		t.Error("the message should no longer be confirming")
	}
}

func TestDismissDeletes(t *testing.T) {
	h := readyMessage(t, twoWaySettings())
	h.tap("1", keyOf(t, h, "1", "dismiss"))
	if got := h.deletes(); !reflect.DeepEqual(got, []string{"1"}) {
		t.Errorf("deletes = %v, want message 1", got)
	}
	if got := h.messages(); len(got) != 0 {
		t.Errorf("messages = %+v, want none tracked", got)
	}
	if got := h.merges(); len(got) != 0 {
		t.Errorf("merges = %v, want none", got)
	}
}

func TestStaleKey(t *testing.T) {
	h := readyMessage(t, twoWaySettings())
	h.tap("1", "zzzzzzzz")
	if got := h.merges(); len(got) != 0 {
		t.Fatalf("merges = %v, want none", got)
	}
	shown := edited(t, h, "1", "That button had expired")
	if got := labels(shown); !slices.Contains(got, "Squash & merge") {
		t.Errorf("labels = %v, want the current buttons", got)
	}
	rekeyed(t, h, "1", "zzzzzzzz", shown)
	if got := h.acked(); len(got) != 1 {
		t.Errorf("acked = %+v, want the tap", got)
	}
}

func TestUnknownMessageLosesButtons(t *testing.T) {
	h := readyMessage(t, twoWaySettings())
	h.tap("99", "abcdefgh")
	shown := edited(t, h, "99", "No longer tracked")
	if shown.Title == "" {
		t.Error("title is empty, which Telegram shows as an empty bold line")
	}
	if len(shown.Buttons) != 0 {
		t.Errorf("buttons = %+v, want none", shown.Buttons)
	}
	if _, found := h.messages()["99"]; found {
		t.Error("an unknown message shouldn't become tracked")
	}
	if got := h.acked(); len(got) != 1 {
		t.Errorf("acked = %+v, want the tap", got)
	}
}

func TestRerunChecks(t *testing.T) {
	h := failingMessage(t)
	h.twoWay.Lock()
	h.twoWay.DeleteErr = fmt.Errorf("telegram: %w", twoway.ErrTooOld)
	h.twoWay.Unlock()
	h.tap("1", keyOf(t, h, "1", "rerun_checks"))
	if got := h.client.callsMatching("rerun "); !reflect.DeepEqual(got, []string{"rerun acme/api [7]"}) {
		t.Fatalf("reruns = %v, want run 7", got)
	}
	if got := h.merges(); len(got) != 0 {
		t.Errorf("merges = %v, want none", got)
	}
	edited(t, h, "1", "🔁 Re-running failed checks")
	if got := h.messages(); len(got) != 0 {
		t.Errorf("messages = %+v, want none tracked", got)
	}
}

// headMoves gives PR 1 a new head, still failing, and polls, so the backend
// knows the new head but the message, whose status hasn't changed, stays.
func headMoves(t *testing.T, h *harness) {
	t.Helper()
	becomes(h, testfixtures.Failing, failedActionsCheck, canBypass, func(pr *models.PullRequest) {
		pr.HeadSha = models.Ptr(movedHead)
	})
	if message, found := h.messages()["1"]; !found || message.HeadSha != testfixtures.HeadSha {
		t.Fatalf("message = %+v, want it still tracked for the old head", message)
	}
}

func TestCancelAnnouncesMovedHead(t *testing.T) {
	h := failingMessage(t)
	headMoves(t, h)
	askForceMerge(t, h)
	h.tap("1", keyOf(t, h, "1", "cancel"))
	shown := edited(t, h, "1", "The PR changed since this message")
	if got := labels(shown); !slices.Contains(got, "Force merge") {
		t.Errorf("labels = %v, want the current buttons", got)
	}
	if got := h.messages()["1"].HeadSha; got != movedHead {
		t.Errorf("head = %s, want the announced head", got)
	}
	if got := h.merges(); len(got) != 0 {
		t.Errorf("merges = %v, want none", got)
	}
}

func TestExpiredKeyAnnouncesMovedHead(t *testing.T) {
	h := failingMessage(t)
	headMoves(t, h)
	h.tap("1", "zzzzzzzz")
	shown := edited(t, h, "1", "That button had expired")
	if !slices.Contains(shown.Lines, "The PR changed since this message") {
		t.Errorf("lines = %q, want the head move announced", shown.Lines)
	}
}

func TestConfirmRefusesMovedHead(t *testing.T) {
	h := failingMessage(t)
	askForceMerge(t, h)
	h.monitor.SetConfirmGuard(0)
	// A commit lands while the question is showing.
	h.client.setRepo(repoWith("acme/api", testfixtures.PR(1, testfixtures.Failing,
		failedActionsCheck, canBypass, func(pr *models.PullRequest) { pr.HeadSha = models.Ptr(movedHead) })))
	h.tap("1", keyOf(t, h, "1", "confirm_force_merge"))
	if got := h.merges(); len(got) != 0 {
		t.Fatalf("merges = %v, want none", got)
	}
	shown := edited(t, h, "1", "The PR changed since this message")
	if n := strings.Count(strings.Join(shown.Lines, "\n"), "The PR changed"); n != 1 {
		t.Errorf("lines = %q, want the change said once", shown.Lines)
	}
	if got := labels(shown); !slices.Contains(got, "Force merge") {
		t.Errorf("labels = %v, want Force merge offered again, to be confirmed again", got)
	}
}

// A Confirm that comes too soon after the question is a stray second tap:
// the question is asked again, with fresh keys.
func TestConfirmTooSoonAsksAgain(t *testing.T) {
	h := failingMessage(t)
	askForceMerge(t, h)
	key := keyOf(t, h, "1", "confirm_force_merge")
	h.tap("1", key)
	if got := h.merges(); len(got) != 0 {
		t.Fatalf("merges = %v, want none from a confirm this soon", got)
	}
	shown := edited(t, h, "1", "Force merge #1?")
	if got := labels(shown); !reflect.DeepEqual(got, []string{"Cancel", "Confirm"}) {
		t.Errorf("labels = %v, want the question again", got)
	}
	rekeyed(t, h, "1", key, shown)
	h.monitor.SetConfirmGuard(0)
	h.tap("1", keyOf(t, h, "1", "confirm_force_merge"))
	if got := h.merges(); !reflect.DeepEqual(got, []string{"merge PR_1 SQUASH"}) {
		t.Errorf("merges = %v, want one once confirmed in time", got)
	}
}

// The second tap of a double-tapped Dismiss finds the message deleted; that
// is no failure.
func TestEditOfGoneMessageIsQuiet(t *testing.T) {
	h := readyMessage(t, twoWaySettings())
	h.twoWay.Lock()
	h.twoWay.EditErr = fmt.Errorf("telegram: %w", twoway.ErrGone)
	h.twoWay.Unlock()
	h.tap("99", "abcdefgh")
	if _, found := h.edit("99"); !found {
		t.Fatal("message 99 wasn't edited")
	}
	if h.hasToast("notification failed") {
		t.Errorf("toasts = %q, want none for a message already gone", h.toasts())
	}
}

// A merge is always pinned to a commit; without a known head it isn't run.
func TestTapMergeWithoutHeadRefuses(t *testing.T) {
	noHead := func(pr *models.PullRequest) { pr.HeadSha = nil }
	client := newFakeGitHub(repoWith("acme/api", testfixtures.PR(1, testfixtures.Pending, noHead)))
	h := startTwoWay(t, client, twoWaySettings())
	becomes(h, noHead)
	if got := h.posts(); len(got) != 1 || got[0].Status != "READY" {
		t.Fatalf("posts = %+v, want one READY message", got)
	}
	key := keyOf(t, h, "1", "merge")
	h.tap("1", key)
	if got := h.merges(); len(got) != 0 {
		t.Fatalf("merges = %v, want none without a head to pin", got)
	}
	edited(t, h, "1", "⚠️ GitHub hasn't said which commit the PR is at")
	if got := h.deletes(); len(got) != 0 {
		t.Errorf("deletes = %v, want the message kept", got)
	}
}

func (h *harness) failReceive(err error) {
	h.t.Helper()
	select {
	case h.twoWay.ReceiveErrs <- err:
	case <-time.After(2 * time.Second):
		h.t.Fatal("nothing is receiving taps")
	}
}

func (h *harness) count(kind string) int {
	return len(slices.DeleteFunc(h.kinds(), func(k string) bool { return k != kind }))
}

// A failing receive is logged and retried; whether two-way works is the
// channel's to report, so it adds no warning.
func TestReceiveErrorIsRetriedQuietly(t *testing.T) {
	h := readyMessage(t, twoWaySettings())
	h.monitor.SetReceiveBackoff(time.Millisecond)
	before := h.count("status")
	h.failReceive(errors.New("sqs ReceiveMessage: 400 Bad Request: queue does not exist"))
	h.failReceive(errors.New("sqs ReceiveMessage: 403 Forbidden"))
	h.tap("99", "abcdefgh") // still receiving
	if got := h.monitor.Status().Warnings; len(got) != 0 {
		t.Errorf("warnings = %v, want none", got)
	}
	if got := h.count("status"); got != before {
		t.Errorf("status events = %d, want %d", got, before)
	}
}

// A tap on a message waiting to be retired retries the delete and does nothing else.
func TestTapOnARetiringMessageRetriesTheDelete(t *testing.T) {
	h := readyMessage(t, twoWaySettings())
	key := keyOf(t, h, "1", "merge")
	h.twoWay.SetUnavailable(true)
	h.client.setRepo(repoWith("acme/api"))
	h.poll()
	if got := h.messages()["1"].Retiring; got != "PR closed" {
		t.Fatalf("retiring = %q, want PR closed", got)
	}
	h.twoWay.SetUnavailable(false)
	h.tap("1", key)
	if got := h.deletes(); !reflect.DeepEqual(got, []string{"1"}) {
		t.Errorf("deletes = %v, want message 1", got)
	}
	if got := h.acked(); len(got) != 1 || got[0].MessageID != "1" {
		t.Errorf("acked = %+v, want the tap", got)
	}
	if _, found := h.edit("1"); found {
		t.Error("the message should be deleted, not edited")
	}
	if got := h.merges(); len(got) != 0 {
		t.Errorf("merges = %v, want none", got)
	}
	if got := h.messages(); len(got) != 0 {
		t.Errorf("messages = %+v, want none", got)
	}
}
