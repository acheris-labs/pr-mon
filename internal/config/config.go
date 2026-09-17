// Package config reads and writes config.toml: the repos to watch, how often to
// poll, and each repo's notification settings.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/acheris-labs/pr-mon/internal/files"
)

const (
	DefaultPollInterval = 60
	DefaultMessage      = "{{PR_REPO}}#{{PR_NUM}} is {{PR_STATE}}: {{PR_TITLE}} {{PR_URL}}"
)

// TOML rejects a raw DEL, the one control character JSON leaves unescaped.
var (
	del       = string(rune(0x7f))
	delEscape = "\\u007f"
)

// EventNames are the notification events, in display order.
var EventNames = []string{
	"READY", "FAILING", "CONFLICT", "BLOCKED", "BEHIND", "PENDING", "NEW", "MERGED", "MERGE_FAILED",
}

// DefaultEvents are on for a newly configured repo.
var DefaultEvents = []string{"READY", "FAILING", "CONFLICT", "MERGED", "MERGE_FAILED"}

// NotifyConfig is one repo's notification settings.
type NotifyConfig struct {
	Message        string   `json:"message" toml:"message"`
	Events         []string `json:"events" toml:"events"`
	IncludeDrafts  bool     `json:"include_drafts" toml:"include_drafts"`
	ScriptEnabled  bool     `json:"script_enabled" toml:"script_enabled"`
	Script         string   `json:"script" toml:"script"`
	DesktopEnabled bool     `json:"desktop_enabled" toml:"desktop_enabled"`
}

func NewNotifyConfig() NotifyConfig {
	return NotifyConfig{Message: DefaultMessage, Events: append([]string{}, DefaultEvents...)}
}

// Equal reports whether two settings would be saved identically.
func (n NotifyConfig) Equal(other NotifyConfig) bool {
	if n.Message != other.Message || n.IncludeDrafts != other.IncludeDrafts ||
		n.ScriptEnabled != other.ScriptEnabled || n.Script != other.Script ||
		n.DesktopEnabled != other.DesktopEnabled || len(n.Events) != len(other.Events) {
		return false
	}
	for i := range n.Events {
		if n.Events[i] != other.Events[i] {
			return false
		}
	}
	return true
}

type Config struct {
	Repos        []string `json:"repos"`
	PollInterval int      `json:"poll_interval"`
	// A repo without an entry never notifies.
	Notifications map[string]NotifyConfig `json:"notifications"`
}

func New() Config {
	return Config{
		Repos:         []string{},
		PollInterval:  DefaultPollInterval,
		Notifications: map[string]NotifyConfig{},
	}
}

func DefaultPath() string {
	dir := files.AppDir("XDG_CONFIG_HOME", filepath.Join(files.Home(), ".config"))
	return filepath.Join(dir, "config.toml")
}

// raw mirrors the file so unset keys can be told from zero values.
type raw struct {
	Repos         []string                  `toml:"repos"`
	PollInterval  *int                      `toml:"poll_interval"`
	Notifications map[string]toml.Primitive `toml:"notifications"`
}

// Load returns the config and a warning if the file was unusable.
func Load(path string) (Config, string) {
	text, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return New(), ""
	}
	if err != nil {
		return New(), fmt.Sprintf("Ignoring unreadable config %s: %v", path, err)
	}
	var parsed raw
	meta, err := toml.Decode(string(text), &parsed)
	if err != nil {
		return New(), fmt.Sprintf("Ignoring unreadable config %s: %v", path, err)
	}
	if parsed.PollInterval != nil && *parsed.PollInterval <= 0 {
		return New(), fmt.Sprintf("Ignoring invalid config %s: bad 'repos' or 'poll_interval'", path)
	}
	config := New()
	if parsed.Repos != nil {
		config.Repos = parsed.Repos
	}
	if parsed.PollInterval != nil {
		config.PollInterval = *parsed.PollInterval
	}
	problems := []string{}
	for repo, table := range parsed.Notifications {
		settings, ok := parseNotify(meta, table)
		if !ok {
			problems = append(problems,
				fmt.Sprintf("ignoring invalid notification settings for %s", repo))
			continue
		}
		config.Notifications[repo] = settings
	}
	if len(problems) > 0 {
		return config, fmt.Sprintf("%s: %s", path, strings.Join(problems, "; "))
	}
	return config, ""
}

// parseNotify builds settings from a TOML table; not ok when a value has the
// wrong type or an event isn't one we know.
func parseNotify(meta toml.MetaData, table toml.Primitive) (NotifyConfig, bool) {
	settings := NewNotifyConfig()
	if err := meta.PrimitiveDecode(table, &settings); err != nil {
		return NotifyConfig{}, false
	}
	for _, event := range settings.Events {
		if !KnownEvent(event) {
			return NotifyConfig{}, false
		}
	}
	return settings, true
}

func KnownEvent(event string) bool {
	for _, name := range EventNames {
		if name == event {
			return true
		}
	}
	return false
}

// tomlValue writes one value as TOML. JSON string escapes are valid TOML
// basic-string escapes, except \u surrogate pairs, so non-ASCII text is written
// literally; TOML also forbids a raw DEL, which JSON leaves unescaped.
func tomlValue(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return `""`
	}
	return strings.ReplaceAll(string(encoded), del, delEscape)
}

func Save(path string, config Config) error {
	lines := []string{fmt.Sprintf("poll_interval = %d", config.PollInterval), "repos = ["}
	for _, repo := range config.Repos {
		lines = append(lines, fmt.Sprintf("    %s,", tomlValue(repo)))
	}
	lines = append(lines, "]")
	for _, repo := range config.Repos {
		settings, found := config.Notifications[repo]
		if !found {
			continue
		}
		lines = append(lines, "",
			fmt.Sprintf("[notifications.%s]", tomlValue(repo)),
			fmt.Sprintf("message = %s", tomlValue(settings.Message)),
			fmt.Sprintf("events = %s", tomlValue(settings.Events)),
			fmt.Sprintf("include_drafts = %s", tomlValue(settings.IncludeDrafts)),
			fmt.Sprintf("script_enabled = %s", tomlValue(settings.ScriptEnabled)),
			fmt.Sprintf("script = %s", tomlValue(settings.Script)),
			fmt.Sprintf("desktop_enabled = %s", tomlValue(settings.DesktopEnabled)),
		)
	}
	return files.WriteAtomic(path, strings.Join(lines, "\n")+"\n")
}
