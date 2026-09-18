package daemon_test

import (
	"testing"

	"github.com/acheris-labs/pr-mon/internal/daemon"
)

func TestToolPathAddsHomebrew(t *testing.T) {
	cases := map[string]string{
		// launchd's PATH, which an app opened from the Dock passes on.
		"/usr/bin:/bin:/usr/sbin:/sbin": "/usr/bin:/bin:/usr/sbin:/sbin:/opt/homebrew/bin:/usr/local/bin",
		// Already there: left where the user put it, not duplicated.
		"/opt/homebrew/bin:/usr/bin": "/opt/homebrew/bin:/usr/bin:/usr/local/bin",
		"":                          "/opt/homebrew/bin:/usr/local/bin",
	}
	for path, want := range cases {
		if got := daemon.ToolPath(path); got != want {
			t.Errorf("ToolPath(%q) = %q, want %q", path, got, want)
		}
	}
}
