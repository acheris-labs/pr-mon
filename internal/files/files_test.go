package files

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOpenLogIsPrivate(t *testing.T) {
	dir := t.TempDir()

	fresh := filepath.Join(dir, "new.log")
	file, err := OpenLog(fresh)
	if err != nil {
		t.Fatal(err)
	}
	file.Close()
	if info, _ := os.Stat(fresh); info.Mode().Perm() != 0o600 {
		t.Errorf("new log mode = %o, want 600", info.Mode().Perm())
	}

	old := filepath.Join(dir, "old.log")
	if err := os.WriteFile(old, []byte("kept\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(old, 0o644); err != nil { // umask may have trimmed it
		t.Fatal(err)
	}
	file, err = OpenLog(old)
	if err != nil {
		t.Fatal(err)
	}
	file.WriteString("more\n")
	file.Close()
	if info, _ := os.Stat(old); info.Mode().Perm() != 0o600 {
		t.Errorf("existing log mode = %o, want 600", info.Mode().Perm())
	}
	if got, _ := os.ReadFile(old); string(got) != "kept\nmore\n" {
		t.Errorf("content = %q", got)
	}
}
