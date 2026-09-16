"""Pure rendering helpers: turn models into Rich text for the widgets."""

from datetime import datetime

from rich.text import Text

from pr_mon.models import BLOCKED_STATUSES, PullRequest, RepoInfo, Status

STATUS_STYLE = {
    Status.READY: ("✓", "bold green"),
    Status.CONFLICT: ("✗", "bold red"),
    Status.FAILING: ("✗", "bold red"),
    Status.BLOCKED: ("⚠", "bold red"),
    Status.BEHIND: ("↓", "yellow"),
    Status.PENDING: ("⋯", "yellow"),
    Status.CHECKING: ("?", "dim"),
    Status.DRAFT: ("◌", "dim"),
}

REASON_STYLE = {"error": ("✗", "red"), "warning": ("⚠", "yellow"), "info": ("•", "dim")}


def badge_style(prs: list[PullRequest]) -> str:
    statuses = {pr.status for pr in prs}
    if Status.READY in statuses:
        return "bold green"
    if statuses & BLOCKED_STATUSES:
        return "bold red"
    return "bold yellow"


def _badged(name: str, unseen: list[PullRequest], error: bool, name_style: str) -> Text:
    label = Text()
    if error:
        label.append("⚠ ", style="bold red")
    elif unseen:
        label.append("● ", style=badge_style(unseen))
    else:
        label.append("  ")
    label.append(name, style=name_style)
    if unseen:
        label.append(f" ({len(unseen)})", style=badge_style(unseen))
    return label


def repo_label(name: str, unseen: list[PullRequest], error: str | None, loaded: bool) -> Text:
    return _badged(name, unseen, bool(error), "" if loaded or error else "dim")


def owner_label(owner: str, unseen: list[PullRequest], error: bool) -> Text:
    """Owner group row; `unseen` and `error` roll up from all of its repos."""
    return _badged(f"{owner}/", unseen, error, "bold")


def pr_row(pr: PullRequest, unseen: bool) -> tuple[Text, Text, Text, Text, Text]:
    icon, style = STATUS_STYLE[pr.status]
    return (
        Text(icon, style=style),
        Text(f"#{pr.number}", style="bold" if unseen else ""),
        Text(str(pr.status), style=style) + Text(" auto" if pr.auto_merge else "", style="cyan"),
        Text(pr.author, style="dim"),
        Text(pr.title, style="bold" if unseen else ""),
    )


def format_time(iso: str, now: datetime) -> str:
    """Local timestamp plus a rough age, e.g. '2026-09-16 09:10 (2h ago)'."""
    when = datetime.fromisoformat(iso)
    seconds = max(0, int((now - when).total_seconds()))
    if seconds < 60:
        age = "just now"
    elif seconds < 3600:
        age = f"{seconds // 60}m ago"
    elif seconds < 86400:
        age = f"{seconds // 3600}h ago"
    else:
        age = f"{seconds // 86400}d ago"
    return f"{when.astimezone().strftime('%Y-%m-%d %H:%M')} ({age})"


def pr_details(repo: RepoInfo, pr: PullRequest, now: datetime) -> Text:
    icon, style = STATUS_STYLE[pr.status]
    text = Text()
    text.append(f"#{pr.number} {pr.title}\n", style="bold")
    text.append("Branch: ", style="dim")
    text.append(f"{pr.head_ref} → {pr.base_ref}\n", style="bold")
    text.append("Author: ", style="dim")
    text.append(f"{pr.author}\n")
    times = [
        ("Opened:      ", pr.created_at),
        ("Last commit: ", pr.last_commit_at),
        ("Last check:  ", pr.last_check_started_at),
    ]
    for label, iso in times:
        if iso:
            text.append(label, style="dim")
            text.append(f"{format_time(iso, now)}\n")
    if pr.head_sha:
        text.append("Head SHA:    ", style="dim")
        text.append(f"{pr.head_sha}\n")
    text.append(f"{pr.url}\n\n", style="dim underline")
    text.append(f"{icon} {pr.status}\n", style=style)
    if pr.auto_merge:
        method = pr.auto_merge.method.lower()
        text.append(f"Auto-merge: on ({method}, by {pr.auto_merge.enabled_by})\n", style="cyan")
    reasons = pr.reasons
    if reasons:
        text.append("\nBlocked by:\n" if pr.status != Status.READY else "\nNotes:\n")
        for reason in reasons:
            r_icon, r_style = REASON_STYLE[reason.level]
            text.append(f"  {r_icon} {reason.text}\n", style=r_style)
    elif pr.status == Status.READY:
        text.append("\nReady to merge\n", style="green")
    return text


def pr_table_title(repo: RepoInfo | None) -> str:
    if repo is None:
        return "PRs"
    if repo.pr_total > len(repo.prs):
        return f"PRs — {repo.name} (newest {len(repo.prs)} of {repo.pr_total})"
    return f"PRs — {repo.name} ({repo.pr_total})"
