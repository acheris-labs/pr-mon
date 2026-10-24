# pr-mon Telegram handler — design

## Intent

pr-mon no longer knows about Telegram or AWS. It runs a **two-way handler**
(`[two_way] command`) and talks to it over handler protocol v1, which is
specified in pr-mon's `docs/two-way-handler.md` (branch
`two-way-notifications`, worktree `~/src/pr-mon-two-way`). That document is
the contract; this one says how the Telegram handler meets it.

`pr-mon-telegram` is that handler: a Go program in this repo that turns
generic pr-mon messages into Telegram messages through the existing
`telegram-sender` Lambda, and turns taps arriving on the `pr-mon` SQS queue
into protocol `tap` events. Everything AWS- and Telegram-shaped lives here, next
to the Lambda and Terraform it depends on.

It is for pr-mon only. The route is fixed at `pr-mon`.

## Goals

- pr-mon with `[two_way] command = "<abs path>/pr-mon-telegram --config <abs path>"`
  posts, edits and deletes Telegram messages, and acts on button taps, exactly
  as the removed built-in Telegram channel did.
- Passes every sample line in pr-mon's `protocol-fixtures/two-way-handler/`.
- Standard library only, plus `github.com/BurntSushi/toml` for the settings file.

## Non-goals

- Other apps or routes. No route setting.
- Changes to the Lambdas, queues or Terraform. They stay as they are.
- The AWS security-audit fixes (separate work).
- Keychain storage, Homebrew packaging, code signing.

## Anti-goals

- Writing a secret anywhere: stdout, stderr or an error message. Config errors
  say where the file is wrong, never what's in it.
- Logging whole Telegram updates (they carry names and user ids). Log the
  `update_id` and why the update was skipped.
- Writing anything but protocol lines to stdout.

## Starting point

The code being replaced is in pr-mon's history at `8ce836d~1`:

| Removed from pr-mon | Becomes |
|:--------------------|:--------|
| `internal/aws/` (SigV4 signing, Lambda invoke, SQS receive/delete, tests) | `internal/aws/`, ported as is |
| `internal/twoway/telegram/` (rendering, error mapping, receive, tests) | `internal/telegram/`, minus the `twoway.Channel` interface |
| `internal/config/credentials.go` (0600 check, safe decode errors) | `internal/config/`, for the new file |

The rendering (status emoji, HTML, 4096 UTF-16 unit limit, `pr-mon:<key>`
callback data of at most 64 bytes) and the Telegram error mapping are kept
exactly; they were reviewed and live-tested.

## Layout

```
cmd/pr-mon-telegram/main.go   flags, config, wire it up, exit codes
internal/config/              the settings file
internal/aws/                 ported client
internal/telegram/            Message → telegram-sender request; Update → tap
internal/protocol/            line reading/writing, request dispatch, the receive loop
testdata/two-way-handler/     copies of pr-mon's fixtures
Makefile                      test, lint, install
```

Module path `github.com/acheris-labs/telegram-webhook` *(my call; there is no
remote yet)*. Go 1.27 (installed). The Python and Terraform stay where they
are.

## Settings file

Passed as `--config <path>`; required. TOML, read with
`github.com/BurntSushi/toml` (the user's decision; the same library pr-mon
uses, and the only dependency).

```toml
region = "us-east-1"
queue_url = "https://sqs.us-east-1.amazonaws.com/<acct>/telegram-webhook-pr-mon.fifo"
sender_function = "telegram-sender"
chat_id = ""

[aws]
access_key_id = "AKIA..."
secret_access_key = "..."
```

- `chat_id` is optional; empty means telegram-sender's default chat.
- Every other field is required. Unknown keys are an error (catches typos;
  `toml.MetaData.Undecoded`).
- TOML decode errors quote the offending text, which may be the secret, so a
  decode error reports the line number only (as pr-mon's removed
  `decodeError` did).
- The file must not be readable or writable by group or others (refuse
  otherwise, saying `chmod 600`).
- Suggested home: `~/.config/pr-mon-telegram/config.toml`. Under launchd there
  is no shell environment, so the path in `[two_way] command` is absolute.

A bad or missing file: one line on stderr saying what's wrong (path and
reason, no contents), exit 1. pr-mon shows that line in its warning and retries
with backoff, which is fine.

## Protocol behaviour

| Request | Does | Reply |
|:--------|:-----|:------|
| `hello` | Nothing else | `{"protocol": 1, "name": "Telegram"}`, whatever version pr-mon sent |
| `post` | telegram-sender with rendered text and keyboard | `{"message_id": "<Telegram id as decimal string>"}` |
| `edit` | telegram-sender with `message_id` | `{}`; "message is not modified" is success; "message to edit not found" → `gone` |
| `delete` | telegram-sender `delete: true` | `{}`; "message can't be deleted" → `too_old`; "message to delete not found" → `gone` |
| `ack` | SQS `DeleteMessage` | `{}`; `ReceiptHandleIsInvalid` (unknown or expired receipt) is success |

- A `message_id` that isn't a decimal integer is one this handler never issued:
  `gone`.
- Anything else that fails is `failed` with a message that contains no secret.
- An unknown `op`, or any request before `hello` has been answered: `failed`.
- Requests run concurrently (one goroutine each); stdout writes are serialised
  so lines never interleave. stdout is flushed after every line.
- A line that isn't a JSON object, or is over 1 MiB: logged to stderr, skipped.
- Message ids are Telegram's, so they stay valid across restarts with nothing
  stored.

### Taps

- The receive loop starts only after the `hello` reply is written (protocol:
  no events before it).
- Long poll: `ReceiveMessage`, 20 s wait, up to 10 messages.
- An update that is a `callback_query` with a message and `pr-mon:` data
  becomes `{"event":"tap","args":{"message_id","key","receipt"}}`; the key is
  the data after `pr-mon:` and the receipt is the SQS receipt handle.
- Anything else is deleted from the queue and logged by `update_id` only.
- Receive errors: one stderr line, then retry after a pause doubling from 1 s to
  1 min, reset by a successful receive. The handler doesn't exit for them;
  pr-mon stays ready and posts still work.
- A tap not acked within the queue's visibility timeout (30 s, the SQS
  default) is received again with a new receipt and sent as a new tap event.
  The protocol allows repeats and pr-mon copes; no dedupe here.

### Timeouts and shutdown

- AWS HTTP calls time out after 25 s for Lambda and SQS delete, and 30 s for
  the 20 s long poll *(my call: under pr-mon's 30 s request timeout, so the
  handler answers `failed` itself rather than pr-mon timing it out)*.
- On stdin EOF: cancel the receive loop and in-flight calls, exit 0 within a
  second or two (pr-mon kills after 5 s).
- Write errors on stdout (pr-mon gone): exit.

## Tests

Standard `testing`, no network:

- Ported `aws` and `telegram` tests (fake AWS via `httptest`).
- Config: valid file, missing field, unknown field, group-readable, bad TOML,
  and that no error text contains the secret.
- Protocol, driven through in-memory pipes with a fake AWS: every request
  fixture decodes and gets the right reply shape; reply and event lines match
  the fixtures' shapes; concurrent requests answered by id; no tap before
  `hello`; unknown op; malformed line skipped; ack of an unknown receipt is
  `ok`; EOF exits.
- `make test` (race detector) and `make lint` (gofmt, go vet) clean.

## Install and hand-over

- `make install` builds to `~/.local/bin/pr-mon-telegram`.
- README gains a "pr-mon handler" section: build, settings file, the
  `[two_way] command` line, and how to fill the file from
  `terraform output`.
- **Not part of the build:** moving the live setup over (writing the settings
  file from the current `~/.config/pr-mon/credentials.toml` and `[two_way]`
  values, pointing pr-mon at the handler, installing a pr-mon that speaks the
  handler protocol). It happens after the build and the end-to-end check, as a
  script written then, reviewed and run by the user, since it handles
  credentials. The build's plan has no task that touches `~/.config/pr-mon` or
  `/Applications`.
- **End-to-end check** (after the build, with the user present to tap): the
  isolated pr-mon test copy in the controlling session's scratchpad runs the
  `two-way-notifications` build against this handler. The live PrMon also
  long-polls the `pr-mon` queue and would compete for taps, so it is quit for
  the duration of the check (the user's decision) and reopened after.

## Secrets

Only `secret_access_key` is a secret *(my call)*. The access key id is an
identifier AWS itself puts in error bodies and CloudTrail; AWS error text may
pass through into `failed` messages and stderr unscrubbed, as it did in pr-mon.
Tests check the secret key never appears in any config or AWS error text the
handler produces.

## Left to the builder

Internal package boundaries beyond the layout above, names, test structure,
how the dispatcher tracks the hello state.
