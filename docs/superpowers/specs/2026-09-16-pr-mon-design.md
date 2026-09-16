# pr-mon — Design

Date: 2026-09-16

## Purpose

A terminal dashboard (TUI) that monitors open GitHub pull requests across a
user-chosen set of repositories, shows each PR's merge readiness and blocking
reasons, flags repos that need attention, and lets the user merge or update a
PR directly from the TUI.

## Scope

In scope (v1):

- Add/remove monitored repos from within the TUI; persist the list.
- Monitor all open PRs per repo (newest 50).
- Per-PR status and full blocking reasons.
- Attention indicators on repos (new / became ready / became blocked).
- Action menu per PR: merge (with method sub-choice and delete-branch option), update branch.
- Persisted seen/unseen state across restarts.

Out of scope (v1, planned later):

- PR filters.
- Notifications outside the TUI (macOS, `im`, scripts). The tracker emits
  internal events so pluggable notifiers can subscribe later; no plugin system
  is built now.
- Additional actions (open in browser, approve, re-run checks).
- GitHub Enterprise hosts (github.com only).

## Stack

- Python, packaged with `uv` (`uv init --package`, `src/pr_mon/` layout,
  `tests/` at repo root, `pr-mon` console entry point).
- `textual` for the TUI.
- `httpx` (async) for the GitHub GraphQL API.
- Auth: token obtained once at startup from `gh auth token`.
- Tests: `unittest`.
- `Makefile` targets: `install`, `run`, `test`, `lint`, `fmt`, `clean`,
  `tool-install` (`uv tool install .`).

## Layout

```
┌─ Repos ──────────────┬─ PRs ───────────────────────────────────────────┐
│ ● acme/api      (2)  │ ✓ #412 Add retry to client      alice   READY    │
│   acme/web           │ ✗ #409 Bump deps                 bot    CONFLICT │
│ ● acme/infra    (1)  │ ⋯ #405 Refactor auth            bob    PENDING  │
│                      ├─ Details ───────────────────────────────────────┤
│                      │ #409 Bump deps  (bot → main)                    │
│                      │ Blocked by:                                     │
│                      │  ✗ Merge conflicts                              │
│                      │  ✗ Check failed: test (ubuntu)                  │
│                      │  ⚠ Review required                              │
└──────────────────────┴─────────────────────────────────────────────────┘
 A add repo  D remove repo  enter actions  r refresh  q quit
```

- Left: repo navigator with attention badge.
- Top right: PR list for the selected repo, newest first (by creation date).
- Bottom right: details for the selected PR, including all blocking reasons.

### Key bindings

| Key | Context | Action |
|-----|---------|--------|
| `A` | global | Add repo (prompt for `owner/name`; validated against GitHub before saving) |
| `D` | repo pane | Remove selected repo (with confirmation) |
| `r` | global | Force refresh all repos |
| `q` | global | Quit |
| `enter` | PR list | Open action menu |
| `m` | action menu | Merge (disabled with reason if not mergeable) |
| `u` | action menu | Update branch (only shown when `mergeStateStatus` is `BEHIND`) |
| `esc` | dialogs | Close |

## Modules

| Module | Responsibility | Depends on |
|--------|----------------|------------|
| `config.py` | Load/save config (`~/.config/pr-mon/config.toml`): repo list, poll interval | `tomllib` + small hand-written writer |
| `state.py` | Load/save seen state (`~/.local/state/pr-mon/state.json`) | `json` |
| `github.py` | GraphQL client: fetch repo + PRs, validate repo, merge, update branch, delete branch | `httpx` |
| `models.py` | `RepoInfo`, `PullRequest` dataclasses; derive status and blocking reasons from raw fields | — |
| `tracker.py` | Diff successive polls, emit events, maintain unseen set | `models`, `state` |
| `app.py`, `widgets/` | Textual app, panes, action menu, merge sub-choice, add-repo prompt | all above |

### Data flow

Poll timer → `github` fetches each repo concurrently → `tracker` diffs against
previous snapshot, emits events, updates unseen set → UI refreshes badges,
PR list, and details.

## Data fetched per repo

GitHub's GraphQL API times out after ~10s and mergeability fields are slow, so
PR details are paged: one request returns repo settings, the newest 50 PR
numbers, and full details for the first 10; remaining PRs are fetched in
parallel batches of 10 via `pullRequest(number:)` aliases (PRs no longer
`OPEN` by then are dropped).


Repository:

- `mergeCommitAllowed`, `squashMergeAllowed`, `rebaseMergeAllowed`
- `deleteBranchOnMerge`

Open PRs (first 50, ordered by `CREATED_AT` DESC), each with:

- `number`, `title`, `author`, `url`, `createdAt`, `isDraft`
- `headRefName`, `baseRefName`, `headRepository` (to detect forks)
- `mergeable`, `mergeStateStatus`, `reviewDecision`
- head commit `statusCheckRollup` (state + first 20 contexts: name, conclusion/state)
- `totalCount` of open PRs and of check contexts (for "showing 50 of N" / "+N more checks")

## Status derivation

Each PR has one status; first matching row wins.

| Status | Condition | Icon / color |
|--------|-----------|--------------|
| `DRAFT` | `isDraft` | ◌ dim |
| `CHECKING` | `mergeable == UNKNOWN` or `mergeStateStatus == UNKNOWN` | ? dim |
| `CONFLICT` | `mergeable == CONFLICTING` | ✗ red |
| `FAILING` | check rollup `FAILURE` or `ERROR` and `mergeStateStatus != UNSTABLE` | ✗ red |
| `PENDING` | check rollup `PENDING` / `EXPECTED` | ⋯ yellow |
| `BEHIND` | `mergeStateStatus == BEHIND` | ↓ yellow |
| `BLOCKED` | `mergeStateStatus == BLOCKED` | ⚠ red |
| `READY` | `mergeStateStatus` in `CLEAN`, `HAS_HOOKS`, `UNSTABLE` | ✓ green |

Notes:

- `UNSTABLE` (only non-required checks failing) maps to `READY`; failing
  checks are shown as warnings in details. Revisit after real use.
- Any `mergeStateStatus` not covered above maps to `BLOCKED` with the raw
  value shown as the reason.

### Blocking reasons (details pane)

All applicable reasons are listed, independent of status:

- Draft
- Merge conflicts
- Each failing check by name (and "+N more checks" if truncated)
- Checks pending
- Changes requested / review required
- Behind base branch
- Blocked by branch protection (when `BLOCKED` with no other identified reason)
- Mergeability still being computed

## Attention tracking

Events emitted by `tracker`:

- `NEW` — PR not present in the previous snapshot or in persisted seen state.
- `READY` — status transitioned to `READY` from any other status.
- `BLOCKED` — status transitioned into {`CONFLICT`, `FAILING`, `BLOCKED`}
  from a status outside that set. Transitions within the set do not re-fire.

Unseen:

- An event marks the PR unseen.
- Selecting the PR in the list marks it seen.
- Repo badge shows count of unseen PRs; hidden when zero.
- Badge color priority: green if any unseen PR is `READY`; else red if any
  unseen PR is blocked; else yellow.
- Closed/merged PRs are removed from the snapshot and seen state.

Persistence:

- `state.json` stores, per repo, each PR's last known status and seen flag.
- On startup the first poll is diffed against persisted state, so changes that
  happened while pr-mon was closed are flagged; previously seen, unchanged PRs
  stay quiet.

## Actions

Enter on a PR opens the action menu.

### Merge (`m`)

- Disabled if status is not `READY`; the menu shows why.
- Allowed methods come from the repo settings.
  - One allowed method: `m` merges directly with it.
  - Multiple: `m` opens a sub-choice listing only allowed methods
    (`s` squash, `m` merge, `r` rebase).
- Delete-branch checkbox:
  - Hidden if the repo has `deleteBranchOnMerge` enabled.
  - Otherwise shown, checked by default. When checked, the head branch is
    deleted via a separate API call after a successful merge.
  - Deletion failure (e.g., fork without push access) is a warning; the merge
    is still reported as successful.
- After the action, the repo is refreshed immediately.

### Update branch (`u`)

- Shown only when `mergeStateStatus` is `BEHIND`.
- Calls GitHub's update-branch mutation; repo is refreshed afterward.

## Polling

- Default interval 60s, configurable in `config.toml`.
- `r` forces an immediate refresh.
- Repos are fetched concurrently; one failing repo does not affect others.
- If a repo has any `CHECKING` PRs, it is re-polled after ~5s, up to 3 times,
  before returning to the normal interval.

## Error handling

| Situation | Behavior |
|-----------|----------|
| `gh` missing or not authenticated | Exit at startup with instruction to run `gh auth login` |
| Repo not found / no access on add | Reject in the add prompt; not saved |
| Network/API error during poll | Repo shows `⚠` badge; error shown in details; last good data retained |
| Rate limited | Status bar message; polling backs off until reset time |
| Merge/update failure | Show GitHub's error message; refresh the repo |
| Branch delete failure after merge | Warning only |
| Missing/corrupt config or state file | Start empty, show a warning; do not overwrite a corrupt config until the user changes it |

## Testing

`unittest`, run via `make test` (`uv run python -m unittest`).

| Layer | Approach |
|-------|----------|
| `models.py` | Unit tests per status row and precedence, from recorded GraphQL JSON fixtures |
| `tracker.py` | Poll sequences → expected events; no re-fire within blocked set; cleanup of closed PRs; restart diff against persisted state |
| `config.py` / `state.py` | Round-trip in temp dirs; missing/corrupt files |
| `github.py` | `httpx.MockTransport`: query shape, PR cap, error mapping (404, rate limit, GraphQL errors) |
| UI | Textual `run_test` pilot: navigation, enter → action menu, `m` with one vs. multiple methods, delete-branch checkbox visibility, `A`/`D` flows, badge clears on view |
| Smoke | `make run` against a real repo, read-only unless a throwaway PR is provided |
