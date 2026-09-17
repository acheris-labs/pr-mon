"""Wire format between the daemon and its clients: one JSON object per line.

Request:  {"id": 7, "op": "mark_seen", "args": {...}}
Response: {"id": 7, "ok": true, "result": ...} or {"id": 7, "ok": false, "error": "..."}
Event:    {"event": "repo", "data": {...}}

The full specification is docs/protocol.md.
"""

import json
from dataclasses import asdict

from pr_mon.backend import BackendEvent, BackendStatus
from pr_mon.config import Config, NotifyConfig, parse_notify_config
from pr_mon.models import Action, MergeMethod, armed_to_dict, repo_to_dict

# Bump when the wire format changes (see CLAUDE.md); clients refuse other versions.
PROTOCOL_VERSION = 1
EVENT_KINDS = ("repos", "repo", "seen", "collapsed", "config", "status", "toast")
ACTION_KINDS = (
    "merge",
    "update",
    "auto_merge_on",
    "auto_merge_off",
    "arm_merge",
    "disarm_merge",
)


class ProtocolError(Exception):
    pass


def encode(message: dict) -> bytes:
    # json.dumps escapes newlines inside strings, so the message stays on one line.
    return json.dumps(message, ensure_ascii=False).encode() + b"\n"


def decode(line: bytes) -> dict:
    try:
        message = json.loads(line.decode())
    except (UnicodeDecodeError, ValueError) as e:
        raise ProtocolError(f"malformed message: {e}") from e
    if not isinstance(message, dict):
        raise ProtocolError("message is not an object")
    return message


def settings_to_dict(settings: NotifyConfig) -> dict:
    return asdict(settings)


def settings_from_dict(data: object) -> NotifyConfig:
    settings = parse_notify_config(data) if isinstance(data, dict) else None
    if settings is None:
        raise ProtocolError("invalid notification settings")
    return settings


def action_to_dict(action: Action) -> dict:
    return {
        "kind": action.kind,
        "method": str(action.method) if action.method else None,
        "delete_branch": action.delete_branch,
    }


def action_from_dict(data: dict) -> Action:
    try:
        if data["kind"] not in ACTION_KINDS:
            raise ValueError(data["kind"])
        method = MergeMethod(data["method"]) if data.get("method") else None
        return Action(data["kind"], method, bool(data.get("delete_branch", False)))
    except (KeyError, ValueError, TypeError) as e:
        raise ProtocolError(f"invalid action: {e}") from e


def status_to_dict(status: BackendStatus) -> dict:
    return asdict(status)


def status_from_dict(data: dict) -> BackendStatus:
    return BackendStatus(**data)


def config_to_dict(config: Config) -> dict:
    return {
        "repos": list(config.repos),
        "poll_interval": config.poll_interval,
        "notifications": {
            name: settings_to_dict(settings) for name, settings in config.notifications.items()
        },
    }


def config_from_dict(data: dict) -> Config:
    return Config(
        repos=list(data["repos"]),
        poll_interval=data["poll_interval"],
        notifications={
            name: settings_from_dict(settings) for name, settings in data["notifications"].items()
        },
    )


def _unseen(backend, name: str) -> list[int]:
    return sorted(backend.unseen(name))


def _armed(backend, name: str) -> dict:
    return {str(number): armed_to_dict(merge) for number, merge in backend.armed(name).items()}


def snapshot(backend) -> dict:
    """Everything a client needs to render, from any Backend."""
    return {
        "config": config_to_dict(backend.config),
        "repos": {name: repo_to_dict(repo) for name, repo in backend.repos.items()},
        "errors": dict(backend.errors),
        "unseen": {name: _unseen(backend, name) for name in backend.config.repos},
        "collapsed": sorted(backend.collapsed),
        "armed": {name: _armed(backend, name) for name in backend.config.repos},
        "status": status_to_dict(backend.status),
    }


def event_message(backend, event: BackendEvent) -> dict:
    """Wire form of a backend event, carrying the data a client mirror needs."""
    if event.kind == "repos":
        data = snapshot(backend)
    elif event.kind == "repo":
        repo = backend.repos.get(event.name)
        data = {
            "name": event.name,
            "repo": repo_to_dict(repo) if repo else None,
            "error": backend.errors.get(event.name),
            "unseen": _unseen(backend, event.name),
            "armed": _armed(backend, event.name),
        }
    elif event.kind == "seen":
        data = {"name": event.name, "unseen": _unseen(backend, event.name)}
    elif event.kind == "collapsed":
        data = {"collapsed": sorted(backend.collapsed)}
    elif event.kind == "config":
        data = {"config": config_to_dict(backend.config)}
    elif event.kind == "status":
        data = {"status": status_to_dict(backend.status)}
    elif event.kind == "toast":
        data = {"message": event.message, "severity": event.severity}
    else:
        raise ProtocolError(f"unknown event kind {event.kind!r}")
    return {"event": event.kind, "data": data}
