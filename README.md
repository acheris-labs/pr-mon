# pr-mon

Terminal dashboard for watching open GitHub pull requests across repos, seeing
why they can't merge, and merging them when they're ready.

## Requirements

- [uv](https://docs.astral.sh/uv/)
- [GitHub CLI](https://cli.github.com/), logged in (`gh auth login`); pr-mon uses its token

## Install

```sh
make tool-install   # uv tool install; puts `pr-mon` on your PATH
```

Or run from the checkout with `make run`.

## How it runs

A background **backend** does the work: it polls GitHub, tracks what changed,
saves state, sends notifications, and performs merges. The dashboard is just a
window onto it, so notifications keep coming after you close the window.

- `pr-mon` opens the dashboard and starts the backend if it isn't running.
- Quitting the dashboard (`q`) leaves the backend running.
- The backend runs until you stop it (`pr-mon stop`), log out, or reboot.
- The header shows `● connected` or `○ disconnected`. If the backend goes
  away, the dashboard says so, keeps the last data on screen, and reconnects
  on its own once the backend is back.
- Several dashboards can be open at once; they all show the same state.
- Other clients (e.g. a menu bar app) can use the same socket; the protocol is
  documented in [docs/protocol.md](docs/protocol.md).

| Command | What it does |
|---------|--------------|
| `pr-mon` | Open the dashboard (starts the backend if needed) |
| `pr-mon start` | Start the backend in the background |
| `pr-mon stop` | Stop the backend |
| `pr-mon restart` | Restart the backend (through launchd when autostart runs it) |
| `pr-mon status` | Show whether it's running (exit code 0 if running) |
| `pr-mon daemon` | Run the backend in the foreground (for debugging) |
| `pr-mon autostart enable` | macOS: run the backend at every login, starting it now |
| `pr-mon autostart disable` | Stop running it at login (a running backend keeps running) |
| `pr-mon autostart status` | Whether autostart is on (exit code 0 if enabled) |

### Start at login (macOS)

`pr-mon autostart enable` installs a launchd agent
(`~/Library/LaunchAgents/com.acheris-labs.pr-mon.plist`) that runs
`pr-mon daemon` at login. It records your current `PATH` (so the backend can
find `gh`), hands the backend over to launchd, waits for it to come up, and
reports whether GitHub access works. launchd restarts the backend if it
crashes, but not after `pr-mon stop`.

Install with `make tool-install` first, so the agent points at a stable
`~/.local/bin/pr-mon` rather than a checkout. macOS shows a "Background Items
Added" notice the first time. If `im` notifications stop working under
autostart, use Send test in the `N` dialog and approve the Automation prompt.

## Keys

| Key | Where | Action |
|-----|-------|--------|
| `A` | anywhere | Add a repo (`owner/name`) |
| `D` | repo tree | Remove the selected repo |
| `enter` / `space` | owner row | Collapse / expand the group |
| `enter` | repo row | Jump to its PR list |
| `←` / `→` | repo tree | Go to owner / collapse; expand |
| `tab` / `shift+tab` | anywhere | Move between repo tree and PR list |
| `enter` | PR list | Action menu |
| `m` | action menu | Merge (asks `s`/`m`/`r` if the repo allows several methods) |
| `a` | action menu | Toggle auto-merge: GitHub's if the repo allows it, otherwise pr-mon's "merge when ready" (asks `s`/`m`/`r` if several methods) |
| `u` | action menu | Update branch (when behind base) |
| `space` | action menu | Toggle "delete remote branch" |
| `N` | repo tree (on a repo) | Notification settings for that repo |
| `r` | anywhere | Refresh now |
| `q` | anywhere | Quit the dashboard (the backend keeps running) |

## Indicators

- Repo tree: repos are grouped under their owner. `● name (n)` — `n` PRs you
  haven't looked at since they were opened, became ready, or became blocked.
  Green = something is ready, red = something got blocked, yellow = new PRs
  only. `⚠` = the last refresh failed. Owner rows add up their repos, so a
  collapsed group still shows alerts; a new alert expands its group.
  Collapsed groups are remembered.
- Selecting a PR in the PR list marks it seen.
- PR statuses: `READY`, `CONFLICT`, `FAILING`, `BLOCKED`, `BEHIND`, `PENDING`,
  `CHECKING` (GitHub still computing), `DRAFT`. The details pane lists every
  blocking reason. `auto` after a status means GitHub auto-merge is on.

## Auto-merge

`a` toggles auto-merge for PRs that aren't mergeable yet (ready PRs use `m`).

- **Repo allows GitHub auto-merge:** `a` uses it, so GitHub merges even when
  pr-mon isn't running. The list shows `auto`. The head branch is only deleted
  if the repo auto-deletes head branches.
- **Repo doesn't:** `a` arms pr-mon's **merge when ready** (list shows
  `auto*`). The backend merges the PR as your `gh` user once it is strictly
  ready: mergeable, and no check failing, required or optional. New commits
  keep it armed. A delete-branch checkbox is offered. `a` again cancels it; it's
  also cleared when the PR is closed.
  - Merges use GitHub's head check, so a push landing mid-merge is retried on
    the next refresh rather than merged blindly.
  - Any other merge failure cancels it and reports why (toast and the
    "pr-mon auto-merge failed" notification with `{{PR_REASON}}`).
  - It only runs while the backend does; see `pr-mon autostart enable`.

Click a PR's link in the details pane to open it in the browser.

## Notifications

Notifications are configured **per repo**: select a repo and press `N`.
A repo without settings never notifies. The dialog has four tabs
(`←`/`→` switch tabs, `↑`/`↓` move between options):

- **Message** — template with `{{PR_REPO}}`, `{{PR_NUM}}`, `{{PR_TITLE}}`,
  `{{PR_AUTHOR}}`, `{{PR_BRANCH}}`, `{{PR_TARGET}}`, `{{PR_STATE}}`,
  `{{PR_URL}}`, `{{PR_REASON}}` (why a pr-mon merge failed; empty otherwise);
  a live preview uses a sample PR. `{{PR_STATE}}` is the new status, `NEW`,
  `MERGED`, or `MERGE_FAILED`.
- **Events** — notify when a PR *becomes* Ready, Failing, Conflict, Blocked,
  Behind, Pending, or is newly opened, and when pr-mon's merge when ready
  merges a PR or fails to (defaults: Ready, Failing, Conflict, and both merge
  results).
  Drafts are skipped unless "Include draft PRs" is ticked.
- **Script** — a command such as `im --deliver tgram`. It runs without a shell
  (quotes and a leading `~` work; `$VARS` don't), receives the message on
  stdin, and gets the `PR_*` values as environment variables. 30 s timeout.
- **Desktop** — uses the first of `terminal-notifier` (click opens the PR),
  `osascript`, `notify-send` found on `PATH`.

**Send test** fires the enabled channels with a sample PR using the unsaved
settings. A repo's first load after startup (or after adding it) never
notifies; only later changes do. Failures show as warning toasts.

If `terminal-notifier` reports "Notifications are turned off", allow it in
System Settings › Notifications (or `tccutil reset UserNotification
fr.julienxx.oss.terminal-notifier` to be asked again).

## Files

- Config (repos, poll interval, per-repo notifications): `~/.config/pr-mon/config.toml` (respects `XDG_CONFIG_HOME`)

  ```toml
  poll_interval = 60
  repos = [
      "owner/repo",
  ]

  [notifications."owner/repo"]
  message = "{{PR_REPO}}#{{PR_NUM}} is {{PR_STATE}}: {{PR_TITLE}} {{PR_URL}}"
  events = ["READY", "FAILING", "CONFLICT"]
  include_drafts = false
  script_enabled = true
  script = "im --deliver tgram"
  desktop_enabled = false
  ```

- State (seen PRs, collapsed groups): `~/.local/state/pr-mon/state.json` (respects `XDG_STATE_HOME`)
- Backend files, next to the state: `daemon.log` (rotated at 1 MB), `daemon.lock`
  (holds the pid), `daemon.sock`. If that path is too long for a Unix socket,
  the socket lives in `/tmp/pr-mon-<uid>/` instead.

## Troubleshooting

- `pr-mon status` — is the backend up, and which version?
- `~/.local/state/pr-mon/daemon.log` — backend errors, including GitHub auth
  problems (`gh auth login` fixes those; the backend picks up the new token).
- After upgrading pr-mon, the dashboard restarts an older backend by itself
  (after any merge it is in the middle of).

## macOS app

`macos/` holds a native Mac app for the same backend: repositories in a
sidebar, pull requests, and details, with merges and auto-merge from the
toolbar, the Pull Request menu, or a right-click.

Settings (⌘,) has three tabs:

- **General** — appearance (System/Light/Dark), how often the backend checks
  GitHub, opening the app at login, and starting the backend at login.
- **Repositories** — add and remove repos, and edit each one's notifications.
- **Backend** — version, process, socket and log paths, the desktop notifier
  found, and buttons to refresh, restart or stop the backend.

```sh
make app-install   # builds and copies PrMon.app to ~/Applications
```

- Needs Xcode and [XcodeGen](https://github.com/yonaskolb/XcodeGen) (`brew install xcodegen`).
- The app starts the backend with `pr-mon start` through your login shell, so
  install the CLI first (`make tool-install`), or enable `pr-mon autostart`.
- Like the TUI, it's only a window onto the backend: quitting it leaves the
  backend running, and it can run alongside dashboards.
- `macos/PrMonKit` is the Swift client library (`make swift-test`);
  `make xcode` generates `macos/PrMon/PrMon.xcodeproj` for working in Xcode.

## Development

```sh
make install   # uv sync
make test      # unittest
make lint      # ruff check + format check
make fmt       # ruff format + fix
make clean
make fixtures     # regenerate protocol-fixtures/ after a wire change
make swift-test   # PrMonKit tests (decodes protocol-fixtures/)
make app          # build the Mac app into macos/build
```

The backend protocol is documented in [docs/protocol.md](docs/protocol.md).
