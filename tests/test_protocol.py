import json
import unittest

from pr_mon.backend import BackendEvent, BackendStatus
from pr_mon.config import Config, NotifyConfig
from pr_mon.models import Action, ArmedMerge, MergeMethod, Status
from pr_mon.protocol import (
    ProtocolError,
    action_from_dict,
    action_to_dict,
    config_from_dict,
    config_to_dict,
    decode,
    encode,
    event_message,
    repo_from_wire,
    repo_to_wire,
    settings_from_dict,
    settings_to_dict,
    snapshot,
    status_from_dict,
    status_to_dict,
)
from tests.fakes import make_repo
from tests.fixtures import check_run

STARTED = "2026-09-17T10:00:00Z"


class FramingTest(unittest.TestCase):
    def test_round_trip(self):
        message = {"id": 1, "op": "hello", "args": {"text": "café 🚀\nnext"}}
        line = encode(message)
        self.assertTrue(line.endswith(b"\n"))
        self.assertEqual(line.count(b"\n"), 1)
        self.assertEqual(decode(line), message)

    def test_bad_lines(self):
        for line in (b"{nope\n", b"[1, 2]\n", b"\xff\n", b"\n"):
            with self.subTest(line=line), self.assertRaises(ProtocolError):
                decode(line)


class ConversionTest(unittest.TestCase):
    def test_settings(self):
        settings = NotifyConfig(message="x", events=["NEW"], script_enabled=True, script="im")
        self.assertEqual(settings_from_dict(settings_to_dict(settings)), settings)
        with self.assertRaises(ProtocolError):
            settings_from_dict({"events": ["BOGUS"]})
        with self.assertRaises(ProtocolError):
            settings_from_dict("nope")

    def test_action(self):
        for action in (
            Action("arm_merge", MergeMethod.MERGE, True),
            Action("disarm_merge"),
            Action("merge", MergeMethod.REBASE, True),
            Action("update"),
            Action("auto_merge_off"),
        ):
            with self.subTest(action=action):
                restored = action_from_dict(action_to_dict(action))
                self.assertEqual(restored, action)
        self.assertIsInstance(
            action_from_dict({"kind": "merge", "method": "SQUASH", "delete_branch": False}).method,
            MergeMethod,
        )
        with self.assertRaises(ProtocolError):
            action_from_dict({"kind": "explode"})

    def test_repo_wire_carries_computed_fields(self):
        repo = make_repo(
            "acme/api",
            (
                1,
                {
                    "check_state": "FAILURE",
                    "checks": [check_run("ci", conclusion="FAILURE", started_at=STARTED)],
                },
            ),
            (2, {}),
        )
        wire = repo_to_wire(repo)
        failing, ready = wire["prs"]
        self.assertEqual(failing["status"], "FAILING")
        self.assertEqual(failing["reasons"], [{"text": "Check failed: ci", "level": "error"}])
        self.assertFalse(failing["strictly_ready"])
        self.assertEqual(ready["status"], Status.READY)
        self.assertTrue(ready["strictly_ready"])
        self.assertEqual(failing["last_check_started_at"], STARTED)
        self.assertIsNone(ready["last_check_started_at"])
        self.assertEqual(repo_from_wire(json.loads(json.dumps(wire))), repo)

    def test_status(self):
        status = BackendStatus(
            connected=True, pid=7, version="1", notifier="/x", warnings=["careful"]
        )
        self.assertEqual(status_from_dict(status_to_dict(status)), status)

    def test_config(self):
        config = Config(
            repos=["a/b", "c/d"], poll_interval=30, notifications={"a/b": NotifyConfig()}
        )
        self.assertEqual(config_from_dict(config_to_dict(config)), config)


class FakeBackend:
    def __init__(self):
        self.config = Config(repos=["acme/api", "acme/web"])
        self.repos = {"acme/api": make_repo("acme/api", (1, {}), (2, {}))}
        self.errors = {"acme/web": "down"}
        self.status = BackendStatus(pid=3, version="9")
        self.collapsed = {"zeta", "acme"}

    def unseen(self, name):
        return {2, 1} if name == "acme/api" else set()

    def armed(self, name):
        if name != "acme/api":
            return {}
        return {2: ArmedMerge(MergeMethod.SQUASH, True, "2026-09-17T10:00:00+00:00")}


class MessageTest(unittest.TestCase):
    def setUp(self):
        self.backend = FakeBackend()

    def test_snapshot(self):
        data = snapshot(self.backend)
        self.assertEqual(data["config"]["repos"], ["acme/api", "acme/web"])
        self.assertEqual(list(data["repos"]), ["acme/api"])
        self.assertEqual(data["errors"], {"acme/web": "down"})
        self.assertEqual(data["unseen"], {"acme/api": [1, 2], "acme/web": []})
        self.assertEqual(data["collapsed"], ["acme", "zeta"])
        self.assertEqual(
            data["armed"],
            {
                "acme/api": {
                    "2": {
                        "method": "SQUASH",
                        "delete_branch": True,
                        "armed_at": "2026-09-17T10:00:00+00:00",
                    }
                },
                "acme/web": {},
            },
        )
        self.assertEqual(data["status"]["pid"], 3)

    def test_repo_event(self):
        message = event_message(self.backend, BackendEvent("repo", name="acme/web"))
        self.assertEqual(
            message,
            {
                "event": "repo",
                "data": {
                    "name": "acme/web",
                    "repo": None,
                    "error": "down",
                    "unseen": [],
                    "armed": {},
                },
            },
        )
        data = event_message(self.backend, BackendEvent("repo", name="acme/api"))["data"]
        self.assertEqual(data["repo"]["name"], "acme/api")
        self.assertEqual(data["unseen"], [1, 2])
        self.assertEqual(list(data["armed"]), ["2"])

    def test_other_events(self):
        self.assertEqual(
            event_message(self.backend, BackendEvent("seen", name="acme/api")),
            {"event": "seen", "data": {"name": "acme/api", "unseen": [1, 2]}},
        )
        self.assertEqual(
            event_message(self.backend, BackendEvent("collapsed")),
            {"event": "collapsed", "data": {"collapsed": ["acme", "zeta"]}},
        )
        self.assertEqual(
            event_message(self.backend, BackendEvent("toast", message="hi", severity="error")),
            {"event": "toast", "data": {"message": "hi", "severity": "error"}},
        )
        self.assertEqual(
            event_message(self.backend, BackendEvent("status"))["data"]["status"]["version"], "9"
        )
        self.assertEqual(
            event_message(self.backend, BackendEvent("config"))["data"]["config"]["repos"],
            ["acme/api", "acme/web"],
        )
        self.assertEqual(
            set(event_message(self.backend, BackendEvent("repos"))["data"]),
            {"config", "repos", "errors", "unseen", "collapsed", "armed", "status"},
        )


if __name__ == "__main__":
    unittest.main()
