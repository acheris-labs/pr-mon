import json
import re
import subprocess
import unittest
from datetime import UTC, datetime
from unittest import mock

import httpx

from pr_mon.github import (
    AuthError,
    GitHubClient,
    GitHubError,
    NotFoundError,
    RateLimitError,
    get_token,
)
from pr_mon.models import MergeMethod
from tests.fixtures import raw_pr, raw_repo, repo_response


class Recorder:
    """MockTransport handler that records requests and replays a canned response."""

    def __init__(self, response):
        self.response = response
        self.requests = []

    def __call__(self, request):
        self.requests.append(request)
        return self.response

    @property
    def body(self):
        return json.loads(self.requests[-1].content)


def ok(data):
    return httpx.Response(200, json={"data": data})


def gql_error(error_type, message="boom"):
    return httpx.Response(
        200, json={"data": None, "errors": [{"type": error_type, "message": message}]}
    )


class ClientTest(unittest.IsolatedAsyncioTestCase):
    def client_for(self, response):
        recorder = Recorder(response)
        client = GitHubClient(lambda: "tok", transport=httpx.MockTransport(recorder))
        self.addAsyncCleanup(client.aclose)
        return client, recorder

    async def test_fetch_repo(self):
        client, rec = self.client_for(ok(repo_response(raw_repo([raw_pr(number=5)], total=70))))
        repo = await client.fetch_repo("acme/api")
        self.assertEqual(repo.name, "acme/api")
        self.assertEqual([p.number for p in repo.prs], [5])
        self.assertEqual(repo.pr_total, 70)
        self.assertEqual(len(rec.requests), 1)
        request = rec.requests[0]
        self.assertEqual(str(request.url), "https://api.github.com/graphql")
        self.assertEqual(request.headers["authorization"], "Bearer tok")
        self.assertEqual(rec.body["variables"], {"owner": "acme", "name": "api"})
        self.assertIn("CREATED_AT", rec.body["query"])
        self.assertIn("first: 50", rec.body["query"])
        self.assertIn("first: 10", rec.body["query"])

    async def test_fetch_repo_batches_remaining_prs(self):
        prs = [raw_pr(number=n) for n in range(125, 100, -1)]  # 25 PRs, newest first
        closed = raw_pr(number=101)
        closed["state"] = "MERGED"
        by_number = {pr["number"]: pr for pr in prs}
        by_number[101] = closed
        requests = []

        def handler(request):
            body = json.loads(request.content)
            requests.append(body)
            if "newest:" in body["query"]:
                return ok(repo_response(raw_repo(prs)))
            numbers = [int(n) for n in re.findall(r"pullRequest\(number: (\d+)\)", body["query"])]
            return ok({"repository": {f"pr{n}": by_number[n] for n in numbers}})

        client = GitHubClient(lambda: "tok", transport=httpx.MockTransport(handler))
        self.addAsyncCleanup(client.aclose)
        repo = await client.fetch_repo("acme/api")
        self.assertEqual([p.number for p in repo.prs], list(range(125, 101, -1)))
        self.assertEqual(len(requests), 3)
        self.assertEqual(requests[1]["variables"], {"owner": "acme", "name": "api"})
        batch_sizes = sorted(len(re.findall("pullRequest\\(", r["query"])) for r in requests[1:])
        self.assertEqual(batch_sizes, [5, 10])

    async def test_batch_failure_fails_fetch(self):
        prs = [raw_pr(number=n) for n in range(15)]

        def handler(request):
            if "newest:" in json.loads(request.content)["query"]:
                return ok(repo_response(raw_repo(prs)))
            return httpx.Response(502, headers={"content-type": "text/html"}, text="<html>")

        client = GitHubClient(lambda: "tok", transport=httpx.MockTransport(handler))
        self.addAsyncCleanup(client.aclose)
        with self.assertRaisesRegex(GitHubError, "timed out"):
            await client.fetch_repo("acme/api")

    async def test_fetch_missing_repo_null(self):
        client, _ = self.client_for(ok({"repository": None}))
        with self.assertRaises(NotFoundError):
            await client.fetch_repo("acme/nope")

    async def test_fetch_not_found_error(self):
        client, _ = self.client_for(gql_error("NOT_FOUND", "Could not resolve"))
        with self.assertRaisesRegex(NotFoundError, "Could not resolve"):
            await client.fetch_repo("acme/nope")

    async def test_invalid_name_makes_no_request(self):
        client, rec = self.client_for(ok({}))
        for bad in ("noslash", "a/b/c", "/x", "x/", ""):
            with self.assertRaises(NotFoundError):
                await client.fetch_repo(bad)
        self.assertEqual(rec.requests, [])

    async def test_unauthorized(self):
        client, _ = self.client_for(httpx.Response(401, json={"message": "Bad credentials"}))
        with self.assertRaises(AuthError):
            await client.fetch_repo("acme/api")

    async def test_rate_limit_header(self):
        response = httpx.Response(
            403,
            headers={"x-ratelimit-remaining": "0", "x-ratelimit-reset": "1900000000"},
            json={"message": "rate limited"},
        )
        client, _ = self.client_for(response)
        with self.assertRaises(RateLimitError) as ctx:
            await client.fetch_repo("acme/api")
        self.assertEqual(ctx.exception.reset_at, datetime.fromtimestamp(1900000000, UTC))

    async def test_rate_limited_graphql_error(self):
        client, _ = self.client_for(gql_error("RATE_LIMITED"))
        before = datetime.now(UTC)
        with self.assertRaises(RateLimitError) as ctx:
            await client.fetch_repo("acme/api")
        self.assertGreater(ctx.exception.reset_at, before)

    async def test_http_error(self):
        client, _ = self.client_for(httpx.Response(500, text="server broke"))
        with self.assertRaisesRegex(GitHubError, "500 server broke"):
            await client.fetch_repo("acme/api")

    async def test_html_error_body_is_hidden(self):
        response = httpx.Response(503, headers={"content-type": "text/html"}, text="<html>")
        client, _ = self.client_for(response)
        with self.assertRaises(GitHubError) as ctx:
            await client.fetch_repo("acme/api")
        self.assertEqual(str(ctx.exception), "GitHub API error 503")

    async def test_transport_error(self):
        def fail(request):
            raise httpx.ConnectError("no network")

        client = GitHubClient(lambda: "tok", transport=httpx.MockTransport(fail))
        self.addAsyncCleanup(client.aclose)
        with self.assertRaisesRegex(GitHubError, "no network"):
            await client.fetch_repo("acme/api")

    async def test_merge(self):
        client, rec = self.client_for(ok({"mergePullRequest": {"clientMutationId": None}}))
        await client.merge("PR_1", MergeMethod.SQUASH)
        self.assertIn("mergePullRequest", rec.body["query"])
        self.assertEqual(rec.body["variables"], {"id": "PR_1", "method": "SQUASH"})

    async def test_merge_with_expected_head(self):
        client, rec = self.client_for(ok({"mergePullRequest": {"clientMutationId": None}}))
        await client.merge("PR_1", MergeMethod.REBASE, expected_head_oid="abc123")
        self.assertIn("expectedHeadOid", rec.body["query"])
        self.assertEqual(
            rec.body["variables"], {"id": "PR_1", "method": "REBASE", "head": "abc123"}
        )

    async def test_merge_error_surfaces_message(self):
        client, _ = self.client_for(gql_error("UNPROCESSABLE", "Base branch was modified"))
        with self.assertRaisesRegex(GitHubError, "Base branch was modified"):
            await client.merge("PR_1", MergeMethod.MERGE)

    async def test_update_branch(self):
        client, rec = self.client_for(ok({"updatePullRequestBranch": {"clientMutationId": None}}))
        await client.update_branch("PR_1")
        self.assertIn("updatePullRequestBranch", rec.body["query"])
        self.assertEqual(rec.body["variables"], {"id": "PR_1"})

    async def test_enable_auto_merge(self):
        client, rec = self.client_for(
            ok({"enablePullRequestAutoMerge": {"clientMutationId": None}})
        )
        await client.enable_auto_merge("PR_1", MergeMethod.REBASE)
        self.assertIn("enablePullRequestAutoMerge", rec.body["query"])
        self.assertEqual(rec.body["variables"], {"id": "PR_1", "method": "REBASE"})

    async def test_disable_auto_merge(self):
        client, rec = self.client_for(
            ok({"disablePullRequestAutoMerge": {"clientMutationId": None}})
        )
        await client.disable_auto_merge("PR_1")
        self.assertIn("disablePullRequestAutoMerge", rec.body["query"])
        self.assertEqual(rec.body["variables"], {"id": "PR_1"})

    async def test_queries_request_auto_merge_fields(self):
        client, rec = self.client_for(ok(repo_response(raw_repo([raw_pr()]))))
        await client.fetch_repo("acme/api")
        self.assertIn("autoMergeAllowed", rec.body["query"])
        self.assertIn("autoMergeRequest", rec.body["query"])
        for field in ("oid", "committedDate", "startedAt", "createdAt"):
            self.assertIn(field, rec.body["query"])

    async def test_delete_branch(self):
        client, rec = self.client_for(ok({"deleteRef": {"clientMutationId": None}}))
        await client.delete_branch("REF_1")
        self.assertIn("deleteRef", rec.body["query"])
        self.assertEqual(rec.body["variables"], {"id": "REF_1"})


class TokenRefreshTest(unittest.IsolatedAsyncioTestCase):
    def make(self, statuses):
        tokens = iter(["old", "new", "newer"])
        self.provided = []

        def provider():
            token = next(tokens)
            self.provided.append(token)
            return token

        self.seen = []
        responses = iter(statuses)

        def handler(request):
            self.seen.append(request.headers["authorization"])
            status = next(responses)
            if status == 401:
                return httpx.Response(401, json={"message": "Bad credentials"})
            return ok({"updatePullRequestBranch": {"clientMutationId": None}})

        client = GitHubClient(provider, transport=httpx.MockTransport(handler))
        self.addAsyncCleanup(client.aclose)
        return client

    async def test_token_read_at_construction(self):
        self.make([200])
        self.assertEqual(self.provided, ["old"])

    async def test_rejected_token_is_reread_once(self):
        client = self.make([401, 200, 200])
        await client.update_branch("PR_1")
        self.assertEqual(self.seen, ["Bearer old", "Bearer new"])
        await client.update_branch("PR_1")
        self.assertEqual(self.seen[-1], "Bearer new")
        self.assertEqual(self.provided, ["old", "new"])

    async def test_still_rejected_after_reread(self):
        client = self.make([401, 401])
        with self.assertRaises(AuthError):
            await client.update_branch("PR_1")
        self.assertEqual(self.seen, ["Bearer old", "Bearer new"])

    async def test_provider_failure_propagates(self):
        def provider():
            if calls:
                raise AuthError("not logged in")
            calls.append(1)
            return "old"

        calls = []
        client = GitHubClient(
            provider,
            transport=httpx.MockTransport(lambda r: httpx.Response(401, json={})),
        )
        self.addAsyncCleanup(client.aclose)
        with self.assertRaisesRegex(AuthError, "not logged in"):
            await client.update_branch("PR_1")


class GetTokenTest(unittest.TestCase):
    def test_returns_token(self):
        done = subprocess.CompletedProcess([], 0, stdout="gho_abc\n", stderr="")
        with mock.patch("pr_mon.github.subprocess.run", return_value=done) as run:
            self.assertEqual(get_token(), "gho_abc")
        self.assertEqual(run.call_args.args[0], ["gh", "auth", "token"])

    def test_gh_missing(self):
        with (
            mock.patch("pr_mon.github.subprocess.run", side_effect=FileNotFoundError),
            self.assertRaisesRegex(AuthError, "gh"),
        ):
            get_token()

    def test_not_logged_in(self):
        done = subprocess.CompletedProcess([], 1, stdout="", stderr="not logged in")
        with (
            mock.patch("pr_mon.github.subprocess.run", return_value=done),
            self.assertRaisesRegex(AuthError, "not logged in"),
        ):
            get_token()

    def test_empty_token(self):
        done = subprocess.CompletedProcess([], 0, stdout="\n", stderr="")
        with (
            mock.patch("pr_mon.github.subprocess.run", return_value=done),
            self.assertRaises(AuthError),
        ):
            get_token()


if __name__ == "__main__":
    unittest.main()
