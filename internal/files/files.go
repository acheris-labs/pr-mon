// Package files holds XDG locations and safe file writes.
package files

import (
	"os"
	"path/filepath"
)

const AppName = "pr-mon"

// AppDir is $envVar/pr-mon, or fallback/pr-mon when the variable is unset.
func AppDir(envVar, fallback string) string {
	if base := os.Getenv(envVar); base != "" {
		return filepath.Join(base, AppName)
	}
	return filepath.Join(fallback, AppName)
}

func Home() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return os.Getenv("HOME")
	}
	return home
}

// WriteAtomic replaces path's contents, leaving the old file untouched on failure.
func WriteAtomic(path, text string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	temp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".")
	if err != nil {
		return err
	}
	name := temp.Name()
	defer os.Remove(name) // a no-op once the rename succeeded
	if _, err := temp.WriteString(text); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
