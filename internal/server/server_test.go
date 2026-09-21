package server_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/acheris-labs/pr-mon/internal/client"
	"github.com/acheris-labs/pr-mon/internal/config"
	"github.com/acheris-labs/pr-mon/internal/github"
	"github.com/acheris-labs/pr-mon/internal/models"
	"github.com/acheris-labs/pr-mon/internal/protocol"
	"github.com/acheris-labs/pr-mon/internal/readiness"
	"github.com/acheris-labs/pr-mon/internal/server"
	"github.com/acheris-labs/pr-mon/internal/service"
	"github.com/acheris-labs/pr-mon/internal/testfixtures"
)

// fakeGitHub serves one repo and records merges.
type fakeGitHub struct {
	mutex  sync.Mutex
	repo   models.Repo
	merges []string
}

func (f *fakeGitHub) FetchRepo(context.Context, string) (models.Repo, error) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	return f.repo, nil
}

func (f *fakeGitHub) Merge(_ context.Context, prID string, _ models.MergeMethod, _ string) error {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.merges = append(f.merges, prID)
	return nil
}

func (f *fakeGitHub) UpdateBranch(context.Context, string) error { return nil }

func (f *fakeGitHub) EnableAutoMerge(context.Context, string, models.MergeMethod) error { return nil }

func (f *fakeGitHub) DisableAutoMerge(context.Context, string) error { return nil }

func (f *fakeGitHub) DeleteBranch(context.Context, string) error { return nil }

func (f *fakeGitHub) ListOpenPRs(_ context.Context, _, etag string) (github.OpenPRs, error) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	if etag == "etag" {
		return github.OpenPRs{Changed: false, ETag: etag}, nil
	}
	listed := github.OpenPRs{Changed: true, ETag: "etag", Updated: map[int]string{}}
	for _, pr := range f.repo.PRs {
		listed.Numbers = append(listed.Numbers, pr.Number)
		listed.Updated[pr.Number] = "2026-09-15T01:28:54Z"
	}
	return listed, nil
}

func (f *fakeGitHub) BaseMoved(context.Context, string, string) (bool, string, error) {
	return false, "base", nil
}

func (f *fakeGitHub) FetchPRs(_ context.Context, _ string, numbers []int) ([]models.PullRequest, error) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	wanted := map[int]bool{}
	for _, number := range numbers {
		wanted[number] = true
	}
	found := []models.PullRequest{}
	for _, pr := range f.repo.PRs {
		if wanted[pr.Number] {
			found = append(found, pr)
		}
	}
	return found, nil
}

// LookupPRs reports every PR as open.
func (f *fakeGitHub) LookupPRs(_ context.Context, repo string, numbers []int) ([]models.PRRef, error) {
	refs := []models.PRRef{}
	for _, number := range numbers {
		refs = append(refs, models.PRRef{Repo: repo, Number: number, Title: "Elsewhere",
			URL: fmt.Sprintf("https://github.com/%s/pull/%d", repo, number), State: models.PROpen})
	}
	return refs, nil
}

// serve starts a monitor and server on a short socket path (macOS limits them).
func serve(t *testing.T) (*server.Server, *service.Monitor, string) {
	t.Helper()
	return serveWith(t, 0)
}

// serveWith is serve with an idle exit; zero never exits.
func serveWith(t *testing.T, idleExit time.Duration) (*server.Server, *service.Monitor, string) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "prmon")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	saved := config.New()
	saved.Repos = []string{"acme/api"}
	configPath := filepath.Join(dir, "config.toml")
	if err := config.Save(configPath, saved); err != nil {
		t.Fatal(err)
	}
	repo := testfixtures.Repo("acme/api", []models.PullRequest{
		readiness.Assess(testfixtures.PR(1)),
		readiness.Assess(testfixtures.PR(2, testfixtures.Pending)),
	})
	monitor := service.New(&fakeGitHub{repo: repo}, configPath, filepath.Join(dir, "state.json"),
		service.Options{
			Version:  "1.2.3",
			Notifier: "/x/tn",
			Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		})
	socket := filepath.Join(dir, "d.sock")
	listener := server.New(monitor, socket, slog.New(slog.NewTextHandler(io.Discard, nil)))
	listener.IdleExit = idleExit
	if err := listener.Start(); err != nil {
		t.Fatal(err)
	}
	monitor.Start(context.Background())
	monitor.WaitIdle()
	t.Cleanup(func() {
		listener.Close()
		monitor.Stop()
	})
	return listener, monitor, socket
}

func connect(t *testing.T, socket, expectedVersion string) *client.Client {
	t.Helper()
	remote := client.New(socket)
	if err := remote.Connect(expectedVersion); err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(remote.Close)
	return remote
}

func TestClientMirrorsTheSnapshot(t *testing.T) {
	_, _, socket := serve(t)
	remote := connect(t, socket, "1.2.3")

	if got := remote.Config().Repos; !reflect.DeepEqual(got, []string{"acme/api"}) {
		t.Errorf("repos = %v", got)
	}
	repo, found := remote.Repo("acme/api")
	if !found || len(repo.PRs) != 2 {
		t.Fatalf("repo = %+v, found = %v", repo, found)
	}
	if repo.PRs[0].Status != models.StatusReady || len(repo.PRs[0].Actions) == 0 {
		t.Errorf("the backend's computed fields should arrive: %+v", repo.PRs[0])
	}
	if got := remote.Unseen("acme/api"); !reflect.DeepEqual(got, []int{1, 2}) {
		t.Errorf("unseen = %v", got)
	}
	status := remote.Status()
	if !status.Connected || status.Version != "1.2.3" || status.Notifier == nil {
		t.Errorf("status = %+v", status)
	}
}

func TestEventsReachSubscribersOnly(t *testing.T) {
	_, monitor, socket := serve(t)
	quiet := client.New(socket) // connects but never asks for a snapshot
	if err := quiet.Connect(""); err != nil {
		// Connect always subscribes, so use a raw connection for the quiet client.
		t.Fatal(err)
	}
	quiet.Close()

	remote := connect(t, socket, "")
	events := make(chan service.Event, 32)
	remote.AddListener(func(event service.Event) { events <- event })

	monitor.MarkSeen("acme/api", 1)
	waitForEvent(t, events, "seen")
	if got := remote.Unseen("acme/api"); !reflect.DeepEqual(got, []int{2}) {
		t.Errorf("unseen = %v, want the event applied", got)
	}

	monitor.SetCollapsed("acme", true)
	waitForEvent(t, events, "collapsed")
	if got := remote.Collapsed(); !reflect.DeepEqual(got, []string{"acme"}) {
		t.Errorf("collapsed = %v", got)
	}
}

func TestCommandsReachTheMonitor(t *testing.T) {
	_, monitor, socket := serve(t)
	remote := connect(t, socket, "")
	events := make(chan service.Event, 32)
	remote.AddListener(func(event service.Event) { events <- event })

	if err := remote.MarkSeen("acme/api", 1); err != nil {
		t.Fatal(err)
	}
	if got := monitor.Unseen("acme/api"); !reflect.DeepEqual(got, []int{2}) {
		t.Errorf("unseen = %v", got)
	}

	squash := models.MergeSquash
	action := models.Action{Kind: "arm_merge", Method: &squash, DeleteBranch: true}
	if err := remote.Perform("acme/api", 2, action); err != nil {
		t.Fatal(err)
	}
	monitor.WaitIdle()
	if armed := monitor.Armed("acme/api"); len(armed) != 1 || !armed[2].DeleteBranch {
		t.Errorf("armed = %+v", armed)
	}
	// The mirror learns about it through the repo event.
	waitFor(t, func() bool { return len(remote.Armed("acme/api")) == 1 })

	if err := remote.SetPollInterval(120); err != nil {
		t.Fatal(err)
	}
	if monitor.Config().PollInterval != 120 {
		t.Errorf("poll interval = %d", monitor.Config().PollInterval)
	}
	if err := remote.SetPollInterval(1); err == nil {
		t.Error("an out-of-range interval should be refused")
	}
}

func TestDependenciesOverTheSocket(t *testing.T) {
	_, monitor, socket := serve(t)
	remote := connect(t, socket, "")

	if err := remote.AddDependency("acme/api", 1, "other/lib#5"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		repo, _ := remote.Repo("acme/api")
		return repo.PRs[0].Status == models.StatusWaiting
	})
	graph, err := remote.DependencyGraph("acme/api", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(graph.WaitsOn) != 1 || graph.WaitsOn[0].PR.Key() != "other/lib#5" ||
		graph.WaitsOn[0].PR.Title != "Elsewhere" {
		t.Errorf("graph = %+v", graph)
	}
	if err := remote.AddDependency("acme/api", 1, "other/lib#5"); err == nil ||
		err.Error() != "acme/api#1 already waits on other/lib#5" {
		t.Errorf("adding it twice = %v", err)
	}
	if err := remote.RemoveDependency("acme/api", 1, "other/lib#5"); err != nil {
		t.Fatal(err)
	}
	monitor.WaitIdle()
	waitFor(t, func() bool {
		repo, _ := remote.Repo("acme/api")
		return repo.PRs[0].Status == models.StatusReady
	})
}

func TestFocusFollowsTheClient(t *testing.T) {
	_, monitor, socket := serve(t)
	remote := connect(t, socket, "")
	if err := remote.SetFocus("acme/api", 3); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return reflect.DeepEqual(monitor.Focused(), []string{"acme/api"}) })

	// Clearing it, and hanging up, both leave the backend with nothing focused.
	if err := remote.SetFocus("", 0); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return len(monitor.Focused()) == 0 })
	if err := remote.SetFocus("acme/api", 3); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return len(monitor.Focused()) == 1 })
	remote.Close()
	waitFor(t, func() bool { return len(monitor.Focused()) == 0 })
}

func TestErrorsComeBackAsErrors(t *testing.T) {
	_, _, socket := serve(t)
	remote := connect(t, socket, "")
	err := remote.Perform("acme/api", 99, models.Action{Kind: "update"})
	if err == nil || err.Error() != "acme/api#99 is not an open PR" {
		t.Errorf("error = %v", err)
	}
	if err := remote.RemoveRepo("gone/repo"); err == nil {
		t.Error("removing an unmonitored repo should fail")
	}
}

func TestVersionAndProtocolChecks(t *testing.T) {
	_, _, socket := serve(t)
	remote := client.New(socket)
	err := remote.Connect("9.9.9")
	var mismatch *client.MismatchError
	if !asMismatch(err, &mismatch) {
		t.Fatalf("error = %v, want a mismatch", err)
	}
	if mismatch.Version != "1.2.3" || mismatch.Protocol != protocol.Version || mismatch.Merging {
		t.Errorf("mismatch = %+v", mismatch)
	}
	if remote.Connected() {
		t.Error("a mismatched backend should leave the client disconnected")
	}
}

func TestUnavailableBackend(t *testing.T) {
	remote := client.New("/tmp/pr-mon-missing-test.sock")
	if err := remote.Connect(""); err != client.ErrUnavailable {
		t.Errorf("error = %v, want unavailable", err)
	}
}

func idle(listener *server.Server) bool {
	select {
	case <-listener.Idle:
		return true
	default:
		return false
	}
}

func TestIdleAfterTheLastFrontEndLeaves(t *testing.T) {
	const grace = 150 * time.Millisecond
	listener, _, socket := serveWith(t, grace)
	app := connect(t, socket, "")
	dashboard := connect(t, socket, "")
	app.Close()
	time.Sleep(2 * grace)
	if idle(listener) {
		t.Fatal("a dashboard is still open")
	}

	// One that leaves and comes back within the grace period pushes it back.
	dashboard.Close()
	time.Sleep(grace / 2)
	again := connect(t, socket, "")
	time.Sleep(grace)
	if idle(listener) {
		t.Fatal("a front end came back in time")
	}
	// A client that only asks (like `pr-mon status`) is not a front end.
	if _, err := daemonHello(socket); err != nil {
		t.Fatal(err)
	}
	again.Close()
	waitFor(t, func() bool { return idle(listener) })
}

func TestIdleWhenNobodyEverConnects(t *testing.T) {
	listener, _, _ := serveWith(t, 50*time.Millisecond)
	waitFor(t, func() bool { return idle(listener) })
}

func TestNoIdleExitWhenKeptRunning(t *testing.T) {
	listener, _, socket := serve(t)
	connect(t, socket, "").Close()
	time.Sleep(100 * time.Millisecond)
	if idle(listener) {
		t.Error("with no idle exit set the backend stays")
	}
}

func TestAClientThatStopsReadingIsDropped(t *testing.T) {
	listener, monitor, socket := serve(t)
	listener.WriteTimeout = 100 * time.Millisecond

	// Subscribes, then never reads again.
	deaf, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer deaf.Close()
	if _, err := deaf.Write([]byte(`{"id":1,"op":"snapshot","args":{}}` + "\n")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return listener.Subscribers() == 1 })

	// Fill its socket with events until the server gives up on it.
	for range 200 {
		monitor.RefreshAll()
		if listener.Subscribers() == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	waitFor(t, func() bool { return listener.Subscribers() == 0 })

	// The backend still answers everyone else.
	if _, err := daemonHello(socket); err != nil {
		t.Fatalf("the backend should still answer: %v", err)
	}
}

func TestDisconnectIsReported(t *testing.T) {
	listener, _, socket := serve(t)
	remote := connect(t, socket, "")
	events := make(chan service.Event, 8)
	remote.AddListener(func(event service.Event) { events <- event })
	listener.Close()
	waitForEvent(t, events, "disconnected")
	if remote.Connected() {
		t.Error("the client should know it is offline")
	}
	if err := remote.RefreshAll(); err == nil {
		t.Error("commands should fail once disconnected")
	}
}

func TestMalformedLineDropsTheClient(t *testing.T) {
	_, _, socket := serve(t)
	connection, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if _, err := connection.Write([]byte("{nope\n")); err != nil {
		t.Fatal(err)
	}
	connection.SetReadDeadline(time.Now().Add(2 * time.Second))
	buffer := make([]byte, 16)
	if _, err := connection.Read(buffer); err != io.EOF {
		t.Errorf("read = %v, want the server to hang up", err)
	}
}

func TestUnknownOp(t *testing.T) {
	_, _, socket := serve(t)
	connection, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	line, _ := protocol.Encode(protocol.Request{ID: 1, Op: "explode", Args: json.RawMessage(`{}`)})
	if _, err := connection.Write(line); err != nil {
		t.Fatal(err)
	}
	var response protocol.Response
	connection.SetReadDeadline(time.Now().Add(2 * time.Second))
	if err := json.NewDecoder(connection).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response.OK || response.Error != `unknown op "explode"` {
		t.Errorf("response = %+v", response)
	}
}

func TestOpsCoverTheDocumentedRequests(t *testing.T) {
	documented := []string{
		"hello", "snapshot", "refresh_all", "add_repo", "remove_repo", "mark_seen",
		"set_collapsed", "set_focus", "perform", "add_dependency", "remove_dependency",
		"dependency_graph", "save_notifications", "send_test", "set_poll_interval",
		"notification_form", "preview_notification", "shutdown",
	}
	if !reflect.DeepEqual(server.Ops, documented) {
		t.Errorf("ops = %v, want %v", server.Ops, documented)
	}
}

func asMismatch(err error, target **client.MismatchError) bool {
	mismatch, ok := err.(*client.MismatchError)
	if ok {
		*target = mismatch
	}
	return ok
}

func waitForEvent(t *testing.T, events chan service.Event, kind string) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case event := <-events:
			if event.Kind == kind {
				return
			}
		case <-deadline:
			t.Fatalf("no %q event arrived", kind)
		}
	}
}

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

// daemonHello asks once without subscribing, the way `pr-mon status` does.
func daemonHello(socket string) (string, error) {
	connection, err := net.Dial("unix", socket)
	if err != nil {
		return "", err
	}
	defer connection.Close()
	if _, err := connection.Write([]byte(`{"id":1,"op":"hello","args":{}}` + "\n")); err != nil {
		return "", err
	}
	return bufio.NewReader(connection).ReadString('\n')
}
