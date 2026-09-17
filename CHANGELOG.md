# Changelog

All notable changes to pr-mon, newest first. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the versions are
the git tags the release workflow builds from.

## [Unreleased]

### Added

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
