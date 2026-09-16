import os
import tempfile
import unittest
from pathlib import Path
from unittest import mock

from pr_mon.config import Config, default_config_path, load_config, save_config


class ConfigTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.path = Path(self.tmp.name) / "nested" / "config.toml"

    def tearDown(self):
        self.tmp.cleanup()

    def test_missing_file_is_default_without_warning(self):
        config, warning = load_config(self.path)
        self.assertEqual(config, Config())
        self.assertIsNone(warning)

    def test_round_trip(self):
        save_config(self.path, Config(repos=["acme/api", "acme/web"], poll_interval=30))
        config, warning = load_config(self.path)
        self.assertEqual(config, Config(repos=["acme/api", "acme/web"], poll_interval=30))
        self.assertIsNone(warning)

    def test_special_characters_escaped(self):
        save_config(self.path, Config(repos=['we"ird\\name']))
        config, _ = load_config(self.path)
        self.assertEqual(config.repos, ['we"ird\\name'])

    def test_corrupt_file_warns(self):
        self.path.parent.mkdir(parents=True)
        self.path.write_text("repos = [")
        config, warning = load_config(self.path)
        self.assertEqual(config, Config())
        self.assertIn(str(self.path), warning)

    def test_wrong_types_warn(self):
        self.path.parent.mkdir(parents=True)
        self.path.write_text('repos = "acme/api"\npoll_interval = "soon"\n')
        config, warning = load_config(self.path)
        self.assertEqual(config, Config())
        self.assertIsNotNone(warning)

    def test_partial_file_uses_defaults(self):
        self.path.parent.mkdir(parents=True)
        self.path.write_text('repos = ["acme/api"]\n')
        config, warning = load_config(self.path)
        self.assertEqual(config, Config(repos=["acme/api"], poll_interval=60))
        self.assertIsNone(warning)

    def test_xdg_override(self):
        with mock.patch.dict(os.environ, {"XDG_CONFIG_HOME": "/x/cfg"}):
            self.assertEqual(default_config_path(), Path("/x/cfg/pr-mon/config.toml"))

    def test_default_path(self):
        env = {k: v for k, v in os.environ.items() if k != "XDG_CONFIG_HOME"}
        with mock.patch.dict(os.environ, env, clear=True):
            self.assertEqual(
                default_config_path(), Path.home() / ".config" / "pr-mon" / "config.toml"
            )


if __name__ == "__main__":
    unittest.main()
