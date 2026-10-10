// Muted PRs: the user has silenced their notifications. They stay muted until
// unmuted, or until they merge or close.

package service

import (
	"fmt"
	"slices"

	"github.com/acheris-labs/pr-mon/internal/models"
)

// Muted is the repo's muted PRs.
func (m *Monitor) Muted(name string) map[int]bool {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	return m.mutedLocked(name)
}

func (m *Monitor) mutedLocked(name string) map[int]bool {
	muted := map[int]bool{}
	for _, number := range m.state.Muted[name] {
		muted[number] = true
	}
	return muted
}

// setMuted mutes or unmutes a PR and tells clients.
func (m *Monitor) setMuted(name string, number int, on bool) {
	m.mutex.Lock()
	numbers := slices.DeleteFunc(m.state.Muted[name], func(n int) bool { return n == number })
	if on {
		numbers = append(numbers, number)
		slices.Sort(numbers)
	}
	if len(numbers) == 0 {
		delete(m.state.Muted, name)
	} else {
		m.state.Muted[name] = numbers
	}
	m.saveStateLocked()
	m.rebuildLocked(name)
	m.mutex.Unlock()

	what := "Unmuted"
	if on {
		what = "Muted"
	}
	m.toast(fmt.Sprintf("%s notifications for %s#%d", what, name, number))
	m.emit(Event{Kind: "repo", Name: name})
}

// pruneMutedLocked forgets muted PRs that have merged or closed.
func (m *Monitor) pruneMutedLocked(repo models.Repo) {
	numbers, found := m.state.Muted[repo.Name]
	if !found || repo.PRTotal > len(repo.PRs) {
		return // beyond the newest-N cut-off a PR may still be open
	}
	open := map[int]bool{}
	for _, pr := range repo.PRs {
		open[pr.Number] = true
	}
	kept := slices.DeleteFunc(slices.Clone(numbers), func(n int) bool { return !open[n] })
	if len(kept) == len(numbers) {
		return
	}
	if len(kept) == 0 {
		delete(m.state.Muted, repo.Name)
	} else {
		m.state.Muted[repo.Name] = kept
	}
	m.saveStateLocked()
}
