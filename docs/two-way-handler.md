# Two-way handler protocol

Version **1**.

pr-mon posts a message with buttons to a chat service and acts on the button
taps that come back. It doesn't know the service. A **handler** is a program
that does: pr-mon runs it, sends it generic messages, and gets replies and taps
back. You can write one in any language from this document alone.

Sample lines for every request, reply and event are in
`protocol-fixtures/two-way-handler/`. They are written by `make fixtures` from
pr-mon's own encoder, so they are exactly what pr-mon sends.

## Transport

- pr-mon starts the handler from `[two_way] command`, split into arguments the
  way a notification `script` is. The handler inherits pr-mon's environment and
  finds its own credentials.
- **Under launchd** (macOS, when the backend starts at login or from the app)
  that environment is minimal: a `PATH`, and at most `XDG_CONFIG_HOME`,
  `XDG_STATE_HOME` and `LANG`. Nothing from your shell profile is there. Give
  `command` an absolute path, and read credentials from a file or the
  keychain, not from shell variables.
- **stdin** carries pr-mon to handler lines. **stdout** carries handler to
  pr-mon lines. Each line is one JSON object, UTF-8, ending in `\n`. Write
  nothing else to stdout.
- **Flush stdout after every line.** Most languages buffer a pipe, and pr-mon
  waits for the whole line.
- A line, on stdout or stderr, may be at most 1 MiB. A longer one is logged
  and skipped.
- **stderr** is free text. pr-mon copies each line into its log, and shows the
  last one (cut to 200 characters) in the warning when the handler exits.
- Unknown fields are ignored by both sides. A line that isn't a JSON object, or
  a reply to an id pr-mon isn't waiting on, is logged and skipped. It doesn't
  end the session.

## Lines

| Direction | Shape |
|:----------|:------|
| Request, pr-mon to handler | `{"id": 7, "op": "post", "args": {...}}`. `id` is an integer, unique per session. |
| Reply, handler to pr-mon | `{"id": 7, "ok": true, "result": {...}}` or `{"id": 7, "ok": false, "error": {"code": "gone", "message": "..."}}` |
| Event, handler to pr-mon | `{"event": "tap", "args": {...}}`. No `id`. |

pr-mon may send a request before the last one is answered. Replies may come in
any order; they are matched by `id`. Whether you work on requests one at a time
or together is up to you.

## Operations

| `op` | `args` | `result` | Notes |
|:-----|:-------|:---------|:------|
| `hello` | `protocol`: int, `app`: `"pr-mon"`, `version`: string | `protocol`: int, `name`: string | Always the first request. `name` is shown in the clients, e.g. `"Telegram"`. Send no events before replying. |
| `post` | `message`: Message | `message_id`: string | The id is opaque to pr-mon. |
| `edit` | `message_id`, `message` | `{}` | Replaces the text and buttons. A message without key buttons ends with none. `gone` if it no longer exists. |
| `delete` | `message_id` | `{}` | `too_old` or `gone` as below. |
| `ack` | `receipt`: string | `{}` | The tap with this receipt is handled; you may forget it. Reply `ok` to a receipt you don't know: pr-mon may ack a tap from before you restarted. |

| `event` | `args` | Notes |
|:--------|:-------|:------|
| `tap` | `message_id`, `key`, `receipt`: strings | A key button was pressed. A tap that isn't acked may be sent again (after a restart, say); pr-mon copes with repeats. |

## Message

```json
{"status": "READY",
 "title": "acme/api #12 · Fix login redirect",
 "lines": ["octocat · fix-login → main"],
 "buttons": [[{"label": "Squash & merge", "key": "xx2ev6d7"}],
             [{"label": "Open", "url": "https://github.com/acme/api/pull/12"},
              {"label": "Dismiss", "key": "kofmkufw"}]]}
```

- `status`: a PR status or event name (`READY`, `FAILING`, `CONFLICT`,
  `MERGE_FAILED`, ...), or `""`. Pick any emoji or styling you like.
- `title`, `lines`: plain text. Escape it for your service's markup.
- `buttons`: rows of buttons, possibly none. A button has `label` and exactly
  one of `key` (a tap reports it back) or `url` (opens a link, sends no tap).
- `lines` and `buttons` are always arrays, never `null`.
- Keys are 1 to 16 characters of `a-z` and `0-9` (pr-mon uses 8), so you can
  fit one in a small payload with a prefix of your own.

## Errors

| `code` | Meaning |
|:-------|:--------|
| `gone` | The message no longer exists. |
| `too_old` | The service refuses to delete a message this old. |
| `failed` | Anything else; `message` says what. |

An unknown code counts as `failed`.

## Versioning

`protocol` starts at 1. pr-mon sends the version it speaks in `hello`. If you
can't speak it, reply with the version you do speak. pr-mon uses the handler
only when the reply's `protocol` equals its own; otherwise it stops the handler
and warns, and doesn't retry until it restarts.

Adding an optional field or a new error code isn't a version change.
Removing or renaming a field or op, or changing a meaning, is.

## Lifecycle

- **Start:** pr-mon starts the handler when the backend starts, if `[two_way]`
  has a `command`.
- **Ready:** once `hello` succeeds. Until then, and whenever the handler is
  down, clients show no two-way channel.
- **Message ids** you return must stay valid across your restarts. An id you
  don't know gets `gone`.
- **Timeouts:** 10 seconds for `hello`, 30 for any other request. A request
  that times out counts as `failed`, and a late reply is skipped.
- **Crash or exit:** waiting requests fail at once. pr-mon starts the handler
  again after a pause that doubles from 1 second up to 1 minute, resetting
  once the handler has stayed up for a minute. The same applies if the command
  can't be started, or `hello` times out, fails or has a reply without a
  `protocol`. A protocol mismatch is the exception.
- **Shutdown:** pr-mon closes your stdin, waits up to 5 seconds for you to
  exit, then kills you. Exit when stdin closes.

## Example session

`>` is pr-mon to handler, `<` is handler to pr-mon. pr-mon posts a message,
someone taps Dismiss, and pr-mon deletes the message, then acks the tap. Acting
on a tap comes first; the ack follows whatever the outcome.

```
> {"id":1,"op":"hello","args":{"protocol":1,"app":"pr-mon","version":"1.2.3"}}
< {"id":1,"ok":true,"result":{"protocol":1,"name":"Telegram"}}
> {"id":2,"op":"post","args":{"message":{"status":"READY","title":"acme/api #12 · Fix login redirect","lines":["octocat · fix-login → main"],"buttons":[[{"label":"Squash & merge","key":"xx2ev6d7"}],[{"label":"Open","url":"https://github.com/acme/api/pull/12"},{"label":"Dismiss","key":"kofmkufw"}]]}}}
< {"id":2,"ok":true,"result":{"message_id":"1082"}}
< {"event":"tap","args":{"message_id":"1082","key":"kofmkufw","receipt":"r-7731"}}
> {"id":3,"op":"delete","args":{"message_id":"1082"}}
< {"id":3,"ok":true,"result":{}}
> {"id":4,"op":"ack","args":{"receipt":"r-7731"}}
< {"id":4,"ok":true,"result":{}}
```
