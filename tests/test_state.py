import os
import tempfile
import unittest
from pathlib import Path
from unittest import mock

from pr_mon.state import default_state_path, load_state, save_state


class StateTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.path = Path(self.tmp.name) / "nested" / "state.json"

    def tearDown(self):
        self.tmp.cleanup()

    def test_missing_file(self):
        self.assertEqual(load_state(self.path), ({}, None))

    def test_round_trip(self):
        data = {"acme/api": {"12": {"status": "READY", "seen": False}}}
        save_state(self.path, data)
        self.assertEqual(load_state(self.path), (data, None))
        self.assertEqual(list(self.path.parent.iterdir()), [self.path])

    def test_corrupt_file_warns(self):
        self.path.parent.mkdir(parents=True)
        self.path.write_text("{nope")
        data, warning = load_state(self.path)
        self.assertEqual(data, {})
        self.assertIn(str(self.path), warning)

    def test_non_object_warns(self):
        self.path.parent.mkdir(parents=True)
        self.path.write_text("[1, 2]")
        data, warning = load_state(self.path)
        self.assertEqual(data, {})
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
