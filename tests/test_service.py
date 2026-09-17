import asyncio
import json
import tempfile
import unittest
from datetime import UTC, datetime, timedelta
from pathlib import Path
from unittest import mock

from pr_mon.backend import BackendError
from pr_mon.config import Config, NotifyConfig, load_config, save_config
from pr_mon.github import GitHubError, RateLimitError
from pr_mon.models import Action, MergeMethod
from pr_mon.service import CHECKING_RETRIES, Monitor
from pr_mon.state import AppState, load_state, save_state
from tests.fakes import FakeClient, FakeDeliver, make_repo

READY = {}
PENDING = {"merge_state": "BLOCKED", "check_state": "PENDING"}
FAILING = {"merge_state": "BLOCKED", "check_state": "FAILURE"}
CHECKING = {"mergeable": "UNKNOWN", "merge_state": "UNKNOWN"}
ON = NotifyConfig(script_enabled=True, script="im")


def auto_kind(monitor):
    """The kind the first PR's `a` menu entry would send."""
    pr = monitor.repos["acme/api"].prs[0]
    return next(o.kind for o in pr.actions if o.key == "auto_merge")


class MonitorTestCase(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.config_path = Path(self.tmp.name) / "config.toml"
        self.state_path = Path(self.tmp.name) / "state.json"
        self.deliver = FakeDeliver()
        patcher = mock.patch("pr_mon.service.deliver", self.deliver)
        patcher.start()
        self.addCleanup(patcher.stop)
        self.events = []

    async def start(self, repos, notifications=None, state=None, notifier=None):
        save_config(
            self.config_path,
            Config(repos=list(repos), notifications=notifications or {}),
        )
        if state is not None:
            save_state(self.state_path, state)
        self.client = FakeClient(repos)
        self.monitor = Monitor(
            self.client, self.config_path, self.state_path, notifier=notifier, version="9.9"
        )
        self.monitor.add_listener(self.events.append)
        await self.monitor.start()
        self.addAsyncCleanup(self.monitor.stop)
        await self.monitor.wait_idle()
        return self.monitor

    async def poll(self):
        await self.monitor.poll_once()
        await self.monitor.wait_idle()

    def kinds(self):
        return [e.kind for e in self.events]

    def toasts(self):
        return [(e.severity, e.message) for e in self.events if e.kind == "toast"]


class LifecycleTest(MonitorTestCase):
    async def test_start_loads_and_polls(self):
        m = await self.start({"acme/api": make_repo("acme/api", (1, READY))})
        self.assertEqual(list(m.repos), ["acme/api"])
        self.assertEqual(m.unseen("acme/api"), {1})
        self.assertEqual(m.status.version, "9.9")
        self.assertIsNotNone(m.status.last_update)
        self.assertIn("repo", self.kinds())

    async def test_warnings_from_bad_files(self):
        self.state_path.write_text("{nope")
        m = await self.start({})
        self.assertEqual(len(m.status.warnings), 1)

    async def test_stop_saves_state_and_closes_client(self):
        m = await self.start({"acme/api": make_repo("acme/api", (1, READY))})
        self.state_path.unlink()
        await m.stop()
        self.assertIn("acme/api", load_state(self.state_path)[0].prs)
        self.assertIn(("close",), self.client.calls)

    async def test_shutdown_sets_event(self):
        m = await self.start({})
        await m.shutdown()
        self.assertTrue(m.shutdown_requested.is_set())


class PollingTest(MonitorTestCase):
    async def test_fetch_error_keeps_last_data(self):
        m = await self.start({"acme/api": make_repo("acme/api", (1, READY))})
        self.client.repos["acme/api"] = GitHubError("down")
        await self.poll()
        self.assertEqual(m.errors, {"acme/api": "down"})
        self.assertEqual([p.number for p in m.repos["acme/api"].prs], [1])
        self.client.repos["acme/api"] = make_repo("acme/api")
        await self.poll()
        self.assertEqual(m.errors, {})

    async def test_rate_limit_pauses_polling(self):
        m = await self.start({"acme/api": make_repo("acme/api")})
        reset = datetime.now(UTC) + timedelta(minutes=5)
        self.client.repos["acme/api"] = RateLimitError("slow down", reset)
        await self.poll()
        self.assertEqual(m.status.rate_limited_until, reset.isoformat())
        fetches = len(self.client.calls)
        await self.poll()
        self.assertEqual(len(self.client.calls), fetches)

    async def test_rate_limit_expiry_resumes(self):
        m = await self.start({"acme/api": make_repo("acme/api")})
        self.client.repos["acme/api"] = RateLimitError(
            "x", datetime.now(UTC) - timedelta(seconds=1)
        )
        await self.poll()
        self.client.repos["acme/api"] = make_repo("acme/api")
        await self.poll()
        self.assertIsNone(m.status.rate_limited_until)

    async def test_checking_schedules_limited_retries(self):
        with mock.patch("pr_mon.service.CHECKING_RETRY_DELAY", 0.01):
            m = await self.start({"acme/api": make_repo("acme/api", (1, CHECKING))})
            for _ in range(CHECKING_RETRIES + 2):
                await asyncio.sleep(0.05)
                await m.wait_idle()
        fetches = [c for c in self.client.calls if c[0] == "fetch"]
        self.assertEqual(len(fetches), 1 + CHECKING_RETRIES)

    async def test_refresh_all_returns_immediately(self):
        m = await self.start({"acme/api": make_repo("acme/api")})
        before = len(self.client.calls)
        await m.refresh_all()
        await m.wait_idle()
        self.assertEqual(len(self.client.calls), before + 1)


class NotificationTest(MonitorTestCase):
    async def test_first_load_silent_then_changes_notify(self):
        state = AppState(prs={"acme/api": {"1": {"status": "PENDING", "seen": True}}})
        await self.start(
            {"acme/api": make_repo("acme/api", (1, READY))},
            notifications={"acme/api": ON},
            state=state,
        )
        self.assertEqual(self.deliver.calls, [])
        self.client.repos["acme/api"] = make_repo("acme/api", (1, FAILING))
        await self.poll()
        self.assertEqual(self.deliver.states(), [("acme/api", "1", "FAILING")])

    async def test_unconfigured_repo_is_silent(self):
        await self.start({"acme/api": make_repo("acme/api", (1, PENDING))})
        self.client.repos["acme/api"] = make_repo("acme/api", (1, READY))
        await self.poll()
        self.assertEqual(self.deliver.calls, [])

    async def test_failure_toast(self):
        await self.start(
            {"acme/api": make_repo("acme/api", (1, PENDING))}, notifications={"acme/api": ON}
        )
        self.deliver.results = [("script", "im exited with code 1")]
        self.client.repos["acme/api"] = make_repo("acme/api", (1, READY))
        await self.poll()
        self.assertIn(
            ("warning", "Script notification failed: im exited with code 1"), self.toasts()
        )

    async def test_send_test(self):
        m = await self.start({"acme/web": make_repo("acme/web")}, notifier="/x/tn")
        await m.send_test("acme/web", ON)
        await m.wait_idle()
        settings, notifier, variables = self.deliver.calls[0]
        self.assertEqual((settings, notifier, variables["PR_REPO"]), (ON, "/x/tn", "acme/web"))
        self.assertIn(("information", "Test script notification sent"), self.toasts())

    async def test_send_test_nothing_enabled(self):
        m = await self.start({"acme/web": make_repo("acme/web")})
        await m.send_test("acme/web", NotifyConfig())
        await m.wait_idle()
        self.assertTrue(any("Nothing to send" in msg for _, msg in self.toasts()))

    async def test_save_notifications(self):
        m = await self.start({"acme/api": make_repo("acme/api")})
        await m.save_notifications("acme/api", ON)
        self.assertEqual(load_config(self.config_path)[0].notifications, {"acme/api": ON})
        self.assertIn("config", self.kinds())
        with self.assertRaises(BackendError):
            await m.save_notifications("gone/repo", ON)


class RepoCommandTest(MonitorTestCase):
    async def test_add_repo(self):
        m = await self.start({"acme/api": make_repo("acme/api")})
        self.client.repos["Acme/New"] = make_repo("Acme/New", (3, READY))
        name = await m.add_repo("acme/new")
        self.assertEqual(name, "Acme/New")
        self.assertEqual(load_config(self.config_path)[0].repos, ["acme/api", "Acme/New"])
        self.assertEqual(m.unseen("Acme/New"), {3})
        self.assertIn("Acme/New", m.loaded_repos)
        self.assertEqual(self.kinds()[-2:], ["repos", "repo"])

    async def test_add_repo_errors(self):
        m = await self.start({"acme/api": make_repo("acme/api")})
        with self.assertRaisesRegex(BackendError, "already"):
            await m.add_repo("ACME/API")
        with self.assertRaisesRegex(BackendError, "not found"):
            await m.add_repo("acme/missing")

    async def test_remove_repo(self):
        m = await self.start(
            {"acme/api": make_repo("acme/api", (1, READY)), "acme/web": make_repo("acme/web")},
            notifications={"acme/api": ON},
        )
        await m.remove_repo("acme/api")
        config = load_config(self.config_path)[0]
        self.assertEqual((config.repos, config.notifications), (["acme/web"], {}))
        self.assertNotIn("acme/api", json.loads(self.state_path.read_text())["prs"])
        self.assertNotIn("acme/api", m.repos)
        self.assertNotIn("acme/api", m.loaded_repos)
        self.assertEqual(self.kinds()[-1], "repos")
        with self.assertRaises(BackendError):
            await m.remove_repo("acme/api")

    async def test_removed_repo_fetch_result_is_ignored(self):
        m = await self.start({"acme/api": make_repo("acme/api")})
        await m.remove_repo("acme/api")
        self.client.repos["acme/api"] = GitHubError("late")
        await m.refresh_repo("acme/api")
        self.assertEqual(m.errors, {})

    async def test_mark_seen(self):
        m = await self.start({"acme/api": make_repo("acme/api", (1, READY))})
        await m.mark_seen("acme/api", 1)
        await m.mark_seen("acme/api", 1)
        self.assertEqual(m.unseen("acme/api"), set())
        self.assertEqual(self.kinds().count("seen"), 1)
        self.assertEqual(load_state(self.state_path)[0].prs["acme/api"]["1"]["seen"], True)

    async def test_set_collapsed(self):
        m = await self.start({"acme/api": make_repo("acme/api")})
        await m.set_collapsed("acme", True)
        await m.set_collapsed("acme", True)
        self.assertEqual(load_state(self.state_path)[0].collapsed, ["acme"])
        self.assertEqual(self.kinds().count("collapsed"), 1)
        await m.set_collapsed("acme", False)
        self.assertEqual(m.collapsed, set())

    async def test_alert_expands_collapsed_owner(self):
        state = AppState(
            prs={"acme/api": {"1": {"status": "PENDING", "seen": True}}}, collapsed=["acme"]
        )
        m = await self.start({"acme/api": make_repo("acme/api", (1, PENDING))}, state=state)
        self.assertEqual(m.collapsed, {"acme"})
        self.client.repos["acme/api"] = make_repo("acme/api", (1, READY))
        await self.poll()
        self.assertEqual(m.collapsed, set())
        self.assertEqual(load_state(self.state_path)[0].collapsed, [])
        self.assertIn("collapsed", self.kinds())


class PerformTest(MonitorTestCase):
    async def start_one(self, **repo_kwargs):
        return await self.start({"acme/api": make_repo("acme/api", (1, READY), **repo_kwargs)})

    async def test_merge_with_delete(self):
        m = await self.start_one()
        await m.perform("acme/api", 1, Action("merge", MergeMethod.SQUASH, True))
        actions = [c for c in self.client.calls if c[0] not in ("fetch", "close")]
        self.assertEqual(actions, [("merge", "PR_1", MergeMethod.SQUASH), ("delete", "REF_1")])
        self.assertIn(("information", "Merged acme/api#1 (squash)"), self.toasts())
        self.assertEqual(self.client.calls[-1], ("fetch", "acme/api"))

    async def test_delete_failure_is_warning(self):
        m = await self.start_one()
        self.client.delete_error = GitHubError("no push access")
        await m.perform("acme/api", 1, Action("merge", MergeMethod.SQUASH, True))
        self.assertEqual([s for s, _ in self.toasts()], ["information", "warning"])

    async def test_merge_failure_is_error(self):
        m = await self.start_one()
        self.client.merge_error = GitHubError("Base branch was modified")
        await m.perform("acme/api", 1, Action("merge", MergeMethod.SQUASH, True))
        self.assertEqual(
            self.toasts(), [("error", "Merge failed for acme/api#1: Base branch was modified")]
        )

    async def test_auto_merge_and_update(self):
        m = await self.start_one()
        await m.perform("acme/api", 1, Action("auto_merge_on", MergeMethod.REBASE))
        await m.perform("acme/api", 1, Action("auto_merge_off"))
        await m.perform("acme/api", 1, Action("update"))
        self.assertEqual(
            [msg for _, msg in self.toasts()],
            [
                "Auto-merge enabled for acme/api#1 (rebase)",
                "Auto-merge disabled for acme/api#1",
                "Updated branch for acme/api#1",
            ],
        )

    async def test_unknown_pr(self):
        m = await self.start_one()
        with self.assertRaisesRegex(BackendError, "not an open PR"):
            await m.perform("acme/api", 99, Action("update"))
        with self.assertRaises(BackendError):
            await m.perform("gone/repo", 1, Action("update"))


UNSTABLE = {"merge_state": "UNSTABLE", "check_state": "FAILURE"}
MERGE_EVENTS = NotifyConfig(script_enabled=True, script="im", events=["MERGED", "MERGE_FAILED"])


def pr_at(sha, **status):
    return {"head_sha": sha, **status}


class ArmedMergeTest(MonitorTestCase):
    async def start_armed(self, *prs, notifications=None, total=None, arm=True, delete=False):
        kwargs = {"total": total} if total is not None else {}
        m = await self.start(
            {"acme/api": make_repo("acme/api", *prs, **kwargs)},
            notifications={"acme/api": notifications} if notifications else None,
        )
        if arm:
            await m.perform("acme/api", 1, Action("arm_merge", MergeMethod.SQUASH, delete))
        self.events.clear()
        return m

    def set_prs(self, *prs, **kwargs):
        self.client.repos["acme/api"] = make_repo("acme/api", *prs, **kwargs)

    def merges(self):
        return [c for c in self.client.calls if c[0] == "merge"]

    async def test_arm_and_disarm(self):
        m = await self.start({"acme/api": make_repo("acme/api", (1, PENDING))})
        await m.perform("acme/api", 1, Action("arm_merge", MergeMethod.REBASE, True))
        armed = m.armed("acme/api")[1]
        self.assertEqual((armed.method, armed.delete_branch), (MergeMethod.REBASE, True))
        saved = load_state(self.state_path)[0].armed["acme/api"]["1"]
        self.assertEqual(saved["method"], "REBASE")
        self.assertIn(
            ("information", "pr-mon will merge acme/api#1 when it's ready (rebase)"),
            self.toasts(),
        )
        self.assertEqual(self.kinds()[-2:], ["toast", "repo"])
        self.assertEqual(auto_kind(m), "disarm_merge")
        await m.perform("acme/api", 1, Action("disarm_merge"))
        # This repo allows GitHub's own auto-merge, so that's what `a` offers again.
        self.assertEqual(auto_kind(m), "auto_merge_on")
        self.assertEqual(m.armed("acme/api"), {})
        self.assertEqual(load_state(self.state_path)[0].armed, {})
        self.assertIn(("information", "Cancelled merge when ready for acme/api#1"), self.toasts())
        self.assertEqual(self.merges(), [])

    async def test_merges_when_strictly_ready(self):
        m = await self.start_armed((1, pr_at("aaa", **PENDING)), notifications=MERGE_EVENTS)
        self.set_prs((1, pr_at("aaa")))
        fetches = len([c for c in self.client.calls if c[0] == "fetch"])
        await self.poll()
        self.assertEqual(self.merges(), [("merge", "PR_1", MergeMethod.SQUASH)])
        self.assertEqual(self.client.merge_heads, ["aaa"])
        self.assertEqual(m.armed("acme/api"), {})
        self.assertIn(
            ("information", "Merged acme/api#1 (pr-mon auto-merge, squash)"), self.toasts()
        )
        self.assertEqual(self.deliver.states(), [("acme/api", "1", "MERGED")])
        self.assertEqual(len([c for c in self.client.calls if c[0] == "fetch"]), fetches + 2)

    async def test_not_while_only_optionally_green(self):
        m = await self.start_armed((1, PENDING))
        self.set_prs((1, UNSTABLE))
        await self.poll()
        self.assertEqual(self.merges(), [])
        self.assertIn(1, m.armed("acme/api"))

    async def test_new_commits_keep_it_armed(self):
        await self.start_armed((1, pr_at("aaa", **PENDING)))
        self.set_prs((1, pr_at("bbb", **PENDING)))
        await self.poll()
        self.assertEqual(self.merges(), [])
        self.set_prs((1, pr_at("bbb")))
        await self.poll()
        self.assertEqual(self.client.merge_heads, ["bbb"])

    async def test_head_moved_is_retried_quietly(self):
        m = await self.start_armed((1, PENDING), notifications=MERGE_EVENTS)
        self.client.merge_errors = [
            GitHubError("Head branch was modified. Review and try the merge again.")
        ]
        self.set_prs((1, READY))
        await self.poll()
        self.assertIn(1, m.armed("acme/api"))
        self.assertEqual(self.toasts(), [])
        self.assertEqual(self.deliver.calls, [])
        await self.poll()
        self.assertEqual(len(self.merges()), 2)
        self.assertEqual(m.armed("acme/api"), {})

    async def test_other_failure_disarms_and_reports(self):
        m = await self.start_armed((1, PENDING), notifications=MERGE_EVENTS)
        self.client.merge_error = GitHubError("Merge queue is required")
        self.set_prs((1, READY))
        await self.poll()
        self.assertEqual(m.armed("acme/api"), {})
        self.assertIn(
            ("error", "pr-mon auto-merge failed for acme/api#1: Merge queue is required"),
            self.toasts(),
        )
        self.assertEqual(self.deliver.states(), [("acme/api", "1", "MERGE_FAILED")])
        self.assertEqual(self.deliver.calls[0][2]["PR_REASON"], "Merge queue is required")
        await self.poll()
        self.assertEqual(len(self.merges()), 1)

    async def test_deletes_branch_when_chosen(self):
        await self.start_armed((1, PENDING), delete=True)
        self.set_prs((1, READY))
        await self.poll()
        self.assertIn(("delete", "REF_1"), self.client.calls)

    async def test_branch_delete_failure_is_warning(self):
        m = await self.start_armed((1, PENDING), delete=True)
        self.client.delete_error = GitHubError("no push access")
        self.set_prs((1, READY))
        await self.poll()
        self.assertEqual([s for s, _ in self.toasts()], ["information", "warning"])
        self.assertEqual(m.armed("acme/api"), {})

    async def test_keeps_branch_when_not_chosen(self):
        await self.start_armed((1, PENDING))
        self.set_prs((1, READY))
        await self.poll()
        self.assertNotIn("delete", [c[0] for c in self.client.calls])

    async def test_closed_pr_is_disarmed(self):
        m = await self.start_armed((1, PENDING))
        self.set_prs()
        await self.poll()
        self.assertEqual(m.armed("acme/api"), {})
        self.assertEqual(load_state(self.state_path)[0].armed, {})

    async def test_truncated_list_keeps_it_armed(self):
        m = await self.start_armed((1, PENDING))
        self.set_prs((2, PENDING), total=80)
        await self.poll()
        self.assertIn(1, m.armed("acme/api"))

    async def test_merging_flag(self):
        m = await self.start_armed((1, PENDING))
        self.assertFalse(m.merging)
        self.client.merge_gate = asyncio.Event()
        self.set_prs((1, READY))
        await self.monitor.poll_once()
        await asyncio.sleep(0.01)
        self.assertTrue(m.merging)
        self.client.merge_gate.set()
        await m.wait_idle()
        self.assertFalse(m.merging)

    async def test_merging_flag_for_manual_merge(self):
        m = await self.start({"acme/api": make_repo("acme/api", (1, READY))})
        self.client.merge_gate = asyncio.Event()
        task = asyncio.create_task(m.perform("acme/api", 1, Action("merge", MergeMethod.SQUASH)))
        await asyncio.sleep(0.01)
        self.assertTrue(m.merging)
        self.client.merge_gate.set()
        await task
        self.assertFalse(m.merging)

    async def test_no_double_merge_while_in_flight(self):
        await self.start_armed((1, PENDING))
        self.client.merge_gate = asyncio.Event()
        self.set_prs((1, READY))
        await self.monitor.poll_once()
        await self.monitor.poll_once()
        await asyncio.sleep(0.01)
        self.client.merge_gate.set()
        await self.monitor.wait_idle()
        self.assertEqual(len(self.merges()), 1)

    async def test_remove_repo_drops_armed(self):
        m = await self.start_armed((1, PENDING))
        await m.remove_repo("acme/api")
        self.assertEqual(load_state(self.state_path)[0].armed, {})

    async def test_merges_on_first_load(self):
        state = AppState(
            armed={
                "acme/api": {
                    "1": {"method": "MERGE", "delete_branch": False, "armed_at": "2026-09-17"}
                }
            }
        )
        m = await self.start(
            {"acme/api": make_repo("acme/api", (1, READY))},
            notifications={"acme/api": MERGE_EVENTS},
            state=state,
        )
        self.assertEqual(self.merges(), [("merge", "PR_1", MergeMethod.MERGE)])
        self.assertEqual(m.armed("acme/api"), {})
        self.assertEqual(self.deliver.states(), [("acme/api", "1", "MERGED")])

    async def test_unconfigured_or_unselected_notifications(self):
        await self.start_armed((1, PENDING))
        self.set_prs((1, READY))
        await self.poll()
        self.assertEqual(self.deliver.calls, [])
        quiet = NotifyConfig(script_enabled=True, script="im", events=["FAILING"])
        m = await self.start_armed((1, PENDING), notifications=quiet)
        self.set_prs((1, READY))
        await self.poll()
        self.assertEqual(m.armed("acme/api"), {})
        self.assertEqual(self.deliver.calls, [])

    async def test_invalid_saved_entries_are_skipped(self):
        state = AppState(armed={"acme/api": {"1": {"method": "NOPE"}, "x": {}}})
        m = await self.start({"acme/api": make_repo("acme/api", (1, PENDING))}, state=state)
        self.assertEqual(m.armed("acme/api"), {})


if __name__ == "__main__":
    unittest.main()
