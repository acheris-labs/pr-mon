// Package protocol is the wire format between the backend and its clients: one
// JSON object per line. The full specification is docs/protocol.md.
//
// Request:  {"id": 7, "op": "mark_seen", "args": {...}}
// Response: {"id": 7, "ok": true, "result": ...} or {"id": 7, "ok": false, "error": "..."}
// Event:    {"event": "repo", "data": {...}}
package protocol

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"

	"github.com/acheris-labs/pr-mon/internal/config"
	"github.com/acheris-labs/pr-mon/internal/models"
	"github.com/acheris-labs/pr-mon/internal/service"
)

// Version is bumped when a client written against the old docs/protocol.md
// would break; clients refuse a backend speaking another version.
const Version = 1

// EventKinds are every event the backend pushes.
var EventKinds = []string{"repos", "repo", "seen", "collapsed", "config", "status", "toast"}

// ActionKinds are every action a client may ask for.
var ActionKinds = []string{
	"merge", "update", "auto_merge_on", "auto_merge_off", "arm_merge", "disarm_merge",
}

// Request is what a client sends.
type Request struct {
	ID   int             `json:"id"`
	Op   string          `json:"op"`
	Args json.RawMessage `json:"args"`
}

// Response is the reply to one request. A successful reply always carries
// result, null when the op has nothing to return: omitting the key breaks
// clients that decode it as a required field.
type Response struct {
	ID     int    `json:"id"`
	OK     bool   `json:"ok"`
	Result any    `json:"result"`
	Error  string `json:"error,omitempty"`
}

// EventMessage is a pushed change.
type EventMessage struct {
	Event string `json:"event"`
	Data  any    `json:"data"`
}

// Hello tells a client what it is connected to.
type Hello struct {
	Protocol int     `json:"protocol"`
	Version  string  `json:"version"`
	PID      *int    `json:"pid"`
	Notifier *string `json:"notifier"`
	// A client restarting an outdated backend waits while this is true.
	Merging bool `json:"merging"`
}

// ArmedByNumber is a repo's armed merges; PR numbers are JSON keys, so strings.
type ArmedByNumber map[string]models.ArmedMerge

// Snapshot is everything a client needs to render.
type Snapshot struct {
	Config    config.Config            `json:"config"`
	Repos     map[string]models.Repo   `json:"repos"`
	Errors    map[string]string        `json:"errors"`
	Unseen    map[string][]int         `json:"unseen"`
	Collapsed []string                 `json:"collapsed"`
	Armed     map[string]ArmedByNumber `json:"armed"`
	Status    service.Status           `json:"status"`
}

// RepoUpdate is one repo's part of a snapshot.
type RepoUpdate struct {
	Name   string        `json:"name"`
	Repo   *models.Repo  `json:"repo"`
	Error  *string       `json:"error"`
	Unseen []int         `json:"unseen"`
	Armed  ArmedByNumber `json:"armed"`
}

type SeenUpdate struct {
	Name   string `json:"name"`
	Unseen []int  `json:"unseen"`
}

type CollapsedUpdate struct {
	Collapsed []string `json:"collapsed"`
}

type ConfigUpdate struct {
	Config config.Config `json:"config"`
}

type StatusUpdate struct {
	Status service.Status `json:"status"`
}

type Toast struct {
	Message  string `json:"message"`
	Severity string `json:"severity"`
}

// Source is the backend state the protocol reads.
type Source interface {
	Config() config.Config
	Repos() map[string]models.Repo
	Repo(name string) (models.Repo, bool)
	Errors() map[string]string
	Unseen(name string) []int
	Collapsed() []string
	Armed(name string) map[int]models.ArmedMerge
	Status() service.Status
}

func armedOf(source Source, name string) ArmedByNumber {
	armed := ArmedByNumber{}
	for number, merge := range source.Armed(name) {
		armed[strconv.Itoa(number)] = merge
	}
	return armed
}

// Of builds a snapshot from any backend.
func Of(source Source) Snapshot {
	snapshot := Snapshot{
		Config:    source.Config(),
		Repos:     source.Repos(),
		Errors:    source.Errors(),
		Unseen:    map[string][]int{},
		Collapsed: source.Collapsed(),
		Armed:     map[string]ArmedByNumber{},
		Status:    source.Status(),
	}
	for _, name := range snapshot.Config.Repos {
		snapshot.Unseen[name] = source.Unseen(name)
		snapshot.Armed[name] = armedOf(source, name)
	}
	sort.Strings(snapshot.Collapsed)
	return snapshot
}

// EventFor is an event's wire form, carrying the data a client mirror needs.
func EventFor(source Source, event service.Event) (EventMessage, error) {
	switch event.Kind {
	case "repos":
		return EventMessage{Event: event.Kind, Data: Of(source)}, nil
	case "repo":
		update := RepoUpdate{
			Name:   event.Name,
			Unseen: source.Unseen(event.Name),
			Armed:  armedOf(source, event.Name),
		}
		if repo, found := source.Repo(event.Name); found {
			update.Repo = &repo
		}
		if message, found := source.Errors()[event.Name]; found {
			update.Error = &message
		}
		return EventMessage{Event: event.Kind, Data: update}, nil
	case "seen":
		return EventMessage{Event: event.Kind,
			Data: SeenUpdate{Name: event.Name, Unseen: source.Unseen(event.Name)}}, nil
	case "collapsed":
		collapsed := source.Collapsed()
		sort.Strings(collapsed)
		return EventMessage{Event: event.Kind, Data: CollapsedUpdate{Collapsed: collapsed}}, nil
	case "config":
		return EventMessage{Event: event.Kind, Data: ConfigUpdate{Config: source.Config()}}, nil
	case "status":
		return EventMessage{Event: event.Kind, Data: StatusUpdate{Status: source.Status()}}, nil
	case "toast":
		return EventMessage{Event: event.Kind,
			Data: Toast{Message: event.Message, Severity: event.Severity}}, nil
	}
	return EventMessage{}, fmt.Errorf("unknown event kind %q", event.Kind)
}

// ParseAction reads an action from a client, rejecting kinds we don't know.
func ParseAction(raw json.RawMessage) (models.Action, error) {
	var action models.Action
	if err := json.Unmarshal(raw, &action); err != nil {
		return models.Action{}, fmt.Errorf("invalid action: %v", err)
	}
	for _, kind := range ActionKinds {
		if action.Kind == kind {
			if action.Method != nil {
				switch *action.Method {
				case models.MergeSquash, models.MergeCommit, models.MergeRebase:
				default:
					return models.Action{},
						fmt.Errorf("invalid action: unknown merge method %q", *action.Method)
				}
			}
			return action, nil
		}
	}
	return models.Action{}, fmt.Errorf("invalid action: unknown kind %q", action.Kind)
}

// ParseSettings reads notification settings from a client.
func ParseSettings(raw json.RawMessage) (config.NotifyConfig, error) {
	settings := config.NewNotifyConfig()
	if err := json.Unmarshal(raw, &settings); err != nil {
		return config.NotifyConfig{}, fmt.Errorf("invalid notification settings: %v", err)
	}
	for _, event := range settings.Events {
		if !config.KnownEvent(event) {
			return config.NotifyConfig{},
				fmt.Errorf("invalid notification settings: unknown event %q", event)
		}
	}
	return settings, nil
}

// Encode writes one message as a line. JSON escapes newlines inside strings, so
// the message stays on one line.
func Encode(message any) ([]byte, error) {
	line, err := json.Marshal(message)
	if err != nil {
		return nil, err
	}
	return append(line, '\n'), nil
}
