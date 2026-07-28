BUF_IMAGE ?= bufbuild/buf:1.50.0
VENV ?= .venv
MYPY_CACHE ?= /tmp/moirai-mypy-cache

.PHONY: help test lint typecheck validate compose dev-install \
        proto-lint proto-generate proto-check \
        test-orchestrator test-postgres-integration test-runner test-api test-web \
        build-runner build-api build-web

help:
	@printf '%s\n' 'Targets:' '  make test              Run orchestrator, runner, API, and web checks.' '  make test-orchestrator Run orchestrator tests.' '  make test-runner       Run runner tests with the race detector.' '  make test-api          Run API tests.' '  make test-web          Install web dependencies and run typecheck/lint.' '  make lint              Run orchestrator lint.' '  make typecheck         Run orchestrator type checks.' '  make validate          Run test, lint, typecheck, Compose, and proto checks.' '  make compose           Validate the Compose configuration.' '  make proto-check       Lint, generate, and verify protobuf outputs.'

test: test-orchestrator test-runner test-api test-web

# Bootstraps a local virtualenv with the orchestrator installed in editable
# mode plus dev tools (ruff, mypy, pytest). Required on a clean checkout
# before `make lint` / `make typecheck` will find their tools.
dev-install:
	test -d $(VENV) || python3 -m venv $(VENV)
	$(VENV)/bin/pip install --upgrade pip
	$(VENV)/bin/pip install -e "orchestrator[dev]"

test-orchestrator: dev-install
	PYTHONPATH=orchestrator/src $(VENV)/bin/python3 -m unittest discover -s orchestrator/tests

test-postgres-integration: dev-install
	test -n "$(LOOP_TEST_DATABASE_URL)"
	PYTHONPATH=orchestrator/src $(VENV)/bin/python3 -m unittest discover -s orchestrator/tests -p test_postgres_integration.py -v

test-runner:
	cd runner && go test -race ./...

test-api:
	cd api && go test ./...

test-web:
	cd web && npm ci && npm run typecheck && npm run lint

lint: dev-install
	$(VENV)/bin/python3 -m ruff check orchestrator/src orchestrator/tests

typecheck: dev-install
	rm -rf "$(MYPY_CACHE)"
	$(VENV)/bin/python3 -m mypy --cache-dir="$(MYPY_CACHE)" orchestrator/src

build-runner:
	cd runner && go build ./cmd/runner

build-api:
	cd api && go build ./cmd/api

build-web:
	cd web && npm ci && npm run build

compose:
	docker compose config

proto-lint:
	docker run --rm -v "$(CURDIR):/workspace" -w /workspace $(BUF_IMAGE) lint

proto-generate:
	docker run --rm -v "$(CURDIR):/workspace" -w /workspace $(BUF_IMAGE) generate

proto-check: proto-lint proto-generate
	git diff --exit-code -- gen/go orchestrator/src/moirai/protocols

validate: test-orchestrator lint typecheck compose proto-check
