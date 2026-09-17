# pr-mon Auto-merge ("merge when ready") — Design

Date: 2026-09-17

## Purpose

For repos where GitHub's native auto-merge isn't allowed, let the user arm a PR
so the pr-mon backend merges it as soon as it is strictly ready.

## Decisions

1. `a` in the action menu uses GitHub's native auto-merge when the repo allows
   it (unchanged). Otherwise it arms **pr-mon auto-merge** ("merge when ready").
2. Ready means **strict**: status `READY`, `mergeStateStatus` is `CLEAN` or
   `HAS_HOOKS` (not `UNSTABLE`), no failed checks, not a draft.
3. New commits after arming **keep it armed**; it merges once the new head is
   strictly ready. `a` again disarms. Closing/merging the PR elsewhere disarms.
4. A merge failure **disarms** and reports. A head/base-changed race is retried
   silently on the next refresh.
5. Results go through the repo's notification settings as two new events,
   `MERGED` ("Merged by pr-mon") and `MERGE_FAILED` ("pr-mon auto-merge
   failed"), on by default for newly configured repos; new variable
   `{{PR_REASON}}` (the failure reason; empty otherwise). They are results of
   an explicit request, so the first-load gate does not apply.
6. Only the backend merges; with autostart it runs whenever the user is logged in.

## Behavior

### Arming (`a` when the repo doesn't allow native auto-merge)

| PR state | Menu line |
|----------|-----------|
| armed | `[a] Cancel merge when ready (pr-mon)` |
| no merge methods / draft | `[a] Merge when ready — unavailable (reason)` |
| already `READY` | `[a] Merge when ready — unavailable (already mergeable — use m)` |
| otherwise | `[a] Merge when ready (pr-mon)` |

- Arming asks for the merge method like `m` (sub-choice only if several are
  allowed) and uses the delete-branch checkbox, which is shown for arming too
  (unless the repo auto-deletes head branches).
- Arm → toast "pr-mon will merge acme/api#12 when it's ready (squash)".
  Disarm → toast "Cancelled merge when ready for acme/api#12".

### Persistence

`state.json` gains `armed`:

```json
"armed": {"acme/api": {"12": {"method": "SQUASH", "delete_branch": true, "armed_at": "…Z"}}}
```

- Removing a repo drops its armed PRs.
- On each successful refresh, an armed PR missing from the fetched list is
  disarmed **only if** the list isn't truncated (`pr_total <= len(prs)`);
  otherwise it may still be open beyond the newest 50.

### Merging

After each successful refresh (`Monitor.apply`), for every armed PR present and
strictly ready, and not already being merged:

1. `mergePullRequest(pullRequestId, mergeMethod, expectedHeadOid=<head sha just fetched>)`.
2. Success → delete the branch if chosen (failure = warning toast only),
   disarm, toast "Merged acme/api#12 (pr-mon auto-merge, squash)", notify
   `MERGED`, refresh the repo.
3. Error whose message contains "was modified" (GitHub's wording for the
   head/base changing under the merge — **hypothesis**, confirm on first real
   occurrence) → stay armed, log at info, retry next refresh.
4. Any other error → disarm, error toast "pr-mon auto-merge failed for
   acme/api#12: <reason>", notify `MERGE_FAILED` with `PR_REASON`.

`GitHubClient.merge(pr_id, method, expected_head_oid=None)` adds the optional
field; `m` keeps sending none.

### Display

- PR list status cell: `READY auto` (native) / `PENDING auto*` (pr-mon).
- Details: `Auto-merge: pr-mon (squash, delete branch), armed 2026-09-17 10:32 (5m ago)`.

## Interfaces

| Module | Change |
|--------|--------|
| `models.py` | `ArmedMerge(method: MergeMethod, delete_branch: bool, armed_at: str)`; `PullRequest.strictly_ready` property; action kinds `arm_merge`, `disarm_merge` |
| `state.py` | `AppState.armed: dict` (repo → number → dict) |
| `config.py` | `EVENT_NAMES` += `MERGED`, `MERGE_FAILED`; `DEFAULT_EVENTS` includes both |
| `notify.py` | `PR_REASON` in `VARIABLE_NAMES`, `SAMPLE_VARIABLES` (empty), `pr_variables(..., reason="")` |
| `github.py` | `merge(..., expected_head_oid=None)` |
| `backend.py` | `Backend.armed(name) -> dict[int, ArmedMerge]` |
| `service.py` | arm/disarm in `perform`; auto-merge pass in `apply`; cleanup; notifications |
| `protocol.py` | snapshot and `repo` event carry `armed`; action kinds; `ArmedMerge` (de)serialization |
| `remote.py` | mirror `armed` |
| `screens.py` | `ActionMenuScreen(repo, pr, armed)`; menu lines above; `EVENT_LABELS` for new events |
| `views.py` | `pr_row(pr, unseen, armed)`; details line |
| `app.py` | pass `armed` to menu and rendering |

## Testing

| Area | Tests |
|------|-------|
| `strictly_ready` | CLEAN/HAS_HOOKS ready; UNSTABLE, failed optional check, draft, pending, CHECKING not ready |
| Service | arm/disarm persist + toasts; merges when strictly ready with expected head; not while UNSTABLE; stays armed across new commits; "was modified" error keeps armed; other error disarms + toast + `MERGE_FAILED` with reason; success disarms + delete branch + `MERGED`; delete failure warning; closed PR disarmed; truncated list keeps armed; no double merge while in flight; remove repo drops armed; notifications not gated by first load; unconfigured repo → toasts only |
| Protocol/remote | armed round-trips in snapshot and repo events |
| GitHub | `expectedHeadOid` sent only when given |
| TUI | menu lines per state; arming asks method + delete checkbox; `a` on armed disarms; list/details markers; Events tab shows the new checkboxes |
