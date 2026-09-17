import unittest
from pathlib import Path

import pr_mon
from pr_mon.actions import available_actions, with_actions
from pr_mon.models import ArmedMerge, MergeMethod
from tests.fakes import make_repo
from tests.fixtures import auto_merge

READY = {}
PENDING = {"merge_state": "BLOCKED", "check_state": "PENDING"}
BEHIND = {"merge_state": "BEHIND"}
ARMED = ArmedMerge(MergeMethod.SQUASH, True, "2026-09-17T10:00:00+00:00")


def menu(state, armed=None, **repo_kwargs):
    repo = make_repo("acme/api", (1, state), **repo_kwargs)
    return {o.key: o for o in available_actions(repo, repo.prs[0], armed)}


class ActionsTest(unittest.TestCase):
    def test_ready_pr(self):
        options = menu(READY)
        self.assertEqual(list(options), ["merge", "auto_merge"])
        merge = options["merge"]
        self.assertTrue(merge.available)
        self.assertTrue(merge.needs_method)
        self.assertTrue(merge.offers_delete_branch)
        self.assertEqual(options["auto_merge"].reason, "already mergeable — use m")

    def test_merge_unavailable_explains(self):
        merge = menu(PENDING)["merge"]
        self.assertFalse(merge.available)
        self.assertEqual(merge.reason, "PENDING")
        merge = menu({"review_decision": "CHANGES_REQUESTED", "merge_state": "BLOCKED"})["merge"]
        self.assertEqual(merge.reason, "BLOCKED: Changes requested")

    def test_no_merge_methods(self):
        options = menu(READY, merge=False, squash=False, rebase=False)
        self.assertEqual(options["merge"].reason, "READY: no merge methods allowed")
        self.assertEqual(options["auto_merge"].reason, "no merge methods allowed")

    def test_repo_deletes_branches(self):
        options = menu(READY, delete_on_merge=True)
        self.assertFalse(options["merge"].offers_delete_branch)

    def test_update_only_when_behind(self):
        self.assertNotIn("update", menu(READY))
        update = menu(BEHIND)["update"]
        self.assertEqual(
            (update.kind, update.available, update.needs_method), ("update", True, False)
        )

    def test_native_auto_merge(self):
        auto = menu(PENDING)["auto_merge"]
        self.assertEqual(
            (auto.kind, auto.available, auto.needs_method), ("auto_merge_on", True, True)
        )
        self.assertEqual(auto.note, "branch won't be deleted: repo doesn't auto-delete")
        self.assertFalse(auto.offers_delete_branch)
        self.assertIsNone(menu(PENDING, delete_on_merge=True)["auto_merge"].note)
        enabled = menu({**PENDING, "auto_merge": auto_merge()})["auto_merge"]
        self.assertEqual((enabled.kind, enabled.needs_method), ("auto_merge_off", False))

    def test_pr_mon_merge_when_ready(self):
        auto = menu(PENDING, auto_merge_allowed=False)["auto_merge"]
        self.assertEqual(auto.kind, "arm_merge")
        self.assertEqual(auto.label, "Merge when ready (pr-mon)")
        self.assertTrue(auto.offers_delete_branch)
        armed = menu(PENDING, ARMED, auto_merge_allowed=False)["auto_merge"]
        self.assertEqual((armed.kind, armed.available), ("disarm_merge", True))
        draft = menu({"is_draft": True}, auto_merge_allowed=False)["auto_merge"]
        self.assertEqual((draft.kind, draft.available), ("arm_merge", False))
        self.assertEqual((draft.label, draft.reason), ("Merge when ready", "draft PR"))

    def test_with_actions_uses_each_prs_armed_state(self):
        repo = make_repo("acme/api", (1, PENDING), (2, PENDING), auto_merge_allowed=False)
        filled = with_actions(repo, {2: ARMED})
        kinds = [next(o.kind for o in pr.actions if o.key == "auto_merge") for pr in filled.prs]
        self.assertEqual(kinds, ["arm_merge", "disarm_merge"])


class ThinClientTest(unittest.TestCase):
    """The TUI only displays what the backend decided."""

    BACKEND_ONLY = ("actions", "readiness", "github", "service", "server", "tracker", "notify")
    CLIENT_MODULES = ("app", "views", "screens", "remote")

    def test_client_modules_do_not_import_backend_rules(self):
        package = Path(pr_mon.__file__).parent
        for module in self.CLIENT_MODULES:
            source = (package / f"{module}.py").read_text()
            for backend in self.BACKEND_ONLY:
                with self.subTest(module=module, backend=backend):
                    self.assertNotIn(f"pr_mon.{backend} import", source)
                    self.assertNotIn(f"import pr_mon.{backend}", source)


if __name__ == "__main__":
    unittest.main()
