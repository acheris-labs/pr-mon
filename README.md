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

## Files

- Config: `~/.config/pr-mon/config.toml` (respects `XDG_CONFIG_HOME`)

  ```toml
  poll_interval = 60
  repos = [
      "owner/repo",
  ]
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
