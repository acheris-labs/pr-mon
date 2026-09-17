"""Run the real daemon with a fake GitHub client (for lifecycle tests).

Usage: python -m tests.daemon_harness STATE_DIR [owner/repo ...]
"""

import asyncio
import sys
from pathlib import Path

from pr_mon.daemon import AlreadyRunning, DaemonPaths, run_daemon
from tests.fakes import FakeClient, make_repo


def main() -> None:
    directory = Path(sys.argv[1])
    client = FakeClient({name: make_repo(name, (1, {})) for name in sys.argv[2:]})
    try:
        asyncio.run(
            run_daemon(
                DaemonPaths(directory),
                client,
                directory / "config.toml",
                directory / "state.json",
                notifier=None,
                version="test",
            )
        )
    except AlreadyRunning as e:
        print(e, file=sys.stderr)
        sys.exit(3)


if __name__ == "__main__":
    main()
