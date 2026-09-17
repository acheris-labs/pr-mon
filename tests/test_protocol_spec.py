"""Keep docs/protocol.md in step with the code."""

import asyncio
import re
import unittest
from dataclasses import fields
from pathlib import Path
from unittest import mock

from pr_mon.backend import BackendEvent, BackendStatus
from pr_mon.config import EVENT_NAMES, Config, NotifyConfig
from pr_mon.models import Action, ArmedMerge, AutoMerge, Check, MergeMethod, Reason, Status
from pr_mon.protocol import (
    ACTION_KINDS,
    EVENT_KINDS,
    PROTOCOL_VERSION,
    event_message,
    repo_to_wire,
    snapshot,
)
from pr_mon.server import DaemonServer
from tests.fakes import make_repo
from tests.test_protocol import FakeBackend

SPEC = Path(__file__).resolve().parent.parent / "docs" / "protocol.md"
ROW_NAME = re.compile(r"^\| `([^`]+)` \|")


def table_names(text: str, heading: str, table: int = 0) -> list[str]:
    """Backticked first-column names of the `table`-th table under `heading`."""
    lines = text.split("\n")
    start = lines.index(heading) + 1
    tables: list[list[str]] = []
    in_table = False
    for line in lines[start:]:
        if line.startswith("#"):
            break
        if line.startswith("|"):
            if not in_table:
                tables.append([])
                in_table = True
            match = ROW_NAME.match(line)
            if match:
                tables[-1].append(match.group(1))
        else:
            in_table = False
    return tables[table]


class ProtocolSpecTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.text = SPEC.read_text()
        cls.backend = FakeBackend()

    def object_fields(self, name: str) -> list[str]:
        return table_names(self.text, f"### Object: `{name}`")

    def test_version(self):
        self.assertIn(f"Protocol version: **{PROTOCOL_VERSION}**", self.text)

    def test_ops(self):
        server = DaemonServer(mock.Mock(), SPEC)
        self.assertEqual(table_names(self.text, "## Requests"), list(server._ops))

    def test_events(self):
        documented = table_names(self.text, "## Events")
        self.assertEqual(documented, list(EVENT_KINDS))
        for kind in documented:
            with self.subTest(kind=kind):
                self.assertEqual(
                    event_message(self.backend, BackendEvent(kind, name="acme/api"))["event"], kind
                )

    def test_hello_fields(self):
        hello = asyncio.run(DaemonServer(mock.Mock(), SPEC)._hello())
        self.assertEqual(self.object_fields("Hello"), list(hello))
        self.assertEqual(hello["protocol"], PROTOCOL_VERSION)

    def test_snapshot_fields(self):
        self.assertEqual(self.object_fields("Snapshot"), list(snapshot(self.backend)))

    def test_dataclass_fields(self):
        for name, cls in (
            ("Status", BackendStatus),
            ("Config", Config),
            ("NotifyConfig", NotifyConfig),
            ("Check", Check),
            ("AutoMerge", AutoMerge),
            ("Reason", Reason),
            ("ArmedMerge", ArmedMerge),
            ("Action", Action),
        ):
            with self.subTest(name=name):
                self.assertEqual(self.object_fields(name), [f.name for f in fields(cls)])

    def test_repo_and_pr_fields(self):
        wire = repo_to_wire(make_repo("acme/api", (1, {})))
        self.assertEqual(self.object_fields("Repo"), list(wire))
        self.assertEqual(self.object_fields("PullRequest"), list(wire["prs"][0]))

    def test_action_kinds(self):
        self.assertEqual(
            table_names(self.text, "### Object: `Action`", table=1), list(ACTION_KINDS)
        )

    def test_enumerations(self):
        for values in (EVENT_NAMES, list(Status), list(MergeMethod)):
            with self.subTest(values=values):
                self.assertIn(", ".join(f"`{v}`" for v in values), self.text)


if __name__ == "__main__":
    unittest.main()
