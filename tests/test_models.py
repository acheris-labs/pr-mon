import unittest

from pr_mon.models import MergeMethod, Status, parse_repo
from tests.fixtures import check_run, raw_pr, raw_repo, status_context


def pr(**kwargs):
    return parse_repo(raw_repo([raw_pr(**kwargs)])).prs[0]


def texts(p):
    return [r.text for r in p.reasons]


class StatusTest(unittest.TestCase):
    def test_clean_is_ready(self):
        self.assertEqual(pr().status, Status.READY)

    def test_has_hooks_is_ready(self):
        self.assertEqual(pr(merge_state="HAS_HOOKS").status, Status.READY)

    def test_draft(self):
        self.assertEqual(pr(is_draft=True, merge_state="DRAFT").status, Status.DRAFT)

    def test_unknown_mergeable_is_checking(self):
        self.assertEqual(pr(mergeable="UNKNOWN").status, Status.CHECKING)

    def test_unknown_merge_state_is_checking(self):
        self.assertEqual(pr(merge_state="UNKNOWN").status, Status.CHECKING)

    def test_conflicting(self):
        self.assertEqual(pr(mergeable="CONFLICTING", merge_state="DIRTY").status, Status.CONFLICT)

    def test_dirty_is_conflict(self):
        self.assertEqual(pr(merge_state="DIRTY").status, Status.CONFLICT)

    def test_conflict_beats_failing(self):
        p = pr(mergeable="CONFLICTING", merge_state="DIRTY", check_state="FAILURE")
        self.assertEqual(p.status, Status.CONFLICT)

    def test_failing(self):
        self.assertEqual(pr(merge_state="BLOCKED", check_state="FAILURE").status, Status.FAILING)

    def test_error_rollup_is_failing(self):
        self.assertEqual(pr(merge_state="BLOCKED", check_state="ERROR").status, Status.FAILING)

    def test_unstable_is_ready(self):
        self.assertEqual(pr(merge_state="UNSTABLE", check_state="FAILURE").status, Status.READY)

    def test_pending(self):
        self.assertEqual(pr(merge_state="BLOCKED", check_state="PENDING").status, Status.PENDING)

    def test_behind(self):
        self.assertEqual(pr(merge_state="BEHIND").status, Status.BEHIND)

    def test_blocked(self):
        self.assertEqual(pr(merge_state="BLOCKED").status, Status.BLOCKED)

    def test_unrecognized_merge_state_is_blocked(self):
        p = pr(merge_state="SOMETHING_NEW")
        self.assertEqual(p.status, Status.BLOCKED)
        self.assertIn("Merge state: SOMETHING_NEW", texts(p))

    def test_no_rollup(self):
        self.assertEqual(pr(check_state=None).status, Status.READY)


class ReasonsTest(unittest.TestCase):
    def test_ready_has_no_reasons(self):
        self.assertEqual(pr().reasons, [])

    def test_draft_reason(self):
        self.assertIn("Draft", texts(pr(is_draft=True)))

    def test_checking_reason(self):
        self.assertIn("GitHub is still computing mergeability", texts(pr(mergeable="UNKNOWN")))

    def test_conflict_reason_is_error(self):
        p = pr(mergeable="CONFLICTING", merge_state="DIRTY")
        self.assertEqual([(r.text, r.level) for r in p.reasons], [("Merge conflicts", "error")])

    def test_failed_checks_listed_by_name(self):
        p = pr(
            merge_state="BLOCKED",
            check_state="FAILURE",
            checks=[
                check_run("lint"),
                check_run("test (ubuntu)", conclusion="FAILURE"),
                status_context("ci/legacy", state="ERROR"),
            ],
        )
        self.assertEqual(texts(p), ["Check failed: test (ubuntu)", "Check failed: ci/legacy"])
        self.assertTrue(all(r.level == "error" for r in p.reasons))

    def test_unstable_failed_checks_are_warnings(self):
        p = pr(
            merge_state="UNSTABLE",
            check_state="FAILURE",
            checks=[check_run("optional", conclusion="FAILURE")],
        )
        self.assertEqual(
            [(r.text, r.level) for r in p.reasons], [("Check failed: optional", "warning")]
        )

    def test_truncated_checks(self):
        p = pr(checks=[check_run("a")], checks_total=25)
        self.assertIn("+24 more checks not shown", texts(p))

    def test_pending_checks_counted(self):
        p = pr(
            merge_state="BLOCKED",
            check_state="PENDING",
            checks=[
                check_run("a", status="IN_PROGRESS", conclusion=None),
                status_context("b", state="PENDING"),
                check_run("c"),
            ],
        )
        self.assertEqual(texts(p), ["2 checks pending"])

    def test_changes_requested(self):
        p = pr(merge_state="BLOCKED", review_decision="CHANGES_REQUESTED")
        self.assertEqual([(r.text, r.level) for r in p.reasons], [("Changes requested", "error")])

    def test_review_required(self):
        p = pr(merge_state="BLOCKED", review_decision="REVIEW_REQUIRED")
        self.assertEqual([(r.text, r.level) for r in p.reasons], [("Review required", "warning")])

    def test_behind(self):
        self.assertEqual(texts(pr(merge_state="BEHIND")), ["Behind base branch"])

    def test_blocked_without_other_reason(self):
        self.assertEqual(texts(pr(merge_state="BLOCKED")), ["Blocked by branch protection"])

    def test_approved_adds_nothing(self):
        self.assertEqual(pr(review_decision="APPROVED").reasons, [])


class ParseTest(unittest.TestCase):
    def test_fields(self):
        repo = parse_repo(raw_repo([raw_pr(number=7, title="T")], total=60, delete_on_merge=True))
        self.assertEqual(repo.name, "acme/api")
        self.assertEqual(repo.pr_total, 60)
        self.assertTrue(repo.delete_branch_on_merge)
        p = repo.prs[0]
        self.assertEqual((p.id, p.number, p.title, p.author), ("PR_7", 7, "T", "alice"))
        self.assertEqual((p.head_ref, p.base_ref, p.head_ref_id), ("feature-7", "main", "REF_1"))

    def test_merge_methods_in_preference_order(self):
        repo = parse_repo(raw_repo())
        self.assertEqual(
            repo.merge_methods, (MergeMethod.SQUASH, MergeMethod.MERGE, MergeMethod.REBASE)
        )

    def test_only_allowed_methods(self):
        repo = parse_repo(raw_repo(merge=False, squash=False))
        self.assertEqual(repo.merge_methods, (MergeMethod.REBASE,))

    def test_missing_optional_nodes(self):
        raw = raw_pr(head_ref_id=None, check_state=None)
        raw["author"] = None
        raw["headRepository"] = None
        raw["commits"] = {"nodes": []}
        p = parse_repo(raw_repo([raw])).prs[0]
        self.assertIsNone(p.head_ref_id)
        self.assertIsNone(p.head_repo)
        self.assertIsNone(p.check_state)
        self.assertEqual(p.author, "ghost")
        self.assertEqual(p.checks, ())


if __name__ == "__main__":
    unittest.main()
