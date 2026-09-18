# Changelog

All notable changes to pr-mon, newest first. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the versions are
the git tags the release workflow builds from.

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
