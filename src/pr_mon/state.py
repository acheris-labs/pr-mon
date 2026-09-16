"""Persisted UI state: per-PR status and seen flags, collapsed repo groups."""

import json
from dataclasses import asdict, dataclass, field
from pathlib import Path

from pr_mon.files import app_dir, write_atomic


@dataclass
class AppState:
    prs: dict = field(default_factory=dict)  # Tracker records
    collapsed: list[str] = field(default_factory=list)  # lower-cased owner names


def default_state_path() -> Path:
    return app_dir("XDG_STATE_HOME", Path.home() / ".local" / "state") / "state.json"


def load_state(path: Path) -> tuple[AppState, str | None]:
    """Return saved state and a warning if the file was unusable."""
    try:
        data = json.loads(path.read_text())
    except FileNotFoundError:
        return AppState(), None
    except (OSError, ValueError) as e:
        return AppState(), f"Ignoring unreadable state {path}: {e}"
    if not isinstance(data, dict):
        return AppState(), f"Ignoring invalid state {path}: expected an object"
    if "prs" not in data:
        # Before collapsed groups were saved, the file held only the PR records.
        return AppState(prs=data), None
    prs = data["prs"] if isinstance(data["prs"], dict) else {}
    collapsed = data.get("collapsed", [])
    if not (isinstance(collapsed, list) and all(isinstance(o, str) for o in collapsed)):
        collapsed = []
    return AppState(prs=prs, collapsed=collapsed), None


def save_state(path: Path, state: AppState) -> None:
    write_atomic(path, json.dumps(asdict(state), indent=1, sort_keys=True))
