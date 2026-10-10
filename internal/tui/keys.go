// Keys: S settings (adding, removing and notifications live there), r
// refresh, q quit, enter acts, m merges, a auto-merges, w dependencies, left
// and right pick a repo, up and down a PR, tab goes round the panes, J/K
// scroll the details from anywhere.

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

// handleDetailsKey scrolls the details with the arrows. Enter, m, a and w act
// on the PR they show, as in the PR list.
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
	case "enter", "w", "m", "a":
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
	case "m":
		return m, m.quickAction("merge", key)
	case "a":
		return m, m.quickAction("auto_merge", key)
	case "w":
		return m, m.openDependencies()
	}
	return m, nil
}

// quickAction runs one of the action menu's entries straight from the
// dashboard. The menu still asks for a merge method or a confirmation when it
// must; an entry that isn't possible says why instead.
func (m *Model) quickAction(slot string, key tea.KeyMsg) tea.Cmd {
	repo, found := m.backend.Repo(m.selectedRepo())
	pr, ok := m.selectedPR()
	if !found || !ok {
		return nil
	}
	menu := newActionMenu(repo, pr)
	menu.quick = true
	option, offered := menu.option(slot)
	if !offered {
		return nil
	}
	if !option.Available {
		m.addToast(option.Label+": "+derefString(option.Reason), "warning")
		return nil
	}
	next, cmd := menu.update(m, key)
	m.modal = next
	return cmd
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

// removeRepo asks before it stops monitoring the selected repo.
func (m *Model) removeRepo(name string) modal {
	return newConfirm("Stop monitoring "+name+"?", func(model *Model) tea.Cmd {
		return run(func() error { return model.backend.RemoveRepo(name) })
	})
}

// openNotifications is the selected repo's notification settings; nil, with
// a toast, when the backend can't say what to offer.
func (m *Model) openNotifications(name string) (modal, tea.Cmd) {
	form, err := m.backend.NotificationForm()
	if err != nil {
		m.addToast(err.Error(), "error")
		return nil, nil
	}
	settings, found := m.backend.Config().Notifications[name]
	if !found {
		settings = form.Defaults
	}
	dialog := newNotifications(name, settings, form, m.backend.Status().Notifier)
	return dialog, dialog.preview(m)
}

// perform sends an action and reports failures as toasts.
func (m *Model) perform(repo string, number int, action models.Action) tea.Cmd {
	return run(func() error { return m.backend.Perform(repo, number, action) })
}
