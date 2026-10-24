// Package handler talks to a two-way handler program: a separate process that
// carries messages to a chat service and reports button taps. This file is the
// wire format, documented in docs/two-way-handler.md.
package handler

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/acheris-labs/pr-mon/internal/twoway"
)

// Protocol is the version of the handler protocol pr-mon speaks.
const Protocol = 1

// request is a line sent to the handler.
type request struct {
	ID   int64  `json:"id"`
	Op   string `json:"op"`
	Args any    `json:"args"`
}

// incoming is a line from the handler: a reply (ID set) or an event.
type incoming struct {
	ID     *int64          `json:"id"`
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result"`
	Error  *wireError      `json:"error"`
	Event  string          `json:"event"`
	Args   json.RawMessage `json:"args"`
}

type wireError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// err maps the wire error to the twoway errors; unknown codes are plain errors.
func (e wireError) err() error {
	switch e.Code {
	case "gone":
		return fmt.Errorf("%w: %s", twoway.ErrGone, e.Message)
	case "too_old":
		return fmt.Errorf("%w: %s", twoway.ErrTooOld, e.Message)
	}
	return fmt.Errorf("handler: %s", e.Message)
}

type wireButton struct {
	Label string `json:"label"`
	Key   string `json:"key,omitempty"`
	URL   string `json:"url,omitempty"`
}

type wireMessage struct {
	Status  string         `json:"status"`
	Title   string         `json:"title"`
	Lines   []string       `json:"lines"`
	Buttons [][]wireButton `json:"buttons"`
}

// encodeMessage converts a message, making nil lists empty so they encode as
// [] and never null.
func encodeMessage(m twoway.Message) wireMessage {
	out := wireMessage{
		Status: m.Status, Title: m.Title,
		Lines:   append([]string{}, m.Lines...),
		Buttons: make([][]wireButton, 0, len(m.Buttons)),
	}
	for _, row := range m.Buttons {
		buttons := make([]wireButton, 0, len(row))
		for _, b := range row {
			buttons = append(buttons, wireButton{Label: b.Label, Key: b.Key, URL: b.URL})
		}
		out.Buttons = append(out.Buttons, buttons)
	}
	return out
}

// marshal encodes one line without HTML escaping, so "&" stays "&" and a
// reader of the wire sees the text as written. There is no trailing newline.
func marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// Arguments of each op.
type (
	helloArgs struct {
		Protocol int    `json:"protocol"`
		App      string `json:"app"`
		Version  string `json:"version"`
	}
	postArgs struct {
		Message wireMessage `json:"message"`
	}
	editArgs struct {
		MessageID string      `json:"message_id"`
		Message   wireMessage `json:"message"`
	}
	deleteArgs struct {
		MessageID string `json:"message_id"`
	}
	ackArgs struct {
		Receipt string `json:"receipt"`
	}
)

// tapArgs is the args of a tap event.
type tapArgs struct {
	MessageID string `json:"message_id"`
	Key       string `json:"key"`
	Receipt   string `json:"receipt"`
}

// Fixtures returns sample protocol lines by file name, for gen-fixtures. Each
// is one JSON line.
func Fixtures() map[string][]byte {
	message := encodeMessage(twoway.Message{
		Status: "READY",
		Title:  "acme/api #12 · Fix login redirect",
		Lines:  []string{"octocat · fix-login → main"},
		Buttons: [][]twoway.Button{
			{{Label: "Squash & merge", Key: "xx2ev6d7"}},
			{{Label: "Open", URL: "https://github.com/acme/api/pull/12"}, {Label: "Dismiss", Key: "kofmkufw"}},
		},
	})
	reply := func(id int64, result any) any {
		return struct {
			ID     int64 `json:"id"`
			OK     bool  `json:"ok"`
			Result any   `json:"result"`
		}{id, true, result}
	}
	failure := func(id int64, code, text string) any {
		return struct {
			ID    int64     `json:"id"`
			OK    bool      `json:"ok"`
			Error wireError `json:"error"`
		}{id, false, wireError{code, text}}
	}
	lines := map[string]any{
		"hello-request.json": request{1, "hello", helloArgs{Protocol, "pr-mon", "1.2.3"}},
		"hello-reply.json": reply(1, struct {
			Protocol int    `json:"protocol"`
			Name     string `json:"name"`
		}{Protocol, "Telegram"}),
		"post-request.json": request{2, "post", postArgs{message}},
		"post-reply.json": reply(2, struct {
			MessageID string `json:"message_id"`
		}{"1082"}),
		"edit-request.json":   request{3, "edit", editArgs{"1082", message}},
		"delete-request.json": request{4, "delete", deleteArgs{"1082"}},
		"ack-request.json":    request{5, "ack", ackArgs{"r-7731"}},
		"ok-reply.json":       reply(3, struct{}{}),
		"error-reply-gone.json": failure(4, "gone",
			"message 1082 no longer exists"),
		"error-reply-too-old.json": failure(4, "too_old",
			"messages older than 48 hours can't be deleted"),
		"tap-event.json": struct {
			Event string  `json:"event"`
			Args  tapArgs `json:"args"`
		}{"tap", tapArgs{"1082", "xx2ev6d7", "r-7731"}},
	}
	out := make(map[string][]byte, len(lines))
	for name, line := range lines {
		data, err := marshal(line)
		if err != nil {
			panic(err) // fixed inputs; can't fail
		}
		out[name] = data
	}
	return out
}
