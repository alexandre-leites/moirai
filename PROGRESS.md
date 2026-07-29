# Implementation Progress

## Current Status

- Overall status: MVP audit complete (issue #87). The stack builds, boots and schedules, but the
  end-to-end flow **Web UI → API → Orchestrator → Runner → Orchestrator → API → Web UI** does not
  close: the runner cannot authenticate to GitHub, and the API/UI have no read path for a
  workflow's progress, logs or resulting pull request.
- Current phase: MVP gap remediation. Backlog is now the full set of open issues (#88–#124).
- Active implementation: None. Audit finished; no production code changed by this task.
- Last updated: 2026-07-29
- Agent/session identifier: agent/mvp-audit-87

## In Progress

## MVP audit (issue #87, 2026-07-29)

Every claim below is grounded in a file that was opened during the audit. No speculation.

### Critical repository finding: PR #81 was merged as a no-op

`git rev-parse a260c8f^{tree} bbc8432^{tree}` returns the **same tree hash**, so merge commit
`bbc8432` ("Add live operations dashboard (#81)") contributed zero bytes to `main`. Its conflict
resolution discarded the entire 4364-line changeset. Confirmed absent from the tree:
`api/internal/http/handlers/events.go`, `api/internal/http/handlers/queue.go`,
`web/src/events.ts`, `web/src/operations.tsx`, `web/src/api.test.ts`,
`web/src/live-pages.test.tsx`, the workflow/runner action RPCs in `proto/control_plane.proto`,
and the matching orchestrator control-plane methods.

Issues #54, #56 and #57 were auto-closed as `completed` by that merge. The work they describe is
not in the repository. Re-filed as #110, #112, #113, #117, #118, #119, #120 and #123, each stating
explicitly why it is not a duplicate.

### Status by MVP capability

**Complete and working**

- Monorepo layout, `compose.yaml` three-network isolation, secrets-from-files, per-service
  healthchecks; CI covers orchestrator unit tests, a real PostgreSQL 16 migration/persistence
  suite, Go race tests, `govulncheck`/`pip-audit`/`npm audit`/Trivy, and a Compose smoke boot
  (`.github/workflows/ci.yml`).
- Authentication: login/logout/me, server-side sessions, CSRF, per-session rate limiting
  (`api/internal/auth/`, `orchestrator/src/moirai/persistence/authentication.py`).
- Project CRUD end to end: `web/src/projects.tsx` → `api/internal/http/handlers/handlers.go` →
  `orchestrator/src/moirai/grpc/control_plane.py` → PostgreSQL. The API holds no DB handle.
- Runner registration tokens, one-time-token registration, persistent credential, outbound
  bidirectional gRPC, heartbeats, leases with generation fencing, renewal, reconnection and a
  crash-safe event outbox (`runner/internal/control/`, `orchestrator/src/moirai/grpc/runner_control.py`).
- Global scheduler: priority ordering with creation-time tie-break, one-active-workflow-per-project
  lock via `app.project_locks`, up to 50 offers per tick, PostgreSQL advisory-lock leader election
  (`orchestrator/src/moirai/scheduler.py`, `persistence/control_plane.py:946-1163`).
- Issue synchronisation from GitHub through `gh` with exponential backoff, durable retry state and
  label reconciliation (`orchestrator/src/moirai/services/issue_sync.py`).
- Durable LangGraph workflow: 13 nodes, PostgreSQL checkpointing, retry budgets, transition outbox
  with at-least-once drain, stalled-run recovery, project/provider circuit breakers with half-open
  probes (`orchestrator/src/moirai/workflows/`).
- Runner execution: mirror clone/worktree preparation, task-packet and prompt artifacts, opencode /
  CLI / Docker backends, pipeline execution, commit, push, log streaming, cancellation, workspace
  retention (`runner/internal/`).
- Orchestrator-side pull-request creation, check polling, merge and issue closure via `gh`
  (`orchestrator/src/moirai/code_hosts/github_cli.py`, `workflows/nodes.py`).
- Orchestrator observability: structured JSON logs, request-ID propagation, and a metrics loop
  backed by real control-plane state (`orchestrator/src/moirai/main.py:270-281`).

**Partially implemented**

- Workflow visibility. `ListWorkflows` returns only `{id, project_id, status, phase}`
  (`proto/control_plane.proto:56`). The pull-request URL is persisted
  (`workflows/persistence.py:80-97`) and never exposed. → #110, #117
- Local pipeline gate. The dedicated pipeline execution role exists, but
  `app.project_pipeline_steps` is read at `persistence/control_plane.py:848` and **written
  nowhere in the repository**, so every pipeline execution runs zero commands and reports
  `completed` → `pipeline_passed = True`. → #114 (with #90)
- Task packet context. `acceptance_criteria` is the issue title
  (`persistence/control_plane.py:826`); `plan`, `diffSummary`, `failedChecks` and `reviewFindings`
  are never populated. → pre-existing #106
- Merge gate. `nodes.py:146-159` transitions to `"merging"` on an unverified `gh pr merge` and then
  closes the issue; `app.pull_requests.merged_at` is never written. → #121
- Runner administration. `set_runner_state` supports enable/disable/drain/revoke
  (`persistence/control_plane.py:674-705`) with no gRPC caller. → #119
- Metrics. Orchestrator metrics are real; the API (`api/internal/http/server.go:81-88`) and runner
  (`runner/internal/metrics/metrics.go:19-38`) publish gauges permanently set to zero. → #124

**Implemented but not integrated**

- `GET /api/v1/runners` exists (`api/internal/http/handlers/runners.go:19-51`, `openapi.yaml:285`);
  `web/src/api.ts` has no `listRunners` and `web/src/main.tsx` has no route. → #113
- `EnvironmentRef` is specified (`schemas/task-packet.schema.json:42-54`), validated
  (`runner/internal/taskpacket/taskpacket.go:66-69`) and resolved
  (`runner/cmd/runner/main.go:52-62`), but `workflows/task_packets.py:218` hardcodes
  `"environmentRefs": []`. → #109
- `OrchestratorToRunner.drain` and `.cancel` are defined and handled by the runner
  (`dispatch/control_loop.go:187-195`); the orchestrator never constructs either. → #119, #120

**Missing**

- Server-Sent Events. `grep -rn "event-stream\|EventSource"` finds nothing, though `PROJECT.md:117,134`
  specifies REST + SSE. → #118
- Queue visibility. No reader, RPC or route returns the eligible-issue queue; only an aggregate
  `queue_depth` in `metrics_snapshot`. → #112
- Workflow retry/cancel/block. → #120
- Web workflow detail, runners and queue pages. → #117, #113, #112
- Web automated tests; `make test-web` is not wired into CI. → #123

**Blocked by another gap**

- #117 (workflow detail page) is blocked by #110 (detail/event RPCs and routes).
- #118 (SSE) is blocked by #110 (shared change-notification path).
- #119 (drain/revoke) is blocked by #111 (the orchestrator currently aborts the stream on
  `RunnerDraining`) and #113 (the page hosting the controls).
- #120 (retry/cancel/block) is blocked by #110 and #117.
- #122 (pipeline environment isolation) is blocked by #109, which introduces the credential that
  makes the leak exploitable.

### Issues created by this audit

| Issue | Priority | Scope |
|---|---|---|
| #109 | P0 | Task packets never declare credentials; runner cannot authenticate `git clone`/`git push` |
| #110 | P0 | No `GetWorkflow` / workflow-events read path; `app.workflow_events` is write-only |
| #117 | P0 | No web workflow detail page: progress, logs and the pull request are never shown |
| #111 | P1 | Orchestrator aborts the runner stream on `RunnerDraining` |
| #112 | P1 | Eligible-issue queue not exposed by orchestrator, API or UI |
| #113 | P1 | No web runners page although `GET /api/v1/runners` exists |
| #114 | P1 | `app.project_pipeline_steps` is unwritable, so the pipeline gate always passes |
| #116 | P1 | `create_pull_request` / `wait_for_checks` report success when no code host resolves |
| #118 | P1 | Server-Sent Events unimplemented; the dashboard never updates without a reload |
| #119 | P1 | Runner drain/revoke unreachable from the API and UI |
| #120 | P1 | Workflow runs cannot be retried, cancelled or blocked by an operator |
| #121 | P1 | Merge is never verified; `merged_at` never written |
| #122 | P1 | Pipeline commands inherit the entire runner process environment |
| #123 | P2 | Web app has no tests and eslint never runs in CI |
| #124 | P2 | API and runner Prometheus gauges are hardcoded to zero |

#115 was filed and closed as a duplicate of the pre-existing #106.

### Recommended implementation order

1. #109 — restore the delivery half of the flow. Nothing downstream is observable until a push works.
2. #110 — the API read path for workflow state, events and the pull-request URL.
3. #117 — the web detail page that consumes #110, closing the flow's last step.
4. #114 (with the pre-existing #90) — make the deterministic pipeline gate real.
5. #116, #121 — stop the workflow reporting phases and merges it did not achieve.
6. #111, then #119 — runner drain protocol, then the operator surface.
7. #113, #112 — runners and queue pages.
8. #120 — retry/cancel/block, on top of #110 and #117.
9. #118 — SSE, once the pages it refreshes exist.
10. #122 — pipeline environment isolation, immediately after #109.
11. #123, #124 — web test coverage and honest metrics.

The pre-existing platform-review backlog (#88–#108) is complementary and largely orchestrator- and
runner-internal. #88 (event-driven graph suspension) is described there as the root workflow defect
and should be sequenced alongside #110, since both change how a run's progress is observed.

### Gaps deliberately not converted into issues

- `app.project_labels` (migration `001_initial.sql:44`) has zero references anywhere in the
  repository. Dead schema, not an MVP gap; label policy is hardcoded in `domain/issues.py:12-20`.
- `app.audit_events` is written (`persistence/authentication.py:313`) and never read. An audit-log
  view is post-MVP and outside `PROJECT.md`'s stated Web UI scope.
- The `progress` runner event type is accepted (`workflows/runner_events.py:8`) with no emitter.
  Cosmetic once #110 exposes the `log` events that do flow.
- `LOOP_RUNNER_RECONNECT_GRACE` and `LOOP_RUNNER_MAX_LOG_BYTES` are parsed and unused. The sibling
  variables are already covered by the pre-existing #95 and #102; these two are too small to
  warrant a separate issue and belong in #102's checklist.
- "The Web UI submits a task through the API" (issue #87's flow step 1) has no literal
  implementation, and should not get one: `PROJECT.md:55,84` scopes task creation to issue-tracker
  synchronisation. The Web UI's equivalent is project registration plus enablement, which works.
  Operator control over what runs is covered by #112 and #120.

### Validation performed for the audit task (2026-07-29, agent/mvp-audit-87)

This task changed documentation only (`PROGRESS.md`); no production code was written, by design —
every gap became an issue, not a patch. The following were run in the `issue-87` worktree and all
passed:

- `make test-orchestrator` — 288 tests, 2 skipped, OK. (The scheduler tracebacks in the output are
  deliberate failure-injection fixtures in `tests/test_scheduler_service.py`.)
- `make test-runner` — `go test -race ./...`, all 10 packages ok.
- `make test-api` — `go test ./...`, all 5 packages ok.
- `make test-web` — `tsc --noEmit` clean; `eslint .` 0 errors, 10 pre-existing warnings (already in
  the quality backlog below).
- `make lint` — ruff, all checks passed.
- `make typecheck` — mypy, no issues in 47 source files.
- `make compose` — `docker compose config` rendered successfully.
- `cd api && go vet ./...` — clean.

Not run: `make test-postgres-integration` (requires `LOOP_TEST_DATABASE_URL`), `make proto-check`
(requires the buf container; no proto changed), and a full Compose boot (no change to any image).

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

## Issue #103 — Bootstrap NameError and step order-dependence (F16)

### Done

- [x] Make `_bootstrap_initial_setup` restartable and its steps order-independent
  - Completed: 2026-07-29
  - Relevant files: `orchestrator/src/moirai/main.py`, `orchestrator/tests/test_main_bootstrap.py`, `orchestrator/README.md`, `.env.example`, `compose.yaml`
  - Behavior delivered:
    - `uuid4` imported at module scope. Pre-fix conditional import caused `UnboundLocalError`.
    - Bootstrap split into three independent steps (`_bootstrap_admin_user`, `_bootstrap_seed_project`, `_bootstrap_registration_token`), each with its own existence check. Previously one early return silently skipped downstream steps, so interrupt or deferred config never seeded the rest.
    - Seed inserts carry `ON CONFLICT DO NOTHING`; admin step re-reads after insert race. Token check ignores `used_at`/`expires_at` (single-use).
    - `LOOP_SEED_PROJECT_NAME=""` disables seed-project bootstrap; `compose.yaml` uses `${LOOP_SEED_PROJECT_NAME-demo}`.
  - Validation: `make test-orchestrator` (301 tests, OK), `make test-postgres-integration` (2 tests, OK), `make lint`, `make typecheck`, `make compose`.

## Issue #88 — Event-driven issue workflow (platform review finding F1)

### Done

- [x] Suspend the LangGraph issue workflow after every execution dispatch
  - Completed: 2026-07-29
  - Relevant files: `orchestrator/src/moirai/workflows/issue_graph.py`, `orchestrator/src/moirai/workflows/nodes.py`, `orchestrator/src/moirai/workflows/runner_events.py`, `orchestrator/src/moirai/workflows/runtime.py`, `orchestrator/tests/test_end_to_end.py`, `orchestrator/tests/test_workflow_nodes.py`, `orchestrator/tests/test_issue_graph.py`, `orchestrator/tests/test_workflow_runtime.py`, `orchestrator/tests/test_runner_events.py`, `orchestrator/tests/test_asyncpg_control_plane.py`, `README.md`, `PROJECT.md`, `orchestrator/README.md`
  - Behavior delivered:
    - `_dispatch` sets `awaiting_execution` on the graph state; `suspend_after_dispatch` wraps the outgoing edge of every dispatching node (`plan`, `implement`, `pipeline`, `review`, `repair`, `push`) so the invocation ends at `END` while an execution is pending.
    - `workflow_transition_for_terminal_event` clears `awaiting_execution` on every terminal transition, so `PersistedWorkflowRuntime.run` resumes the graph from that same edge.
    - `_dispatch` replay guard: a node re-entered while its own request is still `queued` reuses that request.
    - Checkpointer decision: production requires one. Without a checkpointer the runtime leaves a suspended run untouched and logs a warning.
  - Validation: `make test-orchestrator` (298 tests, OK), `make lint`, `make typecheck`, `make compose`.

### Decisions

- Decision: Suspend with a conditional edge to `END` rather than langgraph's static `interrupt_after`.
- Decision: Clear `awaiting_execution` in `workflow_transition_for_terminal_event`, not in `PersistedWorkflowRuntime.run`.

## Issue #91 — Offer expiry must not cancel in-flight workflows (finding F4)
### Done

- [x] Offer expiry and rejection no longer cancel in-flight workflow runs
  - Completed: 2026-07-29
  - Relevant files: `orchestrator/src/moirai/persistence/control_plane.py`, `orchestrator/src/moirai/scheduler.py`, `orchestrator/tests/test_asyncpg_control_plane.py`, `orchestrator/tests/test_scheduler_service.py`, `orchestrator/tests/test_postgres_integration.py`, `docs/architecture.md`
  - Behavior delivered:
    - `expire_offers` and `reject_offer` share one release path distinguishing bootstrap (cancel) from re-offer (requeue). Consecutive failures bounded by `unanswered_offer_limit` + `unanswered_offer_grace`. Accepting an offer resets the streak.
    - `Scheduler.tick` no longer routes packet-build errors into `reject_offer`: logs and skips.
  - Validation: `make test-orchestrator` (303 tests, OK), `make test-postgres-integration` (8 tests, OK), `make test-runner`, `make test-api`, `make lint`, `make typecheck`, `make compose`.
---

# Issue #89 — Runner: a missing or invalid result document must not be reported as success

- Overall status: Complete; awaiting PR review
- Current phase: 2026-07-29 platform review remediation (finding F2)
- Active implementation: None
- Last updated: 2026-07-29
- Agent/session identifier: agent/issue-89

## Done

- [x] Stop the opencode backend from reporting success without result evidence
  - Completed: 2026-07-29
  - Relevant files: `runner/internal/agents/opencode.go`, `runner/internal/agents/opencode_test.go`, `runner/README.md`
  - Behavior delivered:
    - `OpenCodeBackend.Execute` no longer discards `readResultDocument`'s error. When the process exits 0 but the result document is missing or invalid, it returns `status="failed"` and an error wrapping the new exported sentinel `agents.ErrNoResultEvidence`, e.g. `agent exited 0 without a valid result document (.loop/result.json): agent result was not written`. The summary carries the same text, so the persisted `terminal-result.json` names the missing evidence too.
    - A valid document keeps its own status, summary, changed files, commands, remaining work, session ID, and raw payload — unchanged from before, including the case where the process exits non-zero.
    - "Process failed" and "no result evidence" stay distinguishable in the failure fingerprint: when the process itself fails, the executor error is still returned (`ErrNoResultEvidence` is not used), so the two paths hash differently downstream in `dispatch.FailureFingerprint`. Measured end to end: `agent:42051f1c5fc5560d` (exit 0, no document) vs `agent:68daef1c6e1313c9` (exit 3).
    - Failure text is free of absolute workspace paths (`readResultDocument` reports a missing file without the path; `workspaceRelativePath` renders `.loop/result.json`), so repeated no-evidence failures produce one stable fingerprint instead of a new one per workspace.
  - Validation performed:
    - `make test-runner` — pass (all runner packages, race detector).
    - `cd runner && gofmt -l .` — no output.
    - `cd runner && go vet ./...` — clean.
    - `cd runner && go build ./...` — clean.
    - New unit tests in `runner/internal/agents/opencode_test.go`: exit 0 + missing document, exit 0 + malformed JSON, exit 0 + wrong `executionId` (all three assert `failed`, `errors.Is(err, ErrNoResultEvidence)`, evidence named, no absolute path leak), exit 0 + valid document still `completed` (`TestOpenCodeBackendReadsValidatedResult`), non-zero exit + valid document keeps the document status while returning the process error, non-zero exit + missing document reports the process failure rather than `ErrNoResultEvidence`, and failure text stability across two workspaces.
    - Temporary end-to-end harness (real `OpenCodeBackend` + `Dispatcher` + `ControlLoop`, deleted after use) confirmed the emitted terminal event: `type=failed`, `"error":"execute agent: agent exited 0 without a valid result document (.loop/result.json): agent result was not written"`, `"exitCode":0`, `"failureFingerprint":"agent:42051f1c5fc5560d"`.
  - Commands executed: `make test-runner`; `cd runner && gofmt -l .`; `cd runner && go vet ./...`; `cd runner && go build ./...`; `cd runner && go test -race ./internal/agents/ -v`.
  - Notes: Re-prompting/continuation stays out of scope (issue #104). `runner/internal/dispatch/control_loop.go` and `runner/internal/control/` were not touched; concurrent agents own those files.

## Decisions

- Decision: Carry the "no result evidence" classification in the returned error rather than in `dispatch.FailureFingerprint`'s category table.
  - Context: The fingerprint is computed from the error text in `control_loop.go`, which is owned by a concurrent agent.
  - Alternatives considered: Adding a dedicated fingerprint category for missing result documents.
  - Reason: Keeping the change inside `runner/internal/agents/` avoids a conflict on shared dispatch files, and a stable, path-free error message already yields a distinct, repeatable fingerprint for each failure mode.
  - Consequences: Both modes share the `agent:` component prefix but differ in digest; a later fingerprint-classification change (F14) can promote the distinction to the prefix without touching the backend.

## Validation Status

- Runner tests: Passed — `make test-runner` (`cd runner && go test -race ./...`), all packages `ok`.
- Lint/format: Passed — `gofmt -l .` empty, `go vet ./...` clean.
- Build: Passed — `cd runner && go build ./...`.
- Orchestrator, API, web tests: Not run — change is confined to the runner's agent backend.

---

# Non-progress fingerprinting (issue #101, finding F14, 2026-07-29)

## Current Status

- Overall status: Complete, awaiting review.
- Current phase: P3 hardening from the 2026-07-29 platform review.
- Active implementation: none — issue #101 finished (session `issue-101`, 2026-07-29).
- Last updated: 2026-07-29
- Agent/session identifier: `issue-101`

## Done

- [x] Fix non-progress fingerprinting: cross-phase collisions and unstable failure fingerprints (issue #101, finding F14)
  - Completed: 2026-07-29
  - Relevant files:
    - `orchestrator/src/moirai/persistence/control_plane.py` (`_record_progress_evidence`, the
      non-progress comparison in `accept_event`, and the new outcome-identity helpers)
    - `orchestrator/src/moirai/workflows/persistence.py` (`transition` durable-column writer)
    - `orchestrator/tests/test_asyncpg_control_plane.py`, `orchestrator/tests/test_workflow_persistence.py`
    - `README.md` ("Workflow recovery guarantees")
  - Behavior delivered:
    - A terminal outcome now has a *kind-scoped, role-scoped* identity. Successes are
      identified by a diff hash over `(role, sorted changed files, exit code, result document
      minus per-attempt identifiers)`; failures and cancellations by a fingerprint over
      `(role, event type, exit code, stable failure fingerprint)`. Every zero-diff success no
      longer collides on `sha256("[]")` across phases.
    - The failure identity is the runner's own `failureFingerprint` when the payload carries
      one, so volatile fields (`durationMs`, counters) can no longer make two identical
      failures hash differently. When no fingerprint is supplied (older runners, `cancelled`
      events) the orchestrator derives one with `_runner_failure_fingerprint`, a byte-exact
      port of the runner's `dispatch.FailureFingerprint`, applied to the first five lines of
      the failure text. The two ends now share one definition; no runner change was required.
    - Comparison is like-with-like: a success is only ever compared with `last_diff_hash`, a
      failure only with `last_failure_fingerprint`. The SQL writer uses
      `COALESCE($n, column)` so recording one kind never erases the other kind's identity,
      which is what previously let an intervening success hide a repeated failure.
    - Blocking now happens at the documented threshold: `NON_PROGRESS_OUTCOME_LIMIT = 4`
      identical outcomes (the counter stores repeats, so the code compares `repeats + 1`).
      Previously five outcomes were required.
    - "No diff" has exactly one encoding, SQL NULL. `_record_progress_evidence` reports only
      the column it actually wrote instead of `"" `, and `AsyncpgWorkflowPersistence.transition`
      normalises an empty string to NULL for both outcome-identity columns. `_stored_outcome`
      reads legacy `""` rows as absent.
  - Validation performed:
    - Defects reproduced first: the seven new behavioural tests in `NonProgressEvidenceTests`
      were written against the unmodified detector and all seven failed
      (`FAILED (failures=7)`; e.g. `test_zero_diff_successes_from_different_phases_do_not_collide`
      → `AssertionError: 1 != 0`, `test_identical_failures_survive_volatile_payload_fields`
      → `AssertionError: 0 != 1`, `test_four_identical_failures_block_at_the_documented_threshold`
      → `AssertionError: 0 != 1`, `test_healthy_plan_implement_pipeline_review_never_increments`
      → `AssertionError: 1 != 0`). All seven pass after the fix.
    - `test_transition_stores_an_empty_outcome_hash_as_null` likewise failed first with
      `AssertionError: '' is not None`.
    - The Python fingerprint port was cross-checked against the Go implementation itself by
      running `dispatch.FailureFingerprint` over six messages in a scratch copy of `runner/`;
      output was byte-identical, including PR #128's published value
      `agent:42051f1c5fc5560d`. Those values are pinned in
      `FailureFingerprintDefinitionTests.test_matches_the_runner_implementation`.
  - Commands executed:
    - `make test-orchestrator` — OK, 299 tests, 2 skipped.
    - `make lint` — `All checks passed!`
    - `make typecheck MYPY_CACHE=/tmp/moirai-mypy-cache-issue-101` — `Success: no issues found in 47 source files`.
    - `make test-runner` — all packages `ok` (runner untouched; run to confirm the fingerprint
      contract this change depends on still holds).
  - Notes:
    - `MYPY_CACHE` was overridden because the Makefile default `/tmp/moirai-mypy-cache` is
      shared across every worktree and `make typecheck` deletes it; concurrent agents were
      active in sibling worktrees.
    - No migration was needed: both columns and the counter already exist and keep their
      meaning.

## Decisions

- Decision: The documented threshold wins — four identical terminal outcomes block, and the code was changed to match the README rather than the README changed to match the code.
  - Context: README's "Workflow recovery guarantees" promised four identical outcomes; the code blocked at `non_progress_attempts >= 4` with a counter that starts at 0 and only increments on the *second* identical outcome, so five outcomes were actually required.
  - Alternatives considered: (a) relax the README to "five"; (b) redefine the column to count outcomes rather than repeats so `>= 4` becomes literally correct.
  - Reason: The published guarantee is the contract, and four is the more useful bound given the retry budget usually preempts the detector. (b) would have silently changed the meaning of an existing persisted column for every other reader.
  - Consequences: `NON_PROGRESS_OUTCOME_LIMIT = 4` is the single source of truth for both the comparison and the `blocking_reason` text; `non_progress_attempts` keeps its existing "repeats since the run started" meaning, so 0 still means "no repetition" and no migration or backfill is needed.

- Decision: Zero-diff successes still count toward non-progress, but a zero-diff is no longer an identity on its own.
  - Context: The issue asked whether zero-diff successes should count at all, noting that the pending evidence-gate work reclassifies a zero-diff developer "success" as a failure. Planner, pipeline and reviewer executions legitimately never change files, so under the old changed-files-only hash they all collided.
  - Alternatives considered: (a) count only failures and ignore successes entirely; (b) count successes only for mutating roles (developer, repairer); (c) keep counting all successes but widen the identity.
  - Reason: (a) would discard the genuinely useful signal of a reviewer returning the same findings or a planner returning the same rejected plan four times in a row — a real stuck loop. (b) hard-codes a role policy that the evidence gate is about to make redundant. (c) removes the false positives (role and result document are in the hash) while keeping the true positives.
  - Consequences: A repeated identical *same-role* success still blocks; a healthy plan → implement → pipeline → review sequence never increments the counter, which is covered by `test_healthy_plan_implement_pipeline_review_never_increments`. When the evidence gate lands, a zero-diff developer success simply arrives as a `failed` event and is handled by the failure path with no further change here.

## Validation Status

Record only validation that was actually run.

- Targeted tests: Passed — `orchestrator.tests.test_asyncpg_control_plane` (41) and `orchestrator.tests.test_workflow_persistence` (15).
- Service tests: Passed — `make test-orchestrator`, 299 tests, 2 skipped.
- Full repository tests: Not run (`make test-api` / `make test-web` not exercised; no Go API or web sources changed).
- Build: Not run.
- Lint: Passed — `make lint`.
- Type checks: Passed — `make typecheck MYPY_CACHE=/tmp/moirai-mypy-cache-issue-101`.
- Database migrations: Not applicable — no schema change.
- Docker Compose: Not run.
- End-to-end workflow: Not run against a live stack. The terminal-event path is covered in-process by `NonProgressEvidenceTests`, which replays whole runner-event sequences through `accept_event` against a stateful fake that reproduces the progress columns.
- Runner tests: Passed — `make test-runner` (runner unchanged).

## Next Recommended Implementation

Continue the P3 hardening track from `docs/reviews/2026-07-29-platform-review.md`: F15 (runner lifecycle hardening, issue #102) and F16 (bootstrap `NameError`, issue #103) are both unblocked and independent of this change.
## Label reconciliation data loss and nondeterministic terminal labels (issue #99, F12, 2026-07-29)

### Done

- [x] Scope label reconciliation to the `agent:*` namespace and make terminal-label convergence deterministic
  - Completed: 2026-07-29
  - Agent/session identifier: issue-99 worktree, branch `issue-99`
  - Relevant files:
    - `orchestrator/src/moirai/domain/issues.py`
    - `orchestrator/src/moirai/services/issue_sync.py`
    - `orchestrator/src/moirai/persistence/control_plane.py` (`list_latest_workflow_runs_for_project` only)
    - `orchestrator/tests/test_issues.py`, `orchestrator/tests/test_issue_sync.py`, `orchestrator/tests/test_postgres_integration.py`
    - `README.md`, `orchestrator/README.md`
  - Behavior delivered:
    - `LabelPolicy` gained `managed_prefix` (`agent:`) and a `__post_init__` invariant: every state label must live inside the managed namespace and `priority_prefix` must stay outside it. A misconfiguration that would re-introduce the data loss now fails fast.
    - `reconcile_labels(current, desired, *, managed_prefix=...)` computes removals as `(current ∩ managed_namespace) - desired`. Triage labels, `agent-priority:N`, and every other human-applied label survive a sync pass. An empty prefix is rejected instead of silently meaning "manage everything".
    - `IssueSync.reconcile_project_labels` persists the real resulting label set `(current - to_remove) | to_add` instead of overwriting the stored labels with the agent-only desired set.
    - `AsyncpgControlPlane.list_active_workflows_for_project` was renamed to `list_latest_workflow_runs_for_project` and now returns one row per issue — the newest run by `created_at` — via `SELECT DISTINCT ON (wr.issue_id) ... ORDER BY wr.issue_id ASC, wr.created_at DESC, wr.id DESC`. It also returns `created_at`, and no longer selects the unused `i.labels` column.
    - `IssueSync` additionally collapses runs per issue in `_latest_run_per_issue` so convergence does not depend on the control-plane implementation's row order.
  - Validation performed (all commands run in this worktree):
    - `make test-orchestrator` — `Ran 298 tests ... OK (skipped=3)` (baseline before the change: 288 tests, OK).
    - `make lint` — `All checks passed!`
    - `make typecheck` — `Success: no issues found in 47 source files`
    - `LOOP_TEST_DATABASE_URL=postgresql://loop:loop-test-password@127.0.0.1:55499/ci_like make test-postgres-integration` — `Ran 3 tests ... OK`, against a throwaway `postgres:16-alpine` container bound to port 55499 and removed afterwards.
  - Failing-test-first evidence (the review marks F12 *(verify)*): the new tests were run against the pre-fix sources extracted with `git archive HEAD` into a scratch tree, with only the control-plane method name adapted.
    - `test_label_reconciliation_never_removes_labels_outside_the_agent_namespace` failed with
      `removed == [('42', ['agent-priority:5', 'agent:ready', 'bug', 'enhancement', 'needs-design'])]`.
    - `test_repeated_sync_cycles_preserve_user_priority_and_triage_labels` failed with `priority 0 != 10` on the second sync cycle, confirming that deleting `agent-priority:10` resets the issue priority to `LabelPolicy.default_priority`.
    - `test_label_reconciliation_converges_on_the_newest_run_in_any_order` failed in both list orders; with `[run-new, run-old]` the pre-fix code applied `agent:blocked` last, overwriting `agent:delivered`.
    - `test_label_reconciliation_reads_only_the_newest_workflow_run_per_issue` failed against the pre-fix SQL on a real database: the issue whose newest run was `completed` resolved to `blocked`.
  - Notes: acceptance criteria met — a sync pass never removes a label outside `agent:*`, and terminal-label convergence is deterministic under multiple historical runs.

### Decisions

- Decision: rename `list_active_workflows_for_project` to `list_latest_workflow_runs_for_project` and return the newest workflow run per issue, rather than adding an "active" status filter.
  - Context: the method never filtered by status despite its name, and ordered by `wr.id ASC` (random UUIDs). Label reconciliation processed every historical run for an issue, so the last row processed decided the label.
  - Alternatives considered: (a) filter to non-terminal statuses only — rejected, because reconciliation exists precisely to converge the terminal `agent:blocked` / `agent:delivered` labels, so an "active-only" filter would remove the feature; (b) prefer non-terminal runs and fall back to the latest terminal run — rejected as more machinery for a case that only arises from stale rows, where the newest run is still the honest answer; (c) keep the name and document the behavior — rejected, a name that says "active" while returning everything is how this defect stayed invisible.
  - Reason: an issue's newest workflow run is its current truth. `DISTINCT ON (wr.issue_id)` with `ORDER BY wr.issue_id, wr.created_at DESC, wr.id DESC` is total, so the result is deterministic even when two runs share a `created_at`. The new name states exactly what the query returns.
  - Consequences: one row per issue instead of every historical run, so reconciliation also issues fewer GitHub calls. Callers must use the new name; the only caller is `IssueSync.reconcile_project_labels`. `created_at` is now part of the returned payload.

- Decision: keep the per-issue collapse in `IssueSync._latest_run_per_issue` even though the SQL already returns one row per issue.
  - Context: `IssueSync` takes an untyped `control_plane`, so the guarantee would otherwise rest on one implementation.
  - Alternatives considered: rely on the query alone — rejected, because the regression test for deterministic convergence would then only run in the Postgres integration job.
  - Reason: label reconciliation mutates a live repository; the determinism guarantee should hold for any control-plane implementation and be covered by the fast unit suite.
  - Consequences: about ten lines of defensive code whose purpose is documented at the call site.

### Validation Status

Record only validation that was actually run.

- Targeted tests: Passed — `PYTHONPATH=orchestrator/src .venv/bin/python3 -m unittest discover -s orchestrator/tests -p 'test_issue*.py'`: 31 tests, OK.
- Service tests: Passed — `make test-orchestrator`: 298 tests, OK (skipped=3).
- Full repository tests: Not run (`make test` also builds the Go and web modules, untouched here).
- Build: Not run.
- Lint: Passed — `make lint`.
- Type checks: Passed — `make typecheck`.
- Database migrations: Applied by the integration run; no new migration was required.
- Docker Compose: Not run.
- End-to-end workflow: Not run.

### Known Issues

- Issue: `test_control_plane_persists_project_runner_issue_and_lease_lifecycle` assumes a pristine database.
  - Severity: P3
  - Impact: `make test-postgres-integration` only passes on the first run against a given database; a second run fails on `list_projects() == [project]`.
  - Evidence: pre-existing at `HEAD` — the unmodified test file from `git show HEAD:orchestrator/tests/test_postgres_integration.py` passes on run 1 and fails on run 2 against the same fresh database. CI creates a new Postgres service container per job, so it is green there. The new `test_label_reconciliation_reads_only_the_newest_workflow_run_per_issue` deletes its own project, issues, and workflow runs, and passes on repeated runs.
  - Suggested resolution: scope the assertion to the project the test created, or truncate `app` tables in `asyncSetUp`.

### Next Recommended Implementation

Pick up the next *(verify)* finding from `docs/reviews/2026-07-29-platform-review.md` that no other agent has claimed — F10 (`blocked` agent results flattened to `failed` in `runner/internal/dispatch/control_loop.go`) is the highest-value remaining one, since it makes the orchestrator's planner-`blocked` handling unreachable.
# Session: F6 / issue #93 — Runner terminal-event durability (branch `issue-93`)

## Current Status

- Overall status: Complete for the runner side of finding F6.
- Current phase: Bug fix from the 2026-07-29 platform review (`docs/reviews/2026-07-29-platform-review.md`, F6, P1, marked *(verify)*).
- Active implementation: issue-93 agent session, 2026-07-29 — runner terminal-event loss.
- Last updated: 2026-07-29.
- Agent/session identifier: issue-93.

## Done

- [x] Runner: terminal execution events are no longer silently lost
  - Completed: 2026-07-29.
  - Relevant files: `runner/internal/control/events.go`, `runner/internal/control/events_test.go`, `runner/internal/dispatch/control_loop.go`, `runner/internal/dispatch/control_loop_test.go`, `runner/README.md`.
  - Behavior delivered:
    - `EventReporter.Abandon` (lease expiry) now discards only a job's `log`/`progress` events. Its terminal events stay in the pending queue and in the crash-safe outbox, still fenced by their lease generation.
    - `Abandon` retains the expired lease in a separate `expired` map so a still-running execution can still sequence, persist, and attempt delivery of its terminal event. Non-terminal emits against a retained lease keep returning `ErrNoActiveEventLease`. `Finish` clears the retained record.
    - `Emit` no longer rejects a lifecycle event because chatty log output filled the shared buffer. Events carry a priority (terminal 2 > `started` 1 > `log`/`progress` 0); a higher-priority event evicts the oldest queued event of the lowest priority below it. Terminal events are never evicted. `log`/`progress` never evict anything and are still rejected with the new exported `control.ErrEventBufferFull`.
    - `ControlLoop.execute` no longer discards emit errors with `_, _ =`. The new `emitEvent` helper distinguishes "queued but undelivered" (`WARN`, retried from the outbox on reconnect) from "never queued" — a lost terminal event is logged at `ERROR` with `msg="terminal execution event lost"`, job id, execution id, lease generation, event type, and reason.
    - The effective event buffer is raised to twice the runner capacity when configured lower, covering each running execution plus one whose lease expired while still winding down. It is a floor, not a guarantee; a longer pile-up is reported by the error log rather than passing silently.
    - A terminal event that cannot be queued is retried once stripped to its classification fields (`status`, `exitCode`, `error`, `failureFingerprint`, `durationMs`, `branch`), each string truncated to 2 KiB on a UTF-8 rune boundary, so neither an oversized `result.Raw` document nor an unbounded `error` string costs the run its outcome.
    - `Reporter.Finish` is now deferred in `execute` and its `false` return is logged, so a panic while building a terminal payload cannot leak the retained lease record.
  - Validation performed: failing tests were written and confirmed failing before the fix, then confirmed passing after it; full runner suite with the race detector; `gofmt`; `go vet`.
  - Commands executed:
    - Before the fix (all reproductions failed):
      `cd runner && go test -race ./internal/control/ -run 'TestEventReporterKeepsTerminalEventsWhenLeaseExpires|TestEventReporterQueuesTerminalEventEmittedAfterLeaseExpiry|TestEventReporterEvictsDroppableEventsForTerminalEvent' -v`
      → `pending events after lease expiry = 0, want 1 terminal event`; `Emit(failed) after expiry = (0, runner event lease is not active)`; `Emit(completed) on a full buffer = (0, execution event buffer is full)`.
      `cd runner && go test -race ./internal/dispatch/ -run 'TestControlLoopDeliversTerminalEventAfterLeaseExpiryWhileDisconnected|TestControlLoopLogsTerminalEventLoss' -v`
      → `pending events = 0, want 1`; `terminal event loss was not logged`.
      `TestControlLoopDeliversTerminalEventAfterLogsSaturateTheBufferWhileDisconnected` was additionally checked against the pre-fix buffer behavior → `event outbox never contained a "completed" event`.
    - After the fix: `make test-runner` → all 10 packages `ok`; `cd runner && gofmt -l .` → no output; `cd runner && go vet ./...` → no output.
  - Notes: Tests added — `TestEventReporterKeepsTerminalEventsWhenLeaseExpires`, `TestEventReporterQueuesTerminalEventEmittedAfterLeaseExpiry`, `TestEventReporterEvictsDroppableEventsForTerminalEvent`, `TestEventReporterRejectsTerminalEventWhenBufferHoldsOnlyTerminalEvents`, `TestControlLoopDeliversTerminalEventAfterLeaseExpiryWhileDisconnected`, `TestControlLoopDeliversTerminalEventAfterLogsSaturateTheBufferWhileDisconnected`, `TestControlLoopLogsTerminalEventLoss`. `TestEventReporterRejectsStaleLeaseAndBoundsPendingEvents` was updated: a post-expiry terminal emit is now accepted, so it asserts the `ErrEventBufferFull` sentinel and the retained-lease rules instead.

## Post-review corrections

A `silent-failure-hunter` review of the first commit (`bc09597`) found ten issues; the material ones were fixed in a follow-up commit on the same PR, each with a test that was confirmed failing first.

- Critical — `expired` was keyed by job ID alone, so a job re-offered at the next generation clobbered the retained record of the superseded execution, and that execution's terminal `Emit` then returned `ErrStaleEventLease`. This reintroduced the exact loss #93 exists to fix. Now keyed by `(jobID, generation)`. Test: `TestEventReporterRetainsEveryExpiredGenerationOfAJob` (failed with `Emit(failed, gen 2) = (0, runner event lease is stale)`).
- High — `makeRoomLocked` evicted a queued event but the persist-failure rollback in `Emit` never restored it, so a failed emit silently destroyed a log event and diverged memory from the on-disk outbox. The victim and its index are now returned and re-inserted, and the "discarded" warning moved after a successful persist so it can no longer report a trade that did not happen. Test: `TestEventReporterRestoresEvictedEventWhenPersistFails` (failed with `pending events = 1`).
- High — the `eventBufferSize >= capacity` floor did not provide the invariant its comment claimed, because `control.OfferState.Expire` frees the capacity slot while the abandoned execution still owes a terminal event. Raised to `2*capacity` and the comment corrected to state it is a floor, not a guarantee.
- High/Medium — `Reporter.Finish`'s return value was discarded one line below the fix that removed exactly that pattern from `Emit`, and it was not deferred. Now `defer`red with a warning on `false`.
- Medium (fixed in two passes) — terminal loss was logged but unmitigated. The most likely real-world trigger is the 16 KiB payload limit, since `terminalPayload` embeds `changedFiles`, `commandsRun`, and the whole `result.Raw` document. `emitEvent` now retries once with a minimal payload. Test: `TestControlLoopRescuesOversizedTerminalEventWithMinimalPayload` (without the retry: `timed out waiting for execution events` — the outcome was lost outright).
- Second review pass — the first version of that retry was incomplete: `minimalTerminalPayload` whitelisted `error` verbatim, and `control_loop.go` sets it to `failure.Error()`, which is unbounded. A `failed` result whose error string alone exceeds 16 KiB still failed the retry and landed in "terminal execution event lost" — precisely the case the retry exists for, on the terminal type most likely to carry a huge string (a wedged agent dumping stderr into the returned error). String fields in the reduced payload are now truncated to 2 KiB on a UTF-8 rune boundary with a `… [truncated]` marker. Test: `TestControlLoopRescuesTerminalEventWithOversizedErrorText` (failed with `ERROR terminal execution event lost ... error="execution event payload is too large"`).
- Also tightened the "is this actually a reduction?" guard from a key-count comparison to a set comparison plus a truncation flag, so a same-sized disjoint payload is no longer skipped. Test: `TestMinimalTerminalPayloadReducesOnlyWhenSomethingChanges`, plus `TestTruncateUTF8StopsOnARuneBoundary`.
- Corrected an overstated doc comment: "reached the crash-safe outbox" only holds when an outbox path is configured; `NewControlLoop` and `NewControlLoopWithRedaction` pass `""`.
- Accepted as-is: cross-job eviction of another job's `started` (the orchestrator tolerates a terminal event arriving while the job row is still `preparing`), and the cosmetic case where an in-flight event chosen as an eviction victim is both delivered and logged as discarded.

## Decisions

- Decision: Keep terminal events on lease expiry rather than purging the job's whole queue.
  - Context: `Reporter.Abandon` rewrote the outbox without the job's events, and lease expiry happens precisely when the stream is down and events are queued.
  - Alternatives considered: Keep purging and rely on the orchestrator's lease-expiry recovery.
  - Reason: Recovery discards the run's actual outcome, which breaks the "orchestrator is authoritative, runner reports truth" contract. Terminal events remain fenced by lease generation, so keeping them costs the orchestrator nothing.
  - Consequences: The runner briefly retains an expired lease (until `Finish`) so the in-flight execution can report its outcome. Sequence numbers can now contain gaps where log events were discarded; the orchestrator only requires strictly increasing sequences, so gaps are safe.

- Decision: Priority-based eviction instead of a second queue for lifecycle events.
  - Context: Log and terminal events shared one `maxPending` budget, so a chatty agent during a disconnect starved the terminal emit.
  - Alternatives considered: A separate small queue reserved for `started`/terminal events.
  - Reason: A single queue preserves global send ordering and one durable outbox format; a priority ladder gets the same guarantee with far less machinery.
  - Consequences: Log output can be discarded under buffer pressure; each discard is logged at `WARN` with the job, execution, discarded type, and sequence.

- Decision (step 4 of the issue — analysis only, orchestrator not modified this session): the orchestrator should accept a late terminal event for an expired-lease job as informational.
  - Context: `domain/leases.py:accept_event` raises `StaleLeaseError` when `lease.expires_at <= now`, and `grpc/runner_control.py` turns that into a `_StreamFailure` that `context.abort`s the whole bidirectional stream.
  - Evidence: `orchestrator/src/moirai/domain/leases.py:17-25`, `orchestrator/src/moirai/grpc/runner_control.py:256-276`.
  - Recommendation: (a) keep generation fencing exactly as it is; (b) for a terminal event whose generation matches but whose lease has expired, record the reported outcome as informational instead of discarding it, so lease-expiry recovery can use the real result; and (c) regardless of (b), stop aborting the stream on a rejected event — reject the single event and keep the session, otherwise a late terminal event costs the runner a full reconnect cycle. The runner change is safe without any of this: `Client.SendExecutionEvent` treats a successful stream `Send` as delivery, so a rejected event is dropped from the outbox rather than becoming a poison pill.
  - Consequences: Until the orchestrator side lands, an expired-lease terminal event is durably queued and delivered once, and the orchestrator drops it and tears the stream down once.

## Validation Status

- Targeted tests: Passed — `cd runner && go test -race ./internal/control/ ./internal/dispatch/`.
- Service tests: Passed — `make test-runner` (`cd runner && go test -race ./...`), 10 packages `ok`.
- Full repository tests: Not run — this change is confined to the Go runner.
- Build: Not run separately; `go test -race ./...` compiles every runner package.
- Lint: Passed — `cd runner && gofmt -l .` (no output) and `cd runner && go vet ./...` (no output). These are the runner's CI checks (`.github/workflows/ci.yml`, `runner` job).
- Type checks: Not applicable — no Python changed. `make lint` / `make typecheck` were deliberately not run because they touch the shared `/tmp/moirai-mypy-cache` while other agents work in sibling worktrees.
- Database migrations: Not applicable.
- Docker Compose: Not run — no Compose or configuration change.
- End-to-end workflow: Not run.

## Known Issues

- Issue: An expired-lease terminal event is *guaranteed* to be rejected by the orchestrator, and the rejection aborts the control stream.
  - Severity: P2
  - Impact: The runner now durably records and delivers the outcome, but the orchestrator discards it and the runner pays one reconnect. `expire_leases` (`persistence/control_plane.py:1225-1229`) sets `status='recovering'` and `lease_generation + 1` for exactly the leases the runner is abandoning, so all three predicates of the `accept_event` guard (`:1490-1492`) fail at once — rejection is certain, not merely likely. The runner cannot observe this: `Client.SendExecutionEvent` is a bare `stream.Send` that returns nil on hand-off, so the event is dropped from the outbox regardless. Issue #93 step 3 explicitly asks the runner to persist and attempt delivery, so this is the intended runner-side behavior pending the orchestrator half.
  - Evidence: `orchestrator/src/moirai/domain/leases.py:20-21` and `orchestrator/src/moirai/grpc/runner_control.py:159-160, 274-276`.
  - Suggested resolution: Implement step 4 of issue #93 on the orchestrator side as described under Decisions.

## Next Recommended Implementation

Implement the orchestrator half of issue #93 step 4 in `orchestrator/src/moirai/domain/leases.py` and `orchestrator/src/moirai/grpc/runner_control.py`: keep generation fencing, accept a generation-matching terminal event for an expired lease as informational so lease-expiry recovery can use the real outcome, and reject a single bad event without aborting the runner's control stream. Targeted validation: `make test-orchestrator`, plus new cases in `orchestrator/tests/test_leases.py` and `orchestrator/tests/test_control_plane.py`.
