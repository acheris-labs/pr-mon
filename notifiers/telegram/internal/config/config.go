// Package config reads the handler's private TOML settings file.
package config

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/BurntSushi/toml"
)

// Config is the settings file.
type Config struct {
	Region         string `toml:"region"`
	QueueURL       string `toml:"queue_url"`
	SenderFunction string `toml:"sender_function"`
	ChatID         string `toml:"chat_id"`
	AWS            AWSKey `toml:"aws"`
}

// AWSKey is the access key the handler signs its AWS calls with.
type AWSKey struct {
	AccessKeyID     string `toml:"access_key_id"`
	SecretAccessKey string `toml:"secret_access_key"`
}

// safeKey matches keys that are plainly setting names; anything else might be
// the secret written where a key goes, so it is never quoted.
var safeKey = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,32}$`)

// Load reads the file, refusing one other users can read.
func Load(path string) (Config, error) {
	info, err := os.Stat(path)
	if err != nil {
		return Config{}, err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return Config{}, fmt.Errorf("%s must be readable only by you (chmod 600)", path)
	}
	text, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var cfg Config
	md, err := toml.Decode(string(text), &cfg)
	if err != nil {
		return Config{}, decodeError(path, err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		return Config{}, unknownError(path, undecoded)
	}
	for _, f := range []struct{ key, value string }{
		{"region", cfg.Region},
		{"queue_url", cfg.QueueURL},
		{"sender_function", cfg.SenderFunction},
		{"aws.access_key_id", cfg.AWS.AccessKeyID},
		{"aws.secret_access_key", cfg.AWS.SecretAccessKey},
	} {
		if f.value == "" {
			return Config{}, fmt.Errorf("%s: %s is required", path, f.key)
		}
	}
	return cfg, nil
}

// decodeError says where the file is wrong but never what's there: TOML's own
// errors quote the text, which may be the secret.
func decodeError(path string, err error) error {
	var parse toml.ParseError
	if errors.As(err, &parse) && parse.Position.Line > 0 {
		return fmt.Errorf("%s isn't valid TOML (line %d)", path, parse.Position.Line)
	}
	return fmt.Errorf("%s isn't valid TOML", path)
}

// unknownError names the unknown settings, but only those that look like
// setting names. TOML's public API gives no line for an undecoded key.
func unknownError(path string, keys []toml.Key) error {
	var names []string
	for _, k := range keys {
		name := k.String()
		if !safeKey.MatchString(name) {
			return fmt.Errorf("%s: an unknown setting", path)
		}
		names = append(names, fmt.Sprintf("%q", name))
	}
	return fmt.Errorf("%s: unknown setting %s", path, strings.Join(names, ", "))
}
