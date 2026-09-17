// Package tracker detects PR changes between polls and tracks what the user
// hasn't seen.
package tracker

import (
	"sort"
	"strconv"

	"github.com/acheris-labs/pr-mon/internal/models"
	"github.com/acheris-labs/pr-mon/internal/state"
)

type EventKind string

const (
	EventNew     EventKind = "NEW"
	EventReady   EventKind = "READY"
	EventBlocked EventKind = "BLOCKED"
)

// Change is a PR whose status moved; Old is empty for a PR with no real status yet.
type Change struct {
	Number int
	Old    models.Status
	New    models.Status
}

type Event struct {
	Kind   EventKind
	Repo   string
	Number int
}

// Tracker works on the records map in place, so the caller persists the same map.
type Tracker struct {
	records map[string]map[string]state.Record
}

func New(records map[string]map[string]state.Record) *Tracker {
	if records == nil {
		records = map[string]map[string]state.Record{}
	}
	return &Tracker{records: records}
}

func (t *Tracker) repo(name string) map[string]state.Record {
	entries, found := t.records[name]
	if !found {
		return map[string]state.Record{}
	}
	return entries
}

// Changes are the status changes Update would record for this poll, without
// recording them.
func (t *Tracker) Changes(repo models.Repo) []Change {
	previous := t.repo(repo.Name)
	changes := []Change{}
	for _, pr := range repo.PRs {
		if pr.Status == models.StatusChecking {
			continue
		}
		var old models.Status
		if record, found := previous[strconv.Itoa(pr.Number)]; found {
			old = models.Status(record.Status)
		}
		// A recompute blip counts as "no status yet", not as a change from CHECKING.
		if old == models.StatusChecking {
			old = ""
		}
		if pr.Status != old {
			changes = append(changes, Change{Number: pr.Number, Old: old, New: pr.Status})
		}
	}
	return changes
}

// Update records this poll's statuses and returns the events worth notifying about.
func (t *Tracker) Update(repo models.Repo) []Event {
	previous := t.repo(repo.Name)
	current := map[string]state.Record{}
	events := []Event{}
	for _, pr := range repo.PRs {
		key := strconv.Itoa(pr.Number)
		old, existed := previous[key]
		var kind EventKind
		switch {
		case !existed:
			kind = EventNew
		case pr.Status == models.StatusChecking:
			// Keep the last real status so a recompute blip doesn't re-fire events.
			current[key] = old
			continue
		case pr.Status == models.Status(old.Status):
		case pr.Status == models.StatusReady:
			kind = EventReady
		case pr.Status.IsAlert() && !models.Status(old.Status).IsAlert():
			kind = EventBlocked
		}
		current[key] = state.Record{Status: string(pr.Status), Seen: existed && old.Seen && kind == ""}
		if kind != "" {
			events = append(events, Event{Kind: kind, Repo: repo.Name, Number: pr.Number})
		}
	}
	t.records[repo.Name] = current
	return events
}

// MarkSeen clears a PR's new marker; false when there was nothing to clear.
func (t *Tracker) MarkSeen(repo string, number int) bool {
	entries, found := t.records[repo]
	if !found {
		return false
	}
	key := strconv.Itoa(number)
	entry, found := entries[key]
	if !found || entry.Seen {
		return false
	}
	entry.Seen = true
	entries[key] = entry
	return true
}

func (t *Tracker) IsUnseen(repo string, number int) bool {
	entry, found := t.repo(repo)[strconv.Itoa(number)]
	return found && !entry.Seen
}

// Unseen are the PR numbers the user hasn't looked at, in order.
func (t *Tracker) Unseen(repo string) []int {
	numbers := []int{}
	for key, entry := range t.repo(repo) {
		if entry.Seen {
			continue
		}
		if number, err := strconv.Atoi(key); err == nil {
			numbers = append(numbers, number)
		}
	}
	sort.Ints(numbers)
	return numbers
}

func (t *Tracker) Forget(repo string) {
	delete(t.records, repo)
}
