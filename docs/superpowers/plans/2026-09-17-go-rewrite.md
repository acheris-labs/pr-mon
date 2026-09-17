# pr-mon in Go — one binary (done)

Date: 2026-09-17; completed the same day.

**Goal:** replace the Python backend and TUI with a single Go binary. `pr-mon`
opens the dashboard and starts the backend if needed; `pr-mon daemon` runs the
backend alone. Behaviour, on-disk formats and the socket protocol stay exactly
as they are, so the Mac app keeps working unchanged.

**Why:** packaging. A static ~15MB binary needs no Python, no PyInstaller and
no signing of hundreds of nested libraries, and it can run on Linux too.

**The contract:** `docs/protocol.md` and `protocol-fixtures/`. The Go backend
must produce and consume exactly what the fixtures show, so the Swift client's
tests and the Go tests check the same bytes.

**Look and feel:** the current Python TUI is the specification — same layout
(repo tree, PR list, details), same keys, same status icons and colours, same
wording.

## Layout

```
go.mod                       module github.com/acheris-labs/pr-mon
cmd/pr-mon/main.go           tui | daemon | start | stop | restart | status | autostart
internal/models/             wire types and their JSON forms
internal/readiness/          status, reasons, strict readiness   (backend rules)
internal/actions/            each PR's action menu               (backend rules)
internal/config/             config.toml
internal/state/              state.json
internal/tracker/            change tracking for notifications
internal/notify/             templates, script and desktop delivery
internal/github/             GraphQL client
internal/service/            the monitor: polling, merges, armed merges
internal/protocol/           JSON lines, snapshot and event framing
internal/server/             unix socket server
internal/client/             socket client used by the TUI
internal/daemon/             lock, paths, spawn, stop, logging
internal/autostart/          launchd agent (macOS)
internal/tui/                bubbletea dashboard
```

Python stays in place until the Go binary reaches parity, then is removed in
one commit.

## Decisions

1. **Same files, same formats.** `~/.config/pr-mon/config.toml` and
   `~/.local/state/pr-mon/state.json` keep their current shape, so an existing
   install keeps its repos, seen PRs and armed merges.
2. **Same protocol, version 1.** No wire changes during the port. The fixtures
   are tests, not documentation.
3. **GraphQL without a library:** `net/http` plus `encoding/json`, keeping the
   existing queries and the batching of 10 PRs per request.
4. **Token from `gh auth token`,** re-read once on a 401, as today.
5. **One binary, subcommands as today,** plus the TUI as the default.
6. **Tests ported, not invented.** Each Python test file gets a Go counterpart;
   `protocol-fixtures/` is checked byte for byte.
7. **No behaviour changes.** Anything that seems worth improving gets noted and
   raised, not silently changed.

## New dependencies (need approval)

| Library | For | Alternative |
|---------|-----|-------------|
| `github.com/charmbracelet/bubbletea` + `bubbles` + `lipgloss` | the TUI | hand-rolled ANSI, or `tview` |
| `github.com/BurntSushi/toml` | read and write `config.toml` | hand-written parser (no) |

Everything else is the standard library. Logging rotation is ~40 lines rather
than another dependency.

## Tasks

### Task 1: Module and rules

- [x] `go.mod` (Go 1.27), `make go-test`, `make go-lint` (`gofmt -l`, `go vet`).
- [x] `internal/models`: Status, MergeMethod, Check, Reason, AutoMerge,
      ArmedMerge, ActionOption, PullRequest, Repo, Config, NotifyConfig,
      BackendStatus, Action, with JSON tags matching the wire exactly.
- [x] `internal/readiness`: status, reasons, strict readiness. Port
      `tests/test_models.py`'s Status/Reasons/StrictlyReady cases.
- [x] `internal/actions`: the action menu. Port `tests/test_actions.py`.
- [x] Decode `protocol-fixtures/snapshot-response.json` and check the computed
      values match the fixture's (the Python backend's own output).

### Task 2: Storage, tracking, notifications

- [x] `internal/config`: load and save, unreadable file becomes a warning,
      notifications as `[notifications."owner/repo"]`, non-ASCII preserved.
      Port `tests/test_config.py`.
- [x] `internal/state`: atomic save, invalid entries dropped. Port
      `tests/test_state.py`.
- [x] `internal/tracker`: NEW / READY / BLOCKED changes. Port
      `tests/test_tracker.py`.
- [x] `internal/notify`: `{{VAR}}` rendering in one pass, unknown placeholders,
      event selection, script delivery (no shell, message on stdin, `PR_*` in
      the environment, 30s timeout, process group killed), desktop delivery
      (terminal-notifier / osascript / notify-send), the notification form and
      preview. Port `tests/test_notify.py`.

### Task 3: GitHub client

- [x] `internal/github`: repo query, PR details in batches of 10, merge (with
      `expectedHeadOid`), update branch, enable/disable auto-merge, delete
      branch, token from `gh auth token` with one retry on 401, rate-limit
      errors carrying the reset time, HTML error bodies hidden. Port
      `tests/test_github.py` against an `httptest` server.

### Task 4: The service

- [x] `internal/service`: polling, per-repo refresh, CHECKING retries, rate
      limit pause, first-load notification gate, add/remove repo, mark seen,
      collapsed groups, perform (merge, update, auto-merge on/off, arm, disarm),
      armed merges with the "was modified" retry, notifications for MERGED and
      MERGE_FAILED, poll interval setting. Port `tests/test_service.py`.

### Task 5: Protocol, server, client

- [x] `internal/protocol`: requests, responses, events, snapshot; every fixture
      in `protocol-fixtures/` round-trips byte for byte.
- [x] `internal/server`: unix socket, concurrent requests, subscribe on
      snapshot, broadcast, dead-client cleanup. Port `tests/test_remote.py`'s
      server cases.
- [x] `internal/client`: mirror of backend state, version and protocol check,
      reconnect. Port the remote-backend cases.
- [x] `internal/daemon`: socket path with the `/tmp` fallback, flock,
      spawn detached, stop, status, log file with rotation. Port
      `tests/test_daemon.py`.
- [x] `internal/autostart`: launchd agent, restart through launchctl. Port
      `tests/test_autostart.py`.

### Task 6: The TUI

- [x] `internal/tui`: repo tree grouped by owner with badges and collapse,
      PR table, details pane, footer keys, toasts, reconnect handling.
- [x] Modals: action menu (m/a/u, method choice, delete-branch), add repo,
      confirm remove, notifications editor with live preview.
- [x] Keys as today: A add, D remove, N notifications, r refresh, q and ctrl+c
      quit, enter actions, arrows and tab for navigation.
- [x] Tests drive `Update` with key messages and assert on the model and the
      rendered view, covering what `tests/test_app.py` covers.

### Task 7: CLI and cutover

- [x] `cmd/pr-mon`: tui (default), daemon, start, stop, restart, status,
      autostart enable/disable/status, `--version`. Port `tests/test_cli.py`.
- [x] Run the Go backend against the Swift app and the Python TUI, and the Go
      TUI against the Python backend, before removing anything.
- [x] Live check against real repos: poll, statuses, notifications, and one
      action end to end.
- [x] Remove `src/pr_mon`, `tests/`, `pyproject.toml`, `uv.lock`; move the
      fixture generator to Go (`make fixtures`); update the Makefile, README,
      CLAUDE.md and the packaging plan.

## Risks

| Risk | Handling |
|------|----------|
| Behaviour drifts during the port | The fixtures and ported tests are the check; both backends run side by side until parity |
| The TUI is the biggest unknown | Do it last, against a working backend; keep the Python TUI until it matches |
| GraphQL details (paging, rate limits) are easy to get subtly wrong | Port the existing tests first, then verify live against a busy repo |
| Two backends installed at once during the port | The version check already refuses a mismatched backend |
