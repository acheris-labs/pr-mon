.PHONY: install run test lint fmt clean tool-install fixtures

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
	rm -rf .venv dist build .ruff_cache
	find . -name __pycache__ -type d -prune -exec rm -rf {} +

tool-install:
	uv tool install --reinstall .
