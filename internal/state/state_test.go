package state_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/acheris-labs/pr-mon/internal/state"
)

func path(t *testing.T, parts ...string) string {
	t.Helper()
	return filepath.Join(append([]string{t.TempDir()}, parts...)...)
}

func write(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestMissingFile(t *testing.T) {
	loaded, warning := state.Load(path(t, "nested", "state.json"))
	if warning != "" || !reflect.DeepEqual(loaded, state.New()) {
		t.Errorf("state = %+v, warning = %q", loaded, warning)
	}
}

func TestRoundTrip(t *testing.T) {
	file := path(t, "nested", "state.json")
	saved := state.New()
	saved.PRs = map[string]map[string]state.Record{
		"acme/api": {"12": {Status: "READY", Seen: false}},
	}
	saved.Collapsed = []string{"acme", "cli"}
	saved.Armed = map[string]map[string]json.RawMessage{
		"acme/api": {"12": json.RawMessage(
			`{"method":"SQUASH","delete_branch":true,"armed_at":"2026-09-17"}`)},
	}
	if err := state.Save(file, saved); err != nil {
		t.Fatal(err)
	}
	loaded, warning := state.Load(file)
	if warning != "" {
		t.Errorf("warning = %q", warning)
	}
	if !reflect.DeepEqual(loaded.PRs, saved.PRs) || !reflect.DeepEqual(loaded.Collapsed, saved.Collapsed) {
		t.Errorf("state = %+v, want %+v", loaded, saved)
	}
	if len(loaded.Armed["acme/api"]) != 1 {
		t.Errorf("armed = %v", loaded.Armed)
	}
	// The atomic write leaves no temporary files behind.
	entries, err := os.ReadDir(filepath.Dir(file))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(file) {
		t.Errorf("directory holds %d entries", len(entries))
	}
}

func TestBadSectionsAreIgnored(t *testing.T) {
	cases := map[string]string{
		"armed not an object":   `{"prs": {}, "armed": "x"}`,
		"armed repo not a map":  `{"prs": {}, "armed": {"a/b": "x"}}`,
		"armed entry not a map": `{"prs": {}, "armed": {"a/b": {"1": "x"}}}`,
		"collapsed not a list":  `{"prs": {}, "collapsed": "acme"}`,
	}
	for name, text := range cases {
		t.Run(name, func(t *testing.T) {
			file := path(t, "state.json")
			write(t, file, text)
			loaded, warning := state.Load(file)
			if warning != "" {
				t.Errorf("warning = %q, want none", warning)
			}
			if len(loaded.Armed) != 0 || len(loaded.Collapsed) != 0 {
				t.Errorf("state = %+v, want the bad section dropped", loaded)
			}
		})
	}
}

func TestUnreadableFileWarns(t *testing.T) {
	for _, text := range []string{"{nope", "[1, 2]"} {
		t.Run(text, func(t *testing.T) {
			file := path(t, "state.json")
			write(t, file, text)
			loaded, warning := state.Load(file)
			if !strings.Contains(warning, file) {
				t.Errorf("warning = %q, want one naming the file", warning)
			}
			if !reflect.DeepEqual(loaded, state.New()) {
				t.Errorf("state = %+v, want defaults", loaded)
			}
		})
	}
}

func TestDefaultPath(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/x/state")
	if got := state.DefaultPath(); got != "/x/state/pr-mon/state.json" {
		t.Errorf("path = %q", got)
	}
}
