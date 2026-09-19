// Package service is the monitor: it owns config.toml and state.json, polls
// GitHub, notifies, and performs merges. Everything else talks to it.
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/acheris-labs/pr-mon/internal/actions"
	"github.com/acheris-labs/pr-mon/internal/config"
	"github.com/acheris-labs/pr-mon/internal/dependencies"
	"github.com/acheris-labs/pr-mon/internal/github"
	"github.com/acheris-labs/pr-mon/internal/models"
	"github.com/acheris-labs/pr-mon/internal/notify"
	"github.com/acheris-labs/pr-mon/internal/readiness"
	"github.com/acheris-labs/pr-mon/internal/state"
	"github.com/acheris-labs/pr-mon/internal/tracker"
)

const (
	MinPollInterval    = 10
	MaxPollInterval    = 3600
	checkingRetryDelay = 5 * time.Second
	checkingRetries    = 3
	// GitHub's wording when the head or base moves under a merge ("Head branch was
	// modified. Review and try the merge again."); such merges are retried next refresh.
	retryableMergeError = "was modified"
)

// Error is a command failure whose message is meant for the user.
type Error struct{ Message string }

func (e *Error) Error() string { return e.Message }

// Status is what clients show about the backend itself.
type Status struct {
	Connected        bool     `json:"connected"`
	PID              *int     `json:"pid"`
	Version          string   `json:"version"`
	Notifier         *string  `json:"notifier"`
	LastUpdate       *string  `json:"last_update"`        // ISO-8601 UTC
	RateLimitedUntil *string  `json:"rate_limited_until"` // ISO-8601 UTC
	Warnings         []string `json:"warnings"`
}

// Event is something that changed. Kinds: repos, repo, seen, collapsed, config,
// status, toast, disconnected. Name is the repo for repo and seen events.
type Event struct {
	Kind     string
	Name     string
	Message  string
	Severity string
}

type Listener func(Event)

// GitHub is the part of the GitHub client the monitor uses.
type GitHub interface {
	FetchRepo(ctx context.Context, name string) (models.Repo, error)
	Merge(ctx context.Context, prID string, method models.MergeMethod, expectedHeadOid string) error
	UpdateBranch(ctx context.Context, prID string) error
	EnableAutoMerge(ctx context.Context, prID string, method models.MergeMethod) error
	DisableAutoMerge(ctx context.Context, prID string) error
	DeleteBranch(ctx context.Context, refID string) error
	LookupPRs(ctx context.Context, repo string, numbers []int) ([]models.PRRef, error)
}

// Deliverer sends one notification; the monitor calls it in the background.
type Deliverer func(ctx context.Context, settings config.NotifyConfig, notifier string,
	variables map[string]string) []notify.Result

type prKey struct {
	repo   string
	number int
}

// Monitor owns the backend's state. Every exported method is safe to call from
// several connections at once.
type Monitor struct {
	client     GitHub
	configPath string
	statePath  string
	deliver    Deliverer
	log        *slog.Logger

	mutex  sync.Mutex
	config config.Config
	state  state.AppState
	track  *tracker.Tracker
	repos  map[string]models.Repo
	// Repos as fetched, before dependencies and actions were worked out, so
	// those can be redone when a dependency changes between polls.
	fetched map[string]models.Repo
	errs    map[string]string
	// The last known state of every PR something waits on, by key.
	waitedOn map[string]models.PRRef
	status   Status
	// Repos whose first load finished this session; only they can notify.
	loaded      map[string]bool
	pausedUntil time.Time
	merging     map[prKey]bool
	// Waiting PRs being moved off GitHub's auto-merge, and ones that couldn't be.
	holding    map[prKey]bool
	holdFailed map[prKey]bool
	listeners  []Listener

	busy     sync.WaitGroup
	ctx      context.Context
	cancel   context.CancelFunc
	pollWake chan struct{}
	stopped  chan struct{}
	// Shutdown is closed when a client asks the backend to stop.
	Shutdown chan struct{}
	shutOnce sync.Once
}

type Options struct {
	Notifier string
	Version  string
	Deliver  Deliverer
	Logger   *slog.Logger
}

func New(client GitHub, configPath, statePath string, options Options) *Monitor {
	deliver := options.Deliver
	if deliver == nil {
		deliver = notify.Deliver
	}
	logger := options.Logger
	if logger == nil {
		logger = slog.Default()
	}
	pid := os.Getpid()
	monitor := &Monitor{
		client:     client,
		configPath: configPath,
		statePath:  statePath,
		deliver:    deliver,
		log:        logger,
		config:     config.New(),
		state:      state.New(),
		repos:      map[string]models.Repo{},
		fetched:    map[string]models.Repo{},
		errs:       map[string]string{},
		waitedOn:   map[string]models.PRRef{},
		loaded:     map[string]bool{},
		merging:    map[prKey]bool{},
		holding:    map[prKey]bool{},
		holdFailed: map[prKey]bool{},
		pollWake:   make(chan struct{}, 1),
		stopped:    make(chan struct{}),
		Shutdown:   make(chan struct{}),
		status: Status{
			Connected: true,
			PID:       &pid,
			Version:   options.Version,
			Warnings:  []string{},
		},
	}
	if options.Notifier != "" {
		monitor.status.Notifier = models.Ptr(options.Notifier)
	}
	monitor.track = tracker.New(monitor.state.PRs)
	return monitor
}

// Start loads the saved config and state, polls once, and keeps polling.
func (m *Monitor) Start(ctx context.Context) {
	m.ctx, m.cancel = context.WithCancel(ctx)
	loadedConfig, configWarning := config.Load(m.configPath)
	loadedState, stateWarning := state.Load(m.statePath)
	m.mutex.Lock()
	m.config = loadedConfig
	m.state = loadedState
	m.track = tracker.New(m.state.PRs)
	for _, warning := range []string{configWarning, stateWarning} {
		if warning != "" {
			m.status.Warnings = append(m.status.Warnings, warning)
			m.log.Warn(warning)
		}
	}
	m.mutex.Unlock()
	m.spawn(func() { m.PollOnce(m.ctx) })
	go m.pollLoop()
}

// Stop ends polling and saves state.
func (m *Monitor) Stop() {
	if m.cancel != nil {
		m.cancel()
	}
	close(m.stopped)
	m.busy.Wait()
	m.mutex.Lock()
	defer m.mutex.Unlock()
	m.saveStateLocked()
}

// WaitIdle waits until no refresh, action or notification is in flight.
func (m *Monitor) WaitIdle() { m.busy.Wait() }

func (m *Monitor) spawn(work func()) {
	m.busy.Add(1)
	go func() {
		defer m.busy.Done()
		work()
	}()
}

func (m *Monitor) pollLoop() {
	for {
		m.mutex.Lock()
		interval := time.Duration(m.config.PollInterval) * time.Second
		m.mutex.Unlock()
		select {
		case <-m.stopped:
			return
		case <-m.pollWake:
		case <-time.After(interval):
		}
		select {
		case <-m.stopped:
			return
		default:
		}
		m.PollOnce(m.ctx)
	}
}

// ----- events -----

func (m *Monitor) AddListener(listener Listener) {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	m.listeners = append(m.listeners, listener)
}

func (m *Monitor) emit(event Event) {
	m.mutex.Lock()
	listeners := append([]Listener{}, m.listeners...)
	m.mutex.Unlock()
	for _, listener := range listeners {
		listener(event)
	}
}

func (m *Monitor) toast(message string, severity ...string) {
	level := "information"
	if len(severity) > 0 {
		level = severity[0]
	}
	m.emit(Event{Kind: "toast", Message: message, Severity: level})
}

// ----- read side -----

func (m *Monitor) Config() config.Config {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	return m.config
}

func (m *Monitor) Repos() map[string]models.Repo {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	copied := map[string]models.Repo{}
	for name, repo := range m.repos {
		copied[name] = repo
	}
	return copied
}

func (m *Monitor) Repo(name string) (models.Repo, bool) {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	repo, found := m.repos[name]
	return repo, found
}

func (m *Monitor) Errors() map[string]string {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	copied := map[string]string{}
	for name, message := range m.errs {
		copied[name] = message
	}
	return copied
}

func (m *Monitor) Status() Status {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	status := m.status
	status.Warnings = append([]string{}, m.status.Warnings...)
	return status
}

func (m *Monitor) Collapsed() []string {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	return append([]string{}, m.state.Collapsed...)
}

func (m *Monitor) Unseen(name string) []int {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	return m.track.Unseen(name)
}

// Merging reports whether a merge (manual or pr-mon's) is in flight; restarting
// now could lose its follow-up steps.
func (m *Monitor) Merging() bool {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	return len(m.merging) > 0
}

func (m *Monitor) Armed(name string) map[int]models.ArmedMerge {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	return m.armedLocked(name)
}

func (m *Monitor) armedLocked(name string) map[int]models.ArmedMerge {
	armed := map[int]models.ArmedMerge{}
	for key, raw := range m.state.Armed[name] {
		number, err := strconv.Atoi(key)
		if err != nil {
			m.log.Warn("ignoring invalid armed merge", "repo", name, "pr", key)
			continue
		}
		var merge models.ArmedMerge
		if err := json.Unmarshal(raw, &merge); err != nil || merge.Method == "" || merge.ArmedAt == "" {
			m.log.Warn("ignoring invalid armed merge", "repo", name, "pr", key)
			continue
		}
		armed[number] = merge
	}
	return armed
}

// ----- polling -----

// PollOnce refreshes every monitored repo, unless a rate limit is still in force.
func (m *Monitor) PollOnce(ctx context.Context) {
	m.mutex.Lock()
	if !m.pausedUntil.IsZero() && time.Now().UTC().Before(m.pausedUntil) {
		m.mutex.Unlock()
		return
	}
	m.pausedUntil = time.Time{}
	names := append([]string{}, m.config.Repos...)
	m.mutex.Unlock()

	// First, so this poll's repos are judged against fresh dependency states.
	m.refreshWaitedOn(ctx)

	var group sync.WaitGroup
	for _, name := range names {
		group.Add(1)
		go func(name string) {
			defer group.Done()
			m.RefreshRepo(ctx, name, 0)
		}(name)
	}
	group.Wait()
}

// RefreshRepo fetches one repo and applies the result.
func (m *Monitor) RefreshRepo(ctx context.Context, name string, attempt int) {
	repo, err := m.client.FetchRepo(ctx, name)
	if err != nil {
		m.handleFetchError(name, err)
		return
	}
	m.mutex.Lock()
	delete(m.errs, name)
	m.mutex.Unlock()
	m.apply(name, repo)

	checking := false
	for _, pr := range repo.PRs {
		if pr.Status == models.StatusChecking {
			checking = true
			break
		}
	}
	if attempt < checkingRetries && checking {
		m.scheduleRetry(name, attempt+1)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	m.mutex.Lock()
	m.status.LastUpdate = &now
	m.status.RateLimitedUntil = nil
	m.mutex.Unlock()
	m.emit(Event{Kind: "status"})
}

func (m *Monitor) handleFetchError(name string, err error) {
	var limited *github.RateLimitError
	if errors.As(err, &limited) {
		until := limited.ResetAt.Format(time.RFC3339Nano)
		m.mutex.Lock()
		m.pausedUntil = limited.ResetAt
		m.status.RateLimitedUntil = &until
		m.mutex.Unlock()
		m.emit(Event{Kind: "status"})
		return
	}
	m.mutex.Lock()
	monitored := false
	for _, repo := range m.config.Repos {
		if repo == name {
			monitored = true
			break
		}
	}
	if monitored {
		m.errs[name] = err.Error()
	}
	m.mutex.Unlock()
	if monitored {
		m.emit(Event{Kind: "repo", Name: name})
	}
}

// scheduleRetry looks again shortly, because GitHub is still computing mergeability.
func (m *Monitor) scheduleRetry(name string, attempt int) {
	m.spawn(func() {
		select {
		case <-m.stopped:
		case <-time.After(checkingRetryDelay):
			m.RefreshRepo(m.ctx, name, attempt)
		}
	})
}

// apply records a fetched repo: notifications, change tracking and armed merges.
func (m *Monitor) apply(name string, repo models.Repo) {
	m.mutex.Lock()
	monitored := false
	for _, candidate := range m.config.Repos {
		if candidate == name {
			monitored = true
			break
		}
	}
	if !monitored {
		m.mutex.Unlock()
		return
	}
	m.fetched[name] = repo
	m.pruneDependenciesLocked(repo)
	repo = m.decorateLocked(repo)
	m.repos[name] = repo
	changes := m.track.Changes(repo)
	settings, configured := m.config.Notifications[name]
	firstLoad := !m.loaded[name]
	m.loaded[name] = true
	alerted := len(m.track.Update(repo)) > 0
	owner := models.OwnerKey(name)
	opened := false
	if alerted {
		for i, collapsed := range m.state.Collapsed {
			if collapsed == owner {
				// A new alert opens its group so it's visible next time anyone looks.
				m.state.Collapsed = append(m.state.Collapsed[:i], m.state.Collapsed[i+1:]...)
				opened = true
				break
			}
		}
	}
	m.saveStateLocked()
	m.mutex.Unlock()

	if configured && !firstLoad {
		for _, notification := range notify.Select(repo, changes, settings) {
			variables := notify.PRVariables(notification.Repo, notification.PR, notification.State, "")
			m.send(settings, variables, false)
		}
	}
	if opened {
		m.emit(Event{Kind: "collapsed"})
	}
	m.holdForDependencies(name)
	m.checkArmed(name, repo)
	m.emit(Event{Kind: "repo", Name: name})
}

// ----- pr-mon auto-merge -----

// checkArmed merges armed PRs that are strictly ready and forgets ones that closed.
func (m *Monitor) checkArmed(name string, repo models.Repo) {
	armed := m.Armed(name)
	if len(armed) == 0 {
		return
	}
	prs := map[int]models.PullRequest{}
	for _, pr := range repo.PRs {
		prs[pr.Number] = pr
	}
	truncated := repo.PRTotal > len(repo.PRs)
	for number, merge := range armed {
		pr, present := prs[number]
		if !present {
			// Beyond the newest-N cut-off it may still be open; otherwise it closed.
			if !truncated {
				m.log.Info("no longer armed: the PR closed", "repo", name, "pr", number)
				m.disarm(name, number)
			}
			continue
		}
		if !pr.StrictlyReady {
			continue
		}
		key := prKey{repo: name, number: number}
		m.mutex.Lock()
		// Check again under the lock: another refresh may have merged and
		// disarmed it since `armed` was read.
		_, stillArmed := m.armedLocked(name)[number]
		claimed := stillArmed && !m.merging[key]
		if claimed {
			m.merging[key] = true
		}
		m.mutex.Unlock()
		if !claimed {
			continue
		}
		m.spawn(func() { m.autoMerge(name, pr, merge) })
	}
}

func (m *Monitor) disarm(name string, number int) {
	m.mutex.Lock()
	entries := m.state.Armed[name]
	delete(entries, strconv.Itoa(number))
	if len(entries) == 0 {
		delete(m.state.Armed, name)
	}
	m.saveStateLocked()
	m.rebuildLocked(name)
	m.mutex.Unlock()
}

// arm records a PR for pr-mon to merge once it is strictly ready.
func (m *Monitor) arm(name string, number int, merge models.ArmedMerge) error {
	encoded, err := json.Marshal(merge)
	if err != nil {
		return err
	}
	m.mutex.Lock()
	if m.state.Armed[name] == nil {
		m.state.Armed[name] = map[string]json.RawMessage{}
	}
	m.state.Armed[name][strconv.Itoa(number)] = encoded
	m.saveStateLocked()
	m.rebuildLocked(name)
	m.mutex.Unlock()
	return nil
}

// rebuildLocked works out a repo's dependencies and action menus again, after
// its armed merges or dependencies changed.
func (m *Monitor) rebuildLocked(name string) {
	if repo, found := m.fetched[name]; found {
		m.repos[name] = m.decorateLocked(repo)
	}
}

// decorateLocked adds what the backend knows beyond GitHub to a fetched repo:
// what each PR waits on (holding it back until those merge), and its actions.
func (m *Monitor) decorateLocked(repo models.Repo) models.Repo {
	graph := dependencies.Graph(m.state.Dependencies)
	prs := make([]models.PullRequest, len(repo.PRs))
	for i, pr := range repo.PRs {
		key := models.PRKey(repo.Name, pr.Number)
		pr.WaitsOn = m.refsLocked(graph.WaitsOn(key))
		pr.RequiredBy = m.refsLocked(graph.RequiredBy(key))
		prs[i] = readiness.Wait(pr)
	}
	repo.PRs = prs
	return actions.WithActions(repo, m.armedLocked(repo.Name))
}

func (m *Monitor) autoMerge(name string, pr models.PullRequest, merge models.ArmedMerge) {
	label := fmt.Sprintf("%s#%d", name, pr.Number)
	defer func() {
		m.mutex.Lock()
		delete(m.merging, prKey{repo: name, number: pr.Number})
		m.mutex.Unlock()
	}()
	head := ""
	if pr.HeadSha != nil {
		head = *pr.HeadSha
	}
	if err := m.client.Merge(m.ctx, pr.ID, merge.Method, head); err != nil {
		if strings.Contains(err.Error(), retryableMergeError) {
			m.log.Info("auto-merge raced a change; retrying next refresh",
				"pr", label, "error", err)
			return
		}
		m.log.Warn("auto-merge failed", "pr", label, "error", err)
		m.disarm(name, pr.Number)
		m.emit(Event{Kind: "repo", Name: name})
		m.toast(fmt.Sprintf("pr-mon auto-merge failed for %s: %v", label, err), "error")
		m.notifyResult(name, pr, "MERGE_FAILED", err.Error())
		return
	}
	m.log.Info("auto-merged", "pr", label)
	m.disarm(name, pr.Number)
	m.merged(name, pr)
	m.toast(fmt.Sprintf("Merged %s (pr-mon auto-merge, %s)", label, merge.Method.Lower()))
	if merge.DeleteBranch && pr.HeadRefID != nil {
		if err := m.client.DeleteBranch(m.ctx, *pr.HeadRefID); err != nil {
			m.toast(fmt.Sprintf("Merged %s, but couldn't delete %s: %v", label, pr.HeadRef, err),
				"warning")
		}
	}
	m.notifyResult(name, pr, "MERGED", "")
	m.RefreshRepo(m.ctx, name, 0)
	m.refreshWaiting(m.ctx, name, pr.Number)
}

// notifyResult reports a merge the user asked for, so the first-load gate doesn't apply.
func (m *Monitor) notifyResult(name string, pr models.PullRequest, state, reason string) {
	m.mutex.Lock()
	settings, configured := m.config.Notifications[name]
	m.mutex.Unlock()
	if !configured {
		return
	}
	for _, event := range settings.Events {
		if event == state {
			m.send(settings, notify.PRVariables(name, pr, state, reason), false)
			return
		}
	}
}

// ----- notifications -----

func (m *Monitor) send(settings config.NotifyConfig, variables map[string]string, isTest bool) {
	notifier := ""
	m.mutex.Lock()
	if m.status.Notifier != nil {
		notifier = *m.status.Notifier
	}
	m.mutex.Unlock()
	m.spawn(func() {
		results := m.deliver(m.ctx, settings, notifier, variables)
		for _, result := range results {
			switch {
			case result.Error != "":
				m.log.Warn("notification failed", "channel", result.Channel, "error", result.Error)
				m.toast(fmt.Sprintf("%s notification failed: %s",
					strings.ToUpper(result.Channel[:1])+result.Channel[1:], result.Error), "warning")
			case isTest:
				m.toast(fmt.Sprintf("Test %s notification sent", result.Channel))
			}
		}
		if isTest && len(results) == 0 {
			m.toast("Nothing to send: enable Script or Desktop notifications", "warning")
		}
	})
}
