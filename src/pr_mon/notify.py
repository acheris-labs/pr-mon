"""Notification rendering, selection, and delivery (script and desktop)."""

import asyncio
import contextlib
import os
import re
import shlex
import shutil
import signal
from dataclasses import dataclass
from pathlib import Path

from pr_mon.config import NotifyConfig
from pr_mon.models import PullRequest, RepoInfo
from pr_mon.tracker import Change

APP_TITLE = "pr-mon"
COMMAND_TIMEOUT = 30.0
DESKTOP_NOTIFIERS = ("terminal-notifier", "osascript", "notify-send")
VARIABLE_NAMES = (
    "PR_REPO",
    "PR_NUM",
    "PR_TITLE",
    "PR_AUTHOR",
    "PR_BRANCH",
    "PR_TARGET",
    "PR_STATE",
    "PR_URL",
    "PR_REASON",
)
SAMPLE_VARIABLES = {
    "PR_REPO": "owner/repo",
    "PR_NUM": "123",
    "PR_TITLE": "Example change",
    "PR_AUTHOR": "octocat",
    "PR_BRANCH": "feature",
    "PR_TARGET": "main",
    "PR_STATE": "READY",
    "PR_URL": "https://github.com/owner/repo/pull/123",
    "PR_REASON": "",
}
PLACEHOLDER = re.compile(r"\{\{\s*([A-Za-z0-9_]+)\s*\}\}")


@dataclass(frozen=True)
class Notification:
    repo: str
    pr: PullRequest
    state: str  # a Status value, or "NEW"


def sample_variables(repo_name: str) -> dict[str, str]:
    """Example values for previews and test sends, using a real repo name."""
    return {
        **SAMPLE_VARIABLES,
        "PR_REPO": repo_name,
        "PR_URL": f"https://github.com/{repo_name}/pull/{SAMPLE_VARIABLES['PR_NUM']}",
    }


def render(template: str, variables: dict[str, str]) -> str:
    # One pass: substituted values are never scanned for placeholders.
    return PLACEHOLDER.sub(lambda m: variables.get(m.group(1), m.group(0)), template)


def unknown_placeholders(template: str) -> list[str]:
    unknown = []
    for name in PLACEHOLDER.findall(template):
        if name not in VARIABLE_NAMES and name not in unknown:
            unknown.append(name)
    return unknown


def pr_variables(repo_name: str, pr: PullRequest, state: str, reason: str = "") -> dict[str, str]:
    return {
        "PR_REPO": repo_name,
        "PR_NUM": str(pr.number),
        "PR_TITLE": pr.title,
        "PR_AUTHOR": pr.author,
        "PR_BRANCH": pr.head_ref,
        "PR_TARGET": pr.base_ref,
        "PR_STATE": state,
        "PR_URL": pr.url,
        "PR_REASON": reason,
    }


def select_notifications(
    repo: RepoInfo, changes: list[Change], settings: NotifyConfig
) -> list[Notification]:
    by_number = {pr.number: pr for pr in repo.prs}
    found = []
    for change in changes:
        pr = by_number[change.number]
        if pr.is_draft and not settings.include_drafts:
            continue
        state = "NEW" if change.old is None else str(change.new)
        if state in settings.events:
            found.append(Notification(repo.name, pr, state))
    return found


def detect_desktop_notifier() -> str | None:
    """Path of the first available desktop notifier, in order of preference."""
    for tool in DESKTOP_NOTIFIERS:
        path = shutil.which(tool)
        if path:
            return path
    return None


def desktop_argv(notifier: str, message: str, variables: dict[str, str]) -> list[str]:
    tool = Path(notifier).name
    if tool == "terminal-notifier":
        argv = [notifier, "-title", APP_TITLE, "-subtitle", variables["PR_REPO"]]
        argv += ["-message", message]
        if variables.get("PR_URL"):
            argv += ["-open", variables["PR_URL"]]
        return argv
    if tool == "osascript":
        # The message is passed as data to the script, never spliced into its source.
        return [
            notifier,
            "-e",
            "on run argv",
            "-e",
            "display notification (item 1 of argv) with title (item 2 of argv)",
            "-e",
            "end run",
            message,
            APP_TITLE,
        ]
    return [notifier, "--", APP_TITLE, message]


def script_argv(command: str) -> list[str]:
    """Split a script command like a shell would, without running a shell."""
    argv = shlex.split(command)
    if not argv:
        raise ValueError("script command is empty")
    return [os.path.expanduser(arg) if arg.startswith("~") else arg for arg in argv]


async def run_command(
    argv: list[str], stdin: str | None, env_extra: dict[str, str], timeout: float = COMMAND_TIMEOUT
) -> str | None:
    """Run argv without a shell; return an error message, or None on success."""
    name = Path(argv[0]).name
    try:
        process = await asyncio.create_subprocess_exec(
            *argv,
            stdin=asyncio.subprocess.PIPE if stdin is not None else asyncio.subprocess.DEVNULL,
            stdout=asyncio.subprocess.DEVNULL,
            stderr=asyncio.subprocess.PIPE,
            env={**os.environ, **env_extra},
            cwd=Path.home(),
            start_new_session=True,
        )
    except OSError as e:
        return f"{name}: {e.strerror or e}"
    try:
        _, stderr = await asyncio.wait_for(
            process.communicate(stdin.encode() if stdin is not None else None), timeout
        )
    except TimeoutError:
        # Kill the whole session so helpers the script started die too.
        with contextlib.suppress(ProcessLookupError):
            os.killpg(process.pid, signal.SIGKILL)
        await process.wait()
        return f"{name} timed out after {timeout:g}s"
    if process.returncode:
        lines = stderr.decode(errors="replace").strip().splitlines()
        detail = f": {lines[0]}" if lines else ""
        return f"{name} exited with code {process.returncode}{detail}"
    return None


async def deliver(
    settings: NotifyConfig, notifier: str | None, variables: dict[str, str]
) -> list[tuple[str, str | None]]:
    """Send one notification on every enabled channel; return (channel, error) pairs."""
    message = render(settings.message, variables)
    channels = []
    jobs = []
    if settings.script_enabled and settings.script.strip():
        channels.append("script")
        try:
            argv = script_argv(settings.script)
        except ValueError as e:
            jobs.append(_error(f"Bad script command: {e}"))
        else:
            jobs.append(run_command(argv, message, variables))
    if settings.desktop_enabled and notifier:
        channels.append("desktop")
        jobs.append(run_command(desktop_argv(notifier, message, variables), None, {}))
    errors = await asyncio.gather(*jobs)
    return list(zip(channels, errors, strict=True))


async def _error(message: str) -> str:
    return message
