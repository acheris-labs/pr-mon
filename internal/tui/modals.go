// Modal dialogs: the PR action menu, add repo, confirm, and notification settings.

package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/acheris-labs/pr-mon/internal/config"
	"github.com/acheris-labs/pr-mon/internal/models"
	"github.com/acheris-labs/pr-mon/internal/notify"
)

// modal is one open dialog; update returns the dialog to keep (nil closes it).
type modal interface {
	update(*Model, tea.KeyMsg) (modal, tea.Cmd)
	view(*Model) string
	title() string
}

// ----- PR actions -----

type actionMenu struct {
	repo models.Repo
	pr   models.PullRequest
	// set while asking which merge method to use
	choosing     *models.ActionOption
	deleteBranch bool
}

var menuSlots = []struct{ key, shortcut string }{
	{"merge", "m"}, {"auto_merge", "a"}, {"update", "u"}, {"draft", "t"}, {"rerun", "r"},
}

var methodKeys = []struct {
	method models.MergeMethod
	key    string
	label  string
}{
	{models.MergeSquash, "s", "Squash and merge"},
	{models.MergeCommit, "m", "Create a merge commit"},
	{models.MergeRebase, "r", "Rebase and merge"},
}

func newActionMenu(repo models.Repo, pr models.PullRequest) *actionMenu {
	return &actionMenu{repo: repo, pr: pr, deleteBranch: true}
}

func (a *actionMenu) title() string { return fmt.Sprintf("#%d %s", a.pr.Number, a.pr.Title) }

func (a *actionMenu) option(key string) (models.ActionOption, bool) {
	for _, option := range a.pr.Actions {
		if option.Key == key {
			return option, true
		}
	}
	return models.ActionOption{}, false
}

func (a *actionMenu) offersDelete() bool {
	for _, option := range a.pr.Actions {
		if option.OffersDeleteBranch {
			return true
		}
	}
	return false
}

func (a *actionMenu) methods() []models.MergeMethod {
	return a.repo.MergeMethods
}

func (a *actionMenu) update(model *Model, key tea.KeyMsg) (modal, tea.Cmd) {
	switch key.String() {
	case "esc", "escape":
		if a.choosing != nil {
			a.choosing = nil
			return a, nil
		}
		return nil, nil
	case "d":
		if a.choosing == nil && a.offersDelete() {
			a.deleteBranch = !a.deleteBranch
			return a, nil
		}
	case "w":
		if a.choosing == nil {
			return newDependencies(a.repo.Name, a.pr.Number), nil
		}
	}
	if a.choosing != nil {
		for _, choice := range methodKeys {
			if key.String() == choice.key && contains(a.methods(), choice.method) {
				return nil, a.send(model, *a.choosing, &choice.method)
			}
		}
		return a, nil
	}
	for _, slot := range menuSlots {
		if key.String() != slot.shortcut {
			continue
		}
		option, found := a.option(slot.key)
		if !found {
			return a, nil
		}
		if !option.Available {
			return a, nil // the reason is already on screen
		}
		if !option.NeedsMethod {
			return nil, a.send(model, option, nil)
		}
		methods := a.methods()
		if len(methods) == 1 {
			return nil, a.send(model, option, &methods[0])
		}
		a.choosing = &option
		return a, nil
	}
	return a, nil
}

func (a *actionMenu) send(model *Model, option models.ActionOption, method *models.MergeMethod) tea.Cmd {
	action := models.Action{
		Kind:         option.Kind,
		Method:       method,
		DeleteBranch: option.OffersDeleteBranch && a.deleteBranch,
	}
	return model.perform(a.repo.Name, a.pr.Number, action)
}

func contains(methods []models.MergeMethod, method models.MergeMethod) bool {
	for _, candidate := range methods {
		if candidate == method {
			return true
		}
	}
	return false
}

// ----- add repo -----

type addRepoModal struct {
	input   textinput.Model
	message string
	adding  bool
}

func newAddRepo() *addRepoModal {
	input := textinput.New()
	input.Placeholder = "owner/name"
	input.Focus()
	input.Prompt = "> "
	return &addRepoModal{input: input}
}

func (a *addRepoModal) title() string { return "Add repository (owner/name)" }

// repoAdded reports what AddRepo said.
type repoAdded struct {
	name string
	err  error
}

func (a *addRepoModal) update(model *Model, key tea.KeyMsg) (modal, tea.Cmd) {
	switch key.String() {
	case "esc", "escape":
		return nil, nil
	case "enter":
		name := strings.TrimSpace(a.input.Value())
		parts := strings.Split(name, "/")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			a.message = "Enter the repo as owner/name"
			return a, nil
		}
		a.message = "Checking…"
		a.adding = true
		return a, func() tea.Msg {
			added, err := model.backend.AddRepo(name)
			return repoAdded{name: added, err: err}
		}
	}
	var cmd tea.Cmd
	a.input, cmd = a.input.Update(key)
	return a, cmd
}

// ----- confirm -----

type confirmModal struct {
	message string
	confirm func(*Model) tea.Cmd
}

func newConfirm(message string, confirm func(*Model) tea.Cmd) *confirmModal {
	return &confirmModal{message: message, confirm: confirm}
}

func (c *confirmModal) title() string { return c.message }

func (c *confirmModal) update(model *Model, key tea.KeyMsg) (modal, tea.Cmd) {
	switch key.String() {
	case "y":
		return nil, c.confirm(model)
	case "n", "esc", "escape":
		return nil, nil
	}
	return c, nil
}

// ----- notification settings -----

type notifyTab int

const (
	tabMessage notifyTab = iota
	tabEvents
	tabScript
	tabDesktop
)

var tabNames = []string{"Message", "Events", "Script", "Desktop"}

type notificationsModal struct {
	repo     string
	settings config.NotifyConfig
	form     notify.Form
	notifier *string
	tab      notifyTab
	// which event checkbox the cursor sits on
	eventCursor int
	message     textinput.Model
	script      textinput.Model
	preview_    notify.Preview
	previewErr  string
}

// previewReady carries a rendered preview back to the dialog.
type previewReady struct {
	preview notify.Preview
	err     error
}

func newNotifications(repo string, settings config.NotifyConfig, form notify.Form,
	notifier *string) *notificationsModal {
	message := textinput.New()
	message.SetValue(settings.Message)
	message.Prompt = "> "
	message.Width = 60
	message.Focus()
	script := textinput.New()
	script.SetValue(settings.Script)
	script.Placeholder = "e.g. im --deliver tgram"
	script.Prompt = "> "
	script.Width = 60
	return &notificationsModal{
		repo: repo, settings: settings, form: form, notifier: notifier,
		message: message, script: script,
	}
}

func (n *notificationsModal) title() string { return "Notifications — " + n.repo }

func (n *notificationsModal) preview(model *Model) tea.Cmd {
	template := n.message.Value()
	return func() tea.Msg {
		rendered, err := model.backend.PreviewNotification(n.repo, template)
		return previewReady{preview: rendered, err: err}
	}
}

// current is the settings as edited.
func (n *notificationsModal) current() config.NotifyConfig {
	settings := n.settings
	settings.Message = n.message.Value()
	settings.Script = n.script.Value()
	return settings
}

func (n *notificationsModal) update(model *Model, key tea.KeyMsg) (modal, tea.Cmd) {
	switch key.String() {
	case "esc", "escape":
		return nil, nil
	case "ctrl+s":
		settings := n.current()
		return nil, run(func() error { return model.backend.SaveNotifications(n.repo, settings) })
	case "ctrl+t":
		settings := n.current()
		return n, run(func() error { return model.backend.SendTest(n.repo, settings) })
	case "left":
		n.tab = (n.tab + 3) % 4
		n.focusInputs()
		return n, nil
	case "right":
		n.tab = (n.tab + 1) % 4
		n.focusInputs()
		return n, nil
	}
	switch n.tab {
	case tabMessage:
		var cmd tea.Cmd
		n.message, cmd = n.message.Update(key)
		return n, tea.Batch(cmd, n.preview(model))
	case tabEvents:
		return n.updateEvents(key)
	case tabScript:
		if key.String() == " " || key.String() == "space" {
			n.settings.ScriptEnabled = !n.settings.ScriptEnabled
			return n, nil
		}
		var cmd tea.Cmd
		n.script, cmd = n.script.Update(key)
		return n, cmd
	case tabDesktop:
		if (key.String() == " " || key.String() == "space") && n.notifier != nil {
			n.settings.DesktopEnabled = !n.settings.DesktopEnabled
		}
	}
	return n, nil
}

func (n *notificationsModal) updateEvents(key tea.KeyMsg) (modal, tea.Cmd) {
	// The last row is "Include draft PRs".
	rows := len(n.form.Events) + 1
	switch key.String() {
	case "up", "k":
		n.eventCursor = (n.eventCursor - 1 + rows) % rows
	case "down", "j":
		n.eventCursor = (n.eventCursor + 1) % rows
	case " ", "space", "enter", "x":
		if n.eventCursor == len(n.form.Events) {
			n.settings.IncludeDrafts = !n.settings.IncludeDrafts
			return n, nil
		}
		name := n.form.Events[n.eventCursor].Name
		n.settings.Events = toggleEvent(n.settings.Events, name, n.form)
	}
	return n, nil
}

// toggleEvent adds or removes one event, keeping the backend's order.
func toggleEvent(events []string, name string, form notify.Form) []string {
	chosen := map[string]bool{}
	for _, event := range events {
		chosen[event] = true
	}
	chosen[name] = !chosen[name]
	kept := []string{}
	for _, option := range form.Events {
		if chosen[option.Name] {
			kept = append(kept, option.Name)
		}
	}
	return kept
}

func (n *notificationsModal) focusInputs() {
	n.message.Blur()
	n.script.Blur()
	switch n.tab {
	case tabMessage:
		n.message.Focus()
	case tabScript:
		n.script.Focus()
	}
}
