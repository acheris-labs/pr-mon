// The settings dialog: how often the backend checks GitHub, and (on macOS)
// whether it starts at login, as in the app's Settings › General.

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
	cursor int // 0: poll interval, 1: start at login
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

func (s *settingsModal) rows(model *Model) int {
	if model.loginItem == nil {
		return 1
	}
	return 2
}

func (s *settingsModal) update(model *Model, key tea.KeyMsg) (modal, tea.Cmd) {
	switch key.String() {
	case "esc", "escape", "q", "S":
		return nil, nil
	case "up", "k", "down", "j", "tab":
		s.cursor = (s.cursor + 1) % s.rows(model)
		return s, nil
	}
	if s.cursor == 0 {
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
	rows := []string{"Check GitHub:  ◀ " + intervalLabel(model.backend.Config().PollInterval) + " ▶"}
	if model.loginItem != nil {
		box := "[ ]"
		if s.atLogin {
			box = "[x]"
		}
		row := box + " Start the backend at login"
		if s.checking {
			row += dim.Render("  …")
		}
		rows = append(rows, row)
	}
	for index := range rows {
		if index == s.cursor {
			rows[index] = selected.Render(rows[index])
		}
	}
	lines := append([]string{bold.Render(s.title()), ""}, rows...)
	if s.problem != "" {
		lines = append(lines, "", lipgloss.NewStyle().Foreground(red).Width(model.modalInner()).
			Render(s.problem))
	}
	if model.loginItem != nil {
		lines = append(lines, "", lipgloss.NewStyle().Faint(true).Width(model.modalInner()).Render(
			"On: the backend starts at login and runs all the time. Off: it runs while "+
				"this dashboard or the app is open, and stops two minutes after the last one quits."))
	}
	hint := "↑/↓: choose   ←/→ or space: change   esc: close"
	return strings.Join(append(lines, "", dim.Render(hint)), "\n")
}
