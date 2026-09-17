"""The daemon's socket server: maps client requests onto the Monitor and fans out events."""

import asyncio
import inspect
import logging
import os
from pathlib import Path

from pr_mon.backend import BackendError, BackendEvent
from pr_mon.github import GitHubError
from pr_mon.protocol import (
    ProtocolError,
    action_from_dict,
    decode,
    encode,
    event_message,
    settings_from_dict,
    snapshot,
)
from pr_mon.service import Monitor

log = logging.getLogger(__name__)


class DaemonServer:
    def __init__(self, monitor: Monitor, socket_path: Path):
        self.monitor = monitor
        self.socket_path = socket_path
        self._server: asyncio.Server | None = None
        self._subscribers: set[asyncio.StreamWriter] = set()
        self._connections: set[asyncio.Task] = set()
        self._requests: set[asyncio.Task] = set()
        self._ops = {
            "hello": self._hello,
            "snapshot": self._snapshot,
            "refresh_all": monitor.refresh_all,
            "add_repo": monitor.add_repo,
            "remove_repo": monitor.remove_repo,
            "mark_seen": monitor.mark_seen,
            "set_collapsed": monitor.set_collapsed,
            "perform": self._perform,
            "save_notifications": self._save_notifications,
            "send_test": self._send_test,
            "shutdown": monitor.shutdown,
        }

    async def start(self) -> None:
        self.monitor.add_listener(self._broadcast)
        self._server = await asyncio.start_unix_server(self._serve, path=str(self.socket_path))
        os.chmod(self.socket_path, 0o600)

    async def close(self) -> None:
        if self._server:
            self._server.close()
        for writer in list(self._subscribers):
            writer.close()
        tasks = [*self._connections, *self._requests]
        for task in tasks:
            task.cancel()
        await asyncio.gather(*tasks, return_exceptions=True)
        if self._server:
            await self._server.wait_closed()

    # ----- ops that need translation -----

    async def _hello(self) -> dict:
        status = self.monitor.status
        return {
            "version": status.version,
            "pid": status.pid,
            "notifier": status.notifier,
            # A client restarting an outdated backend waits while this is true.
            "merging": self.monitor.merging,
        }

    async def _snapshot(self, *, writer: asyncio.StreamWriter) -> dict:
        # No awaits between taking the snapshot and subscribing, so no event is missed
        # or delivered before the snapshot response.
        self._subscribers.add(writer)
        return snapshot(self.monitor)

    async def _perform(self, repo: str, number: int, action: dict) -> None:
        await self.monitor.perform(repo, number, action_from_dict(action))

    async def _save_notifications(self, repo: str, settings: dict) -> None:
        await self.monitor.save_notifications(repo, settings_from_dict(settings))

    async def _send_test(self, repo: str, settings: dict) -> None:
        await self.monitor.send_test(repo, settings_from_dict(settings))

    # ----- connections -----

    def _broadcast(self, event: BackendEvent) -> None:
        line = encode(event_message(self.monitor, event))
        for writer in list(self._subscribers):
            if writer.is_closing():
                self._subscribers.discard(writer)
            else:
                writer.write(line)

    async def _serve(self, reader: asyncio.StreamReader, writer: asyncio.StreamWriter) -> None:
        self._connections.add(asyncio.current_task())
        try:
            while line := await reader.readline():
                try:
                    message = decode(line)
                except ProtocolError as e:
                    log.warning("dropping client: %s", e)
                    break
                task = asyncio.create_task(self._answer(message, writer))
                self._requests.add(task)
                task.add_done_callback(self._requests.discard)
        except (ConnectionError, asyncio.IncompleteReadError):
            pass
        finally:
            self._subscribers.discard(writer)
            self._connections.discard(asyncio.current_task())
            writer.close()

    async def _answer(self, message: dict, writer: asyncio.StreamWriter) -> None:
        request_id = message.get("id")
        name = message.get("op")
        op = self._ops.get(name)
        args = message.get("args") or {}
        if name == "snapshot":
            args = {"writer": writer}
        if op is None:
            reply = {"id": request_id, "ok": False, "error": f"unknown op {name!r}"}
        elif not isinstance(args, dict) or not _accepts(op, args):
            reply = {"id": request_id, "ok": False, "error": f"bad arguments for {name}"}
        else:
            try:
                result = await op(**args)
                reply = {"id": request_id, "ok": True, "result": result}
            except (BackendError, GitHubError, ProtocolError) as e:
                reply = {"id": request_id, "ok": False, "error": str(e)}
            except Exception:
                log.exception("request %s failed", message.get("op"))
                reply = {"id": request_id, "ok": False, "error": "internal error; see daemon.log"}
        if not writer.is_closing():
            writer.write(encode(reply))


def _accepts(function, args: dict) -> bool:
    try:
        inspect.signature(function).bind(**args)
    except TypeError:
        return False
    return True
