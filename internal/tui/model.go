// Package tui is the terminal dashboard: a repo tree, the pull requests in the
// selected repo, and the details of the selected PR.
package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/acheris-labs/pr-mon/internal/config"
	"github.com/acheris-labs/pr-mon/internal/models"
	"github.com/acheris-labs/pr-mon/internal/notify"
	"github.com/acheris-labs/pr-mon/internal/service"
)

// Backend is what the dashboard needs from the backend, whether over a socket or
// in the same process (tests).
type Backend interface {
	Config() config.Config
	Repos() map[string]models.Repo
	Repo(name string) (models.Repo, bool)
	Errors() map[string]string
	Status() service.Status
	Collapsed() []string
	Unseen(name string) []int
	Armed(name string) map[int]models.ArmedMerge

	RefreshAll() error
	AddRepo(name string) (string, error)
	RemoveRepo(name string) error
	MarkSeen(name string, number int) error
	SetCollapsed(owner string, collapsed bool) error
	Perform(repo string, number int, action models.Action) error
	AddDependency(repo string, number int, on string) error
	RemoveDependency(repo string, number int, on string) error
	DependencyGraph(repo string, number int) (models.DependencyGraph, error)
	SaveNotifications(repo string, settings config.NotifyConfig) error
	SendTest(repo string, settings config.NotifyConfig) error
	NotificationForm() (notify.Form, error)
	PreviewNotification(repo, message string) (notify.Preview, error)
}

type pane int

const (
	paneRepos pane = iota
	panePRs
)

const (
	toastLife    = 6 * time.Second
	tickInterval = time.Second
)

// row is one line of the repo tree: an owner group or a repo under it.
type row struct {
	owner     string // lower-cased grouping key
	ownerName string // as spelled
	repo      string // empty for an owner row
	last      bool   // last repo in its group
}

type toast struct {
	message  string
	severity string
	until    time.Time
}

// Model is the dashboard's state.
type Model struct {
	backend Backend
	version string

	width, height int
	focus         pane
	rows          []row
	cursor        int
	prCursor      int
	shownRepo     string
	modal         modal
	toasts        []toast
	connected     bool
	quitting      bool
	now           time.Time

	// events arrive from the backend on another goroutine.
	events chan service.Event
	// reconnect is called when the connection drops; nil in tests.
	reconnect func() tea.Cmd
}

// New builds a dashboard around a backend.
func New(backend Backend, version string) *Model {
	model := &Model{
		backend:   backend,
		version:   version,
		events:    make(chan service.Event, 256),
		connected: true,
		now:       time.Now(),
		width:     120,
		height:    40,
	}
	model.rebuildRows("")
	// Start on the first repo, not its group heading.
	for index, item := range model.rows {
		if item.repo != "" {
			model.cursor = index
			break
		}
	}
	return model
}

// Events is where a caller feeds backend events in.
func (m *Model) Events() chan<- service.Event { return m.events }

func (m *Model) Init() tea.Cmd {
	return tea.Batch(waitForEvent(m.events), tick())
}

type eventMsg service.Event

type tickMsg time.Time

// commandDone reports a background command's outcome.
type commandDone struct {
	err error
}

func waitForEvent(events chan service.Event) tea.Cmd {
	return func() tea.Msg { return eventMsg(<-events) }
}

func tick() tea.Cmd {
	return tea.Tick(tickInterval, func(now time.Time) tea.Msg { return tickMsg(now) })
}

func (m *Model) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch typed := message.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = typed.Width, typed.Height
		return m, nil

	case tickMsg:
		m.now = time.Time(typed)
		m.expireToasts()
		return m, tick()

	case eventMsg:
		return m, tea.Batch(m.applyEvent(service.Event(typed)), waitForEvent(m.events))

	case repoAdded:
		dialog, isAdd := m.modal.(*addRepoModal)
		if typed.err != nil {
			if isAdd {
				dialog.message = typed.err.Error()
				dialog.adding = false
			}
			return m, nil
		}
		m.modal = nil
		m.rebuildRows(typed.name)
		return m, nil

	case previewReady:
		if dialog, ok := m.modal.(*notificationsModal); ok {
			dialog.preview_ = typed.preview
			dialog.previewErr = ""
			if typed.err != nil {
				dialog.previewErr = typed.err.Error()
			}
		}
		return m, nil

	case dependencyDone:
		if dialog, ok := m.modal.(*dependencyModal); ok {
			dialog.finished(m, typed)
		} else if typed.err != nil {
			m.addToast(typed.err.Error(), "error")
		}
		return m, nil

	case graphReady:
		if dialog, ok := m.modal.(*dependencyModal); ok {
			graph := typed.graph
			dialog.graph = &graph
			dialog.graphErr = ""
			if typed.err != nil {
				dialog.graphErr = typed.err.Error()
			}
		}
		return m, nil

	case commandDone:
		if typed.err != nil {
			m.addToast(typed.err.Error(), "error")
		}
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(typed)
	}
	return m, nil
}

func (m *Model) applyEvent(event service.Event) tea.Cmd {
	switch event.Kind {
	case "toast":
		m.addToast(event.Message, event.Severity)
	case "repos", "repo", "seen", "collapsed", "config":
		m.rebuildRows(m.selectedRepo())
	case "disconnected":
		m.connected = false
		m.addToast("Backend disconnected — reconnecting…", "warning")
		if m.reconnect != nil {
			return m.reconnect()
		}
	case "connected":
		m.connected = true
		m.addToast("Backend reconnected", "information")
		m.rebuildRows(m.selectedRepo())
	}
	return nil
}

// ----- rows and selection -----

// rebuildRows lays the tree out again, keeping the cursor on `keep` when it can.
func (m *Model) rebuildRows(keep string) {
	names := append([]string{}, m.backend.Config().Repos...)
	sort.Slice(names, func(i, j int) bool {
		left, right := names[i], names[j]
		if models.OwnerKey(left) != models.OwnerKey(right) {
			return models.OwnerKey(left) < models.OwnerKey(right)
		}
		return strings.ToLower(left) < strings.ToLower(right)
	})
	collapsed := map[string]bool{}
	for _, owner := range m.backend.Collapsed() {
		collapsed[owner] = true
	}
	rows := []row{}
	for index, name := range names {
		owner := models.OwnerKey(name)
		if index == 0 || models.OwnerKey(names[index-1]) != owner {
			rows = append(rows, row{owner: owner, ownerName: models.Owner(name)})
		}
		if collapsed[owner] {
			continue
		}
		last := index == len(names)-1 || models.OwnerKey(names[index+1]) != owner
		rows = append(rows, row{owner: owner, ownerName: models.Owner(name), repo: name, last: last})
	}
	m.rows = rows
	m.moveCursorTo(keep)
}

func (m *Model) moveCursorTo(repo string) {
	if repo != "" {
		for index, candidate := range m.rows {
			if candidate.repo == repo {
				m.cursor = index
				return
			}
		}
		// Its group collapsed: sit on the group instead.
		for index, candidate := range m.rows {
			if candidate.repo == "" && candidate.owner == models.OwnerKey(repo) {
				m.cursor = index
				return
			}
		}
	}
	if m.cursor >= len(m.rows) {
		m.cursor = max(0, len(m.rows)-1)
	}
}

func (m *Model) currentRow() (row, bool) {
	if m.cursor < 0 || m.cursor >= len(m.rows) {
		return row{}, false
	}
	return m.rows[m.cursor], true
}

func (m *Model) selectedRepo() string {
	current, ok := m.currentRow()
	if !ok {
		return ""
	}
	return current.repo
}

func (m *Model) prs() []models.PullRequest {
	repo, found := m.backend.Repo(m.selectedRepo())
	if !found {
		return nil
	}
	return repo.PRs
}

func (m *Model) selectedPR() (models.PullRequest, bool) {
	prs := m.prs()
	if len(prs) == 0 {
		return models.PullRequest{}, false
	}
	index := min(m.prCursor, len(prs)-1)
	return prs[index], true
}

func (m *Model) unseen(name string) map[int]bool {
	unseen := map[int]bool{}
	for _, number := range m.backend.Unseen(name) {
		unseen[number] = true
	}
	return unseen
}

// ----- toasts -----

func (m *Model) addToast(message, severity string) {
	if severity == "" {
		severity = "information"
	}
	life := toastLife
	if severity == "error" {
		life *= 2
	}
	m.toasts = append(m.toasts, toast{message: message, severity: severity, until: m.now.Add(life)})
	if len(m.toasts) > 5 {
		m.toasts = m.toasts[len(m.toasts)-5:]
	}
}

func (m *Model) expireToasts() {
	kept := m.toasts[:0]
	for _, item := range m.toasts {
		if item.until.After(m.now) {
			kept = append(kept, item)
		}
	}
	m.toasts = kept
}

// run performs a backend command in the background and reports failures as toasts.
func run(work func() error) tea.Cmd {
	return func() tea.Msg { return commandDone{err: work()} }
}

// markSeen clears the marker on the PR the cursor sits on.
func (m *Model) markSeen() tea.Cmd {
	name := m.selectedRepo()
	pr, ok := m.selectedPR()
	if name == "" || !ok || !m.unseen(name)[pr.Number] {
		return nil
	}
	return run(func() error { return m.backend.MarkSeen(name, pr.Number) })
}

func (m *Model) statusLine() string {
	status := m.backend.Status()
	parts := []string{}
	if m.connected {
		parts = append(parts, "● connected")
	} else {
		return "○ disconnected"
	}
	switch {
	case status.RateLimitedUntil != nil:
		parts = append(parts, "rate limited until "+localTime(*status.RateLimitedUntil))
	case status.LastUpdate != nil:
		parts = append(parts, "updated "+localTime(*status.LastUpdate))
	}
	return strings.Join(parts, " · ")
}

func localTime(stamp string) string {
	when, err := time.Parse(time.RFC3339Nano, stamp)
	if err != nil {
		return stamp
	}
	return when.Local().Format("15:04:05")
}

// age is a timestamp with a rough distance: "2026-09-16 09:10 (2h ago)".
func age(stamp string, now time.Time) string {
	when, err := time.Parse(time.RFC3339Nano, stamp)
	if err != nil {
		return stamp
	}
	seconds := int(now.Sub(when).Seconds())
	if seconds < 0 {
		seconds = 0
	}
	var ago string
	switch {
	case seconds < 60:
		ago = "just now"
	case seconds < 3600:
		ago = fmt.Sprintf("%dm ago", seconds/60)
	case seconds < 86400:
		ago = fmt.Sprintf("%dh ago", seconds/3600)
	default:
		ago = fmt.Sprintf("%dd ago", seconds/86400)
	}
	return fmt.Sprintf("%s (%s)", when.Local().Format("2006-01-02 15:04"), ago)
}
