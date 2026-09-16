"""Pure rendering helpers: turn models into Rich text for the widgets."""

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


def repo_label(name: str, unseen: list[PullRequest], error: str | None, loaded: bool) -> Text:
    label = Text()
    if error:
        label.append("⚠ ", style="bold red")
    elif unseen:
        label.append("● ", style=badge_style(unseen))
    else:
        label.append("  ")
    label.append(name, style="" if loaded or error else "dim")
    if unseen:
        label.append(f" ({len(unseen)})", style=badge_style(unseen))
    return label


def pr_row(pr: PullRequest, unseen: bool) -> tuple[Text, Text, Text, Text, Text]:
    icon, style = STATUS_STYLE[pr.status]
    return (
        Text(icon, style=style),
        Text(f"#{pr.number}", style="bold" if unseen else ""),
        Text(str(pr.status), style=style),
        Text(pr.author, style="dim"),
        Text(pr.title, style="bold" if unseen else ""),
    )


def pr_details(repo: RepoInfo, pr: PullRequest) -> Text:
    icon, style = STATUS_STYLE[pr.status]
    text = Text()
    text.append(f"#{pr.number} {pr.title}\n", style="bold")
    text.append(f"{pr.author}  {pr.head_ref} → {pr.base_ref}\n", style="dim")
    text.append(f"{pr.url}\n\n", style="dim underline")
    text.append(f"{icon} {pr.status}\n", style=style)
    reasons = pr.reasons
    if reasons:
        text.append("\nBlocked by:\n" if pr.status != Status.READY else "\nNotes:\n")
        for reason in reasons:
            r_icon, r_style = REASON_STYLE[reason.level]
            text.append(f"  {r_icon} {reason.text}\n", style=r_style)
    elif pr.status == Status.READY:
        text.append("\nReady to merge\n", style="green")
    text.append("\nenter: actions", style="dim")
    return text


def pr_table_title(repo: RepoInfo | None) -> str:
    if repo is None:
        return "PRs"
    if repo.pr_total > len(repo.prs):
        return f"PRs — {repo.name} (newest {len(repo.prs)} of {repo.pr_total})"
    return f"PRs — {repo.name} ({repo.pr_total})"
