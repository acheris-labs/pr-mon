"""Pull request data and merge-readiness rules."""

import json
from dataclasses import asdict, dataclass
from enum import StrEnum

from pr_mon.config import NotifyConfig


class Status(StrEnum):
    DRAFT = "DRAFT"
    CHECKING = "CHECKING"
    CONFLICT = "CONFLICT"
    FAILING = "FAILING"
    PENDING = "PENDING"
    BEHIND = "BEHIND"
    BLOCKED = "BLOCKED"
    READY = "READY"


BLOCKED_STATUSES = frozenset({Status.CONFLICT, Status.FAILING, Status.BLOCKED})


class MergeMethod(StrEnum):
    SQUASH = "SQUASH"
    MERGE = "MERGE"
    REBASE = "REBASE"


@dataclass(frozen=True)
class Check:
    name: str
    state: str
    started_at: str | None = None


@dataclass(frozen=True)
class Action:
    """A user-requested change to a PR."""

    # "merge" | "update" | "auto_merge_on" | "auto_merge_off" | "arm_merge" | "disarm_merge"
    kind: str
    method: MergeMethod | None = None
    delete_branch: bool = False


def owner_key(repo: str) -> str:
    """Case-insensitive owner of an owner/name repo, used to group repos."""
    return repo.partition("/")[0].lower()


@dataclass(frozen=True)
class ArmedMerge:
    """A PR pr-mon will merge itself once it is strictly ready."""

    method: MergeMethod
    delete_branch: bool
    armed_at: str  # ISO-8601 UTC


def armed_to_dict(armed: ArmedMerge) -> dict:
    return {
        "method": str(armed.method),
        "delete_branch": armed.delete_branch,
        "armed_at": armed.armed_at,
    }


def armed_from_dict(data: object) -> ArmedMerge:
    try:
        return ArmedMerge(
            MergeMethod(data["method"]), bool(data["delete_branch"]), str(data["armed_at"])
        )
    except (KeyError, TypeError, ValueError) as e:
        raise ValueError(f"invalid armed merge: {e}") from e


@dataclass(frozen=True)
class AutoMerge:
    method: MergeMethod
    enabled_by: str


@dataclass(frozen=True)
class ActionOption:
    """One entry of a PR's action menu, as the backend decides it."""

    key: str  # "merge" | "auto_merge" | "update": the menu slot
    kind: str  # the Action kind to send
    label: str
    available: bool
    reason: str | None = None  # why it is unavailable
    note: str | None = None
    needs_method: bool = False  # ask which MergeMethod (from the repo's merge_methods)
    offers_delete_branch: bool = False  # offer deleting the head branch (default on)


@dataclass(frozen=True)
class EventOption:
    name: str  # a NotifyConfig event
    label: str


@dataclass(frozen=True)
class NotificationForm:
    """What the notification settings editor offers, as the backend defines it."""

    events: tuple[EventOption, ...]
    variables: tuple[str, ...]  # placeholder names for {{NAME}}
    script_help: str
    defaults: NotifyConfig  # settings for a repo that has none yet


@dataclass(frozen=True)
class NotificationPreview:
    text: str  # the template rendered with sample values
    unknown: tuple[str, ...]  # placeholders the backend doesn't know


@dataclass(frozen=True)
class Reason:
    text: str
    level: str  # "error" | "warning" | "info"


@dataclass(frozen=True)
class PullRequest:
    id: str
    number: int
    title: str
    url: str
    author: str
    created_at: str
    is_draft: bool
    head_ref: str
    base_ref: str
    head_ref_id: str | None
    head_repo: str | None
    mergeable: str
    merge_state: str
    review_decision: str | None
    check_state: str | None
    checks: tuple[Check, ...]
    checks_total: int
    auto_merge: AutoMerge | None = None
    last_commit_at: str | None = None
    head_sha: str | None = None
    # Filled in by the backend (readiness.py, actions.py); clients only display them.
    status: Status = Status.CHECKING
    reasons: tuple[Reason, ...] = ()
    strictly_ready: bool = False
    last_check_started_at: str | None = None
    actions: tuple[ActionOption, ...] = ()


@dataclass(frozen=True)
class RepoInfo:
    name: str
    merge_methods: tuple[MergeMethod, ...]
    delete_branch_on_merge: bool
    prs: tuple[PullRequest, ...]
    pr_total: int
    auto_merge_allowed: bool = False


def repo_to_dict(repo: RepoInfo) -> dict:
    """JSON-safe form of a RepoInfo (enums become strings, tuples become lists)."""
    return json.loads(json.dumps(asdict(repo)))


def repo_from_dict(data: dict) -> RepoInfo:
    def pr_from_dict(pr: dict) -> PullRequest:
        auto = pr["auto_merge"]
        return PullRequest(
            **{
                **pr,
                "status": Status(pr["status"]),
                "reasons": tuple(Reason(**r) for r in pr["reasons"]),
                "actions": tuple(ActionOption(**a) for a in pr["actions"]),
                "checks": tuple(Check(**c) for c in pr["checks"]),
                "auto_merge": AutoMerge(MergeMethod(auto["method"]), auto["enabled_by"])
                if auto
                else None,
            }
        )

    return RepoInfo(
        **{
            **data,
            "merge_methods": tuple(MergeMethod(m) for m in data["merge_methods"]),
            "prs": tuple(pr_from_dict(pr) for pr in data["prs"]),
        }
    )
