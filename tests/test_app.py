import json
import tempfile
import unittest
from pathlib import Path

from textual.widgets import Checkbox, DataTable, Input, Static

from pr_mon.app import PrMonApp, PrTable
from pr_mon.config import Config, load_config, save_config
from pr_mon.github import GitHubError, NotFoundError
from pr_mon.models import MergeMethod, parse_repo
from pr_mon.screens import ActionMenuScreen, AddRepoScreen, ConfirmScreen, MergeMethodScreen
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

    def make_app(self, repos, config_repos=None):
        names = list(repos) if config_repos is None else config_repos
        save_config(self.config_path, Config(repos=names))
        self.client = FakeClient(repos)
        return PrMonApp(self.client, self.config_path, self.state_path)

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


if __name__ == "__main__":
    unittest.main()
