"""Detects PR changes between polls and tracks what the user hasn't seen."""

from dataclasses import dataclass
from enum import StrEnum

from pr_mon.models import BLOCKED_STATUSES, RepoInfo, Status


class EventKind(StrEnum):
    NEW = "NEW"
    READY = "READY"
    BLOCKED = "BLOCKED"


@dataclass(frozen=True)
class Change:
    """A PR's status moved; `old` is None for a PR with no real status yet."""

    number: int
    old: Status | None
    new: Status


@dataclass(frozen=True)
class Event:
    kind: EventKind
    repo: str
    number: int


class Tracker:
    """Operates on `records` in place so the caller can persist the same dict.

    Shape: {repo: {"<pr number>": {"status": str, "seen": bool}}}
    """

    def __init__(self, records: dict):
        self.records = records

    def _repo(self, repo: str) -> dict:
        entries = self.records.get(repo)
        if entries is None:
            return {}
        if not isinstance(entries, dict) or not all(isinstance(e, dict) for e in entries.values()):
            entries = self.records[repo] = {}
        return entries

    def changes(self, repo: RepoInfo) -> list[Change]:
        """Status changes `update` would record for this poll, without recording them."""
        previous = self._repo(repo.name)
        changes = []
        for pr in repo.prs:
            status = pr.status
            if status == Status.CHECKING:
                continue
            old = (previous.get(str(pr.number)) or {}).get("status")
            old_status = Status(old) if old in Status.__members__ else None
            if old_status == Status.CHECKING:
                old_status = None
            if status != old_status:
                changes.append(Change(pr.number, old_status, status))
        return changes

    def update(self, repo: RepoInfo) -> list[Event]:
        previous = self._repo(repo.name)
        current = {}
        events = []
        for pr in repo.prs:
            key = str(pr.number)
            old = previous.get(key)
            status = pr.status
            if old is None:
                kind = EventKind.NEW
            elif status == Status.CHECKING:
                # Keep the last real status so a recompute blip doesn't re-fire events.
                current[key] = old
                continue
            elif status == old.get("status"):
                kind = None
            elif status == Status.READY:
                kind = EventKind.READY
            elif status in BLOCKED_STATUSES and old.get("status") not in BLOCKED_STATUSES:
                kind = EventKind.BLOCKED
            else:
                kind = None
            seen = bool(old and old.get("seen")) and kind is None
            current[key] = {"status": str(status), "seen": seen}
            if kind:
                events.append(Event(kind, repo.name, pr.number))
        self.records[repo.name] = current
        return events

    def mark_seen(self, repo: str, number: int) -> bool:
        entry = self._repo(repo).get(str(number))
        if entry is None or entry.get("seen"):
            return False
        entry["seen"] = True
        return True

    def is_unseen(self, repo: str, number: int) -> bool:
        return number in self.unseen(repo)

    def unseen(self, repo: str) -> set[int]:
        return {int(k) for k, e in self._repo(repo).items() if not e.get("seen")}

    def forget(self, repo: str) -> None:
        self.records.pop(repo, None)
