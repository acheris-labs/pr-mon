"""User configuration: which repos to watch and how often."""

import json
import tomllib
from dataclasses import dataclass, field
from pathlib import Path

from pr_mon.files import app_dir, write_atomic

DEFAULT_POLL_INTERVAL = 60


@dataclass
class Config:
    repos: list[str] = field(default_factory=list)
    poll_interval: int = DEFAULT_POLL_INTERVAL


def default_config_path() -> Path:
    return app_dir("XDG_CONFIG_HOME", Path.home() / ".config") / "config.toml"


def load_config(path: Path) -> tuple[Config, str | None]:
    """Return the config and a warning if the file was unusable."""
    try:
        data = tomllib.loads(path.read_text())
    except FileNotFoundError:
        return Config(), None
    except (OSError, tomllib.TOMLDecodeError) as e:
        return Config(), f"Ignoring unreadable config {path}: {e}"
    repos = data.get("repos", [])
    interval = data.get("poll_interval", DEFAULT_POLL_INTERVAL)
    valid_repos = isinstance(repos, list) and all(isinstance(r, str) for r in repos)
    valid_interval = isinstance(interval, int) and not isinstance(interval, bool) and interval > 0
    if not (valid_repos and valid_interval):
        return Config(), f"Ignoring invalid config {path}: bad 'repos' or 'poll_interval'"
    return Config(repos=repos, poll_interval=interval), None


def save_config(path: Path, config: Config) -> None:
    # JSON string escapes are valid TOML basic-string escapes.
    lines = [f"poll_interval = {config.poll_interval}", "repos = ["]
    lines += [f"    {json.dumps(repo)}," for repo in config.repos]
    lines.append("]")
    write_atomic(path, "\n".join(lines) + "\n")
