"""Command-line entry point."""

import sys

from pr_mon.app import PrMonApp
from pr_mon.config import default_config_path
from pr_mon.github import AuthError, GitHubClient, get_token
from pr_mon.notify import detect_desktop_notifier
from pr_mon.service import Monitor
from pr_mon.state import default_state_path


def main() -> None:
    try:
        client = GitHubClient(get_token)
    except AuthError as e:
        sys.exit(f"pr-mon: {e}. Run `gh auth login`.")
    monitor = Monitor(
        client,
        default_config_path(),
        default_state_path(),
        notifier=detect_desktop_notifier(),
    )
    PrMonApp(monitor, owns_backend=True).run()
