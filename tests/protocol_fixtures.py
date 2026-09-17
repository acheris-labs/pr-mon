"""Sample protocol messages shared with other clients' tests (protocol-fixtures/).

Regenerate with `make fixtures`; tests/test_protocol_fixtures.py fails when they're stale.
"""

import asyncio
import json
from dataclasses import replace
from pathlib import Path
from unittest import mock

from pr_mon.actions import with_actions
from pr_mon.backend import BackendEvent, BackendStatus
from pr_mon.config import Config, NotifyConfig
from pr_mon.daemon import DaemonPaths
from pr_mon.models import Action, ArmedMerge, MergeMethod, RepoInfo
from pr_mon.protocol import EVENT_KINDS, PROTOCOL_VERSION, action_to_dict, event_message, snapshot
from pr_mon.server import DaemonServer
from tests.fakes import make_repo
from tests.fixtures import auto_merge, check_run, status_context

DIRECTORY = Path(__file__).resolve().parent.parent / "protocol-fixtures"
UID = 501
ARMED = ArmedMerge(MergeMethod.SQUASH, True, "2026-09-17T10:00:00+00:00")
STARTED = "2026-09-15T01:30:00Z"


def retitle(repo: RepoInfo, titles: dict[int, str]) -> RepoInfo:
    prs = tuple(replace(pr, title=titles.get(pr.number, pr.title)) for pr in repo.prs)
    return replace(repo, prs=prs)


class FixtureBackend:
    """A backend in a representative state: every status, armed and native auto-merge."""

    def __init__(self):
        self.config = Config(
            repos=["acme/api", "acme/web", "Textualize/rich"],
            poll_interval=60,
            notifications={"acme/api": NotifyConfig(script_enabled=True, script="~/bin/im")},
        )
        api = make_repo(
            "acme/api",
            (1, {}),
            (
                2,
                {
                    "merge_state": "BLOCKED",
                    "check_state": "PENDING",
                    "checks": [check_run("ci", status="IN_PROGRESS", started_at=STARTED)],
                },
            ),
            (
                3,
                {
                    "merge_state": "BLOCKED",
                    "check_state": "FAILURE",
                    "checks": [
                        check_run("lint", conclusion="FAILURE", started_at=STARTED),
                        status_context("deploy"),
                    ],
                    "checks_total": 5,
                },
            ),
            (4, {"mergeable": "CONFLICTING", "merge_state": "DIRTY"}),
            (5, {"merge_state": "BEHIND", "review_decision": "REVIEW_REQUIRED"}),
            (6, {"is_draft": True, "merge_state": "DRAFT"}),
            (7, {"mergeable": "UNKNOWN", "merge_state": "UNKNOWN", "check_state": None}),
            (
                8,
                {
                    "merge_state": "BLOCKED",
                    "check_state": "PENDING",
                    "auto_merge": auto_merge("REBASE", "carol"),
                    "head_ref_id": None,
                },
            ),
            total=60,
        )
        api = retitle(api, {1: "Ready to go 🚀", 6: "WIP\u2028with a line separator"})
        rich = make_repo(
            "Textualize/rich",
            (41, {"merge_state": "BLOCKED", "check_state": "PENDING"}),
            (42, {"merge_state": "BLOCKED", "check_state": "PENDING"}),
            auto_merge_allowed=False,
            merge=False,
            rebase=False,
            delete_on_merge=True,
        )
        self.armed_merges = {"Textualize/rich": {42: ARMED}}
        self.repos = {repo.name: with_actions(repo, self.armed(repo.name)) for repo in (api, rich)}
        self.errors = {"acme/web": "Repository acme/web not found"}
        self.status = BackendStatus(
            pid=4242,
            version="0.0.0-fixture",
            notifier="/opt/homebrew/bin/terminal-notifier",
            last_update="2026-09-17T12:00:00+00:00",
            warnings=["Ignoring unreadable config: example"],
        )
        self.collapsed = {"textualize"}

    def unseen(self, name):
        return {5, 1} if name == "acme/api" else set()

    def armed(self, name):
        return self.armed_merges.get(name, {})


def build() -> dict[str, object]:
    """File name → JSON content."""
    backend = FixtureBackend()
    monitor = mock.Mock(status=backend.status, merging=False)
    hello = asyncio.run(DaemonServer(monitor, DIRECTORY)._hello())
    files: dict[str, object] = {
        "hello-response.json": {"id": 1, "ok": True, "result": hello},
        "snapshot-response.json": {"id": 2, "ok": True, "result": snapshot(backend)},
        "error-response.json": {"id": 3, "ok": False, "error": "acme/api#99 is not an open PR"},
    }
    for kind in EVENT_KINDS:
        event = BackendEvent(kind, name="acme/api", message="Merged acme/api#1 (squash)")
        files[f"event-{kind}.json"] = event_message(backend, event)
    files["requests.json"] = [
        {"id": 1, "op": "hello", "args": {}},
        {"id": 2, "op": "snapshot", "args": {}},
        {"id": 3, "op": "mark_seen", "args": {"name": "acme/api", "number": 1}},
        {"id": 4, "op": "refresh_all", "args": {}},
    ] + [
        {
            "id": 5 + i,
            "op": "perform",
            "args": {"repo": "acme/api", "number": 1, "action": action_to_dict(action)},
        }
        for i, action in enumerate(
            (
                Action("merge", MergeMethod.SQUASH, True),
                Action("arm_merge", MergeMethod.MERGE, False),
                Action("disarm_merge"),
                Action("update"),
            )
        )
    ]
    with mock.patch("os.getuid", return_value=UID):
        files["socket-paths.json"] = {
            "protocol": PROTOCOL_VERSION,
            "uid": UID,
            "cases": [
                {"state_directory": str(directory), "socket": str(DaemonPaths(directory).socket)}
                for directory in (
                    Path("/Users/alice/.local/state/pr-mon"),
                    Path("/Users/alice/" + "very-long-directory-name/" * 4 + "pr-mon"),
                )
            ],
        }
    return files


def render(content: object) -> str:
    return json.dumps(content, indent=2, ensure_ascii=False) + "\n"


def write() -> None:
    DIRECTORY.mkdir(exist_ok=True)
    expected = build()
    for stale in DIRECTORY.glob("*.json"):
        if stale.name not in expected:
            stale.unlink()
    for name, content in expected.items():
        (DIRECTORY / name).write_text(render(content))


if __name__ == "__main__":
    write()
