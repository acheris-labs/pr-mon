import json
import os
import stat
import tempfile
import time
import unittest
from pathlib import Path
from unittest import mock

from pr_mon.config import NotifyConfig
from pr_mon.github import parse_repo
from pr_mon.models import Status
from pr_mon.notify import (
    SAMPLE_VARIABLES,
    Notification,
    deliver,
    desktop_argv,
    detect_desktop_notifier,
    pr_variables,
    render,
    run_command,
    sample_variables,
    script_argv,
    select_notifications,
    unknown_placeholders,
)
from pr_mon.tracker import Change
from tests.fixtures import raw_pr, raw_repo

VARS = {"PR_NUM": "12", "PR_STATE": "READY", "PR_URL": "https://x/12"}


class RenderTest(unittest.TestCase):
    def test_substitutes(self):
        self.assertEqual(
            render("PR {{PR_NUM}} is {{ PR_STATE }}: {{PR_URL}}", VARS),
            "PR 12 is READY: https://x/12",
        )

    def test_unknown_left_verbatim(self):
        self.assertEqual(render("{{PR_TYPO}} {{PR_NUM}}", VARS), "{{PR_TYPO}} 12")

    def test_values_are_not_rescanned(self):
        self.assertEqual(
            render("{{PR_TITLE}}", {"PR_TITLE": "{{PR_NUM}} $(rm -rf ~)", "PR_NUM": "1"}),
            "{{PR_NUM}} $(rm -rf ~)",
        )

    def test_unknown_placeholders(self):
        self.assertEqual(
            unknown_placeholders("{{PR_NUM}} {{ nope }} {{PR_TYPO}} {{nope}}"),
            ["nope", "PR_TYPO"],
        )
        self.assertEqual(unknown_placeholders("{{PR_REPO}}#{{PR_NUM}}"), [])

    def test_sample_covers_every_variable(self):
        self.assertEqual(
            sorted(SAMPLE_VARIABLES),
            sorted(
                [
                    "PR_REPO",
                    "PR_NUM",
                    "PR_TITLE",
                    "PR_AUTHOR",
                    "PR_BRANCH",
                    "PR_TARGET",
                    "PR_STATE",
                    "PR_URL",
                    "PR_REASON",
                ]
            ),
        )
        self.assertEqual(SAMPLE_VARIABLES["PR_REASON"], "")


def repo_of(*prs):
    return parse_repo(raw_repo([raw_pr(number=n, **kw) for n, kw in prs]))


class VariablesTest(unittest.TestCase):
    def test_sample_variables_use_repo(self):
        v = sample_variables("acme/web")
        self.assertEqual(v["PR_REPO"], "acme/web")
        self.assertEqual(v["PR_URL"], "https://github.com/acme/web/pull/123")
        self.assertEqual(v["PR_TITLE"], SAMPLE_VARIABLES["PR_TITLE"])

    def test_pr_variables(self):
        pr = repo_of((7, {"title": "Fix it"})).prs[0]
        self.assertEqual(
            pr_variables("acme/api", pr, "FAILING"),
            {
                "PR_REPO": "acme/api",
                "PR_NUM": "7",
                "PR_TITLE": "Fix it",
                "PR_AUTHOR": "alice",
                "PR_BRANCH": "feature-7",
                "PR_TARGET": "main",
                "PR_STATE": "FAILING",
                "PR_URL": "https://github.com/acme/api/pull/7",
                "PR_REASON": "",
            },
        )
        failed = pr_variables("acme/api", pr, "MERGE_FAILED", reason="no permission")
        self.assertEqual(failed["PR_REASON"], "no permission")


class SelectTest(unittest.TestCase):
    def setUp(self):
        self.repo = repo_of((1, {}), (2, {"is_draft": True}))

    def select(self, changes, **settings):
        found = select_notifications(self.repo, changes, NotifyConfig(**settings))
        return [(n.pr.number, n.state) for n in found]

    def test_new_only_when_selected(self):
        change = [Change(1, None, Status.READY)]
        self.assertEqual(self.select(change), [])
        self.assertEqual(self.select(change, events=["NEW"]), [(1, "NEW")])

    def test_enters_selected_status(self):
        self.assertEqual(self.select([Change(1, Status.PENDING, Status.READY)]), [(1, "READY")])

    def test_unselected_status_is_quiet(self):
        self.assertEqual(self.select([Change(1, Status.READY, Status.BEHIND)]), [])

    def test_move_between_selected_statuses(self):
        self.assertEqual(
            self.select([Change(1, Status.FAILING, Status.CONFLICT)]), [(1, "CONFLICT")]
        )

    def test_drafts(self):
        change = [Change(2, None, Status.DRAFT)]
        self.assertEqual(self.select(change, events=["NEW"]), [])
        self.assertEqual(self.select(change, events=["NEW"], include_drafts=True), [(2, "NEW")])

    def test_returns_notifications(self):
        found = select_notifications(
            self.repo, [Change(1, Status.PENDING, Status.READY)], NotifyConfig()
        )
        self.assertEqual(found, [Notification("acme/api", self.repo.prs[0], "READY")])


class DesktopTest(unittest.TestCase):
    def test_ladder(self):
        cases = [
            ({"terminal-notifier", "osascript", "notify-send"}, "terminal-notifier"),
            ({"osascript", "notify-send"}, "osascript"),
            ({"notify-send"}, "notify-send"),
            (set(), None),
        ]
        for present, expected in cases:
            with (
                self.subTest(present=present),
                mock.patch(
                    "pr_mon.notify.shutil.which",
                    side_effect=lambda t, p=present: f"/bin/{t}" if t in p else None,
                ),
            ):
                found = detect_desktop_notifier()
                self.assertEqual(found, f"/bin/{expected}" if expected else None)

    def test_argv(self):
        v = dict(SAMPLE_VARIABLES)
        self.assertEqual(
            desktop_argv("/x/terminal-notifier", "-hi", v),
            [
                "/x/terminal-notifier",
                "-title",
                "pr-mon",
                "-subtitle",
                v["PR_REPO"],
                "-message",
                "-hi",
                "-open",
                v["PR_URL"],
            ],
        )
        self.assertEqual(
            desktop_argv("/usr/bin/osascript", 'say "x"', v),
            [
                "/usr/bin/osascript",
                "-e",
                "on run argv",
                "-e",
                "display notification (item 1 of argv) with title (item 2 of argv)",
                "-e",
                "end run",
                'say "x"',
                "pr-mon",
            ],
        )
        self.assertEqual(
            desktop_argv("/usr/bin/notify-send", "-hi", v),
            ["/usr/bin/notify-send", "--", "pr-mon", "-hi"],
        )


class ScriptArgvTest(unittest.TestCase):
    def test_split_and_tilde(self):
        with mock.patch.dict(os.environ, {"HOME": "/home/me"}):
            self.assertEqual(
                script_argv("~/bin/send --to 'two words' ~x $HOME"),
                ["/home/me/bin/send", "--to", "two words", "~x", "$HOME"],
            )

    def test_bad_commands(self):
        for command in ("", "   ", "send 'unterminated"):
            with self.subTest(command=command), self.assertRaises(ValueError):
                script_argv(command)


class ProcessTestCase(unittest.IsolatedAsyncioTestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.dir = Path(self.tmp.name)

    def script(self, name, body):
        path = self.dir / name
        path.write_text("#!/bin/sh\n" + body)
        path.chmod(path.stat().st_mode | stat.S_IXUSR)
        return path

    def out(self, name="out"):
        return (self.dir / name).read_text()


class RunCommandTest(ProcessTestCase):
    async def test_stdin_env_and_cwd(self):
        out = self.dir / "out"
        path = self.script("s", f'cat > "{out}"\necho "$PR_NUM|$PWD" >> "{out}"\n')
        with mock.patch.dict(os.environ, {"HOME": str(self.dir)}):
            error = await run_command([str(path)], "hello\nworld", {"PR_NUM": "12"})
        self.assertIsNone(error)
        self.assertEqual(self.out(), f"hello\nworld12|{self.dir.resolve()}\n")

    async def test_script_ignoring_stdin(self):
        path = self.script("s", "exit 0\n")
        self.assertIsNone(await run_command([str(path)], "x" * 1_000_000, {}))

    async def test_nonzero_exit(self):
        path = self.script("s", "echo 'first problem' >&2\necho second >&2\nexit 3\n")
        error = await run_command([str(path)], "", {})
        self.assertEqual(error, "s exited with code 3: first problem")

    async def test_nonzero_exit_without_stderr(self):
        path = self.script("s", "exit 4\n")
        self.assertEqual(await run_command([str(path)], "", {}), "s exited with code 4")

    async def test_missing_command(self):
        error = await run_command([str(self.dir / "nope")], "", {})
        self.assertIn("nope", error)

    async def test_not_executable(self):
        path = self.dir / "plain"
        path.write_text("hi")
        self.assertIn("plain", await run_command([str(path)], "", {}))

    async def test_timeout_kills_process_group(self):
        marker = self.dir / "marker"
        path = self.script("s", f'(sleep 2; touch "{marker}") &\nsleep 5\n')
        started = time.monotonic()
        error = await run_command([str(path)], None, {}, timeout=0.3)
        self.assertLess(time.monotonic() - started, 2)
        self.assertEqual(error, "s timed out after 0.3s")
        time.sleep(2.2)
        self.assertFalse(marker.exists())


class DeliverTest(ProcessTestCase):
    def settings(self, **kwargs):
        return NotifyConfig(message="PR {{PR_NUM}} {{PR_STATE}}", **kwargs)

    async def test_nothing_enabled(self):
        self.assertEqual(await deliver(self.settings(), None, SAMPLE_VARIABLES), [])

    async def test_script_and_desktop(self):
        out = self.dir / "out"
        script = self.script("send", f'cat > "{out}"\n')
        notifier = self.script(
            "terminal-notifier",
            f'python3 -c "import json,sys; print(json.dumps(sys.argv[1:]))" "$@" > "{out}2"\n',
        )
        results = await deliver(
            self.settings(script_enabled=True, script=str(script), desktop_enabled=True),
            str(notifier),
            SAMPLE_VARIABLES,
        )
        self.assertEqual(results, [("script", None), ("desktop", None)])
        message = f"PR {SAMPLE_VARIABLES['PR_NUM']} READY"
        self.assertEqual(self.out(), message)
        self.assertIn(message, json.loads(self.out("out2")))

    async def test_desktop_enabled_without_notifier_is_skipped(self):
        results = await deliver(self.settings(desktop_enabled=True), None, SAMPLE_VARIABLES)
        self.assertEqual(results, [])

    async def test_script_enabled_but_empty_is_skipped(self):
        results = await deliver(self.settings(script_enabled=True), None, SAMPLE_VARIABLES)
        self.assertEqual(results, [])

    async def test_bad_script_command(self):
        results = await deliver(
            self.settings(script_enabled=True, script="send 'oops"), None, SAMPLE_VARIABLES
        )
        self.assertEqual(len(results), 1)
        self.assertEqual(results[0][0], "script")
        self.assertIn("Bad script command", results[0][1])


if __name__ == "__main__":
    unittest.main()
