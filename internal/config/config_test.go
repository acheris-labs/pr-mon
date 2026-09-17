package config_test

import (
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
			if !reflect.DeepEqual(loaded, config.New()) {
				t.Errorf("config = %+v, want defaults", loaded)
			}
		})
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
	}
	web := config.NotifyConfig{Events: []string{}, DesktopEnabled: true}
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
	want := config.NewNotifyConfig()
	want.ScriptEnabled = true
	want.Script = "im"
	if got := loaded.Notifications["acme/api"]; !got.Equal(want) {
		t.Errorf("settings = %+v, want %+v", got, want)
	}
}

func TestInvalidRepoSectionIsSkipped(t *testing.T) {
	bodies := []string{
		`events = "READY"`,
		`events = ["READY", "EXPLODED"]`,
		`script_enabled = "yes"`,
		`message = 12`,
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
