.PHONY: install run test lint fmt clean tool-install

install:
	uv sync

run:
	uv run pr-mon

test:
	uv run python -m unittest discover -s tests -t . -v

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
