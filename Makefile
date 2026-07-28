BUF_IMAGE ?= bufbuild/buf:1.50.0
VENV ?= .venv
MYPY_CACHE ?= /tmp/moirai-mypy-cache

.PHONY: test lint typecheck validate compose dev-install \
        proto-lint proto-generate proto-check \
        test-orchestrator test-runner test-api test-web \
        build-runner build-api build-web

test: test-orchestrator

# Bootstraps a local virtualenv with the orchestrator installed in editable
# mode plus dev tools (ruff, mypy, pytest). Required on a clean checkout
# before `make lint` / `make typecheck` will find their tools.
dev-install:
	test -d $(VENV) || python3 -m venv $(VENV)
	$(VENV)/bin/pip install --upgrade pip
	$(VENV)/bin/pip install -e "orchestrator[dev]"

test-orchestrator: dev-install
	PYTHONPATH=orchestrator/src $(VENV)/bin/python3 -m unittest discover -s orchestrator/tests

test-runner:
	cd runner && go test -race ./...

test-api:
	cd api && go test ./...

test-web:
	cd web && npm run typecheck && npm run lint

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
