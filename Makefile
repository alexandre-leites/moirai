BUF_IMAGE ?= bufbuild/buf:1.50.0
SQLC_VERSION ?= v1.29.0
GOLANGCI_LINT_VERSION ?= v2.12.2
# `docker compose config` output is not byte-stable across compose versions, so
# compose.tls-stack.yaml is rendered with (and checked against) this exact one.
# Keep in sync with the version pinned in .github/workflows/ci.yml.
COMPOSE_VERSION ?= v2.38.2
GO ?= go

.PHONY: help test lint lint-go typecheck validate compose compose-overlays \
        proto-lint proto-generate proto-check sqlc-generate sqlc-check test-release-tags compose-tls-stack \
        test-orchestrator test-postgres-integration test-integration-notice test-runner test-api test-web \
        coverage-go coverage-web coverage \
        build-orchestrator \
        build-runner build-api build-web build-images

help:
	@printf '%s\n' 'Targets:' '  make test              Run orchestrator, runner, API, and web checks.' '  make test-orchestrator Run Go orchestrator tests (no database; PostgreSQL suites excluded).' '  make test-postgres-integration  Run the PostgreSQL suites. Needs LOOP_TEST_DATABASE_URL.' '  make lint              Verify Go formatting.' '  make lint-go           Run golangci-lint across the Go modules.' '  make typecheck         Run Go vet.' '  make validate          Run test, format, vet, Compose, and proto checks.'

test: test-orchestrator test-runner test-api test-web

# The notice is not decoration. This target does not build the `integration`
# suites at all, and `go test` in package-list mode prints nothing for a
# package that passes, so without it the omission of 100+ state-machine tests
# is invisible and the run just looks green (issue #363).
test-orchestrator:
	cd orchestrator && $(GO) test -race ./...
	@GO='$(GO)' sh scripts/integration-suite-notice.sh

test-integration-notice:
	sh scripts/integration-suite-notice_test.sh

# The orchestrator's correctness is mostly its SQL -- mutual exclusion is a
# primary key, fencing is a WHERE clause -- so these run against a real
# PostgreSQL. The guard is deliberate: a silently skipped suite is worse than a
# missing one. It says so out loud because `test -n` on its own fails with
# nothing but a make error code, which reads like a broken build rather than a
# missing database.
test-postgres-integration:
	@test -n "$(LOOP_TEST_DATABASE_URL)" || { \
		printf '%s\n' 'LOOP_TEST_DATABASE_URL is not set; these suites need a real PostgreSQL.' \
			'' \
			'    docker run -d --name moirai-test-postgres -p 5432:5432 \' \
			'      -e POSTGRES_DB=loop_test -e POSTGRES_USER=loop \' \
			'      -e POSTGRES_PASSWORD=loop-test-password postgres:16-alpine' \
			'' \
			'    LOOP_TEST_DATABASE_URL=postgresql://loop:loop-test-password@localhost:5432/loop_test \' \
			'      make test-postgres-integration' >&2; \
		exit 1; \
	}
	cd orchestrator && $(GO) test -tags integration -race -count=1 ./internal/server/

test-runner:
	cd runner && $(GO) test -race ./...

test-api:
	cd api && $(GO) test ./...

test-web:
	cd web && npm ci && npm run typecheck && npm run lint && npm test

# Per-module coverage floors, deliberately below what issue #372 found on the
# introducing PR so the gate that adds coverage reporting does not itself fail
# CI. Each is a documented floor to raise over time, not the 70% aspiration:
#   api          72.4% actual -> 65% floor. internal/orchestrator (TLS/CA
#                loading) is the weak package at 27%; api/http/handlers and
#                api/auth are already 85%+.
#   runner       80.6% actual -> 75% floor. Every package is already above
#                65%; this is the module closest to the 70% target.
#   orchestrator 16.0% actual (unit suite only) -> 12% floor. This module's
#                real coverage is understated here: internal/server (the
#                largest package by far) is exercised mainly by the
#                Postgres-backed suite behind `make test-postgres-integration`
#                (build tag `integration`), which this target does not run and
#                whose coverage is not merged into this profile. idgen,
#                secrethash and textutil sit at a genuine 0% and are the
#                actionable gap -- see issue #372 for the follow-up to add
#                unit tests for them and to merge the integration suite's
#                coverage profile in for an accurate internal/server number.
# Raise a floor only in the same PR that raises the module's real coverage.
coverage-go:
	sh scripts/go-coverage.sh api 65
	sh scripts/go-coverage.sh runner 75
	sh scripts/go-coverage.sh orchestrator 12

coverage-web:
	cd web && npm ci && npm run test -- --coverage

coverage: coverage-go coverage-web

lint:
	test -z "$$(gofmt -l $$(git ls-files --cached --others --exclude-standard -- '*.go'))"

# golangci-lint across the three Go modules. The config lives at the repo root
# (.golangci.yml) and is auto-discovered from each module directory. The
# ci-runner image bakes a pinned golangci-lint; on a bare runner it is
# installed on demand (see ci.yml lint job).
lint-go:
	@if command -v golangci-lint >/dev/null 2>&1; then \
		cd orchestrator && golangci-lint run ./...; \
		cd ../runner && golangci-lint run ./...; \
		cd ../api && golangci-lint run ./...; \
	else \
		GOBIN="$$($(GO) env GOPATH)/bin" $(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION); \
		cd orchestrator && "$$($(GO) env GOPATH)/bin/golangci-lint" run ./...; \
		cd ../runner && "$$($(GO) env GOPATH)/bin/golangci-lint" run ./...; \
		cd ../api && "$$($(GO) env GOPATH)/bin/golangci-lint" run ./...; \
	fi

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
	COMPOSE_VERSION="$(COMPOSE_VERSION)" sh scripts/render-tls-stack.sh --check
	docker compose -f compose.tls-stack.yaml config --quiet

compose-tls-stack:
	COMPOSE_VERSION="$(COMPOSE_VERSION)" sh scripts/render-tls-stack.sh

test-release-tags:
	sh scripts/release-version_test.sh

# Protobuf tooling. A native `buf` is preferred when it is already on PATH --
# the ci-runner image (infra/ci-runner/Dockerfile) bakes it in, and the native
# path is immune to bind-mount layout mismatches -- with the Docker fallback
# below for bare runners. Both produce the same pinned output: the Docker
# image and the baked binary are the same buf version.
proto-lint:
	@if command -v buf >/dev/null 2>&1; then \
		buf lint; \
	else \
		docker run --rm -v "$(CURDIR):/workspace" -w /workspace $(BUF_IMAGE) lint; \
	fi

proto-generate:
	@if command -v buf >/dev/null 2>&1; then \
		buf generate; \
	else \
		docker run --rm -v "$(CURDIR):/workspace" -w /workspace $(BUF_IMAGE) generate; \
	fi

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
# runners use. A native `sqlc` already on PATH (the ci-runner image bakes it
# in) is used directly; otherwise `go install` produces the same pinned
# output.
sqlc-generate:
	@if command -v sqlc >/dev/null 2>&1; then \
		cd orchestrator && sqlc generate; \
	else \
		GOBIN="$$($(GO) env GOPATH)/bin" $(GO) install github.com/sqlc-dev/sqlc/cmd/sqlc@$(SQLC_VERSION); \
		cd orchestrator && "$$($(GO) env GOPATH)/bin/sqlc" generate; \
	fi

sqlc-check: sqlc-generate
	git diff --exit-code -- orchestrator/internal/db

validate: test-orchestrator test-integration-notice lint lint-go typecheck compose compose-overlays test-release-tags proto-check sqlc-check
