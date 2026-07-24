BUF_IMAGE ?= bufbuild/buf:1.50.0

.PHONY: test lint typecheck validate compose \
        proto-lint proto-generate proto-check \
        test-orchestrator test-runner test-api test-web \
        build-runner build-api build-web

test: test-orchestrator

test-orchestrator:
	PYTHONPATH=orchestrator/src python3 -m unittest discover -s orchestrator/tests

test-runner:
	cd runner && go test -race ./...

test-api:
	cd api && go test ./...

test-web:
	cd web && npm run lint

lint:
	python3 -m ruff check orchestrator/src orchestrator/tests

typecheck:
	python3 -m mypy orchestrator/src

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
