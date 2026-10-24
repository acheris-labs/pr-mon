package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"unicode/utf16"

	"github.com/acheris-labs/pr-mon/notifiers/telegram/internal/aws"
)

const queueURL = "https://sqs.us-east-1.amazonaws.com/123456789012/taps.fifo"

// fakeAWS stands in for the telegram-webhook-sender Lambda and the taps queue.
type fakeAWS struct {
	mu            sync.Mutex
	invokes       []map[string]any
	reply         string // what the sender returns
	functionError string // when set, the sender fails with this errorMessage
	messages      []aws.SQSMessage
	receives      []map[string]any
	deleted       []string
}

func (f *fakeAWS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var request map[string]any
	body, _ := io.ReadAll(r.Body)
	if err := json.Unmarshal(body, &request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	switch {
	case r.URL.Path == "/2015-03-31/functions/telegram-webhook-sender/invocations":
		f.invokes = append(f.invokes, request)
		if f.functionError != "" {
			w.Header().Set("X-Amz-Function-Error", "Unhandled")
			json.NewEncoder(w).Encode(map[string]string{"errorMessage": f.functionError})
			return
		}
		w.Write([]byte(f.reply))
	case r.Header.Get("X-Amz-Target") == "AmazonSQS.ReceiveMessage":
		f.receives = append(f.receives, request)
		json.NewEncoder(w).Encode(map[string]any{"Messages": f.messages})
		f.messages = nil
	case r.Header.Get("X-Amz-Target") == "AmazonSQS.DeleteMessage":
		f.deleted = append(f.deleted, request["ReceiptHandle"].(string))
		w.Write([]byte(`{}`))
	default:
		http.Error(w, "unexpected request "+r.URL.Path, http.StatusNotFound)
	}
}

func (f *fakeAWS) lastInvoke(t *testing.T) map[string]any {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.invokes) == 0 {
		t.Fatal("the sender was not invoked")
	}
	return f.invokes[len(f.invokes)-1]
}

func newChannel(t *testing.T, chatID string) (*Client, *fakeAWS) {
	t.Helper()
	fake := &fakeAWS{reply: `{"ok":true,"message_id":1069,"destination":"default"}`}
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	client := &aws.Client{Region: "us-east-1", LambdaEndpoint: server.URL, SQSEndpoint: server.URL}
	channel := New(Settings{QueueURL: queueURL, SenderFunction: "telegram-webhook-sender", ChatID: chatID}, client, slog.New(slog.DiscardHandler))
	return channel, fake
}

func TestPostSendsHTMLWithKeyboard(t *testing.T) {
	message := Message{
		Status: "READY",
		Title:  "acme/api #123 · Fix",
		Lines:  []string{"by alice"},
		Buttons: [][]Button{
			{{Label: "Squash & merge", Key: "k1"}, {Label: "Dismiss", Key: "k2"}},
			{{Label: "Open", URL: "https://github.com/acme/api/pull/123"}},
		},
	}
	wantKeyboard := map[string]any{"inline_keyboard": []any{
		[]any{
			map[string]any{"text": "Squash & merge", "callback_data": "pr-mon:k1"},
			map[string]any{"text": "Dismiss", "callback_data": "pr-mon:k2"},
		},
		[]any{map[string]any{"text": "Open", "url": "https://github.com/acme/api/pull/123"}},
	}}
	for _, test := range []struct {
		name, chatID string
	}{
		{"default destination", ""},
		{"configured chat", "123456789"},
	} {
		t.Run(test.name, func(t *testing.T) {
			channel, fake := newChannel(t, test.chatID)
			id, err := channel.Post(context.Background(), message)
			if err != nil {
				t.Fatal(err)
			}
			if id != "1069" {
				t.Errorf("id = %q, want 1069", id)
			}
			payload := fake.lastInvoke(t)
			if payload["parse_mode"] != "HTML" {
				t.Errorf("parse_mode = %v", payload["parse_mode"])
			}
			if !reflect.DeepEqual(payload["reply_markup"], wantKeyboard) {
				t.Errorf("reply_markup = %v\nwant %v", payload["reply_markup"], wantKeyboard)
			}
			text, _ := payload["message"].(string)
			for _, want := range []string{"<b>", "READY", "<b>acme/api #123 · Fix</b>", "by alice"} {
				if !strings.Contains(text, want) {
					t.Errorf("message %q lacks %q", text, want)
				}
			}
			destination, present := payload["destination"]
			if test.chatID == "" && present {
				t.Errorf("destination = %v, want none", destination)
			}
			if test.chatID != "" && destination != test.chatID {
				t.Errorf("destination = %v, want %s", destination, test.chatID)
			}
			if _, present := payload["message_id"]; present {
				t.Error("a post sent a message_id")
			}
		})
	}
}

func TestRenderEscapesHTML(t *testing.T) {
	channel, fake := newChannel(t, "")
	if _, err := channel.Post(context.Background(), Message{Status: "READY", Title: `a <b> & "c"`, Lines: []string{"x < y"}}); err != nil {
		t.Fatal(err)
	}
	text := fake.lastInvoke(t)["message"].(string)
	for _, want := range []string{`a &lt;b&gt; &amp; &#34;c&#34;`, `x &lt; y`} {
		if !strings.Contains(text, want) {
			t.Errorf("message %q lacks %q", text, want)
		}
	}
}

var (
	tags     = regexp.MustCompile(`</?b>`)
	entities = regexp.MustCompile(`&(amp|lt|gt|#34|#39);`)
)

func TestRenderFitsTelegramLimit(t *testing.T) {
	channel, fake := newChannel(t, "")
	long := strings.Repeat("a&<", 2000)
	if _, err := channel.Post(context.Background(), Message{
		Status: "FAILING", Title: long, Lines: []string{"by alice", long, "dropped"},
	}); err != nil {
		t.Fatal(err)
	}
	text := fake.lastInvoke(t)["message"].(string)
	if leftover := entities.ReplaceAllString(text, ""); strings.Contains(leftover, "&") {
		t.Errorf("message has a cut entity: %q", text)
	}
	plain := html.UnescapeString(tags.ReplaceAllString(text, ""))
	if units := len(utf16.Encode([]rune(plain))); units > maxText {
		t.Errorf("plain text is %d UTF-16 units, over %d", units, maxText)
	}
	if !strings.HasSuffix(plain, "…") || strings.Contains(plain, "dropped") {
		t.Errorf("plain text ends %q, want the title cut with an ellipsis", plain[len(plain)-20:])
	}
}

func TestEditSendsMessageID(t *testing.T) {
	channel, fake := newChannel(t, "123456789")
	if err := channel.Edit(context.Background(), "1069", Message{Status: "MERGED", Title: "acme/api #123 · Fix"}); err != nil {
		t.Fatal(err)
	}
	payload := fake.lastInvoke(t)
	if payload["message_id"] != 1069.0 {
		t.Errorf("message_id = %#v, want 1069", payload["message_id"])
	}
	if _, present := payload["reply_markup"]; present {
		t.Errorf("reply_markup = %v, want none so the buttons go", payload["reply_markup"])
	}
	if payload["destination"] != "123456789" || payload["parse_mode"] != "HTML" {
		t.Errorf("payload = %v", payload)
	}

	if err := channel.Edit(context.Background(), "not a number", Message{}); err == nil {
		t.Error("Edit accepted a non-numeric id")
	}
}

func TestEditNotModifiedIsSuccess(t *testing.T) {
	channel, fake := newChannel(t, "")
	fake.functionError = `telegram returned 400: {"ok":false,"error_code":400,"description":"Bad Request: message is not modified: specified new message content and reply markup are exactly the same"}`
	if err := channel.Edit(context.Background(), "1069", Message{Status: "READY"}); err != nil {
		t.Errorf("Edit = %v, want success", err)
	}
}

func TestDeleteSendsMessageID(t *testing.T) {
	for _, test := range []struct {
		chatID string
		want   map[string]any
	}{
		{"", map[string]any{"delete": true, "message_id": 1069.0}},
		{"42", map[string]any{"delete": true, "message_id": 1069.0, "destination": "42"}},
	} {
		channel, fake := newChannel(t, test.chatID)
		if err := channel.Delete(context.Background(), "1069"); err != nil {
			t.Fatal(err)
		}
		if got := fake.lastInvoke(t); !reflect.DeepEqual(got, test.want) {
			t.Errorf("chat %q: payload = %v, want %v", test.chatID, got, test.want)
		}
	}
}

func TestDeleteMapsTelegramRefusals(t *testing.T) {
	telegramError := func(description string) string {
		return `telegram returned 400: {"ok":false,"error_code":400,"description":"Bad Request: ` + description + `"}`
	}
	for _, test := range []struct {
		name, functionError string
		edit                bool
		want                error
	}{
		{"too old", telegramError("message can't be deleted for everyone"), false, ErrTooOld},
		{"already deleted", telegramError("message to delete not found"), false, ErrGone},
		{"edit of a deleted message", telegramError("message to edit not found"), true, ErrGone},
	} {
		t.Run(test.name, func(t *testing.T) {
			channel, fake := newChannel(t, "")
			fake.functionError = test.functionError
			var err error
			if test.edit {
				err = channel.Edit(context.Background(), "1069", Message{Status: "READY"})
			} else {
				err = channel.Delete(context.Background(), "1069")
			}
			if !errors.Is(err, test.want) {
				t.Errorf("err = %v, want %v", err, test.want)
			}
		})
	}

	channel, fake := newChannel(t, "")
	fake.functionError = telegramError("chat not found")
	err := channel.Delete(context.Background(), "1069")
	if err == nil || errors.Is(err, ErrTooOld) || errors.Is(err, ErrGone) {
		t.Errorf("other failure: err = %v, want a plain error", err)
	}
}

func TestReceiveParsesTaps(t *testing.T) {
	channel, fake := newChannel(t, "")
	fake.messages = []aws.SQSMessage{
		{ReceiptHandle: "r1", Body: `{"update_id":1,"callback_query":{"id":"c","from":{"id":123456789},"data":"pr-mon:k1","message":{"message_id":1069,"chat":{"id":123456789},"text":"..."}}}`},
		{ReceiptHandle: "r2", Body: `{"update_id":2,"callback_query":{"id":"d","data":"other:k1","message":{"message_id":1069}}}`},
		{ReceiptHandle: "r3", Body: `not json`},
		{ReceiptHandle: "r4", Body: `{"update_id":4,"message":{"message_id":5,"text":"hello"}}`},
	}
	taps, err := channel.Receive(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := []Tap{{MessageID: "1069", Key: "k1", Receipt: "r1"}}; !reflect.DeepEqual(taps, want) {
		t.Errorf("taps = %v, want %v", taps, want)
	}
	if want := []string{"r2", "r3", "r4"}; !reflect.DeepEqual(fake.deleted, want) {
		t.Errorf("deleted = %v, want the skipped bodies %v", fake.deleted, want)
	}
	want := map[string]any{"QueueUrl": queueURL, "WaitTimeSeconds": 20.0, "MaxNumberOfMessages": 10.0}
	if !reflect.DeepEqual(fake.receives, []map[string]any{want}) {
		t.Errorf("receive requests = %v, want %v", fake.receives, want)
	}

	if err := channel.Ack(context.Background(), taps[0].Receipt); err != nil {
		t.Fatal(err)
	}
	if fake.deleted[len(fake.deleted)-1] != "r1" {
		t.Errorf("deleted = %v, want r1 acknowledged", fake.deleted)
	}
}

func TestReceiveReportsQueueErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"The specified queue does not exist."}`, http.StatusBadRequest)
	}))
	defer server.Close()
	channel := New(Settings{QueueURL: queueURL}, &aws.Client{Region: "us-east-1", SQSEndpoint: server.URL}, slog.New(slog.DiscardHandler))
	_, err := channel.Receive(context.Background())
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("Receive err = %v, want the queue's", err)
	}
}

func TestCallbackDataFits(t *testing.T) {
	channel, fake := newChannel(t, "")
	if _, err := channel.Post(context.Background(), Message{Status: "READY", Buttons: [][]Button{{{Label: "Merge", Key: "abcdefgh"}}}}); err != nil {
		t.Fatal(err)
	}
	keyboard := fake.lastInvoke(t)["reply_markup"].(map[string]any)["inline_keyboard"].([]any)
	data := keyboard[0].([]any)[0].(map[string]any)["callback_data"].(string)
	if data != "pr-mon:abcdefgh" || len(data) != 15 {
		t.Errorf("callback_data = %q (%d bytes), want pr-mon:abcdefgh (15)", data, len(data))
	}

	fake.invokes = nil
	_, err := channel.Post(context.Background(), Message{Status: "READY", Buttons: [][]Button{{{Label: "Merge", Key: strings.Repeat("k", 60)}}}})
	if err == nil {
		t.Error("Post accepted callback_data over 64 bytes")
	}
	if len(fake.invokes) != 0 {
		t.Error("Post invoked the sender with oversized callback_data")
	}
}

func TestNonNumericIDIsGone(t *testing.T) {
	for _, id := range []string{"abc", "0", "-5"} {
		t.Run(id, func(t *testing.T) {
			channel, fake := newChannel(t, "")
			if err := channel.Edit(context.Background(), id, Message{Status: "READY"}); !errors.Is(err, ErrGone) {
				t.Errorf("Edit err = %v, want ErrGone", err)
			}
			if err := channel.Delete(context.Background(), id); !errors.Is(err, ErrGone) {
				t.Errorf("Delete err = %v, want ErrGone", err)
			}
			if len(fake.invokes) != 0 {
				t.Errorf("the sender was invoked %d times", len(fake.invokes))
			}
		})
	}
}

func TestAckUnknownReceiptIsOK(t *testing.T) {
	for _, test := range []struct {
		name, errorType string
		wantErr         bool
	}{
		{"unknown receipt", "com.amazonaws.sqs#ReceiptHandleIsInvalid", false},
		{"missing queue", "com.amazonaws.sqs#QueueDoesNotExist", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				json.NewEncoder(w).Encode(map[string]string{"__type": test.errorType, "message": "nope"})
			}))
			defer server.Close()
			channel := New(Settings{QueueURL: queueURL}, &aws.Client{Region: "us-east-1", SQSEndpoint: server.URL}, slog.New(slog.DiscardHandler))
			err := channel.Ack(context.Background(), "r1")
			if (err != nil) != test.wantErr {
				t.Errorf("Ack err = %v, want error: %v", err, test.wantErr)
			}
		})
	}
}

func TestSkippedUpdateLogsOnlyUpdateID(t *testing.T) {
	channel, fake := newChannel(t, "")
	var logs bytes.Buffer
	channel.log = slog.New(slog.NewTextHandler(&logs, nil))
	fake.messages = []aws.SQSMessage{
		{ReceiptHandle: "r1", Body: `{"update_id":99,"message":{"chat":{"id":5},"from":{"first_name":"Mallory"},"text":"hi"}}`},
		{ReceiptHandle: "r2", Body: `{"update_id":100,"callback_query":{"from":{"first_name":"Mallory"},"data":"backup:k1","message":{"message_id":7}}}`},
	}
	taps, err := channel.Receive(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(taps) != 0 {
		t.Errorf("taps = %v, want none", taps)
	}
	out := logs.String()
	for _, want := range []string{"99", "100"} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks update_id %s: %s", want, out)
		}
	}
	if strings.Contains(out, "Mallory") {
		t.Errorf("log has update content: %s", out)
	}
	if want := []string{"r1", "r2"}; !reflect.DeepEqual(fake.deleted, want) {
		t.Errorf("deleted = %v, want %v", fake.deleted, want)
	}
}
