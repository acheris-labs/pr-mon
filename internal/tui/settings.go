// The settings dialog: how often the backend checks GitHub, (on macOS)
// whether it starts at login, as in the app's Settings › General, and the
// things done to a repo now and then: adding, removing, its notifications.

package tui

import (
	"fmt"
	"runtime"
	"slices"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/acheris-labs/pr-mon/internal/autostart"
	"github.com/acheris-labs/pr-mon/internal/daemon"
)

// pollIntervals are the choices offered, as in the app.
var pollIntervals = []int{30, 60, 120, 300, 600, 1800}

// LoginItem starts the backend at login.
type LoginItem interface {
	Enabled() (bool, error)
	SetEnabled(enabled bool) error
}

// launchdLoginItem is `pr-mon autostart`: the app's login item when this
// pr-mon lives in PrMon.app, a LaunchAgent otherwise.
type launchdLoginItem struct{ paths daemon.Paths }

func (l launchdLoginItem) Enabled() (bool, error) {
	enabled, _, err := autostart.Describe(nil)
	return enabled, err
}

func (l launchdLoginItem) SetEnabled(enabled bool) error {
	if enabled {
		_, err := autostart.Enable(l.paths, nil, nil)
		return err
	}
	_, err := autostart.Disable(l.paths, nil)
	return err
}

// defaultLoginItem is nil where there is no launchd to register with.
func defaultLoginItem(paths daemon.Paths) LoginItem {
	if runtime.GOOS != "darwin" {
		return nil
	}
	return launchdLoginItem{paths: paths}
}

type settingsModal struct {
	cursor int // in rows()
	// Start at login, as last read; changing it takes a few seconds.
	atLogin  bool
	checking bool
	problem  string
}

// loginChecked carries the login item's state after reading or changing it.
type loginChecked struct {
	enabled bool
	err     error
}

func newSettings(model *Model) (*settingsModal, tea.Cmd) {
	dialog := &settingsModal{}
	if model.loginItem == nil {
		return dialog, nil
	}
	dialog.checking = true
	item := model.loginItem
	return dialog, func() tea.Msg {
		enabled, err := item.Enabled()
		return loginChecked{enabled: enabled, err: err}
	}
}

func (s *settingsModal) title() string { return "Settings" }

type settingsRow int

const (
	rowInterval settingsRow = iota
	rowLogin
	rowAdd
	rowRemove
	rowNotify
)

// repoRows are the rows that open another dialog, by their keys.
var repoRows = []struct {
	row settingsRow
	key string
}{{rowAdd, "a"}, {rowRemove, "d"}, {rowNotify, "n"}}

// rows are the dialog's rows, top to bottom. Removing and notifications need
// a repo selected.
func (s *settingsModal) rows(model *Model) []settingsRow {
	rows := []settingsRow{rowInterval}
	if model.loginItem != nil {
		rows = append(rows, rowLogin)
	}
	rows = append(rows, rowAdd)
	if model.selectedRepo() != "" {
		rows = append(rows, rowRemove, rowNotify)
	}
	return rows
}

func (s *settingsModal) update(model *Model, key tea.KeyMsg) (modal, tea.Cmd) {
	rows := s.rows(model)
	s.cursor = min(s.cursor, len(rows)-1)
	switch key.String() {
	case "esc", "escape", "q", "S":
		return nil, nil
	case "down", "j", "tab":
		s.cursor = (s.cursor + 1) % len(rows)
		return s, nil
	case "up", "k", "shift+tab":
		s.cursor = (s.cursor + len(rows) - 1) % len(rows)
		return s, nil
	}
	for _, repoRow := range repoRows {
		if key.String() == repoRow.key && slices.Contains(rows, repoRow.row) {
			return s.open(model, repoRow.row)
		}
	}
	switch rows[s.cursor] {
	case rowAdd, rowRemove, rowNotify:
		switch key.String() {
		case "enter", " ", "space":
			return s.open(model, rows[s.cursor])
		}
		return s, nil
	case rowInterval:
		switch key.String() {
		case "left", "h", "right", "l":
			up := key.String() == "right" || key.String() == "l"
			seconds := nextInterval(model.backend.Config().PollInterval, up)
			return s, run(func() error { return model.backend.SetPollInterval(seconds) })
		}
		return s, nil
	}
	switch key.String() {
	case " ", "space", "enter", "left", "h", "right", "l":
		if s.checking {
			return s, nil
		}
		s.checking = true
		s.problem = ""
		want := !s.atLogin
		item := model.loginItem
		return s, func() tea.Msg {
			if err := item.SetEnabled(want); err != nil {
				return loginChecked{enabled: !want, err: err}
			}
			enabled, err := item.Enabled()
			return loginChecked{enabled: enabled, err: err}
		}
	}
	return s, nil
}

// open swaps the settings for the dialog a repo row stands for.
func (s *settingsModal) open(model *Model, row settingsRow) (modal, tea.Cmd) {
	name := model.selectedRepo()
	switch row {
	case rowRemove:
		return model.removeRepo(name), nil
	case rowNotify:
		if dialog, cmd := model.openNotifications(name); dialog != nil {
			return dialog, cmd
		}
		return s, nil
	}
	return newAddRepo(), nil
}

// rowLabel is a repo row's text.
func rowLabel(row settingsRow, repo string) string {
	switch row {
	case rowRemove:
		return "Remove " + repo + "…"
	case rowNotify:
		return "Notifications for " + repo + "…"
	}
	return "Add repository…"
}

func (s *settingsModal) checked(done loginChecked) {
	s.checking = false
	s.atLogin = done.enabled
	s.problem = ""
	if done.err != nil {
		s.problem = done.err.Error()
	}
}

// nextInterval steps through pollIntervals from `seconds`, which may be a
// value set by hand in config.toml that isn't one of them.
func nextInterval(seconds int, up bool) int {
	if up {
		for _, choice := range pollIntervals {
			if choice > seconds {
				return choice
			}
		}
		return pollIntervals[len(pollIntervals)-1]
	}
	for _, choice := range slices.Backward(pollIntervals) {
		if choice < seconds {
			return choice
		}
	}
	return pollIntervals[0]
}

func intervalLabel(seconds int) string {
	if seconds%60 == 0 {
		return fmt.Sprintf("every %d min", seconds/60)
	}
	return fmt.Sprintf("every %d s", seconds)
}

func (s *settingsModal) view(model *Model) string {
	lines := []string{bold.Render(s.title()), ""}
	for index, row := range s.rows(model) {
		style := lipgloss.NewStyle()
		if index == s.cursor {
			style = selected
		}
		switch row {
		case rowInterval:
			lines = append(lines, style.Render(
				"Check GitHub:  ◀ "+intervalLabel(model.backend.Config().PollInterval)+" ▶"))
		case rowLogin:
			box := "[ ]"
			if s.atLogin {
				box = "[x]"
			}
			text := style.Render(box + " Start the backend at login")
			if s.checking {
				text += dim.Render("  …")
			}
			lines = append(lines, text)
			// What the toggle means, and what went wrong with it, sit under it.
			if s.problem != "" {
				lines = append(lines, lipgloss.NewStyle().Foreground(red).
					Width(model.modalInner()).Render(s.problem))
			}
			lines = append(lines, lipgloss.NewStyle().Faint(true).Width(model.modalInner()).Render(
				"On: the backend starts at login and runs all the time. Off: it runs while "+
					"this dashboard or the app is open, and stops two minutes after the last one quits."))
		default:
			if row == rowAdd {
				lines = append(lines, "")
			}
			for _, repoRow := range repoRows {
				if repoRow.row == row {
					text := "[" + repoRow.key + "] " + rowLabel(row, model.selectedRepo())
					lines = append(lines, model.mark(zoneKey+repoRow.key, style.Render(text)))
				}
			}
		}
	}
	hint := "↑/↓: choose   ←/→/space: change   enter: open   esc: close"
	return strings.Join(append(lines, "", model.hints(hint, model.modalInner())), "\n")
}
