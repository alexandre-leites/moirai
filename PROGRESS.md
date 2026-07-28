# Implementation Progress

## Current Status

- Overall status: Complete; PR preparation
- Current phase: Core workflow correctness
- Active implementation: None
- Last updated: 2026-07-28
- Agent/session identifier: agent/workflow-correctness

## In Progress

- [ ] Monitor the workflow-correctness PR CI and merge when green.
  - Started: 2026-07-28
  - Relevant files: GitHub Actions / PR checks
  - Current state: Local orchestrator validation is green; PR pending creation.
  - Remaining work: Push, create PR, resolve CI, merge.
  - Definition of done: Non-draft PR is mergeable and merged.
  - Targeted validation: GitHub Actions checks.

## Done

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

- [ ] Continue next incomplete MVP requirement after this coherent workflow-correctness PR.

## Quality Backlog

## Decisions

- Decision: Treat pipeline execution as a dedicated runner role instead of accepting developer-agent exit status as a deterministic gate.
  - Context: Issue #47.
  - Reason: Keeps runner execution and pipeline result independently durable and deterministic.

- Decision: Complete pending GitHub checks at a durable graph boundary and re-enter through the maintenance loop.
  - Context: Issue #46.
  - Reason: Avoids a busy graph self-loop while preserving a bounded 30-second reconciliation cadence.

## Validation Status

- Targeted tests: Passed — 138 tests for workflow-focused subset.
- Service tests: Passed — 267 tests, 6 subtests.
- Full repository tests: Not run.
- Build: API/runner Docker builders corrected; Compose blocked by disk exhaustion.
- Lint: Passed — Ruff with existing executable-bit EXE002 ignored.
- Type checks: Blocked by 83 existing generated-protobuf and unrelated strict errors; no new errors in changed workflow modules.
- Database migrations: Unit-tested; real Compose boot blocked by disk exhaustion.
- Docker Compose: Blocked by disk exhaustion.
- End-to-end workflow: Passed in orchestrator fake-adapter suite.

## Known Issues

- Issue: Host disk exhaustion blocks Docker Compose and runner integration build.
  - Severity: Environment
  - Impact: Cannot verify fresh-container boot locally.
  - Evidence: `no space left on device` from Docker/containerd and Go linker.
  - Suggested resolution: Free Docker build cache space and rerun the commands recorded above.

## Next Recommended Implementation

Monitor and merge the workflow-correctness PR; after host disk space is available, rerun `docker compose -p moiraiworkflowcorrectness up --build -d` and the runner integration suite, then continue the next MVP requirement.
