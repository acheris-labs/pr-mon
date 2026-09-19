// Package autostart runs the backend at login on macOS with a launchd LaunchAgent.
package autostart

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/acheris-labs/pr-mon/internal/daemon"
)

const (
	Label        = "com.acheris-labs.pr-mon"
	readyTimeout = 20 * time.Second
)

// Error is an autostart problem whose message is for the user.
type Error struct{ Message string }

func (e *Error) Error() string { return e.Message }

// Status is what `pr-mon autostart status` reports.
type Status struct {
	Installed bool
	Loaded    bool
	Program   []string
}

// Agent is the LaunchAgent for this user.
type Agent struct {
	Dir string
	// Run replaces exec for tests.
	Run func(name string, args ...string) (string, int)
	UID int
}

func NewAgent() *Agent {
	return &Agent{
		Dir: filepath.Join(os.Getenv("HOME"), "Library", "LaunchAgents"),
		Run: runCommand,
		UID: os.Getuid(),
	}
}

func runCommand(name string, args ...string) (string, int) {
	command := exec.Command(name, args...)
	output, err := command.CombinedOutput()
	code := 0
	var exit *exec.ExitError
	if err != nil {
		code = 1
		if asExit(err, &exit) {
			code = exit.ExitCode()
		}
	}
	return strings.TrimSpace(string(output)), code
}

func asExit(err error, target **exec.ExitError) bool {
	exit, ok := err.(*exec.ExitError)
	if ok {
		*target = exit
	}
	return ok
}

func (a *Agent) PlistPath() string { return filepath.Join(a.Dir, Label+".plist") }

func (a *Agent) domain() string { return fmt.Sprintf("gui/%d", a.UID) }

func (a *Agent) launchctl(args ...string) (string, int) {
	return a.Run("launchctl", args...)
}

func (a *Agent) IsLoaded() bool {
	_, code := a.launchctl("print", a.domain()+"/"+Label)
	return code == 0
}

func (a *Agent) Status() Status {
	status := Status{Loaded: a.IsLoaded()}
	text, err := os.ReadFile(a.PlistPath())
	if err != nil {
		return status
	}
	status.Installed = true
	status.Program = programFrom(string(text))
	return status
}

// programFrom reads ProgramArguments back out of the plist we wrote.
func programFrom(text string) []string {
	start := strings.Index(text, "<key>ProgramArguments</key>")
	if start < 0 {
		return nil
	}
	rest := text[start:]
	open := strings.Index(rest, "<array>")
	close := strings.Index(rest, "</array>")
	if open < 0 || close < 0 || close < open {
		return nil
	}
	program := []string{}
	for _, line := range strings.Split(rest[open:close], "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "<string>") && strings.HasSuffix(line, "</string>") {
			program = append(program, daemon.UnescapeXML(strings.TrimSuffix(
				strings.TrimPrefix(line, "<string>"), "</string>")))
		}
	}
	return program
}

// Install writes the agent and hands the job to launchd.
func (a *Agent) Install(program []string, environment map[string]string, logPath string) error {
	if err := os.MkdirAll(a.Dir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(a.PlistPath(), []byte(plist(program, environment, logPath)), 0o644); err != nil {
		return err
	}
	if a.IsLoaded() {
		if err := a.bootout(); err != nil {
			return err
		}
	}
	if output, code := a.launchctl("bootstrap", a.domain(), a.PlistPath()); code != 0 {
		return &Error{Message: "launchctl bootstrap failed: " + output}
	}
	return nil
}

// Kickstart restarts the loaded job (killing the running backend).
func (a *Agent) Kickstart() error {
	if output, code := a.launchctl("kickstart", "-k", a.domain()+"/"+Label); code != 0 {
		return &Error{Message: "launchctl kickstart failed: " + output}
	}
	return nil
}

// Uninstall removes the agent (stopping its job); false if it wasn't there.
func (a *Agent) Uninstall() (bool, error) {
	loaded := a.IsLoaded()
	_, statErr := os.Stat(a.PlistPath())
	existed := loaded || statErr == nil
	if loaded {
		if err := a.bootout(); err != nil {
			return existed, err
		}
	}
	os.Remove(a.PlistPath())
	return existed, nil
}

func (a *Agent) bootout() error {
	if output, code := a.launchctl("bootout", a.domain()+"/"+Label); code != 0 {
		return &Error{Message: "launchctl bootout failed: " + output}
	}
	return nil
}

func plist(program []string, environment map[string]string, logPath string) string {
	return daemon.JobPlist(Label, program, environment, logPath, true)
}

// Program is the command launchd should run at login: this pr-mon executable,
// as a backend that stays up with no app or dashboard open.
func Program() []string {
	executable, err := os.Executable()
	if err != nil {
		if found, lookErr := exec.LookPath("pr-mon"); lookErr == nil {
			return []string{found, "daemon", "--keep-running"}
		}
		return []string{"pr-mon", "daemon", "--keep-running"}
	}
	resolved, err := filepath.EvalSymlinks(executable)
	if err == nil {
		executable = resolved
	}
	return []string{executable, "daemon", "--keep-running"}
}

func requireMacOS() error {
	if runtime.GOOS != "darwin" {
		return &Error{Message: "autostart is only supported on macOS"}
	}
	return nil
}

// Enable installs the login agent, hands the backend to launchd, and checks it.
func Enable(paths daemon.Paths, agent *Agent, program []string) ([]string, error) {
	if err := requireMacOS(); err != nil {
		return nil, err
	}
	if agent == nil {
		agent = NewAgent()
	}
	if program == nil {
		program = Program()
	}
	bundle := AppBundle(program[0])
	lines := []string{}
	// launchd should own the only backend; a second one would just exit.
	if _, err := daemon.Stop(paths, daemon.StopTimeout); err != nil {
		return nil, err
	}
	if bundle != "" {
		// An agent this command wrote earlier has the same label; clear it first.
		if _, err := agent.Uninstall(); err != nil {
			return nil, err
		}
		state, err := AppLoginItem(bundle, "on")
		if err != nil {
			return nil, err
		}
		if state == "requires-approval" {
			return []string{"autostart registered, but macOS needs you to allow it:\n" +
				"System Settings › General › Login Items › pr-mon"}, nil
		}
	} else if err := agent.Install(program, daemon.Environment(), paths.Log()); err != nil {
		return nil, err
	}
	check, err := waitUntilReady(paths, readyTimeout)
	if err != nil {
		// Don't leave launchd restarting a backend that can't start.
		if bundle != "" {
			AppLoginItem(bundle, "off")
		} else {
			agent.Uninstall()
		}
		return nil, err
	}
	what := "runs `" + strings.Join(program, " ") + "`"
	if bundle != "" {
		what = "listed as pr-mon in Login Items"
	}
	lines = append(lines, "autostart enabled: "+what+" at login", check)
	return lines, nil
}

// waitUntilReady waits for the launchd-started backend and reports whether
// GitHub access works.
func waitUntilReady(paths daemon.Paths, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for daemon.Info(paths) == nil {
		if time.Now().After(deadline) {
			return "", &Error{Message: strings.TrimSpace(
				"the backend did not start under launchd\n" + daemon.TailLog(paths))}
		}
		time.Sleep(200 * time.Millisecond)
	}
	for {
		result, err := daemon.Request(paths, "snapshot", nil)
		if err != nil {
			return "", &Error{Message: fmt.Sprintf("the backend stopped answering: %v", err)}
		}
		var snapshot struct {
			Config struct {
				Repos []string `json:"repos"`
			} `json:"config"`
			Errors map[string]string `json:"errors"`
			Status struct {
				LastUpdate *string `json:"last_update"`
			} `json:"status"`
		}
		if err := json.Unmarshal(result, &snapshot); err != nil {
			return "", &Error{Message: fmt.Sprintf("the backend sent something unreadable: %v", err)}
		}
		switch {
		case len(snapshot.Config.Repos) == 0:
			return "backend running (no repos yet, so GitHub access wasn't checked)", nil
		case snapshot.Status.LastUpdate != nil:
			return "backend running, GitHub access OK", nil
		case len(snapshot.Errors) > 0:
			for repo, message := range snapshot.Errors {
				return fmt.Sprintf("backend running, but GitHub reported for %s: %s", repo, message), nil
			}
		case time.Now().After(deadline):
			return "backend running; GitHub check still pending (see `pr-mon status`)", nil
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// AppLoginItem has the app register ("on"), remove ("off") or report ("status")
// its login item, and returns the state it reports: "enabled", "disabled" or
// "requires-approval". Only the app can register the agent it ships, through
// ServiceManagement, which is what makes Login Items name it pr-mon with its
// icon; run headless like this it opens no window and no Dock icon.
func AppLoginItem(bundle, action string) (string, error) {
	executable := filepath.Join(bundle, "Contents", "MacOS", "PrMon")
	output, err := exec.Command(executable, "--login-agent", action).CombinedOutput()
	text := strings.TrimSpace(string(output))
	if err != nil {
		if text == "" {
			text = err.Error()
		}
		return "", &Error{Message: "PrMon couldn't change its login item: " + text}
	}
	return text, nil
}

// AppBundle is the .app this path sits inside, or "" when it is a plain
// binary: a bundled pr-mon leaves the login item to the app (AppLoginItem).
func AppBundle(path string) string {
	resolved := path
	if link, err := filepath.EvalSymlinks(path); err == nil {
		resolved = link
	}
	for directory := filepath.Dir(resolved); ; directory = filepath.Dir(directory) {
		if filepath.Ext(directory) == ".app" {
			return directory
		}
		if parent := filepath.Dir(directory); parent == directory {
			return ""
		}
	}
}

// Disable removes the login agent, whichever kind is installed; a backend that
// was running keeps running.
func Disable(paths daemon.Paths, agent *Agent) (string, error) {
	if err := requireMacOS(); err != nil {
		return "", err
	}
	if agent == nil {
		agent = NewAgent()
	}
	wasRunning := daemon.Info(paths) != nil
	existed, err := agent.Uninstall()
	if err != nil {
		return "", err
	}
	if bundle := AppBundle(Program()[0]); bundle != "" {
		state, err := AppLoginItem(bundle, "status")
		if err != nil {
			return "", err
		}
		if state != "disabled" {
			if _, err := AppLoginItem(bundle, "off"); err != nil {
				return "", err
			}
			existed = true
		}
	}
	if !existed {
		return "autostart was not enabled", nil
	}
	if wasRunning {
		// Removing the agent stops its job; start an ordinary backend in its place.
		if _, err := daemon.Spawn(paths, daemon.StartTimeout); err != nil {
			return "", err
		}
		return "autostart disabled (the backend is still running)", nil
	}
	return "autostart disabled", nil
}

// Describe is (enabled, human-readable status).
func Describe(agent *Agent) (bool, string, error) {
	if err := requireMacOS(); err != nil {
		return false, "", err
	}
	if agent == nil {
		agent = NewAgent()
	}
	status := agent.Status()
	if bundle := AppBundle(Program()[0]); bundle != "" && !status.Installed {
		state, err := AppLoginItem(bundle, "status")
		switch {
		case err != nil:
			return false, "", err
		case state == "enabled":
			return true, "enabled (listed as pr-mon in Login Items)", nil
		case state == "requires-approval":
			return true, "enabled, waiting for approval in System Settings › General › Login Items", nil
		}
		return false, "disabled", nil
	}
	if !status.Installed {
		return false, "disabled", nil
	}
	runs := "unreadable agent file"
	if len(status.Program) > 0 {
		runs = "runs `" + strings.Join(status.Program, " ") + "`"
	}
	if status.Loaded {
		return true, "enabled (loaded in launchd, " + runs + ")", nil
	}
	return true, "enabled but not loaded (" + runs + "); run `pr-mon autostart enable` to fix", nil
}

// Restart restarts the backend, keeping it under launchd when autostart manages it.
func Restart(paths daemon.Paths, agent *Agent) (int, error) {
	if agent == nil {
		agent = NewAgent()
	}
	if runtime.GOOS != "darwin" || !agent.IsLoaded() {
		if _, err := daemon.Stop(paths, daemon.StopTimeout); err != nil {
			return 0, err
		}
		return daemon.Spawn(paths, daemon.StartTimeout)
	}
	oldPID := 0
	if hello := daemon.Info(paths); hello != nil && hello.PID != nil {
		oldPID = *hello.PID
	}
	if err := agent.Kickstart(); err != nil {
		return 0, err
	}
	deadline := time.Now().Add(daemon.StartTimeout)
	for time.Now().Before(deadline) {
		if hello := daemon.Info(paths); hello != nil && hello.PID != nil && *hello.PID != oldPID {
			return *hello.PID, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return 0, &Error{Message: strings.TrimSpace(
		"launchd did not restart the backend\n" + daemon.TailLog(paths))}
}
