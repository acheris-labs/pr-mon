// Package notify renders notification messages and delivers them by script or
// desktop notifier.
package notify

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/acheris-labs/pr-mon/internal/config"
	"github.com/acheris-labs/pr-mon/internal/models"
	"github.com/acheris-labs/pr-mon/internal/tracker"
)

const (
	AppTitle       = "pr-mon"
	CommandTimeout = 30 * time.Second
	ScriptHelp     = "The message is sent on stdin. PR_* variables are also set in the environment.\n" +
		"Runs without a shell: use full paths or ~ (no $VARS)."
)

// DesktopNotifiers are tried in order of preference.
var DesktopNotifiers = []string{"terminal-notifier", "osascript", "notify-send"}

// VariableNames are the placeholders a message template may use.
var VariableNames = []string{
	"PR_REPO", "PR_NUM", "PR_TITLE", "PR_AUTHOR",
	"PR_BRANCH", "PR_TARGET", "PR_STATE", "PR_URL", "PR_REASON",
}

// EventLabels describe each notification event to the user.
var EventLabels = map[string]string{
	"READY":        "Ready (mergeable)",
	"FAILING":      "Failing (checks failed)",
	"CONFLICT":     "Conflict (merge conflicts)",
	"BLOCKED":      "Blocked (reviews / branch protection)",
	"BEHIND":       "Behind base branch",
	"PENDING":      "Pending (checks running)",
	"NEW":          "New PR opened",
	"MERGED":       "Merged by pr-mon",
	"MERGE_FAILED": "pr-mon auto-merge failed",
}

var placeholder = regexp.MustCompile(`\{\{\s*([A-Za-z0-9_]+)\s*\}\}`)

var sampleVariables = map[string]string{
	"PR_REPO":   "owner/repo",
	"PR_NUM":    "123",
	"PR_TITLE":  "Example change",
	"PR_AUTHOR": "octocat",
	"PR_BRANCH": "feature",
	"PR_TARGET": "main",
	"PR_STATE":  "READY",
	"PR_URL":    "https://github.com/owner/repo/pull/123",
	"PR_REASON": "",
}

// Notification is one PR worth telling the user about.
type Notification struct {
	Repo  string
	PR    models.PullRequest
	State string // a Status value, or "NEW"
}

// EventOption is one event a settings editor offers.
type EventOption struct {
	Name  string `json:"name"`
	Label string `json:"label"`
}

// Form is what a notification settings editor offers, as the backend defines it.
type Form struct {
	Events     []EventOption       `json:"events"`
	Variables  []string            `json:"variables"`
	ScriptHelp string              `json:"script_help"`
	Defaults   config.NotifyConfig `json:"defaults"`
}

// Preview is a template rendered with sample values.
type Preview struct {
	Text    string   `json:"text"`
	Unknown []string `json:"unknown"`
}

// SampleVariables are example values for previews and test sends, using a real repo name.
func SampleVariables(repo string) map[string]string {
	variables := map[string]string{}
	for name, value := range sampleVariables {
		variables[name] = value
	}
	variables["PR_REPO"] = repo
	variables["PR_URL"] = fmt.Sprintf("https://github.com/%s/pull/%s", repo, sampleVariables["PR_NUM"])
	return variables
}

// Render substitutes {{VAR}} placeholders in one pass, so substituted values are
// never scanned for placeholders themselves.
func Render(template string, variables map[string]string) string {
	return placeholder.ReplaceAllStringFunc(template, func(match string) string {
		name := placeholder.FindStringSubmatch(match)[1]
		if value, found := variables[name]; found {
			return value
		}
		return match
	})
}

// UnknownPlaceholders are the template's placeholders we don't know, in order.
func UnknownPlaceholders(template string) []string {
	unknown := []string{}
	for _, match := range placeholder.FindAllStringSubmatch(template, -1) {
		name := match[1]
		if known(VariableNames, name) || known(unknown, name) {
			continue
		}
		unknown = append(unknown, name)
	}
	return unknown
}

func known(names []string, name string) bool {
	for _, candidate := range names {
		if candidate == name {
			return true
		}
	}
	return false
}

func NotificationForm() Form {
	events := []EventOption{}
	for _, name := range config.EventNames {
		events = append(events, EventOption{Name: name, Label: EventLabels[name]})
	}
	return Form{
		Events:     events,
		Variables:  VariableNames,
		ScriptHelp: ScriptHelp,
		Defaults:   config.NewNotifyConfig(),
	}
}

func PreviewMessage(repo, template string) Preview {
	return Preview{Text: Render(template, SampleVariables(repo)), Unknown: UnknownPlaceholders(template)}
}

func PRVariables(repo string, pr models.PullRequest, state, reason string) map[string]string {
	return map[string]string{
		"PR_REPO":   repo,
		"PR_NUM":    strconv.Itoa(pr.Number),
		"PR_TITLE":  pr.Title,
		"PR_AUTHOR": pr.Author,
		"PR_BRANCH": pr.HeadRef,
		"PR_TARGET": pr.BaseRef,
		"PR_STATE":  state,
		"PR_URL":    pr.URL,
		"PR_REASON": reason,
	}
}

// Select picks the changes this repo's settings ask to be notified about.
func Select(repo models.Repo, changes []tracker.Change, settings config.NotifyConfig) []Notification {
	byNumber := map[int]models.PullRequest{}
	for _, pr := range repo.PRs {
		byNumber[pr.Number] = pr
	}
	found := []Notification{}
	for _, change := range changes {
		pr, ok := byNumber[change.Number]
		if !ok || (pr.IsDraft && !settings.IncludeDrafts) {
			continue
		}
		state := string(change.New)
		if change.Old == "" {
			state = "NEW"
		}
		if known(settings.Events, state) {
			found = append(found, Notification{Repo: repo.Name, PR: pr, State: state})
		}
	}
	return found
}

// DetectDesktopNotifier is the path of the first available notifier, or "".
func DetectDesktopNotifier() string {
	for _, tool := range DesktopNotifiers {
		if path, err := exec.LookPath(tool); err == nil {
			return path
		}
	}
	return ""
}

func DesktopArgv(notifier, message string, variables map[string]string) []string {
	switch filepath.Base(notifier) {
	case "terminal-notifier":
		argv := []string{notifier, "-title", AppTitle, "-subtitle", variables["PR_REPO"],
			"-message", message}
		if url := variables["PR_URL"]; url != "" {
			argv = append(argv, "-open", url)
		}
		return argv
	case "osascript":
		// The message is passed as data to the script, never spliced into its source.
		return []string{notifier,
			"-e", "on run argv",
			"-e", "display notification (item 1 of argv) with title (item 2 of argv)",
			"-e", "end run",
			message, AppTitle}
	}
	return []string{notifier, "--", AppTitle, message}
}

// ScriptArgv splits a script command the way a shell would, without running one.
func ScriptArgv(command string) ([]string, error) {
	argv, err := split(command)
	if err != nil {
		return nil, err
	}
	if len(argv) == 0 {
		return nil, errors.New("script command is empty")
	}
	home, _ := os.UserHomeDir()
	for i, arg := range argv {
		if strings.HasPrefix(arg, "~") && home != "" {
			argv[i] = home + strings.TrimPrefix(arg, "~")
		}
	}
	return argv, nil
}

// split handles the quoting a shell would: single quotes, double quotes and
// backslashes. Anything else (variables, globs, pipes) is left as literal text.
func split(command string) ([]string, error) {
	argv := []string{}
	var current strings.Builder
	inWord := false
	runes := []rune(command)
	for i := 0; i < len(runes); i++ {
		switch char := runes[i]; char {
		case ' ', '\t', '\n':
			if inWord {
				argv = append(argv, current.String())
				current.Reset()
				inWord = false
			}
		case '\'':
			inWord = true
			end := i + 1
			for end < len(runes) && runes[end] != '\'' {
				end++
			}
			if end == len(runes) {
				return nil, errors.New("No closing quotation")
			}
			current.WriteString(string(runes[i+1 : end]))
			i = end
		case '"':
			inWord = true
			end := i + 1
			for end < len(runes) && runes[end] != '"' {
				if runes[end] == '\\' && end+1 < len(runes) {
					end++
				}
				end++
			}
			if end >= len(runes) {
				return nil, errors.New("No closing quotation")
			}
			current.WriteString(unescape(string(runes[i+1 : end])))
			i = end
		case '\\':
			inWord = true
			if i+1 < len(runes) {
				i++
				current.WriteRune(runes[i])
			}
		default:
			inWord = true
			current.WriteRune(char)
		}
	}
	if inWord {
		argv = append(argv, current.String())
	}
	return argv, nil
}

func unescape(text string) string {
	var out strings.Builder
	runes := []rune(text)
	for i := 0; i < len(runes); i++ {
		if runes[i] == '\\' && i+1 < len(runes) {
			i++
		}
		out.WriteRune(runes[i])
	}
	return out.String()
}

// Run executes argv without a shell and returns a message for the user, or "".
func Run(ctx context.Context, argv []string, stdin *string, extraEnv map[string]string,
	timeout time.Duration) string {
	name := filepath.Base(argv[0])
	command := exec.Command(argv[0], argv[1:]...)
	command.Env = os.Environ()
	for key, value := range extraEnv {
		command.Env = append(command.Env, key+"="+value)
	}
	if home, err := os.UserHomeDir(); err == nil {
		command.Dir = home
	}
	if stdin != nil {
		command.Stdin = strings.NewReader(*stdin)
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	// Its own process group, so a timeout can take helpers it started with it.
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		return fmt.Sprintf("%s: %v", name, err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case err := <-done:
		return commandError(name, err, stderr.String())
	case <-time.After(timeout):
		syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		<-done
		return fmt.Sprintf("%s timed out after %gs", name, timeout.Seconds())
	case <-ctx.Done():
		syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		<-done
		return ""
	}
}

func commandError(name string, err error, stderr string) string {
	if err == nil {
		return ""
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		return fmt.Sprintf("%s: %v", name, err)
	}
	detail := ""
	if lines := strings.Split(strings.TrimSpace(stderr), "\n"); lines[0] != "" {
		detail = ": " + lines[0]
	}
	return fmt.Sprintf("%s exited with code %d%s", name, exit.ExitCode(), detail)
}

// Result is one channel's outcome; Error is empty when it worked.
type Result struct {
	Channel string
	Error   string
}

// Deliver sends one notification on every enabled channel.
func Deliver(ctx context.Context, settings config.NotifyConfig, notifier string,
	variables map[string]string) []Result {
	message := Render(settings.Message, variables)
	results := []Result{}
	var mutex sync.Mutex
	var group sync.WaitGroup
	send := func(channel string, run func() string) {
		results = append(results, Result{Channel: channel})
		index := len(results) - 1
		group.Add(1)
		go func() {
			defer group.Done()
			problem := run()
			mutex.Lock()
			results[index].Error = problem
			mutex.Unlock()
		}()
	}
	if settings.ScriptEnabled && strings.TrimSpace(settings.Script) != "" {
		argv, err := ScriptArgv(settings.Script)
		if err != nil {
			send("script", func() string { return fmt.Sprintf("Bad script command: %v", err) })
		} else {
			send("script", func() string {
				return Run(ctx, argv, &message, variables, CommandTimeout)
			})
		}
	}
	if settings.DesktopEnabled && notifier != "" {
		argv := DesktopArgv(notifier, message, variables)
		send("desktop", func() string { return Run(ctx, argv, nil, nil, CommandTimeout) })
	}
	group.Wait()
	return results
}
