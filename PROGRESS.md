# Implementation Progress

## Current Status

- Overall status: Complete; PR follow-up preparation
- Current phase: Workflow quality and recovery
- Active implementation: PR #80 CI monitoring — agent/workflow-quality
- Last updated: 2026-07-28
- Agent/session identifier: agent/workflow-quality

## In Progress

- [x] Implement bounded half-open circuit recovery for PR #80
  - Completed: 2026-07-28
  - Relevant files: `orchestrator/migrations/006_circuit_half_open_probes.sql`, `orchestrator/src/moirai/persistence/control_plane.py`, `orchestrator/src/moirai/workflows/persistence.py`, `orchestrator/tests/`
  - Behavior delivered: Timed cooldown-gated transition to `half_open` state; project/provider circuit lock on probe ownership; probe outcome correctly closes or reopens circuits; atomicity ensures concurrent probes cannot claim the same circuit.
  - Validation performed: 278 orchestrator tests, 6 subtests; focused circuit probe recovery and state tests; full runner suite; Ruff.
  - Commands executed: `/tmp/opencode/moirai-workflow-venv/bin/pytest -q`; `/tmp/opencode/moirai-workflow-venv/bin/ruff check --ignore EXE002 src tests`; `go test ./...`.
  - Notes: Circuit logic verified transactionally in both control-plane scheduling and persistent workflow outcomes.

## Done

- [x] Resolve workflow quality and recovery issues #53, #59, #60, #61, and #62
  - Completed: 2026-07-28
  - Relevant files: `orchestrator/migrations/005_workflow_quality_recovery.sql`, `orchestrator/src/moirai/{workflows,persistence,issue_trackers,code_hosts,services}/`, `orchestrator/tests/`, `runner/internal/{dispatch,taskpacket}/`, `README.md`
  - Behavior delivered: Real runner-event entry-point E2E coverage through `RunnerControlService` and `PersistedWorkflowRuntime`; persisted outcome hashes with four-step non-progress blocking; durable project/provider circuit state and scheduler filtering; task packet context and isolated reviewer prompts; complete portable tracker/code-host interfaces; terminal workflow label reconciliation.
  - Validation performed: 273 orchestrator tests plus 6 subtests; focused end-to-end/recovery tests; Ruff; all runner package tests.
  - Commands executed: `/tmp/opencode/moirai-workflow-venv/bin/pytest -q`; `/tmp/opencode/moirai-workflow-venv/bin/ruff check --ignore EXE002 src tests`; `go test ./...`; focused pytest suites; `go test ./internal/taskpacket ./internal/dispatch`.
  - Notes: Rebased onto merged workflow-correctness PR #77 before implementation.

## Blocked

- [ ] Repository-wide local mypy gate
  - Reason: The installed mypy 2.3.0 reports 83 existing strict diagnostics in generated protobuf stubs and unrelated modules.
  - Evidence: `MYPY_CACHE_DIR=/tmp/opencode/moirai-workflow-quality-mypy-cache /tmp/opencode/moirai-workflow-venv/bin/mypy src/moirai`.
  - Attempts made: Initial incremental cache was locked; isolated-cache rerun completed and identified the pre-existing diagnostics. Changed modules have no reported diagnostics; the remaining targeted result is an existing `schema_validation.py:30` error.
  - Required resolution: Align generated protobuf stubs/mypy configuration or repair the pre-existing strict diagnostics.
  - Independent work still available: PR CI monitoring.

## Pending Implementation

- [ ] Continue the next incomplete MVP requirement after this PR.
  - Priority: P2
  - Dependencies: Merge of #53, #59, #60, #61, and #62.
  - Expected behavior: Advance the next PROJECT.md acceptance criterion.
  - Definition of done: Relevant production behavior and targeted validation.

## Quality Backlog

- None.

## Decisions

- Decision: Keep #53, #59, #60, #61, and #62 in one coherent workflow-quality/recovery change.
  - Context: They share terminal workflow handling, durable evidence, runner task packets, and adapter boundaries.
  - Alternatives considered: Separate narrowly scoped changes.
  - Reason: End-to-end coverage needs the same production boundaries as recovery and reconciliation.
  - Consequences: Changes remain confined to orchestrator workflow/adapters and runner task packet/dispatch modules.

- Decision: Persist progress evidence in the terminal-event transaction but keep graph transition payloads limited to graph state.
  - Context: Outcome metadata must survive retries without changing established workflow transition contracts.
  - Alternatives considered: Add evidence keys to every graph transition.
  - Reason: The graph only needs a blocking transition after the non-progress threshold; persistence remains authoritative for diagnostics.
  - Consequences: Repeated terminal outcomes block on the fourth repeat while ordinary transitions preserve their existing payload shape.

## Validation Status

Record only validation that was actually run.

- Targeted tests: Passed — focused orchestrator suites, 93 tests.
- Service tests: Passed — `/tmp/opencode/moirai-workflow-venv/bin/pytest -q`: 273 passed, 6 subtests.
- Full repository tests: Not run.
- Build: Not run.
- Lint: Passed — `/tmp/opencode/moirai-workflow-venv/bin/ruff check --ignore EXE002 src tests`.
- Type checks: Blocked by 83 existing generated-protobuf/unrelated strict diagnostics under mypy 2.3.0; no changed-module diagnostics reported.
- Database migrations: Unit-tested through migration discovery/content coverage.
- Docker Compose: Not run.
- End-to-end workflow: Passed — runner-event entry test drives `RunnerControlService` into `PersistedWorkflowRuntime` and verifies fake PR/merge/issue completion/checkpoint effects.
- Runner tests: Passed — `go test ./...`.

## Known Issues

- Issue: Repository-wide mypy reports pre-existing generated-protobuf and unrelated strict diagnostics.
  - Severity: P2
  - Impact: Local typecheck gate is not green despite all tests and lint passing.
  - Evidence: 83 errors across 11 files, predominantly `src/moirai/protocols/proto/`.
  - Suggested resolution: Exclude generated sources from strict checking or regenerate compatible stubs, then fix remaining source diagnostics.

## Next Recommended Implementation

Monitor the workflow-quality/recovery PR CI. If CI exposes a failure, reproduce it in this worktree, make a focused fix, rerun the relevant checks, and update this record before the next commit.
