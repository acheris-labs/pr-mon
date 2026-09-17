"""The monitoring service: polling, change tracking, persistence, notifications, actions.

It has no UI dependencies so it can run inside the TUI (tests) or in the daemon.
"""

import asyncio
import logging
import os
from collections.abc import Coroutine
from datetime import UTC, datetime
from pathlib import Path

from pr_mon.backend import BackendError, BackendEvent, BackendStatus, Listener
from pr_mon.config import Config, NotifyConfig, load_config, save_config
from pr_mon.github import GitHubError, RateLimitError
from pr_mon.models import Action, RepoInfo, Status, owner_key
from pr_mon.notify import deliver, pr_variables, sample_variables, select_notifications
from pr_mon.state import AppState, load_state, save_state
from pr_mon.tracker import Tracker

CHECKING_RETRY_DELAY = 5
CHECKING_RETRIES = 3

log = logging.getLogger(__name__)


def _now_iso() -> str:
    return datetime.now(UTC).isoformat()


class Monitor:
    """Owns config.toml and state.json; everything else talks to it (see Backend)."""

    def __init__(
        self,
        client,
        config_path: Path,
        state_path: Path,
        notifier: str | None = None,
        version: str = "",
    ):
        self.client = client
        self.config_path = config_path
        self.state_path = state_path
        self.config = Config()
        self.state = AppState()
        self.tracker = Tracker(self.state.prs)
        self.repos: dict[str, RepoInfo] = {}
        self.errors: dict[str, str] = {}
        self.status = BackendStatus(pid=os.getpid(), version=version, notifier=notifier)
        self.shutdown_requested = asyncio.Event()
        # Repos whose first load finished this session; only they can notify.
        self.loaded_repos: set[str] = set()
        self._paused_until: datetime | None = None
        self._listeners: list[Listener] = []
        self._busy: set[asyncio.Task] = set()
        self._timers: set[asyncio.TimerHandle] = set()
        self._poll_task: asyncio.Task | None = None

    # ----- lifecycle -----

    async def start(self) -> None:
        self.config, config_warning = load_config(self.config_path)
        self.state, state_warning = load_state(self.state_path)
        self.tracker = Tracker(self.state.prs)
        self.status.warnings = [w for w in (config_warning, state_warning) if w]
        for warning in self.status.warnings:
            log.warning(warning)
        self._spawn(self.poll_once())
        self._poll_task = asyncio.create_task(self._poll_loop())

    async def stop(self) -> None:
        if self._poll_task:
            self._poll_task.cancel()
        for timer in self._timers:
            timer.cancel()
        tasks = [t for t in (self._poll_task, *self._busy) if t]
        for task in tasks:
            task.cancel()
        await asyncio.gather(*tasks, return_exceptions=True)
        self._save_state()
        await self.client.aclose()

    async def wait_idle(self) -> None:
        """Wait until no refresh, action, or notification is in flight."""
        while self._busy:
            await asyncio.gather(*self._busy, return_exceptions=True)

    async def _poll_loop(self) -> None:
        while True:
            await asyncio.sleep(self.config.poll_interval)
            await self._spawn(self.poll_once())

    def _spawn(self, coro: Coroutine) -> asyncio.Task:
        task = asyncio.create_task(coro)
        self._busy.add(task)
        task.add_done_callback(self._task_done)
        return task

    def _task_done(self, task: asyncio.Task) -> None:
        self._busy.discard(task)
        if not task.cancelled() and task.exception():
            log.error("background task failed", exc_info=task.exception())

    # ----- events -----

    def add_listener(self, listener: Listener) -> None:
        self._listeners.append(listener)

    def _emit(self, kind: str, **kwargs) -> None:
        event = BackendEvent(kind, **kwargs)
        for listener in list(self._listeners):
            listener(event)

    def _toast(self, message: str, severity: str = "information") -> None:
        self._emit("toast", message=message, severity=severity)

    # ----- read side -----

    @property
    def collapsed(self) -> set[str]:
        return set(self.state.collapsed)

    def unseen(self, name: str) -> set[int]:
        return self.tracker.unseen(name)

    # ----- polling -----

    async def poll_once(self) -> None:
        if self._paused_until and datetime.now(UTC) < self._paused_until:
            return
        self._paused_until = None
        await asyncio.gather(*(self.refresh_repo(name) for name in list(self.config.repos)))

    async def refresh_repo(self, name: str, attempt: int = 0) -> None:
        try:
            repo = await self.client.fetch_repo(name)
        except RateLimitError as e:
            self._paused_until = e.reset_at
            self.status.rate_limited_until = e.reset_at.isoformat()
            self._emit("status")
            return
        except GitHubError as e:
            if name in self.config.repos:
                self.errors[name] = str(e)
                self._emit("repo", name=name)
            return
        self.errors.pop(name, None)
        self.apply(name, repo)
        if attempt < CHECKING_RETRIES and any(pr.status == Status.CHECKING for pr in repo.prs):
            self._schedule_retry(name, attempt + 1)
        self.status.last_update = _now_iso()
        self.status.rate_limited_until = None
        self._emit("status")

    def _schedule_retry(self, name: str, attempt: int) -> None:
        def fire() -> None:
            self._timers.discard(handle)
            self._spawn(self.refresh_repo(name, attempt))

        handle = asyncio.get_running_loop().call_later(CHECKING_RETRY_DELAY, fire)
        self._timers.add(handle)

    def apply(self, name: str, repo: RepoInfo) -> None:
        if name not in self.config.repos:
            return
        self.repos[name] = repo
        changes = self.tracker.changes(repo)
        settings = self.config.notifications.get(name)
        if settings is not None and name in self.loaded_repos:
            for found in select_notifications(repo, changes, settings):
                variables = pr_variables(found.repo, found.pr, found.state)
                self._spawn(self._send(settings, variables))
        self.loaded_repos.add(name)
        alerted = bool(self.tracker.update(repo))
        owner = owner_key(name)
        if alerted and owner in self.state.collapsed:
            # A new alert opens its group so it's visible next time anyone looks.
            self.state.collapsed.remove(owner)
            self._emit("collapsed")
        self._save_state()
        self._emit("repo", name=name)

    # ----- notifications -----

    async def _send(
        self, settings: NotifyConfig, variables: dict[str, str], is_test: bool = False
    ) -> None:
        results = await deliver(settings, self.status.notifier, variables)
        for channel, error in results:
            if error:
                log.warning("%s notification failed: %s", channel, error)
                self._toast(
                    f"{channel.capitalize()} notification failed: {error}", severity="warning"
                )
            elif is_test:
                self._toast(f"Test {channel} notification sent")
        if is_test and not results:
            self._toast(
                "Nothing to send: enable Script or Desktop notifications", severity="warning"
            )

    async def save_notifications(self, repo: str, settings: NotifyConfig) -> None:
        self._require_repo(repo)
        self.config.notifications[repo] = settings
        save_config(self.config_path, self.config)
        self._emit("config")
        self._toast(f"Notification settings saved for {repo}")

    async def send_test(self, repo: str, settings: NotifyConfig) -> None:
        self._spawn(self._send(settings, sample_variables(repo), is_test=True))

    # ----- commands -----

    def _require_repo(self, name: str) -> None:
        if name not in self.config.repos:
            raise BackendError(f"{name} is not being monitored")

    def _save_state(self) -> None:
        save_state(self.state_path, self.state)

    async def refresh_all(self) -> None:
        self._spawn(self.poll_once())

    async def add_repo(self, name: str) -> str:
        if name.lower() in (r.lower() for r in self.config.repos):
            raise BackendError(f"{name} is already being monitored")
        try:
            repo = await self.client.fetch_repo(name)
        except GitHubError as e:
            raise BackendError(str(e)) from e
        if repo.name in self.config.repos:
            raise BackendError(f"{repo.name} is already being monitored")
        self.config.repos.append(repo.name)
        save_config(self.config_path, self.config)
        self._emit("repos")
        self.apply(repo.name, repo)
        return repo.name

    async def remove_repo(self, name: str) -> None:
        self._require_repo(name)
        self.config.repos.remove(name)
        self.config.notifications.pop(name, None)
        self.repos.pop(name, None)
        self.errors.pop(name, None)
        self.loaded_repos.discard(name)
        self.tracker.forget(name)
        save_config(self.config_path, self.config)
        self._save_state()
        self._emit("repos")

    async def mark_seen(self, name: str, number: int) -> None:
        if self.tracker.mark_seen(name, number):
            self._save_state()
            self._emit("seen", name=name)

    async def set_collapsed(self, owner: str, collapsed: bool) -> None:
        current = self.collapsed
        updated = current | {owner} if collapsed else current - {owner}
        if updated != current:
            self.state.collapsed = sorted(updated)
            self._save_state()
            self._emit("collapsed")

    async def perform(self, repo: str, number: int, action: Action) -> None:
        info = self.repos.get(repo)
        pr = next((p for p in info.prs if p.number == number), None) if info else None
        if pr is None:
            raise BackendError(f"{repo}#{number} is not an open PR")
        label = f"{repo}#{number}"
        try:
            if action.kind == "merge":
                await self.client.merge(pr.id, action.method)
                self._toast(f"Merged {label} ({action.method.lower()})")
                if action.delete_branch and pr.head_ref_id:
                    try:
                        await self.client.delete_branch(pr.head_ref_id)
                    except GitHubError as e:
                        self._toast(
                            f"Merged {label}, but couldn't delete {pr.head_ref}: {e}",
                            severity="warning",
                        )
            elif action.kind == "auto_merge_on":
                await self.client.enable_auto_merge(pr.id, action.method)
                self._toast(f"Auto-merge enabled for {label} ({action.method.lower()})")
            elif action.kind == "auto_merge_off":
                await self.client.disable_auto_merge(pr.id)
                self._toast(f"Auto-merge disabled for {label}")
            else:
                await self.client.update_branch(pr.id)
                self._toast(f"Updated branch for {label}")
        except GitHubError as e:
            what = action.kind.replace("_", " ").capitalize()
            self._toast(f"{what} failed for {label}: {e}", severity="error")
        await self.refresh_repo(repo)

    async def shutdown(self) -> None:
        self.shutdown_requested.set()
