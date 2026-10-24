// Package protocol speaks pr-mon's two-way handler protocol: JSON-line
// requests on stdin, replies and tap events on stdout. See pr-mon's
// docs/two-way-handler.md.
package protocol

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/acheris-labs/pr-mon/notifiers/telegram/internal/telegram"
)

const (
	// Version is the protocol version this handler speaks.
	Version = 1
	// Name is shown in pr-mon's clients.
	Name = "Telegram"
	// MaxLine is the longest input line, newline excluded, that is read.
	MaxLine = 1 << 20
)

// Backend is the chat service the session drives. *telegram.Client is one.
type Backend interface {
	Post(ctx context.Context, message telegram.Message) (string, error)
	Edit(ctx context.Context, id string, message telegram.Message) error
	Delete(ctx context.Context, id string) error
	Receive(ctx context.Context) ([]telegram.Tap, error)
	Ack(ctx context.Context, receipt string) error
}

// Options tune a session. Zero values mean a 25 s request timeout, a receive
// backoff from 1 s to 1 min, no logging, and a timer that stops with ctx.
type Options struct {
	RequestTimeout, MinBackoff, MaxBackoff time.Duration
	Log                                    *slog.Logger
	Sleep                                  func(ctx context.Context, d time.Duration) error
}

func (o Options) withDefaults() Options {
	if o.RequestTimeout == 0 {
		o.RequestTimeout = 25 * time.Second
	}
	if o.MinBackoff == 0 {
		o.MinBackoff = time.Second
	}
	if o.MaxBackoff == 0 {
		o.MaxBackoff = time.Minute
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.Sleep == nil {
		o.Sleep = sleep
	}
	return o
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

type session struct {
	out     io.Writer
	backend Backend
	opts    Options

	mu        sync.Mutex // guards out, writeErr and helloDone
	writeErr  error
	helloDone bool          // hello's reply has been written
	failed    chan struct{} // closed on the first write error

	receiveOnce sync.Once
	wg          sync.WaitGroup
}

// input is one line read from stdin, or the error that ended reading.
type input struct {
	line    []byte
	tooLong bool
	err     error
}

// Run serves requests from in and writes replies and tap events to out until
// in reaches EOF or ctx is cancelled, then waits for the requests in flight
// and returns nil. It returns an error when writing to out fails or reading
// in fails other than at EOF.
func Run(ctx context.Context, in io.Reader, out io.Writer, backend Backend, opts Options) error {
	s := &session{out: out, backend: backend, opts: opts.withDefaults(), failed: make(chan struct{})}
	runCtx, cancel := context.WithCancel(ctx)

	// A read can't be interrupted, so lines come from their own goroutine,
	// which stops at the next line once Run has returned.
	lines := make(chan input)
	stop := make(chan struct{})
	defer close(stop)
	go readLines(in, lines, stop)

	var readErr error
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case <-s.failed:
			break loop
		case l := <-lines:
			if l.err != nil {
				if !errors.Is(l.err, io.EOF) {
					readErr = fmt.Errorf("reading requests: %w", l.err)
				}
				break loop
			}
			s.handle(runCtx, l)
		}
	}
	cancel()
	s.wg.Wait()

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writeErr != nil {
		return fmt.Errorf("writing to pr-mon: %w", s.writeErr)
	}
	return readErr
}

// readLines sends each line of in, then the error that ended it.
func readLines(in io.Reader, lines chan<- input, stop <-chan struct{}) {
	r := bufio.NewReaderSize(in, 64<<10)
	for {
		l := readLine(r)
		select {
		case lines <- l:
		case <-stop:
			return
		}
		if l.err != nil {
			return
		}
	}
}

// readLine reads one line without its newline. A line longer than MaxLine is
// read to its end and reported as tooLong, without its bytes. A final line
// without a newline is returned before EOF.
func readLine(r *bufio.Reader) input {
	var line []byte
	tooLong := false
	for {
		chunk, err := r.ReadSlice('\n')
		end := len(chunk)
		if err == nil {
			end-- // the newline
		}
		if !tooLong {
			if len(line)+end > MaxLine {
				tooLong, line = true, nil
			} else {
				line = append(line, chunk[:end]...)
			}
		}
		switch {
		case err == nil:
			return input{line: line, tooLong: tooLong}
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF) && (len(line) > 0 || tooLong):
			// The last line had no newline; EOF comes on the next read.
			return input{line: line, tooLong: tooLong}
		default:
			return input{err: err}
		}
	}
}

type request struct {
	ID   *int64          `json:"id"`
	Op   json.RawMessage `json:"op"`
	Args json.RawMessage `json:"args"`
}

type reply struct {
	ID     int64       `json:"id"`
	OK     bool        `json:"ok"`
	Result any         `json:"result,omitempty"`
	Error  *replyError `json:"error,omitempty"`
}

type replyError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type event struct {
	Event string `json:"event"`
	Args  any    `json:"args"`
}

type tapArgs struct {
	MessageID string `json:"message_id"`
	Key       string `json:"key"`
	Receipt   string `json:"receipt"`
}

// handle parses one line and answers it in its own goroutine.
func (s *session) handle(runCtx context.Context, l input) {
	if l.tooLong {
		s.opts.Log.Warn("skipped a request line longer than the limit", "limit", MaxLine)
		return
	}
	var req request
	if err := json.Unmarshal(l.line, &req); err != nil || req.ID == nil {
		s.opts.Log.Warn("skipped a line that isn't a request with an integer id", "bytes", len(l.line))
		return
	}
	id := *req.ID
	var op string
	if json.Unmarshal(req.Op, &op) != nil {
		op = string(req.Op)
	}
	if op != "hello" && !s.ready() {
		s.wg.Go(func() { s.fail(id, errors.New("hello first")) })
		return
	}
	s.wg.Go(func() {
		reqCtx, cancel := context.WithTimeout(runCtx, s.opts.RequestTimeout)
		defer cancel()
		result, err := s.do(reqCtx, op, req.Args)
		if err != nil {
			s.fail(id, err)
			return
		}
		if s.write(reply{ID: id, OK: true, Result: result}, op == "hello") && op == "hello" {
			s.receiveOnce.Do(func() { s.wg.Go(func() { s.receive(runCtx) }) })
		}
	})
}

// ready reports whether hello's reply has been written. It waits for a write
// in progress, so a request read after pr-mon saw that reply is let through.
func (s *session) ready() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.helloDone
}

// do runs one operation and returns its result.
func (s *session) do(ctx context.Context, op string, raw json.RawMessage) (any, error) {
	decode := func(args any) error {
		if err := json.Unmarshal(raw, args); err != nil {
			return fmt.Errorf("bad args for %s: %w", op, err)
		}
		return nil
	}
	switch op {
	case "hello":
		return struct {
			Protocol int    `json:"protocol"`
			Name     string `json:"name"`
		}{Version, Name}, nil
	case "post":
		var args struct {
			Message telegram.Message `json:"message"`
		}
		if err := decode(&args); err != nil {
			return nil, err
		}
		id, err := s.backend.Post(ctx, args.Message)
		if err != nil {
			return nil, err
		}
		return struct {
			MessageID string `json:"message_id"`
		}{id}, nil
	case "edit":
		var args struct {
			MessageID string           `json:"message_id"`
			Message   telegram.Message `json:"message"`
		}
		if err := decode(&args); err != nil {
			return nil, err
		}
		return struct{}{}, s.backend.Edit(ctx, args.MessageID, args.Message)
	case "delete":
		var args struct {
			MessageID string `json:"message_id"`
		}
		if err := decode(&args); err != nil {
			return nil, err
		}
		return struct{}{}, s.backend.Delete(ctx, args.MessageID)
	case "ack":
		var args struct {
			Receipt string `json:"receipt"`
		}
		if err := decode(&args); err != nil {
			return nil, err
		}
		return struct{}{}, s.backend.Ack(ctx, args.Receipt)
	default:
		return nil, fmt.Errorf("unknown op %q", op)
	}
}

// fail answers request id with err's code and message.
func (s *session) fail(id int64, err error) {
	code := "failed"
	switch {
	case errors.Is(err, telegram.ErrGone):
		code = "gone"
	case errors.Is(err, telegram.ErrTooOld):
		code = "too_old"
	}
	s.write(reply{ID: id, Error: &replyError{Code: code, Message: err.Error()}}, false)
}

// write sends v as one line and reports whether it was written; hello marks
// it as hello's reply. The first write error is kept and ends the session;
// nothing is written after it.
func (s *session) write(v any, hello bool) bool {
	b, err := json.Marshal(v)
	if err != nil {
		// Every value written is built from strings and ints.
		panic(err)
	}
	b = append(b, '\n')
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writeErr != nil {
		return false
	}
	if _, err := s.out.Write(b); err != nil {
		s.writeErr = err
		close(s.failed)
		return false
	}
	if hello {
		s.helloDone = true
	}
	return true
}

// receive turns queued taps into events until ctx is done, backing off while
// Receive fails.
func (s *session) receive(ctx context.Context) {
	backoff := s.opts.MinBackoff
	lastErr := ""
	for {
		taps, err := s.backend.Receive(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			if msg := err.Error(); msg != lastErr {
				s.opts.Log.Warn("receiving taps failed", "err", err)
				lastErr = msg
			}
			if s.opts.Sleep(ctx, backoff) != nil {
				return
			}
			backoff = min(backoff*2, s.opts.MaxBackoff)
			continue
		}
		backoff, lastErr = s.opts.MinBackoff, ""
		for _, tap := range taps {
			if !s.write(event{Event: "tap", Args: tapArgs{tap.MessageID, tap.Key, tap.Receipt}}, false) {
				return
			}
		}
	}
}
