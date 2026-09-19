// Starting the backend through launchd on macOS, and the job plists that takes.

package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// SessionLabel is the launchd job `pr-mon start` runs the backend as on macOS.
// A backend started as a child of the app would belong to the app: macOS then
// keeps the app in the Dock, "Running in Background", after it quits.
const SessionLabel = "com.acheris-labs.pr-mon.session"

// KeptEnv is what a launchd job keeps of the environment it was set up from:
// launchd starts jobs with a bare one, and the backend needs gh (and any
// notification script) on PATH.
var KeptEnv = []string{"PATH", "XDG_CONFIG_HOME", "XDG_STATE_HOME", "LANG"}

// Environment is the current process's values for KeptEnv.
func Environment() map[string]string {
	kept := map[string]string{}
	for _, name := range KeptEnv {
		if value, found := os.LookupEnv(name); found {
			kept[name] = value
		}
	}
	return kept
}

// JobPlist is a launchd job running `program`, with its output in logPath.
// keepAlive restarts it after a crash (but not after a requested shutdown).
func JobPlist(label string, program []string, environment map[string]string, logPath string,
	keepAlive bool) string {
	var builder strings.Builder
	builder.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	builder.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" ` +
		`"http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	builder.WriteString("<plist version=\"1.0\">\n<dict>\n")
	builder.WriteString("  <key>Label</key>\n  <string>" + EscapeXML(label) + "</string>\n")
	builder.WriteString("  <key>ProgramArguments</key>\n  <array>\n")
	for _, argument := range program {
		builder.WriteString("    <string>" + EscapeXML(argument) + "</string>\n")
	}
	builder.WriteString("  </array>\n")
	builder.WriteString("  <key>RunAtLoad</key>\n  <true/>\n")
	if keepAlive {
		builder.WriteString("  <key>KeepAlive</key>\n  <dict>\n" +
			"    <key>SuccessfulExit</key>\n    <false/>\n  </dict>\n")
	}
	builder.WriteString("  <key>EnvironmentVariables</key>\n  <dict>\n")
	for _, name := range KeptEnv {
		if value, found := environment[name]; found {
			builder.WriteString("    <key>" + name + "</key>\n    <string>" +
				EscapeXML(value) + "</string>\n")
		}
	}
	builder.WriteString("  </dict>\n")
	// Output before the backend's own logging starts (a crash, say) lands here too.
	builder.WriteString("  <key>StandardOutPath</key>\n  <string>" + EscapeXML(logPath) + "</string>\n")
	builder.WriteString("  <key>StandardErrorPath</key>\n  <string>" + EscapeXML(logPath) + "</string>\n")
	builder.WriteString("</dict>\n</plist>\n")
	return builder.String()
}

func EscapeXML(text string) string {
	replacer := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return replacer.Replace(text)
}

func UnescapeXML(text string) string {
	replacer := strings.NewReplacer("&amp;", "&", "&lt;", "<", "&gt;", ">")
	return replacer.Replace(text)
}

func sessionTarget() string { return fmt.Sprintf("gui/%d/%s", os.Getuid(), SessionLabel) }

// launchSession hands the backend to launchd as a one-shot job: no KeepAlive,
// so `pr-mon stop` stays stopped. A job left from an earlier start is replaced.
func launchSession(paths Paths, executable string) error {
	plistPath := filepath.Join(paths.Directory, "session.plist")
	text := JobPlist(SessionLabel, []string{executable, "daemon"}, Environment(), paths.Log(), false)
	if err := os.WriteFile(plistPath, []byte(text), 0o644); err != nil {
		return err
	}
	// Already loaded (on demand): just run it. Loading is what macOS charges to
	// the caller, and an app charged with a job stays in the Dock.
	if exec.Command("launchctl", "print", sessionTarget()).Run() == nil {
		if output, err := exec.Command("launchctl", "kickstart", sessionTarget()).CombinedOutput(); err != nil {
			return fmt.Errorf("launchctl kickstart failed: %s", strings.TrimSpace(string(output)))
		}
		return nil
	}
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	output, err := exec.Command("launchctl", "bootstrap", domain, plistPath).CombinedOutput()
	if err != nil {
		return fmt.Errorf("launchctl bootstrap failed: %s", strings.TrimSpace(string(output)))
	}
	return nil
}

var lastExit = regexp.MustCompile(`last exit code = (-?\d+)`)

// sessionExited reports whether the session job ran and exited, and its code.
func sessionExited() (int, bool) {
	output, err := exec.Command("launchctl", "print", sessionTarget()).Output()
	if err != nil || strings.Contains(string(output), "state = running") {
		return 0, false
	}
	match := lastExit.FindSubmatch(output)
	if match == nil {
		return 0, false // "(never exited)": still starting
	}
	code, _ := strconv.Atoi(string(match[1]))
	return code, true
}
