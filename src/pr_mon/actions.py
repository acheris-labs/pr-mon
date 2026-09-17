"""Backend rules: which actions a PR's menu offers. Clients display the results."""

from dataclasses import replace

from pr_mon.models import ActionOption, ArmedMerge, PullRequest, RepoInfo, Status

NO_METHODS = "no merge methods allowed"
KEEPS_BRANCH = "branch won't be deleted: repo doesn't auto-delete"


def with_actions(repo: RepoInfo, armed: dict[int, ArmedMerge]) -> RepoInfo:
    """The repo with every PR's action menu filled in."""
    prs = tuple(
        replace(pr, actions=available_actions(repo, pr, armed.get(pr.number))) for pr in repo.prs
    )
    return replace(repo, prs=prs)


def available_actions(
    repo: RepoInfo, pr: PullRequest, armed: ArmedMerge | None
) -> tuple[ActionOption, ...]:
    deletable = not repo.delete_branch_on_merge and bool(pr.head_ref_id)
    options = [_merge(repo, pr, deletable), _auto_merge(repo, pr, armed, deletable)]
    if pr.merge_state == "BEHIND":
        options.append(ActionOption("update", "update", "Update branch", True))
    return tuple(options)


def _merge(repo: RepoInfo, pr: PullRequest, deletable: bool) -> ActionOption:
    if pr.status == Status.READY and repo.merge_methods:
        return ActionOption(
            "merge", "merge", "Merge", True, needs_method=True, offers_delete_branch=deletable
        )
    why = [reason.text for reason in pr.reasons if reason.level != "info"]
    if not repo.merge_methods:
        why.append(NO_METHODS)
    detail = f"{pr.status}: {', '.join(why)}" if why else str(pr.status)
    return ActionOption("merge", "merge", "Merge", False, reason=detail)


def _auto_merge(
    repo: RepoInfo, pr: PullRequest, armed: ArmedMerge | None, deletable: bool
) -> ActionOption:
    """GitHub's native auto-merge where the repo allows it, else pr-mon's merge when ready."""
    if pr.auto_merge:
        return ActionOption("auto_merge", "auto_merge_off", "Disable auto-merge", True)
    if armed:
        return ActionOption("auto_merge", "disarm_merge", "Cancel merge when ready (pr-mon)", True)
    native = repo.auto_merge_allowed
    kind = "auto_merge_on" if native else "arm_merge"
    blocker = _auto_merge_blocker(repo, pr)
    if blocker:
        label = "Auto-merge" if native else "Merge when ready"
        return ActionOption("auto_merge", kind, label, False, reason=blocker)
    if not native:
        return ActionOption(
            "auto_merge",
            kind,
            "Merge when ready (pr-mon)",
            True,
            needs_method=True,
            offers_delete_branch=deletable,
        )
    note = (
        None if repo.delete_branch_on_merge else "branch won't be deleted: repo doesn't auto-delete"
    )
    return ActionOption("auto_merge", kind, "Enable auto-merge", True, note=note, needs_method=True)


def _auto_merge_blocker(repo: RepoInfo, pr: PullRequest) -> str | None:
    if not repo.merge_methods:
        return NO_METHODS
    if pr.is_draft:
        return "draft PR"
    if pr.status == Status.READY:
        return "already mergeable — use m"
    return None
