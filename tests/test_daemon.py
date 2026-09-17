import json
import os
import signal
import subprocess
import sys
import tempfile
import time
import unittest
from pathlib import Path
from unittest import mock

from pr_mon.backend import BackendError
from pr_mon.config import Config, save_config
from pr_mon.daemon import (
    DaemonError,
    DaemonPaths,
    daemon_info,
    daemon_status,
    request,
    spawn_daemon,
    stop_daemon,
    tail_log,
)

REPO_ROOT = Path(__file__).resolve().parent.parent


class DaemonTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(dir="/tmp")
        self.addCleanup(self.tmp.cleanup)
        self.dir = Path(self.tmp.name)
        self.paths = DaemonPaths(self.dir)
        save_config(self.dir / "config.toml", Config(repos=["acme/api"]))
        env = mock.patch.dict(os.environ, {"PYTHONPATH": str(REPO_ROOT)})
        env.start()
        self.addCleanup(env.stop)
        self.addCleanup(self.force_stop)

    def command(self):
        return (sys.executable, "-m", "tests.daemon_harness", str(self.dir), "acme/api")

    def force_stop(self):
        pid = daemon_status(self.paths)
        if pid:
            os.kill(pid, signal.SIGKILL)

    def spawn(self):
        return spawn_daemon(self.paths, command=self.command(), timeout=10)

    def test_not_running(self):
        self.assertIsNone(daemon_status(self.paths))
        self.assertIsNone(daemon_info(self.paths))
        self.assertFalse(stop_daemon(self.paths))
        self.paths.lock.write_text("12345")
        self.assertIsNone(daemon_status(self.paths))

    def test_spawn_status_stop(self):
        pid = self.spawn()
        self.assertEqual(daemon_status(self.paths), pid)
        info = daemon_info(self.paths)
        self.assertEqual((info["version"], info["pid"]), ("test", pid))
        self.assertEqual(self.spawn(), pid)
        self.assertTrue(stop_daemon(self.paths))
        self.assertIsNone(daemon_status(self.paths))
        self.assertFalse(self.paths.socket.exists())
        state = json.loads((self.dir / "state.json").read_text())
        self.assertIn("acme/api", state["prs"])
        self.assertIn("started", self.paths.log.read_text())

    def test_second_daemon_refuses(self):
        pid = self.spawn()
        result = subprocess.run(self.command(), capture_output=True, text=True, timeout=10)
        self.assertEqual(result.returncode, 3)
        self.assertIn(f"already running (pid {pid})", result.stderr)
        self.assertEqual(daemon_status(self.paths), pid)

    def test_stale_socket_is_replaced(self):
        self.paths.socket.write_text("stale")
        pid = self.spawn()
        self.assertEqual(daemon_info(self.paths)["pid"], pid)

    def test_sigterm_cleans_up(self):
        pid = self.spawn()
        os.kill(pid, signal.SIGTERM)
        deadline = time.monotonic() + 5
        while daemon_status(self.paths) is not None and time.monotonic() < deadline:
            time.sleep(0.05)
        self.assertIsNone(daemon_status(self.paths))
        self.assertFalse(self.paths.socket.exists())
        self.assertEqual(self.paths.lock.read_text(), "")

    def test_request_errors(self):
        self.spawn()
        with self.assertRaisesRegex(BackendError, "unknown op"):
            request(self.paths, "explode")
        self.assertEqual(request(self.paths, "hello")["version"], "test")

    def test_spawn_failure_reports_log(self):
        with self.assertRaisesRegex(DaemonError, "exited with code 4(.|\n)*boom"):
            spawn_daemon(
                self.paths,
                command=(sys.executable, "-c", "import sys; print('boom'); sys.exit(4)"),
            )

    def test_spawn_timeout_kills_process(self):
        marker = self.dir / "survived"
        script = f"import pathlib, time; time.sleep(1); pathlib.Path({str(marker)!r}).touch()"
        with self.assertRaisesRegex(DaemonError, "did not start within 0.3s"):
            spawn_daemon(self.paths, command=(sys.executable, "-c", script), timeout=0.3)
        time.sleep(1.5)
        self.assertFalse(marker.exists())

    def test_tail_log(self):
        self.assertEqual(tail_log(self.paths), "")
        self.paths.log.write_text("\n".join(str(i) for i in range(20)))
        self.assertEqual(tail_log(self.paths, lines=2), "18\n19")


if __name__ == "__main__":
    unittest.main()
