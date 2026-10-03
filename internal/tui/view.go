// Rendering: the header, the repo tabs, the PR table, the details pane, the
// key hints and any open dialog. One layout for every screen, a phone's
// included: only the PR table's columns and a dialog's padding give way when
// it is narrow.

package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/acheris-labs/pr-mon/internal/models"
)

const (
	// Below this many columns the PR table keeps only the icon, number and
	// title, and dialogs lose their padding.
	narrowBelow = 80
	// Below this many lines the key hints give their room to the panes.
	hintsFrom = 16
	// The least the panes can be drawn in: under this the dashboard only says
	// so. The keys still work.
	minWidth, minHeight = 30, 10
)

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

// statusStyle is the icon and colour for each status.
// issueLabel is "#12", or "other/repo#12" when the issue is somewhere else.
func issueLabel(issue models.LinkedIssue, repo string) string {
	if issue.Repo != "" && issue.Repo != repo {
		return fmt.Sprintf("%s#%d", issue.Repo, issue.Number)
	}
	return fmt.Sprintf("#%d", issue.Number)
}

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
	case models.StatusWaiting:
		return "⧗", lipgloss.NewStyle().Foreground(cyan)
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
	m.marked = m.marked[:0]
	if m.width < minWidth || m.height < minHeight {
		return m.tooSmall()
	}
	prs, details := m.paneHeights()
	lines := []string{
		m.headerView(),
		m.tabsView(),
		m.mark(zonePane+"prs",
			framePane(m.prTitle(), m.width, prs, m.focus == panePRs, m.prTable(m.width-2, prs-2))),
		m.detailsPane(m.width, details),
	}
	screen := strings.Join(append(lines, m.footerView(m.mark)...), "\n")
	if m.modal != nil {
		m.marked = m.marked[:0] // a dialog covers everything else
		screen = m.overlay(screen, m.modalView())
	}
	if len(m.toasts) > 0 {
		screen = m.overlayToasts(screen)
	}
	return m.zones.Scan(screen)
}

func (m *Model) headerView() string {
	dot := lipgloss.NewStyle().Bold(true).Foreground(green).Render("●")
	if !m.connected {
		dot = lipgloss.NewStyle().Bold(true).Foreground(red).Render("○")
	}
	line := m.statusLine()
	rest := strings.TrimPrefix(strings.TrimPrefix(line, "●"), "○")
	text := clip(titleBar.Render("pr-mon")+dim.Render(" — ")+dot+dim.Render(rest), m.width)
	return lipgloss.NewStyle().Width(m.width).Align(lipgloss.Center).Render(text)
}

func (m *Model) narrow() bool { return m.width < narrowBelow }

// tooSmall stands in for the dashboard on a screen it doesn't fit.
func (m *Model) tooSmall() string {
	text := fmt.Sprintf("pr-mon needs at least %d×%d; this is %d×%d",
		minWidth, minHeight, m.width, m.height)
	message := lipgloss.NewStyle().Width(m.width).MaxHeight(m.height).Align(lipgloss.Center).Render(text)
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, message)
}

// paneHeights splits what the header, tabs and key hints leave: the PR list
// takes what it needs, up to half, and the details get the rest.
func (m *Model) paneHeights() (prs, details int) {
	unmarked := func(_, text string) string { return text }
	room := max(6, m.height-2-len(m.footerView(unmarked)))
	rows := max(1, len(m.prs()))
	if !m.narrow() {
		rows++ // the column headings
	}
	prs = min(room/2, rows+2)
	return prs, room - prs
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

// tabsView is one line of repo tabs, scrolled sideways so the selected one
// shows, with ‹ and › where more are out of sight.
func (m *Model) tabsView() string {
	if len(m.repos) == 0 {
		return clip(dim.Render(" Press A to add a repository"), m.width)
	}
	tabs := []string{}
	for index, repo := range m.repos {
		tabs = append(tabs, m.mark(fmt.Sprintf("%s%d", zoneRepo, index), m.tab(repo, index == m.cursor)))
	}
	room := m.width - 2 // a column each side for ‹ and ›
	fits := func(from, to int) bool {
		return lipgloss.Width(strings.Join(tabs[from:to], "")) <= room
	}
	start := 0
	for start < m.cursor && !fits(start, m.cursor+1) {
		start++
	}
	end := m.cursor + 1
	for end < len(tabs) && fits(start, end+1) {
		end++
	}
	left, right := " ", " "
	if start > 0 {
		left = m.more("‹", "«", m.repos[:start])
	}
	if end < len(tabs) {
		right = m.more("›", "»", m.repos[end:])
	}
	return left + clip(strings.Join(tabs[start:end], ""), room) + right
}

// more marks tabs out of sight: faint while they are quiet, doubled and in
// their badge's colour when one has something unseen or a failed refresh.
func (m *Model) more(quiet, news string, hidden []string) string {
	flagged, alert := false, false
	for _, repo := range hidden {
		unseen, repoAlert := m.repoCounts(repo)
		flagged = flagged || unseen > 0 || repoAlert
		alert = alert || repoAlert
	}
	if !flagged {
		return dim.Render(quiet)
	}
	return m.badgeStyle(alert).Render(news)
}

// tabName is a repo's short name, or its full name when another repo shares
// the short one.
func (m *Model) tabName(repo string) string {
	short := models.ShortName(repo)
	for _, other := range m.repos {
		if other != repo && strings.EqualFold(models.ShortName(other), short) {
			return repo
		}
	}
	return short
}

// tab is a repo's name with its unseen count: green for something ready, red
// for something blocked or a failed refresh.
func (m *Model) tab(repo string, current bool) string {
	unseen, alert := m.repoCounts(repo)
	label := m.tabName(repo)
	switch {
	case unseen > 0:
		label = fmt.Sprintf("● %s (%d)", label, unseen)
	case alert:
		label = "⚠ " + label
	}
	label = " " + label + " "
	_, loaded := m.backend.Repo(repo)
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

func (m *Model) badgeStyle(alert bool) lipgloss.Style {
	if alert {
		return lipgloss.NewStyle().Bold(true).Foreground(red)
	}
	return lipgloss.NewStyle().Bold(true).Foreground(green)
}

// detailsSize is the details pane's width and height, border included.
func (m *Model) detailsSize() (int, int) {
	_, details := m.paneHeights()
	return m.width, details
}

// detailsPane frames the details from where they're scrolled to, and says they
// scroll when they don't all fit.
func (m *Model) detailsPane(width, height int) string {
	lines, hidden := m.detailsShown(width, height)
	title := "Details"
	focused := m.focus == paneDetails
	switch {
	case hidden > 0 && focused:
		title = "Details (↑/↓ scroll)"
	case hidden > 0:
		title = "Details (J/K scroll)"
	}
	return m.mark(zonePane+"details",
		framePane(title, width, height, focused, strings.Join(lines, "\n")))
}

// detailsShown is the details from the line they're scrolled to, and how many
// lines don't fit the pane.
func (m *Model) detailsShown(width, height int) ([]string, int) {
	lines := m.detailLines(width - 2)
	hidden := max(0, len(lines)-(height-2))
	return lines[min(m.detailScroll, hidden):], hidden
}

// detailLines is the details text, wrapped to width.
func (m *Model) detailLines(width int) []string {
	return strings.Split(lipgloss.NewStyle().Width(width).Render(m.detailsText()), "\n")
}

// scrollDetails moves the details by some lines, stopping at either end.
func (m *Model) scrollDetails(by int) {
	_, hidden := m.detailsShown(m.detailsSize())
	m.detailScroll = max(0, min(m.detailScroll+by, hidden))
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

// prTable is the PR list. A narrow screen drops the heading, the author and the
// status in words, which leaves the title its room.
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
	lines := []string{}
	narrow := m.narrow()
	if !narrow {
		lines = append(lines, dim.Render("  "+padTo("#", numberWidth)+padTo("Status", statusWidth)+
			padTo("Author", authorWidth)+"Title"))
	}
	// On a narrow screen the numbers line up on the longest one.
	shortNumber := 0
	for _, pr := range prs {
		shortNumber = max(shortNumber, len(fmt.Sprintf("#%d ", pr.Number)))
	}
	start := max(0, m.prCursor-(height-len(lines))+1)
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
		auto := lipgloss.NewStyle().Foreground(cyan).Render(marker)
		number := text.Render(fmt.Sprintf("#%d", pr.Number))
		// Each cell is padded after styling, so colour codes don't eat the width.
		var cells string
		if narrow {
			if marker != "" {
				auto = lipgloss.NewStyle().Foreground(cyan).Render(marker[1:]) + " "
			}
			room := width - 2 - shortNumber - lipgloss.Width(auto)
			cells = style.Render(icon) + " " + padTo(number, shortNumber) + auto +
				text.Render(truncate(pr.Title, max(1, room)))
		} else {
			cells = style.Render(icon) + " " + padTo(number, numberWidth) +
				padTo(style.Render(string(pr.Status))+auto, statusWidth) +
				padTo(dim.Render(truncate(pr.Author, authorWidth-1)), authorWidth) +
				text.Render(truncate(pr.Title, titleWidth))
		}
		// The PR stays marked while the details have the focus: they are its.
		if index == m.prCursor && m.focus != paneRepos {
			cells = selected.Render(padTo(cells, width))
		}
		lines = append(lines, m.mark(fmt.Sprintf("%s%d", zonePR, index), padTo(cells, width)))
	}
	return strings.Join(lines, "\n")
}

func (m *Model) detailsText() string {
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
	for index, issue := range pr.ClosingIssues {
		label := "Closes:      "
		if index > 0 {
			label = "             "
		}
		lines = append(lines, dim.Render(label)+issueLabel(issue, name)+" "+issue.Title)
	}
	lines = append(lines, labelled("Waits on:    ", pr.WaitsOn, name)...)
	lines = append(lines, labelled("Required by: ", pr.RequiredBy, name)...)
	lines = append(lines, hyperlink(pr.URL, lipgloss.NewStyle().Underline(true).Foreground(blue).Render(pr.URL)), "")
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

// hyperlink makes text a link the terminal opens (OSC 8), so it opens where
// the viewer is, which over ssh is not where pr-mon runs.
func hyperlink(url, text string) string {
	return linkStart + url + "\x1b\\" + text + linkStart + "\x1b\\"
}

// linkStart opens a hyperlink to the URL that follows it; with none, it ends
// one.
const linkStart = "\x1b]8;;"

// labelled lists PRs under a label that shows on the first line only.
func labelled(label string, refs []models.PRRef, repo string) []string {
	lines := []string{}
	for index, ref := range refs {
		if index > 0 {
			label = strings.Repeat(" ", len(label))
		}
		lines = append(lines, dim.Render(label)+refLine(ref, repo, 0))
	}
	return lines
}

// footerView is the key hints, wrapped to the screen; a short screen goes
// without them. mark makes each a place to tap.
func (m *Model) footerView(mark func(name, text string) string) []string {
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

// overlay centres a dialog on the screen.
func (m *Model) overlay(screen, dialog string) string {
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, dialog,
		lipgloss.WithWhitespaceChars(" "))
}

// overlayToasts draws the toasts over the bottom right of the screen, the
// dashboard showing round them.
func (m *Model) overlayToasts(screen string) string {
	lines := []string{}
	for _, item := range m.toasts {
		// Long messages wrap inside the screen.
		style := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(0, 1).
			Width(min(lipgloss.Width(item.message)+2, m.width-2))
		switch item.severity {
		case "error":
			style = style.BorderForeground(red)
		case "warning":
			style = style.BorderForeground(yellow)
		}
		lines = append(lines, style.Render(item.message))
	}
	stack := strings.Split(lipgloss.JoinVertical(lipgloss.Right, lines...), "\n")
	rows := strings.Split(screen, "\n")
	for len(rows) < m.height {
		rows = append(rows, "")
	}
	// A stack taller than the screen loses its oldest toasts' tops.
	stack = stack[max(0, len(stack)-len(rows)):]
	top := len(rows) - len(stack)
	for index, toastLine := range stack {
		// Narrower toasts are padded on the left: the screen shows there.
		toastLine = strings.TrimLeft(toastLine, " ")
		keep := m.width - lipgloss.Width(toastLine)
		left := ""
		if keep > 0 {
			left = padTo(clip(rows[top+index], keep), keep)
		}
		// Whatever style or link the cut line left open ends before the toast.
		rows[top+index] = left + "\x1b[0m" + linkStart + "\x1b\\" + toastLine
	}
	return strings.Join(rows, "\n")
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
