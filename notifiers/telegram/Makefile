.PHONY: build install test

# The Go tests and lint run from the repo root with the rest of pr-mon.

build:
	go build -o bin/pr-mon-telegram ./cmd/pr-mon-telegram

install:
	go build -o $(HOME)/.local/bin/pr-mon-telegram ./cmd/pr-mon-telegram

test:
	uv run python -m unittest discover -s tests
