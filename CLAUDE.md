# pr-mon

## Architecture

One Go binary (`cmd/pr-mon`): `pr-mon` opens the dashboard, `pr-mon daemon` runs
the backend. The backend does all the work — polling GitHub, working out
readiness, merging, notifying — and clients stay thin: they display what it
sends and send commands back.

- Backend-only rules: `internal/readiness` (status, reasons) and
  `internal/actions` (each PR's action menu). Clients never apply these rules
  themselves; the results travel over the socket.
- The socket protocol is specified in `docs/protocol.md`;
  `internal/protocol/spec_test.go` checks the document against the code.
- `protocol-fixtures/` holds sample messages written by `make fixtures`
  (`cmd/gen-fixtures`). The Go tests and the Swift client's tests both read
  them, so the two implementations are checked against the same bytes.
- The Mac app in `macos/` is a separate client over the same socket.

## Versioning

Two version numbers guard the client ↔ backend connection:

- **`protocol.Version`** is what every client, in any language, checks. Bump it
  (and the number at the top of `docs/protocol.md`) when a client written
  against the old document would break: removing or renaming a field, op,
  argument or event, changing a type or meaning, or adding a required argument.
  Adding a field is not a break.
- **The build version** (`-X main.Version`, from the git tag) is what the
  dashboard also checks, so it restarts a backend running older code.

Any wire change also updates `docs/protocol.md`, the fixtures (`make fixtures`)
and the Swift models (`macos/PrMonKit/Sources/PrMonKit/Models.swift`).

## Go

- `make test` runs everything with the race detector; `make lint` is gofmt plus
  go vet. Keep both clean.
- Tests are standard `testing`, table-driven where it helps, with the fakes in
  `internal/testfixtures`.
- Dependencies: BurntSushi/toml and Charm's bubbletea, bubbles and lipgloss.
  Ask before adding more.

## Mac app

- `macos/PrMonKit`: Swift package (client, models, state); tests use Swift Testing.
- `macos/PrMon`: SwiftUI app; `project.yml` is the XcodeGen spec, the generated
  `.xcodeproj` isn't committed. Build with `make app`.
- The app should look like a standard Mac app, not a port of the TUI: system
  fonts and colours, standard Settings tabs, no key-hint bars.
- Backend control the app can't reach over the socket (autostart, restart,
  stop) runs the `pr-mon` command line.
