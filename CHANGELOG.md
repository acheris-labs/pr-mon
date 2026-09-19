# Changelog

All notable changes to pr-mon, newest first. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the versions are
the git tags the release workflow builds from.

## [0.2.0-rc9]

### Added

- PR dependencies: a PR can wait for any number of other PRs to merge first,
  in any repo, including ones pr-mon doesn't monitor. Until they have merged it
  shows `WAITING` (or `BLOCKED` if one closed without merging), can't be merged,
  and pr-mon's merge when ready holds it; after the last one merges, an armed
  PR merges on its own. A waiting PR with GitHub's auto-merge on moves to
  pr-mon's, since GitHub wouldn't wait.
- TUI: `w` (or `w` in the action menu) lists what a PR waits on; `a` adds one
  from a search of every monitored PR, or `owner/repo#12`, or a URL; `d`
  removes; `g` shows the whole graph. The details pane lists "Waits on" and
  "Required by".
- App: "Wait for Another Pull Request…" (⌥⌘W) and "Show Dependency Graph…"
  (⌥⌘G) in the Pull Request menu, the right-click menu and the toolbar;
  "Waits On" and "Required By" sections in the details.
- Protocol: `add_dependency`, `remove_dependency` and `dependency_graph` ops,
  `waits_on` and `required_by` on each PR, and the `WAITING` status.

## [0.2.0-rc8]

### Changed

- Closing the app's window (Command-W) leaves the app running, like Mail:
  the Dock badge keeps counting, and clicking the Dock icon brings the window
  back. Command-Q quits.

## [0.2.0-rc7]

### Fixed

- A backend started by the app opened from the Dock could not find `gh` and
  exited at once: apps inherit launchd's bare PATH, without Homebrew. The
  backend now adds `/opt/homebrew/bin` and `/usr/local/bin` to its own PATH,
  which also covers the desktop notifier and notification scripts.
- When the backend won't start, the app shows the reason in one line instead of
  the tail of the log.

## [0.2.0-rc6]

### Added

- The app's Dock icon shows how many PRs you haven't looked at, like Mail.

### Fixed

- After `brew upgrade` stopped the backend, an app left open waited forever
  with nothing coming in. It now starts the backend again when one it was
  connected to disappears, unless it was stopped on purpose (`pr-mon stop`, or
  Stop in the app), which the backend now records.

## [0.2.0-rc5]

### Changed

- Linked issues are their own section in the app, shown only when a PR closes
  something.

## [0.2.0-rc4]

### Fixed

- A notification could report success although the command failed: two channels
  sending at once raced on the results slice.

### Added

- The issues a PR closes on merge: its own section in the app, beside Blocked By
  and Details, and a `Closes:` line in the dashboard. An issue in another
  repository shows as `owner/repo#n`.

### Changed

- In the app, a pending merge (GitHub's auto-merge, or one pr-mon will make when
  the PR is ready) is a coloured chip on its own line rather than grey text
  beside the status.

## [0.2.0-rc3]

### Changed

- The app registers "start the backend at login" itself, through an agent that
  ships inside the bundle, so System Settings lists it as pr-mon with its icon
  instead of the name on the signing certificate. `pr-mon autostart enable`
  defers to the app when it is running from inside PrMon.app; a source install
  is unaffected.

## [0.2.0-rc2]

### Fixed

- Commands from the Mac app (enabling auto-merge, arming a merge, marking a PR
  seen) reported "Unexpected data from the backend" although they had worked:
  a reply with nothing to return left out its `result` key.
- Uninstalling the Homebrew cask left the backend running from a deleted
  binary; it is now signalled to stop first.

### Added

- `make uninstall` and `make purge` for a source install.
- Homebrew cask: one notarized artifact installs the app and puts the `pr-mon`
  command line (dashboard, backend and CLI) on PATH.
- An app icon: three status dots.
- `pr-mon restart`, and a poll-interval setting.
- A native macOS app (`macos/`): repositories, pull requests and details in a
  split view, with Settings for repos, notifications and the backend.

### Changed

- pr-mon is now a single Go binary. The backend, dashboard and CLI were
  rewritten from Python, keeping the same config and state files, the same
  socket protocol and the same behaviour.
- The backend works out each PR's status, blocking reasons and available
  actions; clients display what it sends rather than deciding for themselves.

### Removed

- The Python implementation, and its uv/ruff tooling.
