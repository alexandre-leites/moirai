# Contributing to Moirai

Thanks for your interest in contributing. This document covers how to set up
a development environment, run the checks the project actually uses, and what
a pull request needs before it can merge.

## Project layout

Moirai is a monorepo with four independently buildable services plus shared
contracts:

- `orchestrator/` — Go gRPC control plane (PostgreSQL, scheduling, workflow
  state machine).
- `api/` — Go HTTP gateway (REST + SSE, proxies to the orchestrator over
  gRPC).
- `runner/` — Go execution agent.
- `web/` — React + TypeScript dashboard.
- `proto/` — shared gRPC contracts (generated code lives in `gen/go`).

See [`PROJECT.md`](PROJECT.md) for the architecture and product scope, and
[`README.md`](README.md) for how to run the stack with Docker Compose.

## Development setup

You need:

- Go (version pinned in each module's `go.mod`).
- Node.js/npm for `web/`.
- Docker and Docker Compose, for running the full stack or the Postgres
  integration suite.

Bring up the stack for manual testing:

```bash
docker compose up -d
```

or build the images from your checkout instead of pulling published ones:

```bash
docker compose -f compose.yaml -f compose.build.yaml up --build -d
```

## Running checks

`make help` lists every target. The ones you will use most:

```bash
make test              # orchestrator, runner, API, and web checks
make test-orchestrator  # Go orchestrator tests (no database; Postgres suites excluded)
make test-postgres-integration  # the orchestrator's PostgreSQL suites (needs LOOP_TEST_DATABASE_URL)
make test-runner       # Go runner tests
make test-api          # Go API tests
make test-web          # web typecheck, lint, and tests
make lint              # gofmt check across tracked Go files
make lint-go           # golangci-lint across the Go modules
make typecheck         # go vet (orchestrator)
make validate          # test, format, vet, Compose, and proto checks — the full gate
```

`make test-orchestrator` deliberately excludes the orchestrator's PostgreSQL
integration suites — most of the workflow state machine — because they need a
real database. It prints which suites it left out; run them with:

```bash
docker run -d --name moirai-test-postgres -p 5432:5432 \
  -e POSTGRES_DB=loop_test -e POSTGRES_USER=loop \
  -e POSTGRES_PASSWORD=loop-test-password postgres:16-alpine

LOOP_TEST_DATABASE_URL=postgresql://loop:loop-test-password@localhost:5432/loop_test \
  make test-postgres-integration
```

Run only the checks relevant to what you changed while iterating; run
`make validate` before opening a pull request that touches shared contracts,
a database migration, or several services at once. See
[orchestrator local checks](orchestrator/README.md#local-checks) for more
detail on the orchestrator's test suites.

### Database access

Database access in the orchestrator goes through
[sqlc](https://sqlc.dev)-generated code (`orchestrator/internal/db`,
generated from `orchestrator/internal/db/queries/*.sql` and
`orchestrator/migrations/`). Hand-written SQL string literals in Go are not
accepted for new or changed code. A new or changed query means editing a
`.sql` file under `orchestrator/internal/db/queries/` and running:

```bash
make sqlc-generate
```

`make sqlc-check` (wired into CI and `make validate`) fails the build when
the checked-in generated code is stale. See
[`orchestrator/README.md`](orchestrator/README.md) for the full workflow.

### Protocol Buffers

Shared contracts live in `proto/`. After changing a `.proto` file, run:

```bash
make proto-generate
```

`make proto-check` (also wired into CI) fails the build when `gen/go` has
drifted from the proto sources.

## Code standards

- Keep GitHub-specific code inside GitHub adapters, and OpenCode-specific code
  inside the OpenCode backend — portability through interfaces is a core
  design principle (see [`PROJECT.md`](PROJECT.md)).
- Keep database access inside the orchestrator; the API and orchestrator are
  separate services communicating over internal gRPC.
- Keep REST, gRPC, persistence, and domain models separated.
- Use database transactions for project locks, job offers, and leases; use
  lease generations to reject stale runner events.
- Make external side effects (labels, branches, PRs, merge, close) idempotent.
- Add relevant tests with each change; avoid speculative abstractions and
  unrelated refactors.
- Go code is formatted with `gofmt` (`make lint`) and linted with
  `golangci-lint` (`make lint-go`), using the repo-root `.golangci.yml`
  configuration.
- Web code is linted with ESLint and type-checked with `tsc` (`make
  test-web`).

See [`AGENTS.md`](AGENTS.md) for the fuller set of engineering rules this
project (including its autonomous agents) follows.

## Pull request process

1. Fork or branch, make your change, and add or update tests for it.
2. Run the checks relevant to what you changed (see above); run `make
   validate` for changes touching shared contracts, migrations, or multiple
   services.
3. Open a pull request describing what changed and why. Reference the issue
   it closes (e.g. `Closes #123`) when there is one.
4. CI runs the same checks (`.github/workflows/ci.yml`); a green run is
   required before merge.
5. Address review feedback. Once approved and green, the PR can be merged.

## Reporting bugs and requesting features

Use the issue templates under `.github/ISSUE_TEMPLATE/`. For security
vulnerabilities, do not open a public issue — see
[`SECURITY.md`](SECURITY.md).
