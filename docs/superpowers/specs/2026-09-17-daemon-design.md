# pr-mon Background Daemon — Design

Date: 2026-09-17

## Purpose

Keep monitoring and notifying when the TUI is closed. A long-running daemon
owns polling, change tracking, persisted state, notifications, and GitHub
actions. The TUI becomes a client that can come and go.

## Decisions

> **Revised:** the TUI no longer starts or stops the backend while open (no `S`
> key, no banner). The header shows `● connected` / `○ disconnected`; on
> disconnect the TUI shows a toast and retries connecting every 5 s without
> spawning. It still auto-starts the backend when it opens.

Defaults chosen while writing this spec are marked **(default)** — confirm or
change them during review.

1. The TUI is **always** a client; there is no in-process mode in the shipped
   CLI. (Tests may drive the service directly through the same interface.)
2. The daemon is the only writer of `config.toml` and `state.json`.
3. Transport: Unix domain socket, newline-delimited JSON. Stdlib only.
4. Lifetime: the daemon runs until stopped (`S` in the TUI, `pr-mon stop`, or
   SIGTERM). Quitting the TUI never stops it. No start-at-login.
5. Auto-start: `pr-mon` starts the daemon if the socket doesn't answer.
6. **(default)** If the daemon dies or is stopped while the TUI is open, the
   TUI shows a "backend stopped — press S to start" banner and keeps the last
   data visible (read-only). It does not auto-respawn.
7. **(default)** Stopping from the TUI asks for confirmation.
8. **(default)** Version mismatch (daemon older/newer than the TUI) → the TUI
   offers to restart the daemon; declining quits the TUI.
9. **(default)** Toasts for action results and notification failures are
   broadcast to every connected TUI (single user; multiple windows are rare).

## Files and paths

All under `$XDG_STATE_HOME/pr-mon/` (default `~/.local/state/pr-mon/`):

| File | Purpose |
|------|---------|
| `daemon.sock` | Unix socket |
| `daemon.lock` | `fcntl.flock` lock held for the daemon's lifetime; contains its pid |
| `daemon.log` | Log (stdlib `logging`, `RotatingFileHandler`, 1 MB × 3) |
| `state.json` | Unchanged format |

- If `daemon.sock` would exceed the Unix socket path limit (~104 bytes on
  macOS), the socket is `/tmp/pr-mon-<uid>/<hash of state dir>.sock`; that
  directory must be owned by the user with mode 0700 or the daemon refuses to start.
- A second daemon fails to take the lock and exits with "already running (pid N)".
- A stale socket file (no lock holder) is removed at daemon start.

## Components

| Module | Responsibility |
|--------|----------------|
| `models.py` | Add `repo_to_dict` / `repo_from_dict` (lossless, JSON-safe) |
| `github.py` | `GitHubClient(token_provider)`: on `AuthError`, re-read the token once and retry |
| `service.py` (new) | `Monitor`: Textual-free core — config/state ownership, poll loop, CHECKING retries, rate-limit pause, tracker, first-load gate, notifications, actions, add/remove repo; emits events to listeners |
| `backend.py` (new) | `Backend` protocol (the TUI's view of the service) + `BackendEvent` |
| `protocol.py` (new) | JSON-lines framing, message constructors, (de)serialization of settings/actions |
| `server.py` (new) | `DaemonServer`: accepts clients, maps requests to `Monitor`, broadcasts events |
| `remote.py` (new) | `RemoteBackend`: socket client implementing `Backend` with a local mirror |
| `daemon.py` (new) | `run_daemon()` (lock, logging, signals, server), `spawn_daemon()`, `stop_daemon()`, `daemon_status()` |
| `cli.py` | `pr-mon [tui|daemon|start|stop|status]` via `argparse`; `__main__.py` for `python -m pr_mon` |
| `app.py` | Talks only to a `Backend`; rendering unchanged; backend status + `S` key |

## Backend interface

Read side (always current; for `RemoteBackend` it is a mirror updated by events):

- `config: Config` (repos, poll interval, per-repo notifications)
- `repos: dict[str, RepoInfo]`
- `errors: dict[str, str]`
- `collapsed: set[str]`
- `unseen(name) -> set[int]`
- `status: BackendStatus` — `connected`, `pid`, `version`, `notifier`, `last_update`, `rate_limited_until`, `warnings`

Commands (async; raise `BackendError` with a user-facing message):

- `refresh_all()`
- `add_repo(name) -> str` (canonical name; validates against GitHub, rejects duplicates)
- `remove_repo(name)`
- `mark_seen(name, number)` (the mirror updates immediately; the daemon confirms)
- `set_collapsed(owner_key, collapsed)`
- `perform(repo, number, action: Action)`
- `save_notifications(repo, settings)`
- `send_test(repo, settings)`
- `shutdown()` (stop the daemon)

Events (listener callback, delivered on the TUI's event loop):

| Event | Payload | TUI reaction |
|-------|---------|--------------|
| `repos` | — | rebuild tree (add/remove) |
| `repo` | `name` | relabel; re-render if selected (a new alert under a collapsed owner makes the service un-collapse it and emit `collapsed`) |
| `seen` | `name` | relabel |
| `collapsed` | — | sync tree expand/collapse state |
| `config` | — | nothing visible |
| `status` | — | header subtitle |
| `toast` | `message`, `severity` | `notify()` |
| `disconnected` | — | banner, read-only |

## Protocol

One JSON object per line, UTF-8.

- Request: `{"id": 7, "op": "mark_seen", "args": {"repo": "a/b", "number": 12}}`
- Response: `{"id": 7, "ok": true, "result": ...}` or `{"id": 7, "ok": false, "error": "message"}`
- Event: `{"event": "repo", "data": {...}}`

Ops: `hello` (→ `version`, `pid`, `notifier`), `snapshot` (→ config, repos,
errors, unseen, collapsed, status), plus one op per command above. A client
sends `hello` then `snapshot`; events flow after the snapshot response.

`repo` events carry the full serialized `RepoInfo`, the repo's error (or null),
and its unseen PR numbers, so the mirror never needs a follow-up request.

Unknown ops → error response. Malformed line → the daemon logs it and drops
the connection.

## TUI behavior

- Start: connect → if no answer, `spawn_daemon()` and wait up to 5 s for the
  socket → `hello` (version check) → `snapshot` → render.
- Could not start → exit with the last lines of `daemon.log`.
- Header subtitle: `backend pid 1234 · updated 10:32:01` (or rate-limit text);
  `backend stopped` when disconnected.
- `S`: when connected → confirm → `shutdown`; when disconnected → spawn and reconnect.
- `q` quits the TUI only. Footer shows `S Stop backend` / `S Start backend`.
- While disconnected, commands are disabled with a toast ("backend stopped").

## CLI

| Command | Behavior |
|---------|----------|
| `pr-mon` | TUI (auto-starts the daemon) |
| `pr-mon daemon` | Run the daemon in the foreground (logs to stderr too) |
| `pr-mon start` | Start in the background if not running; print pid |
| `pr-mon stop` | Ask the daemon to shut down; wait up to 5 s; SIGTERM fallback |
| `pr-mon status` | `running (pid N, version V)` or `stopped`; exit code 0/1 |

`gh` auth is checked by the daemon at start; failure is logged and makes
`start` / the TUI report "run `gh auth login`".

## Error handling

| Situation | Behavior |
|-----------|----------|
| Daemon crashes | TUI banner; `S` restarts; traceback in `daemon.log` |
| Socket write to a dead client | client dropped silently |
| Token revoked / re-login | re-read token once per `AuthError`, then report the error per repo |
| Corrupt config/state at daemon start | warnings in `status.warnings`, shown as toasts on connect |
| SIGTERM / `shutdown` | stop polling, cancel running notifications, save state, close clients, remove socket, release lock |

## Testing

| Layer | Approach |
|-------|----------|
| Serialization | round-trip every model field incl. enums/None |
| `Monitor` | FakeClient + fake clock-free polling (`poll_once()`); first-load gate, notifications, actions, add/remove, rate limit, CHECKING retry scheduling |
| Protocol/server/remote | real Unix socket in a temp dir; request/response, events, mirror updates, disconnect |
| Daemon lifecycle | lock contention, stale socket cleanup, SIGTERM cleanup — a subprocess runs a small test script that calls `run_daemon(client=FakeClient(...))` (no test hooks in shipped code) |
| TUI | existing pilot tests driven through a `Backend` backed by `Monitor` (in-process) plus a few end-to-end over the socket |
| Smoke | `pr-mon start/status/stop`; TUI auto-start; close TUI, change arrives, notification fires |
