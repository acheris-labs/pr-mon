"""Persisted per-PR status and seen flags."""

import json
from pathlib import Path

from pr_mon.files import app_dir, write_atomic


def default_state_path() -> Path:
    return app_dir("XDG_STATE_HOME", Path.home() / ".local" / "state") / "state.json"


def load_state(path: Path) -> tuple[dict, str | None]:
    """Return saved state and a warning if the file was unusable."""
    try:
        data = json.loads(path.read_text())
    except FileNotFoundError:
        return {}, None
    except (OSError, ValueError) as e:
        return {}, f"Ignoring unreadable state {path}: {e}"
    if not isinstance(data, dict):
        return {}, f"Ignoring invalid state {path}: expected an object"
    return data, None


def save_state(path: Path, data: dict) -> None:
    write_atomic(path, json.dumps(data, indent=1, sort_keys=True))
