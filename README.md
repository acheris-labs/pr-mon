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
| `a` | action menu | Enable / disable GitHub auto-merge (asks `s`/`m`/`r` if several methods) |
| `u` | action menu | Update branch (when behind base) |
| `space` | action menu | Toggle "delete remote branch" |
| `N` | repo tree (on a repo) | Notification settings for that repo |
| `r` | anywhere | Refresh now |
| `q` | anywhere | Quit |

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

`a` uses GitHub's built-in auto-merge, so the merge happens on GitHub even
when pr-mon isn't running. The repo must have "Allow auto-merge" enabled.
It's offered for PRs that aren't mergeable yet; ready PRs use `m`. The head
branch is only deleted afterwards if the repo auto-deletes head branches.

## Notifications

Notifications are configured **per repo**: select a repo and press `N`.
A repo without settings never notifies. The dialog has four tabs
(`←`/`→` switch tabs, `↑`/`↓` move between options):

- **Message** — template with `{{PR_REPO}}`, `{{PR_NUM}}`, `{{PR_TITLE}}`,
  `{{PR_AUTHOR}}`, `{{PR_BRANCH}}`, `{{PR_TARGET}}`, `{{PR_STATE}}`,
  `{{PR_URL}}`; a live preview uses a sample PR.
- **Events** — notify when a PR *becomes* Ready, Failing, Conflict, Blocked,
  Behind, Pending, or is newly opened (defaults: Ready, Failing, Conflict).
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

## Development

```sh
make install   # uv sync
make test      # unittest
make lint      # ruff check + format check
make fmt       # ruff format + fix
make clean
```
