# pr-mon Two-Way Notifications — Design

Date: 2026-10-22

## Purpose

Let the user act on PRs from their phone. When a PR changes state, pr-mon
sends a chat message with buttons (Merge, Re-run failed checks, Dismiss, …);
tapping one makes pr-mon do it. The chat doubles as a **to-do list**: a message
disappears once its work is done, so whatever is left in the chat is what
still needs attention.

Telegram is the first chat service. The user may switch to Slack later, so
pr-mon talks to "a two-way channel" through a generic interface, and Telegram
is one implementation of it.

## Background: what exists already

All of this is deployed and tested end to end (fake taps with curl, and real
taps from the user's phone):

| Piece | Where | Does |
|:------|:------|:-----|
| `telegram-sender` Lambda | `~/src/telegram-sender` | Sends a message (`message`, optional `parse_mode`, `reply_markup`, `destination`), or edits one (`message_id`). Holds the bot token (in SSM). |
| `telegram-webhook` Lambda + Function URL | `~/src/telegram-webhook` | Receives taps from Telegram. Checks the secret header and the user's Telegram id, routes by the `callback_data` prefix up to the first `:`, queues the raw Telegram update, and answers the tap ("Sent to pr-mon"). |
| SQS FIFO queue `telegram-webhook-pr-mon.fifo` | same | Holds pr-mon's taps; unread taps expire after 10 minutes. |
| IAM user `telegram-webhook-pr-mon-reader` | same | Can receive and delete on that queue only. |

The Mac cannot reach `api.telegram.org` (a network filter), only AWS. That is
why everything goes through Lambdas and SQS.

## Goals

- A per-repo **Two-way** switch next to Script and Desktop in notification
  settings. The repo's existing event choices (and drafts setting) decide
  *when*; the switch decides whether the two-way channel is used.
- Buttons per event (see *Buttons*), mirroring what the dashboard offers.
- Taps run through the same code as the dashboard (`Monitor.Perform`).
- Messages are removed when their work is done, superseded, or the PR closes.
- A generic two-way channel interface in Go; Telegram behind it.

## Non-goals

- A Slack implementation (only keep the interface free of Telegram types).
- Editing the two-way setup (queue, region, …) in the dashboard or Mac app;
  it lives in the config file.
- Typed commands to the bot; only button taps.
- More than one two-way channel at a time.
- Per-user customization of buttons or message layout. One-way channels keep
  their message template; the two-way layout is fixed.

## Anti-goals (failure even if it "works")

- The Lambdas learn anything about PRs. They stay dumb pipes.
- pr-mon runs an action that is not on offer right now (stale button, PR
  changed since the message was sent).
- One stray tap merges past branch protection.
- AWS secrets travel over the socket to the dashboard or Mac app.

## Constraints

- No new Go dependencies. AWS request signing (SigV4) is written with the
  standard library (`crypto/hmac`, `crypto/sha256`); AWS calls are plain HTTPS.
- Wire changes follow `CLAUDE.md`: `docs/protocol.md`, `make fixtures`, the
  Swift models. This design only *adds* fields, so `protocol.Version` stays.
- Backend owns the rules. Which buttons a message gets comes from
  `internal/actions`; clients only show the per-repo switch.
- `make test` (race detector) and `make lint` stay clean.

## Decisions made with the user

| Topic | Decision |
|:------|:---------|
| Channels | Formalized as two kinds: **one-way** (script, desktop: send text) and **two-way** (send with buttons, receive taps, act, update the message). |
| Enabling | Per repo, like Desktop. |
| When | The repo's existing event selection, drafts setting and per-PR mute. |
| New state change for a PR | A **new message** (so the phone notifies), and the PR's previous message is deleted. At most one message per PR. |
| Successful tap | Deletes the message. |
| Failed action | Message stays, showing the error, with fresh buttons. |
| Dismiss | Deletes the message. Nothing else changes (not a mute). |
| PR closes or merges by any route | Its message is deleted. |
| PR's status changes without a new message | Its message is deleted. |
| Buttons | The per-event table below is a deliberate subset, not everything the dashboard offers. |
| Merged event | Not sent over two-way (the PR is closed, so it would be cleaned up at once). One-way channels still announce it. |
| Events with no action buttons | Sent with only **Dismiss**. |
| Merge method | Buttons mirror the dashboard: one allowed method → one button named for it; several → one button per method. Delete-branch uses the dashboard's default (on). |
| Force merge | Only when the backend offers it; asks for confirmation first. |
| Setup | Config file only. |
| Secrets | A separate credentials file the backend alone reads; never sent over the socket. |
| Lambdas | As dumb as possible. pr-mon decides what buttons say and what each tap means. |

## Buttons

Built from `actions.Available(repo, pr, armed)` at send time. A button appears
only if its option is `Available`.

**This table is a deliberate subset, decided with the user.** Other actions the
dashboard offers (Force merge on BLOCKED, Update branch on BEHIND, Merge on a
NEW PR that's already mergeable) are left out on purpose.

| Event | Buttons |
|:------|:--------|
| `READY` | Merge (one per method, see above), Dismiss |
| `FAILING` | Re-run failed checks (`rerun_checks`), Force merge (`force_merge`, confirm first), Dismiss |
| `MERGED` | not sent |
| every other event in `notify.EventLabels` (`NEW`, `CONFLICT`, `BLOCKED`, `BEHIND`, `PENDING`, `MERGE_FAILED`) | Dismiss |

Every message also has an **Open** button that links to the PR (a URL button;
Telegram opens it directly, no tap is sent). *My call:* a URL button rather
than a link in the text, so Telegram shows no link preview.

## Architecture

```
            ┌──────────────────────── pr-mon backend ─────────────────────────┐
 PR state → │ notify.Select ─▶ one-way channels (script, desktop: unchanged)  │
 changes    │       │                                                         │
            │       └────────▶ two-way manager ──▶ TwoWay interface           │
            │                  (buttons, keys,       │                        │
            │                   message state,       └─ telegram impl ────────┼─▶ AWS
            │                   tap handling) ◀── taps ─┘  (SigV4, Lambda, SQS)│
            │                       │                                         │
            │                       └─▶ Monitor.Perform (same as dashboard)   │
            └─────────────────────────────────────────────────────────────────┘
```

### The TwoWay interface

Generic: no Telegram (or Slack) types or formatting leak through. A sketch;
names and exact shapes are the builder's call as long as the boundary holds:

```go
type Button struct {
    Label string
    Key   string // opaque reply sent back on tap ("" with URL set means a link button)
    URL   string
}

type Message struct {
    // Structured content; the implementation decides the markup.
    Title   string     // e.g. "acme/api #123 · Fix login redirect"
    Status  string     // e.g. "READY", plus its emoji, chosen by the implementation or a small shared table
    Lines   []string   // e.g. author, branch, reason, an error after a failed tap
    Buttons [][]Button // rows
}

type Tap struct {
    MessageID string // the message whose button was tapped
    Key       string // the Button.Key
}

type TwoWay interface {
    Name() string // "Telegram", shown in clients
    Post(ctx context.Context, message Message) (messageID string, err error)
    Edit(ctx context.Context, messageID string, message Message) error
    Delete(ctx context.Context, messageID string) error // ErrTooOld if the service refuses
    // Receive waits up to ~20 s for taps. Each tap must be acknowledged once
    // handled, so a crash mid-handling lets it be redelivered.
    Receive(ctx context.Context) ([]Tap, func(Tap) error, error)
}
```

A fake implementation goes in `internal/testfixtures` for service tests.

### Telegram implementation (over the AWS relay)

- **Post / Edit:** invoke `telegram-sender` with `message` (rendered as HTML,
  with `parse_mode: "HTML"`, text escaped), `reply_markup` (inline keyboard;
  each key button's `callback_data` is `"<route>:<key>"`), `destination` when
  `chat_id` is configured, and `message_id` for an edit.
- **Delete:** invoke `telegram-sender` with a new delete form (see *AWS side*).
  Telegram only lets a bot delete messages less than 48 hours old; when it
  refuses, return `ErrTooOld`.
- **Receive:** SQS `ReceiveMessage` (JSON protocol: `X-Amz-Target:
  AmazonSQS.ReceiveMessage`, `Content-Type: application/x-amz-json-1.0`),
  `WaitTimeSeconds` 20, up to 10 messages. Each body is a Telegram `Update`;
  the tap is `callback_query.data` with the `"<route>:"` prefix removed, and
  `callback_query.message.message_id`. Acknowledging = `DeleteMessage`.
  Bodies that don't parse, or lack the prefix, are logged and acknowledged.
- **Lambda invoke:** `POST https://lambda.<region>.amazonaws.com/2015-03-31/functions/<name>/invocations`,
  SigV4-signed (service `lambda`). A function error comes back as HTTP 200 with
  an `X-Amz-Function-Error` header and `errorMessage` in the body; treat that
  as an error.
- **SigV4:** standard library only. Tested against AWS's published signing
  examples.
- Implementation-specific limits (Telegram: 64-byte `callback_data`, 4096-char
  text) are handled inside the implementation. Keys must be short enough that
  `"<route>:<key>"` fits in 64 bytes.

### Two-way manager (backend)

Owns everything PR-specific. It lives in the service layer (or a package it
uses), not in the Telegram implementation.

**Sending.** Where `refreshRepo` today calls `m.send(...)` for each selected
notification, also, when the repo's `two_way_enabled` is on and a two-way
channel is configured and the event is not `MERGED`:

1. Build the buttons (table above), giving each key button a fresh random key.
2. Delete the PR's previous message, if any (fallback below).
3. Post the new message; record it.

Runs in the background like `m.send`; failures are logged and toasted the same
way ("Telegram notification failed: …").

`MERGE_FAILED` is sent from `notifyResult`, not the polling path, so two-way
hooks in there too.

**Tap handling.** A receive loop runs while a two-way channel is configured.
For each tap:

| Situation | Response |
|:----------|:---------|
| Key unknown (stale: superseded, already used, or from before a reset) | If the message is still tracked, re-render it with current buttons and the line "That button had expired". If not tracked, remove its buttons and add "No longer tracked" (*my call:* not deleted, in case another backend owns it). |
| Dismiss | Delete the message; forget it. |
| Force merge | Edit the message to "Force merge #N?" with **Confirm** and **Cancel** (fresh keys). Cancel re-renders the original with fresh keys. Confirm proceeds as an action. |
| Action key | See *Acting on a tap*. Success: delete the message. Failure: re-render with the error line and fresh buttons. |

**Acting on a tap** (*my call*, to meet the "PR changed since the message"
anti-goal):

1. Fetch the PR fresh from GitHub (one request; the cached copy can be up to
   `poll_interval` old).
2. Its head commit must be the one recorded when the message was sent. If it
   moved, re-render with "The PR changed since this message" and current
   buttons.
3. Re-check `actions.Available` on the fresh data. If the option is gone,
   re-render with the reason and current buttons.
4. Run it. Merges and force merges pass the recorded head as
   `expectedHeadOid`, so GitHub refuses if a commit lands in between. Today
   `runAction` passes `""` for manual merges; the tap path needs the pinned
   variant. `Monitor.Perform` reports GitHub failures as toasts and returns
   nil, so the tap path needs a way to learn the outcome (a refactor, the
   builder's call how).

No separate age check on taps: the queue already drops taps after 10 minutes,
and steps 1–2 catch anything that changed.

Every re-render issues fresh keys and retires the old ones (single-use keys).
The tap is acknowledged after handling, whatever the outcome.

**Status moved on, no new message (decided with the user).** A message is
about the PR's status when it was sent; record that status. Whenever the
manager sees a tracked PR whose current status differs from the recorded one,
and no new message is being sent for it, it deletes the message. This covers
events the repo hasn't selected, drafts with drafts off, statuses that have no
event (`EARLY_FAIL`, `WAITING`, `DRAFT`), auto-retry turning FAILING into
PENDING, and changes while the backend was down. The comparison must therefore
run on every refresh, including the first load after a restart (which never
notifies), not only on what `notify.Select` returns.

**Cleanup.** When a PR leaves the open list (closed or merged by any route),
delete its message, using the same "is it really gone" test as `checkArmed`
(a PR beyond the newest-N cut-off is not proof it closed). *My call:* when a
repo is removed, delete its messages too; when a repo's switch is turned off,
or a PR is muted, existing messages stay.

**Delete fallback.** When `Delete` returns `ErrTooOld`, edit the message
instead: no buttons, and a line saying why it's done ("✅ Merged", "Dismissed",
"PR closed"). The message is then forgotten. A message the user already
deleted by hand ("message not found" from the service) counts as done.

**State.** A new `messages` section in `state.json` (read on its own like the
other sections, so a bad section doesn't lose the rest), recording per message:
repo, PR number, event, the PR's status and head commit when sent, sent time,
and its live keys → (action kind, merge method, delete-branch). Saved whenever it changes, so a restart keeps buttons
working. *My call:* keys are 8 random base-32 characters.

**Test notification.** "Send test" also posts a sample message with a single
Dismiss button when the repo's switch is on.

**Startup and errors.** The two-way channel starts when the config has a
`two_way` section and the credentials load. Problems (missing credentials,
credentials file readable by others, AWS errors) become a status warning, as
existing backend problems do, and are logged; the rest of pr-mon runs as
normal. Receive errors back off and retry.

## Configuration

`~/.config/pr-mon/config.toml` gains one optional table. *My call* on names:

```toml
[two_way]
kind = "telegram"                     # which implementation
region = "us-east-1"
queue_url = "https://sqs.us-east-1.amazonaws.com/<account-id>/telegram-webhook-pr-mon.fifo"
sender_function = "telegram-sender"
route = "pr-mon"                      # callback_data prefix; must match the webhook's route
chat_id = ""                          # optional; empty means the sender's default chat
```

and each repo's notification table gains `two_way_enabled = true|false`.

**`config.Save` rewrites the whole file from the struct**, so the `[two_way]`
table must be parsed into `Config` and written back by `Save`, or the first
settings save from the dashboard would delete it. It is backend-only: it is not
part of the protocol's `Config` object (`json:"-"` or equivalent).

`~/.config/pr-mon/credentials.toml` (new, mode 0600, backend-only):

```toml
[aws]
access_key_id = "…"
secret_access_key = "…"
```

One section per provider, so another relay (Google, …) can add its own later.

## Protocol and clients

Additions only; `protocol.Version` unchanged.

| Change | Where |
|:-------|:------|
| `NotifyConfig.two_way_enabled` (bool) | `internal/config`, `Equal`, `Save`, `docs/protocol.md`, fixtures, `Models.swift` |
| `Hello.two_way` and `Status.two_way`: string or null, the configured channel's display name ("Telegram") | `docs/protocol.md`, fixtures, `Models.swift` |
| Per-repo **Two-way** switch in notification settings; when `two_way` is null it is shown disabled with the hint "No two-way channel set up ([two_way] in config.toml)" | TUI notification settings (`internal/tui`), Mac app `SettingsView.swift` |
| The "Nothing to send: enable Script or Desktop" warning also counts two-way | `internal/service` |

## AWS side

| Change | Repo |
|:-------|:-----|
| A delete form for `telegram-sender`: `{"message_id": N, "delete": true}` → `deleteMessage`. Returns `{"ok": true}`; Telegram's refusal for old messages surfaces as an error whose text says so, so pr-mon can map it to `ErrTooOld`. Tests, README, deploy. | `~/src/telegram-sender` |
| Grant each route's reader `lambda:InvokeFunction` on `telegram-sender` (variable for the function name, default `telegram-sender`), so pr-mon needs one key. | `~/src/telegram-webhook` |
| Write `credentials.toml` from `terraform output` and the `[two_way]` table into the config. | setup step, done once |

## Testing

- Unit: SigV4 (published AWS examples), the Telegram implementation against an
  `httptest` server standing in for Lambda and SQS, the manager against the
  fake `TwoWay` (send, supersede, each tap row above, confirm/cancel, stale
  keys, head moved, status moved on without a new message, cleanup on close,
  `ErrTooOld` fallback, restart from saved state),
  config round-trip of `[two_way]` and `two_way_enabled`, credentials loading.
- Protocol: `spec_test.go` and fixtures cover the new fields; Swift tests
  decode the updated fixtures.
- End to end, by hand: a real message on the phone; each button; a stale
  button; a PR closed on GitHub.

## Left to the builder

Package layout and names; the exact message layout and emoji; the key format
(within the 64-byte limit); retry and backoff details; how the TUI and Mac app
lay out the switch, as long as each matches its surroundings.
