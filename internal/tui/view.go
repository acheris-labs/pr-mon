// Rendering: the header, the repo tree, the PR table, the details pane, the
// footer and any open dialog.

package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/acheris-labs/pr-mon/internal/models"
)

const treeWidth = 34

var (
	green  = lipgloss.Color("2")
	red    = lipgloss.Color("1")
	yellow = lipgloss.Color("3")
	cyan   = lipgloss.Color("6")
	blue   = lipgloss.Color("4")

	dim      = lipgloss.NewStyle().Faint(true)
	bold     = lipgloss.NewStyle().Bold(true)
	titleBar = lipgloss.NewStyle().Bold(true)
	keyStyle = lipgloss.NewStyle().Bold(true).Foreground(blue)
	selected = lipgloss.NewStyle().Reverse(true)
)

// statusStyle is the icon and colour for each status, as the Python views used.
func statusStyle(status models.Status) (string, lipgloss.Style) {
	switch status {
	case models.StatusReady:
		return "✓", lipgloss.NewStyle().Bold(true).Foreground(green)
	case models.StatusConflict, models.StatusFailing:
		return "✗", lipgloss.NewStyle().Bold(true).Foreground(red)
	case models.StatusBlocked:
		return "⚠", lipgloss.NewStyle().Bold(true).Foreground(red)
	case models.StatusBehind:
		return "↓", lipgloss.NewStyle().Foreground(yellow)
	case models.StatusPending:
		return "⋯", lipgloss.NewStyle().Foreground(yellow)
	case models.StatusDraft:
		return "◌", dim
	}
	return "?", dim
}

func reasonStyle(level string) (string, lipgloss.Style) {
	switch level {
	case "error":
		return "✗", lipgloss.NewStyle().Foreground(red)
	case "warning":
		return "⚠", lipgloss.NewStyle().Foreground(yellow)
	}
	return "•", dim
}

func (m *Model) View() string {
	if m.quitting {
		return ""
	}
	body := lipgloss.JoinHorizontal(lipgloss.Top, m.treeView(), m.rightView())
	screen := lipgloss.JoinVertical(lipgloss.Left, m.headerView(), body, m.footerView())
	if m.modal != nil {
		screen = m.overlay(screen, m.modalView())
	}
	if len(m.toasts) > 0 {
		screen = m.overlayToasts(screen)
	}
	return screen
}

func (m *Model) headerView() string {
	dot := lipgloss.NewStyle().Bold(true).Foreground(green).Render("●")
	if !m.connected {
		dot = lipgloss.NewStyle().Bold(true).Foreground(red).Render("○")
	}
	line := m.statusLine()
	rest := strings.TrimPrefix(strings.TrimPrefix(line, "●"), "○")
	text := titleBar.Render("pr-mon") + dim.Render(" — ") + dot + dim.Render(rest)
	return lipgloss.NewStyle().Width(m.width).Align(lipgloss.Center).Render(text)
}

// paneHeight splits the window between the header, panes and footer.
func (m *Model) paneHeight() int {
	return max(6, m.height-2)
}

// framePane draws a rounded box with its title in the top border, the way the
// Textual dashboard looked. Lines are padded and clipped to fit.
func framePane(title string, width, height int, focused bool, body string) string {
	inner := max(1, width-2)
	borderStyle := dim
	if focused {
		borderStyle = lipgloss.NewStyle().Foreground(blue)
	}
	heading := bold.Render(" " + title + " ")
	if focused {
		heading = keyStyle.Render(" " + title + " ")
	}
	headingWidth := min(lipgloss.Width(heading), inner-2)
	if headingWidth < lipgloss.Width(heading) {
		heading = bold.Render(" " + truncate(title, max(1, inner-4)) + " ")
		headingWidth = lipgloss.Width(heading)
	}
	top := borderStyle.Render("╭─") + heading +
		borderStyle.Render(strings.Repeat("─", max(0, inner-headingWidth-1))+"╮")
	bottom := borderStyle.Render("╰" + strings.Repeat("─", inner) + "╯")
	side := borderStyle.Render("│")

	lines := strings.Split(body, "\n")
	rows := []string{top}
	for index := 0; index < height-2; index++ {
		line := ""
		if index < len(lines) {
			line = clip(lines[index], inner)
		}
		rows = append(rows, side+padTo(line, inner)+side)
	}
	return strings.Join(append(rows, bottom), "\n")
}

// clip shortens a rendered line to width, keeping its styling intact.
func clip(line string, width int) string {
	if lipgloss.Width(line) <= width {
		return line
	}
	return lipgloss.NewStyle().MaxWidth(width).Render(line)
}

func (m *Model) treeView() string {
	height := m.paneHeight()
	lines := []string{}
	for index, item := range m.rows {
		line := m.treeRow(item)
		if index == m.cursor && m.focus == paneRepos {
			line = selected.Render(padTo(line, treeWidth-4))
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		lines = append(lines, dim.Render("Press A to add a repository"))
	}
	return framePane("Repos", treeWidth, height, m.focus == paneRepos, strings.Join(lines, "\n"))
}

func (m *Model) treeRow(item row) string {
	if item.repo == "" {
		marker := "▼ "
		if m.isCollapsed(item.owner) {
			marker = "▶ "
		}
		unseen, alerts := m.groupCounts(item.owner)
		return dim.Render(marker) + m.badge(unseen, alerts) +
			bold.Render(item.ownerName+"/") + m.count(unseen, alerts)
	}
	branch := "├── "
	if item.last {
		branch = "└── "
	}
	unseen, alerts := m.repoCounts(item.repo)
	name := models.ShortName(item.repo)
	style := lipgloss.NewStyle()
	if _, loaded := m.backend.Repo(item.repo); !loaded && m.backend.Errors()[item.repo] == "" {
		style = dim
	}
	return dim.Render(branch) + m.badge(unseen, alerts) + style.Render(name) + m.count(unseen, alerts)
}

// repoCounts is how many PRs are unseen, and whether any of them is an alert.
func (m *Model) repoCounts(name string) (int, bool) {
	if m.backend.Errors()[name] != "" {
		return 0, true
	}
	repo, found := m.backend.Repo(name)
	if !found {
		return 0, false
	}
	unseen := m.unseen(name)
	count, alert, ready := 0, false, false
	for _, pr := range repo.PRs {
		if !unseen[pr.Number] {
			continue
		}
		count++
		if pr.Status.IsAlert() {
			alert = true
		}
		if pr.Status == models.StatusReady {
			ready = true
		}
	}
	if ready {
		return count, false
	}
	return count, alert
}

func (m *Model) groupCounts(owner string) (int, bool) {
	total, alert := 0, false
	for _, name := range m.backend.Config().Repos {
		if models.OwnerKey(name) != owner {
			continue
		}
		count, groupAlert := m.repoCounts(name)
		total += count
		alert = alert || groupAlert
	}
	return total, alert
}

func (m *Model) badge(unseen int, alert bool) string {
	switch {
	case alert && unseen == 0:
		return lipgloss.NewStyle().Bold(true).Foreground(red).Render("⚠ ")
	case unseen > 0:
		return m.badgeStyle(alert).Render("● ")
	}
	return "  "
}

func (m *Model) count(unseen int, alert bool) string {
	if unseen == 0 {
		return ""
	}
	return m.badgeStyle(alert).Render(fmt.Sprintf(" (%d)", unseen))
}

func (m *Model) badgeStyle(alert bool) lipgloss.Style {
	if alert {
		return lipgloss.NewStyle().Bold(true).Foreground(red)
	}
	return lipgloss.NewStyle().Bold(true).Foreground(green)
}

func (m *Model) rightView() string {
	width := max(20, m.width-treeWidth)
	height := m.paneHeight()
	top := height/2 + height%2
	return lipgloss.JoinVertical(lipgloss.Left,
		framePane(m.prTitle(), width, top, m.focus == panePRs, m.prTable(width-6, top-2)),
		framePane("Details", width, height-top, false, m.detailsView(width-6)),
	)
}

func (m *Model) prTitle() string {
	name := m.selectedRepo()
	repo, found := m.backend.Repo(name)
	if !found {
		return "PRs"
	}
	if repo.PRTotal > len(repo.PRs) {
		return fmt.Sprintf("PRs — %s (newest %d of %d)", repo.Name, len(repo.PRs), repo.PRTotal)
	}
	return fmt.Sprintf("PRs — %s (%d)", repo.Name, repo.PRTotal)
}

func (m *Model) prTable(width, height int) string {
	prs := m.prs()
	name := m.selectedRepo()
	if len(prs) == 0 {
		if name == "" {
			return dim.Render("Select a repository")
		}
		if message := m.backend.Errors()[name]; message != "" {
			return lipgloss.NewStyle().Bold(true).Foreground(red).Render("⚠ " + message)
		}
		if _, found := m.backend.Repo(name); !found {
			return dim.Render("Loading…")
		}
		return dim.Render("No open pull requests")
	}
	unseen := m.unseen(name)
	armed := m.backend.Armed(name)
	const (
		numberWidth = 8
		statusWidth = 14
		authorWidth = 14
	)
	titleWidth := max(10, width-numberWidth-statusWidth-authorWidth-2)
	header := dim.Render("  " + padTo("#", numberWidth) + padTo("Status", statusWidth) +
		padTo("Author", authorWidth) + "Title")
	lines := []string{header}
	start := 0
	if m.prCursor >= height-1 {
		start = m.prCursor - height + 2
	}
	for index := start; index < len(prs) && len(lines) < height; index++ {
		pr := prs[index]
		icon, style := statusStyle(pr.Status)
		marker := ""
		switch {
		case pr.AutoMerge != nil:
			marker = " auto"
		case armed[pr.Number].Method != "":
			marker = " auto*"
		}
		text := lipgloss.NewStyle()
		if unseen[pr.Number] {
			text = bold
		}
		// Each cell is padded after styling, so colour codes don't eat the width.
		cells := style.Render(icon) + " " +
			padTo(text.Render(fmt.Sprintf("#%d", pr.Number)), numberWidth) +
			padTo(style.Render(string(pr.Status))+lipgloss.NewStyle().Foreground(cyan).Render(marker),
				statusWidth) +
			padTo(dim.Render(truncate(pr.Author, authorWidth-1)), authorWidth) +
			text.Render(truncate(pr.Title, titleWidth))
		if index == m.prCursor && m.focus == panePRs {
			cells = selected.Render(padTo(cells, width))
		}
		lines = append(lines, cells)
	}
	return strings.Join(lines, "\n")
}

func (m *Model) detailsView(width int) string {
	name := m.selectedRepo()
	if name == "" {
		if len(m.backend.Config().Repos) == 0 {
			return dim.Render("Press A to add a repository")
		}
		return dim.Render("Select a repository")
	}
	lines := []string{}
	if message := m.backend.Errors()[name]; message != "" {
		lines = append(lines, lipgloss.NewStyle().Bold(true).Foreground(red).Render("⚠ "+message), "")
	}
	pr, ok := m.selectedPR()
	if !ok {
		if _, found := m.backend.Repo(name); !found {
			return strings.Join(append(lines, dim.Render("Loading…")), "\n")
		}
		return strings.Join(append(lines, dim.Render("No open pull requests")), "\n")
	}
	icon, style := statusStyle(pr.Status)
	lines = append(lines,
		bold.Render(fmt.Sprintf("#%d %s", pr.Number, pr.Title)),
		dim.Render("Branch: ")+bold.Render(pr.HeadRef+" → "+pr.BaseRef),
		dim.Render("Author: ")+pr.Author,
		dim.Render("Opened:      ")+age(pr.CreatedAt, m.now),
	)
	if pr.LastCommitAt != nil {
		lines = append(lines, dim.Render("Last commit: ")+age(*pr.LastCommitAt, m.now))
	}
	if pr.LastCheckStartedAt != nil {
		lines = append(lines, dim.Render("Last check:  ")+age(*pr.LastCheckStartedAt, m.now))
	}
	if pr.HeadSha != nil {
		lines = append(lines, dim.Render("Head SHA:    ")+*pr.HeadSha)
	}
	lines = append(lines, lipgloss.NewStyle().Underline(true).Foreground(blue).Render(pr.URL), "")
	lines = append(lines, style.Render(icon+" "+string(pr.Status)))
	armed := m.backend.Armed(name)[pr.Number]
	switch {
	case pr.AutoMerge != nil:
		lines = append(lines, lipgloss.NewStyle().Foreground(cyan).Render(fmt.Sprintf(
			"Auto-merge: on (%s, by %s)", pr.AutoMerge.Method.Lower(), pr.AutoMerge.EnabledBy)))
	case armed.Method != "":
		deletes := ""
		if armed.DeleteBranch {
			deletes = ", delete branch"
		}
		lines = append(lines, lipgloss.NewStyle().Foreground(cyan).Render(fmt.Sprintf(
			"Auto-merge: pr-mon (%s%s), armed %s",
			armed.Method.Lower(), deletes, age(armed.ArmedAt, m.now))))
	}
	if len(pr.Reasons) > 0 {
		heading := "\nBlocked by:"
		if pr.Status == models.StatusReady {
			heading = "\nNotes:"
		}
		lines = append(lines, heading)
		for _, reason := range pr.Reasons {
			icon, style := reasonStyle(reason.Level)
			lines = append(lines, style.Render("  "+icon+" "+reason.Text))
		}
	} else if pr.Status == models.StatusReady {
		lines = append(lines, lipgloss.NewStyle().Foreground(green).Render("\nReady to merge"))
	}
	return strings.Join(lines, "\n")
}

func (m *Model) footerView() string {
	keys := []struct{ key, label string }{
		{"A", "Add repo"}, {"D", "Remove repo"}, {"N", "Notifications"},
		{"r", "Refresh"}, {"q", "Quit"},
	}
	parts := []string{}
	for _, item := range keys {
		parts = append(parts, keyStyle.Render(" "+item.key+" ")+" "+item.label)
	}
	left := strings.Join(parts, "  ")
	right := keyStyle.Render(" ⏎ ") + " Actions"
	gap := max(1, m.width-lipgloss.Width(left)-lipgloss.Width(right))
	return left + strings.Repeat(" ", gap) + right
}

// overlay centres a dialog on the screen.
func (m *Model) overlay(screen, dialog string) string {
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, dialog,
		lipgloss.WithWhitespaceChars(" "))
}

func (m *Model) overlayToasts(screen string) string {
	lines := []string{}
	for _, item := range m.toasts {
		style := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(0, 1)
		switch item.severity {
		case "error":
			style = style.BorderForeground(red)
		case "warning":
			style = style.BorderForeground(yellow)
		}
		lines = append(lines, style.Render(item.message))
	}
	stack := lipgloss.JoinVertical(lipgloss.Right, lines...)
	return lipgloss.Place(m.width, m.height, lipgloss.Right, lipgloss.Bottom, stack,
		lipgloss.WithWhitespaceChars(" "))
}

func padTo(text string, width int) string {
	if gap := width - lipgloss.Width(text); gap > 0 {
		return text + strings.Repeat(" ", gap)
	}
	return text
}

func truncate(text string, width int) string {
	runes := []rune(text)
	if len(runes) <= width {
		return text
	}
	if width <= 1 {
		return string(runes[:max(0, width)])
	}
	return string(runes[:width-1]) + "…"
}
