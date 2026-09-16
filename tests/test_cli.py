import unittest
from unittest import mock

from pr_mon import cli
from pr_mon.github import AuthError


class CliTest(unittest.TestCase):
    def test_auth_failure_exits_with_hint(self):
        with (
            mock.patch("pr_mon.cli.get_token", side_effect=AuthError("not logged in")),
            self.assertRaises(SystemExit) as ctx,
        ):
            cli.main()
        self.assertEqual(ctx.exception.code, "pr-mon: not logged in. Run `gh auth login`.")

    def test_runs_app_with_default_paths(self):
        with (
            mock.patch("pr_mon.cli.get_token", return_value="tok"),
            mock.patch("pr_mon.cli.PrMonApp") as app_cls,
            mock.patch("pr_mon.cli.default_config_path", return_value="cfg"),
            mock.patch("pr_mon.cli.default_state_path", return_value="st"),
            mock.patch("pr_mon.cli.detect_desktop_notifier", return_value="/bin/tn"),
        ):
            cli.main()
        _, config_path, state_path = app_cls.call_args.args
        self.assertEqual((config_path, state_path), ("cfg", "st"))
        self.assertEqual(app_cls.call_args.kwargs, {"notifier": "/bin/tn"})
        app_cls.return_value.run.assert_called_once_with()


if __name__ == "__main__":
    unittest.main()
