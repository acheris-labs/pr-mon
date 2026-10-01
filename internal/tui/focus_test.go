package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestTabCyclesThroughThePanes(t *testing.T) {
	model, backend := newModel()
	press(t, model, "tab")
	if model.focus != panePRs || !contains2(backend.recorded(), "seen acme/api 1") {
		t.Fatalf("tab should move to the PR list: focus %v, calls %v", model.focus, backend.recorded())
	}
	press(t, model, "tab")
	if model.focus != paneDetails {
		t.Fatalf("tab should move on to the details, focus %v", model.focus)
	}
	press(t, model, "tab")
	if model.focus != paneRepos {
		t.Errorf("tab should come back round to the repos, focus %v", model.focus)
	}
}

func TestShiftTabCyclesBackwards(t *testing.T) {
	model, _ := newModel()
	want := []pane{paneDetails, panePRs, paneRepos}
	for _, pane := range want {
		press(t, model, "shift+tab")
		if model.focus != pane {
			t.Fatalf("focus %v, want %v", model.focus, pane)
		}
	}
}

func TestTabStaysPutWithoutPRs(t *testing.T) {
	model, _ := newModel()
	press(t, model, "down", "down", "down") // Other/tool has no PRs loaded
	if model.selectedRepo() != "Other/tool" {
		t.Fatalf("selected %q", model.selectedRepo())
	}
	press(t, model, "tab")
	if model.focus != paneRepos {
		t.Errorf("with no PRs there is nowhere to tab to, focus %v", model.focus)
	}
	press(t, model, "shift+tab")
	if model.focus != paneRepos {
		t.Errorf("nor backwards, focus %v", model.focus)
	}
}

func TestArrowsScrollTheFocusedDetails(t *testing.T) {
	model, _ := newModel()
	resize(model, 120, 14) // room for four lines of details
	press(t, model, "tab", "tab")
	if !strings.Contains(line(model.View(), "Details"), "↑/↓") {
		t.Errorf("the focused pane should name the arrows: %q", line(model.View(), "Details"))
	}
	for range 20 {
		press(t, model, "down")
	}
	if !strings.Contains(model.View(), "Ready to merge") {
		t.Errorf("down should scroll to the last line:\n%s", model.View())
	}
	if model.prCursor != 0 {
		t.Errorf("the arrows should leave the PR alone, cursor %d", model.prCursor)
	}
	for range 20 {
		press(t, model, "up")
	}
	if !strings.Contains(model.View(), "Branch:") {
		t.Errorf("up should scroll back to the top:\n%s", model.View())
	}
	press(t, model, "G")
	if !strings.Contains(model.View(), "Ready to merge") {
		t.Errorf("G should jump to the end:\n%s", model.View())
	}
	press(t, model, "g")
	if !strings.Contains(model.View(), "Branch:") {
		t.Errorf("g should jump to the top:\n%s", model.View())
	}
	press(t, model, "left")
	if model.focus != panePRs {
		t.Errorf("left should go back to the PR list, focus %v", model.focus)
	}
}

func TestDetailsKeepThePRInFocusForTheBackend(t *testing.T) {
	model, backend := newModel()
	press(t, model, "tab", "tab")
	got := []string{}
	for _, call := range backend.recorded() {
		if strings.HasPrefix(call, "focus ") {
			got = append(got, call)
		}
	}
	if strings.Join(got, ",") != "focus acme/api 1" {
		t.Errorf("the PR is still what's on screen: %v", got)
	}
}

func TestEnterInTheDetailsOpensTheActions(t *testing.T) {
	model, _ := newModel()
	press(t, model, "tab", "tab", "enter")
	if _, ok := model.modal.(*actionMenu); !ok {
		t.Errorf("enter should act on the PR shown, got %T", model.modal)
	}
}

func TestTapOnTheDetailsFocusesThem(t *testing.T) {
	model, _ := newModel()
	tap(t, model, "Branch:")
	if model.focus != paneDetails {
		t.Errorf("focus %v", model.focus)
	}
}

// The terminal opens a hyperlink where the viewer is, which over ush or ssh is
// not where pr-mon runs.
func TestTheURLIsAHyperlink(t *testing.T) {
	model, _ := newModel()
	const url = "https://github.com/acme/api/pull/1"
	row := line(model.View(), url)
	open, shut := strings.Index(row, "\x1b]8;;"+url+"\x1b\\"), strings.LastIndex(row, "\x1b]8;;\x1b\\")
	if open < 0 || shut < open {
		t.Errorf("the URL should sit inside a terminal hyperlink: %q", row)
	}
}

// Wrapping ends the link at the line break and starts it again on the next
// line; a terminal takes either terminator, ST or BEL.
func TestAWrappedURLIsALinkOnEachLine(t *testing.T) {
	model, _ := newModel()
	resize(model, 30, 40)
	const url = "https://github.com/acme/api/pull/1"
	linked := 0
	for _, row := range strings.Split(model.View(), "\n") {
		if !strings.Contains(row, "\x1b]8;;"+url) {
			continue
		}
		linked++
		if !strings.Contains(row, "\x1b]8;;\x1b\\") && !strings.Contains(row, "\x1b]8;;\a") {
			t.Errorf("the link should end on its own line: %q", row)
		}
	}
	if linked != 2 {
		t.Errorf("a URL wrapped over two lines should be a link on both, got %d:\n%q", linked, model.View())
	}
}

// opened records what a model was asked to open.
func opened(model *Model) *[]string {
	urls := []string{}
	model.open = func(url string) error {
		urls = append(urls, url)
		return nil
	}
	return &urls
}

func TestTapOnTheURLOpensIt(t *testing.T) {
	model, _ := newModel()
	urls := opened(model)
	tap(t, model, "https://github.com/acme/api/pull/1")
	if strings.Join(*urls, ",") != "https://github.com/acme/api/pull/1" {
		t.Errorf("opened %v", *urls)
	}
	if model.focus != paneRepos {
		t.Errorf("opening a link shouldn't move the focus, now %v", model.focus)
	}
}

func TestTapOnAScrolledURLOpensIt(t *testing.T) {
	model, _ := newModel()
	urls := opened(model)
	resize(model, 120, 14) // room for four lines of details
	for range 6 {
		press(t, model, "J")
	}
	tap(t, model, "https://github.com/acme/api/pull/1")
	if len(*urls) != 1 {
		t.Errorf("opened %v", *urls)
	}
}

func TestTapOnEitherLineOfAWrappedURLOpensIt(t *testing.T) {
	model, _ := newModel()
	urls := opened(model)
	resize(model, 30, 40)
	tap(t, model, "https://github.com/acme/api/")
	tap(t, model, "pull/1")
	if strings.Join(*urls, ",") != "https://github.com/acme/api/pull/1,https://github.com/acme/api/pull/1" {
		t.Errorf("opened %v", *urls)
	}
}

func TestTapBesideTheURLOnlyFocusesTheDetails(t *testing.T) {
	model, _ := newModel()
	urls := opened(model)
	x, y := where(t, model, "https://github.com/acme/api/pull/1")
	mouseAt(t, model, x+60, y, tea.MouseButtonLeft)
	if len(*urls) != 0 || model.focus != paneDetails {
		t.Errorf("opened %v, focus %v", *urls, model.focus)
	}
}

// Over ssh there is no opener: the browser would open on the far machine.
func TestTapOnTheURLWithNothingToOpenIt(t *testing.T) {
	model, _ := newModel()
	tap(t, model, "https://github.com/acme/api/pull/1")
	if model.focus != paneDetails {
		t.Errorf("the tap should still focus the details, focus %v", model.focus)
	}
}
