# pr-mon Auto-merge Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Arm PRs in repos without native auto-merge so the backend merges them when strictly ready.

**Architecture:** Armed PRs live in `state.json` (owned by `Monitor`). After each refresh the monitor merges strictly-ready armed PRs with `expectedHeadOid`, reports via toasts and the repo's notification settings. The TUI arms/disarms through new action kinds and shows markers.

**Tech Stack:** Python 3.11+, textual, httpx, unittest.

**Spec:** `docs/superpowers/specs/2026-09-17-pr-mon-auto-merge-design.md`

## Global Constraints

- All imports at module level; unittest only; no new dependencies.
- Strict readiness: status READY, merge state CLEAN or HAS_HOOKS, no failed checks, not draft.
- Retryable merge error: message contains "was modified". Everything else disarms.
- New events `MERGED`, `MERGE_FAILED` (defaults on); variable `PR_REASON`.
- Existing native auto-merge behavior unchanged.

---

### Task 1: Models, state, config, notify, GitHub client

**Files:** `models.py`, `state.py`, `config.py`, `notify.py`, `github.py` + their tests.

- [ ] `ArmedMerge` dataclass; `PullRequest.strictly_ready`; tests per spec.
- [ ] `AppState.armed` load/save (invalid → `{}`); round-trip test.
- [ ] `EVENT_NAMES`/`DEFAULT_EVENTS` additions; config tests updated.
- [ ] `PR_REASON` in variables/sample; `pr_variables(..., reason="")`; tests.
- [ ] `merge(pr_id, method, expected_head_oid=None)`; tests for variables with/without it.
- [ ] Commit.

### Task 2: Service

**Files:** `backend.py`, `service.py`, `tests/test_service.py`.

- [ ] `Backend.armed(name)`; `Monitor.armed(name)`.
- [ ] `perform`: `arm_merge` / `disarm_merge` (persist, toast, emit `repo`).
- [ ] `apply`: cleanup of closed armed PRs (respecting truncation); spawn `_auto_merge` for strictly-ready armed PRs not in flight.
- [ ] `_auto_merge`: merge with expected head; success/failure/retry paths; delete branch; notifications (`MERGED`/`MERGE_FAILED`) when the repo has settings with those events; refresh.
- [ ] `remove_repo` drops armed.
- [ ] Tests per spec's Service row. Commit.

### Task 3: Protocol and remote mirror

**Files:** `protocol.py`, `remote.py`, tests.

- [ ] `armed` in snapshot and `repo` events; `arm_merge`/`disarm_merge` in `ACTION_KINDS`; mirror + `RemoteBackend.armed`.
- [ ] Round-trip tests (protocol) and a remote test arming through the socket. Commit.

### Task 4: TUI

**Files:** `screens.py`, `views.py`, `app.py`, `tests/test_app.py`.

- [ ] `ActionMenuScreen(repo, pr, armed)`: menu lines, method sub-choice, delete checkbox when arming, `a` on armed → `disarm_merge`.
- [ ] `EVENT_LABELS` for the new events (Events tab lists them).
- [ ] `pr_row(..., armed)` marker `auto*`; details line.
- [ ] App passes `backend.armed(name)`.
- [ ] Pilot tests per spec's TUI row. Commit.

### Task 5: Docs and verification

- [ ] README (keys table, Auto-merge section).
- [ ] `make lint test`; live check of the menu against a repo without native auto-merge (read-only; do not arm on repos we don't own).
- [ ] Commit.
