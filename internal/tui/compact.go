// The narrow layout, for a phone or a small window: repos as a row of tabs, the
// PR list under them, and the details under that.

package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/acheris-labs/pr-mon/internal/models"
)

func (m *Model) compact() bool { return m.width < compactBelow }

func (m *Model) compactView() string {
	prs, details := m.compactHeights()
	lines := []string{
		m.headerView(),
		m.tabsView(),
		m.mark(zonePane+"prs",
			framePane(m.prTitle(), m.width, prs, m.focus == panePRs, m.prTable(m.width-2, prs-2))),
		m.detailsPane(m.width, details),
	}
	return strings.Join(append(lines, m.compactFooter(m.mark)...), "\n")
}

// compactHeights splits what the header, tabs and key hints leave: the PR list
// takes what it needs, up to half, and the details get the rest.
func (m *Model) compactHeights() (prs, details int) {
	unmarked := func(_, text string) string { return text }
	room := max(6, m.height-2-len(m.compactFooter(unmarked)))
	prs = min(room/2, max(1, len(m.prs()))+2)
	return prs, room - prs
}

// tabsView is one line of repo tabs, scrolled sideways so the selected one
// shows, with ‹ and › where more are out of sight.
func (m *Model) tabsView() string {
	if len(m.rows) == 0 {
		return clip(dim.Render(" Press A to add a repository"), m.width)
	}
	tabs := []string{}
	for index, item := range m.rows {
		tabs = append(tabs, m.mark(fmt.Sprintf("%s%d", zoneRepo, index), m.tab(item, index == m.cursor)))
	}
	room := m.width - 2 // a column each side for ‹ and ›
	fits := func(from, to int) bool {
		return lipgloss.Width(strings.Join(tabs[from:to], "")) <= room
	}
	cursor := min(m.cursor, len(tabs)-1)
	start := 0
	for start < cursor && !fits(start, cursor+1) {
		start++
	}
	end := cursor + 1
	for end < len(tabs) && fits(start, end+1) {
		end++
	}
	left, right := " ", " "
	if start > 0 {
		left = dim.Render("‹")
	}
	if end < len(tabs) {
		right = dim.Render("›")
	}
	return left + clip(strings.Join(tabs[start:end], ""), room) + right
}

// tab is a repo's short name with its unseen count, coloured like the tree's
// badges.
func (m *Model) tab(item row, current bool) string {
	unseen, alert := m.repoCounts(item.repo)
	label := models.ShortName(item.repo)
	switch {
	case unseen > 0:
		label = fmt.Sprintf("● %s (%d)", label, unseen)
	case alert:
		label = "⚠ " + label
	}
	label = " " + label + " "
	_, loaded := m.backend.Repo(item.repo)
	switch {
	case current && m.focus == paneRepos:
		return selected.Render(label)
	case current:
		return keyStyle.Underline(true).Render(label)
	case unseen > 0 || alert:
		return m.badgeStyle(alert).Render(label)
	case !loaded:
		return dim.Render(label)
	}
	return label
}

// compactFooter is the key hints, wrapped to the screen; a short screen goes
// without them. mark makes each a place to tap.
func (m *Model) compactFooter(mark func(name, text string) string) []string {
	if m.height < hintsFrom {
		return nil
	}
	hints := []struct{ key, label string }{
		{"A", "Add"}, {"D", "Remove"}, {"N", "Notify"}, {"S", "Settings"},
		{"r", "Refresh"}, {"q", "Quit"}, {"w", "Deps"}, {"⏎", "Actions"},
	}
	items := []string{}
	for _, hint := range hints {
		items = append(items, mark(zoneKey+hint.key, keyStyle.Render(hint.key)+" "+hint.label))
	}
	return wrapItems(items, "  ", m.width)
}
