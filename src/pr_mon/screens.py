"""Modal dialogs: add repo, confirm, PR action menu, merge method choice, notifications."""

from collections.abc import Awaitable, Callable
from dataclasses import dataclass
from pathlib import Path

from rich.text import Text
from textual import on, work
from textual.app import ComposeResult
from textual.binding import Binding
from textual.containers import Horizontal, Vertical
from textual.screen import ModalScreen
from textual.widgets import (
    Button,
    Checkbox,
    Input,
    Label,
    Static,
    TabbedContent,
    TabPane,
    Tabs,
)

from pr_mon.config import EVENT_NAMES, NotifyConfig
from pr_mon.models import MergeMethod, PullRequest, RepoInfo, Status
from pr_mon.notify import VARIABLE_NAMES, render, sample_variables, unknown_placeholders

MODAL_CSS = """
{name} {{
    align: center middle;
}}
{name} > Vertical {{
    width: 70;
    height: auto;
    border: thick $accent;
    background: $surface;
    padding: 1 2;
}}
{name} .error {{
    color: $error;
}}
{name} .hint {{
    color: $text-muted;
    margin-top: 1;
}}
"""

METHOD_KEYS = {
    MergeMethod.SQUASH: ("s", "Squash and merge"),
    MergeMethod.MERGE: ("m", "Create a merge commit"),
    MergeMethod.REBASE: ("r", "Rebase and merge"),
}


@dataclass(frozen=True)
class Action:
    kind: str  # "merge" | "update" | "auto_merge_on" | "auto_merge_off"
    method: MergeMethod | None = None
    delete_branch: bool = False


class AddRepoScreen(ModalScreen[RepoInfo | None]):
    """Prompt for owner/name; `validate` returns the repo or raises with a message."""

    DEFAULT_CSS = MODAL_CSS.format(name="AddRepoScreen")
    BINDINGS = [Binding("escape", "cancel", "Cancel")]

    def __init__(self, validate: Callable[[str], Awaitable[RepoInfo]]):
        super().__init__()
        self.validate = validate

    def compose(self) -> ComposeResult:
        with Vertical():
            yield Label("Add repository (owner/name)")
            yield Input(placeholder="owner/name", id="repo-name")
            yield Static("", id="error", classes="error")
            yield Static("enter: add   esc: cancel", classes="hint")

    @on(Input.Submitted)
    def submitted(self, event: Input.Submitted) -> None:
        name = event.value.strip()
        if name.count("/") != 1 or name.startswith("/") or name.endswith("/"):
            self.query_one("#error", Static).update("Enter the repo as owner/name")
            return
        self.query_one("#error", Static).update("Checking…")
        self.check(name)

    @work(exclusive=True)
    async def check(self, name: str) -> None:
        try:
            repo = await self.validate(name)
        except Exception as e:  # noqa: BLE001 - any failure is shown to the user
            self.query_one("#error", Static).update(str(e))
            return
        self.dismiss(repo)

    def action_cancel(self) -> None:
        self.dismiss(None)


class ConfirmScreen(ModalScreen[bool]):
    DEFAULT_CSS = MODAL_CSS.format(name="ConfirmScreen")
    BINDINGS = [
        Binding("y", "answer(True)", "Yes"),
        Binding("n", "answer(False)", "No"),
        Binding("escape", "answer(False)", "No"),
    ]

    def __init__(self, message: str):
        super().__init__()
        self.message = message

    def compose(self) -> ComposeResult:
        with Vertical():
            yield Label(self.message)
            yield Static("y: yes   n/esc: no", classes="hint")

    def action_answer(self, answer: bool) -> None:
        self.dismiss(answer)


class MergeMethodScreen(ModalScreen[MergeMethod | None]):
    DEFAULT_CSS = MODAL_CSS.format(name="MergeMethodScreen")
    BINDINGS = [
        Binding("s", "choose('SQUASH')", show=False),
        Binding("m", "choose('MERGE')", show=False),
        Binding("r", "choose('REBASE')", show=False),
        Binding("escape", "cancel", "Cancel"),
    ]

    def __init__(self, methods: tuple[MergeMethod, ...]):
        super().__init__()
        self.methods = methods

    def compose(self) -> ComposeResult:
        with Vertical():
            yield Label("Merge method")
            for method in self.methods:
                key, text = METHOD_KEYS[method]
                yield Static(f"[b]\\[{key}][/b] {text}", id=f"method-{key}")
            yield Static("esc: back", classes="hint")

    def action_choose(self, method: str) -> None:
        if MergeMethod(method) in self.methods:
            self.dismiss(MergeMethod(method))

    def action_cancel(self) -> None:
        self.dismiss(None)


class ActionMenuScreen(ModalScreen[Action | None]):
    DEFAULT_CSS = MODAL_CSS.format(name="ActionMenuScreen")
    BINDINGS = [
        Binding("m", "merge", "Merge", show=False),
        Binding("u", "update", "Update branch", show=False),
        Binding("a", "auto_merge", "Auto-merge", show=False),
        Binding("escape", "cancel", "Close"),
    ]

    def __init__(self, repo: RepoInfo, pr: PullRequest):
        super().__init__()
        self.repo = repo
        self.pr = pr

    @property
    def can_merge(self) -> bool:
        return self.pr.status == Status.READY and bool(self.repo.merge_methods)

    @property
    def can_update(self) -> bool:
        return self.pr.merge_state == "BEHIND"

    @property
    def auto_merge_blocker(self) -> str | None:
        """Why auto-merge can't be enabled, or None if it can."""
        if not self.repo.auto_merge_allowed:
            return "disabled in repo settings"
        if not self.repo.merge_methods:
            return "no merge methods allowed"
        if self.pr.is_draft:
            return "draft PR"
        if self.pr.status == Status.READY:
            return "already mergeable — use m"
        return None

    def auto_merge_line(self) -> Static:
        if self.pr.auto_merge:
            return Static("[b]\\[a][/b] Disable auto-merge", id="auto")
        blocker = self.auto_merge_blocker
        if blocker:
            return Static(Text(f"[a] Auto-merge — unavailable ({blocker})", style="dim"), id="auto")
        note = (
            ""
            if self.repo.delete_branch_on_merge
            else " [dim](branch won't be deleted: repo doesn't auto-delete)[/dim]"
        )
        return Static(f"[b]\\[a][/b] Enable auto-merge{note}", id="auto")

    @property
    def offers_delete(self) -> bool:
        return self.can_merge and not self.repo.delete_branch_on_merge and bool(self.pr.head_ref_id)

    def compose(self) -> ComposeResult:
        with Vertical():
            yield Label(Text(f"#{self.pr.number} {self.pr.title}", style="bold"))
            if self.can_merge:
                yield Static("[b]\\[m][/b] Merge", id="merge")
            else:
                why = [r.text for r in self.pr.reasons if r.level != "info"]
                if not self.repo.merge_methods:
                    why.append("no merge methods allowed")
                detail = f"{self.pr.status}: {', '.join(why)}" if why else str(self.pr.status)
                yield Static(Text(f"[m] Merge — unavailable ({detail})", style="dim"), id="merge")
            yield self.auto_merge_line()
            if self.can_update:
                yield Static("[b]\\[u][/b] Update branch", id="update")
            if self.offers_delete:
                yield Checkbox(f"Delete remote branch {self.pr.head_ref}", value=True, id="delete")
            yield Static("esc: close", classes="hint")

    def _delete_branch(self) -> bool:
        return self.offers_delete and self.query_one("#delete", Checkbox).value

    def _dismiss_with_method(self, kind: str, delete_branch: bool = False) -> None:
        """Dismiss with `kind`, asking for a merge method if the repo allows several."""
        methods = self.repo.merge_methods
        if len(methods) == 1:
            self.dismiss(Action(kind, methods[0], delete_branch))
            return

        def chosen(method: MergeMethod | None) -> None:
            if method is not None:
                self.dismiss(Action(kind, method, delete_branch))

        self.app.push_screen(MergeMethodScreen(methods), chosen)

    def action_merge(self) -> None:
        if not self.can_merge:
            self.app.bell()
            return
        self._dismiss_with_method("merge", self._delete_branch())

    def action_auto_merge(self) -> None:
        if self.pr.auto_merge:
            self.dismiss(Action("auto_merge_off"))
        elif self.auto_merge_blocker:
            self.app.bell()
        else:
            self._dismiss_with_method("auto_merge_on")

    def action_update(self) -> None:
        if not self.can_update:
            self.app.bell()
            return
        self.dismiss(Action("update"))

    def action_cancel(self) -> None:
        self.dismiss(None)


EVENT_LABELS = {
    "READY": "Ready (mergeable)",
    "FAILING": "Failing (checks failed)",
    "CONFLICT": "Conflict (merge conflicts)",
    "BLOCKED": "Blocked (reviews / branch protection)",
    "BEHIND": "Behind base branch",
    "PENDING": "Pending (checks running)",
    "NEW": "New PR opened",
}

SCRIPT_HELP = (
    "The message is sent on stdin. PR_* variables are also set in the environment.\n"
    "Runs without a shell: use full paths or ~ (no $VARS)."
)


class NotificationsScreen(ModalScreen[NotifyConfig | None]):
    """Edit one repo's notification settings; `send_test` fires the unsaved settings."""

    DEFAULT_CSS = (
        MODAL_CSS.format(name="NotificationsScreen")
        + """
    NotificationsScreen > Vertical {
        width: 90;
    }
    NotificationsScreen TabbedContent {
        height: auto;
    }
    NotificationsScreen TabPane {
        height: auto;
        padding: 1 0 0 0;
    }
    NotificationsScreen .buttons {
        height: auto;
        margin-top: 1;
        align-horizontal: right;
    }
    NotificationsScreen Button {
        margin-left: 1;
    }
    """
    )
    BINDINGS = [
        Binding("ctrl+s", "save", "Save"),
        Binding("escape", "cancel", "Cancel"),
        # Text fields keep left/right for the cursor; the tab bar handles its own.
        Binding("left", "switch_tab(-1)", "Previous tab", show=False),
        Binding("right", "switch_tab(1)", "Next tab", show=False),
        Binding("up", "app.focus_previous", "Previous", show=False),
        Binding("down", "app.focus_next", "Next", show=False),
    ]

    def __init__(
        self,
        repo: str,
        settings: NotifyConfig,
        notifier: str | None,
        send_test: Callable[[NotifyConfig], None],
    ):
        super().__init__()
        self.repo = repo
        self.settings = settings
        self.notifier = notifier
        self.send_test = send_test

    def compose(self) -> ComposeResult:
        s = self.settings
        with Vertical():
            yield Label(Text(f"Notifications — {self.repo}", style="bold"), id="title")
            with TabbedContent():
                with TabPane("Message", id="tab-message"):
                    yield Label("Template")
                    yield Input(value=s.message, id="message")
                    yield Static(
                        "Variables: " + " ".join(f"{{{{{v}}}}}" for v in VARIABLE_NAMES),
                        classes="hint",
                        markup=False,
                    )
                    yield Static("", id="preview", markup=False)
                    yield Static("", id="unknown", classes="error", markup=False)
                with TabPane("Events", id="tab-events"):
                    yield Label("Notify when a PR becomes:")
                    for name in EVENT_NAMES:
                        yield Checkbox(
                            EVENT_LABELS[name], value=name in s.events, id=f"event-{name.lower()}"
                        )
                    yield Checkbox("Include draft PRs", value=s.include_drafts, id="include-drafts")
                with TabPane("Script", id="tab-script"):
                    yield Checkbox("Run script", value=s.script_enabled, id="script-enabled")
                    yield Input(value=s.script, placeholder="e.g. im --deliver tgram", id="script")
                    yield Static(SCRIPT_HELP, id="script-help", classes="hint", markup=False)
                with TabPane("Desktop", id="tab-desktop"):
                    yield Checkbox(
                        "Show desktop notification",
                        value=s.desktop_enabled and self.notifier is not None,
                        disabled=self.notifier is None,
                        id="desktop-enabled",
                    )
                    if self.notifier:
                        found = f"Using {Path(self.notifier).name} ({self.notifier})"
                    else:
                        found = "No notifier found (install terminal-notifier or notify-send)"
                    yield Static(found, id="notifier", classes="hint", markup=False)
            with Horizontal(classes="buttons"):
                yield Button("Send test", id="test")
                yield Button("Save", variant="primary", id="save")
                yield Button("Cancel", id="cancel")

    def on_mount(self) -> None:
        self.update_preview()
        self.query_one(Tabs).focus()

    def action_switch_tab(self, step: int) -> None:
        if isinstance(self.focused, Tabs):
            return  # the tab bar already switched on this key
        tabs = self.query_one(TabbedContent)
        panes = [pane.id for pane in tabs.query(TabPane)]
        tabs.active = panes[(panes.index(tabs.active) + step) % len(panes)]
        pane = tabs.get_pane(tabs.active)
        # Park focus on the tab bar so it never sits in a hidden pane (which makes
        # TabbedContent switch back), then enter the new tab once it is visible.
        self.query_one(Tabs).focus()

        def enter_pane() -> None:
            controls = [w for w in pane.query("Checkbox, Input") if w.focusable]
            if controls:
                controls[0].focus()

        self.call_after_refresh(enter_pane)

    @on(Input.Changed, "#message")
    def update_preview(self) -> None:
        template = self.query_one("#message", Input).value
        self.query_one("#preview", Static).update(
            "Preview: " + render(template, sample_variables(self.repo))
        )
        unknown = unknown_placeholders(template)
        self.query_one("#unknown", Static).update(
            f"Unknown placeholders: {', '.join(unknown)}" if unknown else ""
        )

    def current_settings(self) -> NotifyConfig:
        def checked(widget_id: str) -> bool:
            return self.query_one(f"#{widget_id}", Checkbox).value

        desktop = checked("desktop-enabled") if self.notifier else self.settings.desktop_enabled
        return NotifyConfig(
            message=self.query_one("#message", Input).value,
            events=[name for name in EVENT_NAMES if checked(f"event-{name.lower()}")],
            include_drafts=checked("include-drafts"),
            script_enabled=checked("script-enabled"),
            script=self.query_one("#script", Input).value,
            desktop_enabled=desktop,
        )

    @on(Button.Pressed, "#test")
    def action_test(self) -> None:
        self.send_test(self.current_settings())

    @on(Button.Pressed, "#save")
    def action_save(self) -> None:
        self.dismiss(self.current_settings())

    @on(Button.Pressed, "#cancel")
    def action_cancel(self) -> None:
        self.dismiss(None)
