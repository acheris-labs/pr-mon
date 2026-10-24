package handler

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/acheris-labs/pr-mon/internal/twoway"
)

func TestEncodeMessage(t *testing.T) {
	tests := []struct {
		name    string
		message twoway.Message
		want    string
	}{
		{
			name: "full",
			message: twoway.Message{
				Status: "READY", Title: "acme/api #12 · Fix login",
				Lines: []string{"octocat · fix → main", "2 approvals"},
				Buttons: [][]twoway.Button{
					{{Label: "Squash & merge", Key: "xx2ev6d7"}},
					{{Label: "Open", URL: "https://x"}, {Label: "Dismiss", Key: "kofmkufw"}},
				},
			},
			want: `{"status":"READY","title":"acme/api #12 · Fix login","lines":["octocat · fix → main","2 approvals"],` +
				`"buttons":[[{"label":"Squash & merge","key":"xx2ev6d7"}],` +
				`[{"label":"Open","url":"https://x"},{"label":"Dismiss","key":"kofmkufw"}]]}`,
		},
		{
			name:    "empty",
			message: twoway.Message{},
			want:    `{"status":"","title":"","lines":[],"buttons":[]}`,
		},
		{
			name:    "empty row",
			message: twoway.Message{Buttons: [][]twoway.Button{nil}},
			want:    `{"status":"","title":"","lines":[],"buttons":[[]]}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := marshal(encodeMessage(test.message))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != test.want {
				t.Errorf("got  %s\nwant %s", got, test.want)
			}
		})
	}
}

func TestErrorCodes(t *testing.T) {
	tests := []struct {
		code    string
		isGone  bool
		isOld   bool
		message string
	}{
		{"gone", true, false, "deleted by hand"},
		{"too_old", false, true, "48 hours"},
		{"failed", false, false, "chat is down"},
		{"weird", false, false, "something odd"},
	}
	for _, test := range tests {
		t.Run(test.code, func(t *testing.T) {
			err := wireError{Code: test.code, Message: test.message}.err()
			if errors.Is(err, twoway.ErrGone) != test.isGone {
				t.Errorf("Is ErrGone = %v, want %v", !test.isGone, test.isGone)
			}
			if errors.Is(err, twoway.ErrTooOld) != test.isOld {
				t.Errorf("Is ErrTooOld = %v, want %v", !test.isOld, test.isOld)
			}
			if !bytes.Contains([]byte(err.Error()), []byte(test.message)) {
				t.Errorf("error %q lacks message %q", err, test.message)
			}
		})
	}
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "protocol-fixtures", "two-way-handler", name))
	if err != nil {
		t.Fatalf("%v (run make fixtures)", err)
	}
	return data
}

func TestFixturesMatchDisk(t *testing.T) {
	for name, want := range Fixtures() {
		got := bytes.TrimSuffix(readFixture(t, name), []byte("\n"))
		if !bytes.Equal(got, want) {
			t.Errorf("%s differs from the encoder; run make fixtures\n got  %s\n want %s", name, got, want)
		}
	}
}

func decode(t *testing.T, name string) incoming {
	t.Helper()
	var in incoming
	if err := json.Unmarshal(readFixture(t, name), &in); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return in
}

func TestFixturesDecode(t *testing.T) {
	t.Run("hello reply", func(t *testing.T) {
		in := decode(t, "hello-reply.json")
		var result struct {
			Protocol int
			Name     string
		}
		if err := json.Unmarshal(in.Result, &result); err != nil {
			t.Fatal(err)
		}
		if in.ID == nil || !in.OK || result.Protocol != Protocol || result.Name != "Telegram" {
			t.Errorf("got %+v %+v", in, result)
		}
	})
	t.Run("post reply", func(t *testing.T) {
		in := decode(t, "post-reply.json")
		var result struct {
			MessageID string `json:"message_id"`
		}
		if err := json.Unmarshal(in.Result, &result); err != nil {
			t.Fatal(err)
		}
		if in.ID == nil || !in.OK || result.MessageID != "1082" {
			t.Errorf("got %+v %+v", in, result)
		}
	})
	t.Run("ok reply", func(t *testing.T) {
		if in := decode(t, "ok-reply.json"); in.ID == nil || !in.OK {
			t.Errorf("got %+v", in)
		}
	})
	for name, code := range map[string]string{
		"error-reply-gone.json":    "gone",
		"error-reply-too-old.json": "too_old",
	} {
		t.Run(name, func(t *testing.T) {
			in := decode(t, name)
			if in.ID == nil || in.OK || in.Error == nil || in.Error.Code != code || in.Error.Message == "" {
				t.Errorf("got %+v", in)
			}
		})
	}
	t.Run("tap event", func(t *testing.T) {
		in := decode(t, "tap-event.json")
		var tap tapArgs
		if err := json.Unmarshal(in.Args, &tap); err != nil {
			t.Fatal(err)
		}
		if in.ID != nil || in.Event != "tap" || tap.MessageID != "1082" || tap.Key != "xx2ev6d7" || tap.Receipt == "" {
			t.Errorf("got %+v %+v", in, tap)
		}
	})
}
