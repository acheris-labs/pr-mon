# pr-mon backend protocol

Protocol version: **1**

The pr-mon backend (`pr-mon daemon`) does all the work: polling GitHub, working
out readiness, merging, and sending notifications. Clients (the TUI, a menu bar
app, a widget) only display state and send commands. This document is enough to
write a client in any language.

`tests/test_protocol_spec.py` checks the tables below against the code, so they
can't drift.

## Transport

- A Unix stream socket. The client connects; the backend never dials out.
- Location:
  1. `$XDG_STATE_HOME/pr-mon/daemon.sock`, or `~/.local/state/pr-mon/daemon.sock`
     when `XDG_STATE_HOME` is unset.
  2. If that path is longer than 100 bytes, the socket is instead
     `/tmp/pr-mon-<uid>/<hash>.sock`, where `<hash>` is the first 16 hex digits
     of the SHA-256 of the state directory path (`…/pr-mon`).
- If nothing is listening, start the backend with `pr-mon start` (or enable
  `pr-mon autostart`). Only one backend runs at a time.
- Any number of clients may be connected at once.

## Framing

- Each message is one JSON object encoded as UTF-8, followed by `\n`.
- Messages can be large (a snapshot of busy repos is several MB); don't cap line length low.
- Three message shapes:

| Shape | Direction | Form |
|:------|:----------|:-----|
| Request | client → backend | `{"id": 7, "op": "mark_seen", "args": {...}}` |
| Response | backend → client | `{"id": 7, "ok": true, "result": ...}` or `{"id": 7, "ok": false, "error": "text for the user"}` |
| Event | backend → client | `{"event": "repo", "data": {...}}` |

- `id` is chosen by the client and echoed back; use a unique integer per request.
- Requests are handled concurrently, so responses may arrive out of order.
- Events and responses are interleaved on the same connection. A message with an
  `event` key is an event; anything else is a response.
- Unknown fields must be ignored, so the backend can add fields without breaking clients.

## Connecting

1. Send `hello`. If `result.protocol` isn't the version you implement, stop:
   the backend is newer or older than you. A backend without a `protocol` field
   predates versioning; treat it as version 0.
2. Send `snapshot`. The result is the full state, and from then on this
   connection receives events. No event is lost or sent before the snapshot
   response.
3. Apply events as they arrive. When the connection closes, the backend has
   stopped; reconnect and start again from step 1.

## Requests

| Op | Args | Result | Notes |
|:---|:-----|:-------|:------|
| `hello` | none | `Hello` | Safe to call without subscribing. |
| `snapshot` | none | `Snapshot` | Subscribes this connection to events. |
| `refresh_all` | none | `null` | Starts a poll in the background. |
| `add_repo` | `name`: `"owner/repo"` | the repo name as GitHub spells it | Error if it's missing or already monitored. |
| `remove_repo` | `name` | `null` | Also drops its notification settings and armed merges. |
| `mark_seen` | `name`, `number` | `null` | Clears the PR's "new" marker. |
| `set_collapsed` | `owner`, `collapsed`: bool | `null` | `owner` is lowercase (the tree grouping key). |
| `perform` | `repo`, `number`, `action`: `Action` | `null` | GitHub failures are reported as `toast` events, not errors. |
| `save_notifications` | `repo`, `settings`: `NotifyConfig` | `null` | |
| `send_test` | `repo`, `settings`: `NotifyConfig` | `null` | Sends a sample notification with these unsaved settings. |
| `shutdown` | none | `null` | Stops the backend. |

Errors (`ok: false`) carry a message meant for the user, for example
`"acme/api#12 is not an open PR"`.

## Events

| Event | Data | Meaning |
|:------|:-----|:--------|
| `repos` | `Snapshot` | The repo list changed; replace all state. |
| `repo` | `name`, `repo`: `Repo` or null, `error`: string or null, `unseen`: [int], `armed`: {number: `ArmedMerge`} | One repo was refreshed. `repo` is null until it has loaded. |
| `seen` | `name`, `unseen`: [int] | New-PR markers changed. |
| `collapsed` | `collapsed`: [string] | Collapsed owner groups changed. |
| `config` | `config`: `Config` | Settings changed. |
| `status` | `status`: `Status` | Backend status changed (last update, rate limit, warnings). |
| `toast` | `message`, `severity`: `information`, `warning` or `error` | Show a transient message. |

## Objects

Timestamps are ISO-8601 UTC strings. JSON object keys are always strings, so
PR numbers used as keys (`armed`) are strings like `"12"`.

### Object: `Hello`

| Field | Type | Meaning |
|:------|:-----|:--------|
| `protocol` | int | Protocol version (this document). |
| `version` | string | pr-mon package version. |
| `pid` | int | Backend process id. |
| `notifier` | string or null | Desktop notifier command found, if any. |
| `merging` | bool | A merge is in flight; don't restart the backend now. |

### Object: `Snapshot`

| Field | Type | Meaning |
|:------|:-----|:--------|
| `config` | `Config` | |
| `repos` | {name: `Repo`} | Loaded repos only. |
| `errors` | {name: string} | Last fetch error per repo. |
| `unseen` | {name: [int]} | PR numbers not yet seen, per configured repo. |
| `collapsed` | [string] | Collapsed owner groups. |
| `armed` | {name: {number: `ArmedMerge`}} | PRs pr-mon will merge when ready. |
| `status` | `Status` | |

### Object: `Status`

| Field | Type | Meaning |
|:------|:-----|:--------|
| `connected` | bool | Always true from the backend; clients use it for their own state. |
| `pid` | int or null | |
| `version` | string | pr-mon package version. |
| `notifier` | string or null | |
| `last_update` | timestamp or null | Last successful poll. |
| `rate_limited_until` | timestamp or null | Polling paused until then. |
| `warnings` | [string] | Problems to show the user (e.g. an unreadable config). |

### Object: `Config`

| Field | Type | Meaning |
|:------|:-----|:--------|
| `repos` | [string] | Monitored repos, in display order. |
| `poll_interval` | int | Seconds between polls. |
| `notifications` | {name: `NotifyConfig`} | Repos without an entry never notify. |

### Object: `NotifyConfig`

| Field | Type | Meaning |
|:------|:-----|:--------|
| `message` | string | Template with `{{PR_*}}` placeholders. |
| `events` | [string] | Any of `READY`, `FAILING`, `CONFLICT`, `BLOCKED`, `BEHIND`, `PENDING`, `NEW`, `MERGED`, `MERGE_FAILED`. |
| `include_drafts` | bool | |
| `script_enabled` | bool | |
| `script` | string | Command run with the message on stdin. |
| `desktop_enabled` | bool | |

### Object: `Repo`

| Field | Type | Meaning |
|:------|:-----|:--------|
| `name` | string | `owner/repo`. |
| `merge_methods` | [`MergeMethod`] | Methods the repo allows; empty means none. |
| `delete_branch_on_merge` | bool | GitHub deletes head branches itself. |
| `prs` | [`PullRequest`] | Newest open PRs, at most 50. |
| `pr_total` | int | All open PRs; larger than `prs` when truncated. |
| `auto_merge_allowed` | bool | GitHub's native auto-merge is enabled for the repo. |

### Object: `PullRequest`

The backend computes the last five fields; clients display them rather than
reimplement the rules.

| Field | Type | Meaning |
|:------|:-----|:--------|
| `id` | string | GitHub node id. |
| `number` | int | |
| `title` | string | |
| `url` | string | |
| `author` | string | Login. |
| `created_at` | timestamp | |
| `is_draft` | bool | |
| `head_ref` | string | Branch name. |
| `base_ref` | string | |
| `head_ref_id` | string or null | Null when the branch is gone. |
| `head_repo` | string or null | `owner/repo` of the head (differs for forks). |
| `mergeable` | string | GitHub's `MERGEABLE`, `CONFLICTING` or `UNKNOWN`. |
| `merge_state` | string | GitHub's `mergeStateStatus`. |
| `review_decision` | string or null | GitHub's `reviewDecision`. |
| `check_state` | string or null | Combined check state. |
| `checks` | [`Check`] | Up to 50 checks. |
| `checks_total` | int | All checks. |
| `auto_merge` | `AutoMerge` or null | GitHub's native auto-merge, if enabled. |
| `last_commit_at` | timestamp or null | |
| `head_sha` | string or null | |
| `status` | string | One of `DRAFT`, `CHECKING`, `CONFLICT`, `FAILING`, `PENDING`, `BEHIND`, `BLOCKED`, `READY`. |
| `reasons` | [`Reason`] | Why it is (or isn't) mergeable, in display order. |
| `strictly_ready` | bool | Safe to merge unattended (what merge when ready waits for). |
| `last_check_started_at` | timestamp or null | |
| `actions` | [`ActionOption`] | The PR's action menu, in display order. |

### Object: `Check`

| Field | Type | Meaning |
|:------|:-----|:--------|
| `name` | string | |
| `state` | string | e.g. `SUCCESS`, `FAILURE`, `PENDING`. |
| `started_at` | timestamp or null | |

### Object: `AutoMerge`

| Field | Type | Meaning |
|:------|:-----|:--------|
| `method` | `MergeMethod` | |
| `enabled_by` | string | Login. |

### Object: `Reason`

| Field | Type | Meaning |
|:------|:-----|:--------|
| `text` | string | Shown to the user. |
| `level` | string | `error`, `warning` or `info`. |

### Object: `ActionOption`

One entry of a PR's action menu. Show every entry; an unavailable one is shown
disabled with its `reason`.

| Field | Type | Meaning |
|:------|:-----|:--------|
| `key` | string | Menu slot: `merge`, `auto_merge` or `update` (the TUI's `m`, `a`, `u`). |
| `kind` | string | The `Action` kind to send when chosen. |
| `label` | string | Text to show. |
| `available` | bool | False: show it disabled. |
| `reason` | string or null | Why it is unavailable. |
| `note` | string or null | Extra detail to show next to the label. |
| `needs_method` | bool | Ask for a `MergeMethod` from the repo's `merge_methods` (skip asking when there's only one). |
| `offers_delete_branch` | bool | Offer deleting the head branch (default on); send the choice as `delete_branch`. |

To act on an entry, send `perform` with
`{"kind": kind, "method": <chosen or null>, "delete_branch": <choice or false>}`.

### Object: `ArmedMerge`

| Field | Type | Meaning |
|:------|:-----|:--------|
| `method` | `MergeMethod` | |
| `delete_branch` | bool | Delete the head branch after merging. |
| `armed_at` | timestamp | |

### Object: `Action`

| Field | Type | Meaning |
|:------|:-----|:--------|
| `kind` | string | See the table below. |
| `method` | `MergeMethod` or null | Required for `merge`, `auto_merge_on`, `arm_merge`. |
| `delete_branch` | bool | For `merge` and `arm_merge`; defaults to false. |

| Action kind | Does |
|:------------|:-----|
| `merge` | Merge now. |
| `update` | Update the branch from its base. |
| `auto_merge_on` | Enable GitHub's native auto-merge. |
| `auto_merge_off` | Disable GitHub's native auto-merge. |
| `arm_merge` | pr-mon merges the PR once it is strictly ready. |
| `disarm_merge` | Cancel `arm_merge`. |

`MergeMethod` is one of `SQUASH`, `MERGE`, `REBASE`.

## Changing the protocol

Bump `PROTOCOL_VERSION` in `src/pr_mon/protocol.py` and the version at the top
of this document whenever a client written against the old document would
break. Adding a field is not such a change (clients ignore unknown fields);
removing or renaming a field, changing a type or meaning, or adding a required
argument is.
