package service_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acheris-labs/pr-mon/internal/config"
	"github.com/acheris-labs/pr-mon/internal/github"
	"github.com/acheris-labs/pr-mon/internal/models"
	"github.com/acheris-labs/pr-mon/internal/notify"
	"github.com/acheris-labs/pr-mon/internal/readiness"
	"github.com/acheris-labs/pr-mon/internal/service"
	"github.com/acheris-labs/pr-mon/internal/testfixtures"
	"github.com/acheris-labs/pr-mon/internal/twoway"
)

// fakeGitHub stands in for the API: it replays repos and records calls.
type fakeGitHub struct {
	mutex       sync.Mutex
	repos       map[string]models.Repo
	fetchErr    map[string]error
	calls       []string
	mergeHeads  []string
	mergeErrs   []error // consumed in order, then mergeErr
	mergeErr    error
	actionErr   error
	deleteErr   error
	mergeGate   chan struct{}
	fetchCounts map[string]int
	// States of PRs outside the fake repos, by "owner/repo#n"; a PR in a fake
	// repo is open. Anything else is not found.
	states map[string]string
	// What the conditional requests answer: the repo's current ETags, and each
	// PR's updated_at.
	listETags map[string]string
	baseETags map[string]string
	updatedAt map[string]string
	// Who the token belongs to; "" makes Viewer fail.
	viewer string
}

func newFakeGitHub(repos ...models.Repo) *fakeGitHub {
	fake := &fakeGitHub{
		repos:       map[string]models.Repo{},
		fetchErr:    map[string]error{},
		fetchCounts: map[string]int{},
		states:      map[string]string{},
		listETags:   map[string]string{},
		baseETags:   map[string]string{},
		updatedAt:   map[string]string{},
		// The fixtures' PRs are alice's, so by default they are the user's.
		viewer: testfixtures.PR(1).Author,
	}
	for _, repo := range repos {
		fake.repos[repo.Name] = repo
	}
	return fake
}

func (f *fakeGitHub) setRepo(repo models.Repo) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.repos[repo.Name] = repo
}

func (f *fakeGitHub) record(call string) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.calls = append(f.calls, call)
}

func (f *fakeGitHub) callsMatching(prefix string) []string {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	found := []string{}
	for _, call := range f.calls {
		if len(call) >= len(prefix) && call[:len(prefix)] == prefix {
			found = append(found, call)
		}
	}
	return found
}

func (f *fakeGitHub) FetchRepo(_ context.Context, name string) (models.Repo, error) {
	f.mutex.Lock()
	f.calls = append(f.calls, "fetch "+name)
	f.fetchCounts[name]++
	err := f.fetchErr[name]
	repo, found := f.repos[name]
	f.mutex.Unlock()
	if err != nil {
		return models.Repo{}, err
	}
	if !found {
		return models.Repo{}, &github.NotFoundError{Message: "Repository " + name + " not found"}
	}
	return repo, nil
}

func (f *fakeGitHub) Merge(_ context.Context, prID string, method models.MergeMethod, head string) error {
	f.mutex.Lock()
	f.calls = append(f.calls, fmt.Sprintf("merge %s %s", prID, method))
	f.mergeHeads = append(f.mergeHeads, head)
	var err error
	if len(f.mergeErrs) > 0 {
		err, f.mergeErrs = f.mergeErrs[0], f.mergeErrs[1:]
	} else {
		err = f.mergeErr
	}
	gate := f.mergeGate
	f.mutex.Unlock()
	if gate != nil {
		<-gate
	}
	return err
}

func (f *fakeGitHub) UpdateBranch(_ context.Context, prID string) error {
	f.record("update " + prID)
	return nil
}

func (f *fakeGitHub) EnableAutoMerge(_ context.Context, prID string, method models.MergeMethod) error {
	f.record(fmt.Sprintf("auto_on %s %s", prID, method))
	return nil
}

func (f *fakeGitHub) DisableAutoMerge(_ context.Context, prID string) error {
	f.record("auto_off " + prID)
	return nil
}

func (f *fakeGitHub) MarkReadyForReview(_ context.Context, prID string) error {
	f.record("ready " + prID)
	return f.actionErr
}

func (f *fakeGitHub) ConvertToDraft(_ context.Context, prID string) error {
	f.record("draft " + prID)
	return f.actionErr
}

func (f *fakeGitHub) RerunFailedJobs(_ context.Context, repo string, runs []int) error {
	f.record(fmt.Sprintf("rerun %s %v", repo, runs))
	return f.actionErr
}

func (f *fakeGitHub) DeleteBranch(_ context.Context, refID string) error {
	f.record("delete " + refID)
	f.mutex.Lock()
	defer f.mutex.Unlock()
	return f.deleteErr
}

// ListOpenPRs answers from the fake repo, and reports 304 when the caller's
// ETag matches the one the repo has now.
func (f *fakeGitHub) ListOpenPRs(_ context.Context, name, etag string) (github.OpenPRs, error) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.calls = append(f.calls, "list "+name)
	if err := f.fetchErr[name]; err != nil {
		return github.OpenPRs{}, err
	}
	repo, found := f.repos[name]
	if !found {
		return github.OpenPRs{}, &github.NotFoundError{Message: "Repository " + name + " not found"}
	}
	current := f.listETags[name]
	if current == "" {
		current = "etag-0"
	}
	if etag == current {
		return github.OpenPRs{Changed: false, ETag: current}, nil
	}
	listed := github.OpenPRs{Changed: true, ETag: current, Updated: map[int]string{}}
	for _, pr := range repo.PRs {
		listed.Numbers = append(listed.Numbers, pr.Number)
		stamp := f.updatedAt[models.PRKey(name, pr.Number)]
		if stamp == "" {
			stamp = testfixtures.CreatedAt
		}
		listed.Updated[pr.Number] = stamp
	}
	return listed, nil
}

func (f *fakeGitHub) BaseMoved(_ context.Context, name, etag string) (bool, string, error) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.calls = append(f.calls, "base "+name)
	current := f.baseETags[name]
	if current == "" {
		current = "base-0"
	}
	return etag != current, current, nil
}

func (f *fakeGitHub) FetchPRs(_ context.Context, name string, numbers []int) ([]models.PullRequest, error) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.calls = append(f.calls, fmt.Sprintf("fetch %s %v", name, numbers))
	wanted := map[int]bool{}
	for _, number := range numbers {
		wanted[number] = true
	}
	found := []models.PullRequest{}
	for _, pr := range f.repos[name].PRs {
		if wanted[pr.Number] {
			found = append(found, pr)
		}
	}
	return found, nil
}

// touch marks a PR as changed since the last look, the way GitHub would.
func (f *fakeGitHub) touch(repo string, number int, stamp string) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.updatedAt[models.PRKey(repo, number)] = stamp
	f.listETags[repo] = "etag-" + stamp
}

// basePushed makes the next BaseMoved report new commits on the base branch.
func (f *fakeGitHub) basePushed(repo, sha string) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.baseETags[repo] = "base-" + sha
}

func (f *fakeGitHub) setState(key, state string) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.states[key] = state
}

func (f *fakeGitHub) Viewer(context.Context) (string, error) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.calls = append(f.calls, "viewer")
	if f.viewer == "" {
		return "", &github.Error{Message: "no viewer"}
	}
	return f.viewer, nil
}

func (f *fakeGitHub) LookupPRs(_ context.Context, repo string, numbers []int) ([]models.PRRef, error) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.calls = append(f.calls, fmt.Sprintf("lookup %s %v", repo, numbers))
	refs := []models.PRRef{}
	for _, number := range numbers {
		key := models.PRKey(repo, number)
		ref := models.PRRef{Repo: repo, Number: number, Title: "Elsewhere",
			URL: "https://github.com/" + repo, State: f.states[key]}
		for _, pr := range f.repos[repo].PRs {
			if pr.Number == number && ref.State == "" {
				ref.Title, ref.State = pr.Title, models.PROpen
			}
		}
		if ref.State == "" {
			return nil, &github.NotFoundError{Message: key + " not found"}
		}
		refs = append(refs, ref)
	}
	return refs, nil
}

// harness is a monitor with temp files, a fake GitHub and recorded events.
type harness struct {
	t          *testing.T
	monitor    *service.Monitor
	client     *fakeGitHub
	configPath string
	statePath  string
	// The two-way channel the backend builds, when set: the fake, or an error.
	twoWay   *testfixtures.TwoWay
	buildErr error
	onBuild  func()
	// legacyTwoWay appends an old-style [two_way] table to the saved config.
	legacyTwoWay bool
	// What the backend gave the builder to report the channel's availability.
	onChange   func(twoway.Availability)
	mutex      sync.Mutex
	events     []service.Event
	deliveries []map[string]string
}

func start(t *testing.T, client *fakeGitHub, repos []string,
	notifications map[string]config.NotifyConfig) *harness {
	t.Helper()
	return startWith(t, client, repos, notifications, nil)
}

// startWith is start with the saved config adjusted, for the poll intervals.
func startWith(t *testing.T, client *fakeGitHub, repos []string,
	notifications map[string]config.NotifyConfig, adjust func(*config.Config)) *harness {
	t.Helper()
	return launch(t, client, repos, notifications, adjust, nil)
}

// launch is startWith with a hook that runs once the temp paths exist and
// before the monitor starts, to write files or choose the two-way channel.
func launch(t *testing.T, client *fakeGitHub, repos []string,
	notifications map[string]config.NotifyConfig, adjust func(*config.Config),
	prepare func(*harness)) *harness {
	t.Helper()
	dir := t.TempDir()
	h := &harness{
		t:          t,
		client:     client,
		configPath: filepath.Join(dir, "config.toml"),
		statePath:  filepath.Join(dir, "state.json"),
	}
	if prepare != nil {
		prepare(h)
	}
	saved := config.New()
	saved.Repos = repos
	if notifications != nil {
		saved.Notifications = notifications
	}
	if adjust != nil {
		adjust(&saved)
	}
	if err := config.Save(h.configPath, saved); err != nil {
		t.Fatal(err)
	}
	if h.legacyTwoWay {
		// Save can't write the old keys, so the file is written by hand.
		text, err := os.ReadFile(h.configPath)
		if err != nil {
			t.Fatal(err)
		}
		legacy := string(text) + "\n[two_way]\nkind = \"telegram\"\nregion = \"us-east-1\"\n"
		if err := os.WriteFile(h.configPath, []byte(legacy), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	h.boot()
	t.Cleanup(func() { h.monitor.Stop() })
	return h
}

// boot starts a monitor on the harness's files.
func (h *harness) boot() {
	options := service.Options{
		Version:  "1.2.3",
		Notifier: "/x/tn",
		Deliver:  h.deliver,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if h.twoWay != nil || h.buildErr != nil {
		options.TwoWay = func(_ context.Context, _ config.TwoWayConfig,
			onChange func(twoway.Availability)) (twoway.Channel, error) {
			if h.onBuild != nil {
				h.onBuild()
			}
			if h.buildErr != nil {
				return nil, h.buildErr
			}
			// Ready at once, as a handler that has said hello is.
			h.onChange = onChange
			onChange(twoway.Availability{Ready: true, Name: "Fake"})
			return h.twoWay, nil
		}
	}
	h.monitor = service.New(h.client, h.configPath, h.statePath, options)
	h.monitor.AddListener(func(event service.Event) {
		h.mutex.Lock()
		defer h.mutex.Unlock()
		h.events = append(h.events, event)
	})
	h.monitor.Start(context.Background())
	h.monitor.WaitIdle()
}

// restart stops the monitor and starts a fresh one on the same files, as a
// backend restart would.
func (h *harness) restart() {
	h.monitor.Stop()
	h.boot()
}

func (h *harness) deliver(_ context.Context, settings config.NotifyConfig, _ string,
	variables map[string]string) []notify.Result {
	h.mutex.Lock()
	defer h.mutex.Unlock()
	h.deliveries = append(h.deliveries, variables)
	if !settings.ScriptEnabled && !settings.DesktopEnabled {
		return nil
	}
	return []notify.Result{{Channel: "script"}}
}

func (h *harness) poll() {
	h.monitor.PollOnce(context.Background())
	h.monitor.WaitIdle()
}

func (h *harness) toasts() []string {
	h.mutex.Lock()
	defer h.mutex.Unlock()
	found := []string{}
	for _, event := range h.events {
		if event.Kind == "toast" {
			found = append(found, event.Message)
		}
	}
	return found
}

func (h *harness) kinds() []string {
	h.mutex.Lock()
	defer h.mutex.Unlock()
	found := []string{}
	for _, event := range h.events {
		found = append(found, event.Kind)
	}
	return found
}

func (h *harness) sent() []map[string]string {
	h.mutex.Lock()
	defer h.mutex.Unlock()
	return append([]map[string]string{}, h.deliveries...)
}

func (h *harness) hasToast(substring string) bool {
	for _, message := range h.toasts() {
		if contains(message, substring) {
			return true
		}
	}
	return false
}

func contains(text, substring string) bool {
	return len(substring) == 0 || (len(text) >= len(substring) &&
		func() bool {
			for i := 0; i+len(substring) <= len(text); i++ {
				if text[i:i+len(substring)] == substring {
					return true
				}
			}
			return false
		}())
}

// waitFor blocks until condition holds, or the test fails.
func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	for range 200 {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition never became true")
}

func repoWith(name string, prs ...models.PullRequest) models.Repo {
	assessed := []models.PullRequest{}
	for _, pr := range prs {
		assessed = append(assessed, readiness.Assess(pr))
	}
	return testfixtures.Repo(name, assessed)
}

func TestPollLoadsRepos(t *testing.T) {
	client := newFakeGitHub(repoWith("acme/api", testfixtures.PR(1)))
	h := start(t, client, []string{"acme/api"}, nil)
	repo, found := h.monitor.Repo("acme/api")
	if !found || len(repo.PRs) != 1 {
		t.Fatalf("repo = %+v, found = %v", repo, found)
	}
	if repo.PRs[0].Actions == nil || len(repo.PRs[0].Actions) == 0 {
		t.Error("PRs should arrive with their action menus filled in")
	}
	if h.monitor.Status().LastUpdate == nil {
		t.Error("status should record the update")
	}
	if got := h.monitor.Unseen("acme/api"); len(got) != 1 {
		t.Errorf("unseen = %v, want the new PR", got)
	}
}

func TestUnseenLeavesOutExcludedPRs(t *testing.T) {
	client := newFakeGitHub(repoWith("acme/api",
		testfixtures.PR(1, func(pr *models.PullRequest) { pr.HeadRef = "release-please--main" }),
		testfixtures.PR(2, func(pr *models.PullRequest) { pr.Author = "noisy" }),
		testfixtures.PR(3)))
	h := start(t, client, []string{"acme/api"}, nil)
	if got := h.monitor.Unseen("acme/api"); len(got) != 3 {
		t.Fatalf("unseen = %v, want all three before any exclusions", got)
	}
	settings := config.NewNotifyConfig()
	settings.SkipBranches = []string{"release-please-*"}
	settings.SkipAuthors = []string{"noisy"}
	if err := h.monitor.SaveNotifications("acme/api", settings); err != nil {
		t.Fatal(err)
	}
	if got := h.monitor.Unseen("acme/api"); !slices.Equal(got, []int{3}) {
		t.Errorf("unseen = %v, want only #3: excluded PRs get no badge", got)
	}
	if !slices.Contains(h.kinds(), "seen") {
		t.Errorf("events = %v, want a seen update so clients redraw their badges", h.kinds())
	}
}

func TestFetchErrorKeepsLastData(t *testing.T) {
	client := newFakeGitHub(repoWith("acme/api", testfixtures.PR(1)))
	h := start(t, client, []string{"acme/api"}, nil)
	client.mutex.Lock()
	client.fetchErr["acme/api"] = &github.Error{Message: "GitHub API error 500"}
	client.mutex.Unlock()
	h.poll()
	if got := h.monitor.Errors()["acme/api"]; got == "" {
		t.Error("the error should be recorded")
	}
	if repo, found := h.monitor.Repo("acme/api"); !found || len(repo.PRs) != 1 {
		t.Error("the last good data should be kept")
	}
}

func TestRateLimitPausesPolling(t *testing.T) {
	client := newFakeGitHub(repoWith("acme/api", testfixtures.PR(1)))
	h := start(t, client, []string{"acme/api"}, nil)
	reset := time.Now().UTC().Add(time.Hour)
	client.mutex.Lock()
	client.fetchErr["acme/api"] = &github.RateLimitError{Message: "rate limited", ResetAt: reset}
	before := len(client.calls)
	client.mutex.Unlock()
	h.poll()
	if h.monitor.Status().RateLimitedUntil == nil {
		t.Error("status should show the pause")
	}
	client.mutex.Lock()
	during := len(client.calls)
	client.mutex.Unlock()
	h.poll() // still paused, so no further fetch
	client.mutex.Lock()
	after := len(client.calls)
	client.mutex.Unlock()
	if during <= before || after != during {
		t.Errorf("fetches: before=%d during=%d after=%d, want the pause to hold", before, during, after)
	}
}

func TestNotificationsWaitForTheFirstLoad(t *testing.T) {
	settings := config.NewNotifyConfig()
	settings.ScriptEnabled = true
	settings.Script = "im"
	client := newFakeGitHub(repoWith("acme/api", testfixtures.PR(1, testfixtures.Pending)))
	h := start(t, client, []string{"acme/api"},
		map[string]config.NotifyConfig{"acme/api": settings})
	if got := h.sent(); len(got) != 0 {
		t.Fatalf("sent %v on the first load, want none", got)
	}
	client.setRepo(repoWith("acme/api", testfixtures.PR(1)))
	h.poll()
	sent := h.sent()
	if len(sent) != 1 || sent[0]["PR_STATE"] != "READY" || sent[0]["PR_NUM"] != "1" {
		t.Fatalf("sent = %v", sent)
	}
}

func TestNotifyFor(t *testing.T) {
	by := func(author string, bot bool) func(*models.PullRequest) {
		return func(pr *models.PullRequest) { pr.Author, pr.AuthorIsBot = author, bot }
	}
	prs := func(mutate ...func(*models.PullRequest)) []models.PullRequest {
		return []models.PullRequest{
			testfixtures.PR(1, append(mutate, by("alice", false))...),
			testfixtures.PR(2, append(mutate, by("bob", false))...),
			testfixtures.PR(3, append(mutate, by("dependabot", true))...),
			testfixtures.PR(4, append(mutate, by("release-robot", false))...),
			testfixtures.PR(5, append(mutate, by("carol", false))...),
		}
	}
	cases := []struct {
		name      string
		notifyFor []string
		myLogins  []string
		viewer    string
		want      []string
		askViewer bool
	}{
		{"everyone", []string{"everyone"}, nil, "alice", []string{"1", "2", "3", "4", "5"}, false},
		{"empty means everyone", []string{}, nil, "alice", []string{"1", "2", "3", "4", "5"}, false},
		{"mine is the token's user", []string{"mine"}, nil, "alice", []string{"1"}, true},
		{"mine and bots", []string{"mine", "bots"}, nil, "alice", []string{"1", "3", "4"}, true},
		{"bots only", []string{"bots"}, nil, "alice", []string{"3", "4"}, true},
		{"the defaults", config.DefaultNotifyFor, nil, "alice", []string{"1", "3", "4", "5"}, true},
		{"my_logins win over the token", []string{"mine"}, []string{"Bob"}, "alice", []string{"2"}, false},
		{"unknown user notifies for everyone", []string{"mine"}, nil, "", []string{"1", "2", "3", "4", "5"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			settings := config.NewNotifyConfig()
			settings.ScriptEnabled = true
			settings.Script = "im"
			settings.NotifyFor = tc.notifyFor
			client := newFakeGitHub(repoWith("acme/api", prs(testfixtures.Pending)...))
			client.viewer = tc.viewer
			h := startWith(t, client, []string{"acme/api"},
				map[string]config.NotifyConfig{"acme/api": settings}, func(saved *config.Config) {
					saved.MyLogins = tc.myLogins
					saved.BotLogins = []string{"release-robot[bot]"}
					saved.WatchedLogins = []string{"carol"}
				})
			client.setRepo(repoWith("acme/api", prs()...))
			h.poll()
			got := []string{}
			for _, sent := range h.sent() {
				got = append(got, sent["PR_NUM"])
			}
			slices.Sort(got)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("notified about %v, want %v", got, tc.want)
			}
			if asked := len(client.callsMatching("viewer")) > 0; asked != tc.askViewer {
				t.Errorf("asked GitHub who the user is: %v, want %v", asked, tc.askViewer)
			}
		})
	}
}

func TestAddAndRemoveRepo(t *testing.T) {
	client := newFakeGitHub(repoWith("acme/api", testfixtures.PR(1)))
	h := start(t, client, []string{"acme/api"}, nil)
	client.setRepo(repoWith("Acme/New", testfixtures.PR(3)))

	name, err := h.monitor.AddRepo(context.Background(), "acme/new")
	if err == nil {
		t.Fatalf("adding an unknown spelling should fail until GitHub is asked: %q", name)
	}
	name, err = h.monitor.AddRepo(context.Background(), "Acme/New")
	if err != nil {
		t.Fatal(err)
	}
	if name != "Acme/New" {
		t.Errorf("name = %q, want GitHub's spelling", name)
	}
	if _, err := h.monitor.AddRepo(context.Background(), "acme/api"); err == nil {
		t.Error("adding a monitored repo again should fail")
	}
	saved, _ := config.Load(h.configPath)
	if len(saved.Repos) != 2 {
		t.Errorf("saved repos = %v", saved.Repos)
	}

	if err := h.monitor.RemoveRepo("Acme/New"); err != nil {
		t.Fatal(err)
	}
	if _, found := h.monitor.Repo("Acme/New"); found {
		t.Error("the repo should be gone")
	}
	if err := h.monitor.RemoveRepo("gone/repo"); err == nil {
		t.Error("removing an unmonitored repo should fail")
	}
	saved, _ = config.Load(h.configPath)
	if len(saved.Repos) != 1 || saved.Repos[0] != "acme/api" {
		t.Errorf("saved repos = %v", saved.Repos)
	}
}

func TestMarkSeenAndCollapsed(t *testing.T) {
	client := newFakeGitHub(repoWith("acme/api", testfixtures.PR(1)))
	h := start(t, client, []string{"acme/api"}, nil)
	h.monitor.MarkSeen("acme/api", 1)
	if got := h.monitor.Unseen("acme/api"); len(got) != 0 {
		t.Errorf("unseen = %v", got)
	}
	h.monitor.SetCollapsed("acme", true)
	if got := h.monitor.Collapsed(); len(got) != 1 || got[0] != "acme" {
		t.Errorf("collapsed = %v", got)
	}
	h.monitor.SetCollapsed("acme", false)
	if got := h.monitor.Collapsed(); len(got) != 0 {
		t.Errorf("collapsed = %v", got)
	}
}

func TestAlertOpensACollapsedGroup(t *testing.T) {
	client := newFakeGitHub(repoWith("acme/api", testfixtures.PR(1)))
	h := start(t, client, []string{"acme/api"}, nil)
	h.monitor.SetCollapsed("acme", true)
	client.setRepo(repoWith("acme/api", testfixtures.PR(1, testfixtures.Failing)))
	h.poll()
	if got := h.monitor.Collapsed(); len(got) != 0 {
		t.Errorf("collapsed = %v, want the group opened by the new alert", got)
	}
}

func TestPerformActions(t *testing.T) {
	client := newFakeGitHub(repoWith("acme/api", testfixtures.PR(1)))
	h := start(t, client, []string{"acme/api"}, nil)
	ctx := context.Background()
	squash := models.MergeSquash

	if err := h.monitor.Perform(ctx, "acme/api", 1,
		models.Action{Kind: "merge", Method: &squash, DeleteBranch: true}); err != nil {
		t.Fatal(err)
	}
	h.monitor.WaitIdle()
	if got := client.callsMatching("merge "); len(got) != 1 || got[0] != "merge PR_1 SQUASH" {
		t.Errorf("merge calls = %v", got)
	}
	if got := client.callsMatching("delete "); len(got) != 1 {
		t.Errorf("delete calls = %v, want the branch deleted", got)
	}
	if !h.hasToast("Merged acme/api#1 (squash)") {
		t.Errorf("toasts = %v", h.toasts())
	}

	if err := h.monitor.Perform(ctx, "acme/api", 1, models.Action{Kind: "update"}); err != nil {
		t.Fatal(err)
	}
	if got := client.callsMatching("update "); len(got) != 1 {
		t.Errorf("update calls = %v", got)
	}
	if err := h.monitor.Perform(ctx, "acme/api", 1,
		models.Action{Kind: "auto_merge_on", Method: &squash}); err != nil {
		t.Fatal(err)
	}
	if got := client.callsMatching("auto_on "); len(got) != 1 {
		t.Errorf("auto-merge calls = %v", got)
	}
	if err := h.monitor.Perform(ctx, "acme/api", 99, models.Action{Kind: "update"}); err == nil {
		t.Error("acting on an unknown PR should fail")
	}
}

func TestMergeFailureIsAToast(t *testing.T) {
	client := newFakeGitHub(repoWith("acme/api", testfixtures.PR(1)))
	client.mergeErr = &github.Error{Message: "not allowed"}
	h := start(t, client, []string{"acme/api"}, nil)
	squash := models.MergeSquash
	if err := h.monitor.Perform(context.Background(), "acme/api", 1,
		models.Action{Kind: "merge", Method: &squash}); err != nil {
		t.Fatalf("GitHub failures are toasts, not errors: %v", err)
	}
	if !h.hasToast("Merge failed for acme/api#1: not allowed") {
		t.Errorf("toasts = %v", h.toasts())
	}
}

func TestSetPollInterval(t *testing.T) {
	client := newFakeGitHub(repoWith("acme/api", testfixtures.PR(1)))
	h := start(t, client, []string{"acme/api"}, nil)
	if err := h.monitor.SetPollInterval(120); err != nil {
		t.Fatal(err)
	}
	if h.monitor.Config().PollInterval != 120 {
		t.Errorf("interval = %d", h.monitor.Config().PollInterval)
	}
	saved, _ := config.Load(h.configPath)
	if saved.PollInterval != 120 {
		t.Errorf("saved interval = %d", saved.PollInterval)
	}
	for _, bad := range []int{5, 4000} {
		if err := h.monitor.SetPollInterval(bad); err == nil {
			t.Errorf("%d should be rejected", bad)
		}
	}
}

// A config file the backend couldn't read is the user's to fix: settings
// changes are refused rather than saved over it, which would wipe it.
func TestBrokenConfigIsNeverSavedOver(t *testing.T) {
	client := newFakeGitHub(repoWith("acme/api", testfixtures.PR(1)), repoWith("acme/web"))
	h := start(t, client, []string{"acme/api"}, nil)
	broken := "repos = [\"acme/api\"\n"
	if err := os.WriteFile(h.configPath, []byte(broken), 0o644); err != nil {
		t.Fatal(err)
	}
	h.restart()
	attempts := map[string]func() error{
		"add repo": func() error {
			_, err := h.monitor.AddRepo(context.Background(), "acme/web")
			return err
		},
		"remove repo":   func() error { return h.monitor.RemoveRepo("acme/api") },
		"notifications": func() error { return h.monitor.SaveNotifications("acme/api", config.NewNotifyConfig()) },
		"poll interval": func() error { return h.monitor.SetPollInterval(300) },
	}
	for name, attempt := range attempts {
		err := attempt()
		if err == nil || !strings.Contains(err.Error(), h.configPath) {
			t.Errorf("%s: error = %v, want one naming the config file", name, err)
		}
	}
	if len(h.monitor.Config().Repos) != 0 || h.monitor.Config().PollInterval == 300 {
		t.Errorf("refused changes should not apply in memory either: %+v", h.monitor.Config())
	}
	if text, _ := os.ReadFile(h.configPath); string(text) != broken {
		t.Errorf("config file = %q, want it untouched", text)
	}
}

func TestSaveNotificationsAndTest(t *testing.T) {
	client := newFakeGitHub(repoWith("acme/api", testfixtures.PR(1)))
	h := start(t, client, []string{"acme/api"}, nil)
	settings := config.NewNotifyConfig()
	settings.ScriptEnabled = true
	settings.Script = "im"
	if err := h.monitor.SaveNotifications("acme/api", settings); err != nil {
		t.Fatal(err)
	}
	saved, _ := config.Load(h.configPath)
	if !saved.Notifications["acme/api"].Equal(settings) {
		t.Errorf("saved = %+v", saved.Notifications)
	}
	if err := h.monitor.SaveNotifications("gone/repo", settings); err == nil {
		t.Error("settings for an unmonitored repo should be refused")
	}
	h.monitor.SendTest("acme/api", settings)
	h.monitor.WaitIdle()
	sent := h.sent()
	if len(sent) != 1 || sent[0]["PR_REPO"] != "acme/api" {
		t.Errorf("sent = %v", sent)
	}
	if !h.hasToast("Test script notification sent") {
		t.Errorf("toasts = %v", h.toasts())
	}
	h.monitor.SendTest("acme/api", config.NewNotifyConfig())
	h.monitor.WaitIdle()
	if !h.hasToast("Nothing to send") {
		t.Errorf("toasts = %v", h.toasts())
	}
}

func TestEventsAreEmitted(t *testing.T) {
	client := newFakeGitHub(repoWith("acme/api", testfixtures.PR(1)))
	h := start(t, client, []string{"acme/api"}, nil)
	wanted := map[string]bool{"repo": false, "status": false}
	for _, kind := range h.kinds() {
		if _, tracked := wanted[kind]; tracked {
			wanted[kind] = true
		}
	}
	for kind, seen := range wanted {
		if !seen {
			t.Errorf("no %q event; kinds = %v", kind, h.kinds())
		}
	}
}

func TestDraftToggleAndRerun(t *testing.T) {
	failing := testfixtures.PR(1, func(pr *models.PullRequest) {
		testfixtures.Failing(pr)
		one := testfixtures.CheckRun("ci", "FAILURE")
		one.RunID = models.Ptr(77)
		two := testfixtures.CheckRun("lint", "FAILURE")
		two.RunID = models.Ptr(77) // the same run: asked to re-run once
		three := testfixtures.CheckRun("deploy", "SUCCESS")
		three.RunID = models.Ptr(88)
		pr.Checks = []models.Check{one, two, three}
		pr.ChecksTotal = 3
	})
	client := newFakeGitHub(repoWith("acme/api", failing))
	h := start(t, client, []string{"acme/api"}, nil)

	perform := func(kind string) {
		t.Helper()
		if err := h.monitor.Perform(context.Background(), "acme/api", 1,
			models.Action{Kind: kind}); err != nil {
			t.Fatal(err)
		}
		h.monitor.WaitIdle()
	}
	perform("convert_to_draft")
	perform("mark_ready")
	perform("rerun_checks")

	for _, want := range []string{"draft PR_1", "ready PR_1", "rerun acme/api [77]"} {
		if len(client.callsMatching(want)) != 1 {
			t.Errorf("calls = %v, want %q", client.calls, want)
		}
	}
	for _, want := range []string{"acme/api#1 is a draft again", "acme/api#1 is ready for review",
		"Re-running failed checks for acme/api#1"} {
		if !h.hasToast(want) {
			t.Errorf("toasts = %v, want %q", h.toasts(), want)
		}
	}
}

func TestForceMergeBypassesOnlyWhenAllowed(t *testing.T) {
	failing := func(canBypass bool) models.PullRequest {
		return testfixtures.PR(1, func(pr *models.PullRequest) {
			testfixtures.Failing(pr)
			pr.CanBypass = canBypass
		})
	}
	force := models.Action{Kind: "force_merge", Method: &squash, DeleteBranch: true}

	client := newFakeGitHub(repoWith("acme/api", failing(true)))
	h := start(t, client, []string{"acme/api"}, nil)
	if err := h.monitor.Perform(context.Background(), "acme/api", 1, force); err != nil {
		t.Fatal(err)
	}
	h.monitor.WaitIdle()
	if got := client.callsMatching("merge "); len(got) != 1 || got[0] != "merge PR_1 SQUASH" {
		t.Errorf("merges = %v", got)
	}
	if len(client.callsMatching("delete ")) != 1 {
		t.Error("the branch should be deleted as asked")
	}
	if !h.hasToast("Force-merged acme/api#1 (squash), bypassing branch protection") {
		t.Errorf("toasts = %v", h.toasts())
	}

	// Someone GitHub won't let bypass gets refused before anything is sent.
	denied := newFakeGitHub(repoWith("acme/api", failing(false)))
	h = start(t, denied, []string{"acme/api"}, nil)
	err := h.monitor.Perform(context.Background(), "acme/api", 1, force)
	if err == nil || err.Error() != "GitHub doesn't let you bypass branch protection on acme/api#1" {
		t.Errorf("err = %v", err)
	}
	if len(denied.callsMatching("merge ")) != 0 {
		t.Error("nothing should have been merged")
	}
}

func TestForceMergeRespectsDependencies(t *testing.T) {
	client := newFakeGitHub(repoWith("acme/api",
		testfixtures.PR(1, func(pr *models.PullRequest) { testfixtures.Failing(pr); pr.CanBypass = true }),
		testfixtures.PR(2)))
	h := start(t, client, []string{"acme/api"}, nil)
	addDependency(t, h, 1, "acme/api#2")

	err := h.monitor.Perform(context.Background(), "acme/api", 1,
		models.Action{Kind: "force_merge", Method: &squash})
	if err == nil || err.Error() != "acme/api#1 waits on PRs that haven't merged; remove them to merge it now" {
		t.Errorf("err = %v", err)
	}
	if len(client.callsMatching("merge ")) != 0 {
		t.Error("a PR waiting on others must not be merged")
	}
}

func TestPerformPinnedPassesExpectedHead(t *testing.T) {
	squash := models.MergeSquash
	for _, kind := range []string{"merge", "force_merge"} {
		t.Run(kind, func(t *testing.T) {
			client := newFakeGitHub(repoWith("acme/api",
				testfixtures.PR(1, func(pr *models.PullRequest) { pr.CanBypass = true })))
			h := start(t, client, []string{"acme/api"}, nil)
			if err := h.monitor.PerformPinned(context.Background(), "acme/api", 1,
				models.Action{Kind: kind, Method: &squash}, "abc"); err != nil {
				t.Fatal(err)
			}
			h.monitor.WaitIdle()
			client.mutex.Lock()
			defer client.mutex.Unlock()
			if len(client.mergeHeads) != 1 || client.mergeHeads[0] != "abc" {
				t.Errorf("expected heads = %v, want [abc]", client.mergeHeads)
			}
		})
	}
}

func TestPerformReturnsGitHubFailure(t *testing.T) {
	client := newFakeGitHub(repoWith("acme/api", testfixtures.PR(1)))
	client.mergeErr = &github.Error{Message: "not allowed"}
	h := start(t, client, []string{"acme/api"}, nil)
	squash := models.MergeSquash
	err := h.monitor.PerformPinned(context.Background(), "acme/api", 1,
		models.Action{Kind: "merge", Method: &squash}, "abc")
	if err == nil || err.Error() != "not allowed" {
		t.Fatalf("err = %v, want GitHub's failure", err)
	}
	if h.hasToast("Merge failed for acme/api#1: not allowed") {
		t.Errorf("perform should not toast the failure: %v", h.toasts())
	}
}
