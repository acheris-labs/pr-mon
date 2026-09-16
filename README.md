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
| `D` | repo list | Remove the selected repo |
| `tab` / `shift+tab` | anywhere | Move between repo list and PR list |
| `enter` | PR list | Action menu |
| `m` | action menu | Merge (asks `s`/`m`/`r` if the repo allows several methods) |
| `u` | action menu | Update branch (when behind base) |
| `space` | action menu | Toggle "delete remote branch" |
| `r` | anywhere | Refresh now |
| `q` | anywhere | Quit |

## Indicators

- Repo list: `● name (n)` — `n` PRs you haven't looked at since they were opened,
  became ready, or became blocked. Green = something is ready, red = something
  got blocked, yellow = new PRs only. `⚠` = the last refresh failed.
- Selecting a PR in the PR list marks it seen.
- PR statuses: `READY`, `CONFLICT`, `FAILING`, `BLOCKED`, `BEHIND`, `PENDING`,
  `CHECKING` (GitHub still computing), `DRAFT`. The details pane lists every
  blocking reason.

## Files

- Config: `~/.config/pr-mon/config.toml` (respects `XDG_CONFIG_HOME`)

  ```toml
  poll_interval = 60
  repos = [
      "owner/repo",
  ]
  ```

- State (seen PRs): `~/.local/state/pr-mon/state.json` (respects `XDG_STATE_HOME`)

## Development

```sh
make install   # uv sync
make test      # unittest
make lint      # ruff check + format check
make fmt       # ruff format + fix
make clean
```
