# Implementation Progress

## Current Status

- Overall status: #51 implementation complete; PR preparation
- Current phase: PostgreSQL persistence integration coverage
- Active implementation: #51 real PostgreSQL persistence tests
- Last updated: 2026-07-28
- Agent/session identifier: agent/postgres-integration-tests

## In Progress

- [ ] Monitor the #51 PR CI and merge when green.
  - Started: 2026-07-28
  - Relevant files: `.github/workflows/ci.yml`, `Makefile`, `orchestrator/tests/test_postgres_integration.py`
  - Current state: Local PostgreSQL integration validation is green; PR pending creation.
  - Remaining work: Push, create PR, resolve CI, merge.
  - Definition of done: Non-draft PR is mergeable and merged.
  - Targeted validation: GitHub Actions checks.

## Done

- [x] Add real PostgreSQL persistence integration coverage for #51
  - Completed: 2026-07-28
  - Relevant files: `.github/workflows/ci.yml`, `Makefile`, `orchestrator/tests/test_postgres_integration.py`
  - Behavior delivered: CI starts PostgreSQL 16, applies every migration, checks migration idempotency, and exercises `AsyncpgControlPlane` project, runner, issue, scheduling, offer, and lease persistence.
  - Validation performed: disposable PostgreSQL 16 integration suite; substantive Ruff checks; mypy with an isolated cache.
  - Commands executed: `make test-postgres-integration`; `.venv/bin/python3 -m ruff check --ignore EXE002 orchestrator/src orchestrator/tests`; `.venv/bin/python3 -m mypy --cache-dir /tmp/opencode/moirai-postgres-integration-mypy-cache orchestrator/src`.
  - Notes: Local `make lint` sees a mount-only executable-bit artifact on tracked 100644 files; the GitHub checkout uses tracked modes. The default mypy cache was locked by another worktree; an isolated cache passed.

- [x] Resolve core workflow correctness issues #37, #38, #39, #45, #46, #47, #58
  - Completed: 2026-07-28
  - Relevant files: `orchestrator/migrations/`, `orchestrator/src/moirai/{persistence,workflows,grpc}/`, `runner/internal/`, `api/Dockerfile`, `runner/Dockerfile`
  - Behavior delivered: Unique stable migrations; durable seeded graph state; terminal lock release; retryable transient outbox failures; dedicated pipeline execution and persistence; pending-check polling state; idempotent merge; reachable human approval state.
  - Validation performed: 267 orchestrator tests plus 6 subtests; targeted Ruff lint; runner taskpacket/dispatch tests.
  - Commands executed: `/tmp/opencode/moirai-workflow-venv/bin/pytest -q`; `/tmp/opencode/moirai-workflow-venv/bin/ruff check --ignore EXE002 src tests`; `go test ./internal/taskpacket ./internal/dispatch`.
  - Notes: `mypy` reports existing generated-protobuf and unrelated strict errors; Docker Compose build is blocked by host disk exhaustion after correcting stale Go 1.24 images to Go 1.25.

## Blocked

- [ ] Full Docker Compose fresh-volume boot and runner integration binary test
  - Reason: Host/containerd disk exhaustion.
  - Evidence: `no space left on device` during Compose image export and Go linker output.
  - Attempts made: Updated API and runner Docker builders from Go 1.24 to Go 1.25; rebuilt Compose.
  - Required resolution: Free host Docker/build cache space, then rerun Compose and runner integration tests.
  - Independent work still available: PR CI monitoring and merge.

## Pending Implementation

- [ ] Continue the next incomplete MVP requirement after the #51 PR is merged.

## Quality Backlog

## Decisions

- Decision: Treat pipeline execution as a dedicated runner role instead of accepting developer-agent exit status as a deterministic gate.
  - Context: Issue #47.
  - Reason: Keeps runner execution and pipeline result independently durable and deterministic.

- Decision: Complete pending GitHub checks at a durable graph boundary and re-enter through the maintenance loop.
  - Context: Issue #46.
  - Reason: Avoids a busy graph self-loop while preserving a bounded 30-second reconciliation cadence.

## Validation Status

- Targeted tests: Passed — #51 PostgreSQL integration suite (2 tests) and prior workflow-focused subset.
- Service tests: Passed — prior 267 tests, 6 subtests.
- Full repository tests: Not run.
- Build: API/runner Docker builders corrected; Compose blocked by disk exhaustion.
- Lint: Passed substantive Ruff checks with the mount-only executable-bit EXE002 ignored; `make lint` is blocked locally by that artifact.
- Type checks: Passed with an isolated mypy cache; the default cache was locked by another worktree.
- Database migrations: Passed against real PostgreSQL 16 and verified idempotent.
- Docker Compose: CI YAML parsed; full Compose remains blocked by disk exhaustion.
- End-to-end workflow: Passed in orchestrator fake-adapter suite.

## Known Issues

- Issue: Host disk exhaustion blocks Docker Compose and runner integration build.
  - Severity: Environment
  - Impact: Cannot verify fresh-container boot locally.
  - Evidence: `no space left on device` from Docker/containerd and Go linker.
  - Suggested resolution: Free Docker build cache space and rerun the commands recorded above.

## Next Recommended Implementation

Push and monitor the #51 PostgreSQL integration PR through green CI and mergeability, then continue the next MVP requirement.
