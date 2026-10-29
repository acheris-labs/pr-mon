package config_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/acheris-labs/pr-mon/internal/config"
)

func tempPath(t *testing.T, parts ...string) string {
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

func TestMissingFileIsDefaultWithoutWarning(t *testing.T) {
	loaded, warning := config.Load(tempPath(t, "nested", "config.toml"))
	if warning != "" {
		t.Errorf("warning = %q, want none", warning)
	}
	if !reflect.DeepEqual(loaded, config.New()) {
		t.Errorf("config = %+v", loaded)
	}
}

func TestRoundTrip(t *testing.T) {
	path := tempPath(t, "nested", "config.toml")
	want := config.New()
	want.Repos = []string{"acme/api", `we"ird\name`}
	want.PollInterval = 30
	if err := config.Save(path, want); err != nil {
		t.Fatal(err)
	}
	loaded, warning := config.Load(path)
	if warning != "" {
		t.Errorf("warning = %q", warning)
	}
	if !reflect.DeepEqual(loaded, want) {
		t.Errorf("config = %+v, want %+v", loaded, want)
	}
}

func TestUnusableFilesWarnAndFallBack(t *testing.T) {
	cases := map[string]string{
		"corrupt":      "repos = [",
		"wrong types":  "repos = \"acme/api\"\npoll_interval = \"soon\"\n",
		"bad interval": "poll_interval = 0\n",
	}
	for name, text := range cases {
		t.Run(name, func(t *testing.T) {
			path := tempPath(t, "config.toml")
			write(t, path, text)
			loaded, warning := config.Load(path)
			if warning == "" || !strings.Contains(warning, path) {
				t.Errorf("warning = %q, want one naming the file", warning)
			}
			if loaded.Ignored == "" {
				t.Error("an ignored file should say so, so it isn't saved over")
			}
			loaded.Ignored = ""
			if !reflect.DeepEqual(loaded, config.New()) {
				t.Errorf("config = %+v, want defaults", loaded)
			}
		})
	}
}

func TestSaveRefusesToOverwriteAnIgnoredFile(t *testing.T) {
	path := tempPath(t, "config.toml")
	write(t, path, "repos = [")
	loaded, _ := config.Load(path)
	loaded.Repos = []string{"acme/api"}
	if err := config.Save(path, loaded); err == nil {
		t.Error("saving over a file that couldn't be read should fail")
	}
	if text, _ := os.ReadFile(path); string(text) != "repos = [" {
		t.Errorf("file = %q, want it untouched", text)
	}
}

func TestPartialFileUsesDefaults(t *testing.T) {
	path := tempPath(t, "config.toml")
	write(t, path, "repos = [\"acme/api\"]\n")
	loaded, warning := config.Load(path)
	if warning != "" {
		t.Errorf("warning = %q", warning)
	}
	if loaded.PollInterval != config.DefaultPollInterval {
		t.Errorf("poll interval = %d", loaded.PollInterval)
	}
	if !reflect.DeepEqual(loaded.Repos, []string{"acme/api"}) {
		t.Errorf("repos = %v", loaded.Repos)
	}
}

func TestDefaultPath(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/x/cfg")
	if got := config.DefaultPath(); got != "/x/cfg/pr-mon/config.toml" {
		t.Errorf("path = %q", got)
	}
	t.Setenv("XDG_CONFIG_HOME", "")
	want := filepath.Join(os.Getenv("HOME"), ".config", "pr-mon", "config.toml")
	if got := config.DefaultPath(); got != want {
		t.Errorf("path = %q, want %q", got, want)
	}
}

func TestNotifyDefaults(t *testing.T) {
	settings := config.NewNotifyConfig()
	if settings.Message != "{{PR_REPO}}#{{PR_NUM}} is {{PR_STATE}}: {{PR_TITLE}} {{PR_URL}}" {
		t.Errorf("message = %q", settings.Message)
	}
	want := []string{"READY", "FAILING", "CONFLICT", "MERGED", "MERGE_FAILED"}
	if !reflect.DeepEqual(settings.Events, want) {
		t.Errorf("events = %v, want %v", settings.Events, want)
	}
	if settings.IncludeDrafts || settings.ScriptEnabled || settings.DesktopEnabled ||
		settings.Script != "" {
		t.Errorf("settings = %+v, want channels off", settings)
	}
	if !reflect.DeepEqual(settings.NotifyFor, []string{"mine", "bots", "watched"}) {
		t.Errorf("notify_for = %v, want [mine bots watched]", settings.NotifyFor)
	}
}

func TestNotificationsRoundTrip(t *testing.T) {
	path := tempPath(t, "config.toml")
	api := config.NotifyConfig{
		Message:        "Line \"one\"\n{{PR_URL}} \\ done 🚀 café \t",
		Events:         []string{"BEHIND", "NEW"},
		IncludeDrafts:  true,
		ScriptEnabled:  true,
		Script:         "~/bin/send 'two words'",
		DesktopEnabled: true,
		NotifyFor:      []string{"mine", "bots"},
		SkipBranches:   []string{"release-please--"},
		SkipAuthors:    []string{"octocat"},
	}
	web := config.NotifyConfig{Events: []string{}, DesktopEnabled: true, NotifyFor: []string{"everyone"},
		SkipBranches: []string{}, SkipAuthors: []string{}}
	saved := config.New()
	saved.Repos = []string{"acme/api", "Acme.Org/web-app"}
	saved.Notifications = map[string]config.NotifyConfig{"acme/api": api, "Acme.Org/web-app": web}
	if err := config.Save(path, saved); err != nil {
		t.Fatal(err)
	}
	loaded, warning := config.Load(path)
	if warning != "" {
		t.Errorf("warning = %q", warning)
	}
	if !reflect.DeepEqual(loaded, saved) {
		t.Errorf("config = %+v, want %+v", loaded, saved)
	}
	text, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(text), `[notifications."acme/api"]`) {
		t.Errorf("file = %s", text)
	}
}

func TestSettingsForUnmonitoredReposAreNotSaved(t *testing.T) {
	path := tempPath(t, "config.toml")
	saved := config.New()
	saved.Repos = []string{"acme/api"}
	saved.Notifications = map[string]config.NotifyConfig{"gone/repo": config.NewNotifyConfig()}
	if err := config.Save(path, saved); err != nil {
		t.Fatal(err)
	}
	if loaded, _ := config.Load(path); len(loaded.Notifications) != 0 {
		t.Errorf("notifications = %+v, want none", loaded.Notifications)
	}
}

func TestPartialRepoSectionUsesDefaults(t *testing.T) {
	path := tempPath(t, "config.toml")
	write(t, path, "repos = [\"acme/api\"]\n[notifications.\"acme/api\"]\n"+
		"script_enabled = true\nscript = \"im\"\n")
	loaded, warning := config.Load(path)
	if warning != "" {
		t.Errorf("warning = %q", warning)
	}
	// A section from before notify_for keeps notifying for everyone.
	want := config.LegacyNotifyConfig()
	want.ScriptEnabled = true
	want.Script = "im"
	if got := loaded.Notifications["acme/api"]; !got.Equal(want) {
		t.Errorf("settings = %+v, want %+v", got, want)
	}
}

func TestEmptyNotifyForMeansEveryone(t *testing.T) {
	path := tempPath(t, "config.toml")
	saved := config.New()
	saved.Repos = []string{"acme/api"}
	settings := config.NewNotifyConfig()
	settings.NotifyFor = []string{}
	if !settings.ForEveryone() {
		t.Error("an empty notify_for should mean everyone")
	}
	saved.Notifications = map[string]config.NotifyConfig{"acme/api": settings}
	if err := config.Save(path, saved); err != nil {
		t.Fatal(err)
	}
	loaded, _ := config.Load(path)
	if got := loaded.Notifications["acme/api"].NotifyFor; !reflect.DeepEqual(got, []string{"everyone"}) {
		t.Errorf("notify_for = %v, want [everyone]", got)
	}
}

func TestLoginsRoundTripAndReachClients(t *testing.T) {
	path := tempPath(t, "config.toml")
	saved := config.New()
	saved.Repos = []string{"acme/api"}
	saved.MyLogins = []string{"octocat", "work-cat"}
	saved.BotLogins = []string{"release-robot"}
	saved.WatchedLogins = []string{"teammate"}
	if err := config.Save(path, saved); err != nil {
		t.Fatal(err)
	}
	loaded, warning := config.Load(path)
	if warning != "" {
		t.Errorf("warning = %q", warning)
	}
	if !reflect.DeepEqual(loaded, saved) {
		t.Errorf("config = %+v, want %+v", loaded, saved)
	}
	encoded, err := json.Marshal(loaded)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"my_logins":["octocat","work-cat"]`) ||
		!strings.Contains(string(encoded), `"bot_logins":["release-robot"]`) ||
		!strings.Contains(string(encoded), `"watched_logins":["teammate"]`) {
		t.Errorf("logins missing for clients: %s", encoded)
	}
}

func TestInvalidRepoSectionIsSkipped(t *testing.T) {
	bodies := []string{
		`events = "READY"`,
		`events = ["READY", "EXPLODED"]`,
		`script_enabled = "yes"`,
		`message = 12`,
		`notify_for = ["friends"]`,
		`notify_for = "mine"`,
	}
	for _, body := range bodies {
		t.Run(body, func(t *testing.T) {
			path := tempPath(t, "config.toml")
			write(t, path, "repos = [\"acme/api\"]\n[notifications.\"acme/api\"]\n"+body+"\n")
			loaded, warning := config.Load(path)
			if len(loaded.Notifications) != 0 {
				t.Errorf("notifications = %+v, want none", loaded.Notifications)
			}
			if !strings.Contains(warning, "acme/api") {
				t.Errorf("warning = %q, want it to name the repo", warning)
			}
			if !reflect.DeepEqual(loaded.Repos, []string{"acme/api"}) {
				t.Errorf("repos = %v, want the rest of the file kept", loaded.Repos)
			}
		})
	}
}

const twoWayTOML = `poll_interval = 30
repos = ["acme/api"]

[two_way]
command = "~/bin/pr-mon-telegram --verbose"
`

func TestTwoWayCommandRoundTrip(t *testing.T) {
	path := tempPath(t, "config.toml")
	write(t, path, twoWayTOML)
	loaded, warning := config.Load(path)
	if warning != "" {
		t.Fatalf("warning = %q", warning)
	}
	want := "~/bin/pr-mon-telegram --verbose"
	if loaded.TwoWay == nil || loaded.TwoWay.Command != want {
		t.Fatalf("TwoWay = %+v, want command %q", loaded.TwoWay, want)
	}
	if problem := loaded.TwoWay.Problem(); problem != "" {
		t.Errorf("Problem() = %q", problem)
	}
	loaded.PollInterval = 99
	if err := config.Save(path, loaded); err != nil {
		t.Fatal(err)
	}
	again, _ := config.Load(path)
	if again.PollInterval != 99 || again.TwoWay == nil || again.TwoWay.Command != want {
		t.Errorf("after save: config = %+v, TwoWay = %+v", again, again.TwoWay)
	}
}

func TestNoTwoWayMeansNil(t *testing.T) {
	path := tempPath(t, "config.toml")
	write(t, path, "repos = [\"acme/api\"]\n")
	loaded, _ := config.Load(path)
	if loaded.TwoWay != nil {
		t.Errorf("TwoWay = %+v, want nil", loaded.TwoWay)
	}
	if err := config.Save(path, loaded); err != nil {
		t.Fatal(err)
	}
	text, _ := os.ReadFile(path)
	if strings.Contains(string(text), "two_way") {
		t.Errorf("saved file mentions two_way:\n%s", text)
	}
}

func TestTwoWayProblems(t *testing.T) {
	cases := []struct {
		name, table, want string
	}{
		{"valid", `command = "/bin/handler"`, ""},
		{"legacy kind and region", "kind = \"telegram\"\nregion = \"us-east-1\"",
			"[two_way] now takes a handler command; the AWS settings belong to the handler"},
		{"legacy chat id number", "chat_id = 5",
			"[two_way] now takes a handler command; the AWS settings belong to the handler"},
		{"empty command", `command = ""`, "[two_way] command is missing"},
		{"blank command", `command = "   "`, "[two_way] command is missing"},
		{"empty table", "", "[two_way] command is missing"},
		{"command with legacy keys", "command = \"/bin/handler\"\nkind = \"telegram\"", ""},
		{"command not a string", "command = 5", "[two_way] command must be a string"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := tempPath(t, "config.toml")
			write(t, path, "repos = [\"acme/api\"]\n[two_way]\n"+tc.table+"\n")
			loaded, warning := config.Load(path)
			if warning != "" {
				t.Fatalf("warning = %q", warning)
			}
			if loaded.TwoWay == nil {
				t.Fatal("TwoWay is nil")
			}
			if got := loaded.TwoWay.Problem(); got != tc.want {
				t.Errorf("Problem() = %q, want %q", got, tc.want)
			}
			if !reflect.DeepEqual(loaded.Repos, []string{"acme/api"}) {
				t.Errorf("repos = %v, want the rest of the file kept", loaded.Repos)
			}
		})
	}
}

func TestSaveDropsLegacyKeys(t *testing.T) {
	path := tempPath(t, "config.toml")
	write(t, path, "repos = [\"acme/api\"]\n[two_way]\nkind = \"telegram\"\nregion = \"us-east-1\"\n"+
		"queue_url = \"https://q\"\nsender_function = \"s\"\nroute = \"pr-mon\"\nchat_id = \"1\"\n")
	loaded, _ := config.Load(path)
	if err := config.Save(path, loaded); err != nil {
		t.Fatal(err)
	}
	text, _ := os.ReadFile(path)
	if !strings.Contains(string(text), "[two_way]\ncommand = \"\"\n") {
		t.Errorf("saved file lacks an empty command:\n%s", text)
	}
	for _, key := range []string{"kind", "region", "queue_url", "sender_function", "route", "chat_id"} {
		if strings.Contains(string(text), key+" =") {
			t.Errorf("saved file still has %s:\n%s", key, text)
		}
	}
	again, _ := config.Load(path)
	if again.TwoWay == nil || again.TwoWay.Command != "" {
		t.Errorf("reloaded TwoWay = %+v", again.TwoWay)
	}
}

func TestTwoWayEnabledRoundTrip(t *testing.T) {
	path := tempPath(t, "config.toml")
	write(t, path, "repos = [\"a/b\", \"c/d\"]\n[notifications.\"a/b\"]\ntwo_way_enabled = true\n[notifications.\"c/d\"]\nmessage = \"x\"\n")
	loaded, warning := config.Load(path)
	if warning != "" {
		t.Fatal(warning)
	}
	if !loaded.Notifications["a/b"].TwoWayEnabled || loaded.Notifications["c/d"].TwoWayEnabled {
		t.Fatalf("notifications = %+v", loaded.Notifications)
	}
	if err := config.Save(path, loaded); err != nil {
		t.Fatal(err)
	}
	again, _ := config.Load(path)
	if !again.Notifications["a/b"].TwoWayEnabled || again.Notifications["c/d"].TwoWayEnabled {
		t.Errorf("after save = %+v", again.Notifications)
	}
	a := config.NewNotifyConfig()
	b := a
	b.TwoWayEnabled = true
	if a.Equal(b) {
		t.Error("Equal ignores TwoWayEnabled")
	}
}

func TestTwoWayNotInJSON(t *testing.T) {
	c := config.New()
	c.TwoWay = &config.TwoWayConfig{Command: "/bin/handler"}
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "two_way\"") || strings.Contains(string(data), "handler") {
		t.Errorf("json leaks two_way: %s", data)
	}
}

// A wrongly typed [two_way] value spoils only the table: the rest of the file
// loads, the table says what's wrong, and saving keeps repos and settings.
func TestWrongTypeInTwoWayKeepsTheRest(t *testing.T) {
	path := tempPath(t, "config.toml")
	text := strings.Replace(twoWayTOML, `command = "~/bin/pr-mon-telegram --verbose"`, "command = 5", 1) + `
[notifications."acme/api"]
events = ["READY"]
`
	write(t, path, text)
	loaded, _ := config.Load(path)
	if !reflect.DeepEqual(loaded.Repos, []string{"acme/api"}) || loaded.PollInterval != 30 {
		t.Fatalf("config = %+v, want the rest of the file kept", loaded)
	}
	if got := loaded.Notifications["acme/api"].Events; !reflect.DeepEqual(got, []string{"READY"}) {
		t.Errorf("events = %v, want the notification settings kept", got)
	}
	if loaded.TwoWay == nil {
		t.Fatal("TwoWay = nil, want the table kept")
	}
	if got := loaded.TwoWay.Problem(); !strings.Contains(got, "command must be a string") {
		t.Errorf("Problem() = %q, want it to name command", got)
	}
	if err := config.Save(path, loaded); err != nil {
		t.Fatal(err)
	}
	again, _ := config.Load(path)
	if !reflect.DeepEqual(again.Repos, []string{"acme/api"}) {
		t.Errorf("repos after save = %v", again.Repos)
	}
	if got := again.Notifications["acme/api"].Events; !reflect.DeepEqual(got, []string{"READY"}) {
		t.Errorf("events after save = %v", got)
	}
	if again.TwoWay == nil {
		t.Errorf("TwoWay after save = %+v", again.TwoWay)
	}
}

func TestTwoWayNotATable(t *testing.T) {
	path := tempPath(t, "config.toml")
	write(t, path, "two_way = 5\nrepos = [\"acme/api\"]\n")
	loaded, _ := config.Load(path)
	if !reflect.DeepEqual(loaded.Repos, []string{"acme/api"}) {
		t.Fatalf("repos = %v, want the rest of the file kept", loaded.Repos)
	}
	if loaded.TwoWay == nil || loaded.TwoWay.Problem() == "" {
		t.Errorf("TwoWay = %+v, want a table that says it's unusable", loaded.TwoWay)
	}
}
