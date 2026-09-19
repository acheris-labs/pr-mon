package actions_test

import (
	"testing"

	"github.com/acheris-labs/pr-mon/internal/actions"
	"github.com/acheris-labs/pr-mon/internal/models"
	"github.com/acheris-labs/pr-mon/internal/readiness"
	"github.com/acheris-labs/pr-mon/internal/testfixtures"
)

// menu builds one PR in a repo and returns its options by menu slot.
func menu(
	state func(*models.PullRequest), armed *models.ArmedMerge, repoOpts ...func(*models.Repo),
) map[string]models.ActionOption {
	pr := readiness.Assess(testfixtures.PR(1, state))
	repo := testfixtures.Repo("acme/api", []models.PullRequest{pr}, repoOpts...)
	options := map[string]models.ActionOption{}
	for _, option := range actions.Available(repo, pr, armed) {
		options[option.Key] = option
	}
	return options
}

func ready(*models.PullRequest) {}

func noMethods(repo *models.Repo) { repo.MergeMethods = nil }

func deletesBranches(repo *models.Repo) { repo.DeleteBranchOnMerge = true }

func noNativeAutoMerge(repo *models.Repo) { repo.AutoMergeAllowed = false }

var armedMerge = &models.ArmedMerge{
	Method: models.MergeSquash, DeleteBranch: true, ArmedAt: "2026-09-17T10:00:00+00:00",
}

func TestReadyPR(t *testing.T) {
	options := menu(ready, nil)
	merge := options["merge"]
	if !merge.Available || !merge.NeedsMethod || !merge.OffersDeleteBranch {
		t.Errorf("merge = %+v", merge)
	}
	if got := options["auto_merge"]; got.Available || *got.Reason != "already mergeable" {
		t.Errorf("auto_merge = %+v", got)
	}
	if _, found := options["update"]; found {
		t.Error("update should not be offered when the branch isn't behind")
	}
}

func TestMergeUnavailableExplains(t *testing.T) {
	if got := *menu(testfixtures.Pending, nil)["merge"].Reason; got != "PENDING" {
		t.Errorf("reason = %q", got)
	}
	blocked := menu(func(p *models.PullRequest) {
		p.MergeState = "BLOCKED"
		p.ReviewDecision = models.Ptr("CHANGES_REQUESTED")
	}, nil)
	if got := *blocked["merge"].Reason; got != "BLOCKED: Changes requested" {
		t.Errorf("reason = %q", got)
	}
}

func TestNoMergeMethods(t *testing.T) {
	options := menu(ready, nil, noMethods)
	if got := *options["merge"].Reason; got != "READY: no merge methods allowed" {
		t.Errorf("merge reason = %q", got)
	}
	if got := *options["auto_merge"].Reason; got != "no merge methods allowed" {
		t.Errorf("auto_merge reason = %q", got)
	}
}

func TestRepoDeletesBranches(t *testing.T) {
	if menu(ready, nil, deletesBranches)["merge"].OffersDeleteBranch {
		t.Error("should not offer to delete a branch GitHub deletes itself")
	}
	noBranch := menu(func(p *models.PullRequest) { p.HeadRefID = nil }, nil)
	if noBranch["merge"].OffersDeleteBranch {
		t.Error("nothing to delete without a head ref")
	}
}

func TestUpdateOnlyWhenBehind(t *testing.T) {
	update := menu(testfixtures.Behind, nil)["update"]
	if update.Kind != "update" || !update.Available || update.NeedsMethod {
		t.Errorf("update = %+v", update)
	}
}

func TestNativeAutoMerge(t *testing.T) {
	auto := menu(testfixtures.Pending, nil)["auto_merge"]
	if auto.Kind != "auto_merge_on" || !auto.Available || !auto.NeedsMethod {
		t.Errorf("auto_merge = %+v", auto)
	}
	if auto.Note == nil || *auto.Note != "branch won't be deleted: repo doesn't auto-delete" {
		t.Errorf("note = %v", auto.Note)
	}
	if auto.OffersDeleteBranch {
		t.Error("GitHub's auto-merge decides branch deletion itself")
	}
	if got := menu(testfixtures.Pending, nil, deletesBranches)["auto_merge"].Note; got != nil {
		t.Errorf("note = %q, want none", *got)
	}
	enabled := menu(func(p *models.PullRequest) {
		testfixtures.Pending(p)
		p.AutoMerge = &models.AutoMerge{Method: models.MergeSquash, EnabledBy: "bob"}
	}, nil)["auto_merge"]
	if enabled.Kind != "auto_merge_off" || enabled.NeedsMethod {
		t.Errorf("auto_merge = %+v", enabled)
	}
}

func TestMergeWhenReady(t *testing.T) {
	auto := menu(testfixtures.Pending, nil, noNativeAutoMerge)["auto_merge"]
	if auto.Kind != "arm_merge" || auto.Label != "Merge when ready (pr-mon)" || !auto.OffersDeleteBranch {
		t.Errorf("auto_merge = %+v", auto)
	}
	armed := menu(testfixtures.Pending, armedMerge, noNativeAutoMerge)["auto_merge"]
	if armed.Kind != "disarm_merge" || !armed.Available {
		t.Errorf("armed = %+v", armed)
	}
	draft := menu(testfixtures.Draft, nil, noNativeAutoMerge)["auto_merge"]
	if draft.Kind != "arm_merge" || draft.Available || draft.Label != "Merge when ready" {
		t.Errorf("draft = %+v", draft)
	}
	if got := *draft.Reason; got != "draft PR" {
		t.Errorf("reason = %q", got)
	}
}

func TestWithActionsUsesEachPRsArmedState(t *testing.T) {
	prs := []models.PullRequest{
		readiness.Assess(testfixtures.PR(1, testfixtures.Pending)),
		readiness.Assess(testfixtures.PR(2, testfixtures.Pending)),
	}
	repo := testfixtures.Repo("acme/api", prs, noNativeAutoMerge)
	filled := actions.WithActions(repo, map[int]models.ArmedMerge{2: *armedMerge})
	want := []string{"arm_merge", "disarm_merge"}
	for i, pr := range filled.PRs {
		if got := pr.Actions[1].Kind; got != want[i] {
			t.Errorf("pr %d auto_merge kind = %q, want %q", pr.Number, got, want[i])
		}
	}
}

func TestWaitingPRUsesPrMonsMergeWhenReady(t *testing.T) {
	pr := readiness.Assess(testfixtures.PR(1))
	pr.WaitsOn = []models.PRRef{{Repo: "acme/lib", Number: 3, State: models.PROpen}}
	pr = readiness.Wait(pr)
	repo := testfixtures.Repo("acme/api", []models.PullRequest{pr})
	options := map[string]models.ActionOption{}
	for _, option := range actions.Available(repo, pr, nil) {
		options[option.Key] = option
	}
	if merge := options["merge"]; merge.Available || *merge.Reason != "WAITING: Waiting on acme/lib#3" {
		t.Errorf("merge = %+v", merge)
	}
	auto := options["auto_merge"]
	if auto.Kind != "arm_merge" || !auto.Available || auto.Note == nil ||
		*auto.Note != "after the PRs it waits on have merged" {
		t.Errorf("auto_merge = %+v", auto)
	}
}
