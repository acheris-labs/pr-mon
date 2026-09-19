// Package daemon runs the backend in the background: single-instance lock,
// socket paths, logging, and starting and stopping the process.
package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/acheris-labs/pr-mon/internal/protocol"
	"github.com/acheris-labs/pr-mon/internal/state"
)

const (
	StartTimeout = 5 * time.Second
	StopTimeout  = 5 * time.Second
	// macOS limits Unix socket paths to 104 bytes (including the terminator).
	socketPathLimit = 100
	shortSocketRoot = "/tmp"
	requestTimeout  = 2 * time.Second
)

// Paths are where the backend keeps its socket, lock and log.
type Paths struct {
	Directory string
}

// Default puts everything beside state.json.
func Default() Paths {
	return Paths{Directory: filepath.Dir(state.DefaultPath())}
}

// Socket is the path clients connect to.
func (p Paths) Socket() string {
	path := filepath.Join(p.Directory, "daemon.sock")
	if len(path) <= socketPathLimit {
		return path
	}
	// Too long to bind: use a private per-user directory, one socket per state dir.
	sum := sha256.Sum256([]byte(p.Directory))
	digest := hex.EncodeToString(sum[:])[:16]
	return filepath.Join(shortSocketRoot, fmt.Sprintf("pr-mon-%d", os.Getuid()), digest+".sock")
}

func (p Paths) Lock() string { return filepath.Join(p.Directory, "daemon.lock") }

func (p Paths) Log() string { return filepath.Join(p.Directory, "daemon.log") }

// Stopped exists while the backend is down because someone asked it to stop
// (the shutdown op: `pr-mon stop`, or Stop in the app). A client that finds
// the backend gone restarts it only when this is absent, so an upgrade or a
// crash is recovered from but a deliberate stop is left alone.
func (p Paths) Stopped() string { return filepath.Join(p.Directory, "stopped") }

// EnsureSocketDir creates the socket's directory, refusing a shared one outside
// the state directory.
func (p Paths) EnsureSocketDir() error {
	parent := filepath.Dir(p.Socket())
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}
	if parent == p.Directory {
		return nil
	}
	info, err := os.Stat(parent)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || !ok || int(stat.Uid) != os.Getuid() || info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("socket directory %s is not private; remove it and retry", parent)
	}
	return nil
}

// toolDirs are where Homebrew puts gh and terminal-notifier.
var toolDirs = []string{"/opt/homebrew/bin", "/usr/local/bin"}

// ToolPath is path with the Homebrew directories appended where missing. An
// app opened from the Dock hands its children launchd's bare PATH
// (/usr/bin:/bin:/usr/sbin:/sbin), so a backend it starts would not find gh,
// terminal-notifier, or whatever a notification script calls.
func ToolPath(path string) string {
	present := map[string]bool{}
	parts := []string{}
	for _, dir := range filepath.SplitList(path) {
		if dir != "" {
			present[dir] = true
			parts = append(parts, dir)
		}
	}
	for _, dir := range toolDirs {
		if !present[dir] {
			parts = append(parts, dir)
		}
	}
	return strings.Join(parts, string(os.PathListSeparator))
}

// AlreadyRunningError means another backend holds the lock.
type AlreadyRunningError struct{ PID int }

func (e *AlreadyRunningError) Error() string {
	return fmt.Sprintf("backend already running (pid %d)", e.PID)
}

// AcquireLock holds the single-instance lock for this process's lifetime and
// records our pid. Close the returned file to release it.
func AcquireLock(paths Paths) (*os.File, error) {
	if err := os.MkdirAll(paths.Directory, 0o755); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(paths.Lock(), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		pid := readPID(file)
		file.Close()
		return nil, &AlreadyRunningError{PID: pid}
	}
	file.Truncate(0)
	file.Seek(0, 0)
	fmt.Fprint(file, os.Getpid())
	file.Sync()
	return file, nil
}

func readPID(file *os.File) int {
	file.Seek(0, 0)
	text := make([]byte, 32)
	count, _ := file.Read(text)
	pid, _ := strconv.Atoi(strings.TrimSpace(string(text[:count])))
	return pid
}

// RunningPID is the running backend's pid (0 if not recorded), or -1 if none.
func RunningPID(paths Paths) int {
	file, err := os.Open(paths.Lock())
	if err != nil {
		return -1
	}
	defer file.Close()
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
		return readPID(file) // someone else holds it: the backend is running
	}
	syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	return -1
}

// Request sends one request synchronously and returns its result.
func Request(paths Paths, op string, args any) (json.RawMessage, error) {
	connection, err := net.DialTimeout("unix", paths.Socket(), requestTimeout)
	if err != nil {
		return nil, err
	}
	defer connection.Close()
	connection.SetDeadline(time.Now().Add(requestTimeout))
	if args == nil {
		args = map[string]any{}
	}
	encoded, err := json.Marshal(args)
	if err != nil {
		return nil, err
	}
	line, err := protocol.Encode(protocol.Request{ID: 1, Op: op, Args: encoded})
	if err != nil {
		return nil, err
	}
	if _, err := connection.Write(line); err != nil {
		return nil, err
	}
	var response struct {
		OK     bool            `json:"ok"`
		Error  string          `json:"error"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.NewDecoder(connection).Decode(&response); err != nil {
		return nil, err
	}
	if !response.OK {
		message := response.Error
		if message == "" {
			message = "request failed"
		}
		return nil, errors.New(message)
	}
	return response.Result, nil
}

// Info is the backend's hello, or nil if it doesn't answer.
func Info(paths Paths) *protocol.Hello {
	result, err := Request(paths, "hello", nil)
	if err != nil {
		return nil
	}
	var hello protocol.Hello
	if err := json.Unmarshal(result, &hello); err != nil {
		return nil
	}
	return &hello
}

// Spawn starts the backend in the background (if needed) and waits until it answers.
func Spawn(paths Paths, timeout time.Duration) (int, error) {
	if hello := Info(paths); hello != nil && hello.PID != nil {
		return *hello.PID, nil
	}
	if err := os.MkdirAll(paths.Directory, 0o755); err != nil {
		return 0, err
	}
	executable, err := os.Executable()
	if err != nil {
		return 0, err
	}
	if runtime.GOOS == "darwin" {
		return spawnSession(paths, executable, timeout)
	}
	logFile, err := os.OpenFile(paths.Log(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return 0, err
	}
	defer logFile.Close()
	home, _ := os.UserHomeDir()
	command := exec.Command(executable, "daemon")
	command.Dir = home
	command.Stdin = nil
	command.Stdout = logFile
	command.Stderr = logFile
	// Its own session, so it outlives the terminal that started it.
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		return 0, err
	}
	// Reap it in the background and report through a channel: reading
	// command.ProcessState while Wait is running is a race.
	exited := make(chan error, 1)
	go func() { exited <- command.Wait() }()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if hello := Info(paths); hello != nil && hello.PID != nil {
			return *hello.PID, nil
		}
		select {
		case err := <-exited:
			code := 0
			var exit *exec.ExitError
			if errors.As(err, &exit) {
				code = exit.ExitCode()
			}
			return 0, fmt.Errorf("backend exited with code %d\n%s",
				code, TailLog(paths))
		case <-time.After(50 * time.Millisecond):
		}
	}
	return 0, fmt.Errorf("backend did not start within %s\n%s", timeout, TailLog(paths))
}

// spawnSession is Spawn on macOS: launchd runs the backend, so it belongs to
// no app or terminal, and whatever started it can go away cleanly.
func spawnSession(paths Paths, executable string, timeout time.Duration) (int, error) {
	// The registered job first; if it can't run, one loaded here instead.
	registered := kickstartSession()
	if !registered {
		if err := loadSession(paths, executable); err != nil {
			return 0, err
		}
	}
	deadline := time.Now().Add(timeout)
	for check := 0; time.Now().Before(deadline); check++ {
		if hello := Info(paths); hello != nil && hello.PID != nil {
			return *hello.PID, nil
		}
		// Asking launchd costs a process, so only every few rounds.
		if check%5 != 4 {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		code, exited := sessionExited()
		switch {
		case registered && (!sessionLoaded() || (exited && code != 0)):
			// launchd refused or dropped the registered job (it removes one that
			// fails a launch constraint); fall back once.
			registered = false
			if err := loadSession(paths, executable); err != nil {
				return 0, err
			}
		case exited && code != 0:
			return 0, fmt.Errorf("backend exited with code %d\n%s", code, TailLog(paths))
		}
		time.Sleep(50 * time.Millisecond)
	}
	return 0, fmt.Errorf("backend did not start within %s\n%s", timeout, TailLog(paths))
}

// Stop asks the backend to shut down and waits for it to go.
func Stop(paths Paths, timeout time.Duration) (bool, error) {
	if RunningPID(paths) < 0 && Info(paths) == nil {
		return false, nil
	}
	if _, err := Request(paths, "shutdown", nil); err != nil {
		// It may have stopped between the check and the request.
		if Info(paths) == nil {
			return false, nil
		}
		return false, err
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if RunningPID(paths) < 0 {
			return true, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false, fmt.Errorf("backend did not stop within %s", timeout)
}

// TailLog is the end of the log file, for error messages.
func TailLog(paths Paths) string {
	text, err := os.ReadFile(paths.Log())
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(text), "\n"), "\n")
	if len(lines) > 10 {
		lines = lines[len(lines)-10:]
	}
	return strings.Join(lines, "\n")
}
