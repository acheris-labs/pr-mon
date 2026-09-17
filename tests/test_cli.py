import io
import unittest
from contextlib import ExitStack, redirect_stderr, redirect_stdout
from pathlib import Path
from unittest import mock

from pr_mon import __version__, autostart, cli
from pr_mon.daemon import AlreadyRunning, DaemonError, DaemonPaths
from pr_mon.github import AuthError

PATHS = DaemonPaths(Path("/tmp/prmon-test"))


class CliTestCase(unittest.TestCase):
    def run_cli(self, *argv):
        out = io.StringIO()
        err = io.StringIO()
        code = 0
        with (
            mock.patch("pr_mon.cli.DaemonPaths.default", return_value=PATHS),
            redirect_stdout(out),
            redirect_stderr(err),
        ):
            try:
                cli.main(list(argv))
            except SystemExit as e:
                code = e.code
        return code, out.getvalue(), err.getvalue()


class TuiTest(CliTestCase):
    def test_default_runs_tui_against_daemon(self):
        with mock.patch("pr_mon.cli.PrMonApp") as app_cls:
            app_cls.return_value.return_code = 0
            code, _, _ = self.run_cli()
        self.assertEqual(code, 0)
        backend = app_cls.call_args.args[0]
        self.assertEqual(backend.socket_path, PATHS.socket)
        self.assertEqual(app_cls.call_args.kwargs, {"daemon": PATHS})
        app_cls.return_value.run.assert_called_once_with()

    def test_tui_exit_code_propagates(self):
        with mock.patch("pr_mon.cli.PrMonApp") as app_cls:
            app_cls.return_value.return_code = 1
            code, _, _ = self.run_cli("tui")
        self.assertEqual(code, 1)

    def test_version(self):
        code, out, _ = self.run_cli("--version")
        self.assertEqual((code, out.strip()), (0, f"pr-mon {__version__}"))


class DaemonCommandTest(CliTestCase):
    def daemon_patches(self, stack):
        self.client_cls = stack.enter_context(mock.patch("pr_mon.cli.GitHubClient"))
        self.run = stack.enter_context(mock.patch("pr_mon.cli.run_daemon", new=mock.MagicMock()))
        self.asyncio_run = stack.enter_context(mock.patch("pr_mon.cli.asyncio.run"))
        for name, value in (
            ("detect_desktop_notifier", "/bin/tn"),
            ("default_config_path", "cfg"),
            ("default_state_path", "st"),
        ):
            stack.enter_context(mock.patch(f"pr_mon.cli.{name}", return_value=value))

    def test_runs_daemon(self):
        with ExitStack() as stack:
            self.daemon_patches(stack)
            code, _, _ = self.run_cli("daemon")
        self.assertEqual(code, 0)
        self.client_cls.assert_called_once_with(cli.get_token)
        self.run.assert_called_once_with(
            PATHS,
            self.client_cls.return_value,
            "cfg",
            "st",
            notifier="/bin/tn",
            version=__version__,
            log_to_stderr=False,
        )
        self.asyncio_run.assert_called_once_with(self.run.return_value)

    def test_daemon_logs_to_a_terminal(self):
        terminal = mock.Mock()
        terminal.isatty.return_value = True
        with ExitStack() as stack:
            self.daemon_patches(stack)
            stack.enter_context(mock.patch("pr_mon.cli.DaemonPaths.default", return_value=PATHS))
            stack.enter_context(mock.patch("sys.stderr", terminal))
            cli.main(["daemon"])
        self.assertTrue(self.run.call_args.kwargs["log_to_stderr"])

    def test_auth_failure(self):
        with mock.patch("pr_mon.cli.get_token", side_effect=AuthError("not logged in")):
            code, _, _ = self.run_cli("daemon")
        self.assertEqual(code, "pr-mon: not logged in. Run `gh auth login`.")

    def test_already_running_is_not_an_error(self):
        with (
            mock.patch("pr_mon.cli.GitHubClient"),
            mock.patch("pr_mon.cli.run_daemon", new=mock.MagicMock()),
            mock.patch("pr_mon.cli.asyncio.run", side_effect=AlreadyRunning(42)),
        ):
            code, _, err = self.run_cli("daemon")
        self.assertEqual(code, 0)
        self.assertEqual(err, "pr-mon: backend already running (pid 42)\n")

    def test_daemon_error(self):
        with (
            mock.patch("pr_mon.cli.GitHubClient"),
            mock.patch("pr_mon.cli.run_daemon", new=mock.MagicMock()),
            mock.patch("pr_mon.cli.asyncio.run", side_effect=DaemonError("not private")),
        ):
            code, _, _ = self.run_cli("daemon")
        self.assertEqual(code, "pr-mon: not private")


class ControlCommandTest(CliTestCase):
    def test_start(self):
        with mock.patch("pr_mon.cli.spawn_daemon", return_value=77) as spawn:
            code, out, _ = self.run_cli("start")
        spawn.assert_called_once_with(PATHS)
        self.assertEqual((code, out), (0, "backend running (pid 77)\n"))

    def test_start_failure(self):
        with mock.patch("pr_mon.cli.spawn_daemon", side_effect=DaemonError("exited\nlog line")):
            code, _, _ = self.run_cli("start")
        self.assertEqual(code, "pr-mon: exited\nlog line")

    def test_stop(self):
        for stopped, message in ((True, "backend stopped\n"), (False, "backend not running\n")):
            with (
                self.subTest(stopped=stopped),
                mock.patch("pr_mon.cli.stop_daemon", return_value=stopped),
            ):
                code, out, _ = self.run_cli("stop")
                self.assertEqual((code, out), (0, message))

    def test_stop_failure(self):
        with mock.patch("pr_mon.cli.stop_daemon", side_effect=DaemonError("stuck")):
            code, _, _ = self.run_cli("stop")
        self.assertEqual(code, "pr-mon: stuck")

    def test_status(self):
        with mock.patch("pr_mon.cli.daemon_info", return_value={"pid": 5, "version": "1.0"}):
            code, out, _ = self.run_cli("status")
        self.assertEqual((code, out), (0, "backend running (pid 5, version 1.0)\n"))
        with (
            mock.patch("pr_mon.cli.daemon_info", return_value=None),
            mock.patch("pr_mon.cli.daemon_status", return_value=None),
        ):
            code, out, _ = self.run_cli("status")
        self.assertEqual((code, out), (1, "backend stopped\n"))
        with (
            mock.patch("pr_mon.cli.daemon_info", return_value=None),
            mock.patch("pr_mon.cli.daemon_status", return_value=9),
        ):
            code, out, _ = self.run_cli("status")
        self.assertEqual(code, 1)
        self.assertIn("not responding (pid 9)", out)


class AutostartCommandTest(CliTestCase):
    def test_enable(self):
        with mock.patch("pr_mon.cli.autostart.enable", return_value=["a", "b"]) as enable:
            code, out, _ = self.run_cli("autostart", "enable")
        enable.assert_called_once_with(PATHS)
        self.assertEqual((code, out), (0, "a\nb\n"))

    def test_enable_failure(self):
        error = autostart.AutostartError("the backend did not start under launchd")
        with mock.patch("pr_mon.cli.autostart.enable", side_effect=error):
            code, _, _ = self.run_cli("autostart", "enable")
        self.assertEqual(code, "pr-mon: the backend did not start under launchd")

    def test_disable(self):
        with mock.patch("pr_mon.cli.autostart.disable", return_value="autostart disabled") as off:
            code, out, _ = self.run_cli("autostart", "disable")
        off.assert_called_once_with(PATHS)
        self.assertEqual((code, out), (0, "autostart disabled\n"))

    def test_disable_spawn_failure(self):
        with mock.patch("pr_mon.cli.autostart.disable", side_effect=DaemonError("nope")):
            code, _, _ = self.run_cli("autostart", "disable")
        self.assertEqual(code, "pr-mon: nope")

    def test_status(self):
        with mock.patch("pr_mon.cli.autostart.describe", return_value=(True, "enabled (x)")):
            code, out, _ = self.run_cli("autostart", "status")
        self.assertEqual((code, out), (0, "autostart enabled (x)\n"))
        with mock.patch("pr_mon.cli.autostart.describe", return_value=(False, "disabled")):
            code, out, _ = self.run_cli("autostart", "status")
        self.assertEqual((code, out), (1, "autostart disabled\n"))

    def test_action_required(self):
        code, _, err = self.run_cli("autostart")
        self.assertEqual(code, 2)
        self.assertIn("action", err)


if __name__ == "__main__":
    unittest.main()
