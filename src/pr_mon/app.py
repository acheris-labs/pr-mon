"""The pr-mon Textual application: a view onto a Backend."""

import asyncio
from datetime import UTC, datetime

from rich.text import Text
from textual import on, work
from textual.app import App, ComposeResult
from textual.binding import Binding
from textual.containers import Horizontal, Vertical
from textual.content import Content
from textual.widgets import DataTable, Footer, Header, Static, Tree
from textual.widgets.tree import TreeNode

from pr_mon import __version__
from pr_mon.autostart import restart_backend
from pr_mon.backend import Backend, BackendError, BackendEvent
from pr_mon.config import Config, NotifyConfig
from pr_mon.daemon import DaemonError, DaemonPaths, spawn_daemon
from pr_mon.models import Action, PullRequest, RepoInfo, owner_key
from pr_mon.remote import BackendMismatch, BackendUnavailable
from pr_mon.screens import (
    ActionMenuScreen,
    AddRepoScreen,
    ConfirmScreen,
    NotificationsScreen,
)
from pr_mon.views import owner_label, pr_details, pr_row, pr_table_title, repo_label

RECONNECT_DELAY = 5.0
MERGE_WAIT = 3.0


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
    BINDINGS = [Binding("enter", "select_cursor", "Actions")]

    def on_focus(self) -> None:
        self.app.mark_current_seen()


class PrMonFooter(Footer):
    """Footer that pins the PR list's Actions key to the right edge."""

    DEFAULT_CSS = """
    PrMonFooter FooterKey.-actions {
        dock: right;
    }
    """

    def compose(self) -> ComposeResult:
        for widget in super().compose():
            # FooterKey isn't exported by Textual, so match on its action attribute.
            if getattr(widget, "action", None) == "select_cursor":
                widget.add_class("-actions")
            yield widget


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
        Binding("N", "notifications", "Notifications"),
        Binding("r", "refresh_all", "Refresh"),
        Binding("q", "quit", "Quit"),
        # Textual's default only shows a "press ctrl+q" hint.
        Binding("ctrl+c", "quit", "Quit", show=False, priority=True),
    ]

    def __init__(
        self,
        backend: Backend,
        owns_backend: bool = False,
        daemon: DaemonPaths | None = None,
    ):
        """Either `owns_backend` (start/stop an in-process backend with the app) or
        `daemon` (connect `backend` to that daemon, starting it if needed)."""
        super().__init__()
        self.backend = backend
        self.owns_backend = owns_backend
        self.daemon = daemon
        self.shown_repo: str | None = None
        self.repo_nodes: dict[str, TreeNode[str]] = {}
        self.owner_nodes: dict[str, TreeNode[str]] = {}

    def format_title(self, title: str, sub_title: str) -> Content:
        """Color the connection dot at the start of the subtitle."""
        colors = {"●": "bold green", "○": "bold red"}
        dot = sub_title[:1]
        if dot not in colors:
            return super().format_title(title, sub_title)
        return Content.assemble(
            Content(title),
            (" — ", "dim"),
            (dot, colors[dot]),
            Content(sub_title[1:]).stylize("dim"),
        )

    @property
    def config(self) -> Config:
        return self.backend.config

    @property
    def repos(self) -> dict[str, RepoInfo]:
        return self.backend.repos

    @property
    def errors(self) -> dict[str, str]:
        return self.backend.errors

    def compose(self) -> ComposeResult:
        yield Header()
        with Horizontal():
            yield RepoTree("repos", id="repos")
            with Vertical():
                yield PrTable(id="prs", cursor_type="row", zebra_stripes=True)
                yield Static(id="details")
        # Only one footer key can dock right, so the palette hint gives way to Actions.
        yield PrMonFooter(show_command_palette=False)

    async def on_mount(self) -> None:
        tree = self.query_one("#repos", RepoTree)
        tree.border_title = "Repos"
        tree.show_root = False
        self.query_one("#details").border_title = "Details"
        table = self.query_one("#prs", PrTable)
        table.add_columns("", "#", "Status", "Author", "Title")
        self.backend.add_listener(self.on_backend_event)
        if self.daemon is not None:
            self.sub_title = "connecting to backend…"
            self.open_session()
            return
        if self.owns_backend:
            await self.backend.start()
        self.show_warnings()
        self.render_all()

    @work(group="backend", exclusive=True)
    async def open_session(self) -> None:
        try:
            await self.connect_backend()
        except (BackendError, DaemonError) as e:
            self.exit(return_code=1, message=f"pr-mon: could not start the backend\n{e}")
            return
        if self.backend.status.connected:
            self.show_warnings()
            self.render_all()

    def show_warnings(self) -> None:
        for warning in self.backend.status.warnings:
            self.notify(warning, severity="warning", timeout=15)

    async def on_unmount(self) -> None:
        if self.owns_backend:
            await self.backend.stop()
        elif self.daemon is not None:
            await self.backend.close()

    # ----- daemon connection -----

    async def connect_backend(self, start: bool = True) -> None:
        """Connect (starting the daemon if `start` and it isn't running); restart a
        backend running another version, once it isn't in the middle of a merge."""
        try:
            try:
                await self.backend.connect(__version__)
            except BackendUnavailable:
                if not start:
                    raise
                await asyncio.to_thread(spawn_daemon, self.daemon)
                await self.backend.connect(__version__)
        except BackendMismatch as mismatch:
            old_version = mismatch.version
            if mismatch.merging:
                self.notify("The backend is finishing a merge; it will restart right after")
            while mismatch.merging:
                self.sub_title = "waiting for the backend to finish a merge…"
                await asyncio.sleep(MERGE_WAIT)
                try:
                    await self.backend.connect(__version__)
                    break  # restarted by someone else meanwhile
                except BackendMismatch as again:
                    mismatch = again
            if not self.backend.status.connected:
                self.sub_title = "restarting backend…"
                await asyncio.to_thread(restart_backend, self.daemon)
                await self.backend.connect(__version__)
                self.notify(f"Backend restarted (it was running pr-mon {old_version})")
        self.render_status()

    @work(group="backend", exclusive=True)
    async def reconnect(self) -> None:
        """Retry quietly until the backend is back (e.g. launchd restarted it)."""
        while not self.backend.status.connected:
            await asyncio.sleep(RECONNECT_DELAY)
            try:
                await self.connect_backend(start=False)
            except BackendError:
                continue
        self.notify("Backend reconnected")
        self.render_all()

    def render_all(self) -> None:
        self.render_repo_tree()
        self.render_prs()
        self.render_status()

    # ----- backend events -----

    def on_backend_event(self, event: BackendEvent) -> None:
        if event.kind == "repos":
            self.render_repo_tree()
            self.render_prs()
        elif event.kind == "repo":
            self.render_repo_label(event.name)
            if event.name == self.selected_repo:
                self.render_prs()
                if self.query_one("#prs", PrTable).has_focus:
                    self.mark_current_seen()
        elif event.kind == "seen":
            self.render_repo_label(event.name)
        elif event.kind == "collapsed":
            self.sync_collapsed()
        elif event.kind == "status":
            self.render_status()
        elif event.kind == "toast":
            self.notify(event.message, severity=event.severity)
        elif event.kind == "disconnected":
            self.notify("Backend disconnected — reconnecting…", severity="warning")
            self.render_status()
            self.reconnect()

    def run_command(self, command) -> None:
        """Run a backend coroutine in the background; show BackendError as a toast."""

        async def runner() -> None:
            try:
                await command
            except BackendError as e:
                self.notify(str(e), severity="error")

        self.run_worker(runner(), group="commands")

    # ----- selection helpers -----

    @property
    def selected_repo(self) -> str | None:
        node = self.query_one("#repos", RepoTree).cursor_node
        if node is None or node.data is None or "/" not in node.data:
            return None
        if node.data not in self.config.repos:
            return None
        return node.data

    @property
    def selected_pr(self) -> PullRequest | None:
        repo = self.repos.get(self.selected_repo or "")
        table = self.query_one("#prs", PrTable)
        if repo is None or not repo.prs or table.row_count == 0:
            return None
        return repo.prs[min(table.cursor_row, len(repo.prs) - 1)]

    # ----- rendering -----

    def render_status(self) -> None:
        status = self.backend.status
        parts = []
        if self.daemon is not None:
            if not status.connected:
                self.sub_title = "○ disconnected"
                return
            parts.append("● connected")
        if status.rate_limited_until:
            local = datetime.fromisoformat(status.rate_limited_until).astimezone()
            parts.append(f"rate limited until {local.strftime('%H:%M:%S')}")
        elif status.last_update:
            local = datetime.fromisoformat(status.last_update).astimezone()
            parts.append(f"updated {local.strftime('%H:%M:%S')}")
        self.sub_title = " · ".join(parts)

    def unseen_prs(self, name: str) -> list[PullRequest]:
        repo = self.repos.get(name)
        unseen = self.backend.unseen(name)
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
        collapsed = self.backend.collapsed
        tree.clear()
        self.repo_nodes.clear()
        self.owner_nodes.clear()
        for name in sorted(self.config.repos, key=lambda n: (owner_key(n), n.lower())):
            key = owner_key(name)
            if key not in self.owner_nodes:
                self.owner_nodes[key] = tree.root.add(
                    self.owner_node_label(key), data=key, expand=key not in collapsed
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

    def sync_collapsed(self) -> None:
        collapsed = self.backend.collapsed
        for key, node in self.owner_nodes.items():
            if key in collapsed and node.is_expanded:
                node.collapse()
            elif key not in collapsed and node.is_collapsed:
                node.expand()

    def render_repo_label(self, name: str) -> None:
        node = self.repo_nodes.get(name)
        if node is not None and name in self.config.repos:
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
            unseen = self.backend.unseen(repo.name)
            armed = self.backend.armed(repo.name)
            for pr in repo.prs:
                row = pr_row(pr, pr.number in unseen, pr.number in armed)
                table.add_row(*row, key=str(pr.number))
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
            armed = self.backend.armed(name).get(pr.number)
            text.append_text(pr_details(repo, pr, datetime.now(UTC), armed))
        details.update(text)

    def mark_current_seen(self) -> None:
        name, pr = self.selected_repo, self.selected_pr
        if name and pr and pr.number in self.backend.unseen(name):
            table = self.query_one("#prs", PrTable)
            armed = pr.number in self.backend.armed(name)
            for column, cell in enumerate(pr_row(pr, False, armed)):
                table.update_cell_at((table.cursor_row, column), cell)
            self.run_command(self.backend.mark_seen(name, pr.number))

    # ----- UI events -----

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
        collapsed = event.node.is_collapsed
        if collapsed != (key in self.backend.collapsed):
            self.run_command(self.backend.set_collapsed(key, collapsed))

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
                self.run_command(self.backend.perform(name, pr.number, action))

        self.push_screen(ActionMenuScreen(repo, pr), chosen)

    # ----- commands -----

    def action_open_link(self, url: str) -> None:
        if url.startswith("https://"):
            self.open_url(url)

    def action_refresh_all(self) -> None:
        self.run_command(self.backend.refresh_all())

    def action_notifications(self) -> None:
        name = self.selected_repo
        if name is None:
            self.notify("Select a repository to configure its notifications", severity="warning")
            return
        self.open_notifications(name)

    @work(group="commands")
    async def open_notifications(self, name: str) -> None:
        try:
            form = await self.backend.notification_form()
        except BackendError as e:
            self.notify(str(e), severity="error")
            return

        def saved(settings: NotifyConfig | None) -> None:
            if settings is not None:
                self.run_command(self.backend.save_notifications(name, settings))

        def send_test(settings: NotifyConfig) -> None:
            self.run_command(self.backend.send_test(name, settings))

        current = self.config.notifications.get(name, NotifyConfig())
        notifier = self.backend.status.notifier

        def preview(message: str):
            return self.backend.preview_notification(name, message)

        screen = NotificationsScreen(name, current, notifier, form, preview, send_test)
        self.push_screen(screen, saved)

    def action_add_repo(self) -> None:
        def added(name: str | None) -> None:
            if name is not None:
                self.render_repo_tree(select=name)

        self.push_screen(AddRepoScreen(self.backend.add_repo), added)

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
            self.remove_repo(name, neighbors[0] if neighbors else None)

        self.push_screen(ConfirmScreen(f"Stop monitoring {name}?"), confirmed)

    @work(group="commands")
    async def remove_repo(self, name: str, select: str | None) -> None:
        try:
            await self.backend.remove_repo(name)
        except BackendError as e:
            self.notify(str(e), severity="error")
            return
        self.render_repo_tree(select=select)
        self.render_prs()
