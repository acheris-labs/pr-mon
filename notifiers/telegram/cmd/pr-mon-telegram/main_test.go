package main

import (
	"bufio"
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const secret = "s3cr3t-value-ZZZ"

func writeConfig(t *testing.T, mode os.FileMode, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func configBody() string {
	return `region = "us-east-1"
queue_url = "https://sqs.us-east-1.amazonaws.com/123/q.fifo"
sender_function = "telegram-webhook-sender"
chat_id = ""

[aws]
access_key_id = "AKIATEST"
secret_access_key = "` + secret + `"
`
}

func TestNoConfigFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(nil, strings.NewReader(""), &stdout, &stderr, endpoints{}); code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "--config") {
		t.Errorf("stderr = %q, want it to mention --config", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want empty", stdout.String())
	}
}

func TestBadConfig(t *testing.T) {
	path := writeConfig(t, 0o644, configBody())
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--config", path}, strings.NewReader(""), &stdout, &stderr, endpoints{}); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	out := stderr.String()
	if strings.Count(strings.TrimSuffix(out, "\n"), "\n") != 0 || !strings.HasSuffix(out, "\n") {
		t.Errorf("stderr is not exactly one line: %q", out)
	}
	if !strings.HasPrefix(out, "pr-mon-telegram: ") || !strings.Contains(out, "chmod 600") {
		t.Errorf("stderr = %q", out)
	}
	if strings.Contains(out, secret) {
		t.Errorf("stderr leaks the secret: %q", out)
	}
}

func TestHelloRoundTrip(t *testing.T) {
	request, err := os.ReadFile("../../../../protocol-fixtures/two-way-handler/hello-request.json")
	if err != nil {
		t.Fatal(err)
	}
	reply, err := os.ReadFile("../../../../protocol-fixtures/two-way-handler/hello-reply.json")
	if err != nil {
		t.Fatal(err)
	}

	polling := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case polling <- struct{}{}:
		default:
		}
		// The server notices a dropped client only once the body is read.
		io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}))
	defer server.Close()

	path := writeConfig(t, 0o600, configBody())
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	var stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		code := run([]string{"--config", path}, stdinR, stdoutW, &stderr, endpoints{Lambda: server.URL, SQS: server.URL})
		stdoutW.Close()
		done <- code
	}()

	if _, err := stdinW.Write(append(bytes.TrimSpace(request), '\n')); err != nil {
		t.Fatal(err)
	}
	lines := bufio.NewScanner(stdoutR)
	if !lines.Scan() {
		t.Fatalf("no reply: %v; stderr: %s", lines.Err(), stderr.String())
	}
	if got, want := lines.Text(), strings.TrimSpace(string(reply)); got != want {
		t.Errorf("reply = %s, want %s", got, want)
	}

	select {
	case <-polling:
	case <-time.After(5 * time.Second):
		t.Fatal("receive loop never polled")
	}

	stdinW.Close()
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("exit = %d, want 0; stderr: %s", code, stderr.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("run did not return after stdin closed")
	}
	if lines.Scan() {
		t.Errorf("extra stdout line: %s", lines.Text())
	}
	if strings.Contains(stderr.String(), secret) {
		t.Error("stderr leaks the secret")
	}
}

func TestLogLinesHaveNoTimestamp(t *testing.T) {
	var out bytes.Buffer
	newLogger(&out).Info("hello", "k", "v")
	line := out.String()
	if strings.Contains(line, "time=") {
		t.Errorf("log line has a time attribute: %q", line)
	}
	if !strings.Contains(line, "msg=hello") || !strings.Contains(line, "k=v") {
		t.Errorf("log line = %q, want level, message and attributes kept", line)
	}
}
