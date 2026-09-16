"""Command-line entry point."""

import sys

from pr_mon.app import PrMonApp
from pr_mon.config import default_config_path
from pr_mon.github import AuthError, GitHubClient, get_token
from pr_mon.notify import detect_desktop_notifier
from pr_mon.state import default_state_path


def main() -> None:
    try:
        token = get_token()
    except AuthError as e:
        sys.exit(f"pr-mon: {e}. Run `gh auth login`.")
    app = PrMonApp(
        GitHubClient(token),
        default_config_path(),
        default_state_path(),
        notifier=detect_desktop_notifier(),
    )
    app.run()
