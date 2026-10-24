package handler

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/acheris-labs/pr-mon/internal/twoway"
)

// Defaults, from docs/two-way-handler.md.
const (
	defaultHelloTimeout   = 10 * time.Second
	defaultRequestTimeout = 30 * time.Second
	defaultFirstBackoff   = time.Second
	defaultMaxBackoff     = time.Minute
	defaultShutdownGrace  = 5 * time.Second
	defaultReceiveWait    = 20 * time.Second
)

const (
	defaultName = "Two-way" // Name before any hello succeeds
	maxLine     = 1 << 20   // longest stdout or stderr line read
	drainWait   = time.Second
	helloID     = 1 // hello is a session's first request
	logFrom     = "two-way handler"
)

var errNoReply = errors.New("the two-way handler didn't reply")

// Settings configure a Runner.
type Settings struct {
	Argv     []string
	Version  string // pr-mon's build version, sent in hello
	Log      *slog.Logger
	OnChange func(twoway.Availability)
	// Zero means the default (Global Constraints); tests shorten them.
	HelloTimeout, RequestTimeout, FirstBackoff, MaxBackoff, ShutdownGrace, ReceiveWait time.Duration
}

// Runner runs a handler program, restarting it when it fails, and is the
// two-way channel it provides.
type Runner struct {
	settings Settings
	log      *slog.Logger
	cancel   context.CancelFunc
	done     chan struct{} // closed when the supervisor has stopped
	tapped   chan struct{} // signalled when a tap is queued or a session becomes current

	mu      sync.Mutex
	current *session // the session that answered hello, or nil
	name    string
	avail   twoway.Availability // as last reported to OnChange
	taps    []twoway.Tap
}

var _ twoway.Channel = (*Runner)(nil)

// Start returns at once; the handler starts in the background. Cancelling ctx
// has the same effect as Close.
func Start(ctx context.Context, settings Settings) *Runner {
	for _, d := range []struct {
		value *time.Duration
		def   time.Duration
	}{
		{&settings.HelloTimeout, defaultHelloTimeout},
		{&settings.RequestTimeout, defaultRequestTimeout},
		{&settings.FirstBackoff, defaultFirstBackoff},
		{&settings.MaxBackoff, defaultMaxBackoff},
		{&settings.ShutdownGrace, defaultShutdownGrace},
		{&settings.ReceiveWait, defaultReceiveWait},
	} {
		if *d.value <= 0 {
			*d.value = d.def
		}
	}
	log := cmp.Or(settings.Log, slog.Default())
	ctx, cancel := context.WithCancel(ctx)
	r := &Runner{
		settings: settings, log: log, cancel: cancel,
		done: make(chan struct{}), tapped: make(chan struct{}, 1),
	}
	go r.supervise(ctx)
	return r
}

// Close stops the handler: it closes its stdin, waits up to the shutdown
// grace, then kills it. Waiting requests fail. Calling it again does nothing.
func (r *Runner) Close() error {
	r.cancel()
	<-r.done
	return nil
}

// Name is the name the handler gave in hello, or "Two-way" before one.
func (r *Runner) Name() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return cmp.Or(r.name, defaultName)
}

func (r *Runner) Post(ctx context.Context, message twoway.Message) (string, error) {
	result, err := r.request(ctx, "post", postArgs{encodeMessage(message)})
	if err != nil {
		return "", err
	}
	var out struct {
		MessageID string `json:"message_id"`
	}
	if err := json.Unmarshal(result, &out); err != nil || out.MessageID == "" {
		return "", errors.New("the two-way handler's post reply has no message_id")
	}
	return out.MessageID, nil
}

func (r *Runner) Edit(ctx context.Context, id string, message twoway.Message) error {
	_, err := r.request(ctx, "edit", editArgs{id, encodeMessage(message)})
	return err
}

func (r *Runner) Delete(ctx context.Context, id string) error {
	_, err := r.request(ctx, "delete", deleteArgs{id})
	return err
}

func (r *Runner) Ack(ctx context.Context, tap twoway.Tap) error {
	_, err := r.request(ctx, "ack", ackArgs{tap.Receipt})
	return err
}

// Receive waits up to ReceiveWait for a tap and returns the taps that have
// arrived, possibly none. Taps wait while no session is current, so acting on
// one never finds the handler unavailable just because hello's reply is still
// on its way to the supervisor.
func (r *Runner) Receive(ctx context.Context) ([]twoway.Tap, error) {
	timer := time.NewTimer(r.settings.ReceiveWait)
	defer timer.Stop()
	for {
		var taps []twoway.Tap
		r.mu.Lock()
		if r.current != nil {
			taps = r.taps
			r.taps = nil
		}
		r.mu.Unlock()
		if len(taps) > 0 {
			return taps, nil
		}
		select {
		case <-r.tapped:
		case <-timer.C:
			return nil, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// request sends op to the ready handler and returns the reply's result.
func (r *Runner) request(ctx context.Context, op string, args any) (json.RawMessage, error) {
	r.mu.Lock()
	s := r.current
	r.mu.Unlock()
	if s == nil {
		return nil, fmt.Errorf("%s: %w", op, twoway.ErrUnavailable)
	}
	reply, err := s.call(ctx, op, args, r.settings.RequestTimeout)
	if err != nil {
		return nil, err
	}
	if !reply.OK {
		return nil, replyError(reply).err()
	}
	return reply.Result, nil
}

func replyError(reply incoming) wireError {
	if reply.Error == nil {
		return wireError{Code: "failed", Message: "request failed"}
	}
	return *reply.Error
}

func (r *Runner) addTap(tap twoway.Tap) {
	r.mu.Lock()
	r.taps = append(r.taps, tap)
	r.mu.Unlock()
	r.signalTapped()
}

// setAvailability reports a to OnChange if it differs from the last report.
// Only the supervisor calls it, never with r.mu held.
func (r *Runner) setAvailability(a twoway.Availability) {
	r.mu.Lock()
	changed := r.avail != a
	r.avail = a
	r.mu.Unlock()
	if !changed {
		return
	}
	// The service logs it with its own context.
	if a.Ready {
		r.log.Debug("Two-way handler ready", "name", a.Name)
	} else {
		r.log.Debug(a.Why)
	}
	if r.settings.OnChange != nil {
		r.settings.OnChange(a)
	}
}

// setCurrent sets the session requests go to, and the name if one is given.
// A session becoming current wakes Receive for the taps waiting on it.
func (r *Runner) setCurrent(s *session, name string) {
	r.mu.Lock()
	r.current = s
	r.name = cmp.Or(name, r.name)
	r.mu.Unlock()
	if s != nil {
		r.signalTapped()
	}
}

func (r *Runner) signalTapped() {
	select {
	case r.tapped <- struct{}{}:
	default:
	}
}

// supervise runs the handler until ctx ends, pausing between attempts for a
// backoff that doubles up to MaxBackoff. It resets once a session has stayed
// ready for MaxBackoff, so one that dies right after hello still backs off.
func (r *Runner) supervise(ctx context.Context) {
	defer close(r.done)
	pause := r.settings.FirstBackoff
	for {
		up, retry := r.runOnce(ctx)
		if !retry {
			return
		}
		if up >= r.settings.MaxBackoff {
			pause = r.settings.FirstBackoff
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(pause):
		}
		pause = min(pause*2, r.settings.MaxBackoff)
	}
}

// runOnce starts the handler and runs it until it fails or ctx ends. up is
// how long it was ready, retry whether to start it again.
func (r *Runner) runOnce(ctx context.Context) (up time.Duration, retry bool) {
	s, err := r.startSession()
	if err != nil {
		r.setAvailability(twoway.Availability{Why: fmt.Sprintf("Two-way handler couldn't start: %v", err)})
		return 0, true
	}
	why, retry := r.hello(ctx, s)
	if ctx.Err() != nil {
		r.setCurrent(nil, "")
		s.stop(r.settings.ShutdownGrace)
		return 0, false
	}
	if why != "" {
		s.stop(r.settings.ShutdownGrace)
		r.setAvailability(twoway.Availability{Why: why})
		return 0, retry
	}
	ready := time.Now()
	select {
	case <-ctx.Done():
		r.setCurrent(nil, "")
		s.stop(r.settings.ShutdownGrace)
		return time.Since(ready), false
	case <-s.exited:
	}
	r.setCurrent(nil, "")
	r.setAvailability(twoway.Availability{Why: s.exitWhy()})
	return time.Since(ready), true
}

// hello greets the handler and, if it answers in our protocol, makes s the
// current session. Otherwise why says what went wrong, and retry whether to
// try again.
func (r *Runner) hello(ctx context.Context, s *session) (why string, retry bool) {
	timeout := r.settings.HelloTimeout
	reply, err := s.call(ctx, "hello", helloArgs{Protocol, "pr-mon", r.settings.Version}, timeout)
	switch {
	case ctx.Err() != nil:
		return "", false
	case errors.Is(err, twoway.ErrUnavailable):
		// Usually it has exited; stop it in case it only closed stdin.
		s.stop(r.settings.ShutdownGrace)
		return s.exitWhy(), true
	case errors.Is(err, errNoReply):
		return fmt.Sprintf("Two-way handler didn't answer hello within %v", timeout), true
	case err != nil:
		return fmt.Sprintf("Two-way handler hello failed: %v", err), true
	case !reply.OK:
		return "Two-way handler refused hello: " + replyError(reply).Message, true
	}
	var result struct {
		Protocol int    `json:"protocol"`
		Name     string `json:"name"`
	}
	// An empty or null result has no protocol, so it is unreadable too.
	if err := json.Unmarshal(reply.Result, &result); err != nil || result.Protocol <= 0 {
		return "Two-way handler sent a hello reply pr-mon can't read", true
	}
	if result.Protocol != Protocol {
		return fmt.Sprintf("Two-way handler speaks protocol %d; pr-mon needs %d", result.Protocol, Protocol), false
	}
	name := cmp.Or(result.Name, defaultName)
	r.setCurrent(s, name)
	r.setAvailability(twoway.Availability{Ready: true, Name: name})
	return "", true
}

// session is one run of the handler process.
type session struct {
	r       *Runner
	cmd     *exec.Cmd
	stdin   *os.File
	exited  chan struct{} // closed once the process and its readers are done
	writeMu sync.Mutex    // one request line at a time

	helloAnswered bool // stdout reader only

	mu         sync.Mutex
	nextID     int64
	pending    map[int64]chan incoming
	dead       bool
	exitErr    error
	lastStderr string
}

// startSession starts the process with its own pipes, which, unlike the ones
// exec.Cmd makes, take deadlines.
func (r *Runner) startSession() (*session, error) {
	argv := r.settings.Argv
	if len(argv) == 0 {
		return nil, errors.New("no command")
	}
	inR, inW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		closeFiles(inR, inW)
		return nil, err
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		closeFiles(inR, inW, outR, outW)
		return nil, err
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = inR, outW, errW
	err = cmd.Start()
	closeFiles(inR, outW, errW) // the child has its own copies
	if err != nil {
		closeFiles(inW, outR, errR)
		return nil, err
	}

	s := &session{r: r, cmd: cmd, stdin: inW, exited: make(chan struct{}), pending: map[int64]chan incoming{}}
	var readers sync.WaitGroup
	readers.Go(func() { s.readStdout(outR) })
	readers.Go(func() { s.readStderr(errR) })
	go func() {
		err := cmd.Wait()
		// Read what's left, but don't wait on a child of the handler that
		// still holds the pipes.
		deadline := time.Now().Add(drainWait)
		_ = outR.SetReadDeadline(deadline)
		_ = errR.SetReadDeadline(deadline)
		readers.Wait()
		closeFiles(inW, outR, errR)
		s.mu.Lock()
		s.dead, s.exitErr = true, err
		s.mu.Unlock()
		close(s.exited)
	}()
	return s, nil
}

func closeFiles(files ...*os.File) {
	for _, f := range files {
		_ = f.Close()
	}
}

// stop closes stdin, waits up to grace for the process to exit, then kills it.
func (s *session) stop(grace time.Duration) {
	_ = s.stdin.Close()
	select {
	case <-s.exited:
		return
	case <-time.After(grace):
	}
	_ = s.cmd.Process.Kill()
	<-s.exited
}

// exitWhy says how the process ended, with its last stderr line.
func (s *session) exitWhy() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	status := "exit status 0"
	if s.exitErr != nil {
		status = s.exitErr.Error()
	}
	why := fmt.Sprintf("Two-way handler exited (%s)", status)
	if s.lastStderr != "" {
		why += ": " + s.lastStderr
	}
	return why
}

// call sends a request and waits for its reply.
func (s *session) call(ctx context.Context, op string, args any, timeout time.Duration) (incoming, error) {
	unavailable := fmt.Errorf("%s: %w", op, twoway.ErrUnavailable)
	s.mu.Lock()
	if s.dead {
		s.mu.Unlock()
		return incoming{}, unavailable
	}
	s.nextID++
	id := s.nextID
	reply := make(chan incoming, 1)
	s.pending[id] = reply
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
	}()

	line, err := marshal(request{ID: id, Op: op, Args: args})
	if err != nil {
		return incoming{}, err
	}
	noReply := fmt.Errorf("%s: %w within %v", op, errNoReply, timeout)
	deadline := time.Now().Add(timeout)
	if err := s.write(append(line, '\n'), deadline); err != nil {
		if errors.Is(err, os.ErrDeadlineExceeded) {
			return incoming{}, noReply
		}
		return incoming{}, unavailable
	}
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case got := <-reply:
		return got, nil
	case <-s.exited:
		select {
		case got := <-reply: // answered just before exiting
			return got, nil
		default:
			return incoming{}, unavailable
		}
	case <-timer.C:
		return incoming{}, noReply
	case <-ctx.Done():
		return incoming{}, ctx.Err()
	}
}

// write sends one line; a handler that stops reading stdin can't block it
// past deadline.
func (s *session) write(line []byte, deadline time.Time) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.stdin.SetWriteDeadline(deadline); err != nil {
		return err
	}
	_, err := s.stdin.Write(line)
	return err
}

func (s *session) readStdout(f *os.File) {
	reader := bufio.NewReaderSize(f, 64<<10)
	for {
		line, tooLong, err := readLine(reader)
		if tooLong {
			s.r.log.Warn("Skipped a two-way handler line over 1 MiB")
		} else if len(bytes.TrimSpace(line)) > 0 {
			s.handle(line)
		}
		if err != nil {
			return
		}
	}
}

// handle acts on one stdout line: a reply goes to its request, a tap to the
// queue. Anything else is logged and skipped.
func (s *session) handle(line []byte) {
	log := s.r.log
	var msg incoming
	if err := json.Unmarshal(line, &msg); err != nil {
		log.Warn("Skipped a two-way handler line pr-mon can't read", "line", clip(string(line)), "err", err)
		return
	}
	switch {
	case msg.ID != nil:
		if *msg.ID == helloID {
			s.helloAnswered = true
		}
		s.mu.Lock()
		reply := s.pending[*msg.ID]
		s.mu.Unlock()
		if reply == nil {
			log.Warn("Skipped a two-way handler reply nothing is waiting for", "id", *msg.ID)
			return
		}
		select {
		case reply <- msg:
		default:
			log.Warn("Skipped a repeated two-way handler reply", "id", *msg.ID)
		}
	case msg.Event == "tap":
		var tap tapArgs
		if !s.helloAnswered {
			log.Warn("Skipped a two-way handler tap sent before hello")
		} else if err := json.Unmarshal(msg.Args, &tap); err != nil {
			log.Warn("Skipped a two-way handler tap pr-mon can't read", "err", err)
		} else {
			s.r.addTap(twoway.Tap{MessageID: tap.MessageID, Key: tap.Key, Receipt: tap.Receipt})
		}
	default:
		log.Debug("Skipped a two-way handler line that is neither a reply nor a known event", "event", msg.Event)
	}
}

// readStderr logs each line and keeps the last one, clipped, for exitWhy.
func (s *session) readStderr(f *os.File) {
	reader := bufio.NewReader(f)
	for {
		line, tooLong, err := readLine(reader)
		text := strings.TrimSpace(string(line))
		if tooLong {
			s.r.log.Warn("Skipped a two-way handler stderr line over 1 MiB")
		} else if text != "" {
			s.r.log.Info(text, "from", logFrom)
			s.mu.Lock()
			s.lastStderr = clip(text) // it goes into a status warning
			s.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

// readLine reads one line without its newline. A line over maxLine is read to
// its end and dropped, with tooLong set, so the next line is read cleanly.
func readLine(reader *bufio.Reader) (line []byte, tooLong bool, err error) {
	for {
		chunk, err := reader.ReadSlice('\n')
		if !tooLong {
			line = append(line, chunk...)
			if len(bytes.TrimSuffix(line, []byte("\n"))) > maxLine {
				line, tooLong = nil, true
			}
		}
		if err != bufio.ErrBufferFull {
			return bytes.TrimSuffix(line, []byte("\n")), tooLong, err
		}
	}
}

// clip shortens text to 200 runes, marking the cut with an ellipsis.
func clip(text string) string {
	const most = 200
	runes := 0
	for i := range text {
		if runes == most {
			return text[:i] + "…"
		}
		runes++
	}
	return text
}
