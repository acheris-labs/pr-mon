// Keys: A add, D remove, N notifications, S settings, r refresh, q quit,
// enter acts, w dependencies, arrows and tab navigate, J/K scroll the details.

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
		m.focus = panePRs
		if m.focus == panePRs && len(m.prs()) == 0 {
			m.focus = paneRepos
		}
		return m, m.markSeen()
	case "shift+tab":
		m.focus = paneRepos
		return m, nil
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
	if m.focus == paneRepos && m.compact() {
		return m.handleTabKey(key)
	}
	if m.focus == paneRepos {
		return m.handleTreeKey(key)
	}
	return m.handlePRKey(key)
}

// detailsPage is how far page up and page down scroll the details.
func (m *Model) detailsPage() int {
	_, height := m.detailsSize()
	return max(1, height-3)
}

// moveRepo steps the repo cursor, stopping at either end.
func (m *Model) moveRepo(by int) {
	if next := m.cursor + by; next >= 0 && next < len(m.rows) {
		m.cursor = next
		m.prCursor = 0
	}
}

// handleTabKey is the repo keys on a narrow screen: the tabs run sideways and
// the PR list sits below them.
func (m *Model) handleTabKey(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch key.String() {
	case "left", "h":
		m.moveRepo(-1)
	case "right", "l":
		m.moveRepo(1)
	case "down", "j", "enter":
		return m, m.enterRow()
	}
	return m, nil
}

func (m *Model) handleTreeKey(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch key.String() {
	case "up", "k":
		m.moveRepo(-1)
	case "down", "j":
		m.moveRepo(1)
	case "left", "h":
		return m, m.collapseOrParent()
	case "right", "l":
		return m, m.expand()
	case "enter":
		return m, m.enterRow()
	}
	return m, nil
}

// collapseOrParent moves from a repo to its group, or folds an open group away.
func (m *Model) collapseOrParent() tea.Cmd {
	current, ok := m.currentRow()
	if !ok {
		return nil
	}
	if current.repo != "" {
		for index, candidate := range m.rows {
			if candidate.repo == "" && candidate.owner == current.owner {
				m.cursor = index
				break
			}
		}
		return nil
	}
	if !m.isCollapsed(current.owner) {
		return m.setCollapsed(current.owner, true)
	}
	return nil
}

func (m *Model) expand() tea.Cmd {
	current, ok := m.currentRow()
	if !ok || current.repo != "" || !m.isCollapsed(current.owner) {
		return nil
	}
	return m.setCollapsed(current.owner, false)
}

// enterRow folds a group, or moves into a repo's pull requests.
func (m *Model) enterRow() tea.Cmd {
	current, ok := m.currentRow()
	if !ok {
		return nil
	}
	if current.repo == "" {
		return m.setCollapsed(current.owner, !m.isCollapsed(current.owner))
	}
	if len(m.prs()) == 0 {
		return nil
	}
	m.focus = panePRs
	m.prCursor = 0
	return m.markSeen()
}

func (m *Model) isCollapsed(owner string) bool {
	for _, candidate := range m.backend.Collapsed() {
		if candidate == owner {
			return true
		}
	}
	return false
}

func (m *Model) setCollapsed(owner string, collapsed bool) tea.Cmd {
	keep := m.selectedRepo()
	if keep == "" {
		keep = ""
	}
	cmd := run(func() error { return m.backend.SetCollapsed(owner, collapsed) })
	return cmd
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
