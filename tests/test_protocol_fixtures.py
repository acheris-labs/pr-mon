import unittest
from unittest import mock

from pr_mon.models import repo_from_dict
from pr_mon.server import DaemonServer
from tests.protocol_fixtures import DIRECTORY, build, render


class ProtocolFixturesTest(unittest.TestCase):
    def test_fixtures_are_current(self):
        expected = build()
        self.assertEqual(
            sorted(path.name for path in DIRECTORY.glob("*.json")),
            sorted(expected),
            "run `make fixtures`",
        )
        for name, content in expected.items():
            with self.subTest(name=name):
                self.assertEqual(
                    (DIRECTORY / name).read_text(), render(content), "run `make fixtures`"
                )

    def test_requests_cover_every_op_but_shutdown(self):
        ops = {request["op"] for request in build()["requests.json"]}
        self.assertEqual(ops, set(DaemonServer(mock.Mock(), DIRECTORY)._ops) - {"shutdown"})

    def test_snapshot_covers_every_status_and_action(self):
        repos = build()["snapshot-response.json"]["result"]["repos"]
        prs = [pr for repo in repos.values() for pr in repo_from_dict(repo).prs]
        self.assertEqual(
            {str(pr.status) for pr in prs},
            {"READY", "PENDING", "FAILING", "CONFLICT", "BEHIND", "DRAFT", "CHECKING"},
        )
        kinds = {option.kind for pr in prs for option in pr.actions}
        self.assertEqual(
            kinds,
            {"merge", "update", "auto_merge_on", "auto_merge_off", "arm_merge", "disarm_merge"},
        )


if __name__ == "__main__":
    unittest.main()
