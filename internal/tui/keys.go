// Keys: A add, D remove, N notifications, S settings, r refresh, q quit,
// enter acts, w dependencies, left and right pick a repo, up and down a PR,
// tab goes round the panes, J/K scroll the details from anywhere.

package tui

import (
	tea "github.com/charmbracelet/bubbletea"

	"github.com/acheris-labs/pr-mon/internal/models"
)

func (m *Model) handleKey(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	// A modal takes every key until it closes.
	if m.modal != nil {
		next, cmd := m.modal.update(m, key)
		m.modal = next
		return m, cmd
	}
	switch key.String() {
	case "ctrl+c", "q":
		m.quitting = true
		return m, tea.Quit
	case "r":
		return m, run(m.backend.RefreshAll)
	case "A":
		m.modal = newAddRepo()
		return m, nil
	case "D":
		return m, m.removeRepo()
	case "N":
		return m, m.openNotifications()
	case "S":
		dialog, cmd := newSettings(m)
		m.modal = dialog
		return m, cmd
	case "tab":
		return m, m.cycleFocus(1)
	case "shift+tab":
		return m, m.cycleFocus(-1)
	case "J":
		m.scrollDetails(1)
		return m, nil
	case "K":
		m.scrollDetails(-1)
		return m, nil
	case "pgdown":
		m.scrollDetails(m.detailsPage())
		return m, nil
	case "pgup":
		m.scrollDetails(-m.detailsPage())
		return m, nil
	}
	switch m.focus {
	case paneRepos:
		return m.handleRepoKey(key)
	case paneDetails:
		return m.handleDetailsKey(key)
	}
	return m.handlePRKey(key)
}

// cycleFocus moves round the panes: repos, PRs, details. A repo with no PRs
// has nothing in the other two, so focus stays on the repos.
func (m *Model) cycleFocus(by int) tea.Cmd {
	if len(m.prs()) == 0 {
		m.focus = paneRepos
		return nil
	}
	m.focus = (m.focus + pane(by) + 3) % 3
	return m.markSeen()
}

// handleDetailsKey scrolls the details with the arrows. Enter and w act on the
// PR they show, as in the PR list.
func (m *Model) handleDetailsKey(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch key.String() {
	case "up", "k":
		m.scrollDetails(-1)
	case "down", "j":
		m.scrollDetails(1)
	case "home", "g":
		m.detailScroll = 0
	case "end", "G":
		width, _ := m.detailsSize()
		m.scrollDetails(len(m.detailLines(width - 2)))
	case "left", "h", "esc", "escape":
		m.focus = panePRs
	case "enter", "w":
		return m.handlePRKey(key)
	}
	return m, nil
}

// detailsPage is how far page up and page down scroll the details.
func (m *Model) detailsPage() int {
	_, height := m.detailsSize()
	return max(1, height-3)
}

// moveRepo steps the repo cursor, stopping at either end.
func (m *Model) moveRepo(by int) {
	if next := m.cursor + by; next >= 0 && next < len(m.repos) {
		m.cursor = next
		m.prCursor = 0
	}
}

// handleRepoKey is the keys on the repo tabs: they run sideways, and the PR
// list sits below them.
func (m *Model) handleRepoKey(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch key.String() {
	case "left", "h":
		m.moveRepo(-1)
	case "right", "l":
		m.moveRepo(1)
	case "down", "j", "enter":
		return m, m.enterRepo()
	}
	return m, nil
}

// enterRepo moves into the selected repo's pull requests.
func (m *Model) enterRepo() tea.Cmd {
	if len(m.prs()) == 0 {
		return nil
	}
	m.focus = panePRs
	m.prCursor = 0
	return m.markSeen()
}

func (m *Model) handlePRKey(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	prs := m.prs()
	switch key.String() {
	case "up", "k":
		if m.prCursor > 0 {
			m.prCursor--
		}
		return m, m.markSeen()
	case "down", "j":
		if m.prCursor < len(prs)-1 {
			m.prCursor++
		}
		return m, m.markSeen()
	case "home", "g":
		m.prCursor = 0
		return m, m.markSeen()
	case "end", "G":
		m.prCursor = max(0, len(prs)-1)
		return m, m.markSeen()
	case "left", "h", "esc", "escape":
		m.focus = paneRepos
		return m, nil
	case "enter":
		return m, m.openActions()
	case "w":
		return m, m.openDependencies()
	}
	return m, nil
}

func (m *Model) openDependencies() tea.Cmd {
	name := m.selectedRepo()
	pr, ok := m.selectedPR()
	if name == "" || !ok {
		return nil
	}
	m.modal = newDependencies(name, pr.Number)
	return nil
}

func (m *Model) openActions() tea.Cmd {
	name := m.selectedRepo()
	repo, found := m.backend.Repo(name)
	pr, ok := m.selectedPR()
	if !found || !ok {
		return nil
	}
	m.modal = newActionMenu(repo, pr)
	return nil
}

func (m *Model) removeRepo() tea.Cmd {
	name := m.selectedRepo()
	if name == "" {
		return nil
	}
	m.modal = newConfirm("Stop monitoring "+name+"?", func(model *Model) tea.Cmd {
		return run(func() error { return model.backend.RemoveRepo(name) })
	})
	return nil
}

func (m *Model) openNotifications() tea.Cmd {
	name := m.selectedRepo()
	if name == "" {
		m.addToast("Select a repository to configure its notifications", "warning")
		return nil
	}
	form, err := m.backend.NotificationForm()
	if err != nil {
		m.addToast(err.Error(), "error")
		return nil
	}
	settings, found := m.backend.Config().Notifications[name]
	if !found {
		settings = form.Defaults
	}
	m.modal = newNotifications(name, settings, form, m.backend.Status().Notifier)
	return m.modal.(*notificationsModal).preview(m)
}

// perform sends an action and reports failures as toasts.
func (m *Model) perform(repo string, number int, action models.Action) tea.Cmd {
	return run(func() error { return m.backend.Perform(repo, number, action) })
}
