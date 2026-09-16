# Notifications Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add configurable PR state-change notifications (script + desktop) with an `N` settings dialog.

**Architecture:** Pure logic in a new `notify.py` (templating, selection, argv building, async process runner); `Tracker.changes()` reports status transitions; `config.py` persists `[notifications]`; `NotificationsScreen` edits settings; `app.py` gates on first load and dispatches in workers.

**Tech Stack:** Python 3.11+, textual, stdlib (`re`, `shlex`, `shutil`, `asyncio.subprocess`), unittest.

**Spec:** `docs/superpowers/specs/2026-09-16-notifications-design.md`

## Global Constraints

- All imports at module level. unittest only. No new dependencies.
- Never run anything through a shell; message to scripts on stdin; `PR_*` env vars.
- Process timeout 30 s. Desktop ladder: terminal-notifier → osascript → notify-send.
- Event names: READY, FAILING, CONFLICT, BLOCKED, BEHIND, PENDING, NEW. Defaults: READY, FAILING, CONFLICT.
- Default message: `{{PR_REPO}}#{{PR_NUM}} is {{PR_STATE}}: {{PR_TITLE}} {{PR_URL}}`.
- A repo's first successful load in a session never notifies.

**Deviation from spec:** instead of changing `Tracker.update`'s return type, add a
read-only `Tracker.changes(repo) -> list[Change]` that the app calls before `update`.

---

### Task 1: `Tracker.changes`

**Files:** Modify `src/pr_mon/tracker.py`; Test `tests/test_tracker.py`.

**Produces:** `@dataclass(frozen=True) Change(number: int, old: Status | None, new: Status)`;
`Tracker.changes(repo: RepoInfo) -> list[Change]` — no mutation; omits CHECKING and unchanged PRs.

- [ ] Tests: new PR → `Change(n, None, s)`; changed → `Change(n, old, new)`; unchanged omitted; CHECKING omitted; READY→CHECKING→READY yields nothing after `update`; does not mutate records.
- [ ] Implement, pass, commit.

### Task 2: `NotifyConfig` in config

**Files:** Modify `src/pr_mon/config.py`; Test `tests/test_config.py`.

**Produces:** `EVENT_NAMES: tuple[str, ...]`; `DEFAULT_MESSAGE`; `@dataclass NotifyConfig(message, events: list[str], include_drafts=False, script_enabled=False, script="", desktop_enabled=False)`; `Config.notifications: NotifyConfig`.

- [ ] Tests: defaults when missing; round-trip with quotes/newlines in message and script; invalid section → default notifications + warning, repos/interval kept; unknown event name invalid.
- [ ] Implement, pass, commit.

### Task 3: `notify.py` pure helpers

**Files:** Create `src/pr_mon/notify.py`; Test `tests/test_notify.py`.

**Produces:**
- `render(template: str, variables: dict[str, str]) -> str`
- `unknown_placeholders(template: str) -> list[str]`
- `pr_variables(repo_name: str, pr: PullRequest, state: str) -> dict[str, str]`
- `SAMPLE_VARIABLES: dict[str, str]`
- `@dataclass(frozen=True) Notification(repo: str, pr: PullRequest, state: str)`
- `select_notifications(repo: RepoInfo, changes: list[Change], settings: NotifyConfig) -> list[Notification]`
- `detect_desktop_notifier() -> str | None`
- `desktop_argv(tool: str, message: str, variables: dict[str, str]) -> list[str]`
- `script_argv(command: str) -> list[str]` (shlex + `~` expansion; raises `ValueError`)

- [ ] Tests per spec's Template / Selection / Desktop rows.
- [ ] Implement, pass, commit.

### Task 4: async runner and dispatch

**Files:** Modify `src/pr_mon/notify.py`; Test `tests/test_notify.py`.

**Produces:**
- `async run_command(argv, stdin: str | None, env_extra: dict[str, str], timeout: float = 30) -> str | None` — returns an error message or None.
- `async deliver(settings: NotifyConfig, notifier: str | None, variables: dict[str, str]) -> list[tuple[str, str | None]]` — `(channel, error)` per enabled channel.

- [ ] Tests with real temp scripts: stdin received; env received; `~` expanded; non-zero exit message includes code + first stderr line; timeout kills; missing command; deliver runs only enabled channels (desktop via a fake notifier on PATH).
- [ ] Implement, pass, commit.

### Task 5: `NotificationsScreen`

**Files:** Modify `src/pr_mon/screens.py`; tests in `tests/test_app.py`.

**Produces:** `NotificationsScreen(settings: NotifyConfig, notifier: str | None, send_test: Callable[[NotifyConfig], None])` → `ModalScreen[NotifyConfig | None]`, tabs Message/Events/Script/Desktop, buttons Send test/Save/Cancel, `ctrl+s`, `esc`.

### Task 6: app wiring

**Files:** Modify `src/pr_mon/app.py`; Test `tests/test_app.py`.

- `N` binding; `self.loaded_repos: set[str]`; `self.notifier = detect_desktop_notifier()` (overridable in tests).
- In `apply`: `changes = tracker.changes(repo)` before `update`; if repo in `loaded_repos`, dispatch `select_notifications(...)`; then add repo to `loaded_repos`.
- Dispatch worker calls `deliver`; toast warnings for errors; Send test toasts success too.

- [ ] Tests per spec's App row using a patched `deliver`.
- [ ] Implement, pass, commit.

### Task 7: docs and verification

- [ ] README section; `make lint test`; live run: open `N`, Send test via terminal-notifier; commit.
