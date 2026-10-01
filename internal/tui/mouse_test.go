package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/acheris-labs/pr-mon/internal/models"
	"github.com/acheris-labs/pr-mon/internal/readiness"
	"github.com/acheris-labs/pr-mon/internal/testfixtures"
)

// where is the screen cell at which `text` first shows.
func where(t *testing.T, model *Model, text string) (int, int) {
	t.Helper()
	view := model.View()
	for y, candidate := range strings.Split(view, "\n") {
		if at := strings.Index(candidate, text); at >= 0 {
			return lipgloss.Width(candidate[:at]), y
		}
	}
	t.Fatalf("%q isn't on screen:\n%s", text, view)
	return 0, 0
}

// mouseAt sends a mouse event at a cell of the screen as last drawn, and runs
// what it leads to.
func mouseAt(t *testing.T, model *Model, x, y int, button tea.MouseButton) {
	t.Helper()
	event := tea.MouseMsg{X: x, Y: y, Action: tea.MouseActionPress, Button: button}
	// The zones of a frame are filed by another goroutine: give it a moment, and
	// longer if nothing is there yet.
	time.Sleep(20 * time.Millisecond)
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
		if model.zoneAt(event, "") != "" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	_, cmd := model.Update(event)
	drain(t, model, cmd)
}

// tap clicks where `text` first shows.
func tap(t *testing.T, model *Model, text string) {
	t.Helper()
	x, y := where(t, model, text)
	mouseAt(t, model, x, y, tea.MouseButtonLeft)
}

// wheel turns the wheel over where `text` first shows.
func wheel(t *testing.T, model *Model, text string, button tea.MouseButton) {
	t.Helper()
	x, y := where(t, model, text)
	mouseAt(t, model, x, y, button)
}

func TestTapSelectsARepo(t *testing.T) {
	model, _ := newModel()
	press(t, model, "enter") // into the PR list, so the tap has to bring focus back
	tap(t, model, "web")
	if model.selectedRepo() != "acme/web" || model.focus != paneRepos {
		t.Errorf("selected %q, focus %v", model.selectedRepo(), model.focus)
	}
}

func TestTapSelectsAPRThenOpensItsActions(t *testing.T) {
	model, backend := newModel()
	tap(t, model, "#2")
	if model.focus != panePRs || model.prCursor != 1 {
		t.Fatalf("focus %v, PR cursor %d", model.focus, model.prCursor)
	}
	if !contains2(backend.recorded(), "seen acme/api 2") {
		t.Errorf("tapping a PR should mark it seen: %v", backend.recorded())
	}
	if model.modal != nil {
		t.Fatal("the first tap only selects")
	}
	tap(t, model, "#2")
	if _, ok := model.modal.(*actionMenu); !ok {
		t.Errorf("tapping the selected PR should open its actions, got %T", model.modal)
	}
}

func TestTapOnAKeyHintPressesIt(t *testing.T) {
	model, backend := newModel()
	tap(t, model, "Refresh")
	if !contains2(backend.recorded(), "refresh") {
		t.Errorf("calls = %v", backend.recorded())
	}
	tap(t, model, "Settings")
	if _, ok := model.modal.(*settingsModal); !ok {
		t.Errorf("tapping Settings should open it, got %T", model.modal)
	}
}

func TestTapsWorkInDialogs(t *testing.T) {
	model, backend := newModel()
	press(t, model, "enter", "enter") // PR 1's action menu
	tap(t, model, "[m] Merge")
	if !strings.Contains(model.View(), "Merge method") {
		t.Fatalf("tapping Merge should ask for the method:\n%s", model.View())
	}
	tap(t, model, "Squash and merge")
	if !contains2(backend.recorded(), "perform acme/api 1 merge SQUASH true") {
		t.Errorf("calls = %v", backend.recorded())
	}

	press(t, model, "enter")
	tap(t, model, "Delete remote branch")
	if !strings.Contains(model.View(), "[ ] Delete remote branch") {
		t.Errorf("tapping the checkbox should clear it:\n%s", model.View())
	}
	tap(t, model, "esc: close")
	if model.modal != nil {
		t.Errorf("tapping esc should close the menu, got %T", model.modal)
	}
}

func TestEachKeyInAHintIsItsOwnTap(t *testing.T) {
	model, backend := newModel()
	press(t, model, "D") // "y: yes   n/esc: no"
	tap(t, model, "n/esc")
	if model.modal != nil || contains2(backend.recorded(), "remove acme/api") {
		t.Fatalf("tapping n should back out: modal %T, calls %v", model.modal, backend.recorded())
	}
	press(t, model, "D")
	tap(t, model, "y: yes")
	if !contains2(backend.recorded(), "remove acme/api") {
		t.Errorf("tapping y should confirm: %v", backend.recorded())
	}

	press(t, model, "S") // "↑/↓: choose   ←/→/space: change   esc: close"
	tap(t, model, "→")
	if !contains2(backend.recorded(), "interval 300") {
		t.Errorf("tapping → should step the interval up: %v", backend.recorded())
	}
}

func TestTapBehindADialogDoesNothing(t *testing.T) {
	model, _ := newModel()
	x, y := where(t, model, "web")
	press(t, model, "S")
	mouseAt(t, model, x, y, tea.MouseButtonLeft)
	if model.selectedRepo() != "acme/api" {
		t.Errorf("a tap went through the dialog to %q", model.selectedRepo())
	}
	if _, ok := model.modal.(*settingsModal); !ok {
		t.Errorf("the dialog should stay open, got %T", model.modal)
	}
}

func TestWheelActsOnWhatIsUnderIt(t *testing.T) {
	model, _ := newModel()
	resize(model, 120, 14) // room for six lines of details
	for range 20 {
		wheel(t, model, "Details", tea.MouseButtonWheelDown)
	}
	if !strings.Contains(model.View(), "Ready to merge") {
		t.Errorf("the wheel over the details should scroll them:\n%s", model.View())
	}
	wheel(t, model, "Details", tea.MouseButtonWheelUp)
	if strings.Contains(model.View(), "Ready to merge") {
		t.Errorf("the wheel should scroll back up too:\n%s", model.View())
	}

	wheel(t, model, "PRs — acme/api", tea.MouseButtonWheelDown)
	if model.focus != panePRs || model.prCursor != 1 {
		t.Errorf("the wheel over the PR list should move down it: focus %v, cursor %d",
			model.focus, model.prCursor)
	}
}

func TestWheelMovesThroughADialogList(t *testing.T) {
	model, _ := newModel()
	press(t, model, "enter", "w", "a") // the dependency picker
	wheel(t, model, "waits on", tea.MouseButtonWheelDown)
	if dialog := model.modal.(*dependencyModal); dialog.pickCursor != 1 {
		t.Errorf("the wheel should move the picker's cursor, at %d", dialog.pickCursor)
	}
}

func TestTapOnATab(t *testing.T) {
	model, _ := newModel()
	resize(model, 45, 24)
	tap(t, model, "web")
	if model.selectedRepo() != "acme/web" {
		t.Errorf("selected %q", model.selectedRepo())
	}
	tap(t, model, "Add")
	if _, ok := model.modal.(*addRepoModal); !ok {
		t.Errorf("tapping Add should open the dialog, got %T", model.modal)
	}
}

func TestTapFindsTheRowInAScrolledList(t *testing.T) {
	model, backend := newModel()
	prs := []models.PullRequest{}
	for number := 101; number <= 112; number++ {
		prs = append(prs, readiness.Assess(testfixtures.PR(number)))
	}
	backend.repos["acme/web"] = testfixtures.Repo("acme/web", prs)
	resize(model, 45, 20)
	press(t, model, "right", "down") // acme/web's PR list
	for range 9 {
		press(t, model, "down")
	}
	if strings.Contains(model.View(), "#101") {
		t.Fatalf("the list should have scrolled:\n%s", model.View())
	}
	tap(t, model, "#108")
	if pr, _ := model.selectedPR(); pr.Number != 108 {
		t.Errorf("tapped #108, selected #%d", pr.Number)
	}
}

func TestHintsWrapWholeOnNarrowScreens(t *testing.T) {
	model, backend := newModel()
	resize(model, 40, 24)
	press(t, model, "N")
	for _, hint := range []string{"ctrl+s: save", "ctrl+t: send test", "esc: cancel"} {
		if !strings.Contains(model.View(), hint) {
			t.Errorf("%q should stay on one line:\n%s", hint, model.View())
		}
	}
	tap(t, model, "ctrl+s: save")
	saved := false
	for _, call := range backend.recorded() {
		saved = saved || strings.HasPrefix(call, "save acme/api")
	}
	if !saved {
		t.Errorf("tapping save should save: %v", backend.recorded())
	}
}

func TestEventsTabOffersSpaceToTap(t *testing.T) {
	model, _ := newModel()
	press(t, model, "N", "right") // the Events tab
	before := fmt.Sprint(model.modal.(*notificationsModal).settings.Events)
	tap(t, model, "space: toggle")
	if after := fmt.Sprint(model.modal.(*notificationsModal).settings.Events); after == before {
		t.Errorf("tapping space should toggle the event under the cursor: %s", after)
	}
}
