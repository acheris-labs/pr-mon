package service_test

import (
	"context"
	"testing"

	"github.com/acheris-labs/pr-mon/internal/config"
	"github.com/acheris-labs/pr-mon/internal/github"
	"github.com/acheris-labs/pr-mon/internal/models"
	"github.com/acheris-labs/pr-mon/internal/testfixtures"
)

var squash = models.MergeSquash

// armNoWait puts a PR on merge-when-ready without waiting for any merge it starts.
func armNoWait(t *testing.T, h *harness, number int, deleteBranch bool) {
	t.Helper()
	action := models.Action{Kind: "arm_merge", Method: &squash, DeleteBranch: deleteBranch}
	if err := h.monitor.Perform(context.Background(), "acme/api", number, action); err != nil {
		t.Fatal(err)
	}
}

// arm also waits for the merge it may start.
func arm(t *testing.T, h *harness, number int, deleteBranch bool) {
	t.Helper()
	armNoWait(t, h, number, deleteBranch)
	h.monitor.WaitIdle()
}

// mergeEvents is a settings set that notifies about pr-mon's own merges.
func mergeEvents() map[string]config.NotifyConfig {
	settings := config.NewNotifyConfig()
	settings.ScriptEnabled = true
	settings.Script = "im"
	return map[string]config.NotifyConfig{"acme/api": settings}
}

// sentState reports whether a notification went out for this state, with the
// given reason when one is expected.
func sentState(h *harness, state, reason string) bool {
	for _, variables := range h.sent() {
		if variables["PR_STATE"] == state && (reason == "" || variables["PR_REASON"] == reason) {
			return true
		}
	}
	return false
}

func TestArmAndDisarm(t *testing.T) {
	client := newFakeGitHub(repoWith("acme/api", testfixtures.PR(1, testfixtures.Pending)))
	h := start(t, client, []string{"acme/api"}, nil)
	arm(t, h, 1, true)

	armed := h.monitor.Armed("acme/api")
	if merge, found := armed[1]; !found || merge.Method != models.MergeSquash || !merge.DeleteBranch {
		t.Fatalf("armed = %+v", armed)
	}
	if !h.hasToast("pr-mon will merge acme/api#1 when it's ready (squash)") {
		t.Errorf("toasts = %v", h.toasts())
	}
	if len(client.callsMatching("merge ")) != 0 {
		t.Error("a PR that isn't ready should not be merged")
	}

	if err := h.monitor.Perform(context.Background(), "acme/api", 1,
		models.Action{Kind: "disarm_merge"}); err != nil {
		t.Fatal(err)
	}
	if len(h.monitor.Armed("acme/api")) != 0 {
		t.Error("disarming should clear it")
	}
	if !h.hasToast("Cancelled merge when ready for acme/api#1") {
		t.Errorf("toasts = %v", h.toasts())
	}
}

func TestMergesWhenStrictlyReady(t *testing.T) {
	client := newFakeGitHub(repoWith("acme/api", testfixtures.PR(1, testfixtures.Pending)))
	h := start(t, client, []string{"acme/api"}, mergeEvents())
	arm(t, h, 1, true)

	client.setRepo(repoWith("acme/api", testfixtures.PR(1)))
	h.poll()
	h.monitor.WaitIdle()

	if got := client.callsMatching("merge "); len(got) != 1 {
		t.Fatalf("merge calls = %v", got)
	}
	client.mutex.Lock()
	heads := append([]string{}, client.mergeHeads...)
	client.mutex.Unlock()
	if len(heads) != 1 || heads[0] != testfixtures.HeadSha {
		t.Errorf("expected head = %v, want the head just fetched", heads)
	}
	if len(client.callsMatching("delete ")) != 1 {
		t.Error("the branch should be deleted")
	}
	if len(h.monitor.Armed("acme/api")) != 0 {
		t.Error("a merged PR should be disarmed")
	}
	if !h.hasToast("Merged acme/api#1 (pr-mon auto-merge, squash)") {
		t.Errorf("toasts = %v", h.toasts())
	}
	// Deliveries run concurrently, and the refresh after the merge can notify
	// too, so look for the one that matters rather than assuming an order.
	if !sentState(h, "MERGED", "") {
		t.Errorf("sent = %v, want a MERGED notification", h.sent())
	}
}

func TestStaysArmedUntilStrictlyReady(t *testing.T) {
	unstable := func(pr *models.PullRequest) {
		pr.MergeState = "UNSTABLE"
		pr.CheckState = models.Ptr("FAILURE")
	}
	client := newFakeGitHub(repoWith("acme/api", testfixtures.PR(1, testfixtures.Pending)))
	h := start(t, client, []string{"acme/api"}, nil)
	arm(t, h, 1, false)

	client.setRepo(repoWith("acme/api", testfixtures.PR(1, unstable)))
	h.poll()
	h.monitor.WaitIdle()
	if len(client.callsMatching("merge ")) != 0 {
		t.Error("an unstable PR is ready but not strictly ready")
	}
	if len(h.monitor.Armed("acme/api")) != 1 {
		t.Error("it should stay armed")
	}
}

func TestRetryableMergeErrorKeepsItArmed(t *testing.T) {
	client := newFakeGitHub(repoWith("acme/api", testfixtures.PR(1)))
	client.mergeErrs = []error{
		&github.Error{Message: "Head branch was modified. Review and try the merge again."},
	}
	h := start(t, client, []string{"acme/api"}, nil)
	arm(t, h, 1, false)
	h.monitor.WaitIdle()

	if len(h.monitor.Armed("acme/api")) != 1 {
		t.Error("a head that moved is retried, so it stays armed")
	}
	if h.hasToast("failed") {
		t.Errorf("a race should be quiet; toasts = %v", h.toasts())
	}
	// The next poll merges it.
	h.poll()
	h.monitor.WaitIdle()
	if got := client.callsMatching("merge "); len(got) != 2 {
		t.Errorf("merge calls = %v, want a retry", got)
	}
	if len(h.monitor.Armed("acme/api")) != 0 {
		t.Error("the retry should have merged it")
	}
}

func TestOtherMergeErrorDisarmsAndReports(t *testing.T) {
	client := newFakeGitHub(repoWith("acme/api", testfixtures.PR(1)))
	client.mergeErr = &github.Error{Message: "Pull request is not mergeable"}
	h := start(t, client, []string{"acme/api"}, mergeEvents())
	arm(t, h, 1, false)
	h.monitor.WaitIdle()

	if len(h.monitor.Armed("acme/api")) != 0 {
		t.Error("a real failure should disarm")
	}
	if !h.hasToast("pr-mon auto-merge failed for acme/api#1: Pull request is not mergeable") {
		t.Errorf("toasts = %v", h.toasts())
	}
	if !sentState(h, "MERGE_FAILED", "Pull request is not mergeable") {
		t.Errorf("sent = %v, want MERGE_FAILED with the reason", h.sent())
	}
}

func TestDeleteBranchFailureIsOnlyAWarning(t *testing.T) {
	client := newFakeGitHub(repoWith("acme/api", testfixtures.PR(1)))
	client.deleteErr = &github.Error{Message: "no permission"}
	h := start(t, client, []string{"acme/api"}, nil)
	arm(t, h, 1, true)
	h.monitor.WaitIdle()

	if !h.hasToast("Merged acme/api#1 (pr-mon auto-merge, squash)") {
		t.Errorf("the merge itself worked; toasts = %v", h.toasts())
	}
	if !h.hasToast("couldn't delete feature-1: no permission") {
		t.Errorf("toasts = %v", h.toasts())
	}
}

func TestClosedPRIsDisarmedUnlessTheListIsTruncated(t *testing.T) {
	client := newFakeGitHub(repoWith("acme/api", testfixtures.PR(1, testfixtures.Pending)))
	h := start(t, client, []string{"acme/api"}, nil)
	arm(t, h, 1, false)

	// Gone from a complete list: it closed.
	client.setRepo(repoWith("acme/api", testfixtures.PR(2, testfixtures.Pending)))
	h.poll()
	h.monitor.WaitIdle()
	if len(h.monitor.Armed("acme/api")) != 0 {
		t.Error("a PR missing from a complete list has closed")
	}

	// Gone from a truncated list: it may still be open further down.
	arm(t, h, 2, false)
	truncated := repoWith("acme/api", testfixtures.PR(3, testfixtures.Pending))
	truncated.PRTotal = 80
	client.setRepo(truncated)
	h.poll()
	h.monitor.WaitIdle()
	if len(h.monitor.Armed("acme/api")) != 1 {
		t.Error("a truncated list should not disarm anything")
	}
}

func TestNoSecondMergeWhileOneIsInFlight(t *testing.T) {
	client := newFakeGitHub(repoWith("acme/api", testfixtures.PR(1)))
	gate := make(chan struct{})
	client.mutex.Lock()
	client.mergeGate = gate
	client.mutex.Unlock()
	h := start(t, client, []string{"acme/api"}, nil)
	armNoWait(t, h, 1, false)
	waitFor(t, h.monitor.Merging)

	if !h.monitor.Merging() {
		t.Error("a merge in flight should be visible")
	}
	h.monitor.PollOnce(context.Background()) // would start a second merge if nothing stopped it
	if merges := len(client.callsMatching("merge ")); merges != 1 {
		t.Errorf("merge calls = %d, want one", merges)
	}
	close(gate)
	h.monitor.WaitIdle()
	if h.monitor.Merging() {
		t.Error("the merge finished, so nothing is in flight")
	}
}

func TestRemoveRepoDropsArmedMerges(t *testing.T) {
	client := newFakeGitHub(repoWith("acme/api", testfixtures.PR(1, testfixtures.Pending)))
	h := start(t, client, []string{"acme/api"}, nil)
	arm(t, h, 1, false)
	if err := h.monitor.RemoveRepo("acme/api"); err != nil {
		t.Fatal(err)
	}
	if len(h.monitor.Armed("acme/api")) != 0 {
		t.Error("armed merges should go with the repo")
	}
}

func TestArmedPRsShowInTheActionMenu(t *testing.T) {
	noNative := func(repo *models.Repo) { repo.AutoMergeAllowed = false }
	repo := repoWith("acme/api", testfixtures.PR(1, testfixtures.Pending))
	noNative(&repo)
	client := newFakeGitHub(repo)
	h := start(t, client, []string{"acme/api"}, nil)
	arm(t, h, 1, false)

	current, _ := h.monitor.Repo("acme/api")
	found := ""
	for _, option := range current.PRs[0].Actions {
		if option.Key == "auto_merge" {
			found = option.Kind
		}
	}
	if found != "disarm_merge" {
		t.Errorf("auto-merge option = %q, want disarm_merge", found)
	}
}
