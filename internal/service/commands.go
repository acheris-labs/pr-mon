// What clients ask the monitor to do.

package service

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/acheris-labs/pr-mon/internal/config"
	"github.com/acheris-labs/pr-mon/internal/models"
	"github.com/acheris-labs/pr-mon/internal/notify"
	"github.com/acheris-labs/pr-mon/internal/state"
)

func nowISO() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func (m *Monitor) saveStateLocked() {
	if err := state.Save(m.statePath, m.state); err != nil {
		m.log.Warn("could not save state", "path", m.statePath, "error", err)
	}
}

func (m *Monitor) saveConfigLocked() {
	if err := config.Save(m.configPath, m.config); err != nil {
		m.log.Warn("could not save config", "path", m.configPath, "error", err)
	}
}

func (m *Monitor) requireRepoLocked(name string) error {
	for _, repo := range m.config.Repos {
		if repo == name {
			return nil
		}
	}
	return &Error{Message: fmt.Sprintf("%s is not being monitored", name)}
}

// RefreshAll polls every repo in the background.
func (m *Monitor) RefreshAll() {
	select {
	case m.pollWake <- struct{}{}:
	default: // a poll is already due
	}
}

// AddRepo starts monitoring a repo and returns the name as GitHub spells it.
func (m *Monitor) AddRepo(ctx context.Context, name string) (string, error) {
	m.mutex.Lock()
	for _, repo := range m.config.Repos {
		if strings.EqualFold(repo, name) {
			m.mutex.Unlock()
			return "", &Error{Message: fmt.Sprintf("%s is already being monitored", name)}
		}
	}
	m.mutex.Unlock()

	repo, err := m.client.FetchRepo(ctx, name)
	if err != nil {
		return "", &Error{Message: err.Error()}
	}
	m.mutex.Lock()
	for _, existing := range m.config.Repos {
		if existing == repo.Name {
			m.mutex.Unlock()
			return "", &Error{Message: fmt.Sprintf("%s is already being monitored", repo.Name)}
		}
	}
	m.config.Repos = append(m.config.Repos, repo.Name)
	m.saveConfigLocked()
	m.mutex.Unlock()

	m.emit(Event{Kind: "repos"})
	m.apply(repo.Name, repo)
	return repo.Name, nil
}

// RemoveRepo forgets a repo, its notification settings and its armed merges.
func (m *Monitor) RemoveRepo(name string) error {
	m.mutex.Lock()
	if err := m.requireRepoLocked(name); err != nil {
		m.mutex.Unlock()
		return err
	}
	kept := []string{}
	for _, repo := range m.config.Repos {
		if repo != name {
			kept = append(kept, repo)
		}
	}
	m.config.Repos = kept
	delete(m.config.Notifications, name)
	delete(m.repos, name)
	delete(m.fetched, name)
	delete(m.errs, name)
	delete(m.loaded, name)
	m.track.Forget(name)
	delete(m.state.Armed, name)
	m.forgetWaitingLocked(func(repo string, _ int) bool { return repo == name })
	m.saveConfigLocked()
	m.saveStateLocked()
	m.mutex.Unlock()

	m.emit(Event{Kind: "repos"})
	return nil
}

// MarkSeen clears a PR's new marker.
func (m *Monitor) MarkSeen(name string, number int) {
	m.mutex.Lock()
	changed := m.track.MarkSeen(name, number)
	if changed {
		m.saveStateLocked()
	}
	m.mutex.Unlock()
	if changed {
		m.emit(Event{Kind: "seen", Name: name})
	}
}

// SetCollapsed remembers whether an owner's group is folded away.
func (m *Monitor) SetCollapsed(owner string, collapsed bool) {
	m.mutex.Lock()
	owners := map[string]bool{}
	for _, name := range m.state.Collapsed {
		owners[name] = true
	}
	if owners[owner] == collapsed {
		m.mutex.Unlock()
		return
	}
	if collapsed {
		owners[owner] = true
	} else {
		delete(owners, owner)
	}
	updated := []string{}
	for name := range owners {
		updated = append(updated, name)
	}
	sort.Strings(updated)
	m.state.Collapsed = updated
	m.saveStateLocked()
	m.mutex.Unlock()
	m.emit(Event{Kind: "collapsed"})
}

// Perform runs one action against a PR. GitHub failures are reported as toasts,
// because the user asked for them from a menu, not as a request that failed.
func (m *Monitor) Perform(ctx context.Context, repoName string, number int, action models.Action) error {
	repo, found := m.Repo(repoName)
	var pr models.PullRequest
	var havePR bool
	if found {
		for _, candidate := range repo.PRs {
			if candidate.Number == number {
				pr, havePR = candidate, true
				break
			}
		}
	}
	if !havePR {
		return &Error{Message: fmt.Sprintf("%s#%d is not an open PR", repoName, number)}
	}
	label := fmt.Sprintf("%s#%d", repoName, number)
	if pr.Waiting() {
		switch action.Kind {
		case "merge":
			return &Error{Message: fmt.Sprintf(
				"%s waits on PRs that haven't merged; remove them to merge it now", label)}
		case "auto_merge_on":
			// GitHub would merge it without waiting; pr-mon's merge when ready waits.
			action.Kind = "arm_merge"
		}
	}

	switch action.Kind {
	case "arm_merge":
		if action.Method == nil {
			return &Error{Message: "arming a merge needs a merge method"}
		}
		merge := models.ArmedMerge{
			Method: *action.Method, DeleteBranch: action.DeleteBranch, ArmedAt: nowISO(),
		}
		if err := m.arm(repoName, number, merge); err != nil {
			return &Error{Message: err.Error()}
		}
		m.toast(fmt.Sprintf("pr-mon will merge %s when it's ready (%s)", label, merge.Method.Lower()))
		m.emit(Event{Kind: "repo", Name: repoName})
		if repo, found := m.Repo(repoName); found {
			m.checkArmed(repoName, repo)
		}
		return nil
	case "disarm_merge":
		m.disarm(repoName, number)
		m.toast(fmt.Sprintf("Cancelled merge when ready for %s", label))
		m.emit(Event{Kind: "repo", Name: repoName})
		return nil
	}

	key := prKey{repo: repoName, number: number}
	manualMerge := false
	if action.Kind == "merge" {
		m.mutex.Lock()
		if !m.merging[key] {
			m.merging[key] = true
			manualMerge = true
		}
		m.mutex.Unlock()
	}
	err := m.runAction(ctx, repoName, pr, action, label)
	if manualMerge {
		m.mutex.Lock()
		delete(m.merging, key)
		m.mutex.Unlock()
	}
	if err != nil {
		what := strings.ToUpper(action.Kind[:1]) + strings.ReplaceAll(action.Kind[1:], "_", " ")
		m.toast(fmt.Sprintf("%s failed for %s: %v", what, label, err), "error")
	}
	m.RefreshRepo(ctx, repoName, 0)
	return nil
}

func (m *Monitor) runAction(ctx context.Context, repoName string, pr models.PullRequest,
	action models.Action, label string) error {
	switch action.Kind {
	case "merge":
		if action.Method == nil {
			return &Error{Message: "merging needs a merge method"}
		}
		if err := m.client.Merge(ctx, pr.ID, *action.Method, ""); err != nil {
			return err
		}
		m.toast(fmt.Sprintf("Merged %s (%s)", label, action.Method.Lower()))
		m.merged(repoName, pr)
		defer m.refreshWaiting(ctx, repoName, pr.Number)
		if action.DeleteBranch && pr.HeadRefID != nil {
			if err := m.client.DeleteBranch(ctx, *pr.HeadRefID); err != nil {
				m.toast(fmt.Sprintf("Merged %s, but couldn't delete %s: %v", label, pr.HeadRef, err),
					"warning")
			}
		}
	case "auto_merge_on":
		if action.Method == nil {
			return &Error{Message: "enabling auto-merge needs a merge method"}
		}
		if err := m.client.EnableAutoMerge(ctx, pr.ID, *action.Method); err != nil {
			return err
		}
		m.toast(fmt.Sprintf("Auto-merge enabled for %s (%s)", label, action.Method.Lower()))
	case "auto_merge_off":
		if err := m.client.DisableAutoMerge(ctx, pr.ID); err != nil {
			return err
		}
		m.toast(fmt.Sprintf("Auto-merge disabled for %s", label))
	default:
		if err := m.client.UpdateBranch(ctx, pr.ID); err != nil {
			return err
		}
		m.toast(fmt.Sprintf("Updated branch for %s", label))
	}
	return nil
}

// SaveNotifications stores one repo's notification settings.
func (m *Monitor) SaveNotifications(repo string, settings config.NotifyConfig) error {
	m.mutex.Lock()
	if err := m.requireRepoLocked(repo); err != nil {
		m.mutex.Unlock()
		return err
	}
	m.config.Notifications[repo] = settings
	m.saveConfigLocked()
	m.mutex.Unlock()
	m.emit(Event{Kind: "config"})
	m.toast(fmt.Sprintf("Notification settings saved for %s", repo))
	return nil
}

// SendTest fires a sample notification with unsaved settings.
func (m *Monitor) SendTest(repo string, settings config.NotifyConfig) {
	m.send(settings, notify.SampleVariables(repo), true)
}

// SetPollInterval changes how often the backend checks GitHub.
func (m *Monitor) SetPollInterval(seconds int) error {
	if seconds < MinPollInterval || seconds > MaxPollInterval {
		return &Error{Message: fmt.Sprintf("poll interval must be between %d and %d seconds",
			MinPollInterval, MaxPollInterval)}
	}
	m.mutex.Lock()
	if seconds == m.config.PollInterval {
		m.mutex.Unlock()
		return nil
	}
	m.config.PollInterval = seconds
	m.saveConfigLocked()
	m.mutex.Unlock()
	m.emit(Event{Kind: "config"})
	m.toast(fmt.Sprintf("Checking GitHub every %ds", seconds))
	return nil
}

func (m *Monitor) NotificationForm() notify.Form { return notify.NotificationForm() }

func (m *Monitor) PreviewNotification(repo, message string) notify.Preview {
	return notify.PreviewMessage(repo, message)
}

// RequestShutdown asks the backend to stop; the daemon watches Shutdown.
func (m *Monitor) RequestShutdown() {
	m.shutOnce.Do(func() { close(m.Shutdown) })
}
