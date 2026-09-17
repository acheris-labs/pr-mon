"""Command-line entry point."""

import argparse
import asyncio
import sys

from pr_mon import __version__, autostart
from pr_mon.app import PrMonApp
from pr_mon.config import default_config_path
from pr_mon.daemon import (
    AlreadyRunning,
    DaemonError,
    DaemonPaths,
    daemon_info,
    daemon_status,
    run_daemon,
    spawn_daemon,
    stop_daemon,
)
from pr_mon.github import AuthError, GitHubClient, get_token
from pr_mon.notify import detect_desktop_notifier
from pr_mon.remote import RemoteBackend
from pr_mon.state import default_state_path

COMMANDS = {
    "tui": "open the dashboard (default); starts the backend if needed",
    "daemon": "run the backend in the foreground",
    "start": "start the backend in the background",
    "stop": "stop the background backend",
    "status": "show whether the backend is running",
}

AUTOSTART_ACTIONS = {
    "enable": "run the backend at every login (and start it now)",
    "disable": "stop running the backend at login",
    "status": "show whether autostart is enabled",
}


def main(argv: list[str] | None = None) -> None:
    parser = argparse.ArgumentParser(
        prog="pr-mon", description="Monitor and merge GitHub pull requests."
    )
    parser.add_argument("--version", action="version", version=f"pr-mon {__version__}")
    subcommands = parser.add_subparsers(dest="command", metavar="command")
    for name, help_text in COMMANDS.items():
        subcommands.add_parser(name, help=help_text)
    login = subcommands.add_parser("autostart", help="start the backend at login (macOS)")
    actions = login.add_subparsers(dest="action", metavar="action", required=True)
    for name, help_text in AUTOSTART_ACTIONS.items():
        actions.add_parser(name, help=help_text)
    args = parser.parse_args(argv)
    paths = DaemonPaths.default()
    if args.command == "autostart":
        autostart_command(args.action, paths)
        return
    handlers = {
        "tui": run_tui,
        "daemon": run_backend,
        "start": start_backend,
        "stop": stop_backend,
        "status": backend_status,
    }
    handlers[args.command or "tui"](paths)


def run_tui(paths: DaemonPaths) -> None:
    app = PrMonApp(RemoteBackend(paths.socket), daemon=paths)
    app.run()
    if app.return_code:
        sys.exit(app.return_code)


def run_backend(paths: DaemonPaths) -> None:
    try:
        client = GitHubClient(get_token)
    except AuthError as e:
        sys.exit(f"pr-mon: {e}. Run `gh auth login`.")
    try:
        asyncio.run(
            run_daemon(
                paths,
                client,
                default_config_path(),
                default_state_path(),
                notifier=detect_desktop_notifier(),
                version=__version__,
                # When spawned, stderr already goes to the log file.
                log_to_stderr=sys.stderr.isatty(),
            )
        )
    except AlreadyRunning as e:
        # Not a failure: launchd must not keep retrying when a backend is already up.
        print(f"pr-mon: {e}", file=sys.stderr)
    except DaemonError as e:
        sys.exit(f"pr-mon: {e}")


def start_backend(paths: DaemonPaths) -> None:
    try:
        pid = spawn_daemon(paths)
    except DaemonError as e:
        sys.exit(f"pr-mon: {e}")
    print(f"backend running (pid {pid})")


def stop_backend(paths: DaemonPaths) -> None:
    try:
        stopped = stop_daemon(paths)
    except DaemonError as e:
        sys.exit(f"pr-mon: {e}")
    print("backend stopped" if stopped else "backend not running")


def backend_status(paths: DaemonPaths) -> None:
    info = daemon_info(paths)
    if info:
        print(f"backend running (pid {info['pid']}, version {info['version']})")
        return
    pid = daemon_status(paths)
    if pid is not None:
        print(f"backend not responding (pid {pid}); see {paths.log}")
    else:
        print("backend stopped")
    sys.exit(1)


def autostart_command(action: str, paths: DaemonPaths) -> None:
    try:
        if action == "enable":
            for line in autostart.enable(paths):
                print(line)
        elif action == "disable":
            print(autostart.disable(paths))
        else:
            enabled, description = autostart.describe()
            print(f"autostart {description}")
            if not enabled:
                sys.exit(1)
    except (autostart.AutostartError, DaemonError) as e:
        sys.exit(f"pr-mon: {e}")
