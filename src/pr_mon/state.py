"""Persisted UI state: per-PR status and seen flags, collapsed repo groups."""

import json
from dataclasses import asdict, dataclass, field
from pathlib import Path

from pr_mon.files import app_dir, write_atomic


@dataclass
class AppState:
    prs: dict = field(default_factory=dict)  # Tracker records
    collapsed: list[str] = field(default_factory=list)  # lower-cased owner names
    # PRs pr-mon will merge when ready: {repo: {"<number>": armed_to_dict(...)}}
    armed: dict = field(default_factory=dict)


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
    prs = data.get("prs", {})
    if not isinstance(prs, dict):
        prs = {}
    collapsed = data.get("collapsed", [])
    if not (isinstance(collapsed, list) and all(isinstance(o, str) for o in collapsed)):
        collapsed = []
    armed = data.get("armed", {})
    if not _valid_armed(armed):
        armed = {}
    return AppState(prs=prs, collapsed=collapsed, armed=armed), None


def _valid_armed(armed: object) -> bool:
    return isinstance(armed, dict) and all(
        isinstance(entries, dict) and all(isinstance(e, dict) for e in entries.values())
        for entries in armed.values()
    )


def save_state(path: Path, state: AppState) -> None:
    write_atomic(path, json.dumps(asdict(state), indent=1, sort_keys=True))
