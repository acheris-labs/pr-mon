// PR dependencies: PRs that must merge before another may. The PR that waits is
// always in a monitored repo; what it waits on can be anywhere on GitHub.

package service

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/acheris-labs/pr-mon/internal/dependencies"
	"github.com/acheris-labs/pr-mon/internal/github"
	"github.com/acheris-labs/pr-mon/internal/models"
)

func (m *Monitor) graphLocked() dependencies.Graph {
	if m.state.Dependencies == nil {
		m.state.Dependencies = map[string][]string{}
	}
	return dependencies.Graph(m.state.Dependencies)
}

func (m *Monitor) refsLocked(keys []string) []models.PRRef {
	refs := make([]models.PRRef, 0, len(keys))
	for _, key := range keys {
		refs = append(refs, m.refLocked(key))
	}
	return refs
}

// refLocked is the best pr-mon knows about a PR. A merge or close seen by a
// lookup wins, because the repo holding the PR may not have been refreshed yet;
// an open PR in a monitored repo also carries pr-mon's status.
func (m *Monitor) refLocked(key string) models.PRRef {
	known, found := m.waitedOn[key]
	if found && (known.State == models.PRMerged || known.State == models.PRClosed) {
		return known
	}
	repo, number, _ := dependencies.ParseKey(key)
	if loaded, monitored := m.repos[repo]; monitored {
		for _, pr := range loaded.PRs {
			if pr.Number == number {
				return models.PRRef{
					Repo: repo, Number: number, Title: pr.Title, URL: pr.URL,
					State: models.PROpen, Status: models.Ptr(pr.Status),
				}
			}
		}
	}
	if found {
		return known
	}
	return models.PRRef{
		Repo: repo, Number: number, State: models.PRUnknown,
		URL: fmt.Sprintf("https://github.com/%s/pull/%d", repo, number),
	}
}

// forgetWaitingLocked drops what the matching waiting PRs wait on.
func (m *Monitor) forgetWaitingLocked(matches func(repo string, number int) bool) bool {
	graph := m.graphLocked()
	changed := false
	for key := range graph {
		if repo, number, ok := dependencies.ParseKey(key); ok && matches(repo, number) {
			graph.Forget(key)
			changed = true
		}
	}
	return changed
}

// pruneDependenciesLocked forgets the dependencies of PRs that merged or closed.
func (m *Monitor) pruneDependenciesLocked(repo models.Repo) {
	if repo.PRTotal > len(repo.PRs) {
		return // beyond the newest-N cut-off a PR may still be open
	}
	open := map[int]bool{}
	for _, pr := range repo.PRs {
		open[pr.Number] = true
	}
	if m.forgetWaitingLocked(func(name string, number int) bool {
		return name == repo.Name && !open[number]
	}) {
		m.saveStateLocked()
	}
}

// refreshWaitedOn looks up every PR something waits on: one query per repo.
func (m *Monitor) refreshWaitedOn(ctx context.Context) {
	m.mutex.Lock()
	targets := m.graphLocked().Targets()
	m.mutex.Unlock()
	byRepo := map[string][]int{}
	for _, key := range targets {
		if repo, number, ok := dependencies.ParseKey(key); ok {
			byRepo[repo] = append(byRepo[repo], number)
		}
	}
	found := map[string]models.PRRef{}
	var mutex sync.Mutex
	var group sync.WaitGroup
	for repo, numbers := range byRepo {
		group.Add(1)
		go func() {
			defer group.Done()
			refs, err := m.client.LookupPRs(ctx, repo, numbers)
			if err != nil {
				m.log.Warn("could not look up PRs others wait on", "repo", repo, "error", err)
				return
			}
			mutex.Lock()
			defer mutex.Unlock()
			for _, ref := range refs {
				ref.Repo = repo // keep the spelling the dependency was saved with
				found[ref.Key()] = ref
			}
		}()
	}
	group.Wait()

	m.mutex.Lock()
	defer m.mutex.Unlock()
	wanted := map[string]bool{}
	for _, key := range targets {
		wanted[key] = true
	}
	for key := range m.waitedOn {
		if !wanted[key] {
			delete(m.waitedOn, key)
		}
	}
	for key, ref := range found {
		m.waitedOn[key] = ref
	}
}

// merged records that pr-mon merged a PR, so anything waiting on it can go
// ahead without another lookup.
func (m *Monitor) merged(name string, pr models.PullRequest) {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	key := models.PRKey(name, pr.Number)
	if _, waitedOn := m.waitedOn[key]; waitedOn || len(m.graphLocked().RequiredBy(key)) > 0 {
		m.waitedOn[key] = models.PRRef{
			Repo: name, Number: pr.Number, Title: pr.Title, URL: pr.URL, State: models.PRMerged,
		}
	}
}

// refreshWaiting refreshes the repos of PRs that waited on one that just merged,
// rather than leaving them for the next poll: they may be armed and ready now.
func (m *Monitor) refreshWaiting(ctx context.Context, name string, number int) {
	m.mutex.Lock()
	waiting := m.graphLocked().RequiredBy(models.PRKey(name, number))
	m.mutex.Unlock()
	refreshed := map[string]bool{}
	for _, key := range waiting {
		repo, _, ok := dependencies.ParseKey(key)
		if !ok || refreshed[repo] {
			continue
		}
		refreshed[repo] = true
		m.RefreshRepo(ctx, repo, 0)
	}
}

// holdForDependencies moves waiting PRs off GitHub's auto-merge, which would
// merge them without waiting, onto pr-mon's merge when ready with the same
// method.
func (m *Monitor) holdForDependencies(name string) {
	repo, found := m.Repo(name)
	if !found {
		return
	}
	for _, pr := range repo.PRs {
		if pr.AutoMerge == nil || !pr.Waiting() {
			continue
		}
		key := prKey{repo: name, number: pr.Number}
		m.mutex.Lock()
		skip := m.holding[key] || m.holdFailed[key]
		if !skip {
			m.holding[key] = true
		}
		m.mutex.Unlock()
		if skip {
			continue
		}
		m.spawn(func() { m.hold(name, pr) })
	}
}

func (m *Monitor) hold(name string, pr models.PullRequest) {
	key := prKey{repo: name, number: pr.Number}
	defer func() {
		m.mutex.Lock()
		delete(m.holding, key)
		m.mutex.Unlock()
	}()
	label := models.PRKey(name, pr.Number)
	method := pr.AutoMerge.Method
	if err := m.client.DisableAutoMerge(m.ctx, pr.ID); err != nil {
		m.mutex.Lock()
		m.holdFailed[key] = true
		m.mutex.Unlock()
		m.log.Warn("could not turn off GitHub auto-merge for a waiting PR", "pr", label, "error", err)
		m.toast(fmt.Sprintf("%s waits on other PRs, but GitHub auto-merge couldn't be turned off, "+
			"so GitHub may merge it first: %v", label, err), "error")
		return
	}
	if err := m.arm(name, pr.Number, models.ArmedMerge{Method: method, ArmedAt: nowISO()}); err != nil {
		m.toast(fmt.Sprintf("Couldn't arm %s: %v", label, err), "error")
		return
	}
	m.toast(fmt.Sprintf("%s waits on other PRs, so pr-mon will merge it instead of "+
		"GitHub auto-merge (%s)", label, method.Lower()))
	m.RefreshRepo(m.ctx, name, 0)
}

// openPR is a PR in a monitored repo that a command can act on.
func (m *Monitor) openPR(name string, number int) (models.PullRequest, error) {
	if repo, found := m.Repo(name); found {
		for _, pr := range repo.PRs {
			if pr.Number == number {
				return pr, nil
			}
		}
	}
	return models.PullRequest{}, &Error{Message: fmt.Sprintf("%s#%d is not an open PR", name, number)}
}

// AddDependency makes a PR wait for another to merge first. `on` is
// "owner/repo#12" or a pull request URL, in any repo.
func (m *Monitor) AddDependency(ctx context.Context, name string, number int, on string) error {
	pr, err := m.openPR(name, number)
	if err != nil {
		return err
	}
	repo, target, err := dependencies.ParseRef(on)
	if err != nil {
		return &Error{Message: err.Error()}
	}
	refs, err := m.client.LookupPRs(ctx, repo, []int{target})
	if err != nil {
		var missing *github.NotFoundError
		if errors.As(err, &missing) {
			return &Error{Message: fmt.Sprintf("%s#%d not found", repo, target)}
		}
		return &Error{Message: err.Error()}
	}
	ref := refs[0]
	switch ref.State {
	case models.PRMerged:
		return &Error{Message: fmt.Sprintf("%s has already merged", ref.Key())}
	case models.PRClosed:
		return &Error{Message: fmt.Sprintf("%s is closed", ref.Key())}
	}
	from := models.PRKey(name, number)

	m.mutex.Lock()
	if err := m.graphLocked().Add(from, ref.Key()); err != nil {
		m.mutex.Unlock()
		return &Error{Message: err.Error()}
	}
	m.waitedOn[ref.Key()] = ref
	m.saveStateLocked()
	m.rebuildLocked(name)
	m.rebuildLocked(ref.Repo)
	m.mutex.Unlock()

	m.toast(fmt.Sprintf("%s now waits on %s", from, ref.Key()))
	m.emitRepos(name, ref.Repo)
	if pr.AutoMerge != nil {
		m.holdForDependencies(name)
	}
	return nil
}

// RemoveDependency stops a PR waiting on another.
func (m *Monitor) RemoveDependency(name string, number int, on string) error {
	repo, target, err := dependencies.ParseRef(on)
	if err != nil {
		return &Error{Message: err.Error()}
	}
	from, to := models.PRKey(name, number), models.PRKey(repo, target)
	m.mutex.Lock()
	if !m.graphLocked().Remove(from, to) {
		m.mutex.Unlock()
		return &Error{Message: fmt.Sprintf("%s doesn't wait on %s", from, to)}
	}
	m.saveStateLocked()
	m.rebuildLocked(name)
	m.rebuildLocked(repo)
	m.mutex.Unlock()

	m.toast(fmt.Sprintf("%s no longer waits on %s", from, to))
	m.emitRepos(name, repo)
	// It may be ready now, and armed.
	if loaded, found := m.Repo(name); found {
		m.checkArmed(name, loaded)
	}
	return nil
}

func (m *Monitor) emitRepos(first, second string) {
	m.emit(Event{Kind: "repo", Name: first})
	if second != first {
		if _, monitored := m.Repo(second); monitored {
			m.emit(Event{Kind: "repo", Name: second})
		}
	}
}

// DependencyGraph is everything connected to a PR, both ways.
func (m *Monitor) DependencyGraph(name string, number int) models.DependencyGraph {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	graph := m.graphLocked()
	key := models.PRKey(name, number)
	return models.DependencyGraph{
		PR:         m.refLocked(key),
		WaitsOn:    graph.Tree(key, true, m.refLocked),
		RequiredBy: graph.Tree(key, false, m.refLocked),
	}
}
