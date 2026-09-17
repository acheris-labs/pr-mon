// Package testfixtures builds PRs and repos for tests: by default a clean,
// mergeable pull request in a repo that allows every merge method.
package testfixtures

import (
	"fmt"

	"github.com/acheris-labs/pr-mon/internal/models"
)

const (
	CreatedAt    = "2026-09-15T01:28:54Z"
	CommittedAt  = "2026-09-15T02:00:00Z"
	HeadSha      = "abc1234def5678abc1234def5678abc1234def56"
	CheckStarted = "2026-09-15T01:30:00Z"
)

// PR is a mergeable pull request; `mutate` applies the differences a test cares about.
func PR(number int, mutate ...func(*models.PullRequest)) models.PullRequest {
	pr := models.PullRequest{
		ID:           fmt.Sprintf("PR_%d", number),
		Number:       number,
		Title:        "A change",
		URL:          fmt.Sprintf("https://github.com/acme/api/pull/%d", number),
		Author:       "alice",
		CreatedAt:    CreatedAt,
		HeadRef:      fmt.Sprintf("feature-%d", number),
		BaseRef:      "main",
		HeadRefID:    models.Ptr("REF_1"),
		HeadRepo:     models.Ptr("acme/api"),
		Mergeable:    "MERGEABLE",
		MergeState:   "CLEAN",
		CheckState:   models.Ptr("SUCCESS"),
		Checks:       []models.Check{},
		LastCommitAt: models.Ptr(CommittedAt),
		HeadSha:      models.Ptr(HeadSha),
		Reasons:      []models.Reason{},
		Actions:      []models.ActionOption{},
	}
	for _, apply := range mutate {
		apply(&pr)
	}
	return pr
}

// Repo allows every merge method and GitHub's own auto-merge.
func Repo(name string, prs []models.PullRequest, mutate ...func(*models.Repo)) models.Repo {
	if prs == nil {
		prs = []models.PullRequest{}
	}
	repo := models.Repo{
		Name:             name,
		MergeMethods:     []models.MergeMethod{models.MergeSquash, models.MergeCommit, models.MergeRebase},
		PRs:              prs,
		PRTotal:          len(prs),
		AutoMergeAllowed: true,
	}
	for _, apply := range mutate {
		apply(&repo)
	}
	return repo
}

// CheckRun is a completed check with the given conclusion.
func CheckRun(name, conclusion string) models.Check {
	return models.Check{Name: name, State: conclusion, StartedAt: models.Ptr(CheckStarted)}
}

// Common PR states.

func Pending(pr *models.PullRequest) {
	pr.MergeState = "BLOCKED"
	pr.CheckState = models.Ptr("PENDING")
}

func Failing(pr *models.PullRequest) {
	pr.MergeState = "BLOCKED"
	pr.CheckState = models.Ptr("FAILURE")
}

func Conflict(pr *models.PullRequest) {
	pr.Mergeable = "CONFLICTING"
	pr.MergeState = "DIRTY"
}

func Behind(pr *models.PullRequest) { pr.MergeState = "BEHIND" }

func Draft(pr *models.PullRequest) {
	pr.IsDraft = true
	pr.MergeState = "DRAFT"
}
