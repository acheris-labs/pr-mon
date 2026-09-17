import io
import unittest
from contextlib import redirect_stderr, redirect_stdout
from pathlib import Path
from unittest import mock

from pr_mon import __version__, cli
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
    def test_runs_daemon(self):
        with (
            mock.patch("pr_mon.cli.GitHubClient") as client_cls,
            mock.patch("pr_mon.cli.run_daemon", new=mock.MagicMock()) as run,
            mock.patch("pr_mon.cli.asyncio.run") as asyncio_run,
            mock.patch("pr_mon.cli.detect_desktop_notifier", return_value="/bin/tn"),
            mock.patch("pr_mon.cli.default_config_path", return_value="cfg"),
            mock.patch("pr_mon.cli.default_state_path", return_value="st"),
        ):
            code, _, _ = self.run_cli("daemon")
        self.assertEqual(code, 0)
        client_cls.assert_called_once_with(cli.get_token)
        run.assert_called_once_with(
            PATHS,
            client_cls.return_value,
            "cfg",
            "st",
            notifier="/bin/tn",
            version=__version__,
            log_to_stderr=True,
        )
        asyncio_run.assert_called_once_with(run.return_value)

    def test_auth_failure(self):
        with mock.patch("pr_mon.cli.get_token", side_effect=AuthError("not logged in")):
            code, _, _ = self.run_cli("daemon")
        self.assertEqual(code, "pr-mon: not logged in. Run `gh auth login`.")

    def test_already_running(self):
        with (
            mock.patch("pr_mon.cli.GitHubClient"),
            mock.patch("pr_mon.cli.run_daemon", new=mock.MagicMock()),
            mock.patch("pr_mon.cli.asyncio.run", side_effect=AlreadyRunning(42)),
        ):
            code, _, _ = self.run_cli("daemon")
        self.assertEqual(code, "pr-mon: backend already running (pid 42)")


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


if __name__ == "__main__":
    unittest.main()
