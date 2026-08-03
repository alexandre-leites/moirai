BUF_IMAGE ?= bufbuild/buf:1.50.0
SQLC_VERSION ?= v1.29.0
GO ?= go

.PHONY: help test lint typecheck validate compose compose-overlays \
        proto-lint proto-generate proto-check sqlc-generate sqlc-check test-release-tags compose-tls-stack \
        test-orchestrator test-postgres-integration test-runner test-api test-web \
        build-orchestrator \
        build-runner build-api build-web build-images

help:
	@printf '%s\n' 'Targets:' '  make test              Run orchestrator, runner, API, and web checks.' '  make test-orchestrator Run Go orchestrator tests.' '  make lint              Verify Go formatting.' '  make typecheck         Run Go vet.' '  make validate          Run test, format, vet, Compose, and proto checks.'

test: test-orchestrator test-runner test-api test-web

test-orchestrator:
	cd orchestrator && $(GO) test -race ./...

# The orchestrator's correctness is mostly its SQL -- mutual exclusion is a
# primary key, fencing is a WHERE clause -- so these run against a real
# PostgreSQL. The guard is deliberate: a silently skipped suite is worse than a
# missing one.
test-postgres-integration:
	@test -n "$(LOOP_TEST_DATABASE_URL)" || { echo "test-postgres-integration: LOOP_TEST_DATABASE_URL is not set; e.g. LOOP_TEST_DATABASE_URL=postgresql://loop:loop-test-password@localhost:5432/loop_test make test-postgres-integration" >&2; exit 1; }
	cd orchestrator && $(GO) test -tags integration -race -count=1 ./internal/server/

test-runner:
	cd runner && $(GO) test -race ./...

test-api:
	cd api && $(GO) test ./...

test-web:
	cd web && npm ci && npm run typecheck && npm run lint && npm test

lint:
	test -z "$$(gofmt -l $$(git ls-files --cached --others --exclude-standard -- '*.go'))"

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

# Database access goes through sqlc-generated code (see AGENTS.md §12 and
# orchestrator/README.md): queries live in orchestrator/internal/db/queries as
# .sql files, and this is how the Go bindings in orchestrator/internal/db are
# produced from them.
#
# Installed as a native binary (not run via Docker): a Docker bind mount only
# works when the path visible to the invoking process is also the path the
# Docker daemon can see, which fails whenever the two are separated by a
# container boundary of their own -- exactly the layout some self-hosted
# runners use. `go install` sidesteps the mismatch entirely and produces the
# same pinned output.
sqlc-generate:
	GOBIN="$$($(GO) env GOPATH)/bin" $(GO) install github.com/sqlc-dev/sqlc/cmd/sqlc@$(SQLC_VERSION)
	cd orchestrator && "$$($(GO) env GOPATH)/bin/sqlc" generate

sqlc-check: sqlc-generate
	git diff --exit-code -- orchestrator/internal/db

validate: test-orchestrator lint typecheck compose compose-overlays test-release-tags proto-check sqlc-check
