import unittest

from pr_mon.backend import BackendEvent, BackendStatus
from pr_mon.config import Config, NotifyConfig
from pr_mon.models import Action, MergeMethod
from pr_mon.protocol import (
    ProtocolError,
    action_from_dict,
    action_to_dict,
    config_from_dict,
    config_to_dict,
    decode,
    encode,
    event_message,
    settings_from_dict,
    settings_to_dict,
    snapshot,
    status_from_dict,
    status_to_dict,
)
from tests.fakes import make_repo


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
        self.assertEqual(data["status"]["pid"], 3)

    def test_repo_event(self):
        message = event_message(self.backend, BackendEvent("repo", name="acme/web"))
        self.assertEqual(
            message,
            {
                "event": "repo",
                "data": {"name": "acme/web", "repo": None, "error": "down", "unseen": []},
            },
        )
        data = event_message(self.backend, BackendEvent("repo", name="acme/api"))["data"]
        self.assertEqual(data["repo"]["name"], "acme/api")
        self.assertEqual(data["unseen"], [1, 2])

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
            {"config", "repos", "errors", "unseen", "collapsed", "status"},
        )


if __name__ == "__main__":
    unittest.main()
