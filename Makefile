.PHONY: build run install test lint fmt fixtures clean universal \
	swift-test xcode app app-install icon sign zip dmg release verify \
	setup-notary setup-secrets

DESIGN ?= dots
# A monotonic build number, so a newer build always sorts above an older one.
BUILD_NUMBER := $(shell date +%s)
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.Version=$(VERSION)
BIN := build/pr-mon
APP_BUILD := macos/build
APP := $(APP_BUILD)/Build/Products/Release/PrMon.app
CONTENTS := $(APP)/Contents
ZIP := $(APP_BUILD)/PrMon.zip
DMG := $(APP_BUILD)/PrMon.dmg

# --- Code signing -----------------------------------------------------------
# SIGN_ID defaults to ad-hoc ("-"): fine locally, but cannot be notarized. For
# distributable builds pass a Developer ID (see DISTRIBUTING.md):
#   make release SIGN_ID="Developer ID Application: Your Name (TEAMID)"
SIGN_ID        ?= -
NOTARY_PROFILE ?= pr-mon-notary

# Hardened runtime and a secure timestamp are required for notarization, and
# rejected by ad-hoc signing, so only add them with a real identity.
ifeq ($(SIGN_ID),-)
  HARDENED :=
else
  HARDENED := --options runtime --timestamp
endif

build:
	go build -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/pr-mon

run: build
	$(BIN)

install:
	go install -ldflags "$(LDFLAGS)" ./cmd/pr-mon

test:
	go test -race ./...

lint:
	@test -z "$$(gofmt -l cmd internal)" || { gofmt -l cmd internal; echo "run make fmt"; exit 1; }
	go vet ./...

fmt:
	gofmt -w cmd internal

fixtures:
	go run ./cmd/gen-fixtures

clean:
	rm -rf build $(APP_BUILD) macos/PrMonKit/.build

# ----- macOS app (macos/) -----

# Regenerate the app icon (designs: dots, merge, pull, graph, graph-green, prompt).
icon:
	swift tools/gen-icon.swift macos/PrMon/PrMon.iconset $(DESIGN)
	iconutil -c icns macos/PrMon/PrMon.iconset -o macos/PrMon/PrMon/PrMon.icns
	rm -rf macos/PrMon/PrMon.iconset
	@echo "wrote macos/PrMon/PrMon/PrMon.icns"


swift-test:
	cd macos/PrMonKit && swift test

xcode:
	cd macos/PrMon && xcodegen generate

# A universal CLI, so one app bundle serves both Apple silicon and Intel.
universal:
	GOOS=darwin GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o build/pr-mon-arm64 ./cmd/pr-mon
	GOOS=darwin GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o build/pr-mon-amd64 ./cmd/pr-mon
	lipo -create -output build/pr-mon-universal build/pr-mon-arm64 build/pr-mon-amd64

# The app carries the CLI: one install gives you the window, the dashboard and
# the backend, and the cask symlinks the binary onto PATH.
app: universal xcode
	xcodebuild -project macos/PrMon/PrMon.xcodeproj -scheme PrMon -configuration Release \
		-destination generic/platform=macOS -derivedDataPath $(APP_BUILD) -quiet \
		MARKETING_VERSION="$(VERSION)" CURRENT_PROJECT_VERSION="$(BUILD_NUMBER)" build
	mkdir -p $(CONTENTS)/Resources/bin
	cp build/pr-mon-universal $(CONTENTS)/Resources/bin/pr-mon
	# Adding a file invalidates Xcode's signature; re-sign ad-hoc so it still runs.
	codesign --force --sign - $(CONTENTS)/Resources/bin/pr-mon
	codesign --force --sign - $(APP)
	@echo "built $(APP)"

# Sign inside-out: the nested binary first, then the bundle seals it. Never
# --deep, which would re-sign the inner binary without these options.
sign: app
	@test "$(SIGN_ID)" != "-" || { echo "set SIGN_ID to a Developer ID"; exit 1; }
	codesign --force $(HARDENED) --sign "$(SIGN_ID)" $(CONTENTS)/Resources/bin/pr-mon
	codesign --force $(HARDENED) --sign "$(SIGN_ID)" $(APP)
	codesign --verify --strict --deep $(APP)

# Notarize through a zip, staple the ticket into the bundle, then re-zip so the
# archive the cask downloads carries the ticket (stapler works on bundles).
zip: sign
	rm -f $(ZIP)
	ditto -c -k --keepParent $(APP) $(ZIP)
	xcrun notarytool submit $(ZIP) --keychain-profile "$(NOTARY_PROFILE)" --wait
	xcrun stapler staple $(APP)
	xcrun stapler validate $(APP)
	rm -f $(ZIP)
	ditto -c -k --keepParent $(APP) $(ZIP)
	@echo "notarized $(ZIP)"

# A DMG for people who download from the releases page, notarized in its own right.
dmg: zip
	rm -f $(DMG)
	hdiutil create -volname "pr-mon" -srcfolder $(APP) -ov -format UDZO $(DMG)
	xcrun notarytool submit $(DMG) --keychain-profile "$(NOTARY_PROFILE)" --wait
	xcrun stapler staple $(DMG)
	xcrun stapler validate $(DMG)
	@echo "notarized $(DMG)"

# Everything a release publishes: the stapled zip (cask) and DMG (humans).
release: dmg

verify:
	codesign --verify --strict --verbose=2 $(APP)
	codesign -dv --verbose=4 $(CONTENTS)/Resources/bin/pr-mon 2>&1 | head -5
	spctl -a -vv $(APP)
	xcrun stapler validate $(APP)

LS := /System/Library/Frameworks/CoreServices.framework/Frameworks/LaunchServices.framework
LSREGISTER := $(LS)/Support/lsregister

app-install: app
	mkdir -p ~/Applications
	rm -rf ~/Applications/PrMon.app
	cp -R $(APP) ~/Applications/
	# Replacing the bundle leaves macOS showing the old icon until it re-reads it.
	$(LSREGISTER) -f ~/Applications/PrMon.app
	touch ~/Applications/PrMon.app
	@echo "installed ~/Applications/PrMon.app"

# --- Credential bootstrap ---------------------------------------------------
# Reads the Developer ID identity and Team ID from the keychain; inlined into
# the recipes that need it so it only runs when invoked.
define _devid_detect
DEVID_LINE=$$(security find-identity -v -p codesigning \
  | sed -nE 's/^[[:space:]]*[0-9]+\)[[:space:]]+[A-F0-9]+[[:space:]]+"(.*)".*$$/\1/p' \
  | head -1); \
test -n "$$DEVID_LINE" || { echo "no Developer ID Application identity in keychain"; exit 1; }; \
DEVID_TEAM=$$(echo "$$DEVID_LINE" | sed -nE 's/.*\(([A-Z0-9]+)\).*/\1/p')
endef

# Store the notarytool keychain profile. Run once per machine.
#   1. Copy the 19-char app-specific password from appleid.apple.com.
#   2. make setup-notary APPLE_ID=you@example.com
setup-notary:
	@test -n "$(APPLE_ID)" || { \
	  echo "usage: make setup-notary APPLE_ID=you@example.com"; \
	  echo "(copy your 19-char app-specific password to the clipboard first)"; \
	  exit 1; }
	@bash -ec '$(_devid_detect); \
	  PW=$$(pbpaste); \
	  test $${#PW} -eq 19 || { echo "clipboard is $${#PW} chars, expected 19"; exit 1; }; \
	  xcrun notarytool store-credentials $(NOTARY_PROFILE) \
	    --apple-id "$(APPLE_ID)" --team-id "$$DEVID_TEAM" --password "$$PW"; \
	  echo "notarytool profile $(NOTARY_PROFILE) is ready (team $$DEVID_TEAM)"'

# Upload the signing and notary credentials as org secrets scoped to this repo.
# newt and tracker already set these; this adds pr-mon to their scope.
#   make setup-secrets P12=devid.p12 APPLE_ID=you@example.com \
#                      ORG=acheris-labs REPOS=tracker,newt,pr-mon
setup-secrets:
	@test -f "$(P12)" || { echo "set P12=path/to/devid.p12"; exit 1; }
	@test -n "$(APPLE_ID)" || { echo "set APPLE_ID=you@example.com"; exit 1; }
	@test -n "$(ORG)"      || { echo "set ORG=acheris-labs"; exit 1; }
	@test -n "$(REPOS)"    || { echo "set REPOS=tracker,newt,pr-mon"; exit 1; }
	@bash -ec '$(_devid_detect); \
	  read -s -p ".p12 export password: " P12_PW; echo; \
	  APP_PW=$$(pbpaste); \
	  test $${#APP_PW} -eq 19 || { echo "clipboard not a 19-char app-specific password"; exit 1; }; \
	  echo "uploading to $(ORG) repos: $(REPOS)"; \
	  base64 -i "$(P12)"         | gh secret set DEVID_CERT_P12      --org $(ORG) --repos $(REPOS); \
	  printf "%s" "$$P12_PW"     | gh secret set DEVID_CERT_PASSWORD --org $(ORG) --repos $(REPOS); \
	  printf "%s" "$$DEVID_LINE" | gh secret set DEVID_IDENTITY      --org $(ORG) --repos $(REPOS); \
	  printf "%s" "$(APPLE_ID)"  | gh secret set APPLE_ID            --org $(ORG) --repos $(REPOS); \
	  printf "%s" "$$DEVID_TEAM" | gh secret set APPLE_TEAM_ID       --org $(ORG) --repos $(REPOS); \
	  printf "%s" "$$APP_PW"     | gh secret set APPLE_APP_PASSWORD  --org $(ORG) --repos $(REPOS); \
	  echo done'
