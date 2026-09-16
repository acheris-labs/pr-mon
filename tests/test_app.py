import json
import tempfile
import unittest
from pathlib import Path
from unittest import mock

from textual.widgets import (
    Button,
    Checkbox,
    DataTable,
    Footer,
    Input,
    Static,
    TabbedContent,
    Tabs,
)

from pr_mon.app import PrMonApp, PrTable
from pr_mon.config import Config, NotifyConfig, load_config, save_config
from pr_mon.github import GitHubError, NotFoundError
from pr_mon.models import MergeMethod, parse_repo
from pr_mon.notify import SAMPLE_VARIABLES
from pr_mon.screens import (
    ActionMenuScreen,
    AddRepoScreen,
    ConfirmScreen,
    MergeMethodScreen,
    NotificationsScreen,
)
from pr_mon.state import AppState, save_state
from tests.fixtures import auto_merge, raw_pr, raw_repo

READY = {}
CONFLICT = {"mergeable": "CONFLICTING", "merge_state": "DIRTY"}
BEHIND = {"merge_state": "BEHIND"}


def make_repo(name, *prs, **repo_kwargs):
    return parse_repo(
        raw_repo(
            [raw_pr(number=n, title=f"PR {n}", **kw) for n, kw in prs], name=name, **repo_kwargs
        )
    )


class FakeClient:
    def __init__(self, repos):
        self.repos = dict(repos)  # name -> RepoInfo or Exception
        self.calls = []
        self.merge_error = None
        self.delete_error = None

    async def fetch_repo(self, name):
        self.calls.append(("fetch", name))
        for key, value in self.repos.items():
            if key.lower() == name.lower():
                if isinstance(value, Exception):
                    raise value
                return value
        raise NotFoundError(f"Repository {name} not found")

    async def merge(self, pr_id, method):
        self.calls.append(("merge", pr_id, method))
        if self.merge_error:
            raise self.merge_error

    async def update_branch(self, pr_id):
        self.calls.append(("update", pr_id))

    async def enable_auto_merge(self, pr_id, method):
        self.calls.append(("auto_on", pr_id, method))

    async def disable_auto_merge(self, pr_id):
        self.calls.append(("auto_off", pr_id))

    async def delete_branch(self, ref_id):
        self.calls.append(("delete", ref_id))
        if self.delete_error:
            raise self.delete_error

    async def aclose(self):
        self.calls.append(("close",))

    def actions(self):
        return [c for c in self.calls if c[0] not in ("fetch", "close")]


def text_of(widget):
    return str(widget.render())


class AppTestCase(unittest.IsolatedAsyncioTestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.config_path = Path(self.tmp.name) / "config.toml"
        self.state_path = Path(self.tmp.name) / "state.json"

    def make_app(self, repos, config_repos=None, notifications=None, notifier=None):
        names = list(repos) if config_repos is None else config_repos
        per_repo = {name: notifications for name in names} if notifications else {}
        config = Config(repos=names, notifications=per_repo)
        save_config(self.config_path, config)
        self.client = FakeClient(repos)
        return PrMonApp(self.client, self.config_path, self.state_path, notifier=notifier)

    async def settle(self, pilot):
        await pilot.app.workers.wait_for_complete()
        await pilot.pause()
        await pilot.app.workers.wait_for_complete()
        await pilot.pause()

    def tree_view(self, app):
        """Visible repo tree lines: '▼'/'▶' + owner label, repos indented."""
        lines = []
        for owner in app.query_one("#repos").root.children:
            lines.append(f"{'▼' if owner.is_expanded else '▶'} {owner.label}")
            if owner.is_expanded:
                lines += [f"    {repo.label}" for repo in owner.children]
        return lines

    def pr_numbers(self, app):
        table = app.query_one("#prs", DataTable)
        return [str(table.get_row_at(i)[1]) for i in range(table.row_count)]


class LayoutTest(AppTestCase):
    async def test_repos_and_badges(self):
        app = self.make_app(
            {
                "acme/api": make_repo("acme/api", (1, READY), (2, CONFLICT)),
                "acme/web": make_repo("acme/web", (3, CONFLICT)),
                "acme/empty": make_repo("acme/empty"),
            }
        )
        async with app.run_test(size=(120, 30)) as pilot:
            await self.settle(pilot)
            self.assertEqual(
                self.tree_view(app),
                ["▼ ● acme/ (3)", "    ● api (2)", "      empty", "    ● web (1)"],
            )
            self.assertEqual(self.pr_numbers(app), ["#1", "#2"])
            details = text_of(app.query_one("#details", Static))
            self.assertIn("#1 PR 1", details)
            self.assertIn("Branch: feature-1 → main", details)
            self.assertIn("Author: alice", details)
            self.assertIn("Opened:", details)
            self.assertIn("Last commit:", details)
            self.assertIn("Head SHA:    abc1234def5678abc1234def5678abc1234def56", details)
            self.assertNotIn("Last check", details)
            self.assertNotIn("enter", details)

    async def test_navigating_repos_switches_prs(self):
        app = self.make_app(
            {
                "acme/api": make_repo("acme/api", (1, READY)),
                "acme/web": make_repo("acme/web", (3, CONFLICT), (4, READY)),
            }
        )
        async with app.run_test(size=(120, 30)) as pilot:
            await self.settle(pilot)
            await pilot.press("down")
            await pilot.pause()
            self.assertEqual(self.pr_numbers(app), ["#3", "#4"])
            details = text_of(app.query_one("#details", Static))
            self.assertIn("Merge conflicts", details)

    async def test_focusing_prs_clears_badge(self):
        app = self.make_app({"acme/api": make_repo("acme/api", (1, READY), (2, READY))})
        async with app.run_test(size=(120, 30)) as pilot:
            await self.settle(pilot)
            await pilot.press("tab")
            await pilot.pause()
            self.assertEqual(self.tree_view(app), ["▼ ● acme/ (1)", "    ● api (1)"])
            await pilot.press("down")
            await pilot.pause()
            self.assertEqual(self.tree_view(app), ["▼   acme/", "      api"])
            state = json.loads(self.state_path.read_text())
            self.assertTrue(all(e["seen"] for e in state["prs"]["acme/api"].values()))

    async def test_seen_state_survives_restart(self):
        repos = {"acme/api": make_repo("acme/api", (1, READY))}
        app = self.make_app(repos)
        async with app.run_test(size=(120, 30)) as pilot:
            await self.settle(pilot)
            await pilot.press("tab")
            await pilot.pause()
        app = self.make_app(repos)
        async with app.run_test(size=(120, 30)) as pilot:
            await self.settle(pilot)
            self.assertEqual(self.tree_view(app), ["▼   acme/", "      api"])

    async def test_fetch_error_shows_warning_and_keeps_data(self):
        app = self.make_app({"acme/api": make_repo("acme/api", (1, READY))})
        async with app.run_test(size=(120, 30)) as pilot:
            await self.settle(pilot)
            self.client.repos["acme/api"] = GitHubError("Network error: down")
            await pilot.press("r")
            await self.settle(pilot)
            self.assertEqual(self.tree_view(app), ["▼ ⚠ acme/ (1)", "    ⚠ api (1)"])
            self.assertEqual(self.pr_numbers(app), ["#1"])
            self.assertIn("Network error: down", text_of(app.query_one("#details", Static)))

    async def test_actions_hint_only_in_pr_list(self):
        app = self.make_app({"acme/api": make_repo("acme/api", (1, READY))})
        async with app.run_test(size=(120, 30)) as pilot:
            await self.settle(pilot)
            footer = app.query_one(Footer)
            self.assertEqual(list(footer.query(".-actions").results()), [])
            await pilot.press("tab")
            await pilot.pause()
            actions = list(footer.query(".-actions").results())
            self.assertEqual([k.description for k in actions], ["Actions"])
            self.assertEqual(actions[0].styles.dock, "right")

    async def test_truncated_title(self):
        app = self.make_app({"acme/api": make_repo("acme/api", (1, READY), total=80)})
        async with app.run_test(size=(120, 30)) as pilot:
            await self.settle(pilot)
            self.assertIn("newest 1 of 80", str(app.query_one("#prs").border_title))

    async def test_closes_client(self):
        app = self.make_app({})
        async with app.run_test(size=(120, 30)) as pilot:
            await self.settle(pilot)
        self.assertIn(("close",), self.client.calls)


class ActionTest(AppTestCase):
    async def open_menu(self, pilot, row=0):
        await self.settle(pilot)
        await pilot.press("tab")
        for _ in range(row):
            await pilot.press("down")
        await pilot.press("enter")
        await pilot.pause()

    async def test_single_method_merges_directly(self):
        app = self.make_app(
            {"acme/api": make_repo("acme/api", (1, READY), merge=False, rebase=False)}
        )
        async with app.run_test(size=(120, 30)) as pilot:
            await self.open_menu(pilot)
            self.assertIsInstance(app.screen, ActionMenuScreen)
            await pilot.press("m")
            await self.settle(pilot)
            self.assertNotIsInstance(app.screen, ActionMenuScreen)
            self.assertEqual(
                self.client.actions(),
                [("merge", "PR_1", MergeMethod.SQUASH), ("delete", "REF_1")],
            )

    async def test_multiple_methods_prompt(self):
        app = self.make_app({"acme/api": make_repo("acme/api", (1, READY), merge=False)})
        async with app.run_test(size=(120, 30)) as pilot:
            await self.open_menu(pilot)
            await pilot.press("m")
            await pilot.pause()
            self.assertIsInstance(app.screen, MergeMethodScreen)
            self.assertEqual(len(app.screen.query("#method-m")), 0)
            await pilot.press("m")  # not allowed: ignored
            await pilot.pause()
            self.assertIsInstance(app.screen, MergeMethodScreen)
            await pilot.press("r")
            await self.settle(pilot)
            self.assertEqual(app.screen, app.screen_stack[0])
            self.assertEqual(self.client.actions()[0], ("merge", "PR_1", MergeMethod.REBASE))

    async def test_escape_from_method_returns_to_menu(self):
        app = self.make_app({"acme/api": make_repo("acme/api", (1, READY))})
        async with app.run_test(size=(120, 30)) as pilot:
            await self.open_menu(pilot)
            await pilot.press("m", "escape")
            await pilot.pause()
            self.assertIsInstance(app.screen, ActionMenuScreen)
            await pilot.press("escape")
            await pilot.pause()
            self.assertEqual(self.client.actions(), [])

    async def test_delete_checkbox_hidden_when_repo_auto_deletes(self):
        app = self.make_app(
            {
                "acme/api": make_repo(
                    "acme/api", (1, READY), merge=False, rebase=False, delete_on_merge=True
                )
            }
        )
        async with app.run_test(size=(120, 30)) as pilot:
            await self.open_menu(pilot)
            self.assertEqual(len(app.screen.query(Checkbox)), 0)
            await pilot.press("m")
            await self.settle(pilot)
            self.assertEqual(self.client.actions(), [("merge", "PR_1", MergeMethod.SQUASH)])

    async def test_unchecking_delete_skips_deletion(self):
        app = self.make_app(
            {"acme/api": make_repo("acme/api", (1, READY), merge=False, rebase=False)}
        )
        async with app.run_test(size=(120, 30)) as pilot:
            await self.open_menu(pilot)
            checkbox = app.screen.query_one(Checkbox)
            self.assertTrue(checkbox.value)
            await pilot.press("space")
            self.assertFalse(checkbox.value)
            await pilot.press("m")
            await self.settle(pilot)
            self.assertEqual(self.client.actions(), [("merge", "PR_1", MergeMethod.SQUASH)])

    async def test_delete_failure_is_warning(self):
        app = self.make_app(
            {"acme/api": make_repo("acme/api", (1, READY), merge=False, rebase=False)}
        )
        async with app.run_test(size=(120, 30), notifications=True) as pilot:
            self.client.delete_error = GitHubError("no push access")
            await self.open_menu(pilot)
            await pilot.press("m")
            await self.settle(pilot)
            severities = [n.severity for n in app._notifications]
            self.assertIn("warning", severities)
            self.assertNotIn("error", severities)

    async def test_merge_failure_is_error(self):
        app = self.make_app(
            {"acme/api": make_repo("acme/api", (1, READY), merge=False, rebase=False)}
        )
        async with app.run_test(size=(120, 30), notifications=True) as pilot:
            self.client.merge_error = GitHubError("Base branch was modified")
            await self.open_menu(pilot)
            await pilot.press("m")
            await self.settle(pilot)
            messages = [(n.severity, n.message) for n in app._notifications]
            self.assertTrue(any(s == "error" and "Base branch" in m for s, m in messages))
            self.assertNotIn("delete", [c[0] for c in self.client.actions()])

    async def test_merge_disabled_when_conflicting(self):
        app = self.make_app({"acme/api": make_repo("acme/api", (1, CONFLICT))})
        async with app.run_test(size=(120, 30)) as pilot:
            await self.open_menu(pilot)
            self.assertIn("unavailable", text_of(app.screen.query_one("#merge")))
            self.assertIn("Merge conflicts", text_of(app.screen.query_one("#merge")))
            self.assertEqual(len(app.screen.query(Checkbox)), 0)
            await pilot.press("m")
            await pilot.pause()
            self.assertIsInstance(app.screen, ActionMenuScreen)
            self.assertEqual(self.client.actions(), [])

    async def test_unavailable_reason_omits_info_notes(self):
        blocked = {
            "merge_state": "BLOCKED",
            "review_decision": "REVIEW_REQUIRED",
            "checks_total": 30,
        }
        app = self.make_app({"acme/api": make_repo("acme/api", (1, blocked))})
        async with app.run_test(size=(120, 30)) as pilot:
            await self.open_menu(pilot)
            merge_line = text_of(app.screen.query_one("#merge"))
            self.assertIn("Review required", merge_line)
            self.assertNotIn("more checks", merge_line)

    async def test_draft_unavailable_reason(self):
        app = self.make_app({"acme/api": make_repo("acme/api", (1, {"is_draft": True}))})
        async with app.run_test(size=(120, 30)) as pilot:
            await self.open_menu(pilot)
            merge_line = text_of(app.screen.query_one("#merge"))
            self.assertIn("unavailable (DRAFT)", merge_line)
            self.assertNotIn("merge methods", merge_line)

    async def test_update_only_when_behind(self):
        app = self.make_app({"acme/api": make_repo("acme/api", (1, READY), (2, BEHIND))})
        async with app.run_test(size=(120, 30)) as pilot:
            await self.open_menu(pilot)
            self.assertEqual(len(app.screen.query("#update")), 0)
            await pilot.press("u")
            await pilot.pause()
            self.assertEqual(self.client.actions(), [])
            await pilot.press("escape", "down", "enter")
            await pilot.pause()
            self.assertEqual(len(app.screen.query("#update")), 1)
            await pilot.press("u")
            await self.settle(pilot)
            self.assertEqual(self.client.actions(), [("update", "PR_2")])

    async def test_action_refreshes_repo(self):
        app = self.make_app(
            {"acme/api": make_repo("acme/api", (1, READY), merge=False, rebase=False)}
        )
        async with app.run_test(size=(120, 30)) as pilot:
            await self.open_menu(pilot)
            self.client.repos["acme/api"] = make_repo("acme/api")
            await pilot.press("m")
            await self.settle(pilot)
            self.assertEqual(self.pr_numbers(app), [])


class AutoMergeTest(AppTestCase):
    open_menu = ActionTest.open_menu

    async def test_enable_single_method(self):
        app = self.make_app(
            {"acme/api": make_repo("acme/api", (1, BEHIND), merge=False, rebase=False)}
        )
        async with app.run_test(size=(120, 30)) as pilot:
            await self.open_menu(pilot)
            self.assertIn("Enable auto-merge", text_of(app.screen.query_one("#auto")))
            await pilot.press("a")
            await self.settle(pilot)
            self.assertEqual(self.client.actions(), [("auto_on", "PR_1", MergeMethod.SQUASH)])

    async def test_enable_asks_for_method(self):
        app = self.make_app({"acme/api": make_repo("acme/api", (1, BEHIND), squash=False)})
        async with app.run_test(size=(120, 30)) as pilot:
            await self.open_menu(pilot)
            await pilot.press("a")
            await pilot.pause()
            self.assertIsInstance(app.screen, MergeMethodScreen)
            await pilot.press("m")
            await self.settle(pilot)
            self.assertEqual(self.client.actions(), [("auto_on", "PR_1", MergeMethod.MERGE)])

    async def test_disable(self):
        on = {"merge_state": "BLOCKED", "auto_merge": auto_merge("SQUASH", "bob")}
        app = self.make_app({"acme/api": make_repo("acme/api", (1, on))})
        async with app.run_test(size=(120, 30)) as pilot:
            await self.settle(pilot)
            self.assertIn("auto", str(app.query_one("#prs", DataTable).get_row_at(0)[2]))
            self.assertIn(
                "Auto-merge: on (squash, by bob)", text_of(app.query_one("#details", Static))
            )
            await self.open_menu(pilot)
            self.assertIn("Disable auto-merge", text_of(app.screen.query_one("#auto")))
            await pilot.press("a")
            await self.settle(pilot)
            self.assertEqual(self.client.actions(), [("auto_off", "PR_1")])

    async def test_unavailable_when_repo_disallows(self):
        app = self.make_app(
            {"acme/api": make_repo("acme/api", (1, BEHIND), auto_merge_allowed=False)}
        )
        async with app.run_test(size=(120, 30)) as pilot:
            await self.open_menu(pilot)
            self.assertIn("repo settings", text_of(app.screen.query_one("#auto")))
            await pilot.press("a")
            await pilot.pause()
            self.assertIsInstance(app.screen, ActionMenuScreen)
            self.assertEqual(self.client.actions(), [])

    async def test_unavailable_when_already_mergeable(self):
        app = self.make_app({"acme/api": make_repo("acme/api", (1, READY))})
        async with app.run_test(size=(120, 30)) as pilot:
            await self.open_menu(pilot)
            self.assertIn("already mergeable", text_of(app.screen.query_one("#auto")))
            await pilot.press("a")
            await pilot.pause()
            self.assertEqual(self.client.actions(), [])

    async def test_unavailable_for_draft(self):
        app = self.make_app({"acme/api": make_repo("acme/api", (1, {"is_draft": True}))})
        async with app.run_test(size=(120, 30)) as pilot:
            await self.open_menu(pilot)
            self.assertIn("draft", text_of(app.screen.query_one("#auto")))

    async def test_branch_deletion_note(self):
        app = self.make_app({"acme/api": make_repo("acme/api", (1, BEHIND))})
        async with app.run_test(size=(120, 30)) as pilot:
            await self.open_menu(pilot)
            self.assertIn("branch won't be deleted", text_of(app.screen.query_one("#auto")))
        app = self.make_app({"acme/api": make_repo("acme/api", (1, BEHIND), delete_on_merge=True)})
        async with app.run_test(size=(120, 30)) as pilot:
            await self.open_menu(pilot)
            self.assertNotIn("deleted", text_of(app.screen.query_one("#auto")))


class RepoManagementTest(AppTestCase):
    async def test_add_repo(self):
        app = self.make_app(
            {"acme/api": make_repo("acme/api"), "acme/new": make_repo("acme/new", (9, READY))},
            config_repos=["acme/api"],
        )
        async with app.run_test(size=(120, 30)) as pilot:
            await self.settle(pilot)
            await pilot.press("A")
            await pilot.pause()
            self.assertIsInstance(app.screen, AddRepoScreen)
            app.screen.query_one(Input).value = "ACME/NEW"
            await pilot.press("enter")
            await self.settle(pilot)
            self.assertNotIsInstance(app.screen, AddRepoScreen)
            self.assertEqual(self.tree_view(app), ["▼ ● acme/ (1)", "      api", "    ● new (1)"])
            self.assertEqual(load_config(self.config_path)[0].repos, ["acme/api", "acme/new"])
            self.assertEqual(app.selected_repo, "acme/new")
            self.assertEqual(self.pr_numbers(app), ["#9"])

    async def test_add_rejects_bad_format_and_missing_repo(self):
        app = self.make_app({"acme/api": make_repo("acme/api")})
        async with app.run_test(size=(120, 30)) as pilot:
            await self.settle(pilot)
            await pilot.press("A")
            await pilot.pause()
            screen = app.screen
            screen.query_one(Input).value = "nope"
            await pilot.press("enter")
            await pilot.pause()
            self.assertIn("owner/name", text_of(screen.query_one("#error")))
            screen.query_one(Input).value = "acme/missing"
            await pilot.press("enter")
            await self.settle(pilot)
            self.assertIs(app.screen, screen)
            self.assertIn("not found", text_of(screen.query_one("#error")))
            await pilot.press("escape")
            await pilot.pause()
            self.assertEqual(load_config(self.config_path)[0].repos, ["acme/api"])

    async def test_add_rejects_duplicate(self):
        app = self.make_app({"acme/api": make_repo("acme/api")})
        async with app.run_test(size=(120, 30)) as pilot:
            await self.settle(pilot)
            await pilot.press("A")
            await pilot.pause()
            app.screen.query_one(Input).value = "ACME/api"
            await pilot.press("enter")
            await self.settle(pilot)
            self.assertIn("already", text_of(app.screen.query_one("#error")))

    async def test_remove_repo(self):
        app = self.make_app(
            {"acme/api": make_repo("acme/api", (1, READY)), "acme/web": make_repo("acme/web")}
        )
        async with app.run_test(size=(120, 30)) as pilot:
            await self.settle(pilot)
            await pilot.press("D")
            await pilot.pause()
            self.assertIsInstance(app.screen, ConfirmScreen)
            await pilot.press("y")
            await pilot.pause()
            self.assertEqual(self.tree_view(app), ["▼   acme/", "      web"])
            self.assertEqual(load_config(self.config_path)[0].repos, ["acme/web"])
            self.assertNotIn("acme/api", json.loads(self.state_path.read_text())["prs"])
            self.assertEqual(app.selected_repo, "acme/web")
            self.assertEqual(self.pr_numbers(app), [])

    async def test_remove_cancelled(self):
        app = self.make_app({"acme/api": make_repo("acme/api")})
        async with app.run_test(size=(120, 30)) as pilot:
            await self.settle(pilot)
            await pilot.press("D", "n")
            await pilot.pause()
            self.assertEqual(load_config(self.config_path)[0].repos, ["acme/api"])

    async def test_remove_only_from_repo_pane(self):
        app = self.make_app({"acme/api": make_repo("acme/api", (1, READY))})
        async with app.run_test(size=(120, 30)) as pilot:
            await self.settle(pilot)
            await pilot.press("tab", "D")
            await pilot.pause()
            self.assertNotIsInstance(app.screen, ConfirmScreen)


class RepoTreeTest(AppTestCase):
    def write_state(self, **kwargs):
        save_state(self.state_path, AppState(**kwargs))

    async def test_groups_by_owner_sorted(self):
        app = self.make_app(
            {n: make_repo(n) for n in ("zeta/b", "Alpha/x", "zeta/a")},
        )
        async with app.run_test(size=(120, 30)) as pilot:
            await self.settle(pilot)
            self.assertEqual(
                self.tree_view(app),
                ["▼   Alpha/", "      x", "▼   zeta/", "      a", "      b"],
            )
            self.assertEqual(app.selected_repo, "Alpha/x")

    async def test_rollup_across_repos_and_owners(self):
        app = self.make_app(
            {
                "acme/api": make_repo("acme/api", (1, CONFLICT)),
                "acme/web": make_repo("acme/web", (2, READY), (3, BEHIND)),
                "beta/x": make_repo("beta/x", (4, CONFLICT)),
            }
        )
        async with app.run_test(size=(120, 30)) as pilot:
            await self.settle(pilot)
            self.assertEqual(
                self.tree_view(app),
                [
                    "▼ ● acme/ (3)",
                    "    ● api (1)",
                    "    ● web (2)",
                    "▼ ● beta/ (1)",
                    "    ● x (1)",
                ],
            )
            owners = app.query_one("#repos").root.children
            self.assertEqual(owners[0].label.spans[0].style, "bold green")
            self.assertEqual(owners[1].label.spans[0].style, "bold red")

    async def test_enter_on_owner_toggles_and_persists(self):
        app = self.make_app({"acme/api": make_repo("acme/api", (1, READY))})
        async with app.run_test(size=(120, 30)) as pilot:
            await self.settle(pilot)
            await pilot.press("up")
            await pilot.pause()
            self.assertIsNone(app.selected_repo)
            self.assertEqual(self.pr_numbers(app), [])
            self.assertIn("Select a repository", text_of(app.query_one("#details", Static)))
            await pilot.press("enter")
            await pilot.pause()
            self.assertEqual(self.tree_view(app), ["▶ ● acme/ (1)"])
            self.assertEqual(json.loads(self.state_path.read_text())["collapsed"], ["acme"])
            self.assertFalse(app.query_one(PrTable).has_focus)
        app = self.make_app({"acme/api": make_repo("acme/api", (1, READY))})
        async with app.run_test(size=(120, 30)) as pilot:
            await self.settle(pilot)
            self.assertEqual(self.tree_view(app), ["▶ ● acme/ (1)"])
            await pilot.press("enter")
            await pilot.pause()
            self.assertEqual(self.tree_view(app), ["▼ ● acme/ (1)", "    ● api (1)"])
            self.assertEqual(json.loads(self.state_path.read_text())["collapsed"], [])

    async def test_enter_on_repo_focuses_prs(self):
        app = self.make_app({"acme/api": make_repo("acme/api", (1, READY))})
        async with app.run_test(size=(120, 30)) as pilot:
            await self.settle(pilot)
            await pilot.press("enter")
            await pilot.pause()
            self.assertTrue(app.query_one(PrTable).has_focus)
            self.assertEqual(self.tree_view(app), ["▼   acme/", "      api"])

    async def test_left_and_right(self):
        app = self.make_app({"acme/api": make_repo("acme/api")})
        async with app.run_test(size=(120, 30)) as pilot:
            await self.settle(pilot)
            await pilot.press("left")
            await pilot.pause()
            self.assertIsNone(app.selected_repo)
            self.assertEqual(self.tree_view(app), ["▼   acme/", "      api"])
            await pilot.press("left")
            await pilot.pause()
            self.assertEqual(self.tree_view(app), ["▶   acme/"])
            await pilot.press("right")
            await pilot.pause()
            self.assertEqual(self.tree_view(app), ["▼   acme/", "      api"])

    async def test_collapsed_owner_with_old_unseen_stays_collapsed(self):
        self.write_state(
            prs={"beta/x": {"5": {"status": "CONFLICT", "seen": False}}}, collapsed=["beta"]
        )
        app = self.make_app(
            {"acme/api": make_repo("acme/api"), "beta/x": make_repo("beta/x", (5, CONFLICT))}
        )
        async with app.run_test(size=(120, 30)) as pilot:
            await self.settle(pilot)
            self.assertEqual(self.tree_view(app), ["▼   acme/", "      api", "▶ ● beta/ (1)"])

    async def test_new_alert_expands_owner_and_keeps_cursor(self):
        self.write_state(
            prs={"acme/api": {"1": {"status": "PENDING", "seen": True}}}, collapsed=["acme"]
        )
        pending = {"merge_state": "BLOCKED", "check_state": "PENDING"}
        app = self.make_app(
            {
                "acme/api": make_repo("acme/api", (1, pending)),
                "beta/x": make_repo("beta/x", (7, READY), (8, READY)),
            }
        )
        async with app.run_test(size=(120, 30)) as pilot:
            await self.settle(pilot)
            self.assertEqual(self.tree_view(app), ["▶   acme/", "▼ ● beta/ (2)", "    ● x (2)"])
            self.assertEqual(app.selected_repo, "beta/x")
            await pilot.press("tab", "down")
            await pilot.pause()
            self.client.repos["acme/api"] = make_repo("acme/api", (1, READY))
            await pilot.press("r")
            await self.settle(pilot)
            self.assertEqual(
                self.tree_view(app),
                ["▼ ● acme/ (1)", "    ● api (1)", "▼   beta/", "      x"],
            )
            self.assertEqual(app.selected_repo, "beta/x")
            self.assertEqual(app.query_one(PrTable).cursor_row, 1)
            self.assertEqual(app.query_one("#repos").cursor_node.data, "beta/x")
            self.assertEqual(json.loads(self.state_path.read_text())["collapsed"], [])

    async def test_remove_ignores_owner_rows(self):
        app = self.make_app({"acme/api": make_repo("acme/api")})
        async with app.run_test(size=(120, 30)) as pilot:
            await self.settle(pilot)
            await pilot.press("up", "D")
            await pilot.pause()
            self.assertNotIsInstance(app.screen, ConfirmScreen)

    async def test_removing_last_repo_of_owner_drops_group(self):
        app = self.make_app({"acme/api": make_repo("acme/api"), "beta/x": make_repo("beta/x")})
        async with app.run_test(size=(120, 30)) as pilot:
            await self.settle(pilot)
            await pilot.press("D", "y")
            await self.settle(pilot)
            self.assertEqual(self.tree_view(app), ["▼   beta/", "      x"])
            self.assertEqual(app.selected_repo, "beta/x")


class FakeDeliver:
    """Stands in for notify.deliver; records calls and returns canned results."""

    def __init__(self):
        self.calls = []
        self.results = None

    async def __call__(self, settings, notifier, variables):
        self.calls.append((settings, notifier, variables))
        if self.results is not None:
            return self.results
        channels = []
        if settings.script_enabled and settings.script:
            channels.append(("script", None))
        if settings.desktop_enabled and notifier:
            channels.append(("desktop", None))
        return channels

    def states(self):
        return [(v["PR_REPO"], v["PR_NUM"], v["PR_STATE"]) for _, _, v in self.calls]


PENDING = {"merge_state": "BLOCKED", "check_state": "PENDING"}
FAILING = {"merge_state": "BLOCKED", "check_state": "FAILURE"}
ON = NotifyConfig(script_enabled=True, script="im")


class NotificationDispatchTest(AppTestCase):
    def setUp(self):
        super().setUp()
        self.deliver = FakeDeliver()
        patcher = mock.patch("pr_mon.app.deliver", self.deliver)
        patcher.start()
        self.addCleanup(patcher.stop)

    async def refresh(self, pilot):
        await pilot.press("r")
        await self.settle(pilot)

    async def test_first_load_is_silent_then_changes_notify(self):
        save_state(
            self.state_path,
            AppState(prs={"acme/api": {"1": {"status": "PENDING", "seen": True}}}),
        )
        app = self.make_app(
            {"acme/api": make_repo("acme/api", (1, READY), (2, READY))}, notifications=ON
        )
        async with app.run_test(size=(120, 30)) as pilot:
            await self.settle(pilot)
            self.assertEqual(self.deliver.calls, [])
            self.client.repos["acme/api"] = make_repo("acme/api", (1, FAILING), (2, READY))
            await self.refresh(pilot)
            self.assertEqual(self.deliver.states(), [("acme/api", "1", "FAILING")])
            settings, _, variables = self.deliver.calls[0]
            self.assertEqual(settings, ON)
            self.assertEqual(variables["PR_TITLE"], "PR 1")

    async def test_unselected_changes_are_quiet(self):
        app = self.make_app({"acme/api": make_repo("acme/api", (1, READY))}, notifications=ON)
        async with app.run_test(size=(120, 30)) as pilot:
            await self.settle(pilot)
            self.client.repos["acme/api"] = make_repo("acme/api", (1, BEHIND), (2, READY))
            await self.refresh(pilot)
            self.assertEqual(self.deliver.calls, [])

    async def test_new_pr_after_first_load(self):
        settings = NotifyConfig(events=["NEW"], script_enabled=True, script="im")
        app = self.make_app({"acme/api": make_repo("acme/api")}, notifications=settings)
        async with app.run_test(size=(120, 30)) as pilot:
            await self.settle(pilot)
            self.client.repos["acme/api"] = make_repo("acme/api", (5, PENDING))
            await self.refresh(pilot)
            self.assertEqual(self.deliver.states(), [("acme/api", "5", "NEW")])

    async def test_added_repo_notifies_once_configured(self):
        app = self.make_app(
            {"acme/api": make_repo("acme/api"), "acme/new": make_repo("acme/new", (9, PENDING))},
            config_repos=["acme/api"],
            notifications=ON,
        )
        async with app.run_test(size=(120, 30)) as pilot:
            await self.settle(pilot)
            await pilot.press("A")
            await pilot.pause()
            app.screen.query_one(Input).value = "acme/new"
            await pilot.press("enter")
            await self.settle(pilot)
            self.assertNotIn("acme/new", app.config.notifications)
            await pilot.press("N")
            await pilot.pause()
            app.screen.query_one("#script-enabled", Checkbox).value = True
            app.screen.query_one("#script", Input).value = "im"
            await pilot.press("ctrl+s")
            await pilot.pause()
            self.assertEqual(self.deliver.calls, [])
            self.client.repos["acme/new"] = make_repo("acme/new", (9, READY))
            await self.refresh(pilot)
            self.assertEqual(self.deliver.states(), [("acme/new", "9", "READY")])

    async def test_failed_first_fetch_stays_first_load(self):
        save_state(
            self.state_path,
            AppState(prs={"acme/api": {"1": {"status": "PENDING", "seen": True}}}),
        )
        app = self.make_app({"acme/api": GitHubError("down")}, notifications=ON)
        async with app.run_test(size=(120, 30)) as pilot:
            await self.settle(pilot)
            self.client.repos["acme/api"] = make_repo("acme/api", (1, READY))
            await self.refresh(pilot)
            self.assertEqual(self.deliver.calls, [])
            self.client.repos["acme/api"] = make_repo("acme/api", (1, FAILING))
            await self.refresh(pilot)
            self.assertEqual(self.deliver.states(), [("acme/api", "1", "FAILING")])

    async def test_removing_repo_drops_its_settings(self):
        repos = {"acme/api": make_repo("acme/api", (1, PENDING)), "acme/web": make_repo("acme/web")}
        app = self.make_app(repos, notifications=ON)
        async with app.run_test(size=(120, 30)) as pilot:
            await self.settle(pilot)
            await pilot.press("D", "y")
            await pilot.pause()
            self.assertEqual(list(app.config.notifications), ["acme/web"])
            self.assertEqual(list(load_config(self.config_path)[0].notifications), ["acme/web"])
            self.client.repos["acme/api"] = make_repo("acme/api", (1, READY))
            await pilot.press("A")
            await pilot.pause()
            app.screen.query_one(Input).value = "acme/api"
            await pilot.press("enter")
            await self.settle(pilot)
            self.assertEqual(self.deliver.calls, [])
            self.assertNotIn("acme/api", app.config.notifications)

    async def test_only_configured_repos_notify(self):
        save_config(
            self.config_path,
            Config(repos=["acme/api", "acme/web"], notifications={"acme/web": ON}),
        )
        self.client = FakeClient(
            {
                "acme/api": make_repo("acme/api", (1, PENDING)),
                "acme/web": make_repo("acme/web", (2, PENDING)),
            }
        )
        app = PrMonApp(self.client, self.config_path, self.state_path)
        async with app.run_test(size=(120, 30)) as pilot:
            await self.settle(pilot)
            self.client.repos["acme/api"] = make_repo("acme/api", (1, READY))
            self.client.repos["acme/web"] = make_repo("acme/web", (2, READY))
            await self.refresh(pilot)
            self.assertEqual(self.deliver.states(), [("acme/web", "2", "READY")])

    async def test_each_repo_uses_its_own_settings(self):
        api = NotifyConfig(script_enabled=True, script="api-hook", events=["READY"])
        web = NotifyConfig(desktop_enabled=True, events=["FAILING"])
        save_config(
            self.config_path,
            Config(
                repos=["acme/api", "acme/web"], notifications={"acme/api": api, "acme/web": web}
            ),
        )
        self.client = FakeClient(
            {
                "acme/api": make_repo("acme/api", (1, PENDING)),
                "acme/web": make_repo("acme/web", (2, PENDING)),
            }
        )
        app = PrMonApp(self.client, self.config_path, self.state_path, notifier="/x/tn")
        async with app.run_test(size=(120, 30)) as pilot:
            await self.settle(pilot)
            self.client.repos["acme/api"] = make_repo("acme/api", (1, FAILING))
            self.client.repos["acme/web"] = make_repo("acme/web", (2, FAILING))
            await self.refresh(pilot)
            self.assertEqual(self.deliver.states(), [("acme/web", "2", "FAILING")])
            self.assertEqual(self.deliver.calls[0][0], web)

    async def test_delivery_errors_are_toasted(self):
        app = self.make_app({"acme/api": make_repo("acme/api", (1, PENDING))}, notifications=ON)
        async with app.run_test(size=(120, 30), notifications=True) as pilot:
            await self.settle(pilot)
            self.deliver.results = [("script", "im exited with code 1: nope")]
            self.client.repos["acme/api"] = make_repo("acme/api", (1, READY))
            await self.refresh(pilot)
            messages = [(n.severity, n.message) for n in app._notifications]
            self.assertIn(
                ("warning", "Script notification failed: im exited with code 1: nope"), messages
            )


API = {"acme/api": make_repo("acme/api")}


class NotificationsScreenTest(AppTestCase):
    def setUp(self):
        super().setUp()
        self.deliver = FakeDeliver()
        patcher = mock.patch("pr_mon.app.deliver", self.deliver)
        patcher.start()
        self.addCleanup(patcher.stop)

    async def open(self, pilot):
        await self.settle(pilot)
        await pilot.press("N")
        await pilot.pause()
        self.assertIsInstance(pilot.app.screen, NotificationsScreen)
        return pilot.app.screen

    async def test_requires_a_repo(self):
        app = self.make_app(API)
        async with app.run_test(size=(120, 40), notifications=True) as pilot:
            await self.settle(pilot)
            await pilot.press("left")
            await pilot.pause()
            self.assertIsNone(app.selected_repo)
            await pilot.press("N")
            await pilot.pause()
            self.assertNotIsInstance(app.screen, NotificationsScreen)
            self.assertTrue(any("Select a repository" in n.message for n in app._notifications))

    async def test_title_names_repo_and_shows_its_settings(self):
        app = self.make_app(API, notifications=NotifyConfig(events=["NEW"], script="im"))
        async with app.run_test(size=(120, 40)) as pilot:
            screen = await self.open(pilot)
            self.assertIn("acme/api", text_of(screen.query_one("#title")))
            self.assertEqual(self.event_values(screen)["new"], True)
            self.assertEqual(self.event_values(screen)["ready"], False)
            self.assertEqual(screen.query_one("#script", Input).value, "im")

    def event_values(self, screen):
        return {
            box.id.removeprefix("event-"): box.value
            for box in screen.query(Checkbox)
            if box.id.startswith("event-")
        }

    async def test_shows_current_settings(self):
        app = self.make_app(API)
        async with app.run_test(size=(120, 40)) as pilot:
            screen = await self.open(pilot)
            self.assertEqual(
                self.event_values(screen),
                {
                    "ready": True,
                    "failing": True,
                    "conflict": True,
                    "blocked": False,
                    "behind": False,
                    "pending": False,
                    "new": False,
                },
            )
            self.assertEqual(screen.query_one("#message", Input).value, NotifyConfig().message)
            self.assertFalse(screen.query_one("#include-drafts", Checkbox).value)
            self.assertFalse(screen.query_one("#script-enabled", Checkbox).value)
            self.assertIn("stdin", text_of(screen.query_one("#script-help")))

    async def test_preview_and_unknown_placeholders(self):
        app = self.make_app(API)
        async with app.run_test(size=(120, 40)) as pilot:
            screen = await self.open(pilot)
            self.assertIn("acme/api#123 is READY", text_of(screen.query_one("#preview")))
            screen.query_one("#message", Input).value = "PR {{PR_NUM}} {{PR_OOPS}}"
            await pilot.pause()
            self.assertIn("PR 123 {{PR_OOPS}}", text_of(screen.query_one("#preview")))
            self.assertIn("PR_OOPS", text_of(screen.query_one("#unknown")))

    async def test_save_persists_and_applies(self):
        app = self.make_app({"acme/api": make_repo("acme/api")})
        async with app.run_test(size=(120, 40)) as pilot:
            screen = await self.open(pilot)
            screen.query_one("#message", Input).value = "hi {{PR_NUM}}"
            screen.query_one("#event-ready", Checkbox).value = False
            screen.query_one("#event-new", Checkbox).value = True
            screen.query_one("#include-drafts", Checkbox).value = True
            screen.query_one("#script-enabled", Checkbox).value = True
            screen.query_one("#script", Input).value = "im --deliver tgram"
            await pilot.press("ctrl+s")
            await pilot.pause()
            self.assertNotIsInstance(app.screen, NotificationsScreen)
            expected = NotifyConfig(
                message="hi {{PR_NUM}}",
                events=["FAILING", "CONFLICT", "NEW"],
                include_drafts=True,
                script_enabled=True,
                script="im --deliver tgram",
            )
            self.assertEqual(app.config.notifications, {"acme/api": expected})
            saved = load_config(self.config_path)[0]
            self.assertEqual(saved.notifications, {"acme/api": expected})
            self.assertEqual(saved.repos, ["acme/api"])

    async def test_save_button(self):
        app = self.make_app(API)
        async with app.run_test(size=(120, 40)) as pilot:
            screen = await self.open(pilot)
            screen.query_one("#event-behind", Checkbox).value = True
            await pilot.click("#save")
            await pilot.pause()
            saved = load_config(self.config_path)[0].notifications["acme/api"]
            self.assertIn("BEHIND", saved.events)

    async def test_cancel_discards(self):
        app = self.make_app(API)
        async with app.run_test(size=(120, 40)) as pilot:
            for leave in ("escape", "#cancel"):
                screen = await self.open(pilot)
                screen.query_one("#script-enabled", Checkbox).value = True
                if leave.startswith("#"):
                    await pilot.click(leave)
                else:
                    await pilot.press(leave)
                await pilot.pause()
                self.assertNotIsInstance(app.screen, NotificationsScreen)
                self.assertEqual(app.config.notifications, {})
                self.assertEqual(load_config(self.config_path)[0].notifications, {})

    async def test_send_test_uses_unsaved_settings(self):
        app = self.make_app(API, notifier="/opt/bin/terminal-notifier")
        async with app.run_test(size=(120, 40), notifications=True) as pilot:
            screen = await self.open(pilot)
            screen.query_one("#script-enabled", Checkbox).value = True
            screen.query_one("#script", Input).value = "im"
            screen.query_one("#desktop-enabled", Checkbox).value = True
            await pilot.click("#test")
            await self.settle(pilot)
            self.assertIsInstance(app.screen, NotificationsScreen)
            settings, notifier, variables = self.deliver.calls[0]
            self.assertEqual((settings.script_enabled, settings.script), (True, "im"))
            self.assertTrue(settings.desktop_enabled)
            self.assertEqual(notifier, "/opt/bin/terminal-notifier")
            self.assertEqual(
                variables,
                {
                    **SAMPLE_VARIABLES,
                    "PR_REPO": "acme/api",
                    "PR_URL": "https://github.com/acme/api/pull/123",
                },
            )
            messages = [n.message for n in app._notifications]
            self.assertIn("Test script notification sent", messages)
            self.assertIn("Test desktop notification sent", messages)
            self.assertEqual(app.config.notifications, {})

    async def test_send_test_with_nothing_enabled(self):
        app = self.make_app(API)
        async with app.run_test(size=(120, 40), notifications=True) as pilot:
            await self.open(pilot)
            await pilot.click("#test")
            await self.settle(pilot)
            self.assertTrue(any("Nothing to send" in n.message for n in app._notifications))

    async def test_desktop_notifier_detected(self):
        app = self.make_app(API, notifier="/opt/homebrew/bin/terminal-notifier")
        async with app.run_test(size=(120, 40)) as pilot:
            screen = await self.open(pilot)
            self.assertFalse(screen.query_one("#desktop-enabled", Checkbox).disabled)
            self.assertIn("terminal-notifier", text_of(screen.query_one("#notifier")))

    async def test_no_desktop_notifier(self):
        settings = NotifyConfig(desktop_enabled=True)
        app = self.make_app(API, notifications=settings, notifier=None)
        async with app.run_test(size=(120, 40)) as pilot:
            screen = await self.open(pilot)
            box = screen.query_one("#desktop-enabled", Checkbox)
            self.assertTrue(box.disabled)
            self.assertIn("No notifier found", text_of(screen.query_one("#notifier")))
            await pilot.press("ctrl+s")
            await pilot.pause()
            # A setting made on a machine with a notifier is preserved.
            self.assertTrue(
                load_config(self.config_path)[0].notifications["acme/api"].desktop_enabled
            )

    async def test_buttons_present(self):
        app = self.make_app(API)
        async with app.run_test(size=(120, 40)) as pilot:
            screen = await self.open(pilot)
            labels = [str(b.label) for b in screen.query(Button)]
            self.assertEqual(labels, ["Send test", "Save", "Cancel"])

    def active_tab(self, screen):
        return screen.query_one(TabbedContent).active

    async def test_left_right_switch_tabs_up_down_move_between_events(self):
        app = self.make_app(API, notifier="/x/terminal-notifier")
        async with app.run_test(size=(120, 40)) as pilot:
            screen = await self.open(pilot)
            self.assertEqual(self.active_tab(screen), "tab-message")
            self.assertIsInstance(app.focused, Tabs)
            await pilot.press("right")
            await pilot.pause()
            self.assertEqual(self.active_tab(screen), "tab-events")
            self.assertIsInstance(app.focused, Tabs)
            await pilot.press("down")
            self.assertEqual(app.focused.id, "event-ready")
            await pilot.press("down", "down")
            self.assertEqual(app.focused.id, "event-conflict")
            await pilot.press("space")
            self.assertFalse(screen.query_one("#event-conflict", Checkbox).value)
            await pilot.press("up")
            self.assertEqual(app.focused.id, "event-failing")
            await pilot.press("right")
            await pilot.pause()
            self.assertEqual(self.active_tab(screen), "tab-script")
            self.assertEqual(app.focused.id, "script-enabled")
            await pilot.press("right")
            await pilot.pause()
            self.assertEqual(self.active_tab(screen), "tab-desktop")
            await pilot.press("right")
            await pilot.pause()
            self.assertEqual(self.active_tab(screen), "tab-message")
            self.assertEqual(app.focused.id, "message")
            await pilot.press("left")
            await pilot.pause()
            self.assertEqual(self.active_tab(screen), "tab-message")
            await pilot.press("up")
            self.assertIsInstance(app.focused, Tabs)
            await pilot.press("left")
            await pilot.pause()
            self.assertEqual(self.active_tab(screen), "tab-desktop")
            self.assertIsInstance(app.focused, Tabs)
            await pilot.press("down")
            self.assertEqual(app.focused.id, "desktop-enabled")

    async def test_left_right_edit_text_fields(self):
        app = self.make_app(API)
        async with app.run_test(size=(120, 40)) as pilot:
            screen = await self.open(pilot)
            await pilot.press("right", "right")
            await pilot.pause()
            await pilot.press("down", "down")
            self.assertEqual(app.focused.id, "script")
            await pilot.press("i", "m", "left", "x")
            self.assertEqual(screen.query_one("#script", Input).value, "ixm")
            self.assertEqual(self.active_tab(screen), "tab-script")

    async def test_tab_without_usable_controls_keeps_focus_on_tab_bar(self):
        app = self.make_app(API, notifier=None)
        async with app.run_test(size=(120, 40)) as pilot:
            screen = await self.open(pilot)
            await pilot.press("right", "right")
            await pilot.pause()
            await pilot.press("down")
            self.assertEqual(app.focused.id, "script-enabled")
            await pilot.press("right")
            await pilot.pause()
            self.assertEqual(self.active_tab(screen), "tab-desktop")
            self.assertIsInstance(app.focused, Tabs)
            await pilot.press("right")
            await pilot.pause()
            self.assertEqual(self.active_tab(screen), "tab-message")

    async def test_down_reaches_buttons(self):
        app = self.make_app(API)
        async with app.run_test(size=(120, 40)) as pilot:
            await self.open(pilot)
            await pilot.press("right")
            await pilot.pause()
            await pilot.press(*["down"] * 9)
            self.assertEqual(app.focused.id, "test")


if __name__ == "__main__":
    unittest.main()
