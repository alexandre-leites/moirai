# Implementation Progress

## Current Status

- Overall status: Complete; PR #74 rebased onto current main and awaiting CI
- Current phase: Resolve GitHub issues #40 and #70
- Active implementation: None
- Last updated: 2026-07-28
- Agent/session identifier: agent/docs-contracts

## In Progress

## Done

- [x] Rebase PR #74 onto `origin/main`
  - Completed: 2026-07-28
  - Relevant files: `PROGRESS.md`, orchestrator workflow files, and runner-event tests
  - Behavior delivered: Preserved #40/#70 schema packaging, fail-closed validation, GitHub token wiring, OpenAPI, and documentation alongside main's workflow-correctness changes.
  - Validation performed: 270 orchestrator tests, runner race tests, API tests, web typecheck/lint, Ruff, Mypy, and Compose rendering.
  - Commands executed: `git rebase origin/main`; `make VENV=/tmp/opencode/moirai-venv test`; Ruff and Mypy commands; `docker compose config`.

- Overall status: #51 PR is green and mergeable; awaiting review/merge
- Current phase: PostgreSQL persistence integration coverage
- Active implementation: None
- Last updated: 2026-07-28
- Agent/session identifier: agent/postgres-integration-tests

## In Progress

- [ ] Await review and merge of #51 PR #79.
  - Started: 2026-07-28
  - Relevant files: GitHub Actions / PR checks
  - Current state: PR #79 is non-draft, all CI checks passed, and GitHub reports `CLEAN`.
  - Remaining work: Human review and merge.
  - Definition of done: PR is merged.
  - Targeted validation: GitHub Actions checks.
- Overall status: PR #75 review remediation complete; preparing push and CI verification.
- Current phase: production-readiness hardening.
- Active implementation: GitHub issues #64, #65, and #66.
- Last updated: 2026-07-28
- Agent/session identifier: agent/api-transport-observability

## In Progress

- [ ] Resolve PR #75 independent review findings
  - Started: 2026-07-28
  - Relevant files: `orchestrator/src/moirai`, `orchestrator/tests`, `runner/internal/control`
  - Current state: rebased onto `origin/main`; metrics use read-only control-plane state and runner mTLS integration coverage is passing.
  - Remaining work: push and CI monitoring.
  - Definition of done: metrics are backed by control-plane state and a runner↔orchestrator mTLS integration test passes.
  - Targeted validation: completed — 272 orchestrator tests, full API and runner race suites, ruff, and mypy.


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
  - Behavior delivered: Durable graph state, terminal lock release, transient outbox retries, dedicated pipeline execution, pending-check polling, idempotent merge, and reachable human approval state.
  - Validation performed: 267 orchestrator tests plus 6 subtests; targeted Ruff lint; runner taskpacket/dispatch tests.

- [x] Resolve #40 and #70
  - Completed: 2026-07-28
  - Relevant files: `orchestrator/`, `api/`, `runner/`, `web/`, `Makefile`, `compose.yaml`, root documentation
  - Behavior delivered: Planner and reviewer schemas are packaged within the orchestrator Docker context and loaded with `importlib.resources`; a missing schema fails closed instead of terminating a runner event path. The configured GitHub token reaches issue-sync and code-host `gh` commands, and configured credentials are checked during orchestrator startup. Added a maintained OpenAPI 3.1 contract, service configuration documentation, architecture documentation, Make help, aggregate tests, and the HTTP Compose cookie override required for local browser sessions.
  - Validation performed: 258 orchestrator tests, runner/API tests, web typecheck/lint and production build, lint/type checks, Compose rendering, OpenAPI parsing/path validation, and isolated orchestrator image build with schema resource loading.

## Blocked

- [ ] Full Docker Compose fresh-volume boot and runner integration binary test
  - Reason: Host/containerd disk exhaustion recorded by the workflow-correctness implementation.
  - Evidence: `no space left on device` during Compose image export and Go linker output.
  - Required resolution: Free host Docker/build cache space, then rerun Compose and runner integration tests.
  - Independent work still available: PR CI monitoring and mergeability checks.

## Pending Implementation

- [ ] Continue the next incomplete MVP requirement after current PRs merge.

- [ ] Continue the next incomplete MVP requirement after the #51 PR is merged.

  - Validation performed: 267 orchestrator tests, targeted Ruff, runner taskpacket/dispatch tests.

- [x] Structured logging, metrics, request correlation, and config-gated TLS
  - Completed: 2026-07-28
  - Behavior delivered: JSON logs retain extras, API propagates request IDs, and TLS/mTLS is config-gated across API, orchestrator, and runner.
  - Validation performed: API and runner Go tests/builds; 258 orchestrator tests; ruff, mypy, and Compose configuration.

## Blocked

- [ ] Fresh Compose boot and runner integration image build
  - Reason: Host/containerd disk exhaustion reported by the workflow-correctness workstream.
  - Independent work still available: PR #75 review remediation.

## Pending Implementation

- [ ] Continue next incomplete MVP requirement after PR #75.


## Quality Backlog

- [ ] Address existing web lint warnings
  - Category: Developer experience
  - Risk: Low
  - Expected benefit: Warning-free web validation.
  - Recommended timing: Dedicated web task.

## Decisions

- Decision: Treat pipeline execution as a dedicated runner role.
  - Context: Workflow correctness issue #47.
  - Reason: Keeps the pipeline result independently durable and deterministic.

- Decision: Package result schemas under `moirai.workflows` and load them as package resources.
  - Context: The Compose Docker build context is `./orchestrator`, excluding repository-root schemas.
  - Reason: Package data survives installations and image contexts without Dockerfile coupling.
  - Consequences: A missing schema produces an unvalidated result and follows the existing fail-closed workflow path.

- Decision: Reuse one configured `SubprocessCommandRunner` for GitHub code-host and issue-sync adapters.
  - Context: `LOOP_GITHUB_TOKEN` was parsed but not used.
  - Reason: A shared runner consistently supplies `GH_TOKEN` and supports a single startup readiness check.

## Validation Status

- Targeted tests: PR #74 originally passed 258 orchestrator tests, runner race tests, API tests, and web typecheck/lint.
- Service tests: Runner and API tests passed; web production build passed.
- Full repository tests: Not run.
- Build: API/runner/web builds passed for PR #74.
- Lint: Ruff passed; web lint passed with existing warnings and no errors.
- Type checks: Mypy and web TypeScript checks passed for PR #74.
- Database migrations: Workflow-correctness unit-tested; no fresh Compose validation due host disk exhaustion.
- Docker Compose: PR #74 `docker compose config` passed; fresh boot remains blocked as above.
- End-to-end workflow: Orchestrator fake-adapter suite passed; isolated PR #74 image loaded both packaged schemas.

- Targeted tests: Passed — #51 PostgreSQL integration suite (2 tests) and prior workflow-focused subset.
- Service tests: Passed — prior 267 tests, 6 subtests.
- Full repository tests: Not run.
- Build: API/runner Docker builders corrected; Compose blocked by disk exhaustion.
- Lint: Passed substantive Ruff checks with the mount-only executable-bit EXE002 ignored; `make lint` is blocked locally by that artifact.
- Type checks: Passed with an isolated mypy cache; the default cache was locked by another worktree.
- Database migrations: Passed against real PostgreSQL 16 and verified idempotent.
- Docker Compose: CI YAML parsed; full Compose remains blocked by disk exhaustion.
- End-to-end workflow: Passed in orchestrator fake-adapter suite.
- Decision: Insecure gRPC remains the local-development default; TLS requires explicit configuration.
  - Context: remote runners require secure transport while existing Compose development remains compatible.
  - Reason: preserves compatibility and allows mTLS enforcement with a configured client CA.

- Decision: Metrics collection reads state only through the orchestrator control plane.
  - Context: database access belongs exclusively to the orchestrator.
  - Reason: preserves service boundaries while providing meaningful operational gauges.

## Validation Status

- Targeted tests: 272 orchestrator tests passed; runner mTLS integration test passed.
- Service tests: API `go test ./...` and runner `go test -race ./...` passed.
- Full repository tests: not run.
- Build: `go build ./cmd/api` and `go build ./cmd/runner` passed.
- Lint: ruff passed with EXE002 ignored.
- Type checks: mypy passed on all 47 orchestrator source files.
- Database migrations: not applicable.
- Docker Compose: configuration passed; fresh boot blocked by host disk exhaustion.
- End-to-end workflow: prior orchestrator fake-adapter suite passed.



## Known Issues

- Issue: Host disk exhaustion blocks fresh Docker Compose boot and runner integration build.
  - Severity: Environment
  - Impact: Containerized full-stack validation cannot run locally.
  - Suggested resolution: Free Docker build cache space and rerun the recorded validation.

- Issue: Existing web lint rules report warnings.
  - Severity: Low
  - Impact: No validation errors.
  - Suggested resolution: Address Fast Refresh and React hook dependency warnings in a dedicated web task.

## Next Recommended Implementation

Finish rebasing and validating PR #74, then wait for its CI to establish mergeability without merging it.

Await review and merge of #51 PR #79, then continue the next MVP requirement.

- Issue: Host disk exhaustion can block fresh Docker Compose verification.
  - Severity: Environment
  - Suggested resolution: Free Docker/build cache space before a fresh-container run.

## Next Recommended Implementation

Finish PR #75 review remediation, push it, and monitor CI until clean and mergeable.

## Done

- [x] Resolved runner reliability PR #76 conflicts with orchestrator state.

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


---

# Session: GitHub issue #91 — offer expiry must not cancel in-flight workflows

## Current Status

- Overall status: Complete; branch `issue-91` pushed with a PR open against `main`
- Current phase: Platform review remediation (finding F4)
- Active implementation: None (issue #91 finished 2026-07-29)
- Last updated: 2026-07-29
- Agent/session identifier: agent/issue-91

## Done

- [x] Offer expiry and rejection no longer cancel in-flight workflow runs (GitHub issue #91, review finding F4)
  - Completed: 2026-07-29
  - Relevant files: `orchestrator/src/moirai/persistence/control_plane.py`, `orchestrator/src/moirai/scheduler.py`, `orchestrator/tests/test_asyncpg_control_plane.py`, `orchestrator/tests/test_scheduler_service.py`, `orchestrator/tests/test_postgres_integration.py`, `docs/architecture.md`
  - Behavior delivered:
    - `expire_offers` and `reject_offer` share one release path that distinguishes a bootstrap offer (run with no branch, pull request, execution, or execution request) from a re-offer of a run with progress. Bootstrap offers still cancel and hand the issue back to the queue; every other unanswered offer requeues the run.
    - An unanswered execution re-offer returns its leaked `dispatched` execution request to `queued` in the same transaction, keeps the workflow status/phase and the project lock, and is re-offered by `schedule_execution` on a later tick.
    - An unanswered recovery re-offer returns the job to `recovering` with a fenced lease generation and the run to `recovering`, so `recover_one` re-offers it.
    - Repeated failure is bounded: `unanswered_offer_limit` (default 5) consecutive unanswered offers that have been failing longer than `unanswered_offer_grace` (default 15 minutes) block the run with `blocking_reason = 'unanswered_offer_limit'`, expire its outstanding execution requests, and release the project lock. Accepting an offer resets the streak. Each release also writes an `offer_unanswered` workflow event.
    - `Scheduler.tick` no longer routes a task-packet build error into `reject_offer`: it logs, skips the candidate, and leaves the offer to expire. An undelivered offer (returned `False` or raised) is released and the loop continues instead of breaking, bounded by `max_consecutive_failures` (default 3).
  - Validation performed: failing tests were written first and reproduced both defects (`'cancelled' != 'ai_review'` for a mid-workflow expiry/rejection, `'cancelled' != 'recovering'` for a recovery offer, `OfferDeliveryError` from a packet-build failure, `0 != 1` placements after one undelivered offer), then all suites were rerun green.
  - Commands executed:
    - `make test-orchestrator` — `Ran 303 tests ... OK (skipped=8)`
    - `LOOP_TEST_DATABASE_URL=postgresql://loop:loop@127.0.0.1:55491/loop make test-postgres-integration` — `Ran 8 tests ... OK` (throwaway `postgres:16-alpine` container)
    - `make lint` — `All checks passed!`
    - `make typecheck` — `Success: no issues found in 47 source files`
    - `make test-runner` — `ok` for all runner packages
    - `make test-api` — `ok` for all API packages
    - `make compose` — Compose configuration rendered
  - Notes: No migration was required. The unanswered-offer streak is derived from `app.job_offers` rows (offers created after the last `accepted` offer for that job), so no schema change or new counter column was needed.

## Decisions

- Decision: Bound repeated offer expiry by both a consecutive-offer count and a grace period, and block the run rather than cancel it.
  - Context: Issue #91 step 2 requires a bounded policy so a run cannot ping-pong between expiry and re-offer forever. The scheduler ticks every second while the offer TTL is 600 seconds, so a count-only bound is consumed in seconds by a runner restart.
  - Alternatives considered: (a) count only (`N` consecutive expiries → blocked), which would block a healthy run with an open pull request within seconds of a brief runner outage; (b) time only, which never bounds a fast expiry loop; (c) a new `consecutive_offer_expiries` column plus a migration, which duplicates state `app.job_offers` already records.
  - Reason: The pair (`unanswered_offer_limit = 5`, `unanswered_offer_grace = 15 minutes`) blocks only runs that have both failed repeatedly and been failing for a long time. `blocked` is the correct terminal state because it carries a `blocking_reason` and releases the project lock, whereas `cancelled` reads as "nothing happened".
  - Consequences: A total-fleet outage shorter than the grace period never terminates a run; a genuinely unschedulable run stops holding its project lock after the grace period. Both bounds are `AsyncpgControlPlane` constructor arguments, so `main.py` needs no change and operators can tune them in one place.

- Decision: Keep cancelling the bootstrap offer instead of requeuing it.
  - Context: Issue #91 step 5 allows either behavior for a bootstrap expiry.
  - Alternatives considered: Return the bootstrap run to `recovering` so `recover_one` re-offers it.
  - Reason: A bootstrap run holds no branch, pull request, execution, or execution request, and cancelling it is how the project lock is released so the issue re-enters global priority ordering — possibly behind a higher-priority issue that appeared meanwhile. Requeuing would pin the project to one issue and delay fairness for no durable gain.
  - Consequences: A bootstrap run still ends `cancelled` with `terminal_reason = 'offer_expired'` (or `'runner_rejected_offer'`), but no work is lost and the issue is rescheduled on the next tick. Repeated bootstrap churn on a dead fleet remains bounded only by the project circuit breaker (GitHub issue #92, deliberately out of scope here).

- Decision: A task-packet build failure leaves the offer alone instead of rejecting it.
  - Context: The scheduler's exception arm previously routed any failure, including `build_task_packet` errors, into the terminal reject path.
  - Alternatives considered: Reject the offer immediately so the runner capacity is freed sooner.
  - Reason: A build error says nothing about the runner, and rejecting it would count against the unanswered-offer streak for a fault the runner never saw. Letting the offer expire on its TTL keeps the failure entirely orchestrator-side, and the tick simply moves to the next candidate.
  - Consequences: A failed packet build holds one runner slot until the offer TTL elapses. Repeated build failures are bounded per tick by `max_consecutive_failures`.

## Validation Status

- Targeted tests: Passed — `test_scheduler_service.py` (24 tests), `test_asyncpg_control_plane.py` (39 tests).
- Service tests: Passed — `make test-orchestrator`: 303 tests, 8 skipped (PostgreSQL integration skips without a database URL).
- Full repository tests: Passed — `make test-orchestrator`, `make test-runner`, `make test-api`. `make test-web` not run (no web change; requires npm install).
- Build: Not run (no build-affecting change).
- Lint: Passed — `make lint`.
- Type checks: Passed — `make typecheck`.
- Database migrations: No migration added; migrations applied by `make test-postgres-integration` against a real PostgreSQL 16 container.
- Docker Compose: Passed — `make compose`.
- End-to-end workflow: Not run.

## Known Issues

- Issue: An unanswered offer that blocks a run writes `blocked` directly through the control plane, so it does not resolve a `half_open` project or provider circuit probe.
  - Severity: P2
  - Impact: A run that was serving as a circuit probe and is blocked by the unanswered-offer bound leaves the circuit in `half_open`.
  - Evidence: `workflows/persistence.py` resolves probes only inside `transition()`; `expire_offers` has never used that path.
  - Suggested resolution: GitHub issue #92 (circuit-breaker wedge states), which owns circuit resolution routing and was deliberately excluded from this change to avoid overlapping edits to `expire_offers`.

## Next Recommended Implementation

GitHub issue #92 (circuit-breaker wedge states): route every terminal outcome that `expire_offers` and `reject_offer` can produce — `cancelled` for a bootstrap release and `blocked` for the unanswered-offer bound — through the circuit-probe resolution used by `workflows/persistence.transition`, and add an orphan-probe reaper. Relevant files: `orchestrator/src/moirai/persistence/control_plane.py`, `orchestrator/src/moirai/workflows/persistence.py`. Targeted validation: `make test-orchestrator` plus new PostgreSQL integration coverage in `orchestrator/tests/test_postgres_integration.py`.
