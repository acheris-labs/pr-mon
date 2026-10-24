package handler

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/acheris-labs/pr-mon/internal/twoway"
)

// The tests run this test binary as the handler: with PRMON_FAKE_HANDLER set
// it plays that scenario on stdin and stdout instead of running the tests.
func TestMain(m *testing.M) {
	if scenario := os.Getenv("PRMON_FAKE_HANDLER"); scenario != "" {
		os.Exit(fakeHandler(scenario, os.Getenv("PRMON_FAKE_DIR")))
	}
	os.Exit(m.Run())
}

// fakeHandler records its start time, pid and every request line in dir, and
// answers as the scenario says:
//
//	good       answers everything; "tap me" posts get a tap; set ids fail
//	slowhello  good, but answers hello after 500 ms
//	mismatch   answers hello with protocol 2
//	silent     answers hello, then nothing
//	stubborn   silent, and keeps running after stdin closes
//	crash      writes "boom" to stderr and exits 1 at once
//	exitafter  answers hello, then exits 3 on the next request
//	reverse    answers two posts in reverse order, each with its title as id
//	junk       good, but writes junk to stdout before each post reply
//	loud       writes a 1000-rune stderr line and exits 1 at once
//	emptyhello answers hello with an empty result
//	nullhello  answers hello with a null result
//	brief      answers hello, stays up for PRMON_FAKE_UP, records the time
//	           in exits and exits 4
func fakeHandler(scenario, dir string) int {
	appendTo(dir, "starts", strconv.FormatInt(time.Now().UnixNano(), 10)+"\n")
	if err := os.WriteFile(filepath.Join(dir, "pid"), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		return 2
	}
	switch scenario {
	case "crash":
		fmt.Fprintln(os.Stderr, "boom")
		return 1
	case "loud":
		fmt.Fprintln(os.Stderr, strings.Repeat("é", 1000))
		return 1
	}
	fmt.Fprintln(os.Stderr, "fake handler says hi")
	out := json.NewEncoder(os.Stdout)
	send := func(v any) { _ = out.Encode(v) }
	ok := func(id int64, result any) { send(map[string]any{"id": id, "ok": true, "result": result}) }
	fail := func(id int64, code string) {
		send(map[string]any{"id": id, "ok": false, "error": map[string]string{"code": code, "message": "no " + code}})
	}
	if scenario == "good" {
		// Sent before hello's reply, so the runner must skip it.
		send(map[string]any{"event": "tap", "args": map[string]string{"message_id": "early", "key": "k0", "receipt": "r-0"}})
	}

	type fakeRequest struct {
		ID   int64  `json:"id"`
		Op   string `json:"op"`
		Args struct {
			MessageID string `json:"message_id"`
			Message   struct {
				Title string `json:"title"`
			} `json:"message"`
		} `json:"args"`
	}
	var held []fakeRequest
	in := bufio.NewScanner(os.Stdin)
	for in.Scan() {
		appendTo(dir, "requests", in.Text()+"\n")
		var req fakeRequest
		if err := json.Unmarshal(in.Bytes(), &req); err != nil {
			return 2
		}
		if req.Op == "hello" {
			protocol := Protocol
			switch scenario {
			case "mismatch":
				protocol = 2
			case "slowhello":
				time.Sleep(500 * time.Millisecond)
			case "emptyhello":
				ok(req.ID, map[string]any{})
				continue
			case "nullhello":
				ok(req.ID, nil)
				continue
			}
			ok(req.ID, map[string]any{"protocol": protocol, "name": "Fake"})
			if scenario == "brief" {
				up, _ := time.ParseDuration(os.Getenv("PRMON_FAKE_UP"))
				time.Sleep(up)
				appendTo(dir, "exits", strconv.FormatInt(time.Now().UnixNano(), 10)+"\n")
				return 4
			}
			continue
		}
		switch scenario {
		case "silent", "stubborn":
			continue
		case "exitafter":
			fmt.Fprintln(os.Stderr, "bye")
			return 3
		case "reverse":
			if held = append(held, req); len(held) == 2 {
				ok(held[1].ID, map[string]string{"message_id": held[1].Args.Message.Title})
				ok(held[0].ID, map[string]string{"message_id": held[0].Args.Message.Title})
			}
			continue
		case "junk":
			if req.Op == "post" {
				fmt.Println("not json at all")
				fmt.Println("[1,2,3]")
				fmt.Println(strings.Repeat("x", 100<<10))
				fmt.Println(strings.Repeat("y", 2<<20))
				fmt.Println(`{"id":999,"ok":true,"result":{}}`)
				fmt.Println(`{"event":"wave","args":{}}`)
			}
		}
		switch req.Op {
		case "post":
			id := fmt.Sprintf("msg-%d", req.ID)
			ok(req.ID, map[string]string{"message_id": id})
			if req.Args.Message.Title == "tap me" {
				send(map[string]any{"event": "tap", "args": map[string]string{"message_id": id, "key": "k1", "receipt": "r-1"}})
			}
		case "delete":
			switch req.Args.MessageID {
			case "gone-id":
				fail(req.ID, "gone")
			case "old-id":
				fail(req.ID, "too_old")
			case "odd-id":
				fail(req.ID, "odd")
			default:
				ok(req.ID, struct{}{})
			}
		default:
			ok(req.ID, struct{}{})
		}
	}
	if scenario == "stubborn" {
		time.Sleep(time.Hour)
	}
	return 0
}

func appendTo(dir, name, text string) {
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(text)
}

// lockedBuffer is a log destination the tests can read while the runner writes.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type harness struct {
	t       *testing.T
	runner  *Runner
	dir     string
	log     *lockedBuffer
	changes chan twoway.Availability
}

// start runs the runner on the fake handler playing scenario, with short
// timeouts that adjust may change. Cleanup closes it and checks that no
// goroutine is left behind.
func start(t *testing.T, scenario string, adjust func(*Settings)) *harness {
	t.Helper()
	h := &harness{t: t, dir: t.TempDir(), log: &lockedBuffer{}, changes: make(chan twoway.Availability, 64)}
	t.Setenv("PRMON_FAKE_HANDLER", scenario)
	t.Setenv("PRMON_FAKE_DIR", h.dir)
	// A -race binary sleeps a second before exiting; the fake shouldn't.
	t.Setenv("GORACE", strings.TrimSpace(os.Getenv("GORACE")+" atexit_sleep_ms=0"))
	settings := Settings{
		Argv:           []string{os.Args[0]},
		Version:        "9.9.9",
		Log:            slog.New(slog.NewTextHandler(h.log, &slog.HandlerOptions{Level: slog.LevelDebug})),
		OnChange:       func(a twoway.Availability) { h.changes <- a },
		HelloTimeout:   2 * time.Second,
		RequestTimeout: 2 * time.Second,
		FirstBackoff:   50 * time.Millisecond,
		MaxBackoff:     200 * time.Millisecond,
		ShutdownGrace:  500 * time.Millisecond,
		ReceiveWait:    200 * time.Millisecond,
	}
	if adjust != nil {
		adjust(&settings)
	}
	before := runtime.NumGoroutine()
	h.runner = Start(context.Background(), settings)
	t.Cleanup(func() {
		h.runner.Close()
		for deadline := time.Now().Add(2 * time.Second); runtime.NumGoroutine() > before; {
			if time.Now().After(deadline) {
				stacks := make([]byte, 1<<20)
				t.Errorf("goroutines left after Close: %d, before %d\n%s",
					runtime.NumGoroutine(), before, stacks[:runtime.Stack(stacks, true)])
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
	return h
}

// next returns the next availability change.
func (h *harness) next() twoway.Availability {
	h.t.Helper()
	select {
	case a := <-h.changes:
		return a
	case <-time.After(5 * time.Second):
		h.t.Fatal("no availability change")
		return twoway.Availability{}
	}
}

// ready waits for the handler to become ready.
func (h *harness) ready() {
	h.t.Helper()
	if a := h.next(); a != (twoway.Availability{Ready: true, Name: "Fake"}) {
		h.t.Fatalf("got %+v, want ready", a)
	}
}

func (h *harness) read(name string) string {
	data, _ := os.ReadFile(filepath.Join(h.dir, name))
	return string(data)
}

func (h *harness) starts() []time.Time { return h.times("starts") }

// times reads the times the fake recorded in a file.
func (h *harness) times(name string) []time.Time {
	var out []time.Time
	for _, field := range strings.Fields(h.read(name)) {
		n, err := strconv.ParseInt(field, 10, 64)
		if err != nil {
			h.t.Fatal(err)
		}
		out = append(out, time.Unix(0, n))
	}
	return out
}

// exited says whether the last fake handler started is gone.
func (h *harness) exited() bool {
	pid, err := strconv.Atoi(h.read("pid"))
	if err != nil {
		h.t.Fatal(err)
	}
	return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}

// waitFor polls until cond holds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !cond(); {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestHelloMakesItReady(t *testing.T) {
	h := start(t, "slowhello", nil)
	if got := h.runner.Name(); got != "Two-way" {
		t.Errorf("Name before hello = %q, want Two-way", got)
	}
	h.ready()
	if got := h.runner.Name(); got != "Fake" {
		t.Errorf("Name = %q, want Fake", got)
	}
}

func TestProtocolMismatchIsNotRetried(t *testing.T) {
	h := start(t, "mismatch", nil)
	want := twoway.Availability{Why: "Two-way handler speaks protocol 2; pr-mon needs 1"}
	if got := h.next(); got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	time.Sleep(5 * 200 * time.Millisecond)
	if n := len(h.starts()); n != 1 {
		t.Errorf("started %d times, want 1", n)
	}
	if !h.exited() {
		t.Error("the handler is still running")
	}
	if _, err := h.runner.Post(context.Background(), twoway.Message{}); !errors.Is(err, twoway.ErrUnavailable) {
		t.Errorf("Post: %v, want ErrUnavailable", err)
	}
}

func TestPostEditDeleteAck(t *testing.T) {
	h := start(t, "good", nil)
	h.ready()
	ctx := context.Background()
	message := twoway.Message{
		Status: "READY", Title: "acme/api #12 · Fix & ship",
		Lines:   []string{"octocat · fix → main"},
		Buttons: [][]twoway.Button{{{Label: "Squash & merge", Key: "xx2ev6d7"}, {Label: "Open", URL: "https://x/?a=1&b=2"}}},
	}
	edited := twoway.Message{Status: "MERGED", Title: "acme/api #12 · Fix & ship"}

	id, err := h.runner.Post(ctx, message)
	if err != nil || id != "msg-2" {
		t.Fatalf("Post = %q, %v; want msg-2", id, err)
	}
	if err := h.runner.Edit(ctx, id, edited); err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if err := h.runner.Delete(ctx, id); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := h.runner.Ack(ctx, twoway.Tap{MessageID: id, Key: "xx2ev6d7", Receipt: "r-9"}); err != nil {
		t.Fatalf("Ack: %v", err)
	}

	encode := func(m twoway.Message) string {
		data, err := marshal(encodeMessage(m))
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	want := strings.Join([]string{
		`{"id":1,"op":"hello","args":{"protocol":1,"app":"pr-mon","version":"9.9.9"}}`,
		`{"id":2,"op":"post","args":{"message":` + encode(message) + `}}`,
		`{"id":3,"op":"edit","args":{"message_id":"msg-2","message":` + encode(edited) + `}}`,
		`{"id":4,"op":"delete","args":{"message_id":"msg-2"}}`,
		`{"id":5,"op":"ack","args":{"receipt":"r-9"}}`,
	}, "\n") + "\n"
	if got := h.read("requests"); got != want {
		t.Errorf("requests:\n%s\nwant:\n%s", got, want)
	}
}

func TestErrorCodesMapToErrors(t *testing.T) {
	h := start(t, "good", nil)
	h.ready()
	tests := []struct {
		id         string
		gone, old  bool
		wantErrNil bool
	}{
		{id: "gone-id", gone: true},
		{id: "old-id", old: true},
		{id: "odd-id"},
		{id: "fine-id", wantErrNil: true},
	}
	for _, test := range tests {
		t.Run(test.id, func(t *testing.T) {
			err := h.runner.Delete(context.Background(), test.id)
			if (err == nil) != test.wantErrNil {
				t.Fatalf("err = %v", err)
			}
			if errors.Is(err, twoway.ErrGone) != test.gone || errors.Is(err, twoway.ErrTooOld) != test.old {
				t.Errorf("err = %v; want gone %v, too old %v", err, test.gone, test.old)
			}
			if errors.Is(err, twoway.ErrUnavailable) {
				t.Errorf("err = %v; an error reply isn't ErrUnavailable", err)
			}
		})
	}
}

func TestRepliesOutOfOrder(t *testing.T) {
	h := start(t, "reverse", nil)
	h.ready()
	titles := []string{"first", "second"}
	got := make([]string, len(titles))
	errs := make([]error, len(titles))
	var wg sync.WaitGroup
	for i, title := range titles {
		wg.Go(func() { got[i], errs[i] = h.runner.Post(context.Background(), twoway.Message{Title: title}) })
	}
	wg.Wait()
	for i, title := range titles {
		if errs[i] != nil || got[i] != title {
			t.Errorf("Post %q = %q, %v", title, got[i], errs[i])
		}
	}
}

func TestRequestTimesOut(t *testing.T) {
	timeout := 200 * time.Millisecond
	h := start(t, "silent", func(s *Settings) { s.RequestTimeout = timeout })
	h.ready()
	began := time.Now()
	_, err := h.runner.Post(context.Background(), twoway.Message{})
	elapsed := time.Since(began)
	if err == nil || errors.Is(err, twoway.ErrUnavailable) {
		t.Errorf("err = %v; want a timeout that isn't ErrUnavailable", err)
	}
	if elapsed < timeout || elapsed > timeout+time.Second {
		t.Errorf("took %v; want about %v", elapsed, timeout)
	}
}

func TestUnavailableBeforeHello(t *testing.T) {
	h := start(t, "slowhello", nil)
	began := time.Now()
	_, err := h.runner.Post(context.Background(), twoway.Message{})
	if !errors.Is(err, twoway.ErrUnavailable) {
		t.Errorf("err = %v, want ErrUnavailable", err)
	}
	if elapsed := time.Since(began); elapsed > 250*time.Millisecond {
		t.Errorf("took %v; want at once", elapsed)
	}
	h.ready()
}

func TestTapsAndReceive(t *testing.T) {
	wait := 200 * time.Millisecond
	h := start(t, "good", func(s *Settings) { s.ReceiveWait = wait })
	h.ready()
	ctx := context.Background()

	// The fake's tap sent before hello's reply was skipped.
	began := time.Now()
	taps, err := h.runner.Receive(ctx)
	if err != nil || len(taps) != 0 {
		t.Errorf("Receive = %v, %v; want none", taps, err)
	}
	if elapsed := time.Since(began); elapsed < wait {
		t.Errorf("Receive returned after %v; want it to wait %v", elapsed, wait)
	}

	id, err := h.runner.Post(ctx, twoway.Message{Title: "tap me"})
	if err != nil {
		t.Fatal(err)
	}
	taps, err = h.runner.Receive(ctx)
	want := []twoway.Tap{{MessageID: id, Key: "k1", Receipt: "r-1"}}
	if err != nil || len(taps) != 1 || taps[0] != want[0] {
		t.Errorf("Receive = %v, %v; want %v", taps, err, want)
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := h.runner.Receive(cancelled); !errors.Is(err, context.Canceled) {
		t.Errorf("Receive with a cancelled context: %v", err)
	}
}

func TestJunkOnStdoutIsSkipped(t *testing.T) {
	h := start(t, "junk", nil)
	h.ready()
	for i := range 2 {
		if _, err := h.runner.Post(context.Background(), twoway.Message{}); err != nil {
			t.Fatalf("Post %d: %v", i, err)
		}
	}
	log := h.log.String()
	for _, want := range []string{"can't read", "over 1 MiB", "nothing is waiting"} {
		if !strings.Contains(log, want) {
			t.Errorf("log has no %q:\n%.2000s", want, log)
		}
	}
}

func TestStderrIsLogged(t *testing.T) {
	h := start(t, "good", nil)
	h.ready()
	waitFor(t, "the stderr line in the log", func() bool {
		return strings.Contains(h.log.String(), `level=INFO msg="fake handler says hi" from="two-way handler"`)
	})
}

func TestCrashLoopBacksOff(t *testing.T) {
	first, most := 50*time.Millisecond, 200*time.Millisecond
	h := start(t, "crash", func(s *Settings) { s.FirstBackoff, s.MaxBackoff = first, most })
	waitFor(t, "seven starts", func() bool { return len(h.starts()) >= 7 })

	starts := h.starts()
	pauses := []time.Duration{first, 2 * first, most, most, most, most}
	for i, pause := range pauses {
		gap := starts[i+1].Sub(starts[i])
		if gap < pause {
			t.Errorf("start %d came %v after the one before; want at least %v", i+1, gap, pause)
		}
	}
	// Doubling without the cap would make the last gap 1.6 s.
	if gap := starts[6].Sub(starts[5]); gap > most+800*time.Millisecond {
		t.Errorf("last gap %v; want it capped near %v", gap, most)
	}

	got := h.next()
	if got.Ready || !strings.Contains(got.Why, "exit status 1") || !strings.Contains(got.Why, "boom") {
		t.Errorf("got %+v; want not ready with the exit status and the last stderr line", got)
	}
	select {
	case again := <-h.changes:
		t.Errorf("OnChange called again with %+v for the same failure", again)
	default:
	}
}

func TestRestartAfterExit(t *testing.T) {
	h := start(t, "exitafter", nil)
	h.ready()
	if _, err := h.runner.Post(context.Background(), twoway.Message{}); !errors.Is(err, twoway.ErrUnavailable) {
		t.Errorf("Post: %v, want ErrUnavailable", err)
	}
	down := h.next()
	if down.Ready || !strings.Contains(down.Why, "exit status 3") || !strings.Contains(down.Why, "bye") {
		t.Errorf("got %+v; want not ready with exit status 3 and bye", down)
	}
	h.ready()
}

func TestCloseFailsWaitingRequests(t *testing.T) {
	grace := 2 * time.Second
	h := start(t, "silent", func(s *Settings) { s.RequestTimeout, s.ShutdownGrace = 10*time.Second, grace })
	h.ready()
	done := make(chan error, 1)
	go func() {
		_, err := h.runner.Post(context.Background(), twoway.Message{})
		done <- err
	}()
	waitFor(t, "the post to reach the handler", func() bool { return strings.Contains(h.read("requests"), `"post"`) })

	// The fake exits when stdin closes, so Close needn't wait out the grace.
	began := time.Now()
	h.runner.Close()
	if elapsed := time.Since(began); elapsed > grace/2 {
		t.Errorf("Close took %v", elapsed)
	}
	select {
	case err := <-done:
		if !errors.Is(err, twoway.ErrUnavailable) {
			t.Errorf("Post: %v, want ErrUnavailable", err)
		}
	case <-time.After(time.Second):
		t.Error("Post still waiting after Close")
	}
	if !h.exited() {
		t.Error("the handler is still running")
	}
	h.runner.Close() // again: no effect
}

func TestCloseKillsAHandlerThatIgnoresStdin(t *testing.T) {
	grace := 300 * time.Millisecond
	h := start(t, "stubborn", func(s *Settings) { s.ShutdownGrace = grace })
	h.ready()
	began := time.Now()
	h.runner.Close()
	elapsed := time.Since(began)
	if elapsed < grace || elapsed > grace+time.Second {
		t.Errorf("Close took %v; want a little over %v", elapsed, grace)
	}
	if !h.exited() {
		t.Error("the handler is still running")
	}
}

func TestMissingCommand(t *testing.T) {
	path := filepath.Join(t.TempDir(), "no-such-handler")
	h := start(t, "good", func(s *Settings) { s.Argv = []string{path} })
	down := h.next()
	if down.Ready || !strings.Contains(down.Why, path) {
		t.Fatalf("got %+v; want not ready, naming %s", down, path)
	}
	// It keeps trying, so it starts once the command exists.
	if err := os.Symlink(os.Args[0], path); err != nil {
		t.Fatal(err)
	}
	h.ready()
}

// The reader may queue a tap as soon as hello is answered, before the
// supervisor makes the session current; Receive holds it until then.
func TestTapsWaitForTheSessionToBeCurrent(t *testing.T) {
	r := &Runner{settings: Settings{ReceiveWait: 100 * time.Millisecond}, tapped: make(chan struct{}, 1)}
	tap := twoway.Tap{MessageID: "m", Key: "k", Receipt: "r"}
	r.addTap(tap)
	ctx := context.Background()
	if taps, err := r.Receive(ctx); err != nil || len(taps) != 0 {
		t.Errorf("Receive = %v, %v; want none before a session is current", taps, err)
	}

	r.settings.ReceiveWait = 5 * time.Second
	go func() {
		time.Sleep(50 * time.Millisecond)
		r.setCurrent(&session{}, "Fake")
	}()
	began := time.Now()
	taps, err := r.Receive(ctx)
	if err != nil || len(taps) != 1 || taps[0] != tap {
		t.Errorf("Receive = %v, %v; want %v", taps, err, tap)
	}
	if elapsed := time.Since(began); elapsed > 2*time.Second {
		t.Errorf("Receive took %v; want it woken when the session became current", elapsed)
	}
}

func TestLongStderrLineIsClippedInWhy(t *testing.T) {
	h := start(t, "loud", nil)
	down := h.next()
	stderr, found := strings.CutPrefix(down.Why, "Two-way handler exited (exit status 1): ")
	if !found {
		t.Fatalf("got %+v; want the exit status and the stderr line", down)
	}
	want := strings.Repeat("é", 200) + "…"
	if stderr != want {
		t.Errorf("stderr in why is %d runes; want the first 200 and an ellipsis", utf8.RuneCountInString(stderr))
	}
}

func TestUnreadableHelloIsRetried(t *testing.T) {
	for _, scenario := range []string{"emptyhello", "nullhello"} {
		t.Run(scenario, func(t *testing.T) {
			h := start(t, scenario, nil)
			want := twoway.Availability{Why: "Two-way handler sent a hello reply pr-mon can't read"}
			if got := h.next(); got != want {
				t.Fatalf("got %+v, want %+v", got, want)
			}
			waitFor(t, "a second start", func() bool { return len(h.starts()) >= 2 })
		})
	}
}

// pauses are the gaps between each brief session's exit and the next start.
func (h *harness) pauses(n int) []time.Duration {
	h.t.Helper()
	waitFor(h.t, fmt.Sprintf("%d starts", n+1), func() bool { return len(h.starts()) > n })
	starts, exits := h.starts(), h.times("exits")
	out := []time.Duration{}
	for i := range n {
		out = append(out, starts[i+1].Sub(exits[i]))
	}
	return out
}

// A handler that dies right after hello still backs off.
func TestBriefSessionsBackOff(t *testing.T) {
	first, most := 50*time.Millisecond, 400*time.Millisecond
	t.Setenv("PRMON_FAKE_UP", "0s")
	h := start(t, "brief", func(s *Settings) { s.FirstBackoff, s.MaxBackoff = first, most })
	for i, pause := range h.pauses(4) {
		if want := min(first<<i, most); pause < want {
			t.Errorf("pause %d was %v; want at least %v", i, pause, want)
		}
	}
}

// A session that stayed up at least MaxBackoff resets the backoff.
func TestLongSessionResetsBackoff(t *testing.T) {
	first, most := 40*time.Millisecond, 300*time.Millisecond
	t.Setenv("PRMON_FAKE_UP", "500ms")
	h := start(t, "brief", func(s *Settings) { s.FirstBackoff, s.MaxBackoff = first, most })
	pauses := h.pauses(5)
	// Without the reset these would be 300 ms each.
	if shortest := min(pauses[3], pauses[4]); shortest >= 250*time.Millisecond {
		t.Errorf("pauses %v; want them back near %v after long sessions", pauses, first)
	}
}

// The service logs availability with its context; the runner only at debug.
func TestAvailabilityIsLoggedAtDebug(t *testing.T) {
	h := start(t, "exitafter", nil)
	h.ready()
	if _, err := h.runner.Post(context.Background(), twoway.Message{}); !errors.Is(err, twoway.ErrUnavailable) {
		t.Fatalf("Post: %v, want ErrUnavailable", err)
	}
	h.next()
	log := h.log.String()
	for _, want := range []string{`level=DEBUG msg="Two-way handler ready"`, `level=DEBUG msg="Two-way handler exited`} {
		if !strings.Contains(log, want) {
			t.Errorf("log has no %q:\n%.2000s", want, log)
		}
	}
}
