"""Start the backend at login on macOS with a launchd LaunchAgent."""

import os
import plistlib
import shutil
import subprocess
import sys
import time
from collections.abc import Callable
from dataclasses import dataclass
from pathlib import Path

from pr_mon.backend import BackendError
from pr_mon.daemon import (
    START_TIMEOUT,
    DaemonError,
    DaemonPaths,
    daemon_info,
    daemon_status,
    request,
    spawn_daemon,
    stop_daemon,
    tail_log,
)
from pr_mon.protocol import ProtocolError

LABEL = "com.acheris-labs.pr-mon"
# launchd starts jobs with a bare environment; keep what the backend needs (gh on PATH).
KEPT_ENV = ("PATH", "XDG_CONFIG_HOME", "XDG_STATE_HOME", "LANG")
READY_TIMEOUT = 20.0

Runner = Callable[..., subprocess.CompletedProcess]


class AutostartError(Exception):
    """Autostart could not be changed; the message is for the user."""


@dataclass(frozen=True)
class AutostartStatus:
    installed: bool
    loaded: bool
    program: list[str] | None


class LaunchAgent:
    def __init__(
        self,
        agents_dir: Path | None = None,
        runner: Runner = subprocess.run,
        uid: int | None = None,
    ):
        self.agents_dir = agents_dir or Path.home() / "Library" / "LaunchAgents"
        self.plist_path = self.agents_dir / f"{LABEL}.plist"
        self.runner = runner
        self.domain = f"gui/{os.getuid() if uid is None else uid}"

    def _launchctl(self, *args: str) -> subprocess.CompletedProcess:
        return self.runner(["launchctl", *args], capture_output=True, text=True)

    def is_loaded(self) -> bool:
        return self._launchctl("print", f"{self.domain}/{LABEL}").returncode == 0

    def status(self) -> AutostartStatus:
        program = None
        installed = self.plist_path.exists()
        if installed:
            try:
                program = plistlib.loads(self.plist_path.read_bytes())["ProgramArguments"]
            except (OSError, plistlib.InvalidFileException, KeyError, ValueError):
                program = None
        return AutostartStatus(installed, self.is_loaded(), program)

    def install(self, program: list[str], env: dict[str, str], log_path: Path) -> None:
        self.agents_dir.mkdir(parents=True, exist_ok=True)
        self.plist_path.write_bytes(plistlib.dumps(agent_plist(program, env, log_path)))
        if self.is_loaded():
            self._bootout()
        result = self._launchctl("bootstrap", self.domain, str(self.plist_path))
        if result.returncode:
            raise AutostartError(f"launchctl bootstrap failed: {_output(result)}")

    def kickstart(self) -> None:
        """Restart the loaded job under launchd (kills the running backend)."""
        result = self._launchctl("kickstart", "-k", f"{self.domain}/{LABEL}")
        if result.returncode:
            raise AutostartError(f"launchctl kickstart failed: {_output(result)}")

    def uninstall(self) -> bool:
        """Remove the agent (stopping its job); False if it wasn't there."""
        loaded = self.is_loaded()
        existed = loaded or self.plist_path.exists()
        if loaded:
            self._bootout()
        self.plist_path.unlink(missing_ok=True)
        return existed

    def _bootout(self) -> None:
        result = self._launchctl("bootout", f"{self.domain}/{LABEL}")
        if result.returncode:
            raise AutostartError(f"launchctl bootout failed: {_output(result)}")


def _output(result: subprocess.CompletedProcess) -> str:
    return (result.stderr or result.stdout or f"exit code {result.returncode}").strip()


def agent_plist(program: list[str], env: dict[str, str], log_path: Path) -> dict:
    return {
        "Label": LABEL,
        "ProgramArguments": program,
        "RunAtLoad": True,
        # Restart after a crash, but not after a requested shutdown (exit 0).
        "KeepAlive": {"SuccessfulExit": False},
        "EnvironmentVariables": env,
        # Output before the backend's own logging starts (e.g. a crash) lands here too.
        "StandardOutPath": str(log_path),
        "StandardErrorPath": str(log_path),
    }


def daemon_program(argv0: str | None = None) -> list[str]:
    """The command launchd should run: this pr-mon executable with `daemon`."""
    path = Path(argv0 if argv0 is not None else sys.argv[0])
    if path.name == "pr-mon" and path.exists():
        return [str(path.absolute()), "daemon"]
    found = shutil.which("pr-mon")
    if found:
        return [found, "daemon"]
    return [sys.executable, "-m", "pr_mon", "daemon"]


def launch_environment() -> dict[str, str]:
    return {name: os.environ[name] for name in KEPT_ENV if name in os.environ}


def _require_macos() -> None:
    if sys.platform != "darwin":
        raise AutostartError("autostart is only supported on macOS")


def wait_until_ready(paths: DaemonPaths, timeout: float = READY_TIMEOUT) -> str:
    """Wait for the launchd-started backend and report whether GitHub access works."""
    deadline = time.monotonic() + timeout
    while daemon_info(paths) is None:
        if time.monotonic() > deadline:
            raise AutostartError(
                f"the backend did not start under launchd\n{tail_log(paths)}".rstrip()
            )
        time.sleep(0.2)
    while True:
        try:
            snapshot = request(paths, "snapshot")
        except (OSError, BackendError, ProtocolError) as e:
            raise AutostartError(f"the backend stopped answering: {e}") from e
        if not snapshot["config"]["repos"]:
            return "backend running (no repos yet, so GitHub access wasn't checked)"
        if snapshot["status"]["last_update"]:
            return "backend running, GitHub access OK"
        if snapshot["errors"]:
            repo, error = next(iter(snapshot["errors"].items()))
            return f"backend running, but GitHub reported for {repo}: {error}"
        if time.monotonic() > deadline:
            return "backend running; GitHub check still pending (see `pr-mon status`)"
        time.sleep(0.5)


def enable(
    paths: DaemonPaths,
    agent: LaunchAgent | None = None,
    program: list[str] | None = None,
    timeout: float = READY_TIMEOUT,
) -> list[str]:
    """Install the login agent, hand the backend over to launchd, and verify it."""
    _require_macos()
    agent = agent or LaunchAgent()
    program = program or daemon_program()
    lines = []
    if ".venv" in Path(program[0]).parts:
        lines.append(
            f"warning: {program[0]} is inside a checkout; run `make tool-install` "
            "and enable autostart again so moving the checkout doesn't break it"
        )
    # launchd should own the only backend; a second one would just exit.
    stop_daemon(paths)
    agent.install(program, launch_environment(), paths.log)
    try:
        check = wait_until_ready(paths, timeout)
    except AutostartError:
        # Don't leave launchd restarting a backend that can't start.
        agent.uninstall()
        raise
    lines.append(f"autostart enabled: runs `{' '.join(program)}` at login")
    lines.append(check)
    return lines


def disable(paths: DaemonPaths, agent: LaunchAgent | None = None) -> str:
    """Remove the login agent; a backend that was running keeps running."""
    _require_macos()
    agent = agent or LaunchAgent()
    was_running = daemon_status(paths) is not None
    if not agent.uninstall():
        return "autostart was not enabled"
    if was_running:
        # Removing the agent stops its job; start an ordinary backend in its place.
        spawn_daemon(paths)
        return "autostart disabled (the backend is still running)"
    return "autostart disabled"


def describe(agent: LaunchAgent | None = None) -> tuple[bool, str]:
    """(enabled, human-readable status)."""
    _require_macos()
    status = (agent or LaunchAgent()).status()
    if not status.installed:
        return False, "disabled"
    runs = f"runs `{' '.join(status.program)}`" if status.program else "unreadable agent file"
    if status.loaded:
        return True, f"enabled (loaded in launchd, {runs})"
    return True, f"enabled but not loaded ({runs}); run `pr-mon autostart enable` to fix"


def restart_backend(
    paths: DaemonPaths, agent: LaunchAgent | None = None, timeout: float = START_TIMEOUT
) -> int:
    """Restart the backend, keeping it under launchd when autostart manages it."""
    agent = agent or LaunchAgent()
    if sys.platform != "darwin" or not agent.is_loaded():
        stop_daemon(paths)
        return spawn_daemon(paths)
    old_pid = daemon_status(paths)
    try:
        agent.kickstart()
    except AutostartError as e:
        raise DaemonError(str(e)) from e
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        info = daemon_info(paths)
        if info and info["pid"] != old_pid:
            return info["pid"]
        time.sleep(0.1)
    raise DaemonError(f"launchd did not restart the backend\n{tail_log(paths)}".rstrip())
