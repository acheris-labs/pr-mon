package service_test

import (
	"context"
	"testing"
	"time"

	"github.com/acheris-labs/pr-mon/internal/config"
	"github.com/acheris-labs/pr-mon/internal/models"
	"github.com/acheris-labs/pr-mon/internal/service"
	"github.com/acheris-labs/pr-mon/internal/testfixtures"
)

// fetched is how often exactly these PRs were fetched together.
func fetched(client *fakeGitHub, call string) int {
	count := 0
	for _, made := range client.callsMatching("fetch") {
		if made == call {
			count++
		}
	}
	return count
}

// look is one scheduled conditional check of acme/api.
func look(h *harness) {
	h.monitor.Look(context.Background(), "acme/api")
	h.monitor.WaitIdle()
}

func TestAQuietRepoCostsNothing(t *testing.T) {
	client := newFakeGitHub(repoWith("acme/api", testfixtures.PR(1), testfixtures.PR(2)))
	h := start(t, client, []string{"acme/api"}, nil)
	before := len(client.callsMatching("fetch"))

	look(h)
	look(h)
	if got := client.callsMatching("fetch"); len(got) != before {
		t.Errorf("nothing changed, so no PR should be fetched: %v", got)
	}
	// It still asked, twice, and both answers were conditional.
	if got := client.callsMatching("list acme/api"); len(got) < 2 {
		t.Errorf("calls = %v", client.callsMatching("list"))
	}
}

func TestOnlyChangedPRsAreFetched(t *testing.T) {
	client := newFakeGitHub(repoWith("acme/api",
		testfixtures.PR(1, testfixtures.Pending), testfixtures.PR(2)))
	h := start(t, client, []string{"acme/api"}, nil)

	// #1's checks passed: GitHub's list shows it changed, #2 is untouched.
	client.setRepo(repoWith("acme/api", testfixtures.PR(1), testfixtures.PR(2)))
	client.touch("acme/api", 1, "2026-09-20T12:00:00Z")
	look(h)

	if fetched(client, "fetch acme/api [1]") != 1 {
		t.Errorf("should fetch #1 alone, got %v", client.callsMatching("fetch"))
	}
	if got := pr(t, h, "acme/api", 1); got.Status != models.StatusReady {
		t.Errorf("#1 = %s, want the new status", got.Status)
	}
}

func TestAPushToTheBaseBranchRefetchesEveryPR(t *testing.T) {
	client := newFakeGitHub(repoWith("acme/api", testfixtures.PR(1), testfixtures.PR(2)))
	h := start(t, client, []string{"acme/api"}, nil)

	// Nothing about the PRs changed, but they all merge differently now.
	client.setRepo(repoWith("acme/api",
		testfixtures.PR(1, testfixtures.Behind), testfixtures.PR(2, testfixtures.Behind)))
	client.basePushed("acme/api", "deadbee")
	look(h)

	if fetched(client, "fetch acme/api [1 2]") != 1 {
		t.Errorf("should refetch both, got %v", client.callsMatching("fetch"))
	}
	if got := pr(t, h, "acme/api", 2); got.Status != models.StatusBehind {
		t.Errorf("#2 = %s", got.Status)
	}
}

func TestAClosedPRLeavesTheList(t *testing.T) {
	client := newFakeGitHub(repoWith("acme/api", testfixtures.PR(1), testfixtures.PR(2)))
	h := start(t, client, []string{"acme/api"}, nil)

	client.setRepo(repoWith("acme/api", testfixtures.PR(2)))
	client.touch("acme/api", 2, "2026-09-20T12:00:00Z")
	look(h)

	repo, _ := h.monitor.Repo("acme/api")
	if len(repo.PRs) != 1 || repo.PRs[0].Number != 2 || repo.PRTotal != 1 {
		t.Errorf("repo = %+v", repo.PRs)
	}
}

// checking is a PR whose checks are running right now.
func checking(pr *models.PullRequest) {
	testfixtures.Pending(pr)
	pr.Checks = []models.Check{{Name: "ci", State: "IN_PROGRESS", StartedAt: models.Ptr(justNow())}}
	pr.ChecksTotal = 1
	pr.LastCommitAt = models.Ptr(justNow())
}

func justNow() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func TestPRsInFlightRefreshWithoutTheirRepo(t *testing.T) {
	client := newFakeGitHub(repoWith("acme/api",
		testfixtures.PR(1, checking), testfixtures.PR(2)))
	h := start(t, client, []string{"acme/api"}, nil)
	before := len(client.callsMatching("list"))

	// #1's checks finish. Its updated_at doesn't move — check runs often don't
	// touch it — so only the in-flight pass sees it.
	client.setRepo(repoWith("acme/api", testfixtures.PR(1), testfixtures.PR(2)))
	h.monitor.RefreshInFlight(context.Background())
	h.monitor.WaitIdle()

	if fetched(client, "fetch acme/api [1]") != 1 {
		t.Errorf("should fetch the moving PR alone, got %v", client.callsMatching("fetch"))
	}
	if got := len(client.callsMatching("list")); got != before {
		t.Error("the repo itself shouldn't be listed again")
	}
	if got := pr(t, h, "acme/api", 1); got.Status != models.StatusReady {
		t.Errorf("#1 = %s", got.Status)
	}
	// Nothing moving now, so the next pass asks for nothing.
	calls := len(client.callsMatching("fetch"))
	h.monitor.RefreshInFlight(context.Background())
	h.monitor.WaitIdle()
	if got := len(client.callsMatching("fetch")); got != calls {
		t.Errorf("fetches = %d, want %d", got, calls)
	}
}

func TestChecksPendingForAgesAreNotWatched(t *testing.T) {
	// The fixture's PR has been sitting with a pending check since last week.
	client := newFakeGitHub(repoWith("acme/api", testfixtures.PR(1, testfixtures.Pending)))
	h := start(t, client, []string{"acme/api"}, nil)
	calls := len(client.callsMatching("fetch acme/api ["))

	h.monitor.RefreshInFlight(context.Background())
	h.monitor.WaitIdle()
	if got := len(client.callsMatching("fetch acme/api [")); got != calls {
		t.Errorf("a stale PR shouldn't be watched every few seconds: %v",
			client.callsMatching("fetch"))
	}
	// Unless it is what a client is looking at.
	h.monitor.SetFocus("client-1", service.Focus{Repo: "acme/api", Number: 1})
	h.monitor.RefreshInFlight(context.Background())
	h.monitor.WaitIdle()
	if fetched(client, "fetch acme/api [1]") != 1 {
		t.Errorf("the PR on screen should be watched: %v", client.callsMatching("fetch"))
	}
}

func TestArmedPRsStayInFlight(t *testing.T) {
	client := newFakeGitHub(repoWith("acme/api", testfixtures.PR(1, testfixtures.Behind)))
	h := start(t, client, []string{"acme/api"}, nil)
	arm(t, h, 1, false)
	calls := len(client.callsMatching("fetch acme/api ["))

	h.monitor.RefreshInFlight(context.Background())
	h.monitor.WaitIdle()
	if got := len(client.callsMatching("fetch acme/api [")); got <= calls {
		t.Errorf("an armed PR should be watched: %v", client.callsMatching("fetch"))
	}
}

func TestTheFocusedRepoIsLookedAtSooner(t *testing.T) {
	client := newFakeGitHub(repoWith("acme/api", testfixtures.PR(1)),
		repoWith("acme/web", testfixtures.PR(7)))
	h := startWith(t, client, []string{"acme/api", "acme/web"}, nil, func(saved *config.Config) {
		saved.PollInterval = 3600 // in the background, nothing is due for an hour
		saved.FocusInterval = 1
		saved.ActiveInterval = 3600
	})
	h.monitor.SetFocus("client-1", service.Focus{Repo: "acme/web", Number: 7})

	waitFor(t, func() bool { return len(client.callsMatching("list acme/web")) > 1 })
	if got := client.callsMatching("list acme/api"); len(got) > 1 {
		t.Errorf("the unfocused repo should wait its turn: %v", got)
	}

	h.monitor.SetFocus("client-1", service.Focus{})
	looks := len(client.callsMatching("list acme/web"))
	waitFor(t, func() bool { return len(client.callsMatching("list acme/web")) >= looks })
}
