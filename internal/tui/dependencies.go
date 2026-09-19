// The dependencies dialog: what a PR waits on, adding and removing those, and
// the whole graph around it.

package tui

import (
	"fmt"
	"sort"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/acheris-labs/pr-mon/internal/models"
)

type dependencyMode int

const (
	modeList dependencyMode = iota
	modePick
	modeGraph
)

// pickerRows is how many matching PRs the picker shows at once.
const pickerRows = 8

type dependencyModal struct {
	repo   string
	number int
	mode   dependencyMode
	cursor int
	// picker
	input      textinput.Model
	pickCursor int
	message    string
	busy       bool
	// graph
	graph    *models.DependencyGraph
	graphErr string
}

// dependencyDone reports an add or remove; graphReady carries a fetched graph.
type dependencyDone struct {
	adding bool
	err    error
}

type graphReady struct {
	graph models.DependencyGraph
	err   error
}

func newDependencies(repo string, number int) *dependencyModal {
	input := textinput.New()
	input.Placeholder = "search, owner/repo#12, or a PR URL"
	input.Prompt = "> "
	input.Width = 60
	return &dependencyModal{repo: repo, number: number, input: input}
}

func (d *dependencyModal) title() string {
	return fmt.Sprintf("Dependencies — %s#%d", d.repo, d.number)
}

// pr is the PR as the backend last described it, so the dialog follows events.
func (d *dependencyModal) pr(model *Model) (models.PullRequest, bool) {
	repo, found := model.backend.Repo(d.repo)
	if !found {
		return models.PullRequest{}, false
	}
	for _, pr := range repo.PRs {
		if pr.Number == d.number {
			return pr, true
		}
	}
	return models.PullRequest{}, false
}

// candidates are open PRs in monitored repos this one could wait on that
// match every word of the search, in repo order.
func (d *dependencyModal) candidates(model *Model) []models.PRRef {
	current, _ := d.pr(model)
	taken := map[string]bool{models.PRKey(d.repo, d.number): true}
	for _, ref := range current.WaitsOn {
		taken[ref.Key()] = true
	}
	words := strings.Fields(strings.ToLower(d.input.Value()))
	found := []models.PRRef{}
	names := make([]string, 0, len(model.backend.Repos()))
	for name := range model.backend.Repos() {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		repo, _ := model.backend.Repo(name)
		for _, pr := range repo.PRs {
			key := models.PRKey(name, pr.Number)
			if taken[key] || !matchesAll(strings.ToLower(key+" "+pr.Title+" "+pr.Author), words) {
				continue
			}
			found = append(found, models.PRRef{Repo: name, Number: pr.Number, Title: pr.Title,
				URL: pr.URL, State: models.PROpen, Status: models.Ptr(pr.Status)})
		}
	}
	return found
}

func matchesAll(text string, words []string) bool {
	for _, word := range words {
		if !strings.Contains(text, word) {
			return false
		}
	}
	return true
}

func (d *dependencyModal) update(model *Model, key tea.KeyMsg) (modal, tea.Cmd) {
	switch d.mode {
	case modePick:
		return d.updatePick(model, key)
	case modeGraph:
		switch key.String() {
		case "esc", "escape", "g", "q":
			d.mode = modeList
		}
		return d, nil
	}
	current, _ := d.pr(model)
	switch key.String() {
	case "esc", "escape", "q":
		return nil, nil
	case "up", "k":
		d.cursor = max(0, d.cursor-1)
	case "down", "j":
		d.cursor = min(max(0, len(current.WaitsOn)-1), d.cursor+1)
	case "a", "w":
		d.mode = modePick
		d.input.SetValue("")
		d.input.Focus()
		d.pickCursor = 0
		d.message = ""
		return d, textinput.Blink
	case "d", "x", "delete", "backspace":
		if d.cursor < len(current.WaitsOn) {
			on := current.WaitsOn[d.cursor].Key()
			repo, number := d.repo, d.number
			return d, func() tea.Msg {
				return dependencyDone{err: model.backend.RemoveDependency(repo, number, on)}
			}
		}
	case "g":
		d.mode = modeGraph
		d.graph = nil
		d.graphErr = ""
		repo, number := d.repo, d.number
		return d, func() tea.Msg {
			graph, err := model.backend.DependencyGraph(repo, number)
			return graphReady{graph: graph, err: err}
		}
	}
	return d, nil
}

func (d *dependencyModal) updatePick(model *Model, key tea.KeyMsg) (modal, tea.Cmd) {
	if d.busy {
		return d, nil
	}
	candidates := d.candidates(model)
	switch key.String() {
	case "esc", "escape":
		d.mode = modeList
		d.input.Blur()
		return d, nil
	case "up":
		d.pickCursor = max(0, d.pickCursor-1)
		return d, nil
	case "down":
		d.pickCursor = min(max(0, min(len(candidates), pickerRows)-1), d.pickCursor+1)
		return d, nil
	case "enter":
		on := strings.TrimSpace(d.input.Value())
		if len(candidates) > 0 {
			on = candidates[min(d.pickCursor, len(candidates)-1)].Key()
		}
		if on == "" {
			return d, nil
		}
		d.busy = true
		d.message = "Checking " + on + "…"
		repo, number := d.repo, d.number
		return d, func() tea.Msg {
			return dependencyDone{adding: true, err: model.backend.AddDependency(repo, number, on)}
		}
	}
	var cmd tea.Cmd
	d.input, cmd = d.input.Update(key)
	d.pickCursor = 0
	return d, cmd
}

// finished handles the backend's answer to an add or remove.
func (d *dependencyModal) finished(model *Model, done dependencyDone) {
	d.busy = false
	if done.err != nil {
		if done.adding {
			d.message = done.err.Error()
		} else {
			model.addToast(done.err.Error(), "error")
		}
		return
	}
	d.message = ""
	if done.adding {
		d.mode = modeList
		d.input.Blur()
		if current, ok := d.pr(model); ok {
			d.cursor = max(0, len(current.WaitsOn)-1)
		}
	}
}

// ----- drawing -----

func (d *dependencyModal) view(model *Model) string {
	switch d.mode {
	case modePick:
		return d.pickView(model)
	case modeGraph:
		return d.graphView(model.modalInner())
	}
	lines := []string{bold.Render(d.title()), ""}
	current, ok := d.pr(model)
	switch {
	case !ok:
		lines = append(lines, dim.Render("This PR is no longer open"))
	case len(current.WaitsOn) == 0:
		lines = append(lines, dim.Render("Doesn't wait on any other PR"))
	default:
		lines = append(lines, "Waits on:")
		for index, ref := range current.WaitsOn {
			line := "  " + refLine(ref, d.repo, model.modalInner()-2)
			if index == d.cursor {
				line = selected.Render(line)
			}
			lines = append(lines, line)
		}
	}
	if ok && len(current.RequiredBy) > 0 {
		lines = append(lines, "", "Required by:")
		for _, ref := range current.RequiredBy {
			lines = append(lines, "  "+refLine(ref, d.repo, model.modalInner()-2))
		}
	}
	hint := "a: add   d: remove   g: graph   esc: close"
	return strings.Join(append(lines, "", dim.Render(hint)), "\n")
}

func (d *dependencyModal) pickView(model *Model) string {
	lines := []string{bold.Render(fmt.Sprintf("%s#%d waits on…", d.repo, d.number)), d.input.View()}
	candidates := d.candidates(model)
	for index, ref := range candidates[:min(len(candidates), pickerRows)] {
		line := "  " + refLine(ref, d.repo, model.modalInner()-2)
		if index == d.pickCursor {
			line = selected.Render(line)
		}
		lines = append(lines, line)
	}
	if extra := len(candidates) - pickerRows; extra > 0 {
		lines = append(lines, dim.Render(fmt.Sprintf("  …and %d more; type to narrow", extra)))
	}
	if len(candidates) == 0 {
		lines = append(lines, dim.Render("  No monitored PR matches; enter uses what you typed"))
	}
	if d.message != "" {
		style := lipgloss.NewStyle().Foreground(red)
		if d.busy {
			style = dim
		}
		lines = append(lines, style.Render(d.message))
	}
	return strings.Join(append(lines, "", dim.Render("↑/↓: choose   enter: add   esc: back")), "\n")
}

func (d *dependencyModal) graphView(width int) string {
	lines := []string{bold.Render("Dependency graph"), ""}
	switch {
	case d.graphErr != "":
		lines = append(lines, lipgloss.NewStyle().Foreground(red).Render(d.graphErr))
	case d.graph == nil:
		lines = append(lines, dim.Render("Loading…"))
	default:
		lines = append(lines, graphLines(*d.graph, width)...)
	}
	return strings.Join(append(lines, "", dim.Render("esc: back")), "\n")
}

// graphLines draws a graph as two trees around the PR: what it waits on above,
// what waits on it below.
func graphLines(graph models.DependencyGraph, width int) []string {
	repo := graph.PR.Repo
	lines := []string{"Waits on:"}
	if len(graph.WaitsOn) == 0 {
		lines = append(lines, dim.Render("  nothing"))
	}
	lines = append(lines, treeLines(graph.WaitsOn, "  ", repo, width)...)
	lines = append(lines, "", bold.Render("▶ "+refLine(graph.PR, repo, width-2)), "", "Required by:")
	if len(graph.RequiredBy) == 0 {
		lines = append(lines, dim.Render("  nothing"))
	}
	return append(lines, treeLines(graph.RequiredBy, "  ", repo, width)...)
}

func treeLines(nodes []models.DependencyNode, prefix, repo string, width int) []string {
	lines := []string{}
	for index, node := range nodes {
		branch, next := "├─ ", "│  "
		if index == len(nodes)-1 {
			branch, next = "└─ ", "   "
		}
		room := width - lipgloss.Width(prefix+branch)
		line := dim.Render(prefix+branch) + refLine(node.PR, repo, room)
		if node.Repeated {
			line = dim.Render(prefix+branch) + refLine(node.PR, repo, room-len(" (shown above)")) +
				dim.Render(" (shown above)")
		}
		lines = append(lines, line)
		lines = append(lines, treeLines(node.Children, prefix+next, repo, width)...)
	}
	return lines
}

// refLine is one PR a dependency names: an icon for where it stands, its
// number (qualified when it's in another repo) and title, which is shortened to
// fit `width` (0: no limit) so the state stays on the same line.
func refLine(ref models.PRRef, repo string, width int) string {
	label := fmt.Sprintf("#%d", ref.Number)
	if ref.Repo != repo {
		label = ref.Key()
	}
	var icon, state string
	var style lipgloss.Style
	switch {
	case ref.State == models.PRMerged:
		icon, state, style = "✓", "merged", lipgloss.NewStyle().Foreground(green)
	case ref.State == models.PRClosed:
		icon, state, style = "✗", "closed without merging", lipgloss.NewStyle().Foreground(red)
	case ref.Status != nil:
		icon, style = statusStyle(*ref.Status)
		state = string(*ref.Status)
	case ref.State == models.PROpen:
		icon, state, style = "○", "open", dim
	default:
		icon, state, style = "?", "not looked up yet", dim
	}
	title := ref.Title
	if width > 0 {
		room := width - lipgloss.Width(icon+" "+label+"  "+state)
		title = truncate(title, max(0, room))
	}
	text := style.Render(icon) + " " + label
	if title != "" {
		text += " " + title
	}
	return text + " " + style.Render(state)
}
