# pr-mon

## Versioning

The TUI refuses to use a backend whose version differs from its own, so the
version number is how an old running backend gets detected.

Bump the version with `uv version --bump patch` in the same change whenever you
alter anything the TUI and the backend must agree on:

- the socket protocol (`protocol.py`): ops, their arguments or results, event
  names or payloads, snapshot fields
- `Backend` / `BackendStatus` / `BackendEvent` (`backend.py`)
- model serialization sent over the socket (`repo_to_dict`, `armed_to_dict`, …)
- `Action` kinds or fields
- `NotifyConfig` fields (they travel in snapshots and requests)

Commit the bumped `pyproject.toml` and `uv.lock` together with the change.
