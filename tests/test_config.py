import os
import tempfile
import unittest
from pathlib import Path
from unittest import mock

from pr_mon.config import (
    DEFAULT_MESSAGE,
    Config,
    NotifyConfig,
    default_config_path,
    load_config,
    save_config,
)


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


class NotifyConfigTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.path = Path(self.tmp.name) / "config.toml"

    def write(self, text):
        self.path.write_text(text)

    def test_defaults(self):
        n = Config().notifications
        self.assertEqual(n.message, DEFAULT_MESSAGE)
        self.assertEqual(
            DEFAULT_MESSAGE, "{{PR_REPO}}#{{PR_NUM}} is {{PR_STATE}}: {{PR_TITLE}} {{PR_URL}}"
        )
        self.assertEqual(n.events, ["READY", "FAILING", "CONFLICT"])
        self.assertFalse(n.include_drafts or n.script_enabled or n.desktop_enabled)
        self.assertEqual(n.script, "")

    def test_old_file_without_section_gets_defaults(self):
        self.write('poll_interval = 30\nrepos = ["acme/api"]\n')
        config, warning = load_config(self.path)
        self.assertIsNone(warning)
        self.assertEqual(config.notifications, NotifyConfig())
        self.assertEqual(config.repos, ["acme/api"])

    def test_round_trip(self):
        notifications = NotifyConfig(
            message='Line "one"\n{{PR_URL}} \\ done 🚀 café \x7f\t',
            events=["BEHIND", "NEW"],
            include_drafts=True,
            script_enabled=True,
            script="~/bin/send 'two words'",
            desktop_enabled=True,
        )
        save_config(self.path, Config(repos=["acme/api"], notifications=notifications))
        config, warning = load_config(self.path)
        self.assertIsNone(warning)
        self.assertEqual(config, Config(repos=["acme/api"], notifications=notifications))

    def test_partial_section_uses_defaults(self):
        self.write('repos = []\n[notifications]\nscript_enabled = true\nscript = "im"\n')
        config, warning = load_config(self.path)
        self.assertIsNone(warning)
        self.assertEqual(config.notifications, NotifyConfig(script_enabled=True, script="im"))

    def test_invalid_section_keeps_repos(self):
        for body in (
            'events = "READY"',
            'events = ["READY", "EXPLODED"]',
            "desktop_enabled = 1",
            "message = 5",
            "include_drafts = []",
        ):
            with self.subTest(body=body):
                self.write(f'poll_interval = 30\nrepos = ["acme/api"]\n[notifications]\n{body}\n')
                config, warning = load_config(self.path)
                self.assertIn("notifications", warning)
                self.assertEqual(config.notifications, NotifyConfig())
                self.assertEqual((config.repos, config.poll_interval), (["acme/api"], 30))

    def test_section_not_a_table(self):
        self.write('repos = []\nnotifications = "loud"\n')
        config, warning = load_config(self.path)
        self.assertIsNotNone(warning)
        self.assertEqual(config.notifications, NotifyConfig())


if __name__ == "__main__":
    unittest.main()
