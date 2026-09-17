"""Daemon lifecycle: single-instance lock, logging, signals, spawn/stop/status."""

import asyncio
import contextlib
import fcntl
import logging
import logging.handlers
import os
import signal
import socket
import subprocess
import sys
import time
from dataclasses import dataclass
from pathlib import Path
from typing import TextIO

from pr_mon.backend import BackendError
from pr_mon.protocol import ProtocolError, decode, encode
from pr_mon.server import DaemonServer
from pr_mon.service import Monitor
from pr_mon.state import default_state_path

DAEMON_COMMAND = (sys.executable, "-m", "pr_mon", "daemon")
START_TIMEOUT = 5.0
STOP_TIMEOUT = 5.0
LOG_BYTES = 1_000_000
LOG_BACKUPS = 3

log = logging.getLogger(__name__)


@dataclass(frozen=True)
class DaemonPaths:
    directory: Path

    @classmethod
    def default(cls) -> "DaemonPaths":
        return cls(default_state_path().parent)

    @property
    def socket(self) -> Path:
        return self.directory / "daemon.sock"

    @property
    def lock(self) -> Path:
        return self.directory / "daemon.lock"

    @property
    def log(self) -> Path:
        return self.directory / "daemon.log"


class AlreadyRunning(Exception):
    def __init__(self, pid: int):
        super().__init__(f"backend already running (pid {pid})")
        self.pid = pid


class DaemonError(Exception):
    """The daemon could not be started or stopped; the message is for the user."""


def _read_pid(handle: TextIO) -> int:
    handle.seek(0)
    text = handle.read().strip()
    return int(text) if text.isdigit() else 0


def acquire_lock(paths: DaemonPaths) -> TextIO:
    """Hold the single-instance lock for this process's lifetime; record our pid."""
    paths.directory.mkdir(parents=True, exist_ok=True)
    handle = open(paths.lock, "a+")  # noqa: SIM115 - held open for the daemon's lifetime
    try:
        fcntl.flock(handle, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except BlockingIOError:
        pid = _read_pid(handle)
        handle.close()
        raise AlreadyRunning(pid) from None
    handle.seek(0)
    handle.truncate()
    handle.write(str(os.getpid()))
    handle.flush()
    return handle


def daemon_status(paths: DaemonPaths) -> int | None:
    """The running daemon's pid (0 if not yet recorded), or None if none is running."""
    if not paths.lock.exists():
        return None
    with open(paths.lock) as handle:
        try:
            fcntl.flock(handle, fcntl.LOCK_SH | fcntl.LOCK_NB)
        except BlockingIOError:
            return _read_pid(handle)
        fcntl.flock(handle, fcntl.LOCK_UN)
    return None


def request(paths: DaemonPaths, op: str, timeout: float = 2.0, **args) -> object:
    """Send one request to the daemon synchronously and return its result."""
    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as sock:
        sock.settimeout(timeout)
        sock.connect(str(paths.socket))
        sock.sendall(encode({"id": 1, "op": op, "args": args}))
        with sock.makefile("rb") as stream:
            reply = decode(stream.readline())
    if not reply.get("ok"):
        raise BackendError(reply.get("error", "request failed"))
    return reply.get("result")


def daemon_info(paths: DaemonPaths) -> dict | None:
    """The daemon's hello (version, pid, notifier), or None if it doesn't answer."""
    try:
        return request(paths, "hello", timeout=0.5)
    except (OSError, BackendError, ProtocolError):
        return None


def tail_log(paths: DaemonPaths, lines: int = 10) -> str:
    try:
        text = paths.log.read_text(errors="replace")
    except OSError:
        return ""
    return "\n".join(text.splitlines()[-lines:])


def _setup_logging(paths: DaemonPaths, to_stderr: bool) -> None:
    handlers: list[logging.Handler] = [
        logging.handlers.RotatingFileHandler(paths.log, maxBytes=LOG_BYTES, backupCount=LOG_BACKUPS)
    ]
    if to_stderr:
        handlers.append(logging.StreamHandler())
    logging.basicConfig(
        level=logging.INFO,
        format="%(asctime)s %(levelname)s %(name)s: %(message)s",
        handlers=handlers,
        force=True,
    )


async def run_daemon(
    paths: DaemonPaths,
    client,
    config_path: Path,
    state_path: Path,
    notifier: str | None,
    version: str,
    log_to_stderr: bool = False,
) -> None:
    """Serve until asked to shut down (shutdown request, SIGTERM, or SIGINT)."""
    lock = acquire_lock(paths)
    _setup_logging(paths, log_to_stderr)
    monitor = Monitor(client, config_path, state_path, notifier=notifier, version=version)
    server = DaemonServer(monitor, paths.socket)
    loop = asyncio.get_running_loop()
    signals = (signal.SIGTERM, signal.SIGINT)
    for sig in signals:
        loop.add_signal_handler(sig, monitor.shutdown_requested.set)
    try:
        # We hold the lock, so any socket file left behind belongs to a dead daemon.
        paths.socket.unlink(missing_ok=True)
        await monitor.start()
        await server.start()
        log.info("pr-mon backend %s started (pid %s)", version, os.getpid())
        await monitor.shutdown_requested.wait()
        log.info("shutting down")
    finally:
        await server.close()
        await monitor.stop()
        paths.socket.unlink(missing_ok=True)
        for sig in signals:
            loop.remove_signal_handler(sig)
        lock.seek(0)
        lock.truncate()
        lock.close()


def spawn_daemon(
    paths: DaemonPaths, command: tuple[str, ...] = DAEMON_COMMAND, timeout: float = START_TIMEOUT
) -> int:
    """Start the daemon in the background (if needed) and wait until it answers."""
    info = daemon_info(paths)
    if info:
        return info["pid"]
    paths.directory.mkdir(parents=True, exist_ok=True)
    with open(paths.log, "ab") as log_file:
        process = subprocess.Popen(
            command,
            stdin=subprocess.DEVNULL,
            stdout=log_file,
            stderr=subprocess.STDOUT,
            start_new_session=True,
            cwd=Path.home(),
        )
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        info = daemon_info(paths)
        if info:
            return info["pid"]
        code = process.poll()
        if code is not None:
            raise DaemonError(f"backend exited with code {code}\n{tail_log(paths)}".rstrip())
        time.sleep(0.05)
    process.kill()
    process.wait()
    raise DaemonError(f"backend did not start within {timeout:g}s\n{tail_log(paths)}".rstrip())


def _wait_stopped(paths: DaemonPaths, timeout: float) -> bool:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if daemon_status(paths) is None:
            return True
        time.sleep(0.05)
    return daemon_status(paths) is None


def stop_daemon(paths: DaemonPaths, timeout: float = STOP_TIMEOUT) -> bool:
    """Stop the daemon; False if it wasn't running. Falls back to SIGTERM."""
    pid = daemon_status(paths)
    if pid is None:
        return False
    with contextlib.suppress(OSError, BackendError, ProtocolError):
        request(paths, "shutdown")
    if _wait_stopped(paths, timeout):
        return True
    if pid:
        with contextlib.suppress(ProcessLookupError):
            os.kill(pid, signal.SIGTERM)
        if _wait_stopped(paths, timeout):
            return True
    raise DaemonError(f"backend (pid {pid}) did not stop; see {paths.log}")
