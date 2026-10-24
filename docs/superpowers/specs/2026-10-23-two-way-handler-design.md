# pr-mon Two-Way Handler Protocol — Design

Supersedes the transport parts of
[2026-10-22-two-way-notifications-design.md](2026-10-22-two-way-notifications-design.md):
its *Telegram implementation*, *Configuration* (the `[two_way]` table and
`credentials.toml`) and *AWS side* sections. Everything else in that design
(buttons, keys, tap handling, message state, cleanup, the clients' switch)
stands unchanged.

## Purpose

pr-mon should not know how a two-way message reaches the user. Today the
branch `two-way-notifications` builds Telegram in: AWS SigV4 signing, Lambda
invokes, SQS long-polling, an AWS key in `credentials.toml`. The user wants all
of that to be an implementation detail of a separate tool, the way one-way
notifications already hand off to a user-chosen `script`.

So pr-mon gets a **handler protocol**: pr-mon runs a handler program, sends it
generic messages (post, edit, delete) and receives generic replies and button
taps. The handler encodes those for its chat service and decodes what comes
back. The first handler will be a Go program in `~/src/telegram-webhook` that
reuses the Telegram/AWS code written on this branch; a Slack handler could
replace it with no pr-mon change.

## Background: what exists on the branch

- `internal/twoway` defines the boundary already: `Channel` with `Name`,
  `Post`, `Edit`, `Delete` (`ErrTooOld`, `ErrGone`), `Receive`, `Ack`, and the
  generic `Message` / `Button` / `Tap` types.
- `internal/twoway/telegram` (+ `internal/aws`) is the only implementation.
- `internal/service` (`twoway.go`, `taps.go`) does everything PR-specific
  against `Channel`, and is tested against `testfixtures.TwoWay`.
- `config.TwoWayConfig` holds `kind`, `region`, `queue_url`,
  `sender_function`, `route`, `chat_id`; `config.LoadCredentials` reads
  `credentials.toml`.
- One-way notifications (`internal/notify`): a per-repo `script`, split into
  argv by `notify.ScriptArgv`, run per event with the rendered message on
  stdin and `PR_*` variables in the environment.

## Goals

- pr-mon contains no chat-service or cloud code: no AWS, no Telegram, no
  credentials.
- A documented, versioned protocol that a handler in any language can
  implement from the document alone.
- The two-way behaviour the user already tested live (READY message, buttons,
  merge from the phone, message removed) works the same through a handler.

## Non-goals

- Changing one-way notifications. The `script`, its stdin message and `PR_*`
  variables stay exactly as they are (decided with the user).
- Building the Telegram handler. That is the next project, in
  `~/src/telegram-webhook`; this design only fixes the protocol it implements.
- The AWS audit fixes (secrets in Terraform state, queue policy, the `test`
  route). Separate work.
- More than one handler at a time.

## Anti-goals

- Anything chat-specific leaking into the protocol: no Telegram HTML, no
  `callback_data` prefixes, no chat ids, no 64-byte limits beyond a key-length
  rule pr-mon already meets.
- A handler problem breaking the rest of pr-mon. A missing, crashing or
  misbehaving handler turns two-way off with a status warning; polling,
  one-way notifications and the clients carry on.
- Secrets passing through pr-mon. The handler finds its own credentials.

## Decisions made with the user

| Topic | Decision |
|:------|:---------|
| One-way | Unchanged. |
| Two-way | A new protocol. Outbound and inbound messages are generic; the handler encodes and decodes. |
| Process model | One long-running handler process (not a process per request). |
| Handler | Written in Go, living in `~/src/telegram-webhook` with the Lambdas and Terraform, which stay as they are. |
| Order | pr-mon side first, on a branch. |

*My calls* (the user saw these and didn't object): rework the existing
`two-way-notifications` branch so the AWS code never reaches `main`; the
protocol is pr-mon's and documented in the pr-mon repo; acks travel back from
pr-mon, so the existing ack-after-handling logic stays; `[two_way] command`
replaces the AWS fields.

*My calls after the builder check* (listed for the user to confirm):

- **History:** new commits go on top of the branch; nothing is rewritten. The
  branch is merged with a squash merge, so `main` gets one commit with no AWS
  code in it. (The branch isn't pushed or shared.)
- **Handler down means "unavailable", not "absent".** See *Availability*.
- **Restarts:** every failure to get a working handler is retried with the
  backoff (the command missing or not executable, `hello` timing out or
  erroring, a crash), except a protocol mismatch, which stays off until the
  backend restarts.
- **Old AWS keys in `[two_way]`** are not kept: the next settings save drops
  them. Their values are in `terraform output` and the earlier design, and the
  handler gets its own config.
- **Message ids** must stay valid across handler restarts; an id the handler
  doesn't know gets `gone`. No per-handler tagging of saved messages.

## The protocol

Documented for handler authors in a new `docs/two-way-handler.md` in the pr-mon
repo. Its content, normatively:

### Transport

- pr-mon starts the handler as a child process from `[two_way] command`,
  split into argv the same way as a notification `script`. It inherits the
  backend's environment.
- **stdin** carries pr-mon → handler lines; **stdout** carries handler → pr-mon
  lines. Each line is one JSON object, UTF-8, terminated by `\n`. Nothing else
  may be written to stdout.
- **stderr** is free-form; pr-mon copies each line into its log, prefixed so
  it is clear it came from the handler.
- Unknown fields are ignored by both sides. A line that isn't a JSON object, or
  a reply to an id pr-mon isn't waiting on, is logged and skipped; it does not
  end the session.

### Message shapes (shared with the socket protocol's style)

| Direction | Shape |
|:----------|:------|
| Request, pr-mon → handler | `{"id": 7, "op": "post", "args": {...}}` — `id` is an integer, unique per session |
| Reply, handler → pr-mon | `{"id": 7, "ok": true, "result": {...}}` or `{"id": 7, "ok": false, "error": {"code": "gone", "message": "…"}}` |
| Event, handler → pr-mon | `{"event": "tap", "args": {...}}` — no `id` |

pr-mon may send a request before the previous one is answered; replies may
arrive in any order and are matched by `id`. Whether the handler works on
requests one at a time or concurrently is its choice.

**Error codes:** `gone` (the message no longer exists), `too_old` (the
service refuses to delete a message this old), `failed` (anything else;
`message` says what). Unknown codes count as `failed`.

### Operations

| `op` | `args` | `result` | Notes |
|:-----|:-------|:---------|:------|
| `hello` | `protocol`: int, `app`: `"pr-mon"`, `version`: string | `protocol`: int, `name`: string | Always the first request. `name` is shown in the clients (e.g. `"Telegram"`). The handler sends no events before replying. |
| `post` | `message`: `Message` | `message_id`: string | The id is opaque to pr-mon. |
| `edit` | `message_id`, `message` | `{}` | Replaces the text and buttons. A message without key buttons ends with none. `gone` if it no longer exists. |
| `delete` | `message_id` | `{}` | `too_old` or `gone` as above. |
| `ack` | `receipt`: string | `{}` | The tap with this receipt is handled; the handler may forget it. |

| `event` | `args` | Notes |
|:--------|:-------|:------|
| `tap` | `message_id`, `key`, `receipt`: string | A key button was pressed. A tap not acked may be delivered again (after a handler restart, say); pr-mon handles repeats. |

### `Message`

```json
{"status": "READY",
 "title": "acme/api #12 · Fix login redirect",
 "lines": ["octocat · fix-login → main"],
 "buttons": [[{"label": "Squash & merge", "key": "xx2ev6d7"}],
             [{"label": "Open", "url": "https://github.com/acme/api/pull/12"},
              {"label": "Dismiss", "key": "kofmkufw"}]]}
```

- `status`: one of pr-mon's PR statuses or event names (`READY`, `FAILING`,
  `CONFLICT`, `MERGE_FAILED`, …), or `""`. The handler picks any emoji or
  styling.
- `title`, `lines`: plain text. The handler escapes for its markup.
- `buttons`: rows of buttons, possibly none. A button has `label` and exactly
  one of `key` (a tap reports it back) or `url` (opens a link, no tap).
- Keys are 1–16 characters of `a-z` and `0-9` (pr-mon uses 8), so a handler
  can fit one into a small payload with its own prefix.

### Versioning

`protocol` starts at **1**. pr-mon sends the version it speaks; a handler that
can't speak it replies with the version it does. pr-mon uses the handler only
when the reply's `protocol` equals its own; otherwise it stops the handler and
warns ("Two-way handler speaks protocol N; pr-mon needs 1"). Adding an
optional field or a new error code is not a version change; removing or
renaming a field or op, or changing a meaning, is.

### Lifecycle

- **Start:** when `[two_way]` has a `command`, at backend start.
- **Ready:** once `hello` succeeds. Until then, and whenever the handler is
  down, `Status.two_way` and `Hello.two_way` are null; once ready they are the
  handler's `name`, and a `status` event goes out on each change.
- **Message ids** a handler returns must stay valid across its restarts. An
  id it doesn't know (one from another handler, say) gets `gone`.
- **Timeouts:** 10 s for `hello`, 30 s for any other request. A request that
  times out counts as `failed`; a late reply is skipped.
- **Crash, exit, or no working start:** waiting requests fail at once;
  pr-mon starts the handler again after a pause that doubles from 1 s up to
  1 minute, resetting once the handler has stayed up for a minute. This covers a command that
  can't be started (not found, not executable), a `hello` that times out or
  errors, and a handler that exits. A **protocol mismatch** is not retried: it
  stays off, with its warning, until the backend restarts. While it's down, a
  status warning says so, with the exit status and the last stderr line; it
  clears when `hello` succeeds.
- **Shutdown:** pr-mon closes the handler's stdin, waits up to 5 s for it to
  exit, then kills it.

## pr-mon changes

| Change | Detail |
|:-------|:-------|
| New `Channel` implementation | Runs the handler per *The protocol*. `Receive` returns taps the reader has collected (waiting a while when there are none, as the SQS version did); `Ack` sends `ack`. `gone` / `too_old` map to `ErrGone` / `ErrTooOld`. `Name` is the `hello` name. |
| `[two_way]` table | Becomes `command = "…"` only. `Problem()`: missing or empty `command`, or a command that doesn't split. A table that still has the old AWS keys and no `command` gets a specific warning: "[two_way] now takes a handler command; the AWS settings belong to the handler". `Save` writes `command` back. |
| Removed | `internal/aws`, `internal/twoway/telegram`, `config/credentials.go` and `credentials.toml` support, the Telegram-specific wording ("Telegram notification failed" becomes the handler's name). |
| Kept | `internal/twoway` types, `internal/service/twoway.go` and `taps.go` behaviour, `state.Messages`, `testfixtures.TwoWay`, the clients' switch, `Hello.two_way` / `Status.two_way`. |
| Startup | `Start` no longer builds the channel synchronously from credentials; it starts the handler, and two-way becomes available when `hello` succeeds. See *Availability*. |
| `daemon.log` | Readable only by the user: handler stderr may now carry chat content (from the security audit). Every place that opens it (the backend, `cmd/pr-mon`, log rotation) uses `0600` and chmods an existing file to `0600`; the launchd plist path is covered by that chmod at backend start. |

### Availability

Today `m.twoWay` is set once in `Start`, and every two-way path treats nil as
"skip". With a handler, readiness comes and goes. *My call:*

- `m.twoWay` is the runner whenever `[two_way]` has a usable `command`; it
  never changes after `Start`. Its calls fail fast with a new
  `twoway.ErrUnavailable` while the handler isn't ready.
- **Posts** while unavailable are skipped: logged, no toast (the status
  warning already says two-way is down). The PR's next event posts as usual.
- **Retirements** (supersede, status moved on, PR closed, dismiss) that fail
  with `ErrUnavailable` leave the message tracked, so the next sync after the
  handler is back retires it. A message is never orphaned in the chat because
  the handler was down. (Other delete failures behave as today.)
- **Taps** can't arrive while it's down; the receive loop waits for
  readiness instead of backing off on errors.
- **One warning:** the handler-down warning replaces the existing "can't
  receive taps" warning rather than adding a second.
- **Names:** toasts and warnings use the handler's `name` when known, and
  "Two-way" before any `hello` has succeeded.

The socket protocol (`docs/protocol.md`) does not change: `two_way` keeps its
meaning (the running channel's display name, or null).

## Configuration

```toml
[two_way]
command = "/Users/you/.local/bin/pr-mon-telegram"
```

The handler reads its own settings (queue URL, sender function, chat, AWS
keys) from wherever it chooses; pr-mon passes nothing but the protocol.

## Testing

- **Handler runner**, against a fake handler: a test binary re-executed in
  "be a handler" mode (the standard helper-process pattern), scripted per test.
  Covers: hello and name; protocol mismatch; post/edit/delete results; each
  error code; an unknown code as `failed`; out-of-order replies; a timeout and
  the late reply skipped; tap events and `ack`; garbage on stdout skipped;
  stderr reaching the log; crash → waiting requests fail, restart with backoff,
  warning set then cleared; shutdown closes stdin and then kills.
- **Protocol fixtures:** sample lines for each request, reply and event in
  `protocol-fixtures/two-way-handler/`, written by `make fixtures` from the
  real encoder and checked by a test, so the document, the code and a
  handler's tests can all use the same bytes.
- **Config:** `[two_way] command` round-trip; the old-keys warning; missing
  command.
- **Service:** existing two-way and tap tests stay green against
  `testfixtures.TwoWay` (they don't depend on the transport). New: with the
  fake returning `ErrUnavailable`, a post is skipped without a toast, and a
  retirement leaves the message tracked and succeeds on the next sync once the
  fake is available again.
- **End to end, by hand** (after the handler exists): the same live checks as
  before, plus killing the handler process and watching pr-mon restart it.

## Left to the builder

Package name and layout for the runner; how `Receive` waits; log prefix
wording; the fixture file names; whether `hello`'s timeout and the request
timeout are constants or settings (constants are fine).
