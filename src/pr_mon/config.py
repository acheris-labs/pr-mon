"""User configuration: which repos to watch, how often, and how to notify."""

import json
import tomllib
from dataclasses import dataclass, field, fields
from pathlib import Path

from pr_mon.files import app_dir, write_atomic

DEFAULT_POLL_INTERVAL = 60
DEFAULT_MESSAGE = "{{PR_REPO}}#{{PR_NUM}} is {{PR_STATE}}: {{PR_TITLE}} {{PR_URL}}"
EVENT_NAMES = ("READY", "FAILING", "CONFLICT", "BLOCKED", "BEHIND", "PENDING", "NEW")
DEFAULT_EVENTS = ("READY", "FAILING", "CONFLICT")


@dataclass
class NotifyConfig:
    message: str = DEFAULT_MESSAGE
    events: list[str] = field(default_factory=lambda: list(DEFAULT_EVENTS))
    include_drafts: bool = False
    script_enabled: bool = False
    script: str = ""
    desktop_enabled: bool = False


@dataclass
class Config:
    repos: list[str] = field(default_factory=list)
    poll_interval: int = DEFAULT_POLL_INTERVAL
    notifications: NotifyConfig = field(default_factory=NotifyConfig)


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
    notifications = _parse_notifications(data.get("notifications", {}))
    warning = None
    if notifications is None:
        notifications = NotifyConfig()
        warning = f"Ignoring invalid [notifications] in {path}; using defaults"
    return Config(repos=repos, poll_interval=interval, notifications=notifications), warning


def _parse_notifications(data: object) -> NotifyConfig | None:
    """Build NotifyConfig from a TOML table; None if anything is the wrong type."""
    if not isinstance(data, dict):
        return None
    values = {}
    for f in fields(NotifyConfig):
        if f.name not in data:
            continue
        value = data[f.name]
        if f.name == "events":
            ok = isinstance(value, list) and all(v in EVENT_NAMES for v in value)
        elif f.type is bool:
            ok = isinstance(value, bool)
        else:
            ok = isinstance(value, str)
        if not ok:
            return None
        values[f.name] = value
    return NotifyConfig(**values)


def _toml(value: object) -> str:
    # TOML also forbids a raw DEL, which JSON leaves unescaped.
    return json.dumps(value, ensure_ascii=False).replace("\x7f", "\\u007f")


def save_config(path: Path, config: Config) -> None:
    # JSON string escapes are valid TOML basic-string escapes, except \\u surrogate
    # pairs, so non-ASCII text is written literally.
    lines = [f"poll_interval = {config.poll_interval}", "repos = ["]
    lines += [f"    {_toml(repo)}," for repo in config.repos]
    lines.append("]")
    n = config.notifications
    lines += [
        "",
        "[notifications]",
        f"message = {_toml(n.message)}",
        f"events = {_toml(n.events)}",
        f"include_drafts = {_toml(n.include_drafts)}",
        f"script_enabled = {_toml(n.script_enabled)}",
        f"script = {_toml(n.script)}",
        f"desktop_enabled = {_toml(n.desktop_enabled)}",
    ]
    write_atomic(path, "\n".join(lines) + "\n")
