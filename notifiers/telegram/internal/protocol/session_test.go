package protocol

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

	"github.com/acheris-labs/pr-mon/notifiers/telegram/internal/telegram"
)

var _ Backend = (*telegram.Client)(nil)

const guard = 2 * time.Second

// fake is a Backend whose methods each test sets; Receive blocks until ctx is
// done unless replaced, and the others fail.
type fake struct {
	post    func(context.Context, telegram.Message) (string, error)
	edit    func(context.Context, string, telegram.Message) error
	del     func(context.Context, string) error
	receive func(context.Context) ([]telegram.Tap, error)
	ack     func(context.Context, string) error
}

func (f *fake) Post(ctx context.Context, m telegram.Message) (string, error) {
	if f.post == nil {
		return "", errors.New("post not expected")
	}
	return f.post(ctx, m)
}

func (f *fake) Edit(ctx context.Context, id string, m telegram.Message) error {
	if f.edit == nil {
		return errors.New("edit not expected")
	}
	return f.edit(ctx, id, m)
}

func (f *fake) Delete(ctx context.Context, id string) error {
	if f.del == nil {
		return errors.New("delete not expected")
	}
	return f.del(ctx, id)
}

func (f *fake) Receive(ctx context.Context) ([]telegram.Tap, error) {
	if f.receive == nil {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return f.receive(ctx)
}

func (f *fake) Ack(ctx context.Context, receipt string) error {
	if f.ack == nil {
		return errors.New("ack not expected")
	}
	return f.ack(ctx, receipt)
}

// lockedBuffer collects log output written from several goroutines.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) lines() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.Split(strings.TrimSuffix(b.buf.String(), "\n"), "\n")
}

// harness runs a session over pipes: send writes to its stdin, next reads its
// stdout.
type harness struct {
	inW    *io.PipeWriter
	outR   *io.PipeReader
	outW   *io.PipeWriter
	lines  chan string
	done   chan struct{}
	err    error
	cancel context.CancelFunc
}

func start(t *testing.T, backend Backend, opts Options) *harness {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	h := &harness{inW: inW, outR: outR, outW: outW, lines: make(chan string, 100), done: make(chan struct{}), cancel: cancel}
	go func() {
		defer close(h.lines)
		r := bufio.NewReader(outR)
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			h.lines <- line
		}
	}()
	go func() {
		defer close(h.done)
		h.err = Run(ctx, inR, outW, backend, opts)
	}()
	t.Cleanup(func() {
		inW.Close()
		select {
		case <-h.done:
		case <-time.After(guard):
			t.Error("Run did not return after stdin closed")
		}
		cancel()
		outW.Close()
	})
	return h
}

func (h *harness) send(t *testing.T, line string) {
	t.Helper()
	if _, err := io.WriteString(h.inW, line+"\n"); err != nil {
		t.Fatalf("send: %v", err)
	}
}

func (h *harness) next(t *testing.T) map[string]any {
	t.Helper()
	select {
	case line, ok := <-h.lines:
		if !ok {
			t.Fatal("stdout closed")
		}
		return decode(t, line)
	case <-time.After(guard):
		t.Fatal("no line within 2s")
	}
	return nil
}

// wait returns Run's result, failing the test if it hasn't returned within d.
func (h *harness) wait(t *testing.T, d time.Duration) error {
	t.Helper()
	select {
	case <-h.done:
		return h.err
	case <-time.After(d):
		t.Fatalf("Run did not return within %v", d)
	}
	return nil
}

// finish closes stdin, waits for Run, and returns every line not yet read.
func (h *harness) finish(t *testing.T) []map[string]any {
	t.Helper()
	h.inW.Close()
	if err := h.wait(t, guard); err != nil {
		t.Fatalf("Run: %v", err)
	}
	h.outW.Close()
	var rest []map[string]any
	for line := range h.lines {
		rest = append(rest, decode(t, line))
	}
	return rest
}

func decode(t *testing.T, line string) map[string]any {
	t.Helper()
	if !strings.HasSuffix(line, "\n") || strings.Count(line, "\n") != 1 {
		t.Fatalf("not one line: %q", line)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("decode %q: %v", line, err)
	}
	return m
}

func fixtureLine(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "protocol-fixtures", "two-way-handler", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

func fixture(t *testing.T, name string) map[string]any {
	t.Helper()
	return decode(t, fixtureLine(t, name)+"\n")
}

func equal(t *testing.T, got, want map[string]any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		g, _ := json.Marshal(got)
		w, _ := json.Marshal(want)
		t.Errorf("got  %s\nwant %s", g, w)
	}
}

func hello(t *testing.T, h *harness) {
	t.Helper()
	h.send(t, fixtureLine(t, "hello-request"))
	equal(t, h.next(t), fixture(t, "hello-reply"))
}

func failure(t *testing.T, reply map[string]any) (code, message string) {
	t.Helper()
	if reply["ok"] != false {
		t.Fatalf("ok = %v in %v", reply["ok"], reply)
	}
	e, _ := reply["error"].(map[string]any)
	code, _ = e["code"].(string)
	message, _ = e["message"].(string)
	return code, message
}

var wantMessage = telegram.Message{
	Status: "READY",
	Title:  "acme/api #12 · Fix login redirect",
	Lines:  []string{"octocat · fix-login → main"},
	Buttons: [][]telegram.Button{
		{{Label: "Squash & merge", Key: "xx2ev6d7"}},
		{{Label: "Open", URL: "https://github.com/acme/api/pull/12"}, {Label: "Dismiss", Key: "kofmkufw"}},
	},
}

func TestFixtureSession(t *testing.T) {
	var posted, edited telegram.Message
	var editedID, deletedID, acked string
	h := start(t, &fake{
		post: func(_ context.Context, m telegram.Message) (string, error) {
			posted = m
			return "1082", nil
		},
		edit: func(_ context.Context, id string, m telegram.Message) error {
			editedID, edited = id, m
			return nil
		},
		del: func(_ context.Context, id string) error {
			deletedID = id
			return nil
		},
		ack: func(_ context.Context, receipt string) error {
			acked = receipt
			return nil
		},
	}, Options{})
	hello(t, h)

	h.send(t, fixtureLine(t, "post-request"))
	equal(t, h.next(t), fixture(t, "post-reply"))
	h.send(t, fixtureLine(t, "edit-request"))
	equal(t, h.next(t), fixture(t, "ok-reply"))
	for _, c := range []struct {
		name string
		id   float64
	}{{"delete-request", 4}, {"ack-request", 5}} {
		h.send(t, fixtureLine(t, c.name))
		want := fixture(t, "ok-reply")
		want["id"] = c.id
		equal(t, h.next(t), want)
	}

	if !reflect.DeepEqual(posted, wantMessage) {
		t.Errorf("posted %+v\nwant   %+v", posted, wantMessage)
	}
	if editedID != "1082" || !reflect.DeepEqual(edited, wantMessage) {
		t.Errorf("edited %q %+v", editedID, edited)
	}
	if deletedID != "1082" {
		t.Errorf("deleted %q", deletedID)
	}
	if acked != "r-7731" {
		t.Errorf("acked %q", acked)
	}
}

func keys(m map[string]any) []string {
	var k []string
	for key := range m {
		k = append(k, key)
	}
	slices.Sort(k)
	return k
}

func TestErrorRepliesMatchFixtures(t *testing.T) {
	for _, c := range []struct {
		err      error
		code     string
		message  string
		fixture  string
		checkMsg bool
	}{
		{err: fmt.Errorf("%w: x", telegram.ErrGone), code: "gone", fixture: "error-reply-gone"},
		{err: fmt.Errorf("%w: x", telegram.ErrTooOld), code: "too_old", fixture: "error-reply-too-old"},
		{err: errors.New("boom"), code: "failed", message: "boom", fixture: "error-reply-gone", checkMsg: true},
	} {
		t.Run(c.code, func(t *testing.T) {
			h := start(t, &fake{del: func(context.Context, string) error { return c.err }}, Options{})
			hello(t, h)
			h.send(t, fixtureLine(t, "delete-request"))
			got := h.next(t)
			want := fixture(t, c.fixture)
			if !reflect.DeepEqual(keys(got), keys(want)) {
				t.Errorf("keys %v, want %v", keys(got), keys(want))
			}
			ge, _ := got["error"].(map[string]any)
			we, _ := want["error"].(map[string]any)
			if !reflect.DeepEqual(keys(ge), keys(we)) {
				t.Errorf("error keys %v, want %v", keys(ge), keys(we))
			}
			if got["id"] != float64(4) {
				t.Errorf("id = %v", got["id"])
			}
			code, message := failure(t, got)
			if code != c.code {
				t.Errorf("code = %q, want %q", code, c.code)
			}
			if message != c.err.Error() || (c.checkMsg && message != c.message) {
				t.Errorf("message = %q, want %q", message, c.err.Error())
			}
		})
	}
}

func TestTapEventMatchesFixture(t *testing.T) {
	var once sync.Once
	h := start(t, &fake{receive: func(ctx context.Context) ([]telegram.Tap, error) {
		var taps []telegram.Tap
		once.Do(func() { taps = []telegram.Tap{{MessageID: "1082", Key: "xx2ev6d7", Receipt: "r-7731"}} })
		if taps != nil {
			return taps, nil
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}}, Options{})
	hello(t, h)
	equal(t, h.next(t), fixture(t, "tap-event"))
}

func TestNoTapBeforeHello(t *testing.T) {
	called := make(chan struct{}, 10)
	h := start(t, &fake{receive: func(ctx context.Context) ([]telegram.Tap, error) {
		called <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	}}, Options{})
	h.send(t, fixtureLine(t, "post-request"))
	reply := h.next(t)
	if reply["id"] != float64(2) {
		t.Errorf("id = %v", reply["id"])
	}
	if code, message := failure(t, reply); code != "failed" || message != "hello first" {
		t.Errorf("got %q %q, want failed / hello first", code, message)
	}
	select {
	case <-called:
		t.Fatal("Receive called before hello")
	default:
	}
	hello(t, h)
	select {
	case <-called:
	case <-time.After(guard):
		t.Fatal("Receive not called after hello")
	}
}

func TestHelloAnswersVersionOne(t *testing.T) {
	h := start(t, &fake{}, Options{})
	h.send(t, `{"id":1,"op":"hello","args":{"protocol":2,"app":"pr-mon","version":"9.0.0"}}`)
	got := h.next(t)
	result, _ := got["result"].(map[string]any)
	if got["ok"] != true || result["protocol"] != float64(1) || result["name"] != "Telegram" {
		t.Errorf("got %v", got)
	}
}

func TestUnknownOp(t *testing.T) {
	h := start(t, &fake{}, Options{})
	hello(t, h)
	h.send(t, `{"id":9,"op":"frob","args":{}}`)
	got := h.next(t)
	if got["id"] != float64(9) {
		t.Errorf("id = %v", got["id"])
	}
	if code, message := failure(t, got); code != "failed" || message != `unknown op "frob"` {
		t.Errorf("got %q %q", code, message)
	}
}

func TestMissingOpFails(t *testing.T) {
	h := start(t, &fake{}, Options{})
	hello(t, h)
	h.send(t, `{"id":9,"args":{}}`)
	got := h.next(t)
	if got["id"] != float64(9) {
		t.Errorf("id = %v", got["id"])
	}
	if code, _ := failure(t, got); code != "failed" {
		t.Errorf("code = %q", code)
	}
}

func TestBadArgsFail(t *testing.T) {
	h := start(t, &fake{post: func(context.Context, telegram.Message) (string, error) { return "1", nil }}, Options{})
	hello(t, h)
	h.send(t, `{"id":2,"op":"post","args":{"message":"not a message"}}`)
	got := h.next(t)
	if got["id"] != float64(2) {
		t.Errorf("id = %v", got["id"])
	}
	if code, _ := failure(t, got); code != "failed" {
		t.Errorf("code = %q", code)
	}
}

// padded returns a JSON object line of exactly n bytes, before the newline.
func padded(t *testing.T, prefix string, n int) string {
	t.Helper()
	line := prefix + `,"pad":"` + strings.Repeat("x", n-len(prefix)-len(`,"pad":""}`)) + `"}`
	if len(line) != n {
		t.Fatalf("padded line is %d bytes, want %d", len(line), n)
	}
	return line
}

func TestBadLinesSkipped(t *testing.T) {
	log := &lockedBuffer{}
	h := start(t, &fake{}, Options{Log: slog.New(slog.NewTextHandler(log, nil))})
	h.send(t, "not json")
	h.send(t, "[1,2]")
	h.send(t, `{"op":"hello"}`)
	h.send(t, padded(t, `{"id":7,"op":"hello"`, MaxLine+1))
	hello(t, h)
	if rest := h.finish(t); len(rest) != 0 {
		t.Errorf("extra lines: %v", rest)
	}
	if got := log.lines(); len(got) != 4 {
		t.Errorf("log has %d lines, want 4: %q", len(got), got)
	}
}

func TestMaxLineAccepted(t *testing.T) {
	h := start(t, &fake{}, Options{})
	hello(t, h)
	h.send(t, padded(t, `{"id":8,"op":"frob"`, MaxLine))
	if got := h.next(t); got["id"] != float64(8) {
		t.Errorf("got %v", got)
	}
}

func TestConcurrentRequests(t *testing.T) {
	release := make(chan struct{})
	h := start(t, &fake{
		post: func(context.Context, telegram.Message) (string, error) {
			<-release
			return "1082", nil
		},
		edit: func(context.Context, string, telegram.Message) error { return nil },
	}, Options{})
	hello(t, h)
	h.send(t, fixtureLine(t, "post-request"))
	h.send(t, fixtureLine(t, "edit-request"))
	equal(t, h.next(t), fixture(t, "ok-reply"))
	close(release)
	equal(t, h.next(t), fixture(t, "post-reply"))
}

func TestRequestTimeout(t *testing.T) {
	h := start(t, &fake{post: func(ctx context.Context, _ telegram.Message) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}}, Options{RequestTimeout: 50 * time.Millisecond})
	hello(t, h)
	h.send(t, fixtureLine(t, "post-request"))
	got := h.next(t)
	if code, _ := failure(t, got); code != "failed" || got["id"] != float64(2) {
		t.Errorf("got %v", got)
	}
}

func TestReceiveErrorsBackOff(t *testing.T) {
	errSame := errors.New("relay down")
	results := []error{errSame, errSame, errSame, errSame, nil, errSame}
	var mu sync.Mutex
	var sleeps []time.Duration
	calls := 0
	blocked := make(chan struct{})
	log := &lockedBuffer{}
	h := start(t, &fake{receive: func(ctx context.Context) ([]telegram.Tap, error) {
		mu.Lock()
		i := calls
		calls++
		mu.Unlock()
		if i < len(results) {
			return nil, results[i]
		}
		if i == len(results) {
			close(blocked)
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}}, Options{
		MinBackoff: time.Second,
		MaxBackoff: 4 * time.Second,
		Log:        slog.New(slog.NewTextHandler(log, nil)),
		Sleep: func(_ context.Context, d time.Duration) error {
			mu.Lock()
			sleeps = append(sleeps, d)
			mu.Unlock()
			return nil
		},
	})
	hello(t, h)
	select {
	case <-blocked:
	case <-time.After(guard):
		t.Fatal("Receive never reached its last call")
	}
	mu.Lock()
	got := slices.Clone(sleeps)
	mu.Unlock()
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 4 * time.Second, time.Second}
	if !slices.Equal(got, want) {
		t.Errorf("sleeps %v, want %v", got, want)
	}
	if lines := log.lines(); len(lines) != 2 {
		t.Errorf("log has %d lines, want 2: %q", len(lines), lines)
	}
}

func TestRequestsAnsweredWhileReceiveFails(t *testing.T) {
	h := start(t, &fake{
		receive: func(context.Context) ([]telegram.Tap, error) { return nil, errors.New("relay down") },
		post:    func(context.Context, telegram.Message) (string, error) { return "1082", nil },
	}, Options{Sleep: func(ctx context.Context, _ time.Duration) error {
		<-ctx.Done()
		return ctx.Err()
	}})
	hello(t, h)
	h.send(t, fixtureLine(t, "post-request"))
	equal(t, h.next(t), fixture(t, "post-reply"))
}

func TestEOFExitsPromptly(t *testing.T) {
	posting := make(chan struct{})
	h := start(t, &fake{post: func(ctx context.Context, _ telegram.Message) (string, error) {
		close(posting)
		<-ctx.Done()
		return "", ctx.Err()
	}}, Options{})
	hello(t, h)
	h.send(t, fixtureLine(t, "post-request"))
	select {
	case <-posting:
	case <-time.After(guard):
		t.Fatal("Post not called")
	}
	h.inW.Close()
	if err := h.wait(t, time.Second); err != nil {
		t.Errorf("Run = %v, want nil", err)
	}
}

func TestContextCancelExits(t *testing.T) {
	h := start(t, &fake{}, Options{})
	hello(t, h)
	h.cancel()
	if err := h.wait(t, guard); err != nil {
		t.Errorf("Run = %v, want nil", err)
	}
}

func TestWriteFailureEnds(t *testing.T) {
	h := start(t, &fake{}, Options{})
	h.outR.Close()
	h.send(t, fixtureLine(t, "hello-request"))
	if err := h.wait(t, guard); err == nil {
		t.Error("Run = nil, want an error")
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("read broke") }

func TestReadFailureEnds(t *testing.T) {
	if err := Run(context.Background(), failingReader{}, io.Discard, &fake{}, Options{}); err == nil {
		t.Error("Run = nil, want an error")
	}
}
