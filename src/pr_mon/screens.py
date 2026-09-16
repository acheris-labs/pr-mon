"""Modal dialogs: add repo, confirm, PR action menu, merge method choice."""

from collections.abc import Awaitable, Callable
from dataclasses import dataclass

from rich.text import Text
from textual import on, work
from textual.app import ComposeResult
from textual.binding import Binding
from textual.containers import Vertical
from textual.screen import ModalScreen
from textual.widgets import Checkbox, Input, Label, Static

from pr_mon.models import MergeMethod, PullRequest, RepoInfo, Status

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
    kind: str  # "merge" | "update"
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
            if self.can_update:
                yield Static("[b]\\[u][/b] Update branch", id="update")
            if self.offers_delete:
                yield Checkbox(f"Delete remote branch {self.pr.head_ref}", value=True, id="delete")
            yield Static("esc: close", classes="hint")

    def _delete_branch(self) -> bool:
        return self.offers_delete and self.query_one("#delete", Checkbox).value

    def action_merge(self) -> None:
        if not self.can_merge:
            self.app.bell()
            return
        methods = self.repo.merge_methods
        if len(methods) == 1:
            self.dismiss(Action("merge", methods[0], self._delete_branch()))
            return

        def chosen(method: MergeMethod | None) -> None:
            if method is not None:
                self.dismiss(Action("merge", method, self._delete_branch()))

        self.app.push_screen(MergeMethodScreen(methods), chosen)

    def action_update(self) -> None:
        if not self.can_update:
            self.app.bell()
            return
        self.dismiss(Action("update"))

    def action_cancel(self) -> None:
        self.dismiss(None)
