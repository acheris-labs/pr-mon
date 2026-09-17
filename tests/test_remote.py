import asyncio
import os
import stat
import tempfile
import unittest
from pathlib import Path
from unittest import mock

from pr_mon.backend import BackendError
from pr_mon.config import Config, NotifyConfig, load_config, save_config
from pr_mon.models import Action, MergeMethod
from pr_mon.protocol import PROTOCOL_VERSION, decode, encode
from pr_mon.remote import BackendMismatch, BackendUnavailable, RemoteBackend
from pr_mon.server import DaemonServer
from pr_mon.service import Monitor
from tests.fakes import FakeClient, FakeDeliver, make_repo

READY = {}
PENDING = {"merge_state": "BLOCKED", "check_state": "PENDING"}


class ServerTestCase(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self):
        # Short path: Unix socket paths are limited to ~104 bytes on macOS.
        self.tmp = tempfile.TemporaryDirectory(dir="/tmp")
        self.addCleanup(self.tmp.cleanup)
        self.dir = Path(self.tmp.name)
        self.socket_path = self.dir / "d.sock"
        self.deliver = FakeDeliver()
        patcher = mock.patch("pr_mon.service.deliver", self.deliver)
        patcher.start()
        self.addCleanup(patcher.stop)

    async def serve(self, repos, notifications=None):
        save_config(
            self.dir / "config.toml",
            Config(repos=list(repos), notifications=notifications or {}),
        )
        self.client = FakeClient(repos)
        self.monitor = Monitor(
            self.client,
            self.dir / "config.toml",
            self.dir / "state.json",
            notifier="/x/tn",
            version="1.2.3",
        )
        await self.monitor.start()
        await self.monitor.wait_idle()
        self.server = DaemonServer(self.monitor, self.socket_path)
        await self.server.start()
        self.addAsyncCleanup(self.shutdown_server)

    async def shutdown_server(self):
        await self.server.close()
        await self.monitor.stop()

    async def remote(self):
        backend = RemoteBackend(self.socket_path)
        await backend.connect()
        self.addAsyncCleanup(backend.close)
        self.events = []
        backend.add_listener(self.events.append)
        return backend

    async def settle(self):
        await self.monitor.wait_idle()
        for _ in range(5):
            await asyncio.sleep(0.01)

    def kinds(self):
        return [e.kind for e in self.events]


class RawClient:
    async def open(self, path):
        self.reader, self.writer = await asyncio.open_unix_connection(str(path))
        return self

    async def send(self, message):
        self.writer.write(encode(message))
        await self.writer.drain()

    async def read(self):
        return decode(await asyncio.wait_for(self.reader.readline(), 2))

    async def ask(self, op, request_id=1, **args):
        await self.send({"id": request_id, "op": op, "args": args})
        while True:
            message = await self.read()
            if message.get("id") == request_id:
                return message

    def close(self):
        self.writer.close()


class ServerTest(ServerTestCase):
    async def raw(self):
        client = await RawClient().open(self.socket_path)
        self.addCleanup(client.close)
        return client

    async def test_socket_is_private(self):
        await self.serve({})
        self.assertEqual(stat.S_IMODE(os.stat(self.socket_path).st_mode), 0o600)

    async def test_hello_and_snapshot(self):
        await self.serve({"acme/api": make_repo("acme/api", (1, READY))})
        client = await self.raw()
        hello = await client.ask("hello")
        self.assertEqual(
            hello,
            {
                "id": 1,
                "ok": True,
                "result": {
                    "protocol": PROTOCOL_VERSION,
                    "version": "1.2.3",
                    "pid": os.getpid(),
                    "notifier": "/x/tn",
                    "merging": False,
                },
            },
        )
        snap = (await client.ask("snapshot", request_id=2))["result"]
        self.assertEqual(list(snap["repos"]), ["acme/api"])
        self.assertEqual(snap["unseen"], {"acme/api": [1]})

    async def test_errors(self):
        await self.serve({"acme/api": make_repo("acme/api")})
        client = await self.raw()
        unknown = await client.ask("explode")
        self.assertEqual((unknown["ok"], unknown["error"]), (False, "unknown op 'explode'"))
        bad = await client.ask("mark_seen", request_id=2, nope=1)
        self.assertEqual(bad["error"], "bad arguments for mark_seen")
        failed = await client.ask("remove_repo", request_id=3, name="gone/repo")
        self.assertEqual(failed["error"], "gone/repo is not being monitored")
        bad_action = await client.ask(
            "perform", request_id=4, repo="acme/api", number=1, action={"kind": "boom"}
        )
        self.assertFalse(bad_action["ok"])
        self.assertIn("invalid action", bad_action["error"])

    async def test_malformed_line_drops_connection(self):
        await self.serve({})
        client = await self.raw()
        client.writer.write(b"{nope\n")
        await client.writer.drain()
        self.assertEqual(await asyncio.wait_for(client.reader.readline(), 2), b"")
        other = await self.raw()
        self.assertTrue((await other.ask("hello"))["ok"])

    async def test_events_only_after_snapshot_and_to_every_subscriber(self):
        await self.serve({"acme/api": make_repo("acme/api", (1, READY))})
        quiet = await self.raw()
        first = await self.raw()
        second = await self.raw()
        await quiet.ask("hello")
        await first.ask("snapshot")
        await second.ask("snapshot")
        await self.monitor.mark_seen("acme/api", 1)
        for client in (first, second):
            self.assertEqual(
                await client.read(),
                {"event": "seen", "data": {"name": "acme/api", "unseen": []}},
            )
        with self.assertRaises(TimeoutError):
            await asyncio.wait_for(quiet.reader.readline(), 0.2)

    async def test_dead_subscriber_does_not_break_broadcast(self):
        await self.serve({"acme/api": make_repo("acme/api", (1, READY))})
        dead = await self.raw()
        live = await self.raw()
        await dead.ask("snapshot")
        await live.ask("snapshot")
        dead.close()
        await asyncio.sleep(0.05)
        await self.monitor.mark_seen("acme/api", 1)
        self.assertEqual((await live.read())["event"], "seen")

    async def test_shutdown_op(self):
        await self.serve({})
        client = await self.raw()
        self.assertTrue((await client.ask("shutdown"))["ok"])
        self.assertTrue(self.monitor.shutdown_requested.is_set())

    async def test_slow_request_does_not_block_others(self):
        await self.serve({"acme/api": make_repo("acme/api", (1, READY))})
        gate = asyncio.Event()
        original = self.client.update_branch

        async def slow(pr_id):
            await gate.wait()
            await original(pr_id)

        self.client.update_branch = slow
        client = await self.raw()
        await client.send(
            {
                "id": 1,
                "op": "perform",
                "args": {"repo": "acme/api", "number": 1, "action": {"kind": "update"}},
            }
        )
        self.assertTrue((await client.ask("hello", request_id=2))["ok"])
        gate.set()
        self.assertEqual((await client.ask("hello", request_id=3))["id"], 3)


class RemoteBackendTest(ServerTestCase):
    async def test_unavailable(self):
        backend = RemoteBackend(self.dir / "missing.sock")
        with self.assertRaises(BackendUnavailable):
            await backend.connect()
        self.assertFalse(backend.status.connected)

    async def test_expected_version_matches(self):
        await self.serve({"acme/api": make_repo("acme/api")})
        backend = RemoteBackend(self.socket_path)
        self.addAsyncCleanup(backend.close)
        await backend.connect("1.2.3")
        self.assertEqual(list(backend.repos), ["acme/api"])

    async def test_mismatch_raises_before_snapshot(self):
        await self.serve({"acme/api": make_repo("acme/api")})
        backend = RemoteBackend(self.socket_path)
        with self.assertRaises(BackendMismatch) as ctx:
            await backend.connect("9.9.9")
        self.assertEqual((ctx.exception.version, ctx.exception.merging), ("1.2.3", False))
        self.assertFalse(backend.status.connected)
        self.assertEqual(backend.repos, {})

    async def test_mismatch_reports_merge_in_flight(self):
        await self.serve({"acme/api": make_repo("acme/api")})
        self.monitor._merging.add(("acme/api", 1))
        backend = RemoteBackend(self.socket_path)
        with self.assertRaises(BackendMismatch) as ctx:
            await backend.connect("9.9.9")
        self.assertTrue(ctx.exception.merging)

    async def test_notification_form_and_preview(self):
        await self.serve({"acme/api": make_repo("acme/api")})
        backend = await self.remote()
        form = await backend.notification_form()
        self.assertEqual(form.events[0].name, "READY")
        self.assertIn("PR_REASON", form.variables)
        self.assertIn("stdin", form.script_help)
        self.assertEqual(form.defaults, NotifyConfig())
        preview = await backend.preview_notification("acme/api", "{{PR_REPO}} {{NOPE}}")
        self.assertEqual((preview.text, preview.unknown), ("acme/api {{NOPE}}", ("NOPE",)))

    async def test_protocol_mismatch_raises_before_snapshot(self):
        await self.serve({"acme/api": make_repo("acme/api")})
        original = self.server._hello

        async def other_protocol():
            return {**await original(), "protocol": 99}

        self.server._ops["hello"] = other_protocol
        backend = RemoteBackend(self.socket_path)
        with self.assertRaises(BackendMismatch) as ctx:
            await backend.connect("1.2.3")
        self.assertEqual((ctx.exception.version, ctx.exception.protocol), ("1.2.3", 99))
        self.assertEqual(backend.repos, {})

    async def test_backend_without_protocol_is_a_mismatch(self):
        await self.serve({"acme/api": make_repo("acme/api")})
        original = self.server._hello

        async def unversioned():
            hello = await original()
            del hello["protocol"]
            return hello

        self.server._ops["hello"] = unversioned
        backend = RemoteBackend(self.socket_path)
        with self.assertRaises(BackendMismatch) as ctx:
            await backend.connect()
        self.assertEqual(ctx.exception.protocol, 0)

    async def test_unreadable_snapshot_is_an_error(self):
        await self.serve({"acme/api": make_repo("acme/api")})
        original = self.server._snapshot

        async def old_style(*, writer):
            data = await original(writer=writer)
            del data["armed"]
            return data

        self.server._ops["snapshot"] = old_style
        backend = RemoteBackend(self.socket_path)
        with self.assertRaisesRegex(BackendError, "pr-mon stop"):
            await backend.connect()
        self.assertFalse(backend.status.connected)

    async def test_mirror_matches_monitor(self):
        await self.serve(
            {"acme/api": make_repo("acme/api", (1, READY)), "acme/web": make_repo("acme/web")},
            notifications={"acme/api": NotifyConfig(script="im")},
        )
        await self.monitor.set_collapsed("acme", True)
        backend = await self.remote()
        self.assertEqual(backend.config, self.monitor.config)
        self.assertEqual(backend.repos, self.monitor.repos)
        self.assertEqual(backend.unseen("acme/api"), {1})
        self.assertEqual(backend.collapsed, {"acme"})
        self.assertTrue(backend.status.connected)
        self.assertEqual((backend.status.version, backend.status.notifier), ("1.2.3", "/x/tn"))

    async def test_repo_events_update_mirror(self):
        await self.serve({"acme/api": make_repo("acme/api", (1, PENDING))})
        backend = await self.remote()
        self.client.repos["acme/api"] = make_repo("acme/api", (1, READY), (2, READY))
        await self.monitor.poll_once()
        await self.settle()
        self.assertEqual([p.number for p in backend.repos["acme/api"].prs], [1, 2])
        self.assertEqual(backend.unseen("acme/api"), {1, 2})
        self.assertIn("repo", self.kinds())
        self.assertIn("status", self.kinds())

    async def test_commands_round_trip(self):
        await self.serve({"acme/api": make_repo("acme/api", (1, READY))})
        backend = await self.remote()
        self.client.repos["acme/new"] = make_repo("acme/new", (5, READY))
        self.assertEqual(await backend.add_repo("acme/new"), "acme/new")
        await self.settle()
        self.assertEqual(backend.config.repos, ["acme/api", "acme/new"])
        self.assertEqual(backend.unseen("acme/new"), {5})

        await backend.mark_seen("acme/api", 1)
        self.assertEqual(backend.unseen("acme/api"), set())
        self.assertEqual(self.monitor.unseen("acme/api"), set())

        await backend.set_collapsed("acme", True)
        self.assertEqual(self.monitor.collapsed, {"acme"})

        settings = NotifyConfig(script_enabled=True, script="im")
        await backend.save_notifications("acme/api", settings)
        await self.settle()
        self.assertEqual(
            load_config(self.dir / "config.toml")[0].notifications["acme/api"], settings
        )
        self.assertEqual(backend.config.notifications["acme/api"], settings)

        await backend.send_test("acme/api", settings)
        await self.settle()
        self.assertEqual(self.deliver.states(), [("acme/api", "123", "READY")])

        await backend.perform("acme/api", 1, Action("merge", MergeMethod.SQUASH))
        await self.settle()
        self.assertIn(("merge", "PR_1", MergeMethod.SQUASH), self.client.calls)
        toasts = [e.message for e in self.events if e.kind == "toast"]
        self.assertIn("Merged acme/api#1 (squash)", toasts)

        await backend.remove_repo("acme/new")
        await self.settle()
        self.assertEqual(backend.config.repos, ["acme/api"])
        self.assertNotIn("acme/new", backend.repos)

        await backend.refresh_all()
        await backend.shutdown()
        self.assertTrue(self.monitor.shutdown_requested.is_set())

    async def test_armed_merges_are_mirrored(self):
        await self.serve({"acme/api": make_repo("acme/api", (1, {"merge_state": "BLOCKED"}))})
        backend = await self.remote()
        self.assertEqual(backend.armed("acme/api"), {})
        await backend.perform("acme/api", 1, Action("arm_merge", MergeMethod.REBASE, True))
        await self.settle()
        armed = backend.armed("acme/api")[1]
        self.assertEqual((armed.method, armed.delete_branch), (MergeMethod.REBASE, True))
        self.assertEqual(backend.armed("acme/api"), self.monitor.armed("acme/api"))
        other = await self.remote()
        self.assertEqual(other.armed("acme/api"), self.monitor.armed("acme/api"))
        await backend.perform("acme/api", 1, Action("disarm_merge"))
        await self.settle()
        self.assertEqual(backend.armed("acme/api"), {})

    async def test_errors_raise(self):
        await self.serve({"acme/api": make_repo("acme/api")})
        backend = await self.remote()
        with self.assertRaisesRegex(BackendError, "already being monitored"):
            await backend.add_repo("acme/api")
        with self.assertRaisesRegex(BackendError, "not an open PR"):
            await backend.perform("acme/api", 9, Action("update"))

    async def test_server_close_disconnects(self):
        await self.serve({"acme/api": make_repo("acme/api")})
        backend = await self.remote()
        await self.server.close()
        await self.settle()
        self.assertFalse(backend.status.connected)
        self.assertEqual(self.kinds().count("disconnected"), 1)
        with self.assertRaisesRegex(BackendError, "backend stopped"):
            await backend.refresh_all()
        self.assertEqual(list(backend.repos), ["acme/api"])

    async def test_pending_request_fails_on_disconnect(self):
        await self.serve({"acme/api": make_repo("acme/api", (1, READY))})
        gate = asyncio.Event()

        async def hang(pr_id):
            await gate.wait()

        self.client.update_branch = hang
        backend = await self.remote()
        pending = asyncio.create_task(backend.perform("acme/api", 1, Action("update")))
        await asyncio.sleep(0.05)
        await self.server.close()
        with self.assertRaisesRegex(BackendError, "backend stopped"):
            await asyncio.wait_for(pending, 2)
        gate.set()

    async def test_close_is_quiet(self):
        await self.serve({})
        backend = await self.remote()
        await backend.close()
        self.assertFalse(backend.status.connected)
        self.assertNotIn("disconnected", self.kinds())


if __name__ == "__main__":
    unittest.main()
