import json
import os
import tempfile
import unittest
from pathlib import Path
from unittest import mock

from pr_mon.state import AppState, default_state_path, load_state, save_state


class StateTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.path = Path(self.tmp.name) / "nested" / "state.json"

    def tearDown(self):
        self.tmp.cleanup()

    def test_missing_file(self):
        self.assertEqual(load_state(self.path), (AppState(), None))

    def test_round_trip(self):
        state = AppState(
            prs={"acme/api": {"12": {"status": "READY", "seen": False}}},
            collapsed=["acme", "cli"],
            armed={
                "acme/api": {
                    "12": {"method": "SQUASH", "delete_branch": True, "armed_at": "2026-09-17"}
                }
            },
        )
        save_state(self.path, state)
        self.assertEqual(load_state(self.path), (state, None))
        self.assertEqual(list(self.path.parent.iterdir()), [self.path])

    def test_bad_armed_is_ignored(self):
        self.path.parent.mkdir(parents=True)
        for armed in ("x", {"a/b": "x"}, {"a/b": {"1": "x"}}):
            with self.subTest(armed=armed):
                self.path.write_text(json.dumps({"prs": {}, "armed": armed}))
                self.assertEqual(load_state(self.path)[0].armed, {})

    def test_bad_collapsed_is_ignored(self):
        self.path.parent.mkdir(parents=True)
        self.path.write_text(json.dumps({"prs": {}, "collapsed": "acme"}))
        self.assertEqual(load_state(self.path)[0], AppState())

    def test_corrupt_file_warns(self):
        self.path.parent.mkdir(parents=True)
        self.path.write_text("{nope")
        state, warning = load_state(self.path)
        self.assertEqual(state, AppState())
        self.assertIn(str(self.path), warning)

    def test_non_object_warns(self):
        self.path.parent.mkdir(parents=True)
        self.path.write_text("[1, 2]")
        state, warning = load_state(self.path)
        self.assertEqual(state, AppState())
        self.assertIsNotNone(warning)

    def test_xdg_override(self):
        with mock.patch.dict(os.environ, {"XDG_STATE_HOME": "/x/state"}):
            self.assertEqual(default_state_path(), Path("/x/state/pr-mon/state.json"))

    def test_default_path(self):
        env = {k: v for k, v in os.environ.items() if k != "XDG_STATE_HOME"}
        with mock.patch.dict(os.environ, env, clear=True):
            self.assertEqual(
                default_state_path(), Path.home() / ".local" / "state" / "pr-mon" / "state.json"
            )


if __name__ == "__main__":
    unittest.main()
