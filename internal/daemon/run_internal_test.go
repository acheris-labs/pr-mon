package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRotatedLogIsPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	log, err := openLog(path)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	// Big enough to force a rotation.
	if _, err := log.Write([]byte(strings.Repeat("x", logBytes))); err != nil {
		t.Fatal(err)
	}
	if _, err := log.Write([]byte("after\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("no rotation happened: %v", err)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("rotated log mode = %o, want 600", info.Mode().Perm())
	}
}
