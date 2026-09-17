"""A Backend that talks to the daemon over its Unix socket and mirrors its state."""

import asyncio
import contextlib
import itertools
from pathlib import Path

from pr_mon.backend import BackendError, BackendEvent, BackendStatus, Listener
from pr_mon.config import Config, NotifyConfig
from pr_mon.models import (
    Action,
    ArmedMerge,
    NotificationForm,
    NotificationPreview,
    RepoInfo,
    armed_from_dict,
    repo_from_dict,
)
from pr_mon.protocol import (
    PROTOCOL_VERSION,
    ProtocolError,
    action_to_dict,
    config_from_dict,
    decode,
    encode,
    form_from_dict,
    preview_from_dict,
    settings_to_dict,
    status_from_dict,
)

STOPPED = "backend stopped"
# Snapshots of busy repos are large; allow long lines.
LINE_LIMIT = 64 * 1024 * 1024


class BackendUnavailable(BackendError):
    """Nothing is listening on the daemon socket."""


class BackendMismatch(BackendError):
    """The daemon runs a different pr-mon or protocol version than this client."""

    def __init__(self, version: str, merging: bool, protocol: object = PROTOCOL_VERSION):
        super().__init__(f"backend is running pr-mon {version} (protocol {protocol})")
        self.version = version
        self.merging = merging
        self.protocol = protocol


class RemoteBackend:
    def __init__(self, socket_path: Path):
        self.socket_path = socket_path
        self.config = Config()
        self.repos: dict[str, RepoInfo] = {}
        self.errors: dict[str, str] = {}
        self.status = BackendStatus(connected=False)
        self._unseen: dict[str, set[int]] = {}
        self._collapsed: set[str] = set()
        self._armed: dict[str, dict[int, ArmedMerge]] = {}
        self._listeners: list[Listener] = []
        self._ids = itertools.count(1)
        self._pending: dict[int, asyncio.Future] = {}
        self._reader_task: asyncio.Task | None = None
        self._writer: asyncio.StreamWriter | None = None

    # ----- connection -----

    async def connect(self, expected_version: str | None = None) -> None:
        """Connect and load a snapshot. Refuse a daemon speaking another protocol
        version (or, with `expected_version`, running another pr-mon version)
        before reading any of its data."""
        try:
            reader, self._writer = await asyncio.open_unix_connection(
                str(self.socket_path), limit=LINE_LIMIT
            )
        except (FileNotFoundError, ConnectionRefusedError) as e:
            raise BackendUnavailable(f"no backend at {self.socket_path}") from e
        self._reader_task = asyncio.create_task(self._read_loop(reader))
        self.status.connected = True
        try:
            hello = await self._request("hello")
            version = hello.get("version", "")
            # Backends older than protocol versioning don't send one.
            protocol = hello.get("protocol", 0)
            if protocol != PROTOCOL_VERSION or (
                expected_version is not None and version != expected_version
            ):
                raise BackendMismatch(version, bool(hello.get("merging")), protocol)
            snapshot = await self._request("snapshot")
            try:
                self._apply_snapshot(snapshot)
            except (KeyError, TypeError, ValueError) as e:
                raise BackendError(
                    "the backend sent data this pr-mon doesn't understand "
                    f"({e!r}); run `pr-mon stop` and try again"
                ) from e
        except BackendError:
            await self.close()
            raise

    async def close(self) -> None:
        if self._writer:
            self._writer.close()
        if self._reader_task:
            self._reader_task.cancel()
            with contextlib.suppress(asyncio.CancelledError):
                await self._reader_task
        self._mark_disconnected(emit=False)

    async def _read_loop(self, reader: asyncio.StreamReader) -> None:
        try:
            while line := await reader.readline():
                message = decode(line)
                if "event" in message:
                    self._apply_event(message["event"], message.get("data") or {})
                else:
                    future = self._pending.pop(message.get("id"), None)
                    if future and not future.done():
                        if message.get("ok"):
                            future.set_result(message.get("result"))
                        else:
                            future.set_exception(BackendError(message.get("error", "failed")))
        except (ConnectionError, ProtocolError, asyncio.IncompleteReadError, ValueError):
            pass
        self._mark_disconnected(emit=True)

    def _mark_disconnected(self, emit: bool) -> None:
        was_connected = self.status.connected
        self.status.connected = False
        if self._writer is not None:
            self._writer.close()
        self._writer = None
        for future in self._pending.values():
            if not future.done():
                future.set_exception(BackendError(STOPPED))
        self._pending.clear()
        if emit and was_connected:
            self._emit(BackendEvent("disconnected"))

    async def _request(self, op: str, **args):
        if not self.status.connected or self._writer is None:
            raise BackendError(STOPPED)
        request_id = next(self._ids)
        future = asyncio.get_running_loop().create_future()
        self._pending[request_id] = future
        try:
            self._writer.write(encode({"id": request_id, "op": op, "args": args}))
            await self._writer.drain()
        except ConnectionError as e:
            self._pending.pop(request_id, None)
            raise BackendError(STOPPED) from e
        return await future

    # ----- mirror -----

    def add_listener(self, listener: Listener) -> None:
        self._listeners.append(listener)

    def _emit(self, event: BackendEvent) -> None:
        for listener in list(self._listeners):
            listener(event)

    def _apply_snapshot(self, data: dict) -> None:
        self.config = config_from_dict(data["config"])
        self.repos = {name: repo_from_dict(repo) for name, repo in data["repos"].items()}
        self.errors = dict(data["errors"])
        self._unseen = {name: set(numbers) for name, numbers in data["unseen"].items()}
        self._collapsed = set(data["collapsed"])
        self._armed = {name: _parse_armed(armed) for name, armed in data["armed"].items()}
        self.status = status_from_dict(data["status"])
        self.status.connected = True

    def _apply_event(self, kind: str, data: dict) -> None:
        event = BackendEvent(kind, name=data.get("name"))
        if kind == "repos":
            self._apply_snapshot(data)
        elif kind == "repo":
            name = data["name"]
            if data["repo"] is None:
                self.repos.pop(name, None)
            else:
                self.repos[name] = repo_from_dict(data["repo"])
            if data["error"] is None:
                self.errors.pop(name, None)
            else:
                self.errors[name] = data["error"]
            self._unseen[name] = set(data["unseen"])
            self._armed[name] = _parse_armed(data["armed"])
        elif kind == "seen":
            self._unseen[data["name"]] = set(data["unseen"])
        elif kind == "collapsed":
            self._collapsed = set(data["collapsed"])
        elif kind == "config":
            self.config = config_from_dict(data["config"])
        elif kind == "status":
            self.status = status_from_dict(data["status"])
            self.status.connected = True
        elif kind == "toast":
            event = BackendEvent(kind, message=data["message"], severity=data["severity"])
        else:
            return
        self._emit(event)

    # ----- Backend read side -----

    @property
    def collapsed(self) -> set[str]:
        return set(self._collapsed)

    def unseen(self, name: str) -> set[int]:
        return set(self._unseen.get(name, ()))

    def armed(self, name: str) -> dict[int, ArmedMerge]:
        return dict(self._armed.get(name, {}))

    # ----- Backend commands -----

    async def refresh_all(self) -> None:
        await self._request("refresh_all")

    async def add_repo(self, name: str) -> str:
        return await self._request("add_repo", name=name)

    async def remove_repo(self, name: str) -> None:
        await self._request("remove_repo", name=name)

    async def mark_seen(self, name: str, number: int) -> None:
        self._unseen.get(name, set()).discard(number)
        await self._request("mark_seen", name=name, number=number)

    async def set_collapsed(self, owner: str, collapsed: bool) -> None:
        if collapsed:
            self._collapsed.add(owner)
        else:
            self._collapsed.discard(owner)
        await self._request("set_collapsed", owner=owner, collapsed=collapsed)

    async def perform(self, repo: str, number: int, action: Action) -> None:
        await self._request("perform", repo=repo, number=number, action=action_to_dict(action))

    async def save_notifications(self, repo: str, settings: NotifyConfig) -> None:
        await self._request("save_notifications", repo=repo, settings=settings_to_dict(settings))

    async def send_test(self, repo: str, settings: NotifyConfig) -> None:
        await self._request("send_test", repo=repo, settings=settings_to_dict(settings))

    async def set_poll_interval(self, seconds: int) -> None:
        await self._request("set_poll_interval", seconds=seconds)

    async def notification_form(self) -> NotificationForm:
        return form_from_dict(await self._request("notification_form"))

    async def preview_notification(self, repo: str, message: str) -> NotificationPreview:
        return preview_from_dict(
            await self._request("preview_notification", repo=repo, message=message)
        )

    async def shutdown(self) -> None:
        await self._request("shutdown")


def _parse_armed(data: dict) -> dict[int, ArmedMerge]:
    return {int(number): armed_from_dict(merge) for number, merge in data.items()}
