# Two-Way Handler Protocol Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace pr-mon's built-in Telegram/AWS two-way channel with a long-running handler program that pr-mon talks to over a documented JSON-lines protocol.

**Architecture:** A new `internal/twoway/handler` package implements the existing `twoway.Channel` interface by running the `[two_way] command` as a child process (stdin: requests; stdout: replies and tap events). The service learns availability through a callback, skips posts while the handler is down, and keeps messages it couldn't retire tracked until the handler is back. `internal/aws`, `internal/twoway/telegram` and `credentials.toml` are deleted.

**Tech Stack:** Go standard library only (`os/exec`, `encoding/json`, `bufio`, `log/slog`); existing test fakes in `internal/testfixtures`.

**Spec:** `docs/superpowers/specs/2026-10-23-two-way-handler-design.md` (supersedes the transport parts of `docs/superpowers/specs/2026-10-22-two-way-notifications-design.md`; everything else there still holds).

## Global Constraints

- Work in a worktree, branch `two-way-notifications`. Add commits; never rewrite history.
- Dependencies: standard library plus what `go.mod` already has (BurntSushi/toml, bubbletea, bubbles, lipgloss, bubblezone). No new modules.
- `make test` (race detector) and `make lint` (gofmt + go vet) pass at the end of every task.
- `protocol.Version` does not change; `docs/protocol.md` stays in step with the code (`internal/protocol/spec_test.go` checks it).
- Handler protocol version is **1**. Timeouts: `hello` 10 s, other requests 30 s. Restart backoff 1 s doubling to 1 min, reset after a successful `hello`. Shutdown grace 5 s. Keys: 1–16 chars of `a-z0-9` (pr-mon makes 8).
- Error codes on the wire: `gone`, `too_old`, `failed`; any other code is treated as `failed`.
- Warning prefix for two-way problems in `Status.Warnings`: `"Two-way notifications are off: "` (already used at startup).
- Name used before any `hello` succeeds: `"Two-way"`.
- One-way notifications (`internal/notify` script and desktop) are not changed.
- Tests: standard `testing`, table-driven where it helps.

## Review Focus

1. **Junk on the handler's stdout** (a stray log line, a non-object, a 100 KB line): skipped and logged; the session and later replies carry on. — Task 2 test `TestJunkOnStdoutIsSkipped`.
2. **A handler that never replies**: the request fails after the request timeout, and `Close` still returns within the grace period plus slack, killing the process. — Task 2 tests `TestRequestTimesOut`, `TestCloseKillsAHandlerThatIgnoresStdin`.
3. **A handler that crash-loops** (exits right after starting): restarts back off to the maximum, and `OnChange` is called only when availability or its reason changes, not once per attempt. — Task 2 test `TestCrashLoopBacksOff`.
4. **Backend stop while requests are in flight**: `Monitor.Stop` returns promptly, the handler process is gone afterwards. — Task 2 `TestCloseFailsWaitingRequests`, Task 3 `TestStopClosesTheChannel`.
5. **Handler down when a message must be retired** (PR closed, status moved, superseded): the message stays tracked with `Retiring` set and is deleted once the handler is ready again; a tap on such a message only retries the delete. — Task 3 tests `TestRetirementWaitsForTheHandler`, `TestTapOnARetiringMessageRetriesTheDelete`.

---

### Task 1: Protocol types, document and fixtures

**Files:**
- Modify: `internal/twoway/twoway.go`
- Create: `internal/twoway/handler/wire.go`, `internal/twoway/handler/wire_test.go`
- Create: `docs/two-way-handler.md`
- Modify: `cmd/gen-fixtures/main.go`
- Create (generated): `protocol-fixtures/two-way-handler/*.json`

**Interfaces:**
- Produces in `internal/twoway`:
  - `var ErrUnavailable = errors.New("the two-way handler isn't running")`
  - `type Availability struct { Ready bool; Name, Why string }` — `Name` set when `Ready`; `Why` a sentence when not.
- Produces in `internal/twoway/handler` (unexported unless noted):
  - `const Protocol = 1` (exported)
  - `type request struct { ID int64 \`json:"id"\`; Op string \`json:"op"\`; Args any \`json:"args"\` }`
  - `type incoming struct { ID *int64; OK bool; Result json.RawMessage; Error *wireError; Event string; Args json.RawMessage }` with json names `id ok result error event args`
  - `type wireError struct { Code, Message string }` (json `code`, `message`) and `func (e wireError) err() error`: `gone` → error wrapping `twoway.ErrGone`, `too_old` → wrapping `twoway.ErrTooOld`, anything else → plain error; message text included.
  - `func encodeMessage(twoway.Message) wireMessage` — json `status`, `title`, `lines`, `buttons`; a button is `label` plus `key` or `url` (omitempty). Nil `lines`/`buttons`/rows encode as `[]`, never `null`.
  - `type tapArgs struct { MessageID, Key, Receipt string }` (json `message_id`, `key`, `receipt`).
  - `func Fixtures() map[string][]byte` (exported): file name → one JSON line, for `gen-fixtures`.

- [ ] **Step 1: Write failing tests** in `wire_test.go`:
  - `TestEncodeMessage`: a message with status, title, two lines and rows `[[{Label:"Squash & merge",Key:"xx2ev6d7"}],[{Label:"Open",URL:"https://x"},{Label:"Dismiss",Key:"kofmkufw"}]]` marshals to exactly `{"status":"READY","title":"…","lines":["…","…"],"buttons":[[{"label":"Squash & merge","key":"xx2ev6d7"}],[{"label":"Open","url":"https://x"},{"label":"Dismiss","key":"kofmkufw"}]]}`; an empty `twoway.Message{}` marshals to `{"status":"","title":"","lines":[],"buttons":[]}`.
  - `TestErrorCodes` (table): `gone` → `errors.Is(err, twoway.ErrGone)`; `too_old` → `errors.Is(err, twoway.ErrTooOld)`; `failed` and `"weird"` → neither, and `err.Error()` contains the message.
  - `TestFixturesMatchDisk`: every entry of `Fixtures()` equals the file `protocol-fixtures/two-way-handler/<name>` (trailing newline allowed), and every reply/event fixture decodes into `incoming` with the expected values (hello reply → protocol 1, name `"Telegram"`; post reply → `message_id` `"1082"`; error replies → codes `gone`, `too_old`; tap event → `message_id` `"1082"`, `key` `"xx2ev6d7"`, `receipt` non-empty). Follow how the existing `protocol-fixtures` tests locate the directory (grep `protocol-fixtures` in `*_test.go`).
- [ ] **Step 2:** `go test ./internal/twoway/...` — expect compile failure.
- [ ] **Step 3: Implement** `twoway.go` additions and `wire.go`. `Fixtures()` builds requests with the real `request`/`encodeMessage` (hello with `protocol` 1, `app` `"pr-mon"`, `version` `"1.2.3"`; post; edit; delete; ack) and the handler-side lines from literal structs. File names: `hello-request.json`, `hello-reply.json`, `post-request.json`, `post-reply.json`, `edit-request.json`, `delete-request.json`, `ack-request.json`, `ok-reply.json`, `error-reply-gone.json`, `error-reply-too-old.json`, `tap-event.json`.
- [ ] **Step 4:** Extend `cmd/gen-fixtures` to write `handler.Fixtures()` into `protocol-fixtures/two-way-handler/`; run `make fixtures`.
- [ ] **Step 5: Write `docs/two-way-handler.md`** for handler authors from the spec's *The protocol* section: transport, shapes, operations table, events table, `Message`, error codes, versioning, lifecycle (timeouts, restarts, mismatch not retried, shutdown, ids valid across restarts, unknown id → `gone`), and a short example session using the fixture lines. Point to `protocol-fixtures/two-way-handler/`.
- [ ] **Step 6:** `make test && make lint` — pass.
- [ ] **Step 7: Commit** `Define the two-way handler protocol`.

---

### Task 2: The handler runner

**Files:**
- Create: `internal/twoway/handler/runner.go`, `internal/twoway/handler/runner_test.go`

**Interfaces:**
- Consumes: Task 1 types.
- Produces:
  ```go
  type Settings struct {
      Argv     []string
      Version  string // pr-mon's build version, sent in hello
      Log      *slog.Logger
      OnChange func(twoway.Availability)
      // Zero means the default (Global Constraints); tests shorten them.
      HelloTimeout, RequestTimeout, FirstBackoff, MaxBackoff, ShutdownGrace, ReceiveWait time.Duration
  }
  func Start(ctx context.Context, settings Settings) *Runner // returns at once; the process starts in the background
  func (r *Runner) Close() error                            // idempotent
  var _ twoway.Channel = (*Runner)(nil)
  ```
  `ReceiveWait` default 20 s.

Behaviour (spec *The protocol*, *Lifecycle*):
- A supervisor goroutine starts the process (stdin/stdout/stderr pipes, inherited env), sends `hello`, and on a matching reply records the name and calls `OnChange({Ready: true, Name})`.
- Failures that restart with backoff: start error (not found / not executable), `hello` timeout or error reply, process exit. Each calls `OnChange({Ready: false, Why})` where `Why` names the cause and, for an exit, the exit status and the last stderr line. `OnChange` is not called again when nothing changed.
- Protocol mismatch: `Why` = `"Two-way handler speaks protocol N; pr-mon needs 1"`; the process is stopped and not restarted.
- While not ready, `Post`/`Edit`/`Delete`/`Ack` return an error wrapping `twoway.ErrUnavailable` at once. On exit, every waiting request fails with an error wrapping `ErrUnavailable`.
- Requests: unique increasing ids; concurrent requests allowed; writes to stdin serialized; replies matched by id; timeout → plain error (not `ErrUnavailable`); late reply logged and skipped.
- stdout reader: lines up to 1 MiB; a longer line, a non-object, or an unknown id is logged and skipped without ending the session. Unknown events are skipped. Taps before the `hello` reply are skipped.
- stderr: each line logged at Info with the handler marked as the source; the last line kept for `Why`.
- `Receive`: waits up to `ReceiveWait` for a tap, then returns what has arrived (possibly none), never an error except `ctx.Err()`.
- `Name()`: the `hello` name, `"Two-way"` before one.
- `Close` (and `ctx` cancellation): stop supervising, close stdin, wait up to `ShutdownGrace`, kill, wait for the process and goroutines; waiting requests fail.
- `OnChange` is never called while the runner holds its own lock.

- [ ] **Step 1: Write the fake handler** in `runner_test.go` using the helper-process pattern: `TestMain` checks an env var (e.g. `PRMON_FAKE_HANDLER=<scenario>`) and, if set, runs that scenario on stdin/stdout instead of the tests. Scenarios as the tests below need (well-behaved; wrong protocol; never replies; exits at once; prints junk; ignores stdin close; error codes for chosen message ids; emits a tap after a given post).
- [ ] **Step 2: Write failing tests** (short timeouts via `Settings`):
  - `TestHelloMakesItReady` — `OnChange` gets `{Ready: true, Name: "Fake"}`; `Name()` is `"Fake"`; before that it was `"Two-way"`.
  - `TestProtocolMismatchIsNotRetried` — `Why` equals `"Two-way handler speaks protocol 2; pr-mon needs 1"`; the scenario's start count stays 1 after several backoff periods.
  - `TestPostEditDeleteAck` — `Post` returns the handler's id; `Edit`/`Delete`/`Ack` succeed; the fake saw the exact request lines (compare with `encodeMessage` output).
  - `TestErrorCodesMapToErrors` — delete of the scenario's "gone" id → `errors.Is(err, twoway.ErrGone)`; "old" id → `ErrTooOld`; unknown code → neither.
  - `TestRepliesOutOfOrder` — two concurrent posts answered in reverse order each get their own id.
  - `TestRequestTimesOut` — no reply → error within `RequestTimeout`+slack, `!errors.Is(err, twoway.ErrUnavailable)`.
  - `TestUnavailableBeforeHello` — with a scenario that delays its hello reply, `Post` returns `ErrUnavailable` immediately.
  - `TestTapsAndReceive` — after a post titled to trigger a tap, `Receive` returns `{MessageID, Key, Receipt}`; with no taps `Receive` returns empty after `ReceiveWait`.
  - `TestJunkOnStdoutIsSkipped` — junk lines and a 100 KB line, then a valid reply: the request still succeeds.
  - `TestStderrIsLogged` — a stderr line appears in the `slog` output.
  - `TestCrashLoopBacksOff` — a scenario that exits at once: start times are spaced by growing pauses up to `MaxBackoff`; `OnChange` called once for the repeated identical failure; `Why` includes the exit status and the last stderr line.
  - `TestRestartAfterExit` — the handler exits after ready: a waiting request fails with `ErrUnavailable`, `OnChange` goes not-ready then ready again.
  - `TestCloseFailsWaitingRequests` and `TestCloseKillsAHandlerThatIgnoresStdin` — `Close` returns within `ShutdownGrace`+slack; the process has exited.
  - `TestMissingCommand` — argv naming a non-existent file: not ready, `Why` mentions the path, retried with backoff.
- [ ] **Step 3:** `go test -race ./internal/twoway/handler/` — fail.
- [ ] **Step 4: Implement** `runner.go`.
- [ ] **Step 5:** run the package tests with `-race -count=3` — pass and no goroutine leaks reported.
- [ ] **Step 6:** `make test && make lint`.
- [ ] **Step 7: Commit** `Run the two-way handler as a child process`.

---

### Task 3: Service handles a channel that comes and goes

**Files:**
- Modify: `internal/service/service.go`, `internal/service/twoway.go`, `internal/service/taps.go`, `internal/service/commands.go`
- Modify: `internal/state/state.go` (+ its test)
- Modify: `internal/testfixtures/testfixtures.go` (+ its test)
- Modify: `internal/service/twoway_test.go`, `internal/service/taps_test.go`, `internal/service/service_test.go` (harness)
- Modify: `docs/protocol.md` (`two_way` wording only)

**Interfaces:**
- Consumes: `twoway.ErrUnavailable`, `twoway.Availability` (Task 1).
- Changes `service.Options.TwoWay` to
  `func(ctx context.Context, table config.TwoWayConfig, credentials config.Credentials, onChange func(twoway.Availability)) (twoway.Channel, error)`
  (credentials go in Task 4). `defaultTwoWay` (Telegram, still present) calls `onChange({Ready: true, Name: "Telegram"})` before returning.
- `state.Message` gains `Retiring string \`json:"retiring,omitempty"\`` — the retirement reason of a message waiting for the handler.
- `testfixtures.TwoWay` gains `SetUnavailable(bool)`: while set, `Post`/`Edit`/`Delete` return `twoway.ErrUnavailable`; and `Closed bool` set by a new `Close() error`.

Behaviour (spec *Availability*):
- `Start` builds the channel with `m.ctx` and an `onChange` that, under the monitor lock: ready → `Status.TwoWay = &name`, removes warnings with the two-way prefix; not ready → `Status.TwoWay = nil`, replaces the two-way warning with prefix + `Why`. Emits `status` when the status changed. On ready it also retires every message with `Retiring` set (claim under lock, retire in the background). `Status.TwoWay` starts nil and is set only through `onChange`.
- `retire`: a delete (or too-old edit) failing with `ErrUnavailable` puts the record back with `Retiring = why` and `Keys = nil`, and saves state; no toast.
- `twoWayFailed`: `ErrUnavailable` → log only, no toast.
- `postSample` while unavailable: toast `"Test two-way notification not sent: the handler isn't running"` (severity warning).
- `syncMessages` skips messages with `Retiring` set. `post`'s supersede step claims them like any other message of that PR.
- `handleTap` on a tracked message with `Retiring` set: claim it, retire it with its reason, ack; nothing else.
- Remove the receive warning (`receiveWarning`, `setReceiveWarning`): receive errors are logged and backed off only (availability is reported by `onChange`).
- `Stop`: after `m.busy.Wait()`, if the channel implements `io.Closer`, call `Close()`.
- `docs/protocol.md` `two_way` (Hello and Status rows): "The two-way handler's name while it is running (e.g. `"Telegram"`); null when none is configured or it isn't running."

- [ ] **Step 1: Update the harness**: the builder hook passes `onChange` to the fake; `startTwoWay` calls `onChange({Ready: true, Name: "Fake"})`; expose `h.onChange` for tests.
- [ ] **Step 2: Write failing tests:**
  - `TestTwoWayStatusFollowsAvailability` — after start with the fake ready, `Status.TwoWay` is `"Fake"`; `onChange({Ready:false, Why:"handler exited (exit status 1)"})` → `TwoWay` nil and a warning `"Two-way notifications are off: handler exited (exit status 1)"`, `status` event emitted; ready again → warning gone.
  - `TestPostWhileUnavailableIsQuiet` — fake unavailable, a READY event: no post recorded, no toast.
  - `TestRetirementWaitsForTheHandler` — a tracked message, fake unavailable, the PR closes: message still in `state.json` with `Retiring == "PR closed"` and no keys; fake available + `onChange(ready)`: the fake records the delete and the message is gone from state.
  - `TestTapOnARetiringMessageRetriesTheDelete` — tap on a `Retiring` message: delete recorded, tap acked, nothing else (no edit, no action).
  - `TestSendTestWhileUnavailable` — toast `"Test two-way notification not sent: the handler isn't running"`.
  - `TestStopClosesTheChannel` — after `Stop`, the fake's `Closed` is true.
  - Update existing tests that asserted the receive warning to assert the log-only behaviour instead (or delete them if they only covered the warning).
- [ ] **Step 3:** `go test ./internal/service/ ./internal/state/ ./internal/testfixtures/` — fail.
- [ ] **Step 4: Implement.**
- [ ] **Step 5:** `make fixtures` if fixture output changed; `make test && make lint`.
- [ ] **Step 6: Commit** `Keep two-way messages tracked while the channel is down`.

---

### Task 4: Swap Telegram for the handler; remove AWS

**Files:**
- Modify: `internal/config/config.go`, `internal/config/config_test.go`
- Delete: `internal/config/credentials.go`, `internal/config/credentials_test.go`, `internal/aws/`, `internal/twoway/telegram/`
- Modify: `internal/service/service.go`, `internal/service/twoway.go`, service tests
- Modify: `internal/daemon/run.go` if `service.Options` construction needs the version passed through
- Modify: `README.md` (config section near the `[notifications."owner/repo"]` example)

**Interfaces:**
- Consumes: `handler.Start`, `handler.Settings` (Task 2); `notify.ScriptArgv`; the Task 3 builder hook.
- `config.TwoWayConfig` becomes `{ Command string \`toml:"command"\`; invalid string; legacy bool }`.
- `service.Options.TwoWay` becomes `func(ctx context.Context, table config.TwoWayConfig, onChange func(twoway.Availability)) (twoway.Channel, error)`.

Behaviour:
- `parseTwoWay`: `command` must be a string (else `invalid` = `"[two_way] command must be a string"`); `legacy` is true when `command` is absent and any of `kind`, `region`, `queue_url`, `sender_function`, `route`, `chat_id` is present.
- `Problem()`: `invalid` if set; `legacy` → `"[two_way] now takes a handler command; the AWS settings belong to the handler"`; empty or blank `Command` → `"[two_way] command is missing"`.
- `Save` writes `[two_way]` with only `command = …` (old keys are not kept — spec decision).
- `Start`/`openTwoWay`: `Problem()` → warning `"Two-way notifications are off: " + problem`; `notify.ScriptArgv(Command)` error → warning `"Two-way notifications are off: [two_way] command: " + err`; otherwise build the channel. The default builder is `handler.Start(ctx, handler.Settings{Argv, Version, Log, OnChange})`, where `Version` is the backend's build version (`Options.Version`).
- Remove `defaultTwoWay`'s AWS code, `config.LoadCredentials`, `CredentialsPath`, `Credentials`, and every `credentials.toml` mention outside `docs/superpowers/`.
- README: a short `[two_way]` example (`command = "/path/to/handler"`), one sentence on what a handler is, and a link to `docs/two-way-handler.md`.

- [ ] **Step 1: Write failing tests** in `config_test.go`:
  - `TestTwoWayCommandRoundTrip` — `command = "~/bin/pr-mon-telegram --verbose"` saves and loads unchanged; `Problem()` is `""`.
  - `TestTwoWayProblems` (table): `[two_way]` with only `kind`/`region` → the legacy sentence; `command = ""` → `"[two_way] command is missing"`; `command = 5` → `"[two_way] command must be a string"` and the rest of the config still loads (repos kept).
  - `TestSaveDropsLegacyKeys` — load a legacy table, `Save`, reload: the file has `[two_way]` with `command = ""` and none of the old keys.
- [ ] **Step 2: Service tests**: replace `configureTwoWay`/`writeCredentials`/`awsCredentials` with a `[two_way] command` table; add `TestUnsplittableCommandWarns` (`command = "'unclosed"` → warning starting `"Two-way notifications are off: [two_way] command: "`, `Status.TwoWay` nil) and `TestLegacyTableWarns`.
- [ ] **Step 3:** run — fail. **Step 4: Implement**, deleting the AWS/Telegram packages.
- [ ] **Step 5:** `grep -rn "credentials.toml\|internal/aws\|twoway/telegram\|queue_url" --include='*.go' --include='*.md' --include='*.swift' . | grep -v docs/superpowers` returns nothing; `go mod tidy` leaves `go.mod` unchanged or only shrinks it.
- [ ] **Step 6:** `make fixtures && make test && make lint`; `cd macos/PrMonKit && swift test` still passes (no wire change expected).
- [ ] **Step 7: Commit** `Talk to a two-way handler instead of Telegram over AWS`.

---

### Task 5: Make daemon.log private

**Files:**
- Modify: `internal/files/` (new helper), `internal/daemon/run.go`, `internal/daemon/daemon.go`, `cmd/pr-mon/main.go`
- Test: `internal/daemon/run_test.go` (or a new `internal/files` test)

**Interfaces:**
- Produces: `files.OpenLog(path string) (*os.File, error)` — opens for append, creating with mode `0600`, and chmods an existing file to `0600`.

- [ ] **Step 1: Write failing tests:** `TestOpenLogIsPrivate` — a new file is `0600`; an existing `0644` file becomes `0600` and keeps its content; `TestRotatedLogIsPrivate` — after rotation the new `daemon.log` is `0600`.
- [ ] **Step 2:** run — fail. **Step 3:** use `files.OpenLog` at every `daemon.log` open: `openLog` and `rotate` in `run.go`, the non-darwin spawn in `daemon.go`, `logStartupError` in `cmd/pr-mon/main.go`. The launchd-created file is covered because the backend opens it through `openLog` at start (check this is true in `run.go`; if the backend writes to an inherited stdout instead, chmod `paths.Log()` to `0600` at backend start).
- [ ] **Step 4:** `make test && make lint`.
- [ ] **Step 5: Commit** `Keep daemon.log readable only by its owner`.
