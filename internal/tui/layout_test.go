package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/acheris-labs/pr-mon/internal/models"
	"github.com/acheris-labs/pr-mon/internal/service"
	"github.com/acheris-labs/pr-mon/internal/testfixtures"
)

func resize(model *Model, width, height int) {
	model.Update(tea.WindowSizeMsg{Width: width, Height: height})
}

// assertFits fails when a view spills past the screen it was drawn for.
func assertFits(t *testing.T, view string, width, height int) {
	t.Helper()
	lines := strings.Split(view, "\n")
	if len(lines) > height {
		t.Errorf("%d lines on a %d-line screen:\n%s", len(lines), height, view)
	}
	for _, candidate := range lines {
		if got := lipgloss.Width(candidate); got > width {
			t.Errorf("a line is %d wide on a %d-column screen: %q\n%s", got, width, candidate, view)
			return
		}
	}
}

// One layout for every screen: a wide one only gives the PR list its columns.
func TestWideScreenStacksThePanesToo(t *testing.T) {
	model, _ := newModel()
	view := model.View()
	assertFits(t, view, 120, 40)
	if strings.Contains(view, "├── ") || strings.Contains(view, "Repos") {
		t.Errorf("there should be no repo tree:\n%s", view)
	}
	tabs := line(view, "tool")
	if !strings.Contains(tabs, "api") || !strings.Contains(tabs, "web") {
		t.Errorf("the repos should be tabs on one line: %q", tabs)
	}
	if got := lipgloss.Width(line(view, "PRs — acme/api")); got != 120 {
		t.Errorf("the PR pane is %d wide, want the whole 120", got)
	}
	if heading := line(view, "Status"); !strings.Contains(heading, "Author") {
		t.Errorf("a wide PR list should head its columns: %q", heading)
	}
	row := line(view, "#2")
	if !strings.Contains(row, "PENDING") || !strings.Contains(row, "alice") {
		t.Errorf("a wide PR row has room for the status and the author: %q", row)
	}
}

func TestHiddenTabsWithNewsAreFlagged(t *testing.T) {
	model, backend := newModel()
	backend.config.Repos = []string{"acme/alpha", "acme/bravo", "acme/charlie", "acme/delta",
		"acme/echo", "acme/foxtrot", "acme/golf"}
	backend.repos["acme/golf"] = backend.repos["acme/web"]
	backend.unseen["acme/golf"] = []int{7}
	resize(model, 40, 24)
	model.reloadRepos("acme/alpha")

	tabs := line(model.View(), "alpha")
	if !strings.Contains(tabs, "»") || strings.Contains(tabs, "›") {
		t.Errorf("an unseen PR out of sight to the right should be flagged: %q", tabs)
	}
	press(t, model, "right", "right", "right", "right", "right", "right")
	tabs = line(model.View(), "golf")
	if !strings.Contains(tabs, "‹") || strings.Contains(tabs, "«") {
		t.Errorf("quiet tabs out of sight get the plain marker: %q", tabs)
	}
}

func TestTabsNameTheOwnerWhenRepoNamesClash(t *testing.T) {
	model, backend := newModel()
	backend.config.Repos = []string{"acme/api", "Other/api", "acme/web"}
	model.reloadRepos("acme/api")
	tabs := line(model.View(), "web")
	for _, want := range []string{"acme/api", "Other/api"} {
		if !strings.Contains(tabs, want) {
			t.Errorf("two repos called api should show their owners, missing %q: %q", want, tabs)
		}
	}
	if strings.Contains(tabs, "acme/web") {
		t.Errorf("a name on its own stays short: %q", tabs)
	}
}

func TestNarrowScreenStacksThePanes(t *testing.T) {
	model, _ := newModel()
	resize(model, 45, 24)
	view := model.View()
	assertFits(t, view, 45, 24)
	if strings.Contains(view, "├── ") {
		t.Errorf("the tree should give way to tabs:\n%s", view)
	}
	tabs := line(view, "tool")
	for _, want := range []string{"api", "web", "tool"} {
		if !strings.Contains(tabs, want) {
			t.Errorf("the tabs are missing %q: %q", want, tabs)
		}
	}
	if !strings.Contains(tabs, "(2)") {
		t.Errorf("tabs should carry the unseen count: %q", tabs)
	}
	prs, details := strings.Index(view, "PRs — acme/api (2)"), strings.Index(view, "Details")
	if prs < 0 || details < prs {
		t.Errorf("the PR list should sit above the details:\n%s", view)
	}
	if got := lipgloss.Width(line(view, "PRs — acme/api")); got != 45 {
		t.Errorf("the PR pane is %d wide, want the whole 45", got)
	}
	if !strings.Contains(view, "Quit") {
		t.Errorf("the key hints should still show:\n%s", view)
	}
}

func TestNarrowPRRowsKeepTheTitle(t *testing.T) {
	model, backend := newModel()
	backend.editPR("acme/api", 2, func(pr *models.PullRequest) {
		pr.Title = "Teach the parser about tabs"
	})
	resize(model, 45, 24)
	row := line(model.View(), "#2")
	for _, want := range []string{"⋯", "Teach the parser about tabs"} {
		if !strings.Contains(row, want) {
			t.Errorf("the row is missing %q: %q", want, row)
		}
	}
	for _, dropped := range []string{"alice", "PENDING"} {
		if strings.Contains(row, dropped) {
			t.Errorf("the row should leave %q to the details: %q", dropped, row)
		}
	}
}

func TestRepoTabsScrollToTheSelection(t *testing.T) {
	model, backend := newModel()
	backend.config.Repos = []string{"acme/alpha", "acme/bravo", "acme/charlie", "acme/delta",
		"acme/echo", "acme/foxtrot", "acme/golf"}
	resize(model, 40, 24)
	model.reloadRepos("acme/alpha")

	view := model.View()
	assertFits(t, view, 40, 24)
	tabs := line(view, "alpha")
	if !strings.Contains(tabs, "›") || strings.Contains(tabs, "‹") || strings.Contains(tabs, "golf") {
		t.Errorf("only the first tabs fit, with more to the right: %q", tabs)
	}

	press(t, model, "right", "right", "right", "right", "right", "right")
	if model.selectedRepo() != "acme/golf" {
		t.Fatalf("selected = %q", model.selectedRepo())
	}
	view = model.View()
	assertFits(t, view, 40, 24)
	tabs = line(view, "golf")
	if !strings.Contains(tabs, "‹") || strings.Contains(tabs, "›") || strings.Contains(tabs, "alpha") {
		t.Errorf("the strip should scroll to the last tab: %q", tabs)
	}
}

func TestNarrowKeysFollowTheStackedLayout(t *testing.T) {
	model, backend := newModel()
	resize(model, 45, 24)
	press(t, model, "right")
	if model.selectedRepo() != "acme/web" {
		t.Fatalf("right should move to the next tab, selected = %q", model.selectedRepo())
	}
	press(t, model, "left")
	if model.selectedRepo() != "acme/api" {
		t.Fatalf("left should move back, selected = %q", model.selectedRepo())
	}
	press(t, model, "down")
	if model.focus != panePRs || !contains2(backend.recorded(), "seen acme/api 1") {
		t.Errorf("down should move into the PR list below: focus %v, calls %v",
			model.focus, backend.recorded())
	}
	press(t, model, "left")
	if model.focus != paneRepos || model.selectedRepo() != "acme/api" {
		t.Errorf("left should go back to the tabs without changing repo")
	}
}

func TestDetailsScroll(t *testing.T) {
	model, _ := newModel()
	resize(model, 120, 14) // room for six lines of details
	press(t, model, "enter")
	view := model.View()
	if strings.Contains(view, "Ready to merge") || !strings.Contains(view, "Branch:") {
		t.Fatalf("the details should start at the top, cut short:\n%s", view)
	}
	if !strings.Contains(line(view, "Details"), "J/K") {
		t.Errorf("the pane should say it scrolls: %q", line(view, "Details"))
	}

	model.Update(tea.KeyMsg{Type: tea.KeyPgDown})
	if strings.Contains(model.View(), "Branch:") {
		t.Errorf("page down should move past the first lines:\n%s", model.View())
	}
	for range 20 {
		press(t, model, "J")
	}
	view = model.View()
	if !strings.Contains(view, "Ready to merge") {
		t.Errorf("scrolling should stop with the last line showing:\n%s", view)
	}
	for range 20 {
		press(t, model, "K")
	}
	if !strings.Contains(model.View(), "Branch:") {
		t.Errorf("scrolling back should reach the top:\n%s", model.View())
	}

	for range 20 {
		press(t, model, "J")
	}
	press(t, model, "down") // another PR starts at its top
	if !strings.Contains(model.View(), "#2 A change") {
		t.Errorf("a newly selected PR should show from its first line:\n%s", model.View())
	}
}

func TestDetailsThatFitDontOfferScrolling(t *testing.T) {
	model, _ := newModel()
	press(t, model, "enter")
	if strings.Contains(line(model.View(), "Details"), "J/K") {
		t.Errorf("nothing is cut off, so no scroll hint: %q", line(model.View(), "Details"))
	}
}

func TestDetailsWrapOnNarrowScreens(t *testing.T) {
	model, backend := newModel()
	backend.editPR("acme/api", 1, func(pr *models.PullRequest) {
		pr.Title = "Rework the scheduler so that focused repositories poll soonest"
	})
	resize(model, 40, 30)
	press(t, model, "down")
	view := model.View()
	assertFits(t, view, 40, 30)
	if !strings.Contains(view, "soonest") {
		t.Errorf("a long line should wrap, not lose its end:\n%s", view)
	}
}

func TestShortScreenDropsTheKeyHints(t *testing.T) {
	model, _ := newModel()
	resize(model, 45, 12)
	view := model.View()
	assertFits(t, view, 45, 12)
	if strings.Contains(view, "Quit") {
		t.Errorf("a short screen has no room for key hints:\n%s", view)
	}
	if !strings.Contains(view, "Details") || !strings.Contains(view, "PRs — acme/api") {
		t.Errorf("both panes should still show:\n%s", view)
	}
}

func TestNarrowHeaderStaysOnOneLine(t *testing.T) {
	model, backend := newModel()
	backend.status.RateLimitedUntil = models.Ptr("2026-09-16T09:10:00Z")
	resize(model, 40, 24)
	view := model.View()
	assertFits(t, view, 40, 24)
	lines := strings.Split(view, "\n")
	if !strings.Contains(lines[0], "pr-mon") || !strings.Contains(lines[1], "tool") {
		t.Errorf("the header should take one line, then the tabs:\n%s", view)
	}
}

func TestDialogsFitNarrowScreens(t *testing.T) {
	cases := []struct {
		name string
		keys []string
	}{
		{"action menu", []string{"down", "enter"}},
		{"add repo", []string{"A"}},
		{"remove repo", []string{"D"}},
		{"dependencies", []string{"down", "w"}},
		{"dependency picker", []string{"down", "w", "a"}},
		{"notifications", []string{"N"}},
		{"settings", []string{"S"}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			model, _ := newModel()
			resize(model, 40, 30)
			press(t, model, test.keys...)
			if model.modal == nil {
				t.Fatal("the dialog didn't open")
			}
			assertFits(t, model.View(), 40, 30)
		})
	}
}

func TestToastsFitNarrowScreens(t *testing.T) {
	model, _ := newModel()
	resize(model, 40, 24)
	model.Update(eventMsg(service.Event{Kind: "toast", Severity: "error",
		Message: "Could not merge acme/api#1: the base branch requires two approving reviews"}))
	view := model.View()
	assertFits(t, view, 40, 24)
	if !strings.Contains(view, "reviews") {
		t.Errorf("a long toast should wrap, not lose its end:\n%s", view)
	}
}

func TestNarrowPRRowsMarkArmedPRs(t *testing.T) {
	model, backend := newModel()
	backend.armed["acme/api"] = map[int]models.ArmedMerge{
		2: {Method: models.MergeSquash, ArmedAt: testfixtures.CreatedAt},
	}
	resize(model, 45, 24)
	if row := line(model.View(), "#2"); !strings.Contains(row, "⋯ #2 auto* A change") {
		t.Errorf("an armed PR should be marked before its title: %q", row)
	}
}

func TestDetailsScrollOnANarrowScreen(t *testing.T) {
	model, _ := newModel()
	resize(model, 42, 20)
	press(t, model, "down") // into the PR list
	if strings.Contains(model.View(), "Ready to merge") {
		t.Fatalf("the details should be cut short:\n%s", model.View())
	}
	for range 20 {
		press(t, model, "J")
	}
	if !strings.Contains(model.View(), "Ready to merge") {
		t.Errorf("scrolling should reach the last line:\n%s", model.View())
	}
}

func TestNotificationsDialogFitsAPhone(t *testing.T) {
	model, _ := newModel()
	resize(model, 42, 20)
	press(t, model, "N")
	view := model.View()
	assertFits(t, view, 42, 20)
	if !strings.Contains(view, "Notifications — acme/api") {
		t.Errorf("the dialog's title should show:\n%s", view)
	}
}

func TestTooSmallAScreenSaysSo(t *testing.T) {
	cases := []struct {
		name          string
		width, height int
		fits          bool
	}{
		{"too short", 120, 9, false},
		{"too thin", 29, 24, false},
		{"just enough", 30, 10, true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			model, _ := newModel()
			resize(model, test.width, test.height)
			view := model.View()
			assertFits(t, view, test.width, test.height)
			asks := strings.Contains(view, "30×10")
			if asks == test.fits || strings.Contains(view, "Details") != test.fits {
				t.Errorf("at %d×%d:\n%s", test.width, test.height, view)
			}
		})
	}
}
