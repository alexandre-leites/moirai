BUF_IMAGE ?= bufbuild/buf:1.50.0
GO ?= go

.PHONY: help test lint typecheck validate compose compose-overlays dev-install \
        proto-lint proto-generate proto-check test-release-tags compose-tls-stack \
        test-orchestrator test-runner test-api test-web build-orchestrator \
        build-runner build-api build-web build-images

help:
	@printf '%s\n' 'Targets:' '  make test              Run orchestrator, runner, API, and web checks.' '  make test-orchestrator Run Go orchestrator tests.' '  make lint              Verify Go formatting.' '  make typecheck         Run Go vet.' '  make validate          Run test, format, vet, Compose, and proto checks.'

test: test-orchestrator test-runner test-api test-web

dev-install:
	@true

test-orchestrator:
	cd orchestrator && $(GO) test ./...

test-runner:
	cd runner && $(GO) test -race ./...

test-api:
	cd api && $(GO) test ./...

test-web:
	cd web && npm ci && npm run typecheck && npm run lint && npm test

lint:
	cd orchestrator && test -z "$$(gofmt -l $$(git ls-files --cached --others --exclude-standard -- '*.go'))"

typecheck:
	cd orchestrator && $(GO) vet ./...

build-orchestrator:
	cd orchestrator && $(GO) build ./cmd/orchestrator

build-runner:
	cd runner && $(GO) build ./cmd/runner

build-api:
	cd api && $(GO) build ./cmd/api

build-web:
	cd web && npm ci && npm run build

build-images:
	MOIRAI_BUILD_VERSION="$$(git rev-parse --short=12 HEAD)" docker compose -f compose.yaml -f compose.build.yaml build

compose:
	docker compose config

compose-overlays:
	docker compose -f compose.yaml -f compose.build.yaml config --quiet
	docker compose -f compose.yaml -f compose.build.yaml config | grep -q '^ *build:'
	docker compose -f compose.yaml -f compose.secrets.yaml config --quiet
	! docker compose -f compose.yaml -f compose.secrets.yaml config | grep -qE '^ *(LOOP_DATABASE_URL|LOOP_INITIAL_ADMIN_PASSWORD|LOOP_RUNNER_REGISTRATION_TOKEN|LOOP_SECRET_KEY):'
	docker compose -f compose.yaml -f compose.tls.yaml config --quiet
	docker compose -f compose.yaml -f compose.tls.yaml config | grep -q 'LOOP_GRPC_TLS_CERT_FILE'
	test "$$(docker compose -f compose.yaml -f compose.tls.yaml config | grep -c 'LOOP_ORCHESTRATOR_TLS: "true"')" = 2
	docker compose -f compose.yaml -f compose.tls.yaml -f compose.secrets.yaml config --quiet
	sh scripts/render-tls-stack.sh --check
	docker compose -f compose.tls-stack.yaml config --quiet

compose-tls-stack:
	sh scripts/render-tls-stack.sh

test-release-tags:
	sh scripts/release-version_test.sh

proto-lint:
	docker run --rm -v "$(CURDIR):/workspace" -w /workspace $(BUF_IMAGE) lint

proto-generate:
	docker run --rm -v "$(CURDIR):/workspace" -w /workspace $(BUF_IMAGE) generate

proto-check: proto-lint proto-generate
	git diff --exit-code -- gen/go

validate: test-orchestrator lint typecheck compose compose-overlays test-release-tags proto-check
