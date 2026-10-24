# pr-mon Telegram handler Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `pr-mon-telegram`, a Go program pr-mon runs as its two-way handler, speaking handler protocol v1 and doing the work through the existing telegram-sender Lambda and the `pr-mon` SQS queue.

**Architecture:** Four small packages under `internal/` and one command. `aws` and most of `telegram` are ported from pr-mon's history (`8ce836d~1`), where they were reviewed and live-tested; `config` reads the settings file; `protocol` runs the stdin/stdout session against a `Backend` interface that `telegram.Client` satisfies.

**Tech Stack:** Go 1.27, standard library plus `github.com/BurntSushi/toml` v1.6.0.

**Spec:** `docs/superpowers/specs/2026-10-23-pr-mon-telegram-handler-design.md`. The protocol contract it implements is pr-mon's `docs/two-way-handler.md`, in the `two-way-notifications` worktree (read it before Task 4).

## Global Constraints

- Repo `telegram-webhook` (now `notifiers/telegram/` in pr-mon), branch `pr-mon-handler`. Commit only; **never push**.
- Module path `github.com/acheris-labs/telegram-webhook`, `go 1.27`.
- The only dependency is `github.com/BurntSushi/toml` v1.6.0 (the version pr-mon uses). Ask before any other.
- Do not touch the Python, Terraform, `~/.config/pr-mon`, or `/Applications`.
- The route is the constant `pr-mon`; callback data is `pr-mon:<key>`, at most 64 bytes.
- stdout carries only protocol lines. Nothing written anywhere (stderr, replies, errors) may contain `secret_access_key`'s value.
- Skipped queue updates are logged by `update_id` only, never the body.
- Standard `testing`, no network. `make test` (race detector) and `make lint` (gofmt + go vet) stay clean.
- Commit messages: a plain sentence subject in pr-mon's style, ending with
  `Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>`.
- Porting: read old files with `git show 8ce836d~1:<path>`.

## Review Focus

1. **stdin closes while a 20 s long poll is in flight**: the handler must exit within about a second, not after the poll returns (pr-mon kills it at 5 s). Pinned in Task 4 (`TestEOFExitsPromptly`) and Task 5 (`TestHelloRoundTrip` closes stdin with the receive loop running).
2. **Acking a tap SQS has already redelivered**: on a FIFO queue an expired receipt fails as `InvalidParameterValue … ReceiptHandle … expired`, and an unknown one as `ReceiptHandleIsInvalid`. Both must be `ok`. Pinned in Task 1 (`TestReceiptGone`) and Task 3 (`TestAckUnknownReceiptIsOK`).
3. **The Mac wakes with no network**: Receive fails repeatedly. The loop must back off 1 s → 1 min without exiting or flooding stderr, and posts must still be answered meanwhile. Pinned in Task 4 (`TestReceiveErrorsBackOff`, `TestRequestsAnsweredWhileReceiveFails`).
4. **A typo'd or malformed settings file containing the secret**: the error names the file and the line or key, never a value. Pinned in Task 2.
5. **pr-mon sends a post and an ack while a long poll is in flight**: requests must not wait on the receive loop or on each other. Pinned in Task 4 (`TestConcurrentRequests`).

---

### Task 1: Module, Makefile, and the AWS client

**Files:**
- Create: `go.mod`, `go.sum`, `Makefile`
- Modify: `.gitignore` (add `/bin/`)
- Create: `internal/aws/client.go`, `internal/aws/sigv4.go`, `internal/aws/aws_test.go` (ported from `internal/aws/` at `8ce836d~1`)

**Interfaces:**
- Produces: `aws.Client{Credentials, Region, HTTP, LambdaEndpoint, SQSEndpoint, Now}`, `aws.Credentials{AccessKeyID, SecretAccessKey}`, `(*Client).Invoke(ctx, function string, payload any) (json.RawMessage, error)`, `(*Client).Receive(ctx, queueURL string, wait time.Duration, max int) ([]SQSMessage, error)`, `(*Client).DeleteMessage(ctx, queueURL, receipt string) error`, `aws.SQSMessage{ReceiptHandle, Body}`, `aws.FunctionError{Message}`, **new** `aws.HTTPError{Status string; Body string}`, **new** `aws.ReceiptGone(err error) bool`.

- [ ] **Step 1: Module and Makefile.** `go mod init github.com/acheris-labs/telegram-webhook`, set `go 1.27`. Makefile targets: `test` (`go test -race ./...`), `lint` (pr-mon's, over `GOFILES_DIRS := $(wildcard cmd internal)` so it works before `cmd/` exists: `@test -z "$$(gofmt -l $(GOFILES_DIRS))" || { gofmt -l $(GOFILES_DIRS); echo "run make fmt"; exit 1; }` then `go vet ./...`), `fmt` (`gofmt -w $(GOFILES_DIRS)`), `build` (`go build -o bin/pr-mon-telegram ./cmd/pr-mon-telegram`), `install` (`go build -o $(HOME)/.local/bin/pr-mon-telegram ./cmd/pr-mon-telegram`). Add `/bin/` to `.gitignore`.

- [ ] **Step 2: Port the package and its tests verbatim**, changing only the package comment (it's the handler's, not pr-mon's) and imports. Run `go test ./internal/aws/` — the eight ported tests pass.

- [ ] **Step 3: Write the failing tests** in `aws_test.go`:

```go
func TestHTTPErrorKeepsStatusAndBody(t *testing.T) // 400 with a JSON body → errors.As(err, &*HTTPError) true, Status "400 Bad Request", Body == the body; err.Error() still contains the body text (TestSQSErrorIsReported keeps passing)

func TestReceiptGone(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"invalid", &HTTPError{Status: "400 Bad Request", Body: `{"__type":"com.amazonaws.sqs#ReceiptHandleIsInvalid","message":"The input receipt handle \"x\" is not a valid receipt handle."}`}, true},
		{"expired fifo", &HTTPError{Status: "400 Bad Request", Body: `{"__type":"com.amazonaws.sqs#InvalidParameterValue","message":"Value x for parameter ReceiptHandle is invalid. Reason: The receipt handle has expired."}`}, true},
		{"other bad parameter", &HTTPError{Status: "400 Bad Request", Body: `{"__type":"com.amazonaws.sqs#InvalidParameterValue","message":"Value 0 for parameter WaitTimeSeconds is invalid."}`}, false},
		{"queue missing", &HTTPError{Status: "400 Bad Request", Body: `{"__type":"com.amazonaws.sqs#QueueDoesNotExist","message":"The specified queue does not exist."}`}, false},
		{"wrapped", fmt.Errorf("sqs DeleteMessage: %w", &HTTPError{Status: "400 Bad Request", Body: `{"__type":"com.amazonaws.sqs#ReceiptHandleIsInvalid"}`}), true},
		{"network", errors.New("dial tcp: no route to host"), false},
		{"nil", nil, false},
	}
	...
}
```

- [ ] **Step 4: Run** `go test ./internal/aws/` — the two new tests fail to compile.

- [ ] **Step 5: Implement.** `post` returns `&HTTPError{Status: response.Status, Body: string(bytes.TrimSpace(body))}` for a non-2xx; `Error()` returns `Status + ": " + Body` (the old text). `ReceiptGone` uses `errors.As`, then: body contains `ReceiptHandleIsInvalid`, or contains both `InvalidParameterValue` and `ReceiptHandle`.

- [ ] **Step 6: Verify** `make test` and `make lint` pass.

- [ ] **Step 7: Commit** `go.mod Makefile .gitignore internal/aws` — "Port pr-mon's AWS client: SigV4, Lambda invoke, SQS receive and delete".

### Task 2: Settings file

**Files:**
- Create: `internal/config/config.go`, `internal/config/config_test.go`

**Interfaces:**
- Produces: `config.Config{Region, QueueURL, SenderFunction, ChatID string; AWS config.AWSKey}` with TOML keys `region`, `queue_url`, `sender_function`, `chat_id`, `[aws]`; `config.AWSKey{AccessKeyID, SecretAccessKey}` with keys `access_key_id`, `secret_access_key`; `config.Load(path string) (Config, error)`.

Error texts (exact; `<path>` is the path given):
- not 0600-ish: `<path> must be readable only by you (chmod 600)` — refuse when `perm & 0o077 != 0`
- unreadable/missing: the `os` error (it names the path, not contents)
- bad TOML with a position: `<path> isn't valid TOML (line N)`; without: `<path> isn't valid TOML`
- unknown keys: `<path>: unknown setting "<key>"` (key names joined with `, ` if several; `MetaData.Undecoded()`)
- missing required: `<path>: <key> is required` naming the first missing of `region`, `queue_url`, `sender_function`, `aws.access_key_id`, `aws.secret_access_key`, in that order

- [ ] **Step 1: Write the failing tests** (`t.TempDir()`, files written with mode 0600 unless the test says otherwise; a const `secret = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"`):
  - `TestLoad` — full file → every field as written; `chat_id` omitted → `""`.
  - `TestTooOpen` — mode 0644 → error ends `must be readable only by you (chmod 600)`.
  - `TestMissingFile` — error mentions the path.
  - `TestRequired` — table: each required key removed in turn → `<path>: <key> is required`.
  - `TestUnknownKey` — `chat = "1"` added → `unknown setting "chat"`.
  - `TestSecretStaysOutOfErrors` — table of broken files that each contain `secret` in the offending spot: unquoted value (`secret_access_key = <secret>`), wrong type (`secret_access_key = [<quoted secret>]`), unterminated string, and the secret used as a key name. For each: error is non-nil and `!strings.Contains(err.Error(), secret)` and `!strings.Contains(err.Error(), "wJalr")`.

- [ ] **Step 2: Run** `go test ./internal/config/` — fails to compile.

- [ ] **Step 3: Implement `Load`.** `go get github.com/BurntSushi/toml@v1.6.0`. Order: stat + permission check, read, `toml.Decode`, undecoded keys, required fields. Decode errors go through a `decodeError` like pr-mon's removed one (`toml.ParseError` → line only; anything else → no detail). An unknown key that is itself the secret must not be quoted: quote only keys matching `^[A-Za-z0-9_.-]{1,40}$`, else say `unknown setting on line …` if available, else `an unknown setting`.

- [ ] **Step 4: Verify** `go test ./internal/config/` passes, then `make test lint`.

- [ ] **Step 5: Commit** `go.mod go.sum internal/config` — "Read the handler's settings from a private TOML file".

### Task 3: Telegram client

**Files:**
- Create: `internal/telegram/telegram.go`, `internal/telegram/telegram_test.go` (ported from `internal/twoway/telegram/` at `8ce836d~1`)

**Interfaces:**
- Consumes: Task 1's `aws` package.
- Produces:
  - `telegram.Route = "pr-mon"`
  - `telegram.Message{Status string "status"; Title string "title"; Lines []string "lines"; Buttons [][]Button "buttons"}` (JSON tags as quoted; this is the protocol's Message)
  - `telegram.Button{Label string "label"; Key string "key,omitempty"; URL string "url,omitempty"}`
  - `telegram.Tap{MessageID, Key, Receipt string}`
  - `telegram.ErrGone`, `telegram.ErrTooOld` (sentinels; returned errors wrap them with `%w` and keep Telegram's text)
  - `telegram.Settings{QueueURL, SenderFunction, ChatID string}`
  - `telegram.New(settings Settings, client *aws.Client, log *slog.Logger) *Client`
  - `(*Client).Post(ctx, Message) (string, error)`, `Edit(ctx, id string, Message) error`, `Delete(ctx, id string) error`, `Receive(ctx) ([]Tap, error)`, `Ack(ctx, receipt string) error`

Changes from the old channel, nothing else: no `twoway` import, no `Name()`; route is the constant; `Ack` takes the receipt string; logger is passed in.

- [ ] **Step 1: Port the code and tests**, adapting the tests to the changes above (drop `TestName`; `newChannel` builds via `New(..., slog.New(slog.DiscardHandler))`). Run `go test ./internal/telegram/` — all ported tests pass.

- [ ] **Step 2: Write the failing tests:**
  - `TestNonNumericIDIsGone` — `Edit(ctx, "abc", msg)` and `Delete(ctx, "abc")` → `errors.Is(err, ErrGone)`, and the sender was not invoked.
  - `TestAckUnknownReceiptIsOK` — fake SQS answers DeleteMessage with 400 `{"__type":"com.amazonaws.sqs#ReceiptHandleIsInvalid","message":"..."}` → `Ack` returns nil; with 400 `QueueDoesNotExist` → non-nil.
  - `TestSkippedUpdateLogsOnlyUpdateID` — queue holds `{"update_id":99,"message":{"chat":{"id":5},"from":{"first_name":"Mallory"},"text":"hi"}}` and a callback for another route (`backup:k1`, update_id 100, from first_name "Mallory"). Log to a buffer via `slog.NewTextHandler`: buffer contains `99` and `100`, does not contain `Mallory`; both receipts were deleted; no taps returned.

- [ ] **Step 3: Run** — the three fail.

- [ ] **Step 4: Implement.** `strconv.Atoi` failure → `fmt.Errorf("%w: message id %q isn't one this handler sent", ErrGone, id)`. `Ack` returns nil when `aws.ReceiptGone(err)`. Add `UpdateID int64 "update_id"` to the parsed update and log `update_id` and a short reason (`"not a button tap"`, `"another route"`, `"unreadable"`) instead of the body.

- [ ] **Step 5: Verify** `make test lint`.

- [ ] **Step 6: Commit** `internal/telegram` — "Port the Telegram rendering and relay calls from pr-mon".

### Task 4: Protocol session

**Files:**
- Create: `internal/protocol/session.go`, `internal/protocol/session_test.go`
- Create: `testdata/two-way-handler/*.json` (copied: `cp <pr-mon>/protocol-fixtures/two-way-handler/*.json testdata/two-way-handler/`) and `testdata/two-way-handler/README.md` (one line: copied from pr-mon's `protocol-fixtures/two-way-handler/` at commit `e755149`; refresh by copying again)

**Interfaces:**
- Consumes: Task 3's `telegram.Message`, `telegram.Tap`, `telegram.ErrGone`, `telegram.ErrTooOld`.
- Produces:
  - `protocol.Version = 1`, `protocol.Name = "Telegram"`, `protocol.MaxLine = 1 << 20`
  - `protocol.Backend` interface: `Post(ctx, telegram.Message) (string, error)`, `Edit(ctx, string, telegram.Message) error`, `Delete(ctx, string) error`, `Receive(ctx) ([]telegram.Tap, error)`, `Ack(ctx, string) error` — `*telegram.Client` satisfies it.
  - `protocol.Options{RequestTimeout, MinBackoff, MaxBackoff time.Duration; Log *slog.Logger; Sleep func(ctx context.Context, d time.Duration) error}`; zero values mean 25 s, 1 s, 1 min, discard, a ctx-aware timer.
  - `protocol.Run(ctx context.Context, in io.Reader, out io.Writer, backend Backend, opts Options) error` — returns nil when `in` reaches EOF or ctx is cancelled; an error when writing to `out` fails or reading `in` fails other than EOF.

Behaviour (from the spec and `docs/two-way-handler.md`):
- Each request runs in its own goroutine with `context.WithTimeout(runCtx, RequestTimeout)`. Lines to `out` are written whole under one mutex, one `Write` per line ending `\n`.
- `hello` → `{"protocol":1,"name":"Telegram"}` whatever version was sent. After the hello reply is written, start the receive loop once (a second hello is answered again but starts nothing).
- Any other op before the hello reply was written → `failed`, message `hello first`. Unknown op → `failed`, message `unknown op "<op>"`. Args that don't decode → `failed`.
- Errors: `errors.Is(err, telegram.ErrGone)` → `gone`; `ErrTooOld` → `too_old`; anything else → `failed`. `message` is `err.Error()`.
- A line that isn't a JSON object, has no integer `id`, or is longer than `MaxLine`: one log line, skipped; reading continues with the next line.
- Receive loop: `Receive(runCtx)`; each tap → `{"event":"tap","args":{"message_id","key","receipt"}}`. On error: log `err`, `Sleep(backoff)`, double up to `MaxBackoff`; a successful receive resets to `MinBackoff`. A run of identical consecutive errors logs once, then again when the error changes or a receive succeeds.
- EOF on `in` or ctx cancelled: cancel `runCtx`, wait for every goroutine, return.

- [ ] **Step 1: Write the failing tests.** Drive `Run` through `io.Pipe`s with a fake `Backend` (methods configurable per test via func fields; `Receive` defaults to blocking until ctx is done). Helpers: `send(line)`, `next(t) map[string]any` (read one line with a 2 s timeout), `fixture(name) map[string]any`. Compare JSON objects with `reflect.DeepEqual` after unmarshalling both into `map[string]any`.
  - `TestFixtureSession` — send hello, post, edit, delete, ack request fixtures in order, waiting for each reply; fake Post returns `"1082"`. Replies equal `hello-reply`, `post-reply`, `ok-reply` (id 3), `ok-reply` with id 4, `ok-reply` with id 5. Fake saw the post fixture's Message exactly (title, lines, both button rows, URL button without key).
  - `TestErrorRepliesMatchFixtures` — after hello, `delete-request` with fake Delete returning `fmt.Errorf("%w: x", telegram.ErrGone)` → reply has the same keys as `error-reply-gone`, `ok: false`, `error.code == "gone"`; with `ErrTooOld` → `"too_old"`; with `errors.New("boom")` → `"failed"`, message `boom`.
  - `TestTapEventMatchesFixture` — fake Receive returns `[{1082 xx2ev6d7 r-7731}]` once → the event line equals `tap-event.json`.
  - `TestNoTapBeforeHello` — fake Receive records calls; send `post-request` first → `failed`/`hello first`, and Receive has not been called; then hello → Receive is called.
  - `TestHelloAnswersVersionOne` — hello with `"protocol":2` → `result.protocol == 1`, `result.name == "Telegram"`.
  - `TestUnknownOp` — `{"id":9,"op":"frob","args":{}}` → `failed`, message `unknown op "frob"`.
  - `TestBadLinesSkipped` — send `not json`, `[1,2]`, `{"op":"hello"}` (no id), then a `MaxLine+1`-byte line, then hello → the only reply is hello's.
  - `TestConcurrentRequests` — fake Post blocks on a channel; send post (id 2) then edit (id 3); edit's reply arrives first; release Post; post's reply arrives.
  - `TestRequestTimeout` — `RequestTimeout: 50ms`, fake Post waits on ctx → reply `failed`.
  - `TestReceiveErrorsBackOff` — `MinBackoff 1s, MaxBackoff 4s` with a recording `Sleep` that returns immediately; Receive fails 4 times with the same error, then succeeds, then fails once more with that error, then blocks → sleeps are exactly `[1s 2s 4s 4s 1s]`, and the log has exactly 2 lines (one for the run of 4, one for the failure after the success).
  - `TestRequestsAnsweredWhileReceiveFails` — Receive always fails, Sleep blocks on ctx; a post still gets its reply.
  - `TestEOFExitsPromptly` — after hello (Receive blocks on ctx, Post blocks on ctx), send a post, then close the input pipe → `Run` returns nil within 1 s.
  - `TestWriteFailureEnds` — close the output reader, send hello → `Run` returns a non-nil error.

- [ ] **Step 2: Run** `go test ./internal/protocol/` — fails to compile.

- [ ] **Step 3: Implement `Run`.** Read lines with a `bufio.Reader` of 64 KiB and `ReadSlice('\n')`, accumulating up to `MaxLine`; on overflow keep reading to the newline and drop the line (a `bufio.Scanner` stops for good at its limit, so don't use one).

- [ ] **Step 4: Verify** `go test -race -count=5 ./internal/protocol/` passes, then `make test lint`.

- [ ] **Step 5: Commit** `internal/protocol testdata` — "Speak pr-mon's two-way handler protocol over stdin and stdout".

### Task 5: The command, install, README

**Files:**
- Create: `cmd/pr-mon-telegram/main.go`, `cmd/pr-mon-telegram/main_test.go`
- Modify: `README.md` (new section "pr-mon handler" before "Tests"; "Tests" gains the Go command)

**Interfaces:**
- Consumes: `config.Load`, `aws.Client`, `telegram.New`, `protocol.Run`.
- Produces: `main()` → `os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, endpoints{}))`; `func run(args []string, stdin io.Reader, stdout, stderr io.Writer, ep endpoints) int`; `type endpoints struct{ Lambda, SQS string }` (empty means AWS's; tests only).

Behaviour:
- Flags (`flag.NewFlagSet`, `ContinueOnError`, output to stderr): `--config <path>`, required. Missing or extra args → one usage line on stderr, exit 2.
- `config.Load` error → stderr `pr-mon-telegram: <err>` as one line, exit 1.
- Logger: `slog.New(slog.NewTextHandler(stderr, nil))`.
- `signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)` around `protocol.Run`. Run returns nil → exit 0; error → log it, exit 1.

- [ ] **Step 1: Write the failing tests:**
  - `TestNoConfigFlag` — `run(nil, …)` → 2, stderr mentions `--config`.
  - `TestBadConfig` — a 0644 file containing the secret → 1, stderr is exactly one line, starts `pr-mon-telegram: `, contains `chmod 600`, not the secret.
  - `TestHelloRoundTrip` — valid 0600 config; an `httptest` server whose SQS ReceiveMessage blocks until the request ctx is done (so the receive loop is mid-poll); `endpoints` point at it. Write `hello-request.json` to stdin; the first stdout line equals `hello-reply.json`. Close stdin → `run` returns 0 within 2 s; stdout had nothing else on it.

- [ ] **Step 2: Run** `go test ./cmd/...` — fails to compile.

- [ ] **Step 3: Implement** `main.go`.

- [ ] **Step 4: README section "pr-mon handler"**, short, covering:
  - what it is (one paragraph: pr-mon's two-way handler; pr-mon knows nothing about Telegram or AWS);
  - build: `make install` → `~/.local/bin/pr-mon-telegram`;
  - the settings file at `~/.config/pr-mon-telegram/config.toml`, `chmod 600`, with the example from the spec and where each value comes from (`terraform output queue_urls`, `terraform output reader_access_key_ids`, `terraform output -json reader_secret_access_keys`; `chat_id` optional);
  - pr-mon's side: `[two_way] command = "/Users/<you>/.local/bin/pr-mon-telegram --config /Users/<you>/.config/pr-mon-telegram/config.toml"` — absolute paths because launchd has no shell environment;
  - "Tests" section: add `make test`.
  Remove the "Limits" bullet "telegram-sender sends plain text only…" (no longer true).

- [ ] **Step 5: Verify** `make test lint`, then `make build && ./bin/pr-mon-telegram; echo $?` → usage line, `2`.

- [ ] **Step 6: Commit** `cmd README.md` — "Add the pr-mon-telegram command".

---

## After the plan (controller, with the user)

Not implementer tasks:
1. End-to-end: quit the live PrMon; run the isolated test copy (scratchpad `live/`) on the `two-way-notifications` build with `[two_way] command` pointing at `bin/pr-mon-telegram` and a scratch settings file; the user taps a button on a test PR; reopen PrMon.
2. Switch-over script for the live setup, reviewed and run by the user.
