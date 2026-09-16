import json
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
from tests.fixtures import raw_pr, raw_repo


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
        client = GitHubClient("tok", transport=httpx.MockTransport(recorder))
        self.addAsyncCleanup(client.aclose)
        return client, recorder

    async def test_fetch_repo(self):
        client, rec = self.client_for(ok({"repository": raw_repo([raw_pr(number=5)])}))
        repo = await client.fetch_repo("acme/api")
        self.assertEqual(repo.name, "acme/api")
        self.assertEqual([p.number for p in repo.prs], [5])
        request = rec.requests[0]
        self.assertEqual(str(request.url), "https://api.github.com/graphql")
        self.assertEqual(request.headers["authorization"], "Bearer tok")
        self.assertEqual(rec.body["variables"], {"owner": "acme", "name": "api"})
        self.assertIn("CREATED_AT", rec.body["query"])
        self.assertIn("first: 50", rec.body["query"])

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
        client, _ = self.client_for(httpx.Response(502, text="bad gateway"))
        with self.assertRaisesRegex(GitHubError, "502"):
            await client.fetch_repo("acme/api")

    async def test_transport_error(self):
        def fail(request):
            raise httpx.ConnectError("no network")

        client = GitHubClient("tok", transport=httpx.MockTransport(fail))
        self.addAsyncCleanup(client.aclose)
        with self.assertRaisesRegex(GitHubError, "no network"):
            await client.fetch_repo("acme/api")

    async def test_merge(self):
        client, rec = self.client_for(ok({"mergePullRequest": {"clientMutationId": None}}))
        await client.merge("PR_1", MergeMethod.SQUASH)
        self.assertIn("mergePullRequest", rec.body["query"])
        self.assertEqual(rec.body["variables"], {"id": "PR_1", "method": "SQUASH"})

    async def test_merge_error_surfaces_message(self):
        client, _ = self.client_for(gql_error("UNPROCESSABLE", "Base branch was modified"))
        with self.assertRaisesRegex(GitHubError, "Base branch was modified"):
            await client.merge("PR_1", MergeMethod.MERGE)

    async def test_update_branch(self):
        client, rec = self.client_for(ok({"updatePullRequestBranch": {"clientMutationId": None}}))
        await client.update_branch("PR_1")
        self.assertIn("updatePullRequestBranch", rec.body["query"])
        self.assertEqual(rec.body["variables"], {"id": "PR_1"})

    async def test_delete_branch(self):
        client, rec = self.client_for(ok({"deleteRef": {"clientMutationId": None}}))
        await client.delete_branch("REF_1")
        self.assertIn("deleteRef", rec.body["query"])
        self.assertEqual(rec.body["variables"], {"id": "REF_1"})


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
