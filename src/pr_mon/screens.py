"""Modal dialogs: add repo, confirm, PR action menu, merge method choice, notifications."""

from collections.abc import Awaitable, Callable
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

from pr_mon.config import NotifyConfig
from pr_mon.models import (
    Action,
    MergeMethod,
    NotificationForm,
    NotificationPreview,
    PullRequest,
    RepoInfo,
)

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


class AddRepoScreen(ModalScreen[str | None]):
    """Prompt for owner/name; `add` returns the canonical name or raises with a message."""

    DEFAULT_CSS = MODAL_CSS.format(name="AddRepoScreen")
    BINDINGS = [Binding("escape", "cancel", "Cancel")]

    def __init__(self, add: Callable[[str], Awaitable[str]]):
        super().__init__()
        self.add = add

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
            added = await self.add(name)
        except Exception as e:  # noqa: BLE001 - any failure is shown to the user
            self.query_one("#error", Static).update(str(e))
            return
        self.dismiss(added)

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


# (option key, shortcut, widget id) in display order.
MENU_SLOTS = (("merge", "m", "merge"), ("auto_merge", "a", "auto"), ("update", "u", "update"))


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
        # The backend decides what the menu offers; this screen only shows it.
        self.options = {option.key: option for option in pr.actions}

    @property
    def offers_delete(self) -> bool:
        return any(option.offers_delete_branch for option in self.options.values())

    def option_line(self, key: str, shortcut: str, widget_id: str) -> Static:
        option = self.options[key]
        if not option.available:
            return Static(
                Text(f"[{shortcut}] {option.label} — unavailable ({option.reason})", style="dim"),
                id=widget_id,
            )
        note = f" [dim]({option.note})[/dim]" if option.note else ""
        return Static(f"[b]\\[{shortcut}][/b] {option.label}{note}", id=widget_id)

    def compose(self) -> ComposeResult:
        with Vertical():
            yield Label(Text(f"#{self.pr.number} {self.pr.title}", style="bold"))
            for key, shortcut, widget_id in MENU_SLOTS:
                if key in self.options:
                    yield self.option_line(key, shortcut, widget_id)
            if self.offers_delete:
                yield Checkbox(f"Delete remote branch {self.pr.head_ref}", value=True, id="delete")
            yield Static("esc: close", classes="hint")

    def choose(self, key: str) -> None:
        option = self.options.get(key)
        if option is None or not option.available:
            self.app.bell()
            return
        delete_branch = option.offers_delete_branch and self.query_one("#delete", Checkbox).value
        if not option.needs_method:
            self.dismiss(Action(option.kind, None, delete_branch))
            return
        methods = self.repo.merge_methods
        if len(methods) == 1:
            self.dismiss(Action(option.kind, methods[0], delete_branch))
            return

        def chosen(method: MergeMethod | None) -> None:
            if method is not None:
                self.dismiss(Action(option.kind, method, delete_branch))

        self.app.push_screen(MergeMethodScreen(methods), chosen)

    def action_merge(self) -> None:
        self.choose("merge")

    def action_auto_merge(self) -> None:
        self.choose("auto_merge")

    def action_update(self) -> None:
        self.choose("update")

    def action_cancel(self) -> None:
        self.dismiss(None)


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
        form: NotificationForm,
        preview: Callable[[str], Awaitable[NotificationPreview]],
        send_test: Callable[[NotifyConfig], None],
    ):
        """`form` and `preview` come from the backend, which owns the notification rules."""
        super().__init__()
        self.repo = repo
        self.settings = settings
        self.notifier = notifier
        self.form = form
        self.preview = preview
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
                        "Variables: " + " ".join(f"{{{{{v}}}}}" for v in self.form.variables),
                        classes="hint",
                        markup=False,
                    )
                    yield Static("", id="preview", markup=False)
                    yield Static("", id="unknown", classes="error", markup=False)
                with TabPane("Events", id="tab-events"):
                    yield Label("Notify when a PR becomes:")
                    for event in self.form.events:
                        yield Checkbox(
                            event.label,
                            value=event.name in s.events,
                            id=f"event-{event.name.lower()}",
                        )
                    yield Checkbox("Include draft PRs", value=s.include_drafts, id="include-drafts")
                with TabPane("Script", id="tab-script"):
                    yield Checkbox("Run script", value=s.script_enabled, id="script-enabled")
                    yield Input(value=s.script, placeholder="e.g. im --deliver tgram", id="script")
                    yield Static(
                        self.form.script_help, id="script-help", classes="hint", markup=False
                    )
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
    def message_changed(self) -> None:
        self.update_preview()

    @work(exclusive=True, group="preview")
    async def update_preview(self) -> None:
        template = self.query_one("#message", Input).value
        try:
            preview = await self.preview(template)
        except Exception as e:  # noqa: BLE001 - shown in place of the preview
            self.query_one("#preview", Static).update(f"Preview unavailable: {e}")
            return
        self.query_one("#preview", Static).update("Preview: " + preview.text)
        unknown = preview.unknown
        self.query_one("#unknown", Static).update(
            f"Unknown placeholders: {', '.join(unknown)}" if unknown else ""
        )

    def current_settings(self) -> NotifyConfig:
        def checked(widget_id: str) -> bool:
            return self.query_one(f"#{widget_id}", Checkbox).value

        desktop = checked("desktop-enabled") if self.notifier else self.settings.desktop_enabled
        return NotifyConfig(
            message=self.query_one("#message", Input).value,
            events=[e.name for e in self.form.events if checked(f"event-{e.name.lower()}")],
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
