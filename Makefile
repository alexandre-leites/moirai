BUF_IMAGE ?= bufbuild/buf:1.50.0
VENV ?= .venv
MYPY_CACHE ?= /tmp/moirai-mypy-cache

.PHONY: help test lint typecheck validate compose compose-overlays dev-install \
        proto-lint proto-generate proto-check test-release-tags compose-tls-stack \
        test-orchestrator test-postgres-integration test-runner test-api test-web \
         build-runner build-api build-web build-images

help:
	@printf '%s\n' 'Targets:' '  make test              Run orchestrator, runner, API, and web checks.' '  make test-orchestrator Run orchestrator tests.' '  make test-runner       Run runner tests with the race detector.' '  make test-api          Run API tests.' '  make test-web          Install web dependencies and run typecheck, lint, and unit tests.' '  make lint              Run orchestrator lint.' '  make typecheck         Run orchestrator type checks.' '  make validate          Run test, lint, typecheck, Compose, and proto checks.' '  make compose           Validate the Compose configuration.' '  make compose-overlays  Validate the build and secrets Compose overlays.' '  make test-release-tags Check the release trigger to image tag mapping.' '  make proto-check       Lint, generate, and verify protobuf outputs.'

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
	cd web && npm ci && npm run typecheck && npm run lint && npm test

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

build-images:
	MOIRAI_BUILD_VERSION="$$(git rev-parse --short=12 HEAD)" docker compose -f compose.yaml -f compose.build.yaml build

compose:
	docker compose config

# Every supported Compose combination has to render. The build overlay must
# actually add build sections, and the secrets overlay must leave no secret in
# the environment -- both are silent failures otherwise.
compose-overlays:
	docker compose -f compose.yaml -f compose.build.yaml config --quiet
	docker compose -f compose.yaml -f compose.build.yaml config | grep -q '^ *build:'
	docker compose -f compose.yaml -f compose.secrets.yaml config --quiet
	! docker compose -f compose.yaml -f compose.secrets.yaml config | grep -qE '^ *(LOOP_DATABASE_URL|LOOP_INITIAL_ADMIN_PASSWORD|LOOP_RUNNER_REGISTRATION_TOKEN|LOOP_SECRET_KEY):'
	docker compose -f compose.yaml -f compose.tls.yaml config --quiet
	# Both ends, or the runner dials plaintext against a TLS port and the whole
	# per-project credential path is off with nothing to show for it.
	docker compose -f compose.yaml -f compose.tls.yaml config | grep -q 'LOOP_GRPC_TLS_CERT_FILE'
	test "$$(docker compose -f compose.yaml -f compose.tls.yaml config | grep -c 'LOOP_ORCHESTRATOR_TLS: "true"')" = 2
	docker compose -f compose.yaml -f compose.tls.yaml -f compose.secrets.yaml config --quiet
	sh scripts/render-tls-stack.sh --check
	docker compose -f compose.tls-stack.yaml config --quiet

# Portainer takes one file, so the TLS stack also exists as a single rendered
# document. Generated, never hand-edited -- two full stacks drift, and the way
# you find out is a deployment behaving differently from the one you tested.
compose-tls-stack:
	sh scripts/render-tls-stack.sh

# Executable specification of the release trigger -> image tag mapping.
# release.yml runs this script directly before deriving a version, so a release
# is gated on it either way; this target is what makes `make validate` -- and a
# developer checking before they cut one -- run the same specification.
test-release-tags:
	sh scripts/release-version_test.sh

proto-lint:
	docker run --rm -v "$(CURDIR):/workspace" -w /workspace $(BUF_IMAGE) lint

proto-generate:
	docker run --rm -v "$(CURDIR):/workspace" -w /workspace $(BUF_IMAGE) generate

proto-check: proto-lint proto-generate
	git diff --exit-code -- gen/go orchestrator/src/moirai/protocols

validate: test-orchestrator lint typecheck compose compose-overlays test-release-tags proto-check
