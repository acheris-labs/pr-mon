"""Test doubles shared by service and TUI tests."""

from pr_mon.github import NotFoundError
from pr_mon.models import parse_repo
from tests.fixtures import raw_pr, raw_repo


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
