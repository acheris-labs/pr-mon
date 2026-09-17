import os
import plistlib
import subprocess
import tempfile
import unittest
from pathlib import Path
from unittest import mock

from pr_mon import autostart
from pr_mon.autostart import (
    LABEL,
    AutostartError,
    LaunchAgent,
    daemon_program,
    describe,
    disable,
    enable,
    launch_environment,
    restart_backend,
    wait_until_ready,
)
from pr_mon.backend import BackendError
from pr_mon.daemon import DaemonError, DaemonPaths


class FakeLaunchctl:
    """Stands in for `launchctl`, tracking whether the agent is loaded."""

    def __init__(self):
        self.loaded = False
        self.calls = []
        self.fail = {}

    def __call__(self, argv, capture_output, text):
        self.calls.append(argv[1:])
        command = argv[1]
        if command in self.fail:
            return subprocess.CompletedProcess(argv, 5, "", self.fail[command])
        if command == "print":
            return subprocess.CompletedProcess(argv, 0 if self.loaded else 113, "", "")
        if command == "bootstrap":
            self.loaded = True
        elif command == "bootout":
            self.loaded = False
        return subprocess.CompletedProcess(argv, 0, "", "")

    def commands(self):
        return [call[0] for call in self.calls if call[0] != "print"]


class AutostartTestCase(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.dir = Path(self.tmp.name)
        self.launchctl = FakeLaunchctl()
        self.agent = LaunchAgent(self.dir / "LaunchAgents", runner=self.launchctl, uid=501)
        self.paths = DaemonPaths(self.dir / "state")
        platform = mock.patch("pr_mon.autostart.sys.platform", "darwin")
        platform.start()
        self.addCleanup(platform.stop)

    def plist(self):
        return plistlib.loads(self.agent.plist_path.read_bytes())


class LaunchAgentTest(AutostartTestCase):
    def test_install_writes_agent_and_bootstraps(self):
        self.agent.install(["/bin/pr-mon", "daemon"], {"PATH": "/opt/bin:/bin"}, Path("/l.log"))
        self.assertEqual(self.agent.plist_path.name, f"{LABEL}.plist")
        self.assertEqual(
            self.plist(),
            {
                "Label": LABEL,
                "ProgramArguments": ["/bin/pr-mon", "daemon"],
                "RunAtLoad": True,
                "KeepAlive": {"SuccessfulExit": False},
                "EnvironmentVariables": {"PATH": "/opt/bin:/bin"},
                "StandardOutPath": "/l.log",
                "StandardErrorPath": "/l.log",
            },
        )
        self.assertEqual(
            self.launchctl.calls[-1], ["bootstrap", "gui/501", str(self.agent.plist_path)]
        )
        self.assertTrue(self.agent.is_loaded())

    def test_reinstall_boots_out_first(self):
        self.agent.install(["a"], {}, Path("/l"))
        self.agent.install(["b"], {}, Path("/l"))
        self.assertEqual(self.launchctl.commands(), ["bootstrap", "bootout", "bootstrap"])
        self.assertIn(["bootout", f"gui/501/{LABEL}"], self.launchctl.calls)
        self.assertEqual(self.plist()["ProgramArguments"], ["b"])

    def test_bootstrap_failure(self):
        self.launchctl.fail["bootstrap"] = "Bootstrap failed: 5: Input/output error"
        with self.assertRaisesRegex(AutostartError, "Input/output error"):
            self.agent.install(["a"], {}, Path("/l"))

    def test_uninstall(self):
        self.assertFalse(self.agent.uninstall())
        self.agent.install(["a"], {}, Path("/l"))
        self.assertTrue(self.agent.uninstall())
        self.assertFalse(self.agent.plist_path.exists())
        self.assertFalse(self.launchctl.loaded)

    def test_uninstall_file_only(self):
        self.agent.agents_dir.mkdir()
        self.agent.plist_path.write_bytes(b"junk")
        self.assertTrue(self.agent.uninstall())
        self.assertNotIn("bootout", self.launchctl.commands())

    def test_status(self):
        self.assertEqual(self.agent.status(), autostart.AutostartStatus(False, False, None))
        self.agent.install(["/bin/pr-mon", "daemon"], {}, Path("/l"))
        self.assertEqual(
            self.agent.status(),
            autostart.AutostartStatus(True, True, ["/bin/pr-mon", "daemon"]),
        )
        self.launchctl.loaded = False
        self.assertFalse(self.agent.status().loaded)
        self.agent.plist_path.write_bytes(b"not a plist")
        self.assertEqual(self.agent.status().program, None)


class RestartTest(AutostartTestCase):
    def test_kickstart_when_launchd_manages_it(self):
        self.agent.install(["/bin/pr-mon", "daemon"], {}, Path("/l"))
        infos = iter([None, {"pid": 10}, {"pid": 20}])
        with (
            mock.patch("pr_mon.autostart.daemon_status", return_value=10),
            mock.patch("pr_mon.autostart.daemon_info", side_effect=lambda p: next(infos)),
            mock.patch("pr_mon.autostart.time.sleep"),
            mock.patch("pr_mon.autostart.stop_daemon") as stop,
        ):
            self.assertEqual(restart_backend(self.paths, self.agent), 20)
        self.assertIn(["kickstart", "-k", f"gui/501/{LABEL}"], self.launchctl.calls)
        stop.assert_not_called()

    def test_stop_and_spawn_without_launchd(self):
        with (
            mock.patch("pr_mon.autostart.stop_daemon") as stop,
            mock.patch("pr_mon.autostart.spawn_daemon", return_value=7) as spawn,
        ):
            self.assertEqual(restart_backend(self.paths, self.agent), 7)
        stop.assert_called_once_with(self.paths)
        spawn.assert_called_once_with(self.paths)
        self.assertNotIn("kickstart", self.launchctl.commands())

    def test_stop_and_spawn_off_macos(self):
        self.launchctl.loaded = True
        with (
            mock.patch("pr_mon.autostart.sys.platform", "linux"),
            mock.patch("pr_mon.autostart.stop_daemon"),
            mock.patch("pr_mon.autostart.spawn_daemon", return_value=7),
        ):
            self.assertEqual(restart_backend(self.paths, self.agent), 7)

    def test_kickstart_failures(self):
        self.agent.install(["/bin/pr-mon", "daemon"], {}, Path("/l"))
        self.launchctl.fail["kickstart"] = "Could not find service"
        with (
            mock.patch("pr_mon.autostart.daemon_status", return_value=10),
            self.assertRaisesRegex(DaemonError, "Could not find service"),
        ):
            restart_backend(self.paths, self.agent)
        del self.launchctl.fail["kickstart"]
        with (
            mock.patch("pr_mon.autostart.daemon_status", return_value=10),
            mock.patch("pr_mon.autostart.daemon_info", return_value={"pid": 10}),
            mock.patch("pr_mon.autostart.time.sleep"),
            self.assertRaisesRegex(DaemonError, "did not restart"),
        ):
            restart_backend(self.paths, self.agent, timeout=0)


class DescribeTest(AutostartTestCase):
    def test_states(self):
        self.assertEqual(describe(self.agent), (False, "disabled"))
        self.agent.install(["/bin/pr-mon", "daemon"], {}, Path("/l"))
        self.assertEqual(
            describe(self.agent),
            (True, "enabled (loaded in launchd, runs `/bin/pr-mon daemon`)"),
        )
        self.launchctl.loaded = False
        enabled, text = describe(self.agent)
        self.assertTrue(enabled)
        self.assertIn("enabled but not loaded", text)
        self.assertIn("pr-mon autostart enable", text)

    def test_macos_only(self):
        with (
            mock.patch("pr_mon.autostart.sys.platform", "linux"),
            self.assertRaisesRegex(AutostartError, "only supported on macOS"),
        ):
            describe(self.agent)


class ProgramTest(unittest.TestCase):
    def test_uses_this_executable(self):
        with tempfile.TemporaryDirectory() as tmp:
            exe = Path(tmp) / "pr-mon"
            exe.write_text("")
            self.assertEqual(daemon_program(str(exe)), [str(exe), "daemon"])

    def test_falls_back_to_path_then_module(self):
        with mock.patch("pr_mon.autostart.shutil.which", return_value="/x/pr-mon"):
            self.assertEqual(daemon_program("/usr/lib/python/-m"), ["/x/pr-mon", "daemon"])
        with mock.patch("pr_mon.autostart.shutil.which", return_value=None):
            program = daemon_program("python")
        self.assertEqual(program[1:], ["-m", "pr_mon", "daemon"])

    def test_environment_keeps_path_and_xdg(self):
        env = {"PATH": "/opt/homebrew/bin:/bin", "XDG_STATE_HOME": "/s", "SECRET": "x"}
        with mock.patch.dict(os.environ, env, clear=True):
            self.assertEqual(
                launch_environment(), {"PATH": "/opt/homebrew/bin:/bin", "XDG_STATE_HOME": "/s"}
            )


def snapshot(repos=("a/b",), last_update=None, errors=None):
    return {
        "config": {"repos": list(repos)},
        "status": {"last_update": last_update},
        "errors": errors or {},
    }


class WaitTest(AutostartTestCase):
    def patch(self, info, snapshots):
        infos = iter(info)
        snaps = iter(snapshots)
        stack = [
            mock.patch("pr_mon.autostart.daemon_info", side_effect=lambda p: next(infos)),
            mock.patch("pr_mon.autostart.request", side_effect=lambda p, op: next(snaps)),
            mock.patch("pr_mon.autostart.time.sleep"),
        ]
        for patcher in stack:
            patcher.start()
            self.addCleanup(patcher.stop)

    def test_github_ok(self):
        self.patch([None, {"pid": 1}], [snapshot(), snapshot(last_update="2026-01-01")])
        self.assertEqual(wait_until_ready(self.paths), "backend running, GitHub access OK")

    def test_no_repos(self):
        self.patch([{"pid": 1}], [snapshot(repos=())])
        self.assertIn("no repos yet", wait_until_ready(self.paths))

    def test_github_error(self):
        self.patch([{"pid": 1}], [snapshot(errors={"a/b": "GitHub rejected the token"})])
        self.assertEqual(
            wait_until_ready(self.paths),
            "backend running, but GitHub reported for a/b: GitHub rejected the token",
        )

    def test_never_starts(self):
        self.paths.directory.mkdir(parents=True)
        self.paths.log.write_text("pr-mon: gh not found. Run `gh auth login`.")
        self.patch([None] * 1000, [])
        with self.assertRaisesRegex(AutostartError, "did not start under launchd(.|\n)*gh auth"):
            wait_until_ready(self.paths, timeout=0)

    def test_pending(self):
        self.patch([{"pid": 1}], [snapshot()] * 1000)
        with mock.patch("pr_mon.autostart.time.monotonic", side_effect=[0, 0, 100]):
            self.assertIn("still pending", wait_until_ready(self.paths, timeout=1))

    def test_stops_answering(self):
        def fail(paths, op):
            raise BackendError("gone")

        self.patch([{"pid": 1}], [])
        with (
            mock.patch("pr_mon.autostart.request", side_effect=fail),
            self.assertRaisesRegex(AutostartError, "stopped answering"),
        ):
            wait_until_ready(self.paths)


class EnableDisableTest(AutostartTestCase):
    def setUp(self):
        super().setUp()
        self.order = []
        self.stop = self.start_patch(
            "pr_mon.autostart.stop_daemon", side_effect=lambda p: self.order.append("stop")
        )
        self.wait = self.start_patch(
            "pr_mon.autostart.wait_until_ready", return_value="backend running, GitHub access OK"
        )

    def start_patch(self, target, **kwargs):
        patcher = mock.patch(target, **kwargs)
        self.addCleanup(patcher.stop)
        return patcher.start()

    def test_enable(self):
        with mock.patch.dict(os.environ, {"PATH": "/opt/homebrew/bin:/bin"}, clear=True):
            lines = enable(self.paths, self.agent, program=["/u/.local/bin/pr-mon", "daemon"])
        self.assertEqual(
            lines,
            [
                "autostart enabled: runs `/u/.local/bin/pr-mon daemon` at login",
                "backend running, GitHub access OK",
            ],
        )
        self.assertEqual(self.order, ["stop"])
        self.assertEqual(self.plist()["EnvironmentVariables"], {"PATH": "/opt/homebrew/bin:/bin"})
        self.assertEqual(self.plist()["StandardErrorPath"], str(self.paths.log))
        self.assertTrue(self.launchctl.loaded)
        self.wait.assert_called_once_with(self.paths, autostart.READY_TIMEOUT)

    def test_enable_from_checkout_warns(self):
        lines = enable(self.paths, self.agent, program=["/src/pr-mon/.venv/bin/pr-mon", "daemon"])
        self.assertIn("make tool-install", lines[0])

    def test_enable_rolls_back_when_backend_fails(self):
        self.wait.side_effect = AutostartError("did not start under launchd")
        with self.assertRaises(AutostartError):
            enable(self.paths, self.agent, program=["/bin/pr-mon", "daemon"])
        self.assertFalse(self.agent.plist_path.exists())
        self.assertFalse(self.launchctl.loaded)

    def test_disable_keeps_running_backend(self):
        enable(self.paths, self.agent, program=["/bin/pr-mon", "daemon"])
        with (
            mock.patch("pr_mon.autostart.daemon_status", return_value=42),
            mock.patch("pr_mon.autostart.spawn_daemon") as spawn,
        ):
            message = disable(self.paths, self.agent)
        self.assertEqual(message, "autostart disabled (the backend is still running)")
        spawn.assert_called_once_with(self.paths)
        self.assertFalse(self.agent.plist_path.exists())

    def test_disable_when_backend_stopped(self):
        enable(self.paths, self.agent, program=["/bin/pr-mon", "daemon"])
        with (
            mock.patch("pr_mon.autostart.daemon_status", return_value=None),
            mock.patch("pr_mon.autostart.spawn_daemon") as spawn,
        ):
            self.assertEqual(disable(self.paths, self.agent), "autostart disabled")
        spawn.assert_not_called()

    def test_disable_when_not_enabled(self):
        with mock.patch("pr_mon.autostart.daemon_status", return_value=None):
            self.assertEqual(disable(self.paths, self.agent), "autostart was not enabled")


if __name__ == "__main__":
    unittest.main()
