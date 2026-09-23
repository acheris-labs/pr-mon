// How often the backend looks at what, and how little it can ask for.
//
// Every look starts with conditional requests: GitHub answers an unchanged
// repo with 304, which costs nothing against the rate limit. Only the PRs whose
// `updated_at` moved (or all of them, when the base branch moved) are then
// fetched in full. Three tiers decide how often a repo is looked at:
//
//   - repos nobody has selected: config.PollInterval
//   - the repo a client has selected: config.FocusInterval
//   - PRs in flight (checks running, armed to merge): config.ActiveInterval,
//     and those PRs alone, not their whole repo.
//
// A repo is fetched in full anyway every github.Stale, so nothing can drift.

package service

import (
	"context"
	"slices"
	"time"

	"github.com/acheris-labs/pr-mon/internal/github"
	"github.com/acheris-labs/pr-mon/internal/models"
	"github.com/acheris-labs/pr-mon/internal/readiness"
)

const (
	// tickInterval is how often the scheduler looks at the clock.
	tickInterval = time.Second
	// A PR counts as in flight while its checks are this fresh: a big repo
	// always has some PR with something pending, and watching all of them
	// every few seconds would cost more than it is worth.
	activeWindow = 30 * time.Minute
	// At most this many per repo, newest first.
	activeLimit = 10
)

// watch is what the backend remembers between looks at one repo.
type watch struct {
	listETag string
	baseETag string
	// What each PR's updated_at was when it was last fetched in full.
	updated    map[int]string
	lastLook   time.Time
	lastFull   time.Time
	lastActive time.Time
}

// Focus is the repo, and PR, a client is looking at.
type Focus struct {
	Repo   string
	Number int
}

// SetFocus records what one client has selected, so its repo is looked at more
// often. An empty repo clears it.
func (m *Monitor) SetFocus(client string, focus Focus) {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	if focus.Repo == "" {
		delete(m.focus, client)
		return
	}
	m.focus[client] = focus
}

// ClearFocus forgets a client that has gone.
func (m *Monitor) ClearFocus(client string) {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	delete(m.focus, client)
}

// Focused is every repo a client has selected, sorted.
func (m *Monitor) Focused() []string {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	names := []string{}
	for name := range m.focusedLocked() {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func (m *Monitor) focusedLocked() map[string]bool {
	repos := map[string]bool{}
	for _, focus := range m.focus {
		repos[focus.Repo] = true
	}
	return repos
}

// due reports which repos to look at now, whether in-flight PRs are due, and
// whether to look again at the PRs other PRs are waiting on.
func (m *Monitor) due(now time.Time) (look []string, active, waited bool) {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	if !m.pausedUntil.IsZero() && now.Before(m.pausedUntil) {
		return nil, false, false
	}
	focused := m.focusedLocked()
	background := seconds(m.config.PollInterval, MinPollInterval)
	focus := seconds(m.config.FocusInterval, 1)
	for _, name := range m.config.Repos {
		state := m.watchLocked(name)
		interval := background
		if focused[name] {
			interval = min(focus, background)
		}
		if now.Sub(state.lastLook) >= interval {
			look = append(look, name)
		}
	}
	waited = len(m.graphLocked()) > 0 && now.Sub(m.lastWaited) >= background
	return look, now.Sub(m.lastActive) >= seconds(m.config.ActiveInterval, 1), waited
}

func seconds(value, lowest int) time.Duration {
	return time.Duration(max(value, lowest)) * time.Second
}

func (m *Monitor) watchLocked(name string) *watch {
	state, found := m.watches[name]
	if !found {
		state = &watch{updated: map[int]string{}}
		m.watches[name] = state
	}
	return state
}

// pollLoop is the scheduler: it wakes often and does only what is due.
func (m *Monitor) pollLoop() {
	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-m.stopped:
			return
		case <-m.pollWake:
			m.PollOnce(m.ctx)
			continue
		case <-ticker.C:
		}
		now := time.Now()
		look, active, waited := m.due(now)
		if waited {
			m.mutex.Lock()
			m.lastWaited = now
			m.mutex.Unlock()
			m.spawn(func() { m.RefreshDependencyStates(m.ctx) })
		}
		if active {
			m.mutex.Lock()
			m.lastActive = now
			m.mutex.Unlock()
			m.spawn(func() { m.RefreshInFlight(m.ctx) })
		}
		for _, name := range look {
			m.mutex.Lock()
			m.watchLocked(name).lastLook = now
			m.mutex.Unlock()
			m.spawn(func() { m.Look(m.ctx, name) })
		}
	}
}

// Look is one conditional check of a repo: free unless something changed. The
// scheduler calls it on each repo's own interval.
func (m *Monitor) Look(ctx context.Context, name string) {
	m.mutex.Lock()
	state := *m.watchLocked(name)
	_, loaded := m.repos[name]
	m.mutex.Unlock()
	if !loaded || time.Since(state.lastFull) >= github.Stale {
		m.RefreshRepo(ctx, name, 0)
		return
	}

	listed, err := m.client.ListOpenPRs(ctx, name, state.listETag)
	if err != nil {
		m.handleFetchError(name, err)
		return
	}
	moved, baseETag, err := m.client.BaseMoved(ctx, name, state.baseETag)
	if err != nil {
		m.handleFetchError(name, err)
		return
	}
	m.mutex.Lock()
	current := m.watchLocked(name)
	current.baseETag = baseETag
	if listed.Changed {
		current.listETag = listed.ETag
	}
	m.mutex.Unlock()
	if !listed.Changed && !moved {
		m.noteFresh(name)
		return
	}
	if !listed.Changed {
		// The base branch moved: every PR merges differently now, and the list
		// itself didn't change, so ask what is open before fetching details.
		listed, err = m.client.ListOpenPRs(ctx, name, "")
		if err != nil {
			m.handleFetchError(name, err)
			return
		}
		m.mutex.Lock()
		m.watchLocked(name).listETag = listed.ETag
		m.mutex.Unlock()
	}
	m.applyChanged(ctx, name, listed, moved)
}

// applyChanged fetches the PRs that moved and rebuilds the repo around them.
func (m *Monitor) applyChanged(ctx context.Context, name string, listed github.OpenPRs, all bool) {
	m.mutex.Lock()
	repo, loaded := m.fetched[name]
	state := m.watchLocked(name)
	wanted := []int{}
	for _, number := range listed.Numbers {
		if all || state.updated[number] != listed.Updated[number] {
			wanted = append(wanted, number)
		}
	}
	m.mutex.Unlock()
	if !loaded {
		m.RefreshRepo(ctx, name, 0)
		return
	}

	updated := map[int]models.PullRequest{}
	if len(wanted) > 0 {
		fetched, err := m.client.FetchPRs(ctx, name, wanted)
		if err != nil {
			m.handleFetchError(name, err)
			return
		}
		for _, pr := range fetched {
			updated[pr.Number] = pr
		}
	}

	m.mutex.Lock()
	known := map[int]models.PullRequest{}
	for _, pr := range repo.PRs {
		known[pr.Number] = pr
	}
	prs := []models.PullRequest{}
	for _, number := range listed.Numbers {
		switch pr, have := updated[number]; {
		case have:
			prs = append(prs, pr)
			state.updated[number] = listed.Updated[number]
		default:
			if pr, have := known[number]; have {
				prs = append(prs, pr)
			}
		}
	}
	for number := range state.updated {
		if !slices.Contains(listed.Numbers, number) {
			delete(state.updated, number)
		}
	}
	repo.PRs = prs
	if len(listed.Numbers) < github.PRLimit {
		repo.PRTotal = len(listed.Numbers) // the list is everything that's open
	}
	m.mutex.Unlock()

	m.apply(name, repo)
	m.noteFresh(name)
	// Something waiting on a PR that just left this list shouldn't say it is
	// still waiting until the next scheduled look.
	open := map[int]bool{}
	for _, number := range listed.Numbers {
		open[number] = true
	}
	if m.targetGone(name, open) {
		m.RefreshDependencyStates(ctx)
	}
}

// RefreshInFlight re-fetches only the PRs that are moving: checks running, or
// armed for merge. Their repos are left alone.
func (m *Monitor) RefreshInFlight(ctx context.Context) {
	m.mutex.Lock()
	if !m.pausedUntil.IsZero() && time.Now().Before(m.pausedUntil) {
		m.mutex.Unlock()
		return
	}
	watched := map[string]map[int]bool{}
	for _, focus := range m.focus {
		if focus.Number > 0 {
			if watched[focus.Repo] == nil {
				watched[focus.Repo] = map[int]bool{}
			}
			watched[focus.Repo][focus.Number] = true
		}
	}
	byRepo := map[string][]int{}
	for name, repo := range m.repos {
		armed := m.armedLocked(name)
		for _, pr := range repo.PRs {
			_, isArmed := armed[pr.Number]
			if !isArmed && !watched[name][pr.Number] && !moving(pr, time.Now()) {
				continue
			}
			if len(byRepo[name]) == activeLimit {
				break // the PR list is newest first, and this is the cheap tier
			}
			byRepo[name] = append(byRepo[name], pr.Number)
		}
	}
	m.mutex.Unlock()
	for name, numbers := range byRepo {
		m.refreshPRs(ctx, name, numbers)
	}
}

// moving reports whether GitHub is working on this PR right now. Checks that
// have been pending for ages are something stuck, not something to watch.
func moving(pr models.PullRequest, now time.Time) bool {
	pending := pr.Status == models.StatusChecking
	for _, check := range pr.Checks {
		pending = pending || readiness.CheckPending(check)
	}
	if !pending {
		return false
	}
	if pr.Status == models.StatusChecking {
		return true // GitHub is still computing mergeability; that settles fast
	}
	return fresh(pr.LastCheckStartedAt, now) || fresh(pr.LastCommitAt, now)
}

func fresh(stamp *string, now time.Time) bool {
	if stamp == nil {
		return false
	}
	when, err := time.Parse(time.RFC3339Nano, *stamp)
	return err == nil && now.Sub(when) < activeWindow
}

// refreshPRs replaces named PRs in a loaded repo, leaving the rest as they are.
func (m *Monitor) refreshPRs(ctx context.Context, name string, numbers []int) {
	fetched, err := m.client.FetchPRs(ctx, name, numbers)
	if err != nil {
		m.handleFetchError(name, err)
		return
	}
	m.mutex.Lock()
	repo, loaded := m.fetched[name]
	if !loaded {
		m.mutex.Unlock()
		return
	}
	state := m.watchLocked(name)
	updated := map[int]models.PullRequest{}
	for _, pr := range fetched {
		updated[pr.Number] = pr
	}
	prs := []models.PullRequest{}
	changed := false
	for _, pr := range repo.PRs {
		if fresh, have := updated[pr.Number]; have {
			changed = changed || !samePR(pr, fresh)
			prs = append(prs, fresh)
			// It was fetched in full, but its updated_at may not have moved with
			// it (check runs don't always touch a PR), so leave that alone.
			continue
		}
		if slices.Contains(numbers, pr.Number) {
			changed = true // it closed or merged
			delete(state.updated, pr.Number)
			continue
		}
		prs = append(prs, pr)
	}
	repo.PRs = prs
	m.mutex.Unlock()
	if !changed {
		return
	}
	m.apply(name, repo)
}

// samePR reports whether a PR still looks exactly as it did, so an unchanged
// one raises no event.
func samePR(before, after models.PullRequest) bool {
	if before.Status != after.Status || before.StrictlyReady != after.StrictlyReady ||
		before.MergeState != after.MergeState || before.Mergeable != after.Mergeable ||
		len(before.Checks) != len(after.Checks) {
		return false
	}
	for i := range before.Checks {
		if before.Checks[i] != after.Checks[i] {
			return false
		}
	}
	return true
}

// noteFresh records a successful look, so clients can see the backend is alive.
func (m *Monitor) noteFresh(name string) {
	now := time.Now()
	stamp := now.UTC().Format(time.RFC3339Nano)
	m.mutex.Lock()
	m.watchLocked(name).lastLook = now
	delete(m.errs, name)
	m.status.LastUpdate = &stamp
	m.status.RateLimitedUntil = nil
	m.mutex.Unlock()
	m.emit(Event{Kind: "status"})
}
