"""Backend rules: how mergeable a PR is and why. Clients display the results."""

from dataclasses import replace

from pr_mon.models import Check, PullRequest, Reason, Status

FAILED_CHECK_STATES = frozenset(
    {"FAILURE", "ERROR", "CANCELLED", "TIMED_OUT", "ACTION_REQUIRED", "STARTUP_FAILURE"}
)
PENDING_CHECK_STATES = frozenset(
    {"PENDING", "EXPECTED", "QUEUED", "IN_PROGRESS", "WAITING", "REQUESTED"}
)
READY_MERGE_STATES = frozenset({"CLEAN", "HAS_HOOKS", "UNSTABLE"})
KNOWN_MERGE_STATES = READY_MERGE_STATES | {"BEHIND", "BLOCKED", "DIRTY", "DRAFT", "UNKNOWN"}


def assess(pr: PullRequest) -> PullRequest:
    """The PR with its status, reasons and readiness filled in."""
    status = pr_status(pr)
    return replace(
        pr,
        status=status,
        reasons=tuple(pr_reasons(pr, status)),
        strictly_ready=is_strictly_ready(pr, status),
        # ISO-8601 UTC timestamps from GitHub sort correctly as strings.
        last_check_started_at=max((c.started_at for c in pr.checks if c.started_at), default=None),
    )


def check_failed(check: Check) -> bool:
    return check.state in FAILED_CHECK_STATES


def check_pending(check: Check) -> bool:
    return check.state in PENDING_CHECK_STATES


def is_strictly_ready(pr: PullRequest, status: Status) -> bool:
    """Safe to merge unattended: mergeable and no check failing, required or not."""
    return (
        status == Status.READY
        and pr.merge_state in ("CLEAN", "HAS_HOOKS")
        and pr.check_state not in ("FAILURE", "ERROR")
        and not any(check_failed(check) for check in pr.checks)
    )


def pr_status(pr: PullRequest) -> Status:
    if pr.is_draft:
        return Status.DRAFT
    if pr.mergeable == "UNKNOWN" or pr.merge_state == "UNKNOWN":
        return Status.CHECKING
    if pr.mergeable == "CONFLICTING" or pr.merge_state == "DIRTY":
        return Status.CONFLICT
    if pr.check_state in ("FAILURE", "ERROR") and pr.merge_state != "UNSTABLE":
        return Status.FAILING
    if pr.check_state in ("PENDING", "EXPECTED"):
        return Status.PENDING
    if pr.merge_state == "BEHIND":
        return Status.BEHIND
    if pr.merge_state in READY_MERGE_STATES:
        return Status.READY
    return Status.BLOCKED


def pr_reasons(pr: PullRequest, status: Status) -> list[Reason]:
    reasons = []
    if pr.is_draft:
        reasons.append(Reason("Draft", "info"))
    if pr.mergeable == "UNKNOWN" or pr.merge_state == "UNKNOWN":
        reasons.append(Reason("GitHub is still computing mergeability", "info"))
    if pr.mergeable == "CONFLICTING" or pr.merge_state == "DIRTY":
        reasons.append(Reason("Merge conflicts", "error"))
    check_level = "warning" if pr.merge_state == "UNSTABLE" else "error"
    for check in pr.checks:
        if check_failed(check):
            reasons.append(Reason(f"Check failed: {check.name}", check_level))
    hidden = pr.checks_total - len(pr.checks)
    if hidden > 0:
        reasons.append(Reason(f"+{hidden} more checks not shown", "info"))
    pending = sum(1 for check in pr.checks if check_pending(check))
    if pending:
        reasons.append(Reason(f"{pending} checks pending", "warning"))
    if pr.review_decision == "CHANGES_REQUESTED":
        reasons.append(Reason("Changes requested", "error"))
    elif pr.review_decision == "REVIEW_REQUIRED":
        reasons.append(Reason("Review required", "warning"))
    if pr.merge_state == "BEHIND":
        reasons.append(Reason("Behind base branch", "warning"))
    if pr.merge_state not in KNOWN_MERGE_STATES:
        reasons.append(Reason(f"Merge state: {pr.merge_state}", "warning"))
    elif status == Status.BLOCKED and not any(r.level in ("error", "warning") for r in reasons):
        reasons.append(Reason("Blocked by branch protection", "error"))
    return reasons
