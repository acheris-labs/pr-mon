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
        self.assertEqual(Config().notifications, {})
        n = NotifyConfig()
        self.assertEqual(n.message, DEFAULT_MESSAGE)
        self.assertEqual(
            DEFAULT_MESSAGE, "{{PR_REPO}}#{{PR_NUM}} is {{PR_STATE}}: {{PR_TITLE}} {{PR_URL}}"
        )
        self.assertEqual(n.events, ["READY", "FAILING", "CONFLICT"])
        self.assertFalse(n.include_drafts or n.script_enabled or n.desktop_enabled)
        self.assertEqual(n.script, "")

    def test_file_without_section(self):
        self.write('poll_interval = 30\nrepos = ["acme/api"]\n')
        config, warning = load_config(self.path)
        self.assertIsNone(warning)
        self.assertEqual(config.notifications, {})
        self.assertEqual(config.repos, ["acme/api"])

    def test_round_trip_per_repo(self):
        api = NotifyConfig(
            message='Line "one"\n{{PR_URL}} \\ done 🚀 café \x7f\t',
            events=["BEHIND", "NEW"],
            include_drafts=True,
            script_enabled=True,
            script="~/bin/send 'two words'",
            desktop_enabled=True,
        )
        web = NotifyConfig(events=[], desktop_enabled=True)
        config = Config(
            repos=["acme/api", "Acme.Org/web-app"],
            notifications={"acme/api": api, "Acme.Org/web-app": web},
        )
        save_config(self.path, config)
        loaded, warning = load_config(self.path)
        self.assertIsNone(warning)
        self.assertEqual(loaded, config)
        self.assertIn('[notifications."acme/api"]', self.path.read_text())

    def test_settings_for_unmonitored_repos_are_not_saved(self):
        config = Config(repos=["acme/api"], notifications={"gone/repo": NotifyConfig()})
        save_config(self.path, config)
        self.assertEqual(load_config(self.path)[0].notifications, {})

    def test_partial_repo_section_uses_defaults(self):
        self.write(
            'repos = ["acme/api"]\n[notifications."acme/api"]\n'
            'script_enabled = true\nscript = "im"\n'
        )
        config, warning = load_config(self.path)
        self.assertIsNone(warning)
        self.assertEqual(
            config.notifications, {"acme/api": NotifyConfig(script_enabled=True, script="im")}
        )

    def test_invalid_repo_section_is_skipped(self):
        for body in (
            'events = "READY"',
            'events = ["READY", "EXPLODED"]',
            "desktop_enabled = 1",
            "message = 5",
            "include_drafts = []",
        ):
            with self.subTest(body=body):
                self.write(
                    'poll_interval = 30\nrepos = ["acme/api", "acme/web"]\n'
                    f'[notifications."acme/api"]\n{body}\n'
                    '[notifications."acme/web"]\ndesktop_enabled = true\n'
                )
                config, warning = load_config(self.path)
                self.assertIn("acme/api", warning)
                self.assertEqual(
                    config.notifications, {"acme/web": NotifyConfig(desktop_enabled=True)}
                )
                self.assertEqual(
                    (config.repos, config.poll_interval), (["acme/api", "acme/web"], 30)
                )

    def test_non_table_repo_entry_is_skipped(self):
        self.write('repos = ["acme/api"]\n[notifications]\n"acme/api" = "loud"\n')
        config, warning = load_config(self.path)
        self.assertIn("acme/api", warning)
        self.assertEqual(config.notifications, {})
        self.assertEqual(config.repos, ["acme/api"])

    def test_section_not_a_table(self):
        self.write('repos = []\nnotifications = "loud"\n')
        config, warning = load_config(self.path)
        self.assertIsNotNone(warning)
        self.assertEqual(config.notifications, {})


if __name__ == "__main__":
    unittest.main()
