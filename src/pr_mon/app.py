"""The pr-mon Textual application."""

from datetime import UTC, datetime
from pathlib import Path

from rich.text import Text
from textual import on, work
from textual.app import App, ComposeResult
from textual.binding import Binding
from textual.containers import Horizontal, Vertical
from textual.widgets import DataTable, Footer, Header, Static, Tree
from textual.widgets.tree import TreeNode

from pr_mon.config import Config, load_config, save_config
from pr_mon.github import GitHubError, RateLimitError
from pr_mon.models import PullRequest, RepoInfo, Status
from pr_mon.screens import Action, ActionMenuScreen, AddRepoScreen, ConfirmScreen
from pr_mon.state import AppState, load_state, save_state
from pr_mon.tracker import Tracker
from pr_mon.views import owner_label, pr_details, pr_row, pr_table_title, repo_label

CHECKING_RETRY_DELAY = 5
CHECKING_RETRIES = 3


def owner_key(repo: str) -> str:
    return repo.partition("/")[0].lower()


class RepoTree(Tree[str]):
    """Repos grouped under owner rows.

    Owner node data is the lower-cased owner (no slash); repo node data is owner/name.
    """

    BINDINGS = [
        Binding("D", "app.remove_repo", "Remove repo"),
        Binding("left", "collapse_or_parent", "Collapse", show=False),
        Binding("right", "expand", "Expand", show=False),
    ]

    def action_collapse_or_parent(self) -> None:
        node = self.cursor_node
        if node is None:
            return
        if node.parent is not None and node.parent is not self.root:
            self.move_cursor(node.parent)
        elif node.is_expanded:
            node.collapse()

    def action_expand(self) -> None:
        node = self.cursor_node
        if node is not None and node.allow_expand:
            node.expand()


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
        self.state = AppState()
        self.tracker = Tracker(self.state.prs)
        self.repos: dict[str, RepoInfo] = {}
        self.errors: dict[str, str] = {}
        self.paused_until: datetime | None = None
        self.shown_repo: str | None = None
        self.repo_nodes: dict[str, TreeNode[str]] = {}
        self.owner_nodes: dict[str, TreeNode[str]] = {}

    def compose(self) -> ComposeResult:
        yield Header()
        with Horizontal():
            yield RepoTree("repos", id="repos")
            with Vertical():
                yield PrTable(id="prs", cursor_type="row", zebra_stripes=True)
                yield Static(id="details")
        yield Footer()

    def on_mount(self) -> None:
        tree = self.query_one("#repos", RepoTree)
        tree.border_title = "Repos"
        tree.show_root = False
        self.query_one("#details").border_title = "Details"
        table = self.query_one("#prs", PrTable)
        table.add_columns("", "#", "Status", "Author", "Title")
        self.config, config_warning = load_config(self.config_path)
        self.state, state_warning = load_state(self.state_path)
        self.tracker = Tracker(self.state.prs)
        for warning in (config_warning, state_warning):
            if warning:
                self.notify(warning, severity="warning", timeout=15)
        self.render_repo_tree()
        self.render_prs()
        self.action_refresh_all()
        self.set_interval(self.config.poll_interval, self.action_refresh_all)

    async def on_unmount(self) -> None:
        await self.client.aclose()

    # ----- selection helpers -----

    @property
    def selected_repo(self) -> str | None:
        node = self.query_one("#repos", RepoTree).cursor_node
        if node is None or node.data is None or "/" not in node.data:
            return None
        return node.data

    def save_state(self) -> None:
        save_state(self.state_path, self.state)

    @property
    def selected_pr(self) -> PullRequest | None:
        repo = self.repos.get(self.selected_repo or "")
        table = self.query_one("#prs", PrTable)
        if repo is None or not repo.prs or table.row_count == 0:
            return None
        return repo.prs[min(table.cursor_row, len(repo.prs) - 1)]

    # ----- rendering -----

    def unseen_prs(self, name: str) -> list[PullRequest]:
        repo = self.repos.get(name)
        unseen = self.tracker.unseen(name)
        return [pr for pr in repo.prs if pr.number in unseen] if repo else []

    def repo_node_label(self, name: str) -> Text:
        short = name.partition("/")[2]
        return repo_label(short, self.unseen_prs(name), self.errors.get(name), name in self.repos)

    def owner_node_label(self, key: str) -> Text:
        names = [n for n in self.config.repos if owner_key(n) == key]
        unseen = [pr for n in names for pr in self.unseen_prs(n)]
        owner = names[0].partition("/")[0]
        return owner_label(owner, unseen, any(n in self.errors for n in names))

    def render_repo_tree(self, select: str | None = None) -> None:
        """Rebuild the tree; keep the cursor on `select` (a node's data) or the current row."""
        tree = self.query_one("#repos", RepoTree)
        current = tree.cursor_node.data if tree.cursor_node else None
        target = select or current
        tree.clear()
        self.repo_nodes.clear()
        self.owner_nodes.clear()
        for name in sorted(self.config.repos, key=lambda n: (owner_key(n), n.lower())):
            key = owner_key(name)
            if key not in self.owner_nodes:
                self.owner_nodes[key] = tree.root.add(
                    self.owner_node_label(key),
                    data=key,
                    expand=key not in self.state.collapsed,
                )
            self.repo_nodes[name] = self.owner_nodes[key].add_leaf(
                self.repo_node_label(name), data=name
            )
        node = self.repo_nodes.get(target) or self.owner_nodes.get(target or "")
        if node is None and target and "/" in target:
            node = self.owner_nodes.get(owner_key(target))
        if node is None:
            visible = [n for n in self.repo_nodes.values() if n.parent.is_expanded]
            node = visible[0] if visible else next(iter(self.owner_nodes.values()), None)
        if node is not None:
            if node.parent is not None and node.parent is not tree.root:
                node.parent.expand()
            self.call_after_refresh(tree.move_cursor, node)

    def render_repo_label(self, name: str) -> None:
        node = self.repo_nodes.get(name)
        if node is not None:
            node.set_label(self.repo_node_label(name))
            key = owner_key(name)
            self.owner_nodes[key].set_label(self.owner_node_label(key))

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
            hint = "Select a repository" if self.config.repos else "Press A to add a repository"
            details.update(Text(hint, style="dim"))
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
            self.save_state()
            self.render_repo_label(name)
            table = self.query_one("#prs", PrTable)
            for column, cell in enumerate(pr_row(pr, False)):
                table.update_cell_at((table.cursor_row, column), cell)

    # ----- events -----

    @on(Tree.NodeHighlighted, "#repos")
    def repo_highlighted(self) -> None:
        # The tree re-posts highlights for the same row when groups above it expand.
        if self.selected_repo == self.shown_repo and self.shown_repo is not None:
            return
        self.shown_repo = self.selected_repo
        self.query_one("#prs", PrTable).move_cursor(row=0)
        self.render_prs()

    @on(Tree.NodeSelected, "#repos")
    def repo_selected(self, event: Tree.NodeSelected) -> None:
        if event.node.data and "/" in event.node.data:
            self.query_one("#prs", PrTable).focus()

    @on(Tree.NodeExpanded, "#repos")
    @on(Tree.NodeCollapsed, "#repos")
    def owner_toggled(self, event: Tree.NodeExpanded | Tree.NodeCollapsed) -> None:
        key = event.node.data
        if key is None or "/" in key:
            return
        collapsed = set(self.state.collapsed)
        if event.node.is_collapsed:
            collapsed.add(key)
        else:
            collapsed.discard(key)
        if collapsed != set(self.state.collapsed):
            self.state.collapsed = sorted(collapsed)
            self.save_state()

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
        if self.tracker.update(repo):
            owner = self.owner_nodes.get(owner_key(name))
            if owner is not None and owner.is_collapsed:
                owner.expand()
        self.save_state()
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
            elif action.kind == "auto_merge_on":
                await self.client.enable_auto_merge(pr.id, action.method)
                self.notify(f"Auto-merge enabled for {label} ({action.method.lower()})")
            elif action.kind == "auto_merge_off":
                await self.client.disable_auto_merge(pr.id)
                self.notify(f"Auto-merge disabled for {label}")
            else:
                await self.client.update_branch(pr.id)
                self.notify(f"Updated branch for {label}")
        except GitHubError as e:
            what = action.kind.replace("_", " ").capitalize()
            self.notify(f"{what} failed for {label}: {e}", severity="error")
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
            self.render_repo_tree(select=repo.name)
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
            ordered = list(self.repo_nodes)
            index = ordered.index(name)
            neighbors = ordered[index + 1 :] + ordered[:index][::-1]
            self.config.repos.remove(name)
            self.repos.pop(name, None)
            self.errors.pop(name, None)
            self.tracker.forget(name)
            save_config(self.config_path, self.config)
            self.save_state()
            self.render_repo_tree(select=neighbors[0] if neighbors else None)
            self.render_prs()

        self.push_screen(ConfirmScreen(f"Stop monitoring {name}?"), confirmed)
