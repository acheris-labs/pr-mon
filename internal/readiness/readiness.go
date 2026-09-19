// Package readiness holds the backend rules for how mergeable a PR is and why.
// Clients display the results; they never apply these rules themselves.
package readiness

import (
	"fmt"

	"github.com/acheris-labs/pr-mon/internal/models"
)

var failedCheckStates = map[string]bool{
	"FAILURE": true, "ERROR": true, "CANCELLED": true,
	"TIMED_OUT": true, "ACTION_REQUIRED": true, "STARTUP_FAILURE": true,
}

var pendingCheckStates = map[string]bool{
	"PENDING": true, "EXPECTED": true, "QUEUED": true,
	"IN_PROGRESS": true, "WAITING": true, "REQUESTED": true,
}

var readyMergeStates = map[string]bool{"CLEAN": true, "HAS_HOOKS": true, "UNSTABLE": true}

func knownMergeState(state string) bool {
	switch state {
	case "BEHIND", "BLOCKED", "DIRTY", "DRAFT", "UNKNOWN":
		return true
	}
	return readyMergeStates[state]
}

func CheckFailed(check models.Check) bool { return failedCheckStates[check.State] }

func CheckPending(check models.Check) bool { return pendingCheckStates[check.State] }

// Assess returns the PR with its status, reasons and readiness filled in.
func Assess(pr models.PullRequest) models.PullRequest {
	status := PRStatus(pr)
	pr.Status = status
	pr.Reasons = PRReasons(pr, status)
	pr.StrictlyReady = IsStrictlyReady(pr, status)
	pr.LastCheckStartedAt = lastCheckStartedAt(pr)
	return pr
}

// ISO-8601 UTC timestamps from GitHub sort correctly as strings.
func lastCheckStartedAt(pr models.PullRequest) *string {
	var latest *string
	for _, check := range pr.Checks {
		if check.StartedAt == nil {
			continue
		}
		if latest == nil || *check.StartedAt > *latest {
			latest = check.StartedAt
		}
	}
	return latest
}

// IsStrictlyReady reports whether the PR is safe to merge unattended: mergeable
// and no check failing, required or not.
func IsStrictlyReady(pr models.PullRequest, status models.Status) bool {
	if status != models.StatusReady {
		return false
	}
	if pr.MergeState != "CLEAN" && pr.MergeState != "HAS_HOOKS" {
		return false
	}
	if checkState(pr) == "FAILURE" || checkState(pr) == "ERROR" {
		return false
	}
	for _, check := range pr.Checks {
		if CheckFailed(check) {
			return false
		}
	}
	return true
}

func checkState(pr models.PullRequest) string {
	if pr.CheckState == nil {
		return ""
	}
	return *pr.CheckState
}

func PRStatus(pr models.PullRequest) models.Status {
	state := checkState(pr)
	switch {
	case pr.IsDraft:
		return models.StatusDraft
	case pr.Mergeable == "UNKNOWN" || pr.MergeState == "UNKNOWN":
		return models.StatusChecking
	case pr.Mergeable == "CONFLICTING" || pr.MergeState == "DIRTY":
		return models.StatusConflict
	case (state == "FAILURE" || state == "ERROR") && pr.MergeState != "UNSTABLE":
		return models.StatusFailing
	case state == "PENDING" || state == "EXPECTED":
		return models.StatusPending
	case pr.MergeState == "BEHIND":
		return models.StatusBehind
	case readyMergeStates[pr.MergeState]:
		return models.StatusReady
	}
	return models.StatusBlocked
}

func PRReasons(pr models.PullRequest, status models.Status) []models.Reason {
	reasons := []models.Reason{}
	add := func(text, level string) {
		reasons = append(reasons, models.Reason{Text: text, Level: level})
	}
	if pr.IsDraft {
		add("Draft", "info")
	}
	if pr.Mergeable == "UNKNOWN" || pr.MergeState == "UNKNOWN" {
		add("GitHub is still computing mergeability", "info")
	}
	if pr.Mergeable == "CONFLICTING" || pr.MergeState == "DIRTY" {
		add("Merge conflicts", "error")
	}
	checkLevel := "error"
	if pr.MergeState == "UNSTABLE" {
		checkLevel = "warning"
	}
	for _, check := range pr.Checks {
		if CheckFailed(check) {
			add(fmt.Sprintf("Check failed: %s", check.Name), checkLevel)
		}
	}
	if hidden := pr.ChecksTotal - len(pr.Checks); hidden > 0 {
		add(fmt.Sprintf("+%d more checks not shown", hidden), "info")
	}
	pending := 0
	for _, check := range pr.Checks {
		if CheckPending(check) {
			pending++
		}
	}
	if pending > 0 {
		add(fmt.Sprintf("%d checks pending", pending), "warning")
	}
	switch {
	case pr.ReviewDecision != nil && *pr.ReviewDecision == "CHANGES_REQUESTED":
		add("Changes requested", "error")
	case pr.ReviewDecision != nil && *pr.ReviewDecision == "REVIEW_REQUIRED":
		add("Review required", "warning")
	}
	if pr.MergeState == "BEHIND" {
		add("Behind base branch", "warning")
	}
	if !knownMergeState(pr.MergeState) {
		add(fmt.Sprintf("Merge state: %s", pr.MergeState), "warning")
	} else if status == models.StatusBlocked && !hasProblem(reasons) {
		add("Blocked by branch protection", "error")
	}
	return reasons
}

// Wait holds back a PR that waits on others, whatever GitHub says about it: it
// isn't ready until each one has merged. A PR that is otherwise ready shows
// WAITING, or BLOCKED when one it waits on closed without merging. pr.WaitsOn
// must be filled in, and the rest of the PR already assessed.
func Wait(pr models.PullRequest) models.PullRequest {
	reasons := []models.Reason{}
	closed := false
	for _, ref := range pr.WaitsOn {
		switch ref.State {
		case models.PRMerged:
		case models.PRClosed:
			closed = true
			reasons = append(reasons, models.Reason{
				Text: fmt.Sprintf("%s was closed without merging", ref.Key()), Level: "error"})
		default:
			reasons = append(reasons, models.Reason{
				Text: fmt.Sprintf("Waiting on %s", ref.Key()), Level: "warning"})
		}
	}
	if len(reasons) == 0 {
		return pr
	}
	pr.Reasons = append(reasons, pr.Reasons...)
	pr.StrictlyReady = false
	if pr.Status == models.StatusReady {
		pr.Status = models.StatusWaiting
		if closed {
			pr.Status = models.StatusBlocked
		}
	}
	return pr
}

func hasProblem(reasons []models.Reason) bool {
	for _, reason := range reasons {
		if reason.Level == "error" || reason.Level == "warning" {
			return true
		}
	}
	return false
}
