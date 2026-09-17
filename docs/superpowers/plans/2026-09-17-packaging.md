# pr-mon Packaging Plan — one Homebrew cask

Date: 2026-09-17

**Goal:** `brew install --cask acheris-labs/tools/pr-mon` installs everything:
`PrMon.app`, the `pr-mon` CLI/TUI on PATH, and the backend, with no Python,
uv or Xcode on the user's machine.

**Shape (copied from `agent-scoreboard`, which is the same architecture):**
one notarized artifact. The Swift app bundle carries a self-contained `pr-mon`
binary in `Contents/Resources/bin/`, and the cask's `binary` stanza symlinks it
into Homebrew's prefix.

**Sources to copy from:**

| Piece | Copy from |
|-------|-----------|
| Bundle layout, `app`+`binary` cask, sign-inner-then-seal, staple-then-rezip | `~/src/agent-scoreboard` (`app/Makefile`, `packaging/scoreboard-cask.rb.tmpl`, tap `Casks/scoreboard.rb`) |
| `SIGN_ID ?= -` / `HARDENED`, `_devid_detect`, `setup-notary`, `setup-secrets`, dmg target | `~/src/newt/Makefile` |
| `build.yml`, `release.yml` (keychain import, notarytool credentials, CHANGELOG notes, `brew` job) | `~/src/newt/.github/workflows` |
| `SMAppService.mainApp` login item with a bootstrap flag | `~/src/agent-scoreboard/app/Sources/Scoreboard/LoginItemController.swift` |

**Not copied:** scoreboard ships its CLI as a stdlib-only zipapp. pr-mon needs
httpx and Textual, so the CLI is built with PyInstaller instead.

## Decisions

1. **One cask, no formula.** `app "PrMon.app"` + `binary
   "#{appdir}/PrMon.app/Contents/Resources/bin/pr-mon"`. Homebrew puts `pr-mon`
   on PATH and removes it on uninstall. Not `~/.local/bin`: that needs
   `writable_paths` in a postflight and Homebrew wouldn't track the symlink.
2. **PyInstaller (onedir)** builds the CLI. onedir, not onefile: no extraction
   on every launch, and every nested binary can be signed normally.
3. **The app never shells out to the login shell.** It runs the bundled binary
   by absolute path, so "Start Backend", Restart and Stop work with nothing
   installed. `Launcher` keeps a PATH fallback for source checkouts.
4. **Autostart via `SMAppService.agent`** with a LaunchAgent plist inside the
   bundle pointing at the bundled binary. `pr-mon autostart` (launchctl) stays
   for source installs; the app stops using it.
5. **The version is the tag.** CI checks the tag matches `pyproject.toml`, and
   stamps `MARKETING_VERSION`/`CURRENT_PROJECT_VERSION` into the app.
6. **No Sparkle for now.** Updates come from `brew upgrade`, as in scoreboard;
   so no `auto_updates`, no appcast, no gh-pages. Tracker's wiring is the
   template if that changes.
7. **The TUI ships in the same binary** (`pr-mon` with no arguments), so the
   cask covers terminal users too.

## Layout

```
PrMon.app/Contents/
  MacOS/PrMon                      # SwiftUI app
  Resources/bin/pr-mon             # PyInstaller onedir: launcher + _internal/
  Library/LaunchAgents/com.acheris-labs.pr-mon.backend.plist
```

## Tasks

### Task 1: Build the standalone CLI

**Files:** `packaging/pr-mon.spec`, `Makefile`, `tests/test_cli.py`

- [ ] PyInstaller spec: entry `pr_mon/__main__.py`, onedir, `--collect-data
      textual` (its `.tcss` files) plus `rich`; console binary named `pr-mon`.
- [ ] `make cli` builds `build/cli/pr-mon/` and prints the path; PyInstaller is
      a dev dependency (`uv add --dev pyinstaller`).
- [ ] Smoke test the built binary in the same target: `pr-mon --version` equals
      the package version, `pr-mon status` exits 1 with "backend stopped", and
      `pr-mon daemon` starts and answers `hello` over the socket.
- [ ] Check the size and note it in the plan's results (expect 40–70MB).

### Task 2: Bundle the CLI in the app

**Files:** `macos/PrMon/project.yml`, `Makefile`, `macos/PrMon/PrMon/…`

- [ ] `make app` copies `build/cli/pr-mon/` into
      `Contents/Resources/bin/` after xcodebuild.
- [ ] `PrMonKit.Launcher`: resolve the CLI as bundled binary → `pr-mon` on PATH
      → error naming both; run it directly (no `-l -c`) so the app doesn't
      depend on the login shell. Tests for each branch.
- [ ] `Launcher.commandAvailable()` reports true when bundled.
- [ ] Verify live: with `pr-mon` off PATH, Start/Restart/Stop backend work from
      the Backend settings tab.

### Task 3: Autostart through SMAppService

**Files:** `macos/PrMon/…/LaunchAgent.plist`, `SettingsView.swift`, `project.yml`

- [ ] Bundle `Contents/Library/LaunchAgents/com.acheris-labs.pr-mon.backend.plist`
      running `Contents/Resources/bin/pr-mon daemon`, `KeepAlive` on failure only.
- [ ] `BackendLoginItem` wrapping `SMAppService.agent(plistName:)`:
      status, register, unregister; General tab's "Start the backend at login"
      uses it when bundled, else the CLI path as today.
- [ ] "Open pr-mon at login" keeps `SMAppService.mainApp` (already done).
- [ ] Live check: enable, log out/in or `launchctl print gui/$UID/…`, confirm
      the backend runs and the app connects. Ask before enabling for real.

### Task 4: Sign, notarize, package

**Files:** `Makefile`, `packaging/`, `DISTRIBUTING.md`

- [ ] Copy newt's `SIGN_ID ?= -` / `HARDENED`, `_devid_detect`, `setup-notary`,
      `setup-secrets` (add pr-mon to the `REPOS` list).
- [ ] `make sign`: sign every Mach-O under `Resources/bin/` (the PyInstaller
      launcher, `_internal/**/*.so`, `*.dylib`, `Python`), then the app bundle.
      Inside-out, never `--deep`.
- [ ] Entitlements: start with none; if Python fails to launch hardened, add
      `com.apple.security.cs.disable-library-validation` and record why.
- [ ] `make zip`: `ditto` → `notarytool submit --wait` → `stapler staple` the
      bundle → re-zip. `make dmg` after that, signed, notarized and stapled.
- [ ] `make verify`: `codesign --verify --strict`, `spctl -a -vv`, and
      `stapler validate` on both artifacts.

### Task 5: Release workflow

**Files:** `.github/workflows/build.yml`, `release.yml`, `CHANGELOG.md`

- [ ] `build.yml` (macos-15, push/PR): `make lint test`, `make swift-test`,
      `make cli`, ad-hoc `make app`, upload the app as an artifact.
- [ ] `release.yml` on `v*` tags: refuse if the tag and `pyproject.toml`
      disagree; import the certificate into a temporary keychain (newt's step
      verbatim, including `set-key-partition-list`); store notarytool
      credentials; `make dmg zip SIGN_ID=… NOTARY_PROFILE=…`; write a `.sha256`
      for the zip; create the release with notes from `CHANGELOG.md`; always
      delete the keychain.
- [ ] `brew` job (ubuntu): render `packaging/pr-mon-cask.rb.tmpl` with
      `envsubst`, push to `acheris-labs/homebrew-tools` with
      `HOMEBREW_TAP_SSH_KEY`; warn, don't fail, when the key is missing.
- [ ] Start `CHANGELOG.md` (Keep a Changelog) with 0.2.0 as the first release.

### Task 6: The cask

**Files:** `packaging/pr-mon-cask.rb.tmpl`

- [ ] `version`/`sha256` from the template variables; `url` the release zip;
      `depends_on macos: :sonoma`; `app "PrMon.app"`;
      `binary "#{appdir}/PrMon.app/Contents/Resources/bin/pr-mon"`.
- [ ] `uninstall quit: "com.acheris-labs.pr-mon.app"`, and stop the backend
      first (`pr-mon stop`, `must_succeed: false`) so no daemon outlives it.
- [ ] `zap trash:` config (`~/.config/pr-mon`), state (`~/.local/state/pr-mon`),
      preferences and saved state.
- [ ] No `postflight` opening the app: pr-mon has a Dock icon, so Homebrew's
      normal behaviour is fine.

### Task 7: Docs and verification

- [ ] README: install via cask first, `uv tool install` second (development);
      note that the CLI comes from the app bundle.
- [ ] `DISTRIBUTING.md`: credentials, `make setup-notary`, tagging, what CI does.
- [ ] CLAUDE.md: version bump rule also covers tagging a release.
- [ ] End-to-end on this machine: `make dmg`, install the built cask from a
      local file, confirm `pr-mon` on PATH, the app connects, autostart
      registers, then `brew uninstall --zap` leaves nothing behind.

## Risks

| Risk | Handling |
|------|----------|
| PyInstaller misses Textual's `.tcss` data files | Task 1 runs the TUI binary before anything else depends on it |
| Notarization rejects an unsigned nested `.so` | Sign everything under `Resources/bin/` by glob, and `spctl -a -vv` before releasing |
| Hardened runtime blocks Python's `dlopen` | Add `disable-library-validation` only if it actually fails |
| Bundle size 60–90MB | Accepted; onedir keeps launches fast |
| Two backends (bundled and `uv tool install`) on a dev machine | The protocol/version check already refuses a mismatched backend |
| macOS runner minutes on a private repo (10×) | Signing job runs only on tags |
