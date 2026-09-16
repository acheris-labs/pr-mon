"""Builders for raw GraphQL payloads shaped like the pr-mon repository query."""


def check_run(name, status="COMPLETED", conclusion="SUCCESS"):
    return {"__typename": "CheckRun", "name": name, "status": status, "conclusion": conclusion}


def status_context(context, state="SUCCESS"):
    return {"__typename": "StatusContext", "context": context, "state": state}


def raw_pr(
    number=1,
    title="A change",
    is_draft=False,
    mergeable="MERGEABLE",
    merge_state="CLEAN",
    review_decision=None,
    check_state="SUCCESS",
    checks=None,
    checks_total=None,
    head_ref_id="REF_1",
    auto_merge=None,
):
    checks = [] if checks is None else checks
    rollup = None
    if check_state is not None:
        rollup = {
            "state": check_state,
            "contexts": {
                "totalCount": len(checks) if checks_total is None else checks_total,
                "nodes": checks,
            },
        }
    return {
        "id": f"PR_{number}",
        "number": number,
        "state": "OPEN",
        "title": title,
        "url": f"https://github.com/acme/api/pull/{number}",
        "createdAt": "2026-09-15T01:28:54Z",
        "isDraft": is_draft,
        "author": {"login": "alice"},
        "headRefName": f"feature-{number}",
        "baseRefName": "main",
        "headRef": {"id": head_ref_id} if head_ref_id else None,
        "headRepository": {"nameWithOwner": "acme/api"},
        "mergeable": mergeable,
        "mergeStateStatus": merge_state,
        "reviewDecision": review_decision,
        "autoMergeRequest": auto_merge,
        "commits": {"nodes": [{"commit": {"statusCheckRollup": rollup}}]},
    }


def auto_merge(method="SQUASH", login="bob"):
    return {"mergeMethod": method, "enabledBy": {"login": login}}


def raw_repo(
    prs=(),
    name="acme/api",
    merge=True,
    squash=True,
    rebase=True,
    delete_on_merge=False,
    auto_merge_allowed=True,
    total=None,
):
    prs = list(prs)
    return {
        "nameWithOwner": name,
        "mergeCommitAllowed": merge,
        "squashMergeAllowed": squash,
        "rebaseMergeAllowed": rebase,
        "deleteBranchOnMerge": delete_on_merge,
        "autoMergeAllowed": auto_merge_allowed,
        "pullRequests": {
            "totalCount": len(prs) if total is None else total,
            "nodes": prs,
        },
    }


def repo_response(repo, page_size=10):
    """Shape a raw_repo() dict like the first-page response of the repo query."""
    data = {k: v for k, v in repo.items() if k != "pullRequests"}
    prs = repo["pullRequests"]
    data["newest"] = {
        "totalCount": prs["totalCount"],
        "nodes": [{"number": pr["number"]} for pr in prs["nodes"]],
    }
    data["firstPage"] = {"nodes": prs["nodes"][:page_size]}
    return {"repository": data}
