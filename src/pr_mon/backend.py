"""The interface the TUI uses to reach the monitoring service, in-process or remote."""

from collections.abc import Callable
from dataclasses import dataclass, field
from typing import Protocol

from pr_mon.config import Config, NotifyConfig
from pr_mon.models import Action, ArmedMerge, RepoInfo


class BackendError(Exception):
    """A command failed; the message is meant for the user."""


@dataclass
class BackendStatus:
    connected: bool = True
    pid: int | None = None
    version: str = ""
    notifier: str | None = None
    last_update: str | None = None  # ISO-8601 UTC
    rate_limited_until: str | None = None  # ISO-8601 UTC
    warnings: list[str] = field(default_factory=list)


@dataclass(frozen=True)
class BackendEvent:
    """Something changed. Kinds: repos, repo, seen, collapsed, config, status, toast,
    disconnected. `name` is the repo for repo/seen events."""

    kind: str
    name: str | None = None
    message: str = ""
    severity: str = "information"


Listener = Callable[[BackendEvent], None]


class Backend(Protocol):
    config: Config
    repos: dict[str, RepoInfo]
    errors: dict[str, str]
    status: BackendStatus

    @property
    def collapsed(self) -> set[str]: ...

    def unseen(self, name: str) -> set[int]: ...

    def armed(self, name: str) -> dict[int, ArmedMerge]: ...

    def add_listener(self, listener: Listener) -> None: ...

    async def refresh_all(self) -> None: ...

    async def add_repo(self, name: str) -> str: ...

    async def remove_repo(self, name: str) -> None: ...

    async def mark_seen(self, name: str, number: int) -> None: ...

    async def set_collapsed(self, owner: str, collapsed: bool) -> None: ...

    async def perform(self, repo: str, number: int, action: Action) -> None: ...

    async def save_notifications(self, repo: str, settings: NotifyConfig) -> None: ...

    async def send_test(self, repo: str, settings: NotifyConfig) -> None: ...

    async def shutdown(self) -> None: ...
