import unittest

from pr_mon.models import parse_repo
from pr_mon.tracker import Event, EventKind, Tracker
from tests.fixtures import raw_pr, raw_repo

READY = {}
CONFLICT = {"mergeable": "CONFLICTING", "merge_state": "DIRTY"}
FAILING = {"merge_state": "BLOCKED", "check_state": "FAILURE"}
PENDING = {"merge_state": "BLOCKED", "check_state": "PENDING"}
CHECKING = {"mergeable": "UNKNOWN", "merge_state": "UNKNOWN"}


def repo(*prs):
    """prs: (number, status-kwargs) pairs."""
    return parse_repo(raw_repo([raw_pr(number=n, **kw) for n, kw in prs]))


class TrackerTest(unittest.TestCase):
    def setUp(self):
        self.records = {}
        self.tracker = Tracker(self.records)

    def seen_all(self, *numbers):
        for n in numbers:
            self.tracker.mark_seen("acme/api", n)

    def test_first_poll_is_all_new(self):
        events = self.tracker.update(repo((1, READY), (2, PENDING)))
        self.assertEqual(
            events,
            [Event(EventKind.NEW, "acme/api", 1), Event(EventKind.NEW, "acme/api", 2)],
        )
        self.assertEqual(self.tracker.unseen("acme/api"), {1, 2})

    def test_stable_poll_has_no_events(self):
        self.tracker.update(repo((1, PENDING)))
        self.seen_all(1)
        self.assertEqual(self.tracker.update(repo((1, PENDING))), [])
        self.assertEqual(self.tracker.unseen("acme/api"), set())

    def test_became_ready(self):
        self.tracker.update(repo((1, PENDING)))
        self.seen_all(1)
        events = self.tracker.update(repo((1, READY)))
        self.assertEqual(events, [Event(EventKind.READY, "acme/api", 1)])
        self.assertTrue(self.tracker.is_unseen("acme/api", 1))

    def test_became_blocked(self):
        self.tracker.update(repo((1, READY)))
        self.seen_all(1)
        events = self.tracker.update(repo((1, CONFLICT)))
        self.assertEqual(events, [Event(EventKind.BLOCKED, "acme/api", 1)])
        self.assertTrue(self.tracker.is_unseen("acme/api", 1))

    def test_blocked_to_blocked_does_not_refire(self):
        self.tracker.update(repo((1, FAILING)))
        self.seen_all(1)
        self.assertEqual(self.tracker.update(repo((1, CONFLICT))), [])
        self.assertFalse(self.tracker.is_unseen("acme/api", 1))

    def test_ready_to_pending_is_quiet(self):
        self.tracker.update(repo((1, READY)))
        self.seen_all(1)
        self.assertEqual(self.tracker.update(repo((1, PENDING))), [])

    def test_checking_blip_does_not_refire_ready(self):
        self.tracker.update(repo((1, READY)))
        self.seen_all(1)
        self.assertEqual(self.tracker.update(repo((1, CHECKING))), [])
        self.assertEqual(self.tracker.update(repo((1, READY))), [])

    def test_closed_pr_dropped(self):
        self.tracker.update(repo((1, READY), (2, READY)))
        self.tracker.update(repo((2, READY)))
        self.assertEqual(set(self.records["acme/api"]), {"2"})
        self.assertEqual(self.tracker.unseen("acme/api"), {2})

    def test_mark_seen_reports_change(self):
        self.tracker.update(repo((1, READY)))
        self.assertTrue(self.tracker.mark_seen("acme/api", 1))
        self.assertFalse(self.tracker.mark_seen("acme/api", 1))
        self.assertFalse(self.tracker.mark_seen("acme/api", 99))
        self.assertFalse(self.tracker.mark_seen("other/repo", 1))

    def test_restart_is_quiet_for_unchanged(self):
        self.tracker.update(repo((1, READY), (2, PENDING)))
        self.seen_all(1, 2)
        restarted = Tracker(self.records)
        events = restarted.update(repo((1, READY), (2, READY)))
        self.assertEqual(events, [Event(EventKind.READY, "acme/api", 2)])
        self.assertEqual(restarted.unseen("acme/api"), {2})

    def test_restart_keeps_unseen(self):
        self.tracker.update(repo((1, READY)))
        restarted = Tracker(self.records)
        self.assertEqual(restarted.unseen("acme/api"), {1})

    def test_forget(self):
        self.tracker.update(repo((1, READY)))
        self.tracker.forget("acme/api")
        self.assertNotIn("acme/api", self.records)
        self.assertEqual(self.tracker.unseen("acme/api"), set())
        self.tracker.forget("never/added")
        self.tracker.unseen("never/read")
        self.assertNotIn("never/read", self.records)

    def test_malformed_records_are_reset(self):
        records = {"acme/api": ["junk"], "other/repo": {"1": "junk"}}
        tracker = Tracker(records)
        self.assertEqual(tracker.unseen("other/repo"), set())
        events = tracker.update(repo((1, READY)))
        self.assertEqual(events, [Event(EventKind.NEW, "acme/api", 1)])


if __name__ == "__main__":
    unittest.main()
