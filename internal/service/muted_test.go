package service_test

import (
	"context"
	"testing"

	"github.com/acheris-labs/pr-mon/internal/models"
	"github.com/acheris-labs/pr-mon/internal/testfixtures"
)

func mute(t *testing.T, h *harness, number int, kind string) {
	t.Helper()
	if err := h.monitor.Perform(context.Background(), "acme/api", number,
		models.Action{Kind: kind}); err != nil {
		t.Fatal(err)
	}
}

// shown is the PR as clients see it.
func shown(t *testing.T, h *harness, number int) models.PullRequest {
	t.Helper()
	repo, _ := h.monitor.Repo("acme/api")
	for _, pr := range repo.PRs {
		if pr.Number == number {
			return pr
		}
	}
	t.Fatalf("PR %d isn't shown", number)
	return models.PullRequest{}
}

func TestMuteAndUnmute(t *testing.T) {
	client := newFakeGitHub(repoWith("acme/api", testfixtures.PR(1, testfixtures.Pending)))
	h := start(t, client, []string{"acme/api"}, mergeEvents())
	mute(t, h, 1, "mute")
	if !shown(t, h, 1).Muted {
		t.Fatal("the PR should show as muted")
	}
	if !h.hasToast("Muted notifications for acme/api#1") {
		t.Errorf("toasts = %v", h.toasts())
	}

	client.setRepo(repoWith("acme/api", testfixtures.PR(1, testfixtures.Failing)))
	h.poll()
	if sent := h.sent(); len(sent) != 0 {
		t.Errorf("a muted PR notified: %v", sent)
	}

	mute(t, h, 1, "unmute")
	if shown(t, h, 1).Muted {
		t.Fatal("the PR should no longer show as muted")
	}
	client.setRepo(repoWith("acme/api", testfixtures.PR(1)))
	h.poll()
	if !sentState(h, "READY", "") {
		t.Errorf("an unmuted PR should notify: %v", h.sent())
	}
}

func TestMutedPRsMergeQuietly(t *testing.T) {
	client := newFakeGitHub(repoWith("acme/api", testfixtures.PR(1, testfixtures.Pending)))
	h := start(t, client, []string{"acme/api"}, mergeEvents())
	mute(t, h, 1, "mute")
	arm(t, h, 1, false)
	client.setRepo(repoWith("acme/api", testfixtures.PR(1)))
	h.poll()
	h.monitor.WaitIdle()
	if len(client.callsMatching("merge ")) != 1 {
		t.Fatalf("the armed PR should have merged: %v", client.callsMatching("merge "))
	}
	if sent := h.sent(); len(sent) != 0 {
		t.Errorf("a muted PR notified about its merge: %v", sent)
	}
}

func TestClosedPRIsUnmutedUnlessTheListIsTruncated(t *testing.T) {
	client := newFakeGitHub(repoWith("acme/api",
		testfixtures.PR(1, testfixtures.Pending), testfixtures.PR(2, testfixtures.Pending)))
	h := start(t, client, []string{"acme/api"}, nil)
	mute(t, h, 1, "mute")
	mute(t, h, 2, "mute")

	// Gone from a truncated list: it may still be open further down.
	truncated := repoWith("acme/api", testfixtures.PR(2, testfixtures.Pending))
	truncated.PRTotal = 80
	client.setRepo(truncated)
	h.poll()
	if got := h.monitor.Muted("acme/api"); !got[1] {
		t.Errorf("muted = %v; a truncated list should keep PR 1 muted", got)
	}

	// Gone from a complete list: it closed.
	client.setRepo(repoWith("acme/api", testfixtures.PR(2, testfixtures.Pending)))
	h.poll()
	if got := h.monitor.Muted("acme/api"); got[1] || !got[2] {
		t.Errorf("muted = %v, want only PR 2", got)
	}
}

func TestRemoveRepoDropsMutedPRs(t *testing.T) {
	client := newFakeGitHub(repoWith("acme/api", testfixtures.PR(1, testfixtures.Pending)))
	h := start(t, client, []string{"acme/api"}, nil)
	mute(t, h, 1, "mute")
	if err := h.monitor.RemoveRepo("acme/api"); err != nil {
		t.Fatal(err)
	}
	if got := h.monitor.Muted("acme/api"); len(got) != 0 {
		t.Errorf("muted = %v; it should go with the repo", got)
	}
}
