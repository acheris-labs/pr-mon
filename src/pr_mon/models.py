"""Pull request data and merge-readiness rules."""

from dataclasses import dataclass
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

    @property
    def failed(self) -> bool:
        return self.state in FAILED_CHECK_STATES

    @property
    def pending(self) -> bool:
        return self.state in PENDING_CHECK_STATES


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


def _parse_check(node: dict) -> Check:
    if node.get("__typename") == "StatusContext":
        return Check(node["context"], node["state"])
    if node.get("status") != "COMPLETED":
        return Check(node["name"], "PENDING")
    return Check(node["name"], node.get("conclusion") or "PENDING")


def _parse_pr(node: dict) -> PullRequest:
    commits = node["commits"]["nodes"]
    rollup = commits[0]["commit"]["statusCheckRollup"] if commits else None
    contexts = rollup["contexts"] if rollup else {"nodes": [], "totalCount": 0}
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
    )
