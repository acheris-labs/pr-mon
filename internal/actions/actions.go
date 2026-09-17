// Package actions holds the backend rules for which actions a PR's menu offers.
// Clients display the results; they never decide availability themselves.
package actions

import (
	"fmt"
	"strings"

	"github.com/acheris-labs/pr-mon/internal/models"
)

const (
	noMethods   = "no merge methods allowed"
	keepsBranch = "branch won't be deleted: repo doesn't auto-delete"
)

// WithActions returns the repo with every PR's action menu filled in.
func WithActions(repo models.Repo, armed map[int]models.ArmedMerge) models.Repo {
	prs := make([]models.PullRequest, len(repo.PRs))
	for i, pr := range repo.PRs {
		pr.Actions = Available(repo, pr, armedFor(armed, pr.Number))
		prs[i] = pr
	}
	repo.PRs = prs
	return repo
}

func armedFor(armed map[int]models.ArmedMerge, number int) *models.ArmedMerge {
	if merge, ok := armed[number]; ok {
		return &merge
	}
	return nil
}

func Available(repo models.Repo, pr models.PullRequest, armed *models.ArmedMerge) []models.ActionOption {
	deletable := !repo.DeleteBranchOnMerge && pr.HeadRefID != nil
	options := []models.ActionOption{
		mergeOption(repo, pr, deletable),
		autoMergeOption(repo, pr, armed, deletable),
	}
	if pr.MergeState == "BEHIND" {
		options = append(options, models.ActionOption{
			Key: "update", Kind: "update", Label: "Update branch", Available: true,
		})
	}
	return options
}

func mergeOption(repo models.Repo, pr models.PullRequest, deletable bool) models.ActionOption {
	if pr.Status == models.StatusReady && len(repo.MergeMethods) > 0 {
		return models.ActionOption{
			Key: "merge", Kind: "merge", Label: "Merge", Available: true,
			NeedsMethod: true, OffersDeleteBranch: deletable,
		}
	}
	var why []string
	for _, reason := range pr.Reasons {
		if reason.Level != "info" {
			why = append(why, reason.Text)
		}
	}
	if len(repo.MergeMethods) == 0 {
		why = append(why, noMethods)
	}
	detail := string(pr.Status)
	if len(why) > 0 {
		detail = fmt.Sprintf("%s: %s", pr.Status, strings.Join(why, ", "))
	}
	return models.ActionOption{
		Key: "merge", Kind: "merge", Label: "Merge", Available: false, Reason: &detail,
	}
}

// autoMergeOption offers GitHub's native auto-merge where the repo allows it,
// else pr-mon's own merge when ready.
func autoMergeOption(
	repo models.Repo, pr models.PullRequest, armed *models.ArmedMerge, deletable bool,
) models.ActionOption {
	if pr.AutoMerge != nil {
		return models.ActionOption{
			Key: "auto_merge", Kind: "auto_merge_off", Label: "Disable auto-merge", Available: true,
		}
	}
	if armed != nil {
		return models.ActionOption{
			Key: "auto_merge", Kind: "disarm_merge",
			Label: "Cancel merge when ready (pr-mon)", Available: true,
		}
	}
	native := repo.AutoMergeAllowed
	kind := "arm_merge"
	if native {
		kind = "auto_merge_on"
	}
	if blocker := autoMergeBlocker(repo, pr); blocker != "" {
		label := "Merge when ready"
		if native {
			label = "Auto-merge"
		}
		return models.ActionOption{
			Key: "auto_merge", Kind: kind, Label: label, Available: false, Reason: &blocker,
		}
	}
	if !native {
		return models.ActionOption{
			Key: "auto_merge", Kind: kind, Label: "Merge when ready (pr-mon)", Available: true,
			NeedsMethod: true, OffersDeleteBranch: deletable,
		}
	}
	option := models.ActionOption{
		Key: "auto_merge", Kind: kind, Label: "Enable auto-merge", Available: true,
		NeedsMethod: true,
	}
	if !repo.DeleteBranchOnMerge {
		option.Note = models.Ptr(keepsBranch)
	}
	return option
}

func autoMergeBlocker(repo models.Repo, pr models.PullRequest) string {
	switch {
	case len(repo.MergeMethods) == 0:
		return noMethods
	case pr.IsDraft:
		return "draft PR"
	case pr.Status == models.StatusReady:
		return "already mergeable"
	}
	return ""
}
