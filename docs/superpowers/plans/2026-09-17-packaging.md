# pr-mon Packaging Plan — one Homebrew cask

Date: 2026-09-17 (rewritten after the Go port)

**Goal:** `brew install --cask acheris-labs/tools/pr-mon` installs everything:
`PrMon.app`, the `pr-mon` CLI/TUI on PATH, and the backend, with nothing else
required on the user's machine.

**Shape (copied from `agent-scoreboard`, which is the same architecture):**
one notarized artifact. The Swift app bundle carries the Go `pr-mon` binary in
`Contents/Resources/bin/`, and the cask's `binary` stanza symlinks it into
Homebrew's prefix.

**What the Go port changed:** the CLI is now a single static binary of ~15MB,
so there is no PyInstaller step, no bundled interpreter and no signing of
hundreds of nested libraries — just one more Mach-O to sign inside the bundle.

**Sources to copy from:**

| Piece | Copy from |
|-------|-----------|
| Bundle layout, `app`+`binary` cask, sign-inner-then-seal, staple-then-rezip | `~/src/agent-scoreboard` |
| `SIGN_ID ?= -` / `HARDENED`, `_devid_detect`, `setup-notary`, `setup-secrets`, dmg target | `~/src/newt/Makefile` |
| `build.yml`, `release.yml` (keychain import, notarytool credentials, CHANGELOG notes, `brew` job) | `~/src/newt/.github/workflows` |
| `SMAppService.mainApp` login item with a bootstrap flag | `~/src/agent-scoreboard` |

## Decisions

1. **One cask, no formula.** `app "PrMon.app"` + `binary
   "#{appdir}/PrMon.app/Contents/Resources/bin/pr-mon"`. Homebrew puts `pr-mon`
   on PATH and removes it on uninstall.
2. **Universal binary:** build the Go CLI for arm64 and amd64 and `lipo` them,
   so one cask serves both Macs.
3. **The app runs the bundled binary by absolute path,** so "Start Backend",
   Restart and Stop work with nothing else installed. `Launcher` keeps a PATH
   fallback for source checkouts.
4. **Autostart via `SMAppService.agent`** with a LaunchAgent plist inside the
   bundle pointing at the bundled binary. `pr-mon autostart` (launchctl) stays
   for people who install only the CLI.
5. **The version is the tag,** stamped into both the Go binary
   (`-X main.Version`) and the app (`MARKETING_VERSION`).
6. **No Sparkle for now:** updates come from `brew upgrade`.

## Layout

```
PrMon.app/Contents/
  MacOS/PrMon                      # SwiftUI app
  Resources/bin/pr-mon             # Go binary (universal)
  Library/LaunchAgents/com.acheris-labs.pr-mon.backend.plist
```

## Tasks

### Task 1: Bundle the CLI in the app

- [ ] `make app` builds `pr-mon` for both architectures, `lipo`s them, and
      copies the result into `Contents/Resources/bin/`.
- [ ] `PrMonKit.Launcher`: resolve the CLI as bundled binary → `pr-mon` on PATH
      → an error naming both; run it directly, not through a login shell.
- [ ] Verify live: with `pr-mon` off PATH, Start/Restart/Stop work from the
      Backend settings tab.

### Task 2: Autostart through SMAppService

- [ ] Bundle `Contents/Library/LaunchAgents/com.acheris-labs.pr-mon.backend.plist`
      running the bundled binary with `daemon`, `KeepAlive` on failure only.
- [ ] `BackendLoginItem` wrapping `SMAppService.agent(plistName:)`; the General
      tab uses it when bundled, else the CLI path as today.
- [ ] Live check: enable, confirm with `launchctl print gui/$UID/…`, then
      disable. Ask before enabling for real.

### Task 3: Sign, notarize, package

- [ ] Copy newt's `SIGN_ID ?= -` / `HARDENED`, `_devid_detect`, `setup-notary`,
      `setup-secrets` (add pr-mon to the `REPOS` list).
- [ ] `make sign`: sign `Resources/bin/pr-mon` first, then the app bundle.
      Inside-out, never `--deep`.
- [ ] `make zip`: `ditto` → `notarytool submit --wait` → `stapler staple` the
      bundle → re-zip. `make dmg` after that, signed, notarized and stapled.
- [ ] `make verify`: `codesign --verify --strict`, `spctl -a -vv`,
      `stapler validate`.

### Task 4: Release workflow

- [ ] `build.yml` (macos-15, push/PR): `make lint test`, `make swift-test`,
      ad-hoc `make app`, upload the app as an artifact.
- [ ] `release.yml` on `v*` tags: import the certificate into a temporary
      keychain (newt's step verbatim), store notarytool credentials,
      `make dmg zip SIGN_ID=… NOTARY_PROFILE=…`, write a `.sha256`, create the
      release with notes from `CHANGELOG.md`, always delete the keychain.
- [ ] `brew` job (ubuntu): render `packaging/pr-mon-cask.rb.tmpl` with
      `envsubst` and push to `acheris-labs/homebrew-tools`.
- [ ] Start `CHANGELOG.md` (Keep a Changelog).

### Task 5: The cask

- [ ] `app "PrMon.app"` + `binary "…/Contents/Resources/bin/pr-mon"`,
      `depends_on macos: :sonoma`.
- [ ] `uninstall quit:` the bundle id, and stop the backend first
      (`pr-mon stop`, `must_succeed: false`).
- [ ] `zap trash:` `~/.config/pr-mon`, `~/.local/state/pr-mon`, preferences and
      saved state.

### Task 6: Docs and verification

- [ ] README: install via cask first, `make install` second (development).
- [ ] `DISTRIBUTING.md`: credentials, `make setup-notary`, tagging, what CI does.
- [ ] End to end on this machine: `make dmg`, install the built cask from a
      local file, confirm `pr-mon` on PATH, the app connects, autostart
      registers, then `brew uninstall --zap` leaves nothing behind.

## Risks

| Risk | Handling |
|------|----------|
| Notarization rejects the nested Go binary | Sign it before sealing the bundle; `spctl -a -vv` before releasing |
| A cask depending on nothing else must carry everything | The Go binary is static apart from libSystem, so it does |
| macOS runner minutes on a private repo (10×) | The signing job runs only on tags |
