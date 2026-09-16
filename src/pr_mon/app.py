"""The pr-mon Textual application."""

from datetime import UTC, datetime
from pathlib import Path

from rich.text import Text
from textual import on, work
from textual.app import App, ComposeResult
from textual.binding import Binding
from textual.containers import Horizontal, Vertical
from textual.widgets import DataTable, Footer, Header, OptionList, Static

from pr_mon.config import Config, load_config, save_config
from pr_mon.github import GitHubError, RateLimitError
from pr_mon.models import PullRequest, RepoInfo, Status
from pr_mon.screens import Action, ActionMenuScreen, AddRepoScreen, ConfirmScreen
from pr_mon.state import load_state, save_state
from pr_mon.tracker import Tracker
from pr_mon.views import pr_details, pr_row, pr_table_title, repo_label

CHECKING_RETRY_DELAY = 5
CHECKING_RETRIES = 3


class RepoList(OptionList):
    BINDINGS = [Binding("D", "app.remove_repo", "Remove repo")]


class PrTable(DataTable):
    def on_focus(self) -> None:
        self.app.mark_current_seen()


class PrMonApp(App):
    TITLE = "pr-mon"
    CSS = """
    #repos {
        width: 36;
        height: 100%;
        border: round $primary;
    }
    #prs {
        height: 1fr;
        border: round $primary;
    }
    #details {
        height: 1fr;
        border: round $primary;
        padding: 0 1;
        overflow-y: auto;
    }
    #repos:focus, #prs:focus {
        border: round $accent;
    }
    """
    BINDINGS = [
        Binding("A", "add_repo", "Add repo"),
        Binding("r", "refresh_all", "Refresh"),
        Binding("q", "quit", "Quit"),
    ]

    def __init__(self, client, config_path: Path, state_path: Path):
        super().__init__()
        self.client = client
        self.config_path = config_path
        self.state_path = state_path
        self.config = Config()
        self.tracker = Tracker({})
        self.repos: dict[str, RepoInfo] = {}
        self.errors: dict[str, str] = {}
        self.paused_until: datetime | None = None

    def compose(self) -> ComposeResult:
        yield Header()
        with Horizontal():
            yield RepoList(id="repos")
            with Vertical():
                yield PrTable(id="prs", cursor_type="row", zebra_stripes=True)
                yield Static(id="details")
        yield Footer()

    def on_mount(self) -> None:
        self.query_one("#repos").border_title = "Repos"
        self.query_one("#details").border_title = "Details"
        table = self.query_one("#prs", PrTable)
        table.add_columns("", "#", "Status", "Author", "Title")
        self.config, config_warning = load_config(self.config_path)
        records, state_warning = load_state(self.state_path)
        self.tracker = Tracker(records)
        for warning in (config_warning, state_warning):
            if warning:
                self.notify(warning, severity="warning", timeout=15)
        self.render_repo_list()
        self.render_prs()
        self.action_refresh_all()
        self.set_interval(self.config.poll_interval, self.action_refresh_all)

    async def on_unmount(self) -> None:
        await self.client.aclose()

    # ----- selection helpers -----

    @property
    def selected_repo(self) -> str | None:
        index = self.query_one("#repos", RepoList).highlighted
        if index is None or index >= len(self.config.repos):
            return None
        return self.config.repos[index]

    @property
    def selected_pr(self) -> PullRequest | None:
        repo = self.repos.get(self.selected_repo or "")
        table = self.query_one("#prs", PrTable)
        if repo is None or not repo.prs or table.row_count == 0:
            return None
        return repo.prs[min(table.cursor_row, len(repo.prs) - 1)]

    # ----- rendering -----

    def label_for(self, name: str) -> Text:
        repo = self.repos.get(name)
        unseen_numbers = self.tracker.unseen(name)
        unseen = [pr for pr in repo.prs if pr.number in unseen_numbers] if repo else []
        return repo_label(name, unseen, self.errors.get(name), repo is not None)

    def render_repo_list(self) -> None:
        repo_list = self.query_one("#repos", RepoList)
        highlighted = repo_list.highlighted
        repo_list.clear_options()
        repo_list.add_options([self.label_for(name) for name in self.config.repos])
        if self.config.repos:
            index = min(highlighted or 0, len(self.config.repos) - 1)
            repo_list.highlighted = index

    def render_repo_label(self, name: str) -> None:
        if name in self.config.repos:
            repo_list = self.query_one("#repos", RepoList)
            repo_list.replace_option_prompt_at_index(
                self.config.repos.index(name), self.label_for(name)
            )

    def render_prs(self) -> None:
        name = self.selected_repo
        repo = self.repos.get(name or "")
        table = self.query_one("#prs", PrTable)
        cursor = table.cursor_row
        table.clear()
        table.border_title = pr_table_title(repo)
        if repo:
            unseen = self.tracker.unseen(repo.name)
            for pr in repo.prs:
                table.add_row(*pr_row(pr, pr.number in unseen), key=str(pr.number))
            if repo.prs:
                table.move_cursor(row=min(cursor, len(repo.prs) - 1))
        self.render_details()

    def render_details(self) -> None:
        details = self.query_one("#details", Static)
        name = self.selected_repo
        if name is None:
            details.update(Text("Press A to add a repository", style="dim"))
            return
        text = Text()
        if name in self.errors:
            text.append(f"⚠ {self.errors[name]}\n\n", style="bold red")
        repo = self.repos.get(name)
        pr = self.selected_pr
        if repo is None:
            text.append("Loading…" if name not in self.errors else "", style="dim")
        elif pr is None:
            text.append("No open pull requests", style="dim")
        else:
            text.append_text(pr_details(repo, pr))
        details.update(text)

    def mark_current_seen(self) -> None:
        name, pr = self.selected_repo, self.selected_pr
        if name and pr and self.tracker.mark_seen(name, pr.number):
            save_state(self.state_path, self.tracker.records)
            self.render_repo_label(name)
            table = self.query_one("#prs", PrTable)
            for column, cell in enumerate(pr_row(pr, False)):
                table.update_cell_at((table.cursor_row, column), cell)

    # ----- events -----

    @on(OptionList.OptionHighlighted, "#repos")
    def repo_highlighted(self) -> None:
        table = self.query_one("#prs", PrTable)
        table.move_cursor(row=0)
        self.render_prs()

    @on(DataTable.RowHighlighted, "#prs")
    def pr_highlighted(self) -> None:
        self.render_details()
        if self.query_one("#prs", PrTable).has_focus:
            self.mark_current_seen()

    @on(DataTable.RowSelected, "#prs")
    def pr_selected(self) -> None:
        name, pr = self.selected_repo, self.selected_pr
        if name is None or pr is None:
            return
        repo = self.repos[name]

        def chosen(action: Action | None) -> None:
            if action is not None:
                self.perform(repo, pr, action)

        self.push_screen(ActionMenuScreen(repo, pr), chosen)

    # ----- polling -----

    def action_refresh_all(self) -> None:
        if self.paused_until and datetime.now(UTC) < self.paused_until:
            return
        self.paused_until = None
        for name in self.config.repos:
            self.refresh_repo(name)

    def refresh_repo(self, name: str, attempt: int = 0) -> None:
        self.run_worker(self._refresh(name, attempt), group=f"repo:{name}", exclusive=True)

    async def _refresh(self, name: str, attempt: int = 0) -> None:
        try:
            repo = await self.client.fetch_repo(name)
        except RateLimitError as e:
            self.paused_until = e.reset_at
            local = e.reset_at.astimezone().strftime("%H:%M:%S")
            self.sub_title = f"Rate limited until {local}"
            return
        except GitHubError as e:
            self.errors[name] = str(e)
        else:
            self.errors.pop(name, None)
            self.apply(name, repo)
            if attempt < CHECKING_RETRIES and any(pr.status == Status.CHECKING for pr in repo.prs):
                self.set_timer(CHECKING_RETRY_DELAY, lambda: self.refresh_repo(name, attempt + 1))
            self.sub_title = f"Updated {datetime.now().strftime('%H:%M:%S')}"
        self.render_repo_label(name)
        if name == self.selected_repo:
            self.render_details()

    def apply(self, name: str, repo: RepoInfo) -> None:
        if name not in self.config.repos:
            return
        self.repos[name] = repo
        self.tracker.update(repo)
        save_state(self.state_path, self.tracker.records)
        if name == self.selected_repo:
            self.render_prs()
            if self.query_one("#prs", PrTable).has_focus:
                self.mark_current_seen()

    # ----- actions -----

    @work(group="actions")
    async def perform(self, repo: RepoInfo, pr: PullRequest, action: Action) -> None:
        label = f"{repo.name}#{pr.number}"
        try:
            if action.kind == "merge":
                await self.client.merge(pr.id, action.method)
                self.notify(f"Merged {label} ({action.method.lower()})")
                if action.delete_branch and pr.head_ref_id:
                    try:
                        await self.client.delete_branch(pr.head_ref_id)
                    except GitHubError as e:
                        self.notify(
                            f"Merged {label}, but couldn't delete {pr.head_ref}: {e}",
                            severity="warning",
                        )
            else:
                await self.client.update_branch(pr.id)
                self.notify(f"Updated branch for {label}")
        except GitHubError as e:
            self.notify(f"{action.kind.capitalize()} failed for {label}: {e}", severity="error")
        await self._refresh(repo.name)

    def action_add_repo(self) -> None:
        async def validate(name: str) -> RepoInfo:
            if name.lower() in (r.lower() for r in self.config.repos):
                raise ValueError(f"{name} is already being monitored")
            return await self.client.fetch_repo(name)

        def added(repo: RepoInfo | None) -> None:
            if repo is None:
                return
            self.config.repos.append(repo.name)
            save_config(self.config_path, self.config)
            self.render_repo_list()
            self.apply(repo.name, repo)
            self.render_repo_label(repo.name)

        self.push_screen(AddRepoScreen(validate), added)

    def action_remove_repo(self) -> None:
        name = self.selected_repo
        if name is None:
            return

        def confirmed(yes: bool | None) -> None:
            if not yes:
                return
            self.config.repos.remove(name)
            self.repos.pop(name, None)
            self.errors.pop(name, None)
            self.tracker.forget(name)
            save_config(self.config_path, self.config)
            save_state(self.state_path, self.tracker.records)
            self.render_repo_list()
            self.render_prs()

        self.push_screen(ConfirmScreen(f"Stop monitoring {name}?"), confirmed)
