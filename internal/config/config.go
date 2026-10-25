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
	"slices"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/acheris-labs/pr-mon/internal/files"
)

const (
	// How often the backend looks at a repo nobody is looking at, the repo a
	// client has selected, and PRs that are moving (checks running, armed).
	DefaultPollInterval   = 120
	DefaultFocusInterval  = 60
	DefaultActiveInterval = 10
	DefaultMessage        = "{{PR_REPO}}#{{PR_NUM}} is {{PR_STATE}}: {{PR_TITLE}} {{PR_URL}}"
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

// Whose PRs a repo notifies about (NotifyConfig.NotifyFor).
const (
	NotifyMine     = "mine"
	NotifyBots     = "bots"
	NotifyWatched  = "watched"
	NotifyEveryone = "everyone"
)

// NotifyForNames are the NotifyFor values, in display order.
var NotifyForNames = []string{NotifyMine, NotifyBots, NotifyWatched, NotifyEveryone}

// DefaultNotifyFor is for a newly configured repo. A repo whose settings
// predate NotifyFor notifies for everyone, as it always did.
var DefaultNotifyFor = []string{NotifyMine, NotifyBots, NotifyWatched}

// NotifyConfig is one repo's notification settings.
type NotifyConfig struct {
	Message        string   `json:"message" toml:"message"`
	Events         []string `json:"events" toml:"events"`
	IncludeDrafts  bool     `json:"include_drafts" toml:"include_drafts"`
	ScriptEnabled  bool     `json:"script_enabled" toml:"script_enabled"`
	Script         string   `json:"script" toml:"script"`
	DesktopEnabled bool     `json:"desktop_enabled" toml:"desktop_enabled"`
	TwoWayEnabled  bool     `json:"two_way_enabled" toml:"two_way_enabled"`
	// Whose PRs notify: any of NotifyForNames. Empty means everyone's.
	NotifyFor []string `json:"notify_for" toml:"notify_for"`
}

// ForEveryone reports whether every PR notifies, whoever wrote it.
func (n NotifyConfig) ForEveryone() bool {
	return len(n.NotifyFor) == 0 || slices.Contains(n.NotifyFor, NotifyEveryone)
}

// LegacyNotifyConfig is the starting point for settings written before
// NotifyFor existed (in the file, or from an older client): those notified
// for everyone.
func LegacyNotifyConfig() NotifyConfig {
	settings := NewNotifyConfig()
	settings.NotifyFor = []string{NotifyEveryone}
	return settings
}

// TwoWayConfig is the backend-only [two_way] table: the handler program that
// sends two-way messages and relays taps.
type TwoWayConfig struct {
	Command string `toml:"command"`
	// Why the table as written can't be used (a value of the wrong type);
	// such a table is kept, so the rest of the config still loads.
	invalid string
	// The table has the keys of the old built-in Telegram setup and no command.
	legacy bool
}

// Problem is "" when the table is usable, otherwise a sentence saying why not.
// Whether the command splits into words is the service's check (config can't
// import notify).
func (t TwoWayConfig) Problem() string {
	switch {
	case t.invalid != "":
		return t.invalid
	case t.legacy:
		return "[two_way] now takes a handler command; the AWS settings belong to the handler"
	case strings.TrimSpace(t.Command) == "":
		return "[two_way] command is missing"
	}
	return ""
}

func NewNotifyConfig() NotifyConfig {
	return NotifyConfig{
		Message:   DefaultMessage,
		Events:    append([]string{}, DefaultEvents...),
		NotifyFor: append([]string{}, DefaultNotifyFor...),
	}
}

// Equal reports whether two settings would be saved identically.
func (n NotifyConfig) Equal(other NotifyConfig) bool {
	if n.Message != other.Message || n.IncludeDrafts != other.IncludeDrafts ||
		n.ScriptEnabled != other.ScriptEnabled || n.Script != other.Script ||
		n.DesktopEnabled != other.DesktopEnabled || n.TwoWayEnabled != other.TwoWayEnabled {
		return false
	}
	return slices.Equal(n.Events, other.Events) && slices.Equal(n.NotifyFor, other.NotifyFor)
}

type Config struct {
	Repos        []string `json:"repos"`
	PollInterval int      `json:"poll_interval"`
	// The repo a client has selected, and PRs in flight, are looked at more
	// often; unchanged repos cost nothing (the checks are conditional).
	FocusInterval  int `json:"focus_interval"`
	ActiveInterval int `json:"active_interval"`
	// A repo without an entry never notifies.
	Notifications map[string]NotifyConfig `json:"notifications"`
	// Backend-only: the user's GitHub logins (empty: whoever gh is logged in
	// as), accounts to treat as bots besides the ones GitHub marks, and
	// people whose PRs the user wants to hear about.
	MyLogins      []string `json:"-"`
	BotLogins     []string `json:"-"`
	WatchedLogins []string `json:"-"`
	// Backend-only; never sent to clients.
	TwoWay *TwoWayConfig `json:"-"`
	// Why the file on disk was ignored, if it was. Saving would then replace
	// the user's file with defaults, so Save refuses.
	Ignored string `json:"-"`
}

func New() Config {
	return Config{
		Repos:          []string{},
		PollInterval:   DefaultPollInterval,
		FocusInterval:  DefaultFocusInterval,
		ActiveInterval: DefaultActiveInterval,
		Notifications:  map[string]NotifyConfig{},
	}
}

func DefaultPath() string {
	dir := files.AppDir("XDG_CONFIG_HOME", filepath.Join(files.Home(), ".config"))
	return filepath.Join(dir, "config.toml")
}

// raw mirrors the file so unset keys can be told from zero values.
type raw struct {
	Repos          []string                  `toml:"repos"`
	PollInterval   *int                      `toml:"poll_interval"`
	FocusInterval  *int                      `toml:"focus_interval"`
	ActiveInterval *int                      `toml:"active_interval"`
	Notifications  map[string]toml.Primitive `toml:"notifications"`
	TwoWay         *toml.Primitive           `toml:"two_way"`
	MyLogins       []string                  `toml:"my_logins"`
	BotLogins      []string                  `toml:"bot_logins"`
	WatchedLogins  []string                  `toml:"watched_logins"`
}

// Load returns the config and a warning if the file was unusable.
func Load(path string) (Config, string) {
	text, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return New(), ""
	}
	if err != nil {
		return ignore(fmt.Sprintf("Ignoring unreadable config %s: %v", path, err))
	}
	var parsed raw
	meta, err := toml.Decode(string(text), &parsed)
	if err != nil {
		return ignore(fmt.Sprintf("Ignoring unreadable config %s: %v", path, err))
	}
	if parsed.PollInterval != nil && *parsed.PollInterval <= 0 {
		return ignore(fmt.Sprintf("Ignoring invalid config %s: bad 'repos' or 'poll_interval'", path))
	}
	config := New()
	if parsed.Repos != nil {
		config.Repos = parsed.Repos
	}
	if parsed.PollInterval != nil {
		config.PollInterval = *parsed.PollInterval
	}
	if parsed.FocusInterval != nil && *parsed.FocusInterval > 0 {
		config.FocusInterval = *parsed.FocusInterval
	}
	if parsed.ActiveInterval != nil && *parsed.ActiveInterval > 0 {
		config.ActiveInterval = *parsed.ActiveInterval
	}
	if parsed.TwoWay != nil {
		config.TwoWay = parseTwoWay(meta, *parsed.TwoWay)
	}
	config.MyLogins = parsed.MyLogins
	config.BotLogins = parsed.BotLogins
	config.WatchedLogins = parsed.WatchedLogins
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

// ignore is the defaults, marked so they are never saved over the file.
func ignore(warning string) (Config, string) {
	config := New()
	config.Ignored = warning
	return config, warning
}

// parseNotify builds settings from a TOML table; not ok when a value has the
// wrong type or an event or notify_for value isn't one we know.
func parseNotify(meta toml.MetaData, table toml.Primitive) (NotifyConfig, bool) {
	settings := LegacyNotifyConfig()
	if err := meta.PrimitiveDecode(table, &settings); err != nil {
		return NotifyConfig{}, false
	}
	if UnknownSetting(settings) != "" {
		return NotifyConfig{}, false
	}
	return settings, true
}

// UnknownSetting is the first event or notify_for value that isn't one we
// know, or "" when there is none.
func UnknownSetting(settings NotifyConfig) string {
	for _, event := range settings.Events {
		if !KnownEvent(event) {
			return fmt.Sprintf("unknown event %q", event)
		}
	}
	for _, name := range settings.NotifyFor {
		if !slices.Contains(NotifyForNames, name) {
			return fmt.Sprintf("unknown notify_for %q", name)
		}
	}
	return ""
}

// parseTwoWay builds the [two_way] table on its own, like each repo's
// notifications, so a wrongly typed value spoils only the table.
func parseTwoWay(meta toml.MetaData, table toml.Primitive) *TwoWayConfig {
	values := map[string]any{}
	if err := meta.PrimitiveDecode(table, &values); err != nil {
		return &TwoWayConfig{invalid: "[two_way] must be a table"}
	}
	parsed := &TwoWayConfig{}
	switch command := values["command"].(type) {
	case nil:
		for _, key := range []string{"kind", "region", "queue_url", "sender_function", "route", "chat_id"} {
			if _, found := values[key]; found {
				parsed.legacy = true
			}
		}
	case string:
		parsed.Command = command
	default:
		parsed.invalid = "[two_way] command must be a string"
	}
	return parsed
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

// notifyFor is the value to save: an empty list means everyone, and is
// written as such so it isn't read back as a legacy repo.
func notifyFor(settings NotifyConfig) []string {
	if len(settings.NotifyFor) == 0 {
		return []string{NotifyEveryone}
	}
	return settings.NotifyFor
}

func Save(path string, config Config) error {
	if config.Ignored != "" {
		return errors.New(config.Ignored + "; not saving over it")
	}
	lines := []string{
		fmt.Sprintf("poll_interval = %d", config.PollInterval),
		fmt.Sprintf("focus_interval = %d", config.FocusInterval),
		fmt.Sprintf("active_interval = %d", config.ActiveInterval),
		"repos = [",
	}
	for _, repo := range config.Repos {
		lines = append(lines, fmt.Sprintf("    %s,", tomlValue(repo)))
	}
	lines = append(lines, "]")
	if len(config.MyLogins) > 0 {
		lines = append(lines, fmt.Sprintf("my_logins = %s", tomlValue(config.MyLogins)))
	}
	if len(config.BotLogins) > 0 {
		lines = append(lines, fmt.Sprintf("bot_logins = %s", tomlValue(config.BotLogins)))
	}
	if len(config.WatchedLogins) > 0 {
		lines = append(lines, fmt.Sprintf("watched_logins = %s", tomlValue(config.WatchedLogins)))
	}
	if tw := config.TwoWay; tw != nil {
		lines = append(lines, "", "[two_way]", fmt.Sprintf("command = %s", tomlValue(tw.Command)))
	}
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
			fmt.Sprintf("two_way_enabled = %s", tomlValue(settings.TwoWayEnabled)),
			fmt.Sprintf("notify_for = %s", tomlValue(notifyFor(settings))),
		)
	}
	return files.WriteAtomic(path, strings.Join(lines, "\n")+"\n")
}
