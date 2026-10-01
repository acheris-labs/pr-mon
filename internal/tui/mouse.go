// Mouse: a tap selects a repo or a PR, opens the actions of the PR already
// selected, or presses the key of the hint under it; the wheel moves through the
// list under it, or scrolls the details. Every place to tap is a zone, marked as
// it is drawn and looked up when the mouse reports in.

package tui

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// What a zone's name starts with says what a tap on it does.
const (
	zoneKey  = "key:"  // presses the key named after it
	zoneRepo = "repo:" // selects that row of the tree, or that tab
	zonePR   = "pr:"   // selects that PR
	zonePane = "pane:" // where the wheel applies
)

// mark makes text a place to tap. A name used twice in a frame (d on a
// checkbox and again in the hints) gets a zone each.
func (m *Model) mark(name, text string) string {
	id := name
	for again := 1; slices.Contains(m.marked, id); again++ {
		id = fmt.Sprintf("%s#%d", name, again)
	}
	m.marked = append(m.marked, id)
	return m.zones.Mark(id, text)
}

// zoneAt is the name of the zone of one kind under the mouse, or "".
func (m *Model) zoneAt(mouse tea.MouseMsg, prefix string) string {
	for _, id := range m.marked {
		if strings.HasPrefix(id, prefix) && m.zones.Get(id).InBounds(mouse) {
			name, _, _ := strings.Cut(id, "#")
			return name
		}
	}
	return ""
}

// keyNamed is the key press a hint names.
func keyNamed(name string) tea.KeyMsg {
	switch name {
	case "enter", "⏎":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "space":
		return tea.KeyMsg{Type: tea.KeySpace}
	case "↑":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "↓":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "←":
		return tea.KeyMsg{Type: tea.KeyLeft}
	case "→":
		return tea.KeyMsg{Type: tea.KeyRight}
	case "ctrl+s":
		return tea.KeyMsg{Type: tea.KeyCtrlS}
	case "ctrl+t":
		return tea.KeyMsg{Type: tea.KeyCtrlT}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(name)}
}

func (m *Model) handleMouse(mouse tea.MouseMsg) tea.Cmd {
	if mouse.Action != tea.MouseActionPress {
		return nil
	}
	switch mouse.Button {
	case tea.MouseButtonLeft:
		return m.tap(mouse)
	case tea.MouseButtonWheelUp:
		return m.wheel(mouse, false)
	case tea.MouseButtonWheelDown:
		return m.wheel(mouse, true)
	}
	return nil
}

func (m *Model) tap(mouse tea.MouseMsg) tea.Cmd {
	if id := m.zoneAt(mouse, zoneKey); id != "" {
		_, cmd := m.handleKey(keyNamed(strings.TrimPrefix(id, zoneKey)))
		return cmd
	}
	if m.modal != nil {
		return nil
	}
	if id := m.zoneAt(mouse, zoneRepo); id != "" {
		index, _ := strconv.Atoi(strings.TrimPrefix(id, zoneRepo))
		m.focus = paneRepos
		m.moveRepo(index - m.cursor)
		if m.selectedRepo() == "" {
			return m.enterRow() // an owner row folds
		}
		return nil
	}
	if id := m.zoneAt(mouse, zonePR); id != "" {
		index, _ := strconv.Atoi(strings.TrimPrefix(id, zonePR))
		if m.focus == panePRs && index == m.prCursor {
			return m.openActions()
		}
		m.focus = panePRs
		m.prCursor = index
		return m.markSeen()
	}
	return nil
}

// wheel is an arrow key to a dialog, or to the list under the mouse; over the
// details it scrolls them.
func (m *Model) wheel(mouse tea.MouseMsg, down bool) tea.Cmd {
	key, by := keyNamed("↑"), -1
	if down {
		key, by = keyNamed("↓"), 1
	}
	if m.modal != nil {
		_, cmd := m.handleKey(key)
		return cmd
	}
	switch m.zoneAt(mouse, zonePane) {
	case zonePane + "details":
		m.scrollDetails(by)
	case zonePane + "prs":
		if len(m.prs()) > 0 {
			m.focus = panePRs
			_, cmd := m.handlePRKey(key)
			return cmd
		}
	case zonePane + "repos":
		m.focus = paneRepos
		_, cmd := m.handleTreeKey(key)
		return cmd
	}
	return nil
}

// hints draws a line of key hints, "y: yes   n/esc: no", each key a place to
// tap, and wraps it to width between hints.
func (m *Model) hints(text string, width int) string {
	items := []string{}
	for _, item := range strings.Split(text, "   ") {
		keys, label, _ := strings.Cut(item, ": ")
		names := strings.Split(keys, "/")
		for index, name := range names {
			shown := name
			if index == len(names)-1 {
				shown += ": " + label
			}
			names[index] = m.mark(zoneKey+name, dim.Render(shown))
		}
		items = append(items, strings.Join(names, dim.Render("/")))
	}
	return strings.Join(wrapItems(items, "   ", width), "\n")
}

// wrapItems joins items into lines no wider than width, breaking only between
// them.
func wrapItems(items []string, gap string, width int) []string {
	lines, current := []string{}, ""
	for _, item := range items {
		switch {
		case current == "":
			current = item
		case lipgloss.Width(current+gap+item) > width:
			lines = append(lines, current)
			current = item
		default:
			current += gap + item
		}
	}
	return append(lines, current)
}
