.PHONY: install run test lint fmt clean tool-install fixtures \
	swift-test xcode app app-install go-test go-lint go-build

APP_BUILD := macos/build
APP := $(APP_BUILD)/Build/Products/Release/PrMon.app

install:
	uv sync

run:
	uv run pr-mon

test:
	uv run python -m unittest discover -s tests -t . -v

fixtures:
	uv run python -m tests.protocol_fixtures

lint:
	uv run ruff check .
	uv run ruff format --check .

fmt:
	uv run ruff format .
	uv run ruff check --fix .

clean:
	rm -rf .venv dist build .ruff_cache $(APP_BUILD) macos/PrMonKit/.build
	find . -name __pycache__ -type d -prune -exec rm -rf {} +

tool-install:
	uv tool install --reinstall .

# ----- macOS menu bar app (macos/) -----

swift-test:
	cd macos/PrMonKit && swift test

xcode:
	cd macos/PrMon && xcodegen generate

app: xcode
	xcodebuild -project macos/PrMon/PrMon.xcodeproj -scheme PrMon -configuration Release \
		-destination generic/platform=macOS -derivedDataPath $(APP_BUILD) -quiet build
	@echo "built $(APP)"

app-install: app
	mkdir -p ~/Applications
	rm -rf ~/Applications/PrMon.app
	cp -R $(APP) ~/Applications/
	@echo "installed ~/Applications/PrMon.app"

# ----- Go rewrite (in progress: cmd/, internal/) -----

go-test:
	go test -race ./...

go-lint:
	@test -z "$$(gofmt -l cmd internal)" || { gofmt -l cmd internal; echo "run gofmt -w"; exit 1; }
	go vet ./...

go-build:
	go build -o build/pr-mon ./cmd/pr-mon
