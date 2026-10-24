package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acheris-labs/pr-mon/notifiers/telegram/internal/config"
)

const secret = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"

const full = `region = "us-east-1"
queue_url = "https://sqs.us-east-1.amazonaws.com/123/q"
sender_function = "sender"
chat_id = "42"

[aws]
access_key_id = "AKIAEXAMPLE"
secret_access_key = "` + secret + `"
`

func write(t *testing.T, text string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "handler.toml")
	if err := os.WriteFile(path, []byte(text), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoad(t *testing.T) {
	got, err := config.Load(write(t, full, 0o600))
	if err != nil {
		t.Fatal(err)
	}
	want := config.Config{
		Region:         "us-east-1",
		QueueURL:       "https://sqs.us-east-1.amazonaws.com/123/q",
		SenderFunction: "sender",
		ChatID:         "42",
		AWS:            config.AWSKey{AccessKeyID: "AKIAEXAMPLE", SecretAccessKey: secret},
	}
	if got != want {
		t.Errorf("config = %+v, want %+v", got, want)
	}

	noChat := strings.Replace(full, "chat_id = \"42\"\n", "", 1)
	got, err = config.Load(write(t, noChat, 0o600))
	if err != nil {
		t.Fatal(err)
	}
	if got.ChatID != "" {
		t.Errorf("ChatID = %q, want empty", got.ChatID)
	}
}

func TestTooOpen(t *testing.T) {
	_, err := config.Load(write(t, full, 0o644))
	if err == nil || !strings.HasSuffix(err.Error(), "must be readable only by you (chmod 600)") {
		t.Errorf("err = %v", err)
	}
}

func TestMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nope.toml")
	_, err := config.Load(path)
	if err == nil || !strings.Contains(err.Error(), path) {
		t.Errorf("err = %v, want it to name %s", err, path)
	}
}

func TestRequired(t *testing.T) {
	for _, tc := range []struct{ key, line string }{
		{"region", "region = \"us-east-1\"\n"},
		{"queue_url", "queue_url = \"https://sqs.us-east-1.amazonaws.com/123/q\"\n"},
		{"sender_function", "sender_function = \"sender\"\n"},
		{"aws.access_key_id", "access_key_id = \"AKIAEXAMPLE\"\n"},
		{"aws.secret_access_key", "secret_access_key = \"" + secret + "\"\n"},
	} {
		t.Run(tc.key, func(t *testing.T) {
			if !strings.Contains(full, tc.line) {
				t.Fatalf("fixture lacks %q", tc.line)
			}
			path := write(t, strings.Replace(full, tc.line, "", 1), 0o600)
			_, err := config.Load(path)
			want := path + ": " + tc.key + " is required"
			if err == nil || err.Error() != want {
				t.Errorf("err = %v, want %q", err, want)
			}
		})
	}
}

func TestUnknownKey(t *testing.T) {
	_, err := config.Load(write(t, "chat = \"1\"\n"+full, 0o600))
	if err == nil || !strings.Contains(err.Error(), `unknown setting "chat"`) {
		t.Errorf("err = %v", err)
	}
}

// alnumSecret is 40 characters of [A-Za-z0-9], like an AWS secret with no / or +.
const alnumSecret = "wJalrXUtnFEMIK7MDENGbPxRfiCYEXAMPLEKEY12"

func TestSecretStaysOutOfErrors(t *testing.T) {
	head := strings.Split(full, "secret_access_key")[0]
	for name, text := range map[string]string{
		"unquoted":         head + "secret_access_key = " + secret + "\n",
		"wrong type":       head + "secret_access_key = [\"" + secret + "\"]\n",
		"unterminated":     head + "secret_access_key = \"" + secret + "\n",
		"as key name":      full + "\"" + secret + "\" = 1\n",
		"alphanumeric key": alnumSecret + " = 1\n" + full,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := config.Load(write(t, text, 0o600))
			if err == nil {
				t.Fatal("err = nil, want the file refused")
			}
			if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "wJalr") || strings.Contains(err.Error(), alnumSecret) {
				t.Errorf("err = %q quotes the secret", err)
			}
		})
	}
}
