// Package state persists what the UI remembers: per-PR status and seen flags,
// collapsed repo groups, and PRs armed for merge-when-ready.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/acheris-labs/pr-mon/internal/files"
)

// Record is one PR's tracked status.
type Record struct {
	Status string `json:"status"`
	Seen   bool   `json:"seen"`
}

// AppState is state.json. Fields are alphabetical because that's the order the
// file has always been written in.
type AppState struct {
	// Armed PRs: {repo: {"<number>": {...}}}
	Armed     map[string]map[string]json.RawMessage `json:"armed"`
	Collapsed []string                              `json:"collapsed"` // lower-cased owners
	// Tracker records: {repo: {"<number>": {"status": …, "seen": …}}}
	PRs map[string]map[string]Record `json:"prs"`
}

func New() AppState {
	return AppState{
		Armed:     map[string]map[string]json.RawMessage{},
		Collapsed: []string{},
		PRs:       map[string]map[string]Record{},
	}
}

func DefaultPath() string {
	dir := files.AppDir("XDG_STATE_HOME", filepath.Join(files.Home(), ".local", "state"))
	return filepath.Join(dir, "state.json")
}

// Load returns saved state and a warning if the file was unusable.
func Load(path string) (AppState, string) {
	text, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return New(), ""
	}
	if err != nil {
		return New(), fmt.Sprintf("Ignoring unreadable state %s: %v", path, err)
	}
	// Each part is read on its own, so one bad section doesn't lose the rest.
	var parts struct {
		Armed     json.RawMessage `json:"armed"`
		Collapsed json.RawMessage `json:"collapsed"`
		PRs       json.RawMessage `json:"prs"`
	}
	if err := json.Unmarshal(text, &parts); err != nil {
		return New(), fmt.Sprintf("Ignoring invalid state %s: expected an object", path)
	}
	loaded := New()
	if len(parts.PRs) > 0 {
		var prs map[string]map[string]Record
		if json.Unmarshal(parts.PRs, &prs) == nil {
			loaded.PRs = prs
		}
	}
	if len(parts.Collapsed) > 0 {
		var collapsed []string
		if json.Unmarshal(parts.Collapsed, &collapsed) == nil {
			loaded.Collapsed = collapsed
		}
	}
	if len(parts.Armed) > 0 {
		var armed map[string]map[string]json.RawMessage
		if json.Unmarshal(parts.Armed, &armed) == nil && allObjects(armed) {
			loaded.Armed = armed
		}
	}
	return loaded, ""
}

// allObjects reports whether every armed entry is a JSON object; anything else
// means the file was written by something other than pr-mon.
func allObjects(armed map[string]map[string]json.RawMessage) bool {
	for _, entries := range armed {
		for _, entry := range entries {
			var object map[string]any
			if json.Unmarshal(entry, &object) != nil {
				return false
			}
		}
	}
	return true
}

func Save(path string, state AppState) error {
	if state.Armed == nil {
		state.Armed = map[string]map[string]json.RawMessage{}
	}
	if state.Collapsed == nil {
		state.Collapsed = []string{}
	}
	if state.PRs == nil {
		state.PRs = map[string]map[string]Record{}
	}
	text, err := json.MarshalIndent(state, "", " ")
	if err != nil {
		return err
	}
	return files.WriteAtomic(path, string(text))
}
