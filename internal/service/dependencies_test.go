package service_test

import (
	"context"
	"testing"

	"github.com/acheris-labs/pr-mon/internal/models"
	"github.com/acheris-labs/pr-mon/internal/state"
	"github.com/acheris-labs/pr-mon/internal/testfixtures"
)

func addDependency(t *testing.T, h *harness, number int, on string) {
	t.Helper()
	if err := h.monitor.AddDependency(context.Background(), "acme/api", number, on); err != nil {
		t.Fatal(err)
	}
	h.monitor.WaitIdle()
}

func pr(t *testing.T, h *harness, repo string, number int) models.PullRequest {
	t.Helper()
	loaded, _ := h.monitor.Repo(repo)
	for _, candidate := range loaded.PRs {
		if candidate.Number == number {
			return candidate
		}
	}
	t.Fatalf("%s#%d is not loaded", repo, number)
	return models.PullRequest{}
}

func TestWaitingHoldsAReadyPR(t *testing.T) {
	client := newFakeGitHub(repoWith("acme/api", testfixtures.PR(1), testfixtures.PR(2)))
	h := start(t, client, []string{"acme/api"}, nil)
	addDependency(t, h, 1, "acme/api#2")

	waiting := pr(t, h, "acme/api", 1)
	if waiting.Status != models.StatusWaiting || waiting.StrictlyReady {
		t.Errorf("status = %s, strictly ready = %v", waiting.Status, waiting.StrictlyReady)
	}
	if len(waiting.WaitsOn) != 1 || waiting.WaitsOn[0].Key() != "acme/api#2" ||
		waiting.WaitsOn[0].State != models.PROpen || waiting.WaitsOn[0].Status == nil ||
		*waiting.WaitsOn[0].Status != models.StatusReady {
		t.Errorf("waits on = %+v", waiting.WaitsOn)
	}
	if required := pr(t, h, "acme/api", 2).RequiredBy; len(required) != 1 ||
		required[0].Key() != "acme/api#1" {
		t.Errorf("required by = %+v", required)
	}
	if !h.hasToast("acme/api#1 now waits on acme/api#2") {
		t.Errorf("toasts = %v", h.toasts())
	}
	saved, _ := state.Load(h.statePath)
	if got := saved.Dependencies["acme/api#1"]; len(got) != 1 || got[0] != "acme/api#2" {
		t.Errorf("saved = %v", saved.Dependencies)
	}

	if err := h.monitor.RemoveDependency("acme/api", 1, "acme/api#2"); err != nil {
		t.Fatal(err)
	}
	if got := pr(t, h, "acme/api", 1); got.Status != models.StatusReady || len(got.WaitsOn) != 0 {
		t.Errorf("after removing: status = %s, waits on = %+v", got.Status, got.WaitsOn)
	}
	if err := h.monitor.RemoveDependency("acme/api", 1, "acme/api#2"); err == nil {
		t.Error("removing it twice should fail")
	}
}

func TestAddDependencyRefuses(t *testing.T) {
	client := newFakeGitHub(repoWith("acme/api", testfixtures.PR(1), testfixtures.PR(2)))
	client.setState("other/lib#5", models.PRMerged)
	client.setState("other/lib#6", models.PRClosed)
	h := start(t, client, []string{"acme/api"}, nil)
	addDependency(t, h, 1, "acme/api#2")

	cases := []struct {
		number int
		on     string
		want   string
	}{
		{9, "acme/api#2", "acme/api#9 is not an open PR"},
		{1, "nonsense", `expected owner/repo#number or a pull request URL, got "nonsense"`},
		{1, "other/lib#404", "other/lib#404 not found"},
		{1, "other/lib#5", "other/lib#5 has already merged"},
		{1, "other/lib#6", "other/lib#6 is closed"},
		{1, "acme/api#1", "acme/api#1 can't wait on itself"},
		{1, "https://github.com/acme/api/pull/2", "acme/api#1 already waits on acme/api#2"},
		{2, "acme/api#1",
			"acme/api#1 already waits on acme/api#2, so it can't also be the other way round"},
	}
	for _, test := range cases {
		err := h.monitor.AddDependency(context.Background(), "acme/api", test.number, test.on)
		if err == nil || err.Error() != test.want {
			t.Errorf("AddDependency(#%d, %q) = %v, want %q", test.number, test.on, err, test.want)
		}
	}
}

func TestWaitsOnAPRInAnotherRepo(t *testing.T) {
	client := newFakeGitHub(repoWith("acme/api", testfixtures.PR(1, testfixtures.Pending)))
	client.setState("other/lib#5", models.PROpen)
	h := start(t, client, []string{"acme/api"}, mergeEvents())
	addDependency(t, h, 1, "https://github.com/other/lib/pull/5")
	arm(t, h, 1, false)

	// It turns ready, but still waits.
	client.setRepo(repoWith("acme/api", testfixtures.PR(1)))
	h.poll()
	if got := pr(t, h, "acme/api", 1); got.Status != models.StatusWaiting {
		t.Errorf("status = %s", got.Status)
	}
	if len(client.callsMatching("merge ")) != 0 {
		t.Fatal("a waiting PR must not be merged")
	}

	client.setState("other/lib#5", models.PRMerged)
	h.poll()
	if got := client.callsMatching("merge "); len(got) != 1 || got[0] != "merge PR_1 SQUASH" {
		t.Errorf("merges = %v", got)
	}
	if !sentState(h, "MERGED", "") {
		t.Error("merging it should notify as any pr-mon merge does")
	}
}

func TestClosedDependencyBlocks(t *testing.T) {
	client := newFakeGitHub(repoWith("acme/api", testfixtures.PR(1)))
	client.setState("other/lib#5", models.PROpen)
	h := start(t, client, []string{"acme/api"}, nil)
	addDependency(t, h, 1, "other/lib#5")

	client.setState("other/lib#5", models.PRClosed)
	h.poll()
	got := pr(t, h, "acme/api", 1)
	if got.Status != models.StatusBlocked || got.Reasons[0].Text != "other/lib#5 was closed without merging" {
		t.Errorf("status = %s, reasons = %+v", got.Status, got.Reasons)
	}
	if len(h.monitor.DependencyGraph("acme/api", 1).WaitsOn) != 1 {
		t.Error("a closed dependency stays until it is removed")
	}
}

func TestMergingOneLetsTheNextGo(t *testing.T) {
	client := newFakeGitHub(repoWith("acme/api", testfixtures.PR(1), testfixtures.PR(2)))
	h := start(t, client, []string{"acme/api"}, nil)
	addDependency(t, h, 1, "acme/api#2")
	arm(t, h, 1, false)
	if len(client.callsMatching("merge ")) != 0 {
		t.Fatal("#1 waits on #2")
	}

	arm(t, h, 2, false)
	if got := client.callsMatching("merge "); len(got) != 2 ||
		got[0] != "merge PR_2 SQUASH" || got[1] != "merge PR_1 SQUASH" {
		t.Errorf("merges = %v, want #2 then #1", got)
	}
}

func TestManualMergeWaits(t *testing.T) {
	client := newFakeGitHub(repoWith("acme/api", testfixtures.PR(1, testfixtures.Pending),
		testfixtures.PR(2)))
	h := start(t, client, []string{"acme/api"}, nil)
	addDependency(t, h, 1, "acme/api#2")

	err := h.monitor.Perform(context.Background(), "acme/api", 1,
		models.Action{Kind: "merge", Method: &squash})
	if err == nil || err.Error() != "acme/api#1 waits on PRs that haven't merged; remove them to merge it now" {
		t.Errorf("merge = %v", err)
	}
	if err := h.monitor.Perform(context.Background(), "acme/api", 1,
		models.Action{Kind: "auto_merge_on", Method: &squash}); err != nil {
		t.Fatal(err)
	}
	if len(client.callsMatching("auto_on ")) != 0 {
		t.Error("GitHub's auto-merge would not wait")
	}
	if _, armed := h.monitor.Armed("acme/api")[1]; !armed {
		t.Error("auto-merge on a waiting PR should use pr-mon's merge when ready")
	}
}

func TestWaitingMovesOffGitHubAutoMerge(t *testing.T) {
	client := newFakeGitHub(repoWith("acme/api", testfixtures.PR(1, func(p *models.PullRequest) {
		testfixtures.Pending(p)
		p.AutoMerge = &models.AutoMerge{Method: models.MergeRebase, EnabledBy: "bob"}
	}), testfixtures.PR(2)))
	h := start(t, client, []string{"acme/api"}, nil)
	addDependency(t, h, 1, "acme/api#2")

	if got := client.callsMatching("auto_off "); len(got) != 1 || got[0] != "auto_off PR_1" {
		t.Errorf("calls = %v", got)
	}
	if merge, armed := h.monitor.Armed("acme/api")[1]; !armed || merge.Method != models.MergeRebase {
		t.Errorf("armed = %+v", h.monitor.Armed("acme/api"))
	}
	if !h.hasToast("acme/api#1 waits on other PRs, so pr-mon will merge it instead of GitHub auto-merge (rebase)") {
		t.Errorf("toasts = %v", h.toasts())
	}
}

func TestClosingAWaitingPRForgetsItsDependencies(t *testing.T) {
	client := newFakeGitHub(repoWith("acme/api", testfixtures.PR(1), testfixtures.PR(2)))
	h := start(t, client, []string{"acme/api"}, nil)
	addDependency(t, h, 1, "acme/api#2")

	client.setRepo(repoWith("acme/api", testfixtures.PR(2)))
	h.poll()
	if got := pr(t, h, "acme/api", 2).RequiredBy; len(got) != 0 {
		t.Errorf("required by = %+v", got)
	}
	saved, _ := state.Load(h.statePath)
	if len(saved.Dependencies) != 0 {
		t.Errorf("saved = %v", saved.Dependencies)
	}
}

func TestRemovingARepoForgetsItsWaitingPRs(t *testing.T) {
	client := newFakeGitHub(repoWith("acme/api", testfixtures.PR(1)))
	client.setState("other/lib#5", models.PROpen)
	h := start(t, client, []string{"acme/api"}, nil)
	addDependency(t, h, 1, "other/lib#5")
	if err := h.monitor.RemoveRepo("acme/api"); err != nil {
		t.Fatal(err)
	}
	saved, _ := state.Load(h.statePath)
	if len(saved.Dependencies) != 0 {
		t.Errorf("saved = %v", saved.Dependencies)
	}
}

func TestDependencyGraph(t *testing.T) {
	client := newFakeGitHub(repoWith("acme/api", testfixtures.PR(1), testfixtures.PR(2),
		testfixtures.PR(3)))
	client.setState("other/lib#5", models.PROpen)
	h := start(t, client, []string{"acme/api"}, nil)
	addDependency(t, h, 1, "acme/api#2")
	addDependency(t, h, 2, "other/lib#5")
	addDependency(t, h, 3, "acme/api#2")

	graph := h.monitor.DependencyGraph("acme/api", 2)
	if graph.PR.Key() != "acme/api#2" || graph.PR.Status == nil || *graph.PR.Status != models.StatusWaiting {
		t.Errorf("root = %+v", graph.PR)
	}
	if len(graph.WaitsOn) != 1 || graph.WaitsOn[0].PR.Key() != "other/lib#5" ||
		graph.WaitsOn[0].PR.Title != "Elsewhere" || graph.WaitsOn[0].PR.Status != nil {
		t.Errorf("waits on = %+v", graph.WaitsOn)
	}
	if len(graph.RequiredBy) != 2 || graph.RequiredBy[0].PR.Key() != "acme/api#1" ||
		graph.RequiredBy[1].PR.Key() != "acme/api#3" {
		t.Errorf("required by = %+v", graph.RequiredBy)
	}
	top := h.monitor.DependencyGraph("acme/api", 1)
	if len(top.WaitsOn) != 1 || len(top.WaitsOn[0].Children) != 1 ||
		top.WaitsOn[0].Children[0].PR.Key() != "other/lib#5" {
		t.Errorf("the tree should reach through #2: %+v", top.WaitsOn)
	}
}

func TestAMergedDependencyStopsTheWait(t *testing.T) {
	// A waits on B, both in the same monitored repo.
	client := newFakeGitHub(repoWith("acme/api", testfixtures.PR(1), testfixtures.PR(2)))
	h := start(t, client, []string{"acme/api"}, nil)
	addDependency(t, h, 1, "acme/api#2")
	if got := pr(t, h, "acme/api", 1); got.Status != models.StatusWaiting {
		t.Fatalf("status = %s", got.Status)
	}

	// B merges: it leaves the open list, and GitHub reports it merged.
	client.setRepo(repoWith("acme/api", testfixtures.PR(1)))
	client.setState("acme/api#2", models.PRMerged)
	client.touch("acme/api", 1, "2026-09-23T12:00:00Z")
	look(h)

	got := pr(t, h, "acme/api", 1)
	if got.Status != models.StatusReady {
		t.Errorf("status = %s, want READY once what it waited on merged", got.Status)
	}
	if len(got.WaitsOn) != 1 || got.WaitsOn[0].State != models.PRMerged {
		t.Errorf("waits on = %+v, want it marked merged", got.WaitsOn)
	}
}

func TestADependencyThatMergesElsewhereStopsTheWait(t *testing.T) {
	// The PR being waited on is in a repo pr-mon doesn't monitor, so only the
	// scheduled lookup can notice it merged.
	client := newFakeGitHub(repoWith("acme/api", testfixtures.PR(1)))
	client.setState("other/lib#5", models.PROpen)
	h := start(t, client, []string{"acme/api"}, nil)
	addDependency(t, h, 1, "other/lib#5")
	if got := pr(t, h, "acme/api", 1); got.Status != models.StatusWaiting {
		t.Fatalf("status = %s", got.Status)
	}

	client.setState("other/lib#5", models.PRMerged)
	h.monitor.RefreshDependencyStates(context.Background())
	h.monitor.WaitIdle()

	if got := pr(t, h, "acme/api", 1); got.Status != models.StatusReady {
		t.Errorf("status = %s, want READY", got.Status)
	}
}
