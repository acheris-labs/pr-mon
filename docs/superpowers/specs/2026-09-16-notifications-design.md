# pr-mon Notifications — Design

Date: 2026-09-16

## Purpose

Tell the user when a monitored PR changes state, without them watching the
TUI. The user configures a message template once, picks which state changes
matter, and enables one or both delivery channels: a user script (e.g. `im`)
and a desktop notification.

## Scope

In scope:

- `N` opens a Notifications dialog (tabs: Message, Events, Script, Desktop).
- Message template with `{{VAR}}` placeholders.
- Per-status event selection, plus "new PR" and "include drafts".
- Script channel: command run without a shell, message on stdin, variables in env.
- Desktop channel: `terminal-notifier` → `osascript` → `notify-send` → unavailable.
- "Send test" with the dialog's unsaved settings.
- Settings persisted in `config.toml` under `[notifications]`.

Out of scope (YAGNI):

- Per-repo notification settings.
- Batching several changes into one notification.
- Template logic (loops, conditionals, filters); only placeholder substitution.
- Merged/closed and auto-merge-toggled events.
- Notification sound.

## Settings

> **Revised:** settings are per repo. `N` requires a selected repo; each repo
> has its own `[notifications."owner/repo"]` table with the keys below; a repo
> without a table never notifies; removing a repo removes its table; an old
> shared `[notifications]` section is ignored with a warning.

```toml
[notifications]
message = "{{PR_REPO}}#{{PR_NUM}} is {{PR_STATE}}: {{PR_TITLE}} {{PR_URL}}"
events = ["READY", "FAILING", "CONFLICT"]
include_drafts = false
script_enabled = false
script = ""
desktop_enabled = false
```

- Missing section or keys → defaults above (both channels off).
- Invalid section (wrong types, unknown event names) → notification defaults
  plus a warning toast; the repo list and poll interval still load.
- Valid `events` values: `READY`, `FAILING`, `CONFLICT`, `BLOCKED`, `BEHIND`,
  `PENDING`, `NEW`.
- The config writer emits the section using JSON string escaping (valid TOML
  basic strings), so quotes and newlines in the template round-trip.

## Template

- Syntax: `{{NAME}}`, optional inner whitespace (`{{ NAME }}`).
- Variables:

| Name | Value |
|------|-------|
| `PR_REPO` | `owner/name` |
| `PR_NUM` | PR number |
| `PR_TITLE` | title |
| `PR_AUTHOR` | author login |
| `PR_BRANCH` | head branch |
| `PR_TARGET` | base branch |
| `PR_STATE` | new status (`READY`, `FAILING`, …) or `NEW` for a new PR |
| `PR_URL` | PR URL |

- Unknown placeholders are left verbatim; the Message tab lists them as a warning.
- Substitution is a single regex pass; substituted values are never re-scanned,
  so a PR title containing `{{…}}` stays literal.

## Which changes notify

A notification fires when a PR **enters** a selected status:

- `Tracker.update` additionally reports every status change for the repo as
  `(number, old_status, new_status)`, where `old_status` is `None` for a PR not
  seen before. `CHECKING` never counts as a new status (the stored status is
  kept, as today), so READY → CHECKING → READY does not notify.
- A change notifies if:
  - `old_status is None` and `NEW` is selected (PR_STATE = `NEW`), or
  - `old_status is not None`, `new_status != old_status`, and `new_status` is
    selected.
- Moves between two selected statuses notify (FAILING → CONFLICT notifies if
  CONFLICT is selected).
- Draft PRs never notify unless `include_drafts` is true. (A draft's status is
  `DRAFT`, which is not selectable, so in practice `include_drafts` only affects
  `NEW`.)
- These rules are independent of the dashboard's NEW/READY/BLOCKED attention
  events and unseen badges, which are unchanged.

### First load

- The app keeps an in-memory set of repos that have completed a successful load
  this session.
- A repo's first successful fetch (at startup, after `A`, or after earlier
  fetches failed) never notifies; the repo is added to the set afterwards.
- Later fetches of that repo notify per the rules above.
- Changes that happened while pr-mon was closed update badges only.

## Delivery

Each notification is dispatched in a background worker; the UI never waits.
Every enabled channel is used.

### Script channel

- Enabled when `script_enabled` is true and `script` is non-empty.
- `script` is split with `shlex.split` (quotes respected) and executed directly
  with `asyncio.create_subprocess_exec` — never through a shell.
- A leading `~` in any argument is expanded with `os.path.expanduser`. `$VAR`
  and globs are not expanded.
- stdin: the rendered message (UTF-8), then EOF.
- env: the parent environment plus the eight `PR_*` variables.
- cwd: the user's home directory.
- Timeout 30 s → process killed, warning toast.
- Non-zero exit → warning toast with the exit code and the first line of stderr.
- Command not found / not executable / unparseable (`shlex` error) → warning toast.
- One process per notification; several may run concurrently.

### Desktop channel

- Enabled when `desktop_enabled` is true and a notifier is available.
- Notifier chosen once at startup, first found on `PATH` (`shutil.which`):

| Tool | Invocation (argv, no shell) |
|------|-----------------------------|
| `terminal-notifier` | `-title pr-mon -subtitle <PR_REPO> -message <msg> -open <PR_URL>` |
| `osascript` | `-e 'on run argv' -e 'display notification (item 1 of argv) with title (item 2 of argv)' -e 'end run' <msg> pr-mon` |
| `notify-send` | `pr-mon <msg>` |

- Message text is always passed as data (argv), never interpolated into
  AppleScript or shell source.
- Same timeout and failure toasts as the script channel.
- No notifier found → the Desktop tab shows "No notifier found (install
  terminal-notifier or notify-send)" and the checkbox is disabled.

## Notifications dialog (`N`)

Modal with a `TabbedContent`:

| Tab | Contents |
|-----|----------|
| Message | Template `Input`; variable list; live preview rendered from a sample PR; warning line for unknown placeholders |
| Events | One checkbox per event (Ready, Failing, Conflict, Blocked, Behind, Pending, New PR) and "Include draft PRs" |
| Script | "Run script" checkbox; command `Input`; help: "The message is sent on stdin. PR_* variables are also set in the environment. Runs without a shell; use full paths or ~." |
| Desktop | "Show desktop notification" checkbox; line naming the detected notifier or the "none found" message |

Buttons below the tabs: **Send test**, **Save**, **Cancel**.

- **Send test**: dispatches one notification through every channel enabled in
  the dialog's current (unsaved) settings, using a sample PR
  (`acme/api#123`, "Example change", author `octocat`, `feature → main`,
  state `READY`, `https://github.com/acme/api/pull/123`). Results appear as
  toasts; a success toast per channel.
- **Save** (`ctrl+s`): writes `config.toml`, applies immediately, closes.
- **Cancel** / `esc`: discards changes.
- `N` is a global binding shown in the footer ("Notifications").

## Modules

| Module | Change |
|--------|--------|
| `config.py` | `NotifyConfig` dataclass nested in `Config`; load/validate/save `[notifications]` |
| `tracker.py` | `update` returns `Update(events, changes)`; `Change(number, old, new)` |
| `notify.py` (new) | `render`, `unknown_placeholders`, `pr_variables`, `select_notifications`, `detect_desktop_notifier`, `desktop_argv`, `script_argv`, async `run_command` |
| `screens.py` | `NotificationsScreen` |
| `app.py` | `N` binding, loaded-repo set, dispatch worker, send-test callback |

## Testing

`unittest`.

| Area | Tests |
|------|-------|
| Template | known/unknown placeholders, whitespace, no re-scan of values, unknown list |
| Selection | NEW selected/unselected; enter selected status; unselected status silent; FAILING→CONFLICT; CHECKING blip silent; drafts excluded/included |
| Config | defaults when missing; round-trip incl. quotes/newlines; invalid section → defaults + warning, repos kept |
| Script | argv split + `~` expansion; message on stdin; env vars; non-zero exit message; timeout kill; missing command; shlex error — using small real scripts in a temp dir |
| Desktop | detection ladder with patched `shutil.which`; argv per tool; none found |
| App | first load silent at startup and after `A`; later change notifies; failing-then-succeeding first load silent; `N` opens dialog; Save persists; Cancel discards; Send test uses unsaved settings; desktop checkbox disabled when no notifier |
| Smoke | Send test through `terminal-notifier` on this Mac; through `im` only with the user's go-ahead (it sends a real message) |
