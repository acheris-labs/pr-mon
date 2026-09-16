"""Async GitHub GraphQL client."""

import asyncio
import subprocess
from datetime import UTC, datetime, timedelta

import httpx

from pr_mon.models import MergeMethod, RepoInfo, parse_repo

API_URL = "https://api.github.com/graphql"
PR_LIMIT = 50
CHECK_LIMIT = 20
PAGE_SIZE = 10
RATE_LIMIT_FALLBACK = timedelta(seconds=60)

PR_FIELDS = f"""
fragment PrFields on PullRequest {{
  id number state title url createdAt isDraft
  author {{ login }}
  headRefName baseRefName
  headRef {{ id }}
  headRepository {{ nameWithOwner }}
  mergeable mergeStateStatus reviewDecision
  autoMergeRequest {{ mergeMethod enabledBy {{ login }} }}
  commits(last: 1) {{
    nodes {{
      commit {{
        committedDate
        statusCheckRollup {{
          state
          contexts(first: {CHECK_LIMIT}) {{
            totalCount
            nodes {{
              __typename
              ... on CheckRun {{ name status conclusion startedAt }}
              ... on StatusContext {{ context state createdAt }}
            }}
          }}
        }}
      }}
    }}
  }}
}}
"""

# Mergeability fields are slow for GitHub to compute, and GraphQL requests time out
# after ~10s, so full PR details are fetched in pages of PAGE_SIZE.
REPO_QUERY = (
    f"""
query($owner: String!, $name: String!) {{
  repository(owner: $owner, name: $name) {{
    nameWithOwner
    mergeCommitAllowed
    squashMergeAllowed
    rebaseMergeAllowed
    deleteBranchOnMerge
    autoMergeAllowed
    newest: pullRequests(
      states: OPEN, first: {PR_LIMIT}, orderBy: {{field: CREATED_AT, direction: DESC}}
    ) {{
      totalCount
      nodes {{ number }}
    }}
    firstPage: pullRequests(
      states: OPEN, first: {PAGE_SIZE}, orderBy: {{field: CREATED_AT, direction: DESC}}
    ) {{
      nodes {{ ...PrFields }}
    }}
  }}
}}
"""
    + PR_FIELDS
)


def batch_query(numbers: list[int]) -> str:
    fields = "\n".join(f"    pr{n}: pullRequest(number: {n}) {{ ...PrFields }}" for n in numbers)
    return (
        "query($owner: String!, $name: String!) {\n"
        "  repository(owner: $owner, name: $name) {\n"
        f"{fields}\n"
        "  }\n"
        "}\n" + PR_FIELDS
    )


MERGE_MUTATION = """
mutation($id: ID!, $method: PullRequestMergeMethod!) {
  mergePullRequest(input: {pullRequestId: $id, mergeMethod: $method}) { clientMutationId }
}
"""

UPDATE_BRANCH_MUTATION = """
mutation($id: ID!) {
  updatePullRequestBranch(input: {pullRequestId: $id}) { clientMutationId }
}
"""

ENABLE_AUTO_MERGE_MUTATION = """
mutation($id: ID!, $method: PullRequestMergeMethod!) {
  enablePullRequestAutoMerge(input: {pullRequestId: $id, mergeMethod: $method}) {
    clientMutationId
  }
}
"""

DISABLE_AUTO_MERGE_MUTATION = """
mutation($id: ID!) {
  disablePullRequestAutoMerge(input: {pullRequestId: $id}) { clientMutationId }
}
"""

DELETE_REF_MUTATION = """
mutation($id: ID!) {
  deleteRef(input: {refId: $id}) { clientMutationId }
}
"""


class GitHubError(Exception):
    pass


class NotFoundError(GitHubError):
    pass


class AuthError(GitHubError):
    pass


class RateLimitError(GitHubError):
    def __init__(self, message: str, reset_at: datetime):
        super().__init__(message)
        self.reset_at = reset_at


def get_token() -> str:
    try:
        result = subprocess.run(["gh", "auth", "token"], capture_output=True, text=True)
    except FileNotFoundError as e:
        raise AuthError("GitHub CLI 'gh' not found") from e
    token = result.stdout.strip()
    if result.returncode != 0 or not token:
        raise AuthError(result.stderr.strip() or "gh returned no token")
    return token


def _rate_limit_reset(response: httpx.Response) -> datetime:
    reset = response.headers.get("x-ratelimit-reset")
    if reset and reset.isdigit():
        return datetime.fromtimestamp(int(reset), UTC)
    return datetime.now(UTC) + RATE_LIMIT_FALLBACK


class GitHubClient:
    def __init__(self, token: str, transport: httpx.AsyncBaseTransport | None = None):
        self._http = httpx.AsyncClient(
            transport=transport,
            headers={"Authorization": f"Bearer {token}"},
            timeout=30,
        )

    async def aclose(self) -> None:
        await self._http.aclose()

    async def _graphql(self, query: str, variables: dict) -> dict:
        try:
            response = await self._http.post(API_URL, json={"query": query, "variables": variables})
        except httpx.HTTPError as e:
            raise GitHubError(f"Network error: {e}") from e
        if response.status_code == 401:
            raise AuthError("GitHub rejected the token; run `gh auth login`")
        if (
            response.status_code in (403, 429)
            and response.headers.get("x-ratelimit-remaining") == "0"
        ):
            raise RateLimitError("GitHub rate limit reached", _rate_limit_reset(response))
        if response.status_code in (502, 504):
            raise GitHubError(f"GitHub timed out ({response.status_code}); will retry next poll")
        if response.is_error:
            detail = "" if "html" in response.headers.get("content-type", "") else response.text
            raise GitHubError(f"GitHub API error {response.status_code} {detail[:200]}".strip())
        body = response.json()
        errors = body.get("errors") or []
        if errors:
            message = "; ".join(e.get("message", "unknown error") for e in errors)
            types = {e.get("type") for e in errors}
            if "RATE_LIMITED" in types:
                raise RateLimitError(message, _rate_limit_reset(response))
            if "NOT_FOUND" in types:
                raise NotFoundError(message)
            raise GitHubError(message)
        return body["data"]

    async def fetch_repo(self, name: str) -> RepoInfo:
        owner, _, repo = name.partition("/")
        if not owner or not repo or "/" in repo:
            raise NotFoundError(f"Expected owner/name, got {name!r}")
        variables = {"owner": owner, "name": repo}
        data = (await self._graphql(REPO_QUERY, variables)).get("repository")
        if data is None:
            raise NotFoundError(f"Repository {name} not found")
        by_number = {pr["number"]: pr for pr in data["firstPage"]["nodes"]}
        numbers = [pr["number"] for pr in data["newest"]["nodes"]]
        remaining = [n for n in numbers if n not in by_number]
        batches = [remaining[i : i + PAGE_SIZE] for i in range(0, len(remaining), PAGE_SIZE)]
        results = await asyncio.gather(
            *(self._graphql(batch_query(batch), variables) for batch in batches)
        )
        for result in results:
            by_number.update({pr["number"]: pr for pr in result["repository"].values() if pr})
        data["pullRequests"] = {
            "totalCount": data["newest"]["totalCount"],
            "nodes": [by_number[n] for n in numbers if by_number.get(n, {}).get("state") == "OPEN"],
        }
        return parse_repo(data)

    async def merge(self, pr_id: str, method: MergeMethod) -> None:
        await self._graphql(MERGE_MUTATION, {"id": pr_id, "method": str(method)})

    async def update_branch(self, pr_id: str) -> None:
        await self._graphql(UPDATE_BRANCH_MUTATION, {"id": pr_id})

    async def enable_auto_merge(self, pr_id: str, method: MergeMethod) -> None:
        await self._graphql(ENABLE_AUTO_MERGE_MUTATION, {"id": pr_id, "method": str(method)})

    async def disable_auto_merge(self, pr_id: str) -> None:
        await self._graphql(DISABLE_AUTO_MERGE_MUTATION, {"id": pr_id})

    async def delete_branch(self, ref_id: str) -> None:
        await self._graphql(DELETE_REF_MUTATION, {"id": ref_id})
