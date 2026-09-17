.PHONY: build run install test lint fmt fixtures clean \
	swift-test xcode app app-install

VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.Version=$(VERSION)
BIN := build/pr-mon
APP_BUILD := macos/build
APP := $(APP_BUILD)/Build/Products/Release/PrMon.app

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

swift-test:
	cd macos/PrMonKit && swift test

xcode:
	cd macos/PrMon && xcodegen generate

app: build xcode
	xcodebuild -project macos/PrMon/PrMon.xcodeproj -scheme PrMon -configuration Release \
		-destination generic/platform=macOS -derivedDataPath $(APP_BUILD) -quiet build
	@echo "built $(APP)"

app-install: app
	mkdir -p ~/Applications
	rm -rf ~/Applications/PrMon.app
	cp -R $(APP) ~/Applications/
	@echo "installed ~/Applications/PrMon.app"
