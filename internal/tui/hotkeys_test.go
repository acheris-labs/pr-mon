package tui

import (
	"strings"
	"testing"

	"github.com/acheris-labs/pr-mon/internal/models"
)

// performed lists the perform calls the backend saw.
func performed(backend *fakeBackend) []string {
	calls := []string{}
	for _, call := range backend.recorded() {
		if strings.HasPrefix(call, "perform") {
			calls = append(calls, call)
		}
	}
	return calls
}

func TestMergeFromTheList(t *testing.T) {
	model, backend := newModel()
	press(t, model, "enter", "m") // PR 1 is ready; acme/api allows three methods
	if !strings.Contains(model.View(), "Merge method") {
		t.Fatalf("expected the method choice:\n%s", model.View())
	}
	press(t, model, "s")
	if !contains2(backend.recorded(), "perform acme/api 1 merge SQUASH true") {
		t.Errorf("calls = %v", backend.recorded())
	}
	if model.modal != nil {
		t.Errorf("merging should leave the dashboard, got %T", model.modal)
	}
}

func TestHotkeyEscapeGoesBackToTheDashboard(t *testing.T) {
	model, backend := newModel()
	press(t, model, "enter", "m")
	if !strings.Contains(model.View(), "esc: cancel") {
		t.Errorf("from a hotkey, escape cancels:\n%s", model.View())
	}
	press(t, model, "esc")
	if model.modal != nil {
		t.Errorf("escape should close the method choice, not open the menu: %T", model.modal)
	}
	backend.editPR("acme/api", 2, func(pr *models.PullRequest) { pr.CanBypass = true })
	press(t, model, "down", "m", "s", "n")
	if model.modal != nil {
		t.Errorf("backing out of a force merge should close it: %T", model.modal)
	}
	if calls := performed(backend); len(calls) != 0 {
		t.Errorf("nothing should have been sent: %v", calls)
	}
}

func TestAutoMergeFromTheDetails(t *testing.T) {
	model, backend := newModel()
	press(t, model, "enter", "down", "tab") // PR 2 (pending), its details
	if model.focus != paneDetails {
		t.Fatalf("focus = %v", model.focus)
	}
	press(t, model, "a", "s")
	if !contains2(backend.recorded(), "perform acme/api 2 auto_merge_on SQUASH false") {
		t.Errorf("calls = %v", backend.recorded())
	}
}

func TestForceMergeFromTheListAsksFirst(t *testing.T) {
	model, backend := newModel()
	backend.editPR("acme/api", 2, func(pr *models.PullRequest) { pr.CanBypass = true })
	press(t, model, "enter", "down", "m", "s")
	if !strings.Contains(model.View(), "Force merge #2?") {
		t.Fatalf("expected a confirmation:\n%s", model.View())
	}
	press(t, model, "y")
	if !contains2(backend.recorded(), "perform acme/api 2 force_merge SQUASH true") {
		t.Errorf("calls = %v", backend.recorded())
	}
}

func TestUnavailableHotkeySaysWhy(t *testing.T) {
	model, backend := newModel()
	press(t, model, "right", "enter", "m") // acme/web's failing PR
	if calls := performed(backend); len(calls) != 0 {
		t.Errorf("an unavailable merge sent %v", calls)
	}
	if model.modal != nil {
		t.Errorf("nothing to choose, so no dialog: %T", model.modal)
	}
	if len(model.toasts) == 0 || !strings.Contains(model.toasts[0].message, "Merge: FAILING") {
		t.Errorf("toasts = %+v", model.toasts)
	}
}

func TestHotkeysNeedAPR(t *testing.T) {
	model, backend := newModel()
	press(t, model, "m", "a") // the repo tabs have the focus
	if calls := performed(backend); len(calls) != 0 || model.modal != nil {
		t.Errorf("calls = %v, modal = %T", calls, model.modal)
	}
}

// footer is the hints as key → label, and which are dimmed or dangerous.
func footer(model *Model) (labels map[string]string, looks map[string]hintLook) {
	labels, looks = map[string]string{}, map[string]hintLook{}
	for _, hint := range model.footerHints() {
		labels[hint.key] = hint.label
		looks[hint.key] = hint.look
	}
	return labels, looks
}

func TestFooterFollowsTheSelectedPR(t *testing.T) {
	model, backend := newModel()
	labels, _ := footer(model) // the repo tabs
	if len(labels) != 3 || labels["S"] != "Settings" || labels["r"] != "Refresh" ||
		labels["q"] != "Quit" {
		t.Errorf("with no PR selected, footer = %v", labels)
	}
	for _, gone := range []string{"Add", "Remove", "Notify"} {
		if strings.Contains(model.View(), gone) {
			t.Errorf("%q moved into settings:\n%s", gone, model.View())
		}
	}

	press(t, model, "enter") // PR 1, ready
	labels, looks := footer(model)
	if labels["m"] != "Merge" || looks["m"] != hintPlain {
		t.Errorf("ready PR: m = %q (%v)", labels["m"], looks["m"])
	}
	if labels["a"] != "Auto-merge" || looks["a"] != hintOff {
		t.Errorf("ready PR: a = %q (%v), want the dimmed auto-merge", labels["a"], looks["a"])
	}
	if labels["w"] != "Deps" || labels["⏎"] != "Actions" || labels["S"] != "Settings" {
		t.Errorf("footer = %v", labels)
	}

	press(t, model, "down") // PR 2, pending
	labels, looks = footer(model)
	if looks["m"] != hintOff || labels["a"] != "Enable auto-merge" || looks["a"] != hintPlain {
		t.Errorf("pending PR: footer = %v, looks = %v", labels, looks)
	}

	backend.editPR("acme/api", 2, func(pr *models.PullRequest) { pr.CanBypass = true })
	labels, looks = footer(model)
	if labels["m"] != "Force merge" || looks["m"] != hintDanger {
		t.Errorf("bypassable PR: m = %q (%v)", labels["m"], looks["m"])
	}
}

func TestMuteFromTheMenu(t *testing.T) {
	model, backend := newModel()
	press(t, model, "enter", "enter")
	if !strings.Contains(model.View(), "[n] Mute notifications") {
		t.Fatalf("menu = \n%s", model.View())
	}
	press(t, model, "n")
	if !contains2(backend.recorded(), "perform acme/api 1 mute - false") {
		t.Errorf("calls = %v", backend.recorded())
	}
}

func TestMutedPRsAreMarked(t *testing.T) {
	model, backend := newModel()
	backend.editPR("acme/api", 2, func(pr *models.PullRequest) { pr.Muted = true })
	press(t, model, "enter", "down")
	view := model.View()
	if !strings.Contains(line(view, "#2"), "🔕") {
		t.Errorf("the muted PR's row should say so: %q", line(view, "#2"))
	}
	if strings.Contains(line(view, "#1 "), "🔕") {
		t.Errorf("PR 1 isn't muted: %q", line(view, "#1 "))
	}
	if !strings.Contains(view, "Notifications muted") {
		t.Errorf("the details should say so:\n%s", view)
	}
	press(t, model, "enter")
	if !strings.Contains(model.View(), "[n] Unmute notifications") {
		t.Errorf("menu = \n%s", model.View())
	}
}

func TestSettingsHoldTheRepoActions(t *testing.T) {
	model, backend := newModel()
	press(t, model, "S")
	view := model.View()
	for _, want := range []string{"[a] Add repository…", "[d] Remove acme/api…",
		"[n] Notifications for acme/api…"} {
		if !strings.Contains(view, want) {
			t.Errorf("settings should offer %q:\n%s", want, view)
		}
	}
	press(t, model, "a")
	if _, ok := model.modal.(*addRepoModal); !ok {
		t.Errorf("a should open the add dialog, got %T", model.modal)
	}

	// The cursor reaches them too: interval, then add, remove, notifications.
	press(t, model, "esc", "S", "down", "down", "enter")
	if !strings.Contains(model.View(), "Stop monitoring acme/api?") {
		t.Errorf("enter on Remove should ask:\n%s", model.View())
	}
	press(t, model, "y")
	if !contains2(backend.recorded(), "remove acme/api") {
		t.Errorf("calls = %v", backend.recorded())
	}
}

func TestSettingsWithoutARepo(t *testing.T) {
	model, backend := newModel()
	backend.config.Repos = nil
	model.reloadRepos("")
	press(t, model, "S")
	view := model.View()
	if !strings.Contains(view, "[a] Add repository…") || strings.Contains(view, "[d]") ||
		strings.Contains(view, "[n]") {
		t.Errorf("with no repo, only adding makes sense:\n%s", view)
	}
	press(t, model, "n", "d")
	if _, ok := model.modal.(*settingsModal); !ok {
		t.Errorf("n and d should do nothing here, got %T", model.modal)
	}
}
