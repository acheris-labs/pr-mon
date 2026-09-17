"""Pull request data and merge-readiness rules."""

import json
from dataclasses import asdict, dataclass
from enum import StrEnum


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


FAILED_CHECK_STATES = frozenset(
    {"FAILURE", "ERROR", "CANCELLED", "TIMED_OUT", "ACTION_REQUIRED", "STARTUP_FAILURE"}
)
PENDING_CHECK_STATES = frozenset(
    {"PENDING", "EXPECTED", "QUEUED", "IN_PROGRESS", "WAITING", "REQUESTED"}
)
READY_MERGE_STATES = frozenset({"CLEAN", "HAS_HOOKS", "UNSTABLE"})
KNOWN_MERGE_STATES = READY_MERGE_STATES | {"BEHIND", "BLOCKED", "DIRTY", "DRAFT", "UNKNOWN"}


@dataclass(frozen=True)
class Check:
    name: str
    state: str
    started_at: str | None = None

    @property
    def failed(self) -> bool:
        return self.state in FAILED_CHECK_STATES

    @property
    def pending(self) -> bool:
        return self.state in PENDING_CHECK_STATES


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

    @property
    def last_check_started_at(self) -> str | None:
        # ISO-8601 UTC timestamps from GitHub sort correctly as strings.
        return max((c.started_at for c in self.checks if c.started_at), default=None)

    @property
    def strictly_ready(self) -> bool:
        """Safe to merge unattended: mergeable and no check failing, required or not."""
        return (
            self.status == Status.READY
            and self.merge_state in ("CLEAN", "HAS_HOOKS")
            and self.check_state not in ("FAILURE", "ERROR")
            and not any(check.failed for check in self.checks)
        )

    @property
    def status(self) -> Status:
        if self.is_draft:
            return Status.DRAFT
        if self.mergeable == "UNKNOWN" or self.merge_state == "UNKNOWN":
            return Status.CHECKING
        if self.mergeable == "CONFLICTING" or self.merge_state == "DIRTY":
            return Status.CONFLICT
        if self.check_state in ("FAILURE", "ERROR") and self.merge_state != "UNSTABLE":
            return Status.FAILING
        if self.check_state in ("PENDING", "EXPECTED"):
            return Status.PENDING
        if self.merge_state == "BEHIND":
            return Status.BEHIND
        if self.merge_state in READY_MERGE_STATES:
            return Status.READY
        return Status.BLOCKED

    @property
    def reasons(self) -> list[Reason]:
        reasons = []
        if self.is_draft:
            reasons.append(Reason("Draft", "info"))
        if self.mergeable == "UNKNOWN" or self.merge_state == "UNKNOWN":
            reasons.append(Reason("GitHub is still computing mergeability", "info"))
        if self.mergeable == "CONFLICTING" or self.merge_state == "DIRTY":
            reasons.append(Reason("Merge conflicts", "error"))
        check_level = "warning" if self.merge_state == "UNSTABLE" else "error"
        for check in self.checks:
            if check.failed:
                reasons.append(Reason(f"Check failed: {check.name}", check_level))
        hidden = self.checks_total - len(self.checks)
        if hidden > 0:
            reasons.append(Reason(f"+{hidden} more checks not shown", "info"))
        pending = sum(1 for check in self.checks if check.pending)
        if pending:
            reasons.append(Reason(f"{pending} checks pending", "warning"))
        if self.review_decision == "CHANGES_REQUESTED":
            reasons.append(Reason("Changes requested", "error"))
        elif self.review_decision == "REVIEW_REQUIRED":
            reasons.append(Reason("Review required", "warning"))
        if self.merge_state == "BEHIND":
            reasons.append(Reason("Behind base branch", "warning"))
        if self.merge_state not in KNOWN_MERGE_STATES:
            reasons.append(Reason(f"Merge state: {self.merge_state}", "warning"))
        elif self.status == Status.BLOCKED and not any(
            r.level in ("error", "warning") for r in reasons
        ):
            reasons.append(Reason("Blocked by branch protection", "error"))
        return reasons


@dataclass(frozen=True)
class RepoInfo:
    name: str
    merge_methods: tuple[MergeMethod, ...]
    delete_branch_on_merge: bool
    prs: tuple[PullRequest, ...]
    pr_total: int
    auto_merge_allowed: bool = False


def _parse_check(node: dict) -> Check:
    if node.get("__typename") == "StatusContext":
        return Check(node["context"], node["state"], node.get("createdAt"))
    started_at = node.get("startedAt")
    if node.get("status") != "COMPLETED":
        return Check(node["name"], "PENDING", started_at)
    return Check(node["name"], node.get("conclusion") or "PENDING", started_at)


def _parse_pr(node: dict) -> PullRequest:
    commits = node["commits"]["nodes"]
    commit = commits[0]["commit"] if commits else {}
    rollup = commit.get("statusCheckRollup")
    contexts = rollup["contexts"] if rollup else {"nodes": [], "totalCount": 0}
    auto = node.get("autoMergeRequest")
    return PullRequest(
        id=node["id"],
        number=node["number"],
        title=node["title"],
        url=node["url"],
        author=(node.get("author") or {}).get("login", "ghost"),
        created_at=node["createdAt"],
        is_draft=node["isDraft"],
        head_ref=node["headRefName"],
        base_ref=node["baseRefName"],
        head_ref_id=(node.get("headRef") or {}).get("id"),
        head_repo=(node.get("headRepository") or {}).get("nameWithOwner"),
        mergeable=node["mergeable"],
        merge_state=node["mergeStateStatus"],
        review_decision=node.get("reviewDecision"),
        check_state=rollup["state"] if rollup else None,
        checks=tuple(_parse_check(c) for c in contexts["nodes"] if c),
        checks_total=contexts["totalCount"],
        auto_merge=AutoMerge(
            MergeMethod(auto["mergeMethod"]), (auto.get("enabledBy") or {}).get("login", "ghost")
        )
        if auto
        else None,
        last_commit_at=commit.get("committedDate"),
        head_sha=commit.get("oid"),
    )


def parse_repo(data: dict) -> RepoInfo:
    allowed = {
        MergeMethod.SQUASH: data["squashMergeAllowed"],
        MergeMethod.MERGE: data["mergeCommitAllowed"],
        MergeMethod.REBASE: data["rebaseMergeAllowed"],
    }
    prs = data["pullRequests"]
    return RepoInfo(
        name=data["nameWithOwner"],
        merge_methods=tuple(method for method, ok in allowed.items() if ok),
        delete_branch_on_merge=data["deleteBranchOnMerge"],
        prs=tuple(_parse_pr(node) for node in prs["nodes"]),
        pr_total=prs["totalCount"],
        auto_merge_allowed=data.get("autoMergeAllowed", False),
    )


def repo_to_dict(repo: RepoInfo) -> dict:
    """JSON-safe form of a RepoInfo (enums become strings, tuples become lists)."""
    return json.loads(json.dumps(asdict(repo)))


def repo_from_dict(data: dict) -> RepoInfo:
    def pr_from_dict(pr: dict) -> PullRequest:
        auto = pr["auto_merge"]
        return PullRequest(
            **{
                **pr,
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
