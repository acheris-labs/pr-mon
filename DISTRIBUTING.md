# Distributing pr-mon

One artifact ships everything: `PrMon.app` carries the `pr-mon` binary (the
dashboard, the backend and the CLI) in `Contents/Resources/bin/`, and the
Homebrew cask symlinks that binary onto PATH.

```sh
brew install --cask acheris-labs/tools/pr-mon
```

## What a release produces

| Artifact | For |
|----------|-----|
| `PrMon-<version>.zip` | What the cask downloads; notarized and stapled |
| `PrMon-<version>.dmg` | For people downloading from the releases page |
| `PrMon-<version>.zip.sha256` | Lets the cask job read the hash without re-downloading |

## Releasing

```sh
git tag v0.2.0 && git push origin v0.2.0
```

That is the whole process. The `release` workflow then:

1. Runs the tests (Go and Swift).
2. Imports the Developer ID certificate into a temporary keychain.
3. Builds the universal binary, embeds it in the app, signs inside-out,
   notarizes, staples, and packages the zip and DMG.
4. Creates the GitHub release, with notes from this version's `CHANGELOG.md`
   section when there is one.
5. Renders `packaging/pr-mon-cask.rb.tmpl` and pushes it to
   `acheris-labs/homebrew-tools`.

`workflow_dispatch` does the same from a version string, tagging as it goes.

## Credentials (once)

The signing secrets are org-level in `acheris-labs`, shared with newt and
tracker, but each is scoped to named repositories — so pr-mon has to be added
to that scope:

```sh
make setup-secrets P12=devid.p12 APPLE_ID=you@example.com \
                   ORG=acheris-labs REPOS=tracker,newt,agent-scoreboard,pr-mon
```

| Secret | What it is |
|--------|------------|
| `DEVID_CERT_P12` | Developer ID Application certificate, exported as .p12, base64 |
| `DEVID_CERT_PASSWORD` | The password used for that export |
| `DEVID_IDENTITY` | The identity string, e.g. `Developer ID Application: … (TEAMID)` |
| `APPLE_ID`, `APPLE_TEAM_ID`, `APPLE_APP_PASSWORD` | For notarization |
| `HOMEBREW_TAP_SSH_KEY` | Deploy key with write access to the tap, and nothing else |

For notarizing locally, store a keychain profile once:

```sh
make setup-notary APPLE_ID=you@example.com   # app-specific password on the clipboard
```

## Building by hand

```sh
make app                       # ad-hoc signed, for local use
make release SIGN_ID="Developer ID Application: Your Name (TEAMID)"
make verify                    # signature, Gatekeeper, stapled ticket
```

## Notes

- **Sign inside-out, never `--deep`.** The nested `pr-mon` binary is signed
  first, then the bundle seals it. `--deep` would re-sign the inner binary
  without the hardened runtime and notarization would reject it.
- **Staple the bundle, then re-zip.** `stapler` works on bundles, not archives,
  so the zip is built twice: once to submit, once with the ticket inside.
- **Notarization failures** come back as a log you fetch with
  `xcrun notarytool log <id> --keychain-profile pr-mon-notary`. The usual cause
  is an unsigned nested binary.
- **macOS runners cost 10× the minutes** on private repos, so the signing job
  only runs on tags; `build.yml` does the ordinary checks.
