package tui

import (
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/acheris-labs/pr-mon/internal/actions"
	"github.com/acheris-labs/pr-mon/internal/config"
	"github.com/acheris-labs/pr-mon/internal/models"
	"github.com/acheris-labs/pr-mon/internal/notify"
	"github.com/acheris-labs/pr-mon/internal/readiness"
	"github.com/acheris-labs/pr-mon/internal/service"
	"github.com/acheris-labs/pr-mon/internal/testfixtures"
)

// fakeBackend is a dashboard backend that records what the model asked for.
type fakeBackend struct {
	mutex     sync.Mutex
	config    config.Config
	repos     map[string]models.Repo
	errs      map[string]string
	status    service.Status
	collapsed []string
	unseen    map[string][]int
	armed     map[string]map[int]models.ArmedMerge

	calls    []string
	addErr   error
	addName  string
	formErr  error
	previews []string
}

func newFakeBackend() *fakeBackend {
	api := testfixtures.Repo("acme/api", []models.PullRequest{
		readiness.Assess(testfixtures.PR(1, func(pr *models.PullRequest) {
			pr.ClosingIssues = []models.LinkedIssue{
				{Number: 12, Title: "Crash on empty config", Repo: "acme/api"},
				{Number: 7, Title: "Tracked elsewhere", Repo: "acme/infra"},
			}
		})),
		readiness.Assess(testfixtures.PR(2, func(pr *models.PullRequest) {
			testfixtures.Pending(pr)
			pr.ReviewDecision = models.Ptr("REVIEW_REQUIRED")
		})),
	})
	web := testfixtures.Repo("acme/web", []models.PullRequest{
		readiness.Assess(testfixtures.PR(7, testfixtures.Failing)),
	})
	settings := config.New()
	settings.Repos = []string{"acme/api", "acme/web", "Other/tool"}
	return &fakeBackend{
		config: settings,
		repos:  map[string]models.Repo{"acme/api": api, "acme/web": web},
		errs:   map[string]string{},
		status: service.Status{Connected: true, Version: "1.2.3",
			Notifier: models.Ptr("/x/tn"), Warnings: []string{}},
		unseen: map[string][]int{"acme/api": {1, 2}, "acme/web": {7}},
		armed:  map[string]map[int]models.ArmedMerge{},
	}
}

func (f *fakeBackend) record(call string) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.calls = append(f.calls, call)
}

func (f *fakeBackend) recorded() []string {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	return append([]string{}, f.calls...)
}

func (f *fakeBackend) Config() config.Config         { return f.config }
func (f *fakeBackend) Repos() map[string]models.Repo { return f.repos }
func (f *fakeBackend) Errors() map[string]string     { return f.errs }
func (f *fakeBackend) Status() service.Status        { return f.status }
func (f *fakeBackend) Collapsed() []string           { return f.collapsed }
func (f *fakeBackend) Unseen(name string) []int      { return f.unseen[name] }

// Repo fills in action menus the way the real backend does.
func (f *fakeBackend) Repo(name string) (models.Repo, bool) {
	repo, found := f.repos[name]
	if !found {
		return models.Repo{}, false
	}
	return actions.WithActions(repo, f.Armed(name)), true
}

func (f *fakeBackend) Armed(name string) map[int]models.ArmedMerge {
	if armed, found := f.armed[name]; found {
		return armed
	}
	return map[int]models.ArmedMerge{}
}

func (f *fakeBackend) RefreshAll() error {
	f.record("refresh")
	return nil
}

func (f *fakeBackend) AddRepo(name string) (string, error) {
	f.record("add " + name)
	if f.addErr != nil {
		return "", f.addErr
	}
	if f.addName == "" {
		return name, nil
	}
	return f.addName, nil
}

func (f *fakeBackend) RemoveRepo(name string) error {
	f.record("remove " + name)
	return nil
}

func (f *fakeBackend) MarkSeen(name string, number int) error {
	f.record("seen " + name + " " + itoa(number))
	return nil
}

func (f *fakeBackend) SetCollapsed(owner string, collapsed bool) error {
	f.record("collapsed " + owner + " " + boolText(collapsed))
	f.mutex.Lock()
	defer f.mutex.Unlock()
	if collapsed {
		f.collapsed = append(f.collapsed, owner)
	} else {
		kept := []string{}
		for _, candidate := range f.collapsed {
			if candidate != owner {
				kept = append(kept, candidate)
			}
		}
		f.collapsed = kept
	}
	return nil
}

func (f *fakeBackend) Perform(repo string, number int, action models.Action) error {
	method := "-"
	if action.Method != nil {
		method = string(*action.Method)
	}
	f.record("perform " + repo + " " + itoa(number) + " " + action.Kind + " " + method + " " +
		boolText(action.DeleteBranch))
	return nil
}

func (f *fakeBackend) SaveNotifications(repo string, settings config.NotifyConfig) error {
	f.record("save " + repo + " " + strings.Join(settings.Events, ",") + " " + settings.Message)
	return nil
}

func (f *fakeBackend) SendTest(repo string, settings config.NotifyConfig) error {
	f.record("test " + repo)
	return nil
}

func (f *fakeBackend) NotificationForm() (notify.Form, error) {
	f.record("form")
	if f.formErr != nil {
		return notify.Form{}, f.formErr
	}
	return notify.NotificationForm(), nil
}

func (f *fakeBackend) PreviewNotification(repo, message string) (notify.Preview, error) {
	f.mutex.Lock()
	f.previews = append(f.previews, message)
	f.mutex.Unlock()
	return notify.PreviewMessage(repo, message), nil
}

// AddDependency records the call and adds the edge the way the backend would,
// so the dialog sees it; "bad" is refused.
func (f *fakeBackend) AddDependency(repo string, number int, on string) error {
	f.record("depend " + repo + " " + itoa(number) + " " + on)
	if on == "bad" {
		return errors.New(`expected owner/repo#number or a pull request URL, got "bad"`)
	}
	f.editPR(repo, number, func(pr *models.PullRequest) {
		pr.WaitsOn = append(pr.WaitsOn, models.PRRef{Repo: "acme/web", Number: 7,
			Title: "Failing thing", State: models.PROpen, Status: models.Ptr(models.StatusFailing)})
	})
	return nil
}

func (f *fakeBackend) RemoveDependency(repo string, number int, on string) error {
	f.record("undepend " + repo + " " + itoa(number) + " " + on)
	f.editPR(repo, number, func(pr *models.PullRequest) {
		kept := []models.PRRef{}
		for _, ref := range pr.WaitsOn {
			if ref.Key() != on {
				kept = append(kept, ref)
			}
		}
		pr.WaitsOn = kept
	})
	return nil
}

func (f *fakeBackend) DependencyGraph(repo string, number int) (models.DependencyGraph, error) {
	f.record("graph " + repo + " " + itoa(number))
	merged := models.PRRef{Repo: "acme/lib", Number: 3, Title: "Groundwork", State: models.PRMerged}
	return models.DependencyGraph{
		PR: models.PRRef{Repo: repo, Number: number, Title: "A change", State: models.PROpen,
			Status: models.Ptr(models.StatusWaiting)},
		WaitsOn: []models.DependencyNode{{
			PR: models.PRRef{Repo: "acme/web", Number: 7, Title: "Failing thing",
				State: models.PROpen, Status: models.Ptr(models.StatusFailing)},
			Children: []models.DependencyNode{{PR: merged, Children: []models.DependencyNode{}}},
		}},
		RequiredBy: []models.DependencyNode{},
	}, nil
}

func (f *fakeBackend) editPR(repo string, number int, edit func(*models.PullRequest)) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	loaded := f.repos[repo]
	for i := range loaded.PRs {
		if loaded.PRs[i].Number == number {
			edit(&loaded.PRs[i])
		}
	}
	f.repos[repo] = loaded
}

func itoa(number int) string { return strconv.Itoa(number) }

func boolText(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

// press sends one key and runs whatever command it returned, so the fake backend
// sees the call.
func press(t *testing.T, model *Model, keys ...string) {
	t.Helper()
	for _, name := range keys {
		var key tea.KeyMsg
		switch name {
		case "enter":
			key = tea.KeyMsg{Type: tea.KeyEnter}
		case "esc":
			key = tea.KeyMsg{Type: tea.KeyEscape}
		case "up":
			key = tea.KeyMsg{Type: tea.KeyUp}
		case "down":
			key = tea.KeyMsg{Type: tea.KeyDown}
		case "left":
			key = tea.KeyMsg{Type: tea.KeyLeft}
		case "right":
			key = tea.KeyMsg{Type: tea.KeyRight}
		case "space":
			key = tea.KeyMsg{Type: tea.KeySpace}
		case "ctrl+s":
			key = tea.KeyMsg{Type: tea.KeyCtrlS}
		default:
			key = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(name)}
		}
		_, cmd := model.Update(key)
		drain(t, model, cmd)
	}
}

// drain runs a command and feeds any message it produces back into the model.
func drain(t *testing.T, model *Model, cmd tea.Cmd) {
	t.Helper()
	for range 10 {
		if cmd == nil {
			return
		}
		message := cmd()
		if message == nil {
			return
		}
		if batch, ok := message.(tea.BatchMsg); ok {
			for _, inner := range batch {
				drain(t, model, inner)
			}
			return
		}
		_, cmd = model.Update(message)
	}
}

func newModel() (*Model, *fakeBackend) {
	backend := newFakeBackend()
	model := New(backend, "1.2.3")
	return model, backend
}

func TestTreeShowsOwnersAndRepos(t *testing.T) {
	model, _ := newModel()
	view := model.View()
	for _, want := range []string{"acme/", "api", "web", "Other/", "tool"} {
		if !strings.Contains(view, want) {
			t.Errorf("the tree is missing %q:\n%s", want, view)
		}
	}
	if !strings.Contains(view, "(2)") {
		t.Errorf("unseen counts should show:\n%s", view)
	}
	if model.selectedRepo() != "acme/api" {
		t.Errorf("selected = %q, want the first repo", model.selectedRepo())
	}
}

func TestNavigationAndMarkSeen(t *testing.T) {
	model, backend := newModel()
	press(t, model, "down") // acme/web
	if model.selectedRepo() != "acme/web" {
		t.Fatalf("selected = %q", model.selectedRepo())
	}
	if !strings.Contains(model.View(), "PRs — acme/web (1)") {
		t.Errorf("the PR pane should follow the tree:\n%s", model.View())
	}
	press(t, model, "enter") // into the PR list
	if model.focus != panePRs {
		t.Error("enter on a repo should move to its pull requests")
	}
	if !contains2(backend.recorded(), "seen acme/web 7") {
		t.Errorf("reading a PR should mark it seen: %v", backend.recorded())
	}
}

func TestCollapseAndExpand(t *testing.T) {
	model, backend := newModel()
	press(t, model, "left") // from the repo to its owner row
	if model.selectedRepo() != "" {
		t.Error("left should move to the owner row")
	}
	press(t, model, "left") // collapse the group
	if !contains2(backend.recorded(), "collapsed acme true") {
		t.Errorf("calls = %v", backend.recorded())
	}
	model.rebuildRows("")
	if strings.Contains(model.View(), "├── ") {
		t.Errorf("a collapsed group should hide its repos:\n%s", model.View())
	}
	press(t, model, "right")
	if !contains2(backend.recorded(), "collapsed acme false") {
		t.Errorf("calls = %v", backend.recorded())
	}
}

func TestActionMenuOffersWhatTheBackendDecided(t *testing.T) {
	model, backend := newModel()
	press(t, model, "enter") // into acme/api's PRs (PR 1 is ready)
	press(t, model, "enter") // open the action menu
	view := model.View()
	if !strings.Contains(view, "[m] Merge") || !strings.Contains(view, "[a] ") {
		t.Fatalf("menu = \n%s", view)
	}
	if !strings.Contains(view, "Delete remote branch feature-1") {
		t.Errorf("the delete-branch choice should show:\n%s", view)
	}
	press(t, model, "m") // needs a method: acme/api allows three
	if !strings.Contains(model.View(), "Merge method") {
		t.Fatalf("expected the method choice:\n%s", model.View())
	}
	press(t, model, "s")
	if !contains2(backend.recorded(), "perform acme/api 1 merge SQUASH true") {
		t.Errorf("calls = %v", backend.recorded())
	}
	if model.modal != nil {
		t.Error("choosing a method should close the menu")
	}
}

func TestActionMenuDeleteToggleAndEscape(t *testing.T) {
	model, backend := newModel()
	press(t, model, "enter", "enter", "d") // open the menu, turn off deleting
	press(t, model, "m", "s")
	if !contains2(backend.recorded(), "perform acme/api 1 merge SQUASH false") {
		t.Errorf("calls = %v", backend.recorded())
	}
	press(t, model, "enter", "esc")
	if model.modal != nil {
		t.Error("escape should close the menu")
	}
}

func TestUnavailableActionDoesNothing(t *testing.T) {
	model, backend := newModel()
	press(t, model, "down", "enter") // acme/web's failing PR
	press(t, model, "enter")
	if !strings.Contains(model.View(), "unavailable") {
		t.Fatalf("menu = \n%s", model.View())
	}
	press(t, model, "m")
	for _, call := range backend.recorded() {
		if strings.HasPrefix(call, "perform") {
			t.Errorf("an unavailable action should do nothing: %v", backend.recorded())
		}
	}
	if model.modal == nil {
		t.Error("the menu should stay open")
	}
}

func TestAddRepo(t *testing.T) {
	model, backend := newModel()
	backend.addName = "Acme/New"
	press(t, model, "A")
	if model.modal == nil {
		t.Fatal("A should open the add dialog")
	}
	press(t, model, "n", "o", "p", "e") // not owner/name
	press(t, model, "enter")
	if !strings.Contains(model.View(), "owner/name") {
		t.Errorf("a bad name should be refused:\n%s", model.View())
	}
	for range 4 {
		press(t, model, "backspace")
	}
	press(t, model, "a", "/", "b", "enter")
	if !contains2(backend.recorded(), "add a/b") {
		t.Errorf("calls = %v", backend.recorded())
	}
	if model.modal != nil {
		t.Error("a successful add should close the dialog")
	}
}

func TestAddRepoFailureStaysOpen(t *testing.T) {
	model, backend := newModel()
	backend.addErr = errors.New("Repository a/b not found")
	press(t, model, "A", "a", "/", "b", "enter")
	if model.modal == nil {
		t.Fatal("the dialog should stay open")
	}
	if !strings.Contains(model.View(), "not found") {
		t.Errorf("the error should show:\n%s", model.View())
	}
}

func TestRemoveRepoAsksFirst(t *testing.T) {
	model, backend := newModel()
	press(t, model, "D")
	if !strings.Contains(model.View(), "Stop monitoring acme/api?") {
		t.Fatalf("view = \n%s", model.View())
	}
	press(t, model, "n")
	if contains2(backend.recorded(), "remove acme/api") {
		t.Error("answering no should not remove anything")
	}
	press(t, model, "D", "y")
	if !contains2(backend.recorded(), "remove acme/api") {
		t.Errorf("calls = %v", backend.recorded())
	}
}

func TestNotificationsDialog(t *testing.T) {
	model, backend := newModel()
	press(t, model, "N")
	if model.modal == nil {
		t.Fatal("N should open the notifications dialog")
	}
	view := model.View()
	if !strings.Contains(view, "Notifications — acme/api") || !strings.Contains(view, "Preview:") {
		t.Fatalf("view = \n%s", view)
	}
	press(t, model, "right")         // Events tab
	press(t, model, "down", "space") // turn the second event off
	press(t, model, "ctrl+s")
	saved := ""
	for _, call := range backend.recorded() {
		if strings.HasPrefix(call, "save ") {
			saved = call
		}
	}
	if saved == "" {
		t.Fatalf("nothing was saved: %v", backend.recorded())
	}
	if strings.Contains(saved, "FAILING") {
		t.Errorf("the event should have been turned off: %q", saved)
	}
	if !strings.Contains(saved, "READY") {
		t.Errorf("other events should be kept: %q", saved)
	}
	if model.modal != nil {
		t.Error("saving should close the dialog")
	}
}

func TestNotificationsNeedsARepo(t *testing.T) {
	model, _ := newModel()
	press(t, model, "left") // sit on the owner row
	press(t, model, "N")
	if model.modal != nil {
		t.Error("no dialog without a repo selected")
	}
	if len(model.toasts) == 0 || !strings.Contains(model.toasts[0].message, "Select a repository") {
		t.Errorf("toasts = %+v", model.toasts)
	}
}

func TestRefreshAndQuit(t *testing.T) {
	model, backend := newModel()
	press(t, model, "r")
	if !contains2(backend.recorded(), "refresh") {
		t.Errorf("calls = %v", backend.recorded())
	}
	_, cmd := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if cmd == nil {
		t.Fatal("q should quit")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Error("q should send a quit message")
	}
	model2, _ := newModel()
	_, cmd = model2.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd == nil {
		t.Fatal("ctrl+c should quit")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Error("ctrl+c should send a quit message")
	}
}

func TestToastsAndDisconnection(t *testing.T) {
	model, _ := newModel()
	model.Update(eventMsg(service.Event{Kind: "toast", Message: "Merged acme/api#1", Severity: "information"}))
	if !strings.Contains(model.View(), "Merged acme/api#1") {
		t.Errorf("the toast should show:\n%s", model.View())
	}
	model.Update(eventMsg(service.Event{Kind: "disconnected"}))
	if model.connected {
		t.Error("the model should know it is disconnected")
	}
	if !strings.Contains(model.View(), "disconnected") {
		t.Errorf("the header should say so:\n%s", model.View())
	}
	model.Update(eventMsg(service.Event{Kind: "connected"}))
	if !model.connected || !strings.Contains(model.View(), "connected") {
		t.Errorf("reconnecting should show:\n%s", model.View())
	}
}

func TestDetailsPane(t *testing.T) {
	model, _ := newModel()
	press(t, model, "enter") // into the PR list
	view := model.View()
	for _, want := range []string{"#1 A change", "Branch:", "feature-1 → main", "Author:", "alice",
		"Opened:", "https://github.com/acme/api/pull/1", "✓ READY", "Ready to merge"} {
		if !strings.Contains(view, want) {
			t.Errorf("the details pane is missing %q:\n%s", want, view)
		}
	}
	// Linked issues: bare in this repo, qualified when they live elsewhere.
	for _, want := range []string{"Closes:", "#12 Crash on empty config",
		"acme/infra#7 Tracked elsewhere"} {
		if !strings.Contains(view, want) {
			t.Errorf("the details pane is missing %q:\n%s", want, view)
		}
	}

	press(t, model, "down") // the pending PR
	view = model.View()
	if !strings.Contains(view, "Blocked by:") || !strings.Contains(view, "PENDING") {
		t.Errorf("blocking reasons should show:\n%s", view)
	}
}

func TestArmedPRsAreMarked(t *testing.T) {
	model, backend := newModel()
	backend.armed["acme/api"] = map[int]models.ArmedMerge{
		2: {Method: models.MergeSquash, DeleteBranch: true, ArmedAt: testfixtures.CreatedAt},
	}
	press(t, model, "enter", "down")
	view := model.View()
	if !strings.Contains(view, "auto*") {
		t.Errorf("an armed PR should be marked:\n%s", view)
	}
	if !strings.Contains(view, "Auto-merge: pr-mon (squash, delete branch)") {
		t.Errorf("the details should say so:\n%s", view)
	}
}

func contains2(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

func TestDependenciesDialog(t *testing.T) {
	model, backend := newModel()
	press(t, model, "enter", "w") // PR 1's dependencies
	if !strings.Contains(model.View(), "Doesn't wait on any other PR") {
		t.Fatalf("dialog = \n%s", model.View())
	}

	// Add one through the picker: it lists open PRs in every monitored repo.
	press(t, model, "a")
	view := model.View()
	for _, want := range []string{"#2 A change", "acme/web#7 A change"} {
		if !strings.Contains(view, want) {
			t.Errorf("the picker is missing %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "#1 A change") {
		t.Errorf("a PR can't wait on itself:\n%s", view)
	}
	press(t, model, "w", "e", "b") // narrows to acme/web#7
	press(t, model, "enter")
	if !contains2(backend.recorded(), "depend acme/api 1 acme/web#7") {
		t.Fatalf("calls = %v", backend.recorded())
	}
	view = model.View()
	if !strings.Contains(view, "Waits on:") || !strings.Contains(view, "acme/web#7 Failing thing FAILING") {
		t.Errorf("the list should show it:\n%s", view)
	}

	// Anything else typed goes to the backend as is; its refusal stays on screen.
	press(t, model, "a", "b", "a", "d", "enter")
	if !strings.Contains(model.View(), `got "bad"`) {
		t.Errorf("the refusal should show:\n%s", model.View())
	}
	press(t, model, "esc")

	press(t, model, "g")
	view = model.View()
	for _, want := range []string{"Dependency graph", "└─ ✗ acme/web#7 Failing thing",
		"✓ acme/lib#3 Groundwork merged", "▶ ⧗ #1 A change WAITING"} {
		if !strings.Contains(view, want) {
			t.Errorf("the graph is missing %q:\n%s", want, view)
		}
	}
	press(t, model, "esc", "d")
	if !contains2(backend.recorded(), "undepend acme/api 1 acme/web#7") {
		t.Errorf("calls = %v", backend.recorded())
	}
	press(t, model, "esc")
	if model.modal != nil {
		t.Error("escape should close the dialog")
	}
}

func TestDependenciesFromTheActionMenu(t *testing.T) {
	model, _ := newModel()
	press(t, model, "enter", "enter")
	if !strings.Contains(model.View(), "[w] Dependencies…") {
		t.Fatalf("menu = \n%s", model.View())
	}
	press(t, model, "w")
	if _, ok := model.modal.(*dependencyModal); !ok {
		t.Errorf("w should open the dependencies dialog, got %T", model.modal)
	}
}

func TestWaitingPRDetails(t *testing.T) {
	model, backend := newModel()
	backend.editPR("acme/api", 2, func(pr *models.PullRequest) {
		pr.WaitsOn = []models.PRRef{
			{Repo: "acme/api", Number: 1, Title: "A change", State: models.PROpen,
				Status: models.Ptr(models.StatusReady)},
			{Repo: "acme/lib", Number: 5, Title: "Shared parser", State: models.PRMerged},
		}
		pr.Status = models.StatusWaiting
	})
	press(t, model, "enter", "down")
	view := model.View()
	for _, want := range []string{"⧗ WAITING", "Waits on:    ✓ #1 A change READY",
		"✓ acme/lib#5 Shared parser merged"} {
		if !strings.Contains(view, want) {
			t.Errorf("the details are missing %q:\n%s", want, view)
		}
	}
}
