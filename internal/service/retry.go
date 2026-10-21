// Automatic retry: pr-mon re-runs a PR's failed GitHub Actions jobs once, as
// soon as a run with a failure finishes (GitHub refuses to re-run a run that
// is still going). It switches off after retrying, when the PR gets a new head
// commit, or when the PR closes.

package service

import (
	"fmt"
	"slices"
	"strconv"

	"github.com/acheris-labs/pr-mon/internal/models"
	"github.com/acheris-labs/pr-mon/internal/readiness"
)

func (m *Monitor) autoRetryLocked(name string) map[int]bool {
	retry := map[int]bool{}
	for key := range m.state.Retry[name] {
		if number, err := strconv.Atoi(key); err == nil {
			retry[number] = true
		}
	}
	return retry
}

// setAutoRetry switches automatic retry on or off for a PR and tells clients.
func (m *Monitor) setAutoRetry(name string, pr models.PullRequest, on bool) {
	m.mutex.Lock()
	if on {
		if m.state.Retry[name] == nil {
			m.state.Retry[name] = map[string]string{}
		}
		m.state.Retry[name][strconv.Itoa(pr.Number)] = headOf(pr)
	} else {
		m.forgetRetryLocked(name, pr.Number)
	}
	m.saveStateLocked()
	m.rebuildLocked(name)
	m.mutex.Unlock()

	label := fmt.Sprintf("%s#%d", name, pr.Number)
	if on {
		m.toast(fmt.Sprintf("pr-mon will re-run %s's failed checks once their run finishes", label))
	} else {
		m.toast(fmt.Sprintf("Stopped retrying failed checks for %s", label))
	}
	m.emit(Event{Kind: "repo", Name: name})
}

// checkAutoRetry re-runs the finished failed runs of PRs set to retry, and
// forgets PRs that closed or got a new head commit.
func (m *Monitor) checkAutoRetry(name string, repo models.Repo) {
	type retry struct {
		pr   models.PullRequest
		runs []int
	}
	retries := []retry{}
	moved := []int{}
	m.mutex.Lock()
	entries := m.state.Retry[name]
	if len(entries) == 0 {
		m.mutex.Unlock()
		return
	}
	prs := map[string]models.PullRequest{}
	for _, pr := range repo.PRs {
		prs[strconv.Itoa(pr.Number)] = pr
	}
	truncated := repo.PRTotal > len(repo.PRs)
	changed := false
	for key, head := range entries {
		pr, present := prs[key]
		switch {
		case !present:
			// Beyond the newest-N cut-off it may still be open; otherwise it closed.
			if truncated {
				continue
			}
		case headOf(pr) != head:
			moved = append(moved, pr.Number)
		default:
			runs := finishedFailedRuns(pr)
			if len(runs) == 0 {
				continue
			}
			retries = append(retries, retry{pr: pr, runs: runs})
		}
		delete(entries, key)
		changed = true
	}
	if changed {
		if len(entries) == 0 {
			delete(m.state.Retry, name)
		}
		m.saveStateLocked()
		m.rebuildLocked(name)
	}
	m.mutex.Unlock()
	for _, number := range moved {
		m.toast(fmt.Sprintf("Stopped retrying failed checks for %s#%d: it has new commits",
			name, number))
	}
	if changed {
		m.emit(Event{Kind: "repo", Name: name})
	}

	for _, retry := range retries {
		m.spawn(func() {
			label := fmt.Sprintf("%s#%d", name, retry.pr.Number)
			if err := m.client.RerunFailedJobs(m.ctx, name, retry.runs); err != nil {
				m.log.Warn("automatic retry failed", "pr", label, "error", err)
				m.toast(fmt.Sprintf("Retrying failed checks failed for %s: %v", label, err), "error")
				return
			}
			m.log.Info("retried failed checks", "pr", label, "runs", retry.runs)
			m.toast(fmt.Sprintf("Re-running failed checks for %s (automatic retry)", label))
		})
	}
}

func (m *Monitor) forgetRetryLocked(name string, number int) {
	delete(m.state.Retry[name], strconv.Itoa(number))
	if len(m.state.Retry[name]) == 0 {
		delete(m.state.Retry, name)
	}
}

// finishedFailedRuns are the Actions runs behind a failed check that have no
// check still running.
func finishedFailedRuns(pr models.PullRequest) []int {
	running := map[int]bool{}
	for _, check := range pr.Checks {
		if check.RunID != nil && readiness.CheckPending(check) {
			running[*check.RunID] = true
		}
	}
	return slices.DeleteFunc(failedRuns(pr), func(run int) bool { return running[run] })
}

func headOf(pr models.PullRequest) string {
	if pr.HeadSha == nil {
		return ""
	}
	return *pr.HeadSha
}
