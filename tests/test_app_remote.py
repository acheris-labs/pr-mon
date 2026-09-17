import asyncio
import tempfile
import unittest
from pathlib import Path
from unittest import mock

from textual.widgets import Checkbox, DataTable, Input

from pr_mon import __version__
from pr_mon.app import PrMonApp
from pr_mon.config import Config, load_config, save_config
from pr_mon.daemon import DaemonError, DaemonPaths
from pr_mon.models import MergeMethod
from pr_mon.remote import RemoteBackend
from pr_mon.screens import ActionMenuScreen, ConfirmScreen, NotificationsScreen
from pr_mon.server import DaemonServer
from pr_mon.service import Monitor
from tests.fakes import FakeClient, FakeDeliver, make_repo

READY = {}
SIZE = (120, 36)


class RemoteAppTestCase(unittest.IsolatedAsyncioTestCase):
    """The TUI talks to a real DaemonServer over a socket; spawn/stop are faked in-loop."""

    async def asyncSetUp(self):
        self.tmp = tempfile.TemporaryDirectory(dir="/tmp")
        self.addCleanup(self.tmp.cleanup)
        self.dir = Path(self.tmp.name)
        self.paths = DaemonPaths(self.dir)
        self.config_path = self.dir / "config.toml"
        self.loop = asyncio.get_running_loop()
        self.server = None
        self.monitor = None
        self.version = __version__
        self.spawns = 0
        self.spawn_error = None
        self.client = FakeClient({})
        for name, fake in (("spawn_daemon", self.fake_spawn), ("stop_daemon", self.fake_stop)):
            patcher = mock.patch(f"pr_mon.app.{name}", fake)
            patcher.start()
            self.addCleanup(patcher.stop)
        self.deliver = FakeDeliver()
        patcher = mock.patch("pr_mon.service.deliver", self.deliver)
        patcher.start()
        self.addCleanup(patcher.stop)
        self.addAsyncCleanup(self.stop_server)

    def write_config(self, repos):
        save_config(self.config_path, Config(repos=list(repos)))
        self.client.repos.update(repos)

    async def start_server(self):
        self.monitor = Monitor(
            self.client, self.config_path, self.dir / "state.json", version=self.version
        )
        await self.monitor.start()
        await self.monitor.wait_idle()
        self.server = DaemonServer(self.monitor, self.paths.socket)
        await self.server.start()
        self.watcher = asyncio.create_task(self.close_on_shutdown(self.monitor, self.server))

    async def close_on_shutdown(self, monitor, server):
        await monitor.shutdown_requested.wait()
        await server.close()
        await monitor.stop()
        if self.server is server:
            self.server = None

    async def stop_server(self):
        if self.server:
            self.monitor.shutdown_requested.set()
            await self.watcher

    # Fakes for the blocking helpers the app runs in a thread.
    def fake_spawn(self, paths):
        self.spawns += 1
        if self.spawn_error:
            raise self.spawn_error
        if self.server is None:
            asyncio.run_coroutine_threadsafe(self.start_server(), self.loop).result(5)
        return self.monitor.status.pid

    def fake_stop(self, paths):
        if self.server is not None:
            asyncio.run_coroutine_threadsafe(self.stop_server(), self.loop).result(5)
        return True

    def make_app(self):
        self.backend = RemoteBackend(self.paths.socket)
        return PrMonApp(self.backend, daemon=self.paths)

    async def settle(self, pilot):
        for _ in range(3):
            await pilot.app.workers.wait_for_complete()
            if self.monitor:
                await self.monitor.wait_idle()
            await asyncio.sleep(0.05)
            await pilot.pause()

    async def wait_for_screen(self, pilot, screen_type):
        # Can't settle() here: the startup worker is blocked waiting for this screen.
        for _ in range(100):
            if isinstance(pilot.app.screen, screen_type):
                return
            await asyncio.sleep(0.02)
            await pilot.pause()
        self.fail(f"{screen_type.__name__} never appeared")

    def messages(self, app):
        return [n.message for n in app._notifications]

    def pr_numbers(self, app):
        table = app.query_one("#prs", DataTable)
        return [str(table.get_row_at(i)[1]) for i in range(table.row_count)]


class ConnectTest(RemoteAppTestCase):
    async def test_starts_backend_when_missing(self):
        self.write_config({"acme/api": make_repo("acme/api", (1, READY))})
        app = self.make_app()
        async with app.run_test(size=SIZE) as pilot:
            await self.settle(pilot)
            self.assertEqual(self.spawns, 1)
            self.assertEqual(self.pr_numbers(app), ["#1"])
            self.assertTrue(app.sub_title.startswith("● connected · updated "))
            self.assertNotIn("S", [b.binding.key for b in app.screen.active_bindings.values()])

    async def test_uses_running_backend_and_leaves_it_running(self):
        self.write_config({"acme/api": make_repo("acme/api", (1, READY))})
        await self.start_server()
        app = self.make_app()
        async with app.run_test(size=SIZE) as pilot:
            await self.settle(pilot)
            self.assertEqual(self.spawns, 0)
            self.assertEqual(self.pr_numbers(app), ["#1"])
        self.assertFalse(self.monitor.shutdown_requested.is_set())
        other = RemoteBackend(self.paths.socket)
        await other.connect()
        self.assertTrue(other.status.connected)
        await other.close()

    async def test_start_failure_exits(self):
        self.spawn_error = DaemonError("backend exited with code 1\nRun `gh auth login`")
        app = self.make_app()
        async with app.run_test(size=SIZE) as pilot:
            await self.settle(pilot)
        self.assertEqual(app.return_code, 1)

    async def test_version_mismatch_restart(self):
        self.version = "0.0.1"
        self.write_config({"acme/api": make_repo("acme/api", (1, READY))})
        await self.start_server()
        old_monitor = self.monitor
        app = self.make_app()
        async with app.run_test(size=SIZE) as pilot:
            await self.wait_for_screen(pilot, ConfirmScreen)
            self.assertIn("version 0.0.1", str(app.screen.message))
            self.version = __version__
            await pilot.press("y")
            await self.settle(pilot)
            self.assertIsNot(self.monitor, old_monitor)
            self.assertTrue(old_monitor.shutdown_requested.is_set())
            self.assertEqual(self.backend.status.version, __version__)
            self.assertEqual(self.pr_numbers(app), ["#1"])

    async def test_version_mismatch_declined_quits(self):
        self.version = "0.0.1"
        self.write_config({})
        await self.start_server()
        app = self.make_app()
        async with app.run_test(size=SIZE) as pilot:
            await self.wait_for_screen(pilot, ConfirmScreen)
            await pilot.press("n")
            await self.settle(pilot)
        self.assertEqual(app.return_code, 1)
        self.assertFalse(self.monitor.shutdown_requested.is_set())


class CommandsTest(RemoteAppTestCase):
    async def test_commands_go_through_the_socket(self):
        self.write_config({"acme/api": make_repo("acme/api", (1, READY))})
        self.client.repos["acme/new"] = make_repo("acme/new", (4, READY))
        app = self.make_app()
        async with app.run_test(size=SIZE, notifications=True) as pilot:
            await self.settle(pilot)
            # Add a repo.
            await pilot.press("A")
            await pilot.pause()
            app.screen.query_one(Input).value = "acme/new"
            await pilot.press("enter")
            await self.settle(pilot)
            self.assertEqual(load_config(self.config_path)[0].repos, ["acme/api", "acme/new"])
            self.assertEqual(app.selected_repo, "acme/new")
            # Configure its notifications.
            await pilot.press("N")
            await pilot.pause()
            self.assertIsInstance(app.screen, NotificationsScreen)
            app.screen.query_one("#script-enabled", Checkbox).value = True
            app.screen.query_one("#script", Input).value = "im"
            await pilot.press("ctrl+s")
            await self.settle(pilot)
            saved = load_config(self.config_path)[0].notifications["acme/new"]
            self.assertEqual(saved.script, "im")
            # Merge a PR from the action menu.
            await pilot.press("tab", "enter")
            await pilot.pause()
            self.assertIsInstance(app.screen, ActionMenuScreen)
            await pilot.press("m", "s")
            await self.settle(pilot)
            self.assertIn(("merge", "PR_4", MergeMethod.SQUASH), self.client.calls)
            messages = [n.message for n in app._notifications]
            self.assertIn("Merged acme/new#4 (squash)", messages)
            self.assertEqual(self.monitor.unseen("acme/new"), set())
            # Remove it again.
            await pilot.press("shift+tab", "D", "y")
            await self.settle(pilot)
            self.assertEqual(load_config(self.config_path)[0].repos, ["acme/api"])
            self.assertEqual(app.selected_repo, "acme/api")


class DisconnectTest(RemoteAppTestCase):
    async def asyncSetUp(self):
        await super().asyncSetUp()
        patcher = mock.patch("pr_mon.app.RECONNECT_DELAY", 0.05)
        patcher.start()
        self.addCleanup(patcher.stop)

    async def wait_until(self, pilot, condition):
        for _ in range(100):
            if condition():
                return
            await asyncio.sleep(0.02)
            await pilot.pause()
        self.fail("condition never became true")

    async def test_toast_then_reconnect_when_backend_returns(self):
        self.write_config({"acme/api": make_repo("acme/api", (1, READY))})
        app = self.make_app()
        async with app.run_test(size=SIZE, notifications=True) as pilot:
            await self.settle(pilot)
            await self.stop_server()
            await self.wait_until(pilot, lambda: app.sub_title == "○ disconnected")
            self.assertIn("Backend disconnected — reconnecting…", self.messages(app))
            self.assertEqual(self.pr_numbers(app), ["#1"])
            await pilot.press("r")
            await self.wait_until(pilot, lambda: "backend stopped" in self.messages(app))

            # Nothing restarts it from the TUI; it comes back on its own (e.g. launchd).
            await asyncio.sleep(0.2)
            self.assertIsNone(self.server)
            self.assertEqual(self.spawns, 1)

            self.client.repos["acme/api"] = make_repo("acme/api", (1, READY), (2, READY))
            await self.start_server()
            await self.wait_until(pilot, lambda: "Backend reconnected" in self.messages(app))
            self.assertTrue(app.sub_title.startswith("● connected"))
            self.assertEqual(self.pr_numbers(app), ["#1", "#2"])
            self.assertEqual(self.spawns, 1)

            # And it keeps working after reconnecting.
            await pilot.press("r")
            await self.settle(pilot)
            self.assertTrue(self.backend.status.connected)

    async def test_disconnect_twice(self):
        self.write_config({})
        app = self.make_app()
        async with app.run_test(size=SIZE, notifications=True) as pilot:
            await self.settle(pilot)
            for _ in range(2):
                await self.stop_server()
                await self.wait_until(pilot, lambda: not self.backend.status.connected)
                await self.start_server()
                await self.wait_until(pilot, lambda: self.backend.status.connected)
            await self.wait_until(
                pilot, lambda: self.messages(app).count("Backend reconnected") == 2
            )


if __name__ == "__main__":
    unittest.main()
