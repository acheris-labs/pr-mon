package protocol_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/acheris-labs/pr-mon/internal/models"
	"github.com/acheris-labs/pr-mon/internal/notify"
	"github.com/acheris-labs/pr-mon/internal/protocol"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "protocol-fixtures", name))
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	return data
}

// sameJSON compares two encodings by value, ignoring key order.
func sameJSON(t *testing.T, got, want []byte) bool {
	t.Helper()
	var left, right any
	if err := json.Unmarshal(got, &left); err != nil {
		t.Fatalf("decoding what we produced: %v", err)
	}
	if err := json.Unmarshal(want, &right); err != nil {
		t.Fatalf("decoding the fixture: %v", err)
	}
	return reflect.DeepEqual(left, right)
}

// roundTrip decodes a fixture into a Go type and re-encodes it, so any field the
// Go structs get wrong shows up as a difference.
func roundTrip[T any](t *testing.T, name, field string) {
	t.Helper()
	raw := fixture(t, name)
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	part, found := envelope[field]
	if !found {
		t.Fatalf("%s has no %q", name, field)
	}
	var value T
	if err := json.Unmarshal(part, &value); err != nil {
		t.Fatalf("%s: decoding: %v", name, err)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("%s: encoding: %v", name, err)
	}
	if !sameJSON(t, encoded, part) {
		t.Errorf("%s does not round-trip.\n got: %s\nwant: %s", name, encoded, part)
	}
}

func TestFixturesRoundTrip(t *testing.T) {
	roundTrip[protocol.Hello](t, "hello-response.json", "result")
	roundTrip[protocol.Snapshot](t, "snapshot-response.json", "result")
	roundTrip[notify.Form](t, "notification-form-response.json", "result")
	roundTrip[notify.Preview](t, "preview-notification-response.json", "result")
	roundTrip[protocol.Snapshot](t, "event-repos.json", "data")
	roundTrip[protocol.RepoUpdate](t, "event-repo.json", "data")
	roundTrip[protocol.SeenUpdate](t, "event-seen.json", "data")
	roundTrip[protocol.CollapsedUpdate](t, "event-collapsed.json", "data")
	roundTrip[protocol.ConfigUpdate](t, "event-config.json", "data")
	roundTrip[protocol.StatusUpdate](t, "event-status.json", "data")
	roundTrip[protocol.Toast](t, "event-toast.json", "data")
}

func TestSnapshotFixtureIsUnderstood(t *testing.T) {
	var response struct {
		Result protocol.Snapshot `json:"result"`
	}
	if err := json.Unmarshal(fixture(t, "snapshot-response.json"), &response); err != nil {
		t.Fatal(err)
	}
	snapshot := response.Result
	if len(snapshot.Config.Repos) != 3 || snapshot.Config.PollInterval != 120 ||
		snapshot.Config.FocusInterval != 60 || snapshot.Config.ActiveInterval != 10 {
		t.Errorf("config = %+v", snapshot.Config)
	}
	if snapshot.Errors["acme/web"] == "" {
		t.Error("the fixture's repo error should survive")
	}
	if got := snapshot.Armed["Textualize/rich"]["42"]; got.Method != models.MergeSquash {
		t.Errorf("armed = %+v", snapshot.Armed)
	}
	api := snapshot.Repos["acme/api"]
	if len(api.PRs) != 9 || api.PRTotal != 60 {
		t.Errorf("repo = %+v", api)
	}
	if api.PRs[0].Status != models.StatusReady || len(api.PRs[0].Actions) == 0 {
		t.Errorf("pr = %+v", api.PRs[0])
	}
	if waiting := api.PRs[8]; waiting.Status != models.StatusWaiting || len(waiting.WaitsOn) != 2 ||
		waiting.WaitsOn[1].State != models.PRMerged || waiting.WaitsOn[0].Status == nil {
		t.Errorf("waiting pr = %+v", waiting)
	}
}

func TestRequestsFixtureMatchesWhatWeSend(t *testing.T) {
	var requests []struct {
		ID   int             `json:"id"`
		Op   string          `json:"op"`
		Args json.RawMessage `json:"args"`
	}
	if err := json.Unmarshal(fixture(t, "requests.json"), &requests); err != nil {
		t.Fatal(err)
	}
	ops := map[string]bool{}
	for _, request := range requests {
		ops[request.Op] = true
		if request.Op == "perform" {
			var args struct {
				Action json.RawMessage `json:"action"`
			}
			if err := json.Unmarshal(request.Args, &args); err != nil {
				t.Fatal(err)
			}
			action, err := protocol.ParseAction(args.Action)
			if err != nil {
				t.Errorf("the fixture's action was refused: %v", err)
				continue
			}
			encoded, err := json.Marshal(action)
			if err != nil {
				t.Fatal(err)
			}
			if !sameJSON(t, encoded, args.Action) {
				t.Errorf("action does not round-trip.\n got: %s\nwant: %s", encoded, args.Action)
			}
		}
		if request.Op == "save_notifications" || request.Op == "send_test" {
			var args struct {
				Settings json.RawMessage `json:"settings"`
			}
			if err := json.Unmarshal(request.Args, &args); err != nil {
				t.Fatal(err)
			}
			settings, err := protocol.ParseSettings(args.Settings)
			if err != nil {
				t.Errorf("the fixture's settings were refused: %v", err)
				continue
			}
			encoded, _ := json.Marshal(settings)
			if !sameJSON(t, encoded, args.Settings) {
				t.Errorf("settings do not round-trip.\n got: %s\nwant: %s", encoded, args.Settings)
			}
		}
	}
	// Every op in the fixtures is one we answer (shutdown isn't exercised there).
	for op := range ops {
		if op == "shutdown" {
			continue
		}
		if !known(op) {
			t.Errorf("the fixtures use %q, which the server doesn't answer", op)
		}
	}
}

func known(op string) bool {
	for _, name := range serverOps {
		if name == op {
			return true
		}
	}
	return false
}

// serverOps mirrors server.Ops; the server's own test checks they agree.
var serverOps = []string{
	"hello", "snapshot", "refresh_all", "add_repo", "remove_repo", "mark_seen",
	"set_collapsed", "set_focus", "perform", "add_dependency", "remove_dependency",
	"dependency_graph", "save_notifications", "send_test", "set_poll_interval",
	"notification_form", "preview_notification", "shutdown",
}

func TestParseActionRejectsUnknownKinds(t *testing.T) {
	if _, err := protocol.ParseAction([]byte(`{"kind": "explode"}`)); err == nil {
		t.Error("an unknown action kind should be refused")
	}
	if _, err := protocol.ParseAction([]byte(`{"kind": "merge", "method": "FAST_FORWARD"}`)); err == nil {
		t.Error("an unknown merge method should be refused")
	}
	action, err := protocol.ParseAction([]byte(`{"kind": "merge", "method": "SQUASH", "delete_branch": true}`))
	if err != nil {
		t.Fatal(err)
	}
	if action.Method == nil || *action.Method != models.MergeSquash || !action.DeleteBranch {
		t.Errorf("action = %+v", action)
	}
}

func TestParseSettingsRejectsUnknownEvents(t *testing.T) {
	if _, err := protocol.ParseSettings([]byte(`{"events": ["BOGUS"]}`)); err == nil {
		t.Error("an unknown event should be refused")
	}
	settings, err := protocol.ParseSettings([]byte(`{"script_enabled": true, "script": "im"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !settings.ScriptEnabled || settings.Script != "im" || settings.Message == "" {
		t.Errorf("settings = %+v, want the defaults filled in", settings)
	}
}

func TestEncodeKeepsOneLine(t *testing.T) {
	line, err := protocol.Encode(protocol.Toast{Message: "one\ntwo", Severity: "information"})
	if err != nil {
		t.Fatal(err)
	}
	if line[len(line)-1] != '\n' {
		t.Error("a message should end with a newline")
	}
	for _, char := range line[:len(line)-1] {
		if char == '\n' {
			t.Fatalf("the message spans lines: %s", line)
		}
	}
}

// A command that returns nothing still answers with a result key: a client
// that decodes result as a required field must not break on an empty reply.
func TestSuccessfulResponseAlwaysCarriesResult(t *testing.T) {
	line, err := protocol.Encode(protocol.Response{ID: 7, OK: true})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(line, &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["result"]; !ok {
		t.Errorf("no result key in %s", line)
	}
}
