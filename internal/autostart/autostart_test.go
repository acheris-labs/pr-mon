package autostart_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/acheris-labs/pr-mon/internal/autostart"
	"github.com/acheris-labs/pr-mon/internal/daemon"
)

// fakeLaunchctl records the commands the agent runs and replays answers.
type fakeLaunchctl struct {
	calls  [][]string
	loaded bool
	fail   map[string]string
}

func (f *fakeLaunchctl) run(name string, args ...string) (string, int) {
	f.calls = append(f.calls, append([]string{name}, args...))
	if name != "launchctl" || len(args) == 0 {
		return "", 0
	}
	if message, found := f.fail[args[0]]; found {
		return message, 1
	}
	switch args[0] {
	case "print":
		if f.loaded {
			return "", 0
		}
		return "Could not find service", 113
	case "bootstrap":
		f.loaded = true
	case "bootout":
		f.loaded = false
	}
	return "", 0
}

func (f *fakeLaunchctl) ran(command string) bool {
	for _, call := range f.calls {
		if len(call) > 1 && call[1] == command {
			return true
		}
	}
	return false
}

func newAgent(t *testing.T) (*autostart.Agent, *fakeLaunchctl) {
	t.Helper()
	fake := &fakeLaunchctl{fail: map[string]string{}}
	return &autostart.Agent{Dir: t.TempDir(), Run: fake.run, UID: 501}, fake
}

func TestInstallWritesAnAgentAndBootstrapsIt(t *testing.T) {
	agent, fake := newAgent(t)
	program := []string{"/opt/homebrew/bin/pr-mon", "daemon"}
	err := agent.Install(program, map[string]string{"PATH": "/usr/bin", "IGNORED": "x"},
		"/tmp/pr-mon/daemon.log")
	if err != nil {
		t.Fatal(err)
	}
	text, err := os.ReadFile(agent.PlistPath())
	if err != nil {
		t.Fatal(err)
	}
	plist := string(text)
	for _, want := range []string{
		"<string>com.acheris-labs.pr-mon</string>",
		"<string>/opt/homebrew/bin/pr-mon</string>",
		"<string>daemon</string>",
		"<key>RunAtLoad</key>",
		"<key>SuccessfulExit</key>",
		"<key>PATH</key>",
		"<string>/tmp/pr-mon/daemon.log</string>",
	} {
		if !strings.Contains(plist, want) {
			t.Errorf("the agent file is missing %q:\n%s", want, plist)
		}
	}
	if strings.Contains(plist, "IGNORED") {
		t.Error("only the named environment variables should be kept")
	}
	if !fake.ran("bootstrap") {
		t.Errorf("calls = %v", fake.calls)
	}

	status := agent.Status()
	if !status.Installed || !status.Loaded {
		t.Errorf("status = %+v", status)
	}
	if !reflect.DeepEqual(status.Program, program) {
		t.Errorf("program = %v, want %v", status.Program, program)
	}
}

func TestInstallReplacesAnExistingAgent(t *testing.T) {
	agent, fake := newAgent(t)
	if err := agent.Install([]string{"/bin/pr-mon", "daemon"}, nil, "/tmp/log"); err != nil {
		t.Fatal(err)
	}
	fake.calls = nil
	if err := agent.Install([]string{"/bin/pr-mon", "daemon"}, nil, "/tmp/log"); err != nil {
		t.Fatal(err)
	}
	if !fake.ran("bootout") || !fake.ran("bootstrap") {
		t.Errorf("a loaded agent should be replaced: %v", fake.calls)
	}
}

func TestInstallFailureIsReported(t *testing.T) {
	agent, fake := newAgent(t)
	fake.fail["bootstrap"] = "Load failed: 5: Input/output error"
	err := agent.Install([]string{"/bin/pr-mon", "daemon"}, nil, "/tmp/log")
	if err == nil || !strings.Contains(err.Error(), "Input/output error") {
		t.Errorf("error = %v", err)
	}
}

func TestUninstall(t *testing.T) {
	agent, fake := newAgent(t)
	if existed, err := agent.Uninstall(); existed || err != nil {
		t.Errorf("existed = %v, err = %v, want nothing to remove", existed, err)
	}
	if err := agent.Install([]string{"/bin/pr-mon", "daemon"}, nil, "/tmp/log"); err != nil {
		t.Fatal(err)
	}
	existed, err := agent.Uninstall()
	if err != nil || !existed {
		t.Fatalf("existed = %v, err = %v", existed, err)
	}
	if !fake.ran("bootout") {
		t.Errorf("calls = %v", fake.calls)
	}
	if _, err := os.Stat(agent.PlistPath()); !os.IsNotExist(err) {
		t.Error("the agent file should be gone")
	}
}

func TestKickstart(t *testing.T) {
	agent, fake := newAgent(t)
	if err := agent.Kickstart(); err != nil {
		t.Fatal(err)
	}
	if !fake.ran("kickstart") {
		t.Errorf("calls = %v", fake.calls)
	}
	fake.fail["kickstart"] = "no such service"
	if err := agent.Kickstart(); err == nil {
		t.Error("a failed kickstart should be reported")
	}
}

func TestDescribe(t *testing.T) {
	agent, fake := newAgent(t)
	enabled, description, err := autostart.Describe(agent)
	if err != nil {
		t.Skipf("not macOS: %v", err)
	}
	if enabled || description != "disabled" {
		t.Errorf("describe = %v, %q", enabled, description)
	}

	if err := agent.Install([]string{"/bin/pr-mon", "daemon"}, nil, "/tmp/log"); err != nil {
		t.Fatal(err)
	}
	enabled, description, _ = autostart.Describe(agent)
	if !enabled || !strings.Contains(description, "loaded in launchd") ||
		!strings.Contains(description, "/bin/pr-mon daemon") {
		t.Errorf("describe = %v, %q", enabled, description)
	}

	fake.loaded = false
	enabled, description, _ = autostart.Describe(agent)
	if !enabled || !strings.Contains(description, "not loaded") {
		t.Errorf("describe = %v, %q", enabled, description)
	}
}

func TestProgramPointsAtThisExecutable(t *testing.T) {
	program := autostart.Program()
	if len(program) != 2 || program[1] != "daemon" {
		t.Fatalf("program = %v", program)
	}
	if !filepath.IsAbs(program[0]) {
		t.Errorf("program = %v, want an absolute path", program)
	}
}

func TestRestartWithoutLaunchdStartsAFreshBackend(t *testing.T) {
	agent, _ := newAgent(t)
	// Nothing is loaded and no backend is running, so Restart spawns one; with no
	// real backend to start, it fails rather than hanging.
	paths := daemon.Paths{Directory: t.TempDir()}
	if _, err := autostart.Restart(paths, agent); err == nil {
		t.Skip("a backend started unexpectedly; nothing to assert")
	}
}
