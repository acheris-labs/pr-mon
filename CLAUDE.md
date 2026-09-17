# pr-mon

## Architecture

The backend (`pr-mon daemon`) does all the work. Clients (the TUI and the Mac
app in `macos/`) stay thin: they display what the backend sends
and send commands. Put new logic in the backend and send its results over the
socket rather than computing them in a client.

- Backend-only rules: `readiness.py` (status, reasons), `actions.py` (each PR's
  action menu). `tests/test_actions.py` fails if a TUI module imports backend code.
- The socket protocol is specified in `docs/protocol.md`;
  `tests/test_protocol_spec.py` checks the document against the code.
- `protocol-fixtures/` holds sample messages generated from Python
  (`make fixtures`) and decoded by the Swift tests (`make swift-test`). A stale fixture fails
  `tests/test_protocol_fixtures.py`.

## Versioning

Two version numbers guard the TUI ↔ backend connection:

- **`PROTOCOL_VERSION`** (`protocol.py`) is what every client, in any language,
  checks. Bump it (and the number at the top of `docs/protocol.md`) when a
  client written against the old document would break: removing or renaming a
  field, op, argument or event, changing a type or meaning, or adding a
  required argument. Adding a field is not a break.
- **The package version** is what the TUI also checks, so it restarts a backend
  running older code. Bump it with `uv version --bump patch` in the same change
  whenever you alter anything the TUI and the backend must agree on:
  - the socket protocol (`protocol.py`): ops, their arguments or results, event
    names or payloads, snapshot fields, including added fields
  - `Backend` / `BackendStatus` / `BackendEvent` (`backend.py`)
  - model serialization sent over the socket (`repo_to_dict`, `armed_to_dict`, …)
  - `Action` kinds or fields
  - `NotifyConfig` fields (they travel in snapshots and requests)

Any wire change also updates `docs/protocol.md` and the fixtures (`make fixtures`).
Commit the bumped `pyproject.toml` and `uv.lock` together with the change.

## Mac app

- `macos/PrMonKit`: Swift package (client, models, state); tests use Swift Testing.
- `macos/PrMon`: SwiftUI app; `project.yml` is the XcodeGen spec, the generated
  `.xcodeproj` isn't committed. Build with `make app`.
- The app should look like a standard Mac app, not a port of the TUI: system
  fonts and colours, standard Settings tabs, no key-hint bars.
- Backend control the app can't reach over the socket (autostart, restart,
  stop) runs the `pr-mon` command line through the user's login shell.
- Wire changes also need the Swift models (`Models.swift`) updated.
