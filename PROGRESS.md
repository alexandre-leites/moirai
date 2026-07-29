# Implementation Progress

## Current Status

- Overall status: #109 implemented; branch `issue-109` pushed and PR opened
- Current phase: Resolve GitHub issue #109 (P0 runner cannot authenticate to GitHub)
- Active implementation: None
- Last updated: 2026-07-29
- Agent/session identifier: agent/issue-109-runner-credentials

## Done

- [x] Populate task-packet `environmentRefs` and thread the credential through the runner's Git path (#109)
  - Completed: 2026-07-29
  - Relevant files: `orchestrator/src/moirai/workflows/task_packets.py`,
    `orchestrator/tests/test_task_packets.py`, `orchestrator/tests/test_asyncpg_control_plane.py`,
    `orchestrator/tests/test_end_to_end.py`, `runner/internal/dispatch/dispatch.go`,
    `runner/internal/repository/manager.go`, `runner/internal/repository/delivery.go`,
    `runner/internal/config/config.go`, `runner/cmd/runner/main.go`,
    `compose.yaml`, `.env.example`, `README.md`, `runner/README.md`
  - Behavior delivered:
    - `build_task_packet` emits real `environmentRefs` instead of a hardcoded `[]`.
      `TaskExecutionRequest` carries `environment_refs`, and `environment_refs_for`
      declares `GITHUB_TOKEN` (secretRef `github_token`) for any role that may push
      and for every `managed_clone` repository. Emission is validated for name
      pattern, uniqueness, `secretRef` shape, and count, matching the Go validator.
    - The runner resolves the task environment *before* `Workspaces.Prepare`, so
      `git clone --mirror` and `git fetch` receive the same credential the later
      `git push` does. `repository.PrepareRequest` gained an `Environment` field and
      `prepareSource` runs its networked Git commands through `gitWithEnv`.
    - `pushEnvironment` became `credentialEnvironment`, now shared by clone, fetch,
      and push; it injects `GIT_CONFIG_KEY_0=http.https://github.com/.extraheader`
      so the token never appears in an argument list.
    - `osEnvironmentResolver` resolves a reference from the plain variable or the
      Docker-style `<NAME>_FILE` path, via the new `config.SecretValue`, so a Compose
      secret can back the credential.
    - An unresolvable or disallowed reference fails the execution before the
      workspace exists; the control loop reports a terminal `failed` event whose
      `error` payload names the variable, and no unauthenticated push is attempted.
    - Compose mounts the shared `github_token` secret into the runner and sets
      `LOOP_RUNNER_ALLOWED_ENVIRONMENT=GITHUB_TOKEN`. No token value is committed;
      `.env.example` carries only the allow-list name.
  - Validation performed: 292 orchestrator tests, full runner race suite, Ruff,
    Mypy, and Compose rendering (runner service shows the `github_token` secret and
    `LOOP_RUNNER_ALLOWED_ENVIRONMENT: GITHUB_TOKEN`).
  - Commands executed: `make test-orchestrator`; `make test-runner`; `make lint`;
    `make typecheck`; `make compose`; `cd runner && gofmt -l . && go vet ./...`.
  - Notes: pipeline-command environment isolation stays open as #122; this change is
    the dependency it names.

## Decisions

- Decision: declare `GITHUB_TOKEN` for any role that may push and for every `managed_clone` packet.
  - Context: the runner clones and fetches from the code host during workspace preparation, and pushes during delivery.
  - Alternatives considered: declaring the credential on every packet regardless of mode; sniffing the repository URL scheme.
  - Reason: `managed_clone` is the GitHub-backed mode and always reaches the network, while a read-only `existing_path` role works inside an operator-provided checkout and should not receive a credential. URL sniffing would encode code-host specifics in the orchestrator, which #109 puts out of scope.
  - Consequences: a runner serving `managed_clone` projects must have `GITHUB_TOKEN` allowed and configured; otherwise every such packet fails loudly, which is the requested behavior.

- Decision: resolve the credential from `<NAME>_FILE` as well as the plain variable.
  - Context: Compose delivers secrets as mounted files, but `osEnvironmentResolver` only read `os.LookupEnv`.
  - Alternatives considered: passing the token to the runner as a plain Compose environment variable.
  - Reason: a plain variable would put the token in `docker inspect` and in the Compose file or `.env`, which the repository explicitly forbids.
  - Consequences: the runner reads `/run/secrets/github_token` on demand; the file indirection is documented in `runner/README.md`.

- Overall status: Complete; PR #74 rebased onto current main and awaiting CI
- Current phase: Resolve GitHub issues #40 and #70
- Active implementation: None
- Last updated: 2026-07-28
- Agent/session identifier: agent/docs-contracts
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

# Session: F8 / issue #95 — Runner: expire pending offer reservations and wire the unused OfferTimeout (branch `issue-95`)

## Current Status

Complete. Branch `issue-95`, based on `main` at `bb1e876` and merged with `origin/main` at `a9a1d3b` (which brought in #127, #129, #130, #131, #132). The runner now ages out accepted-but-unacknowledged offers, counts them against its own capacity, and consumes `Config.OfferTimeout`.

## Done

- Issue #95 (platform review finding F8, marked *(verify)*): a pending offer reservation that never received a `LeaseAcknowledged` held a runner capacity slot forever.
  - Root cause: `control.OfferState.Admit` inserted the accepted offer into `s.pending`; `Expire()` swept only `s.active` and `Abandon` ran post-execution, so nothing ever aged `s.pending` out. `ControlLoop.Busy()` reported `ActiveCount() >= Capacity()` while `Admit` rejected on `len(pending)+len(active) >= capacity`, so with capacity 1 a single lost acknowledgement made the runner refuse all work while its heartbeat still said idle. `Config.OfferTimeout` was parsed and validated but had no consumer anywhere in the tree.
  - Reproduced first, before any fix (`make test-runner` equivalent, `go test -race ./internal/dispatch/`):
    - `control_loop_offer_expiry_test.go:69: heartbeat advertises the runner as idle while an unacknowledged reservation holds the only capacity slot`
    - with that assertion removed so the capacity leak shows on its own: `control_loop_offer_expiry_test.go:77: accepted = []string{"job-1"}, want job-3 admitted after the reservation timed out`
    - `control_loop_offer_expiry_test.go:116: late acknowledgement resurrected an expired offer reservation`
  - `runner/internal/control/offer.go`: `pendingOffer` carries `reservedAt`; new `OfferState.ExpirePending(timeout) []Reservation` releases reservations past the timeout; new exported `Reservation` type and `DefaultOfferTimeout` constant; new `OfferState.ReservedCount()`.
  - `runner/internal/dispatch/control_loop.go`: new `ControlLoop.OfferTimeout` field, swept by the existing `expire()` (ticker every `ExpiryInterval`, plus every heartbeat via `Reconcile`); `Busy()` now uses `ReservedCount()`; an acknowledgement that matches nothing is logged instead of dropped silently.
  - `runner/cmd/runner/main.go`: `loop.OfferTimeout = settings.OfferTimeout` — the consumer `Config.OfferTimeout` never had.
  - Docs: `runner/README.md` (`LOOP_RUNNER_OFFER_TIMEOUT` and `LOOP_RUNNER_HEARTBEAT_INTERVAL` rows) and `docs/architecture.md` ("Job offer lifecycle") now describe the runner half of offer reclamation.
  - Acceptance criteria: (1) with capacity 1 an unacknowledged offer leaves the runner accepting new offers — `TestControlLoopReleasesCapacityWhenOfferIsNeverAcknowledged`; (2) `grep -rn OfferTimeout --include='*.go' runner/ | grep -v _test.go` now shows `runner/cmd/runner/main.go:238` alongside the config definition.
  - Notes: tests added — `TestControlLoopReleasesCapacityWhenOfferIsNeverAcknowledged`, `TestControlLoopHonorsConfiguredOfferTimeout`, `TestControlLoopRejectsLeaseAcknowledgementForExpiredOffer` (new file `runner/internal/dispatch/control_loop_offer_expiry_test.go`), and `TestOfferStateExpiresUnacknowledgedReservationAndFreesCapacity`, `TestOfferStateRejectsAcknowledgementForExpiredReservation`, `TestOfferStateKeepsAcknowledgedLeaseThroughPendingExpirySweep`, `TestOfferStateExpiresOnlyTheReservationsPastTheTimeout`, `TestOfferStateStartsTheAcknowledgementWaitWhenTheAcceptanceIsSent`, `TestOfferStateExpirePendingFallsBackToTheDefaultTimeout` in `runner/internal/control/offer_test.go`. No existing test was modified.

## Post-review corrections

An adversarial review of the first commit confirmed the mechanism (no double-release, no un-released slot, no resurrection, expiry composes with the retained-lease logic from #93/PR #134 because a reservation never has a `leaseState` to retain) but found that the *justification* written into the comments was wrong on two counts, plus one new race. All were fixed before the PR was opened.

- The comment and `docs/architecture.md` claimed the orchestrator ages the same offer out through `app.job_offers.expires_at`. It does not: `accept_offer` sets that row to `'accepted'` (`persistence/control_plane.py`), and `expire_offers` matches `status = 'offered'` only. The job is reclaimed by `expire_leases` against `app.jobs.lease_expires_at` on the scheduler's offer TTL, hardcoded to 600s in `orchestrator/src/moirai/main.py`, which also marks the runner offline until its next heartbeat. The *conclusion* — do not send an offer rejection — still holds and is now stated with the real reason (`reject_offer` on an accepted offer raises `OfferError` → `FAILED_PRECONDITION` → an aborted control stream). The consequent claim that a late acknowledgement would start "an execution the orchestrator has already reassigned" was also false at 30s and was corrected.
- The doc implied the `Busy()` change has a scheduling effect. It does not today: the orchestrator never reads `Heartbeat.busy` (zero references to `busy` in `orchestrator/src/moirai/`), and placement is gated on its own `app.jobs` count against `runners.capacity`. Recorded as an internal consistency guarantee instead.
- New race introduced by the first commit: `reservedAt` was stamped before `client.AcceptOffer`, which sends on the control stream outside the state lock. A send that blocked for longer than `OfferTimeout` could have its reservation swept mid-flight and then succeed, leaving the orchestrator believing the runner accepted while the runner held nothing. The wait is now re-stamped after a successful `AcceptOffer` (guarded by pointer identity so a send that outlived its own timeout cannot re-stamp a newer reservation for the same job), because an acknowledgement cannot arrive before the acceptance is on the wire. The creation-time stamp is deliberately kept as the initial value so an `AcceptOffer` that never returns still cannot hold the slot forever — that is the leak this change exists to prevent. Test: `TestOfferStateStartsTheAcknowledgementWaitWhenTheAcceptanceIsSent` (without the re-stamp: `ExpirePending() = [...ReservedAt:12:00:00...], want the wait measured from the acceptance`).
- The new "unmatched acknowledgement" warning fired on two benign paths — a duplicate/non-advancing renewal acknowledgement for a lease the runner still holds, and a renewal acknowledgement landing after `execute()` calls `Abandon`. It is now gated on the runner not holding that job at that generation, and the message no longer asserts a cause it cannot know.
- Two test comments overclaimed (one said it proved the `main.go` assignment, which it does not exercise; one gave a false reason for the mutex on the test clock). Both corrected rather than deleted.
- Coverage gaps the review named were closed: capacity > 1 with a sweep that expires some but not all reservations (`TestOfferStateExpiresOnlyTheReservationsPastTheTimeout`), and the concurrent accept/expiry window above.

## Decisions

- Decision: `Busy()` counts pending reservations, rather than leaving it on `ActiveCount()` once expiry bounds the leak.
  - Context: step 3 of the issue offers both. `Admit` rejects on `len(pending)+len(active) >= capacity`; `Busy()` reported `ActiveCount() >= Capacity()`. Expiry bounds the divergence to one `OfferTimeout` but does not remove it.
  - Alternatives considered: keep `Busy()` as-is and rely on expiry. That leaves a window in which the runner's only public availability signal contradicts the predicate it actually admits on, and re-opens the full inconsistency the moment any future code path holds a reservation for longer.
  - Reason: one predicate, one answer. `ReservedCount() >= Capacity()` is literally the condition under which the next offer is refused, so the signal cannot disagree with the behaviour by construction. It costs one map length.
  - Consequences: `Busy()` is true for the whole reservation window, not just while an execution runs. `WaitForIdle` is unaffected (it keys off in-flight executions, not capacity), drain is unaffected, and every pre-existing `Busy()` assertion still passes. It has no effect on scheduling today because the orchestrator ignores `Heartbeat.busy`; it is a correctness guarantee for whenever that field is read, and for any in-process consumer.

- Decision: expiry is local — the runner never tells the orchestrator that a reservation lapsed.
  - Context: the runner has already sent `AcceptOffer`, so the orchestrator's offer row is `accepted` and its job is `preparing`.
  - Alternatives considered: send `RejectOffer` on expiry so the job can be reassigned promptly.
  - Reason: `reject_offer` requires `status = 'offered'`; on an accepted offer it raises `OfferError`, which `grpc/runner_control.py` turns into a `FAILED_PRECONDITION` that aborts the whole bidirectional stream. Freeing a slot would cost a full reconnect and every queued event's delivery attempt.
  - Consequences: the runner frees its slot in ~`OfferTimeout` while the orchestrator holds the job until `expire_leases` fires at the 600s offer TTL. The two clocks are intentionally unequal — the runner's job is to keep taking work, the orchestrator's is to decide when a job needs recovering — but the gap means a job lost to a dropped acknowledgement waits out the orchestrator's TTL before it is re-offered. See Known Issues.

- Decision: `ControlLoop.OfferTimeout` is an assignable field with a `control.DefaultOfferTimeout` fallback, and `ExpirePending` clamps a non-positive timeout to that same default.
  - Context: `OfferState` is built inside `NewControlLoopWithEventBuffer`, whose signature already has nine parameters; `ReconnectMin`, `ReconnectMax`, and `ExpiryInterval` are all late-bound fields set by `main.go`.
  - Alternatives considered: a tenth constructor parameter or another `NewControlLoopWith…` variant.
  - Reason: it matches the tuning pattern already in the file and leaves every existing call site compiling unchanged.
  - Consequences: a caller that forgets the field still gets expiry at 30s rather than the old unbounded behaviour, and a zero or negative configured value cannot silently disable expiry.

## Validation Status

- Targeted tests: Passed — `cd runner && go test -race ./internal/control/ ./internal/dispatch/` → both `ok`.
- Service tests: Passed — `make test-runner` (`cd runner && go test -race ./...`), 10 packages `ok`, 1 with no test files.
- Failing-test-first evidence: recorded under Done. Additionally mutation-checked — reverting `Busy()` to `ActiveCount()` fails 2 tests, removing the `ExpirePending` call from `expire()` fails 3, flipping the expiry boundary comparison fails 3, and removing the post-accept re-stamp fails 1.
- Build: Not run separately; `go test -race ./...` compiles every runner package.
- Lint: Passed — `cd runner && gofmt -l .` (no output) and `cd runner && go vet ./...` (no output). These are the runner's CI checks.
- Type checks: Not applicable — no Python changed. `make lint` / `make typecheck` deliberately not run; they share `/tmp/moirai-mypy-cache` with sibling worktrees.
- Database migrations: Not applicable.
- Docker Compose: Not run — no Compose or configuration-file change. `LOOP_RUNNER_OFFER_TIMEOUT` was already parsed and documented; only its consumer is new.
- End-to-end workflow: Not run.

## Known Issues

- Issue: the runner and the orchestrator reclaim a dropped acknowledgement on very different clocks — ~30s versus 600s.
  - Severity: P3
  - Impact: the runner is back in service quickly (the point of this issue), but the job it accepted stays `preparing` until `expire_leases` fires at the scheduler's offer TTL, and that sweep also flips the runner to `offline` until its next heartbeat. The work is not lost, only delayed.
  - Evidence: `orchestrator/src/moirai/main.py` constructs `Scheduler(..., timedelta(seconds=600))`; `persistence/control_plane.py` `expire_leases` matches `status IN ('preparing','running') AND lease_expires_at <= now` and then runs `UPDATE app.runners SET status = 'offline' WHERE id = $1`.
  - Suggested resolution: orchestrator-side, either shorten the acceptance-to-acknowledgement window separately from the execution lease TTL, or accept an explicit runner signal for "reservation lapsed" that does not reuse `reject_offer`'s `status = 'offered'` precondition. Both are orchestrator changes and out of scope here.

- Issue: an `AcceptOffer` send that blocks for longer than `OfferTimeout` and then succeeds leaves a phantom acceptance.
  - Severity: P4
  - Impact: the reservation is swept while the send is in flight, so the acceptance reaches the orchestrator but the runner holds no slot and discards the acknowledgement. Bounded by the orchestrator's lease expiry, and requires a control stream wedged for longer than the whole offer timeout — in which case `Disconnect` normally makes the send fail instead, taking the clean rollback path.
  - Evidence: `runner/internal/control/offer.go` `Admit` calls `s.client.AcceptOffer` outside `s.mu` by design (`TestOfferStateAcceptsOutsideStateLock`).
  - Suggested resolution: only if it is ever observed. Closing it fully means either holding the state lock across a network send or making a reservation un-expirable while its accept is in flight; both trade a bounded, rare stall for an unbounded one.

## Next Recommended Implementation

Unchanged from the previous session: the orchestrator half of issue #93 step 4.

# Session: F3 / issue #90 — Always run the local pipeline; stop deriving `pipeline_passed` from the developer exit code (branch `issue-90`)

## Current Status

- Overall status: Complete for issue #90.
- Current phase: Bug fix from the 2026-07-29 platform review (`docs/reviews/2026-07-29-platform-review.md`, F3, P1).
- Active implementation: issue-90 agent session, 2026-07-29.
- Branch `issue-90`, based on `main` at `8956d84`.
- Last updated: 2026-07-29.

## Done

- [x] Issue #90 (finding F3): `pipeline_passed` was inferred from the developer agent's exit code, and the `pipeline` node skipped dispatching a real pipeline execution whenever that inferred flag was already `True`.
  - Root cause: two places conspired. `workflows/runner_events.py` translated a developer `completed` event while `implementing` into `{"status": "local_pipeline", "pipeline_passed": summary.exit_code == 0}`, and `workflows/nodes.py::pipeline` short-circuited with `if state.get("pipeline_passed") is True:` instead of dispatching. The deterministic completion gate therefore ran only when the coding agent had already failed — exactly backwards. Combined with the (since-fixed) runner result-document bug (#89), an agent that did nothing exited 0 and sailed into AI review and PR creation. A second, quieter consequence: after a review-driven repair the stale `pipeline_passed = True` from the pre-repair run also skipped the pipeline, so repaired work never got a pipeline verdict of its own.
  - `orchestrator/src/moirai/workflows/runner_events.py`: the developer `implementing` branch now returns `{"status": "local_pipeline"}` with the gate untouched. The `pipeline` branch is unchanged behaviourally and is now the sole writer of `pipeline_passed` (its redundant `and summary.terminal` was dropped — `_terminal_event_transition` already returns `None` for non-terminal events). The `repairer` branch was verified to already leave the gate alone (issue step 3) and is now commented to say why.
  - `orchestrator/src/moirai/workflows/nodes.py`: `pipeline` is unconditional — every entry into the phase dispatches a real pipeline execution. The node neither reads nor writes `pipeline_passed`. `_dispatch` also stops counting the pipeline against `total_agent_executions` (new `_NON_AGENT_ROLES`); see the Decisions section — this is a regression fix for the first version of this change, not an unrelated tweak.
  - Docs: `orchestrator/README.md` gains a "Gate ownership" section (who decides each gate, why `pipeline_passed` is the strictest, the budget rule, and the two non-orchestrator caveats below); top-level `README.md` gains a matching bullet under "Workflow recovery guarantees".
  - Composition with #88/PR #130 (event-driven graph, already on `main`): a developer terminal event clears `awaiting_execution`, the graph resumes on the `implement` edge, `pipeline` dispatches and re-sets `awaiting_execution`, and the invocation ends. The pipeline execution's own terminal event is what supplies the gate and resumes the run. No change was needed to `suspend_after_dispatch`.
  - Acceptance criteria:
    1. *Only the pipeline-role event branch writes `pipeline_passed`.* Met. `grep -rn '"pipeline_passed":' orchestrator/src` → exactly one hit, `orchestrator/src/moirai/workflows/runner_events.py:212`, inside `if resolved_role == "pipeline":`. Before the change the same grep returned three hits (`runner_events.py` ×2, `nodes.py` ×1). Also asserted behaviourally by `test_only_the_pipeline_role_writes_the_pipeline_gate`, which drives every role/status pair through `workflow_transition_for_terminal_event` and requires the key to be absent for all of them except `pipeline`.
    2. *A developer completion always results in a dispatched pipeline execution.* Met. `test_a_clean_developer_exit_cannot_reach_review_without_the_pipeline` drives the real compiled graph: after a developer `completed` with `exitCode 0`, the queued roles are `["planner", "developer", "pipeline"]`, `pipeline_passed` is absent from the graph state, no reviewer was ever queued, and the run is suspended. A subsequent *failed* pipeline event routes to `repairing`, never to review.
  - Notes: 8 tests added — `test_a_clean_developer_exit_cannot_reach_review_without_the_pipeline`, `test_a_repair_gets_its_own_pipeline_execution_despite_an_earlier_pass`, `test_full_review_cycle_still_reaches_delivery_on_the_agent_budget` (`orchestrator/tests/test_end_to_end.py`); `test_developer_exit_code_never_decides_the_pipeline_gate`, `test_only_the_pipeline_role_writes_the_pipeline_gate` (`orchestrator/tests/test_runner_events.py`); `test_pipeline_dispatches_even_when_the_gate_is_already_set`, `test_pipeline_execution_does_not_spend_the_agent_budget`, `test_pipeline_still_blocks_once_no_agent_run_is_affordable` (`orchestrator/tests/test_workflow_nodes.py`). Tests updated — the three `test_end_to_end.py` happy-path tests now deliver the pipeline event they previously skipped; `test_repair_cycle_blocks_only_after_every_counted_attempt_really_ran` now starts from a developer exit **0** (the case that used to bypass the pipeline) instead of exit 1; `test_completed_repairer_transitions_to_local_pipeline` gained a gate-absence assertion; `test_short_circuiting_nodes_clear_the_awaiting_gate` lost its `pipeline` case because `pipeline` no longer short-circuits.

## Decisions

- Decision: the `pipeline` node does not clear `pipeline_passed` to `False` when it dispatches.
  - Context: while a new pipeline execution is in flight, a stale `True` from an earlier run is still in the LangGraph checkpoint (`PersistedWorkflowRuntime.run` merges updates, so an absent key keeps its previous value).
  - Alternatives considered: have the node write `"pipeline_passed": False` alongside the dispatch, so the gate is provably unset while the execution runs.
  - Reason: it would violate acceptance criterion 1 — the criterion is literally a grep for who writes the gate, and one producer is the invariant worth keeping. The stale value is also unreachable: the graph suspends on `awaiting_execution` until the pipeline reports, `route_after_pipeline` is only evaluated after that report has overwritten the gate, and `route_after_checks` is only reached downstream of a genuinely passing pipeline.
  - Consequences: a reader inspecting persisted graph state mid-pipeline can see `pipeline_passed = True` next to `status = local_pipeline`. `awaiting_execution` is the field that disambiguates. Covered by `test_a_repair_gets_its_own_pipeline_execution_despite_an_earlier_pass`.

- Decision: the pipeline execution does not spend `total_agent_executions`; the budget itself stays at 10.
  - Context: **the first version of this change was a regression and the adversarial review caught it.** Making the pipeline mandatory added an execution to every review-driven repair cycle, which previously cost two agent runs (repairer, reviewer) because `repair → pipeline` short-circuited on the still-`True` gate. At three units per cycle, `review_cycles = 3` no longer fits in `total_agent_executions = 10`. Reproduced against the real compiled graph: after two `changes_requested` cycles the third review **approved** the work and the run still ended `status=blocked`, `blocking_reason="workflow retry budget exhausted"`, `total=10`, with no pull request created — because `route_after_pipeline`'s approved branch has no budget check and `push` then hit the cap in `_dispatch`. The same scenario replayed against `HEAD` (`8956d84`) reaches `pushing` at `total=8`. Second-order: `blocking_reason` is the constant string the project circuit breaker counts, so three review-heavy issues would have opened a project's circuit.
  - Alternatives considered: (1) raise `total_agent_executions` to 11–13; (2) make `route_after_review`'s approved branch budget-aware so a run fails before spending the reviewer.
  - Reason: `total_agent_executions` budgets *agent* runs, and the local pipeline is not one — the runner executes the project's configured commands directly and `dispatch.Dispatcher` does not even require an agent backend for the `pipeline` role. Raising the number would have been a magic constant papering over a category error, and would still have changed the budget's meaning under the same value. Not counting it restores exactly `HEAD`'s accounting: the happy path is 4 again, and the reproduction above now reaches `pushing` at `total=8`, identical to `HEAD`. It also cannot run away — `pipeline` is reachable only from `implement` and `repair`, both of which dispatch a counted agent execution first and are capped by their own attempt counters.
  - Consequences: the exhaustion *check* still applies to the pipeline node (an exhausted agent budget blocks there rather than paying for a verdict whose only two successors dispatch agents), so no dispatch is unbounded. Relative to `HEAD` the change is never stricter: the pipeline-failure repair path, which did count pipelines before, now gets more headroom and still blocks on `pipeline_repair_attempts` (3). Covered by `test_pipeline_execution_does_not_spend_the_agent_budget`, `test_pipeline_still_blocks_once_no_agent_run_is_affordable`, and `test_full_review_cycle_still_reaches_delivery_on_the_agent_budget` (the end-to-end reproduction of the regression).

## Validation Status

Record only validation that was actually run.

- Targeted tests: Passed — `PYTHONPATH=orchestrator/src .venv/bin/python3 -m unittest discover -s orchestrator/tests -p 'test_runner_events.py'`, and the same for `test_workflow_nodes.py` and `test_end_to_end.py`. (One pattern at a time: `unittest discover` honours only the last `-p`.)
- Service tests: Passed — `make test-orchestrator` → `Ran 359 tests ... OK (skipped=9)` (351 before this change; the 9 skips are the pre-existing Postgres-integration skips).
- Failing-test-first evidence: with the fix applied and the tests not yet updated, `make test-orchestrator` failed exactly the four tests that encoded the old behaviour — `test_sequential_runner_events_drive_the_workflow_to_completed` (`'local_pipeline' != 'ai_review'`), `test_runner_event_entry_point_resumes_the_graph_and_completes_delivery` (`'repairing' != 'pushing'`), `test_human_approval_interrupt_pauses_before_merge`, and `test_workflow_nodes.test_short_circuiting_nodes_clear_the_awaiting_gate` (`True is not false`).
- Mutation checks: three, each reverting one part of the change and running the full `make test-orchestrator`, then restoring the file byte-for-byte (`git diff --stat` re-checked afterwards).
  - Developer branch infers the gate again → `FAILED (failures=4)`: `test_developer_exit_code_never_decides_the_pipeline_gate`, `test_only_the_pipeline_role_writes_the_pipeline_gate`, `test_a_clean_developer_exit_cannot_reach_review_without_the_pipeline`, `test_sequential_runner_events_drive_the_workflow_to_completed`.
  - `pipeline` node short-circuits again → `FAILED (failures=3)`: `test_pipeline_dispatches_even_when_the_gate_is_already_set`, `test_a_repair_gets_its_own_pipeline_execution_despite_an_earlier_pass`, `test_full_review_cycle_still_reaches_delivery_on_the_agent_budget`.
  - Pipeline spends the agent budget again → `FAILED (failures=3)`: `test_pipeline_execution_does_not_spend_the_agent_budget`, `test_sequential_runner_events_drive_the_workflow_to_completed`, `test_full_review_cycle_still_reaches_delivery_on_the_agent_budget`.
  - All three parts are load-bearing and none masks another: with only the node's short-circuit restored the happy path still dispatches a pipeline, because the developer no longer seeds the gate.
- Adversarial review: run against the full diff before committing. It found the budget regression above (with a reproduction), two factual overclaims in the new documentation (`plan_valid` is also written by the `plan` node's short-circuit, so "exactly one producer per gate" was false as a repo-wide statement; and the `checks_*` row contradicted the sentence claiming every gate is written by an *execution*), and the workspace-reset gap now filed as #136. All were fixed before the commit; the review's remaining findings are recorded under Known Issues.
- Lint: Passed — `make lint` → `All checks passed!`.
- Type checks: Passed — `make typecheck MYPY_CACHE=/tmp/moirai-mypy-cache-issue-90` → `Success: no issues found in 47 source files` (private cache so it cannot race sibling worktrees).
- Full repository tests: Not run. No Go, web, proto, Compose, or configuration file changed; `make test-runner` / `test-api` / `test-web` / `compose` / `proto-check` were deliberately skipped.
- Database migrations: Not applicable — `pipeline_passed` has no `app.workflow_runs` column (`workflows/persistence.py::_DURABLE_COLUMNS`); it lives only in the LangGraph checkpoint and the `workflow_events` audit trail, so nothing persisted changes shape.
- End-to-end workflow: Not run against live services. Exercised against the real compiled LangGraph graph in `orchestrator/tests/test_end_to_end.py`, including the gRPC `RunnerControlService` entry point.

## Known Issues

- Issue: the local pipeline now always runs, but for every project in the repository today it runs **zero commands** and therefore always passes.
  - Severity: P1 — it caps the value of this fix.
  - Impact: the gate is real and un-bypassable after this change, but it is still vacuous until pipeline steps can be configured. "Deterministic checks decide whether work is complete" is only true once a project has required steps.
  - Evidence: `app.project_pipeline_steps` is read in `persistence/control_plane.py` (`WHERE project_id = $1 AND required = true`) and written nowhere in the repository; `runner/internal/dispatch/dispatch.go` reports `Status: "completed", ExitCode: 0` for an empty command list.
  - Suggested resolution: issue [#114](https://github.com/alexandre-leites/moirai/issues/114), which owns configuring project pipeline steps. Out of scope here — closing it needs the API/web/persistence write path, not the workflow engine. Documented in `orchestrator/README.md` under "Gate ownership".

- Issue: the pipeline execution validates the **default branch**, not the implementation it follows. Filed as [#136](https://github.com/alexandre-leites/moirai/issues/136).
  - Severity: P1 — together with the empty-pipeline issue above, it is what stands between this orchestrator fix and a gate that actually means something.
  - Impact: the mandatory pipeline run now happens, but it runs the project's commands against pristine base content, so a pass says nothing about the developer's work and a repair is never re-validated either. The reviewer execution reads base-branch code for the same reason.
  - Evidence: `runner/internal/repository/manager.go` — `Prepare` does `os.RemoveAll(workspace.Root)` and then `git worktree add -B <branch> <workspace> <default-branch>`; `-B` is create-*or-reset*, so the agent branch is rewound to the default branch on every execution, and `prepareSource` fetches only the default branch. The branch name is stable per job (`task_packets.py`: `agent/{issue}/{job[:8]}`), which is what makes the reset silent. Reproduced with plain git in the issue body: after a commit on the agent branch, the next `worktree add -B` yields base content and moves the branch back to `main`.
  - Suggested resolution: #136. Distinct from #100/F13 (retain and commit *failed* work) — even with #100 landed, a committed diff is reset away by the next `Prepare`. Runner-side, and `runner/` is owned by another agent's task right now, so untouched here.
  - Correction: an earlier draft of this section claimed #114 was "the single thing standing between this fix and a genuinely deterministic completion gate". That was wrong; the adversarial review found #136. Both must land.

- Issue: a `cancelled` pipeline execution is translated to `pipeline_passed = False` and routes to `repairing`, rather than to the `cancelled` status a cancelled execution of any other role produces.
  - Severity: P3
  - Impact: an operator-cancelled or lease-expired pipeline execution looks like a pipeline failure and spends a repair attempt. Not a behaviour change here — but the *exposure* grew, because before this fix a pipeline execution only ran after a non-zero developer exit, so most workflows never opened that window at all.
  - Evidence: `workflows/runner_events.py` tests `resolved_role == "pipeline"` before the `summary.cancelled` branch. The same branch also ignores `current_status` (unlike the `developer` branch, which discriminates `implementing` from `pushing`), so a late terminal pipeline event arriving during `ai_review`/`pushing` rewinds `status` to `local_pipeline`. Both shapes are pre-existing; this issue explicitly scoped the pipeline branch as already correct.
  - Suggested resolution: decide whether a cancelled pipeline should be terminal-cancelled or a failed gate, and whether the pipeline branch should be status-guarded; then order and guard the branches accordingly. Wants its own issue.

- Issue: duplicate delivery of one developer terminal transition queues a repairer alongside the in-flight pipeline execution.
  - Severity: P2 — pre-existing (F9 / [#96](https://github.com/alexandre-leites/moirai/issues/96)), not introduced here.
  - Impact: `_dispatch`'s replay guard only suppresses a duplicate of *the same* role, so it does not help across a phase boundary. The count of spurious dispatches is the same as before this change, but the shape is worse in kind: a repairer can mutate the tree while the pipeline meant to validate it is running.
  - Evidence: replaying one developer transition twice through `PersistedWorkflowRuntime.run` without the scheduler claiming in between yields queued roles `[pipeline, repairer]` (before this change: `[reviewer, repairer]`).
  - Suggested resolution: #96, which owns transition-replay idempotency.

## Next Recommended Implementation

Issues [#114](https://github.com/alexandre-leites/moirai/issues/114) (a write path for `app.project_pipeline_steps`) and [#136](https://github.com/alexandre-leites/moirai/issues/136) (stop force-resetting each execution's workspace to the default branch). The orchestrator now guarantees the gate is dispatched and that only the gate's own execution decides it; those two make what the gate runs, and what it runs *against*, real. Neither is an orchestrator change.

---

# Session: F7 / issue #94 — Close execution requests so stalled-run recovery fires (branch `issue-94`)

## Current Status

- Overall status: complete, pending review.
- Current phase: durability wedges from the 2026-07-29 platform review.
- Active implementation: none — issue #94 delivered (session `issue-94`, 2026-07-29).
- Last updated: 2026-07-29.
- Agent/session identifier: `issue-94`.

## Done

- [x] `app.workflow_execution_requests` rows are closed, and the stalled-run recovery arm actually repairs runs.
  - Completed: 2026-07-29.
  - Relevant files: `orchestrator/src/moirai/persistence/control_plane.py`, `orchestrator/src/moirai/main.py`, `orchestrator/tests/test_asyncpg_control_plane.py`, `orchestrator/tests/test_postgres_integration.py`, `README.md`, `orchestrator/README.md`, `docs/architecture.md`.
  - Behavior delivered:
    - `accept_event` closes the execution request in the same transaction that records the runner's terminal event, with the event's own status (`completed` / `failed` / `cancelled`). `_resolve_dispatched_execution` now returns the request id alongside `(role, attempt)` and takes `FOR UPDATE` on the row, so a concurrent offer release cannot requeue the row between resolution and close. The bootstrap planner dispatch (`{job_id}-plan`) has no request row and closes nothing.
    - New `close_orphaned_execution_requests(now, stale_after)` closes rows nothing can ever execute or report on: any open row on a terminal run, and any `dispatched` row older than the stall window whose workflow run has no job in `offered` / `preparing` / `running` / `recovering`. Status `orphaned`.
    - `find_stalled_workflow_runs` moved from a status blocklist to an allow-list of statuses that mean "an agent execution should be in flight" (`preparing`, `planning`, `implementing`, `local_pipeline`, `repairing`, `ai_review`, `pushing`, `recovering`), added `recovering` to the active-job exclusion, and takes a `limit`.
    - `recover_stalled_workflow_run(workflow_run_id, on_transition, now)` repairs a run in one of two ways, decided by how its most recent request was closed. `orphaned` → the execution was lost, so a fresh `queued` request is written for the **same role** and the graph is left suspended. `completed`/`failed`/`cancelled` → the execution reported and only the graph invocation was lost, so the graph is re-entered with `awaiting_execution` cleared. Terminal runs are skipped.
    - `main._run_workflow_maintenance_loop` gained the sweep arm (ordered before detection, because a leaked `dispatched` row is exactly what hides the run from the detector), per-run failure isolation, a bounded batch, named interval/window constants, and a `_log_unexpected_completion` done-callback so the loop can no longer die silently. The whole iteration — including the leadership probe — is inside the failure guard, so a database blip costs one tick rather than the process.
    - `build_task_packet` refuses to build a packet for a job whose run has execution history but no `dispatched` request, instead of silently emitting the bootstrap planner packet.
  - Validation performed: failing-test-first (below), then unit + Postgres integration + lint + typecheck, all green.
  - Commands executed:
    - `make test-orchestrator` → `Ran 370 tests ... OK (skipped=13)`
    - `LOOP_TEST_DATABASE_URL=postgresql://loop:…@localhost:55494/loop_test make test-postgres-integration` → `Ran 13 tests ... OK`
    - `make lint` → `All checks passed!`
    - `make typecheck MYPY_CACHE=/tmp/moirai-mypy-cache-issue-94` → `Success: no issues found in 47 source files`
  - Notes: the throwaway Postgres ran on port 55494 and was removed afterwards.

## Failing-test-first evidence

The issue is marked *(verify)*. Three integration tests were written against the unmodified code first; on `8956d84` all three failed and the nine pre-existing integration tests passed (`Ran 12 tests ... FAILED (failures=3)`):

- `test_terminal_event_closes_the_dispatched_execution_request` → `AssertionError: Tuples differ: ('planner', 'dispatched') != ('planner', 'completed')` — the row was never closed.
- `test_committed_transition_without_a_graph_invocation_is_recovered` → the maintenance loop recovered nothing (`[] != ['<run id>']`), because `find_stalled_workflow_runs` could not see the run.
- `test_maintenance_loop_closes_an_execution_request_whose_job_is_gone` (now `test_lost_planner_execution_is_requeued_rather_than_left_dispatched`) → same empty recovery.

## Acceptance criteria

- *"After a normal phase completes, no `queued`/`dispatched` request rows remain for it."* — met. `test_terminal_event_closes_the_dispatched_execution_request` drives a real planner execution end to end (`schedule` → `accept_offer` → graph dispatch → `schedule_execution` → `accept_offer` → `started` + `completed` events through `accept_event` with the production runtime as `on_transition`) and asserts the request set is exactly `[('developer', 1, 'queued'), ('planner', 1, 'completed')]`: the planner phase left nothing open, and the only open row is the next phase the resumed graph queued.
- *"A simulated crash (transition committed, graph never invoked) is recovered by the maintenance loop within one interval."* — met. `test_committed_transition_without_a_graph_invocation_is_recovered` calls `accept_event` with `on_transition=None`, which commits the transition, the new run status, and the outbox row without ever invoking the graph, then parks the outbox row in `processing` so the drain arm cannot rescue it. It asserts the run is stalled, runs the real `_run_workflow_maintenance_loop` with a leader that stops it after claiming leadership once, and asserts the run was recovered and is no longer stalled.

## Post-review corrections

Two adversarial reviews of the diff. Round 1 found two defects, round 2 found a third plus several gaps; all are fixed.

- **Blocker.** The first implementation recovered every stalled run by clearing `awaiting_execution` and resuming the graph. `implement`, `repair` and `push` have *unconditional* outgoing edges, so for a run whose execution was **lost** that does not re-run the phase — it skips it. The reviewer demonstrated the `push` case end to end on the real graph: the branch is never pushed, yet `create_pull_request` → `wait_for_checks` → `merge` → `complete` runs, the pull request is merged and the issue closed. Fixed by splitting recovery on how the last request was closed (`orphaned` → re-queue the same role; terminal → advance), with `test_lost_execution_never_advances_the_graph_past_its_phase` as the regression test: after losing a developer execution the request set is `[('developer', 1, 'orphaned'), ('developer', 2, 'queued'), ('planner', 1, 'completed')]` and no `pipeline` row exists.
- **Tests were not exercising the resume path.** The integration fixture built a new `InMemorySaver` per `_runtime()` call, so the recovering runtime had an empty LangGraph thread and `ainvoke(None, config)` replayed from `START` instead of resuming the suspended edge. The reviewer mutation-tested it: reverting the production change left all three tests green. The checkpointer is now memoised per test, and the acceptance-criterion-2 test asserts that exactly one graph node ran during recovery — a `START` replay re-runs `prepare` as well — so the fixture change is itself pinned.
- **Blocker (round 2).** Adding `add_done_callback(_log_unexpected_completion)` to the maintenance task promoted every unhandled exception in that loop into a full orchestrator shutdown, and `leader.is_leader()` sat outside the per-iteration `try`. `AsyncpgLeader` re-raises whatever the database did, so a Postgres failover would have exited the process — where `Scheduler.run` retries the identical call forever. The probe is now inside the guard, with `test_leadership_probe_failure_does_not_kill_the_loop` as the regression test.
- **`build_task_packet` regression (round 2).** Closing the request means a later recovery re-offer of the same job finds no `dispatched` row and falls back to the *bootstrap* planner packet, whose execution ID `{job_id}-plan` `accept_event` always rejects — turning a pre-existing stuck run (see Known Issues) into a control-stream abort on every retry. `build_task_packet` now refuses to build a packet for a run that has execution history but no dispatched request; the scheduler skips such a candidate and the unanswered-offer limit bounds it.
- **Unbounded re-queue (round 2).** A re-queue spends no retry budget, so nothing would have capped how many agent executions one wedged phase could buy. `_LOST_EXECUTION_REQUEUE_LIMIT` (5) now bounds it.
- **Head-of-line starvation (round 2).** A run whose recovery raised kept its old `updated_at` and so reoccupied the bounded batch every tick. `recover_stalled_workflow_run` now bumps `updated_at` for every run it touches, so a failing run backs off one stall window.
- **Coverage gaps (round 2).** The sweep's terminal-run rule, the re-queue limit, the `updated_at` back-off and the `build_task_packet` guard were all uncovered; each now has a test, verified by mutation.

## Decisions

- Decision: a lost execution is re-queued; a delivered one advances the graph. The discriminator is the closed request's status.
  - Context: a stalled run is either "the execution never reported" or "it reported and only the graph invocation was lost". The graph is an event-driven state machine (issue #88), so the two need opposite repairs.
  - Alternatives considered: (a) always clear `awaiting_execution` and resume — skips phases on the three unconditional edges, see Post-review corrections; (b) always re-queue — re-runs an execution that already succeeded, and cannot repair a run whose graph simply never got the transition; (c) resume the graph at the dispatching node with LangGraph's `as_node`, which would need `workflows/runtime.py` to special-case recovery in the same path terminal events use.
  - Reason: the request status is already written by exactly the two code paths that distinguish the cases (`accept_event` vs the orphan sweep), so the signal is free and cannot drift from reality.
  - Consequences: recovery never fabricates progress. The re-queue path spends no extra retry budget (the attempt was charged at dispatch) and the graph stays suspended until the replacement execution reports, which is the same path a first-time execution takes.
- Decision: `find_stalled_workflow_runs` is an allow-list, and excludes every status parked on something other than an execution.
  - Context: the previous predicate was "everything except terminal, `offered`, `preparing`", which would have swept up `waiting_github_checks` — the status a run actually holds while parked at the `wait_for_human` interrupt (`test_end_to_end.py::test_human_approval_interrupt_pauses_before_merge`).
  - Alternatives considered: keep the blocklist and special-case human approval.
  - Reason: re-entering the graph for such a run would execute `wait_for_human` with no decision recorded, and `route_after_human_response(False, False)` blocks the run. A human-approval park must not be resolvable by a background sweep.
  - Consequences: `pr_created` and `merging` are excluded too. Those are transient statuses on unconditional edges rather than parks, so a crash between the transition and the next node leaves such a run with no owner — see Known Issues.
- Decision: `preparing` *is* in the allow-list, and jobs in `recovering` count as active work.
  - Context: `accept_offer` stamps the run `preparing` for the whole life of an execution, so that status covers most in-flight work; a job in `recovering` belongs to `recover_one`.
  - Reason: while an execution really is in flight the run is protected twice over (an open request *and* a job in `offered`/`preparing`/`running`), so including `preparing` cannot double-dispatch. A run in `recovering` whose job is not is unreachable for `recover_one`, which requires both, and is exactly the kind of stall this query exists to find.
  - Consequences: the detector is strictly narrower than the old predicate for parked runs and strictly wider for genuinely stuck ones.
- Decision: the sweep only closes `dispatched` rows on live runs, never `queued` ones.
  - Context: a `queued` row on a run whose job is mid-execution is completely normal and may sit there for the length of an agent run.
  - Reason: there is no time window that separates "queued and waiting for the current phase to finish" from "queued and unclaimable", so closing `queued` rows on live runs would cancel healthy work.
  - Consequences: the narrower leak in Known Issues remains open.

## What #91 already covered

The issue's step 2 says offer expiry leaks `dispatched` rows. That is no longer true on `main`: #91's `_release_unanswered_offer` moves the row from `dispatched` back to `queued` on the requeue path (so `schedule_execution` re-offers the same request), and `_block_unanswered_run` moves `queued`/`dispatched` to `expired` when the unanswered-offer limit is reached. Both are verified by existing integration tests. What #91 does **not** cover, and this change does, is a `dispatched` row whose job disappeared through any path other than an unanswered offer — that is the second rule of `close_orphaned_execution_requests`.

## Validation Status

- Targeted tests: Passed — `PYTHONPATH=orchestrator/src .venv/bin/python3 -m unittest test_asyncpg_control_plane test_postgres_integration`.
- Service tests: Passed — `make test-orchestrator` → `Ran 370 tests ... OK (skipped=13)`.
- Full repository tests: Not run — no Go, proto, or web change in this diff.
- Failing-test-first evidence: recorded above. Additionally mutation-checked after the fix — each production change was reverted in turn and `StalledRunRecoveryIntegrationTests` re-run against a freshly migrated database:

  | Reverted change | Result |
  | --- | --- |
  | `accept_event` closing the request | killed (3 integration tests) |
  | `close_orphaned_execution_requests` stubbed to return 0 | killed (3) |
  | the sweep's terminal-run rule replaced by `false` | killed (`test_open_requests_on_a_terminal_run_are_closed`) |
  | `find_stalled_workflow_runs` allow-list, back to the old blocklist | killed (2) |
  | the `orphaned` / terminal split — always advance the graph | killed, with `AssertionError: 'pipeline' unexpectedly found in ['developer', 'pipeline', 'planner']`: the lost implementation is skipped and the pipeline dispatched, exactly the defect round 1 found |
  | `recover_stalled_workflow_run` clearing `awaiting_execution` | killed (`test_committed_transition_without_a_graph_invocation_is_recovered`) |
  | the `_LOST_EXECUTION_REQUEUE_LIMIT` check | killed (`test_recover_stalled_workflow_run_stops_replacing_a_repeatedly_lost_execution`) |
  | the `updated_at` bump for runs the loop could not repair | killed (`test_recover_stalled_workflow_run_defers_a_run_it_could_not_repair`) |
  | the `build_task_packet` guard | killed (`test_build_task_packet_refuses_a_run_with_history_but_no_dispatched_request`) |
  | sweeping *after* detecting instead of before | killed (2) |
  | the leadership probe moved back outside the iteration's `try` | killed (`test_leadership_probe_failure_does_not_kill_the_loop`) |
  | the test fixture's memoised checkpointer, back to one saver per call | killed (`test_committed_transition_without_a_graph_invocation_is_recovered`) — this is what pins the tests to the real `aupdate_state` + `ainvoke(None, config)` resume path rather than a replay from `START` |

  Not covered by any test, and stated here rather than claimed: the two `FOR UPDATE` clauses (`_resolve_dispatched_execution`, `recover_stalled_workflow_run`) and the batch `LIMIT`/`ORDER BY`. Those are concurrency and scale properties that the suite has no way to exercise; the lock ordering is argued in Decisions instead.
- Build: Not applicable (Python only).
- Lint: Passed — `make lint` → `All checks passed!`.
- Type checks: Passed — `make typecheck MYPY_CACHE=/tmp/moirai-mypy-cache-issue-94` → `Success: no issues found in 47 source files` (private cache, so sibling worktrees are unaffected).
- Database migrations: Not applicable — no schema change. `app.workflow_execution_requests.status` has no CHECK constraint, and `expired` was already written by `_block_unanswered_run`.
- Docker Compose: Passed — `make compose` (`docker compose config`) is valid. No Compose or configuration-file change was needed; the maintenance loop's interval, stall window, and batch size are module constants in `main.py`.
- End-to-end workflow: Not run.
- CI (PR [#142](https://github.com/alexandre-leites/moirai/pull/142), commit `668654f`): all ten checks green. The first attempt failed one job — `runner` / `TestControlLoopDeliversTerminalEventAfterLogsSaturateTheBufferWhileDisconnected` — on a diff that contains no Go code at all. Re-running that job on the same commit passed with no change, and the test passes 20/20 locally under `-race`, as does the whole runner suite at `-count=5`. Recorded as a flake in [#143](https://github.com/alexandre-leites/moirai/issues/143) rather than papered over; `runner/` belongs to another issue's ownership, so it was not touched here.

## Known Issues

- Issue: `accept_offer` overwrites the workflow run's phase with `preparing`, so a successful **developer** terminal event produces no workflow transition.
  - Severity: P1 (pre-existing, not introduced here, but this change alters its symptom).
  - Impact: `workflow_transition_for_terminal_event` maps the `developer` role only from `implementing` or `pushing`. In production the run is `preparing` by then, so the terminal event commits nothing, the job stays `running` until its lease expires, and `expire_leases` → `recover_one` re-offers it. Before this change the request row was still `dispatched`, so `build_task_packet` produced a developer packet and the run re-ran the developer forever. Now the row is closed, so `build_task_packet` falls back to a **planner** packet, the runner emits `{job_id}-plan`, `_resolve_dispatched_execution` rejects it and `accept_event` raises `StaleLeaseError` — the control stream aborts. Both are unrecoverable loops; the new one also burns a reconnect per attempt.
  - Evidence: reproduced against real Postgres during review — after a successful developer event, `graph nodes run: []`, `run status: preparing`, `job status: running`.
  - What this change does about it: the recovery re-offer no longer emits a bogus planner packet (`build_task_packet` refuses), so the symptom is a skipped scheduling candidate and an offer that expires, bounded by `unanswered_offer_limit`, rather than a control-stream abort per retry. The underlying loop is unchanged and still P1.
  - Suggested resolution: stop `accept_offer` clobbering `current_phase` (or make the `developer` branch of `runner_events.py` phase-independent). Both files are outside this issue's ownership; filed separately as [#141](https://github.com/alexandre-leites/moirai/issues/141).
- Issue: a `queued` request on a live run whose job can never be claimed is not swept, and starves the global queue.
  - Severity: P2.
  - Impact: `schedule_execution` orders candidates by `request.created_at, request.id` and returns `None` for the whole tick when the head candidate's job is not in `('completed','failed','cancelled')`, so one unclaimable head row blocks placement for every project.
  - Evidence: `persistence/control_plane.py` `schedule_execution` — the `UPDATE app.jobs … RETURNING` returns no row and the function returns `None` rather than skipping the candidate.
  - Suggested resolution: make `schedule_execution` skip an unclaimable candidate instead of ending the tick. Not attempted here: that query is also where the circuit-breaker half-open exclusion lives (issue #92).
- Issue: the advancing branch of recovery does not replay the state updates from the outbox row it could not drain.
  - Severity: P3.
  - Impact: a gate the lost invocation would have set (e.g. `plan_valid`) stays as it was, so the graph re-runs that phase instead of advancing — safe and budget-bounded, but it costs one repeated execution. Asserted explicitly in `test_committed_transition_without_a_graph_invocation_is_recovered`.
  - Evidence: `recover_stalled_workflow_run` passes only `{"awaiting_execution": False}`; the updates live on the `workflow_transition_outbox` row.
  - Suggested resolution: [#96](https://github.com/alexandre-leites/moirai/issues/96), which owns transition-replay idempotency and the never-retried `processing` rows.
- Issue: `pr_created`, `merging` and `waiting_github_checks` runs have no recovery owner at all.
  - Severity: P3.
  - Impact: a crash between one of those transitions and the next node leaves the run stuck; nothing re-polls GitHub checks either.
  - Evidence: `find_stalled_workflow_runs` excludes them deliberately (a `waiting_github_checks` run may be parked at the `wait_for_human` interrupt, which must not be resolved by a sweep), and no other loop invokes the graph.
  - Suggested resolution: a distinct poller for check-waiting runs, keyed on something that can tell a parked interrupt from a stalled edge.

## Next Recommended Implementation

Unchanged from the previous session: issues [#114](https://github.com/alexandre-leites/moirai/issues/114) and [#136](https://github.com/alexandre-leites/moirai/issues/136). Within the orchestrator track, the `accept_offer` phase clobber recorded under Known Issues ([#141](https://github.com/alexandre-leites/moirai/issues/141)) is the highest-value next fix: it makes every developer execution transition-less, which is a P1 break of the core loop independent of this change.
# Session: F11 / issue #98 — `ci_repair_attempts` is never incremented; CI repairs consume the local pipeline budget (branch `issue-98`)

## Current Status

- Overall status: implemented and validated on branch `issue-98`.
- Current phase: complete — orchestrator workflow graph.
- Active implementation: none (issue-98 agent session, 2026-07-29).
- Last updated: 2026-07-29.
- Agent/session identifier: issue-98 agent, branch `issue-98`, based on `30d8483`.

## Done

- [x] Split the repair phase into two nodes so each repair source spends its own budget.
  - Completed: 2026-07-29.
  - Relevant files: `orchestrator/src/moirai/workflows/policy.py`, `orchestrator/src/moirai/workflows/issue_graph.py`, `orchestrator/src/moirai/workflows/nodes.py`, `orchestrator/tests/test_workflow_policy.py`, `orchestrator/tests/test_issue_graph.py`, `orchestrator/tests/test_workflow_nodes.py`, `orchestrator/tests/test_end_to_end.py`, `orchestrator/README.md`.
  - Behavior delivered: `WorkflowRoute.CI_REPAIR` is a new route; `route_after_checks` returns it for failing required checks (it used to return `REPAIR`); the graph has a new `ci_repair` node that dispatches the same `repairer` role and `repairing` phase as `repair` but increments `ci_repair_attempts`, and its outgoing edge rejoins at `pipeline` through the same `suspend_after_dispatch` wrapper. `repair` is unchanged and still owns `pipeline_repair_attempts`. `ci_repair_attempts` now has exactly one writer (`PersistedWorkflowNodes.ci_repair`); before this change it had none, so `route_after_checks` gated on a counter frozen at 0 and every CI repair spent the local pipeline's repair budget instead.
  - Validation performed: see `Validation Status` below — full orchestrator suite, lint, mypy, plus two mutation runs that reintroduce the bug in two different ways.
  - Commands executed: `make test-orchestrator`, `make lint`, `make typecheck MYPY_CACHE=/tmp/moirai-mypy-cache-issue-98`.
  - Notes: no migration and no configuration change was needed. `app.workflow_runs.ci_repair_attempts` already exists (`migrations/001_initial.sql:116`), `workflows/persistence.py::_DURABLE_COLUMNS` already maps it, `grpc/protocol.py` already carries it, and `RetryBudget.ci_repair_attempts` already defaulted to 3 — the column and the bound were real all along; only the writer was missing. The `repairer` role and the `repairing` status are deliberately shared, so nothing outside the graph (task packets, `schemas/task-packet.schema.json`, `runner/`, `issue_sync.py`'s status list, the web console's status map) needed a matching change.

## Post-review corrections

Two independent adversarial reviews ran against the full diff before the commit. Both mutation-tested the change against throwaway copies of the repository and both returned **no blockers**; both independently confirmed that `total_agent_executions` accounting is byte-identical before and after the diff (the same `repairer` dispatch costs the same unit either way), that the happy path is still 4 agent runs, and that no other file in the repository needed a matching change. What they found, and what was changed in response:

- **The README's arithmetic claim was an overclaim.** "A workflow that reached a pull request can afford two CI cycles" holds only for the cheapest path to a pull request (4 agent runs); a run that already took one local repair reaches its pull request at 7 and affords one. Rewritten.
- **A CI cycle also spends a `review_cycles` unit**, which the first draft never said. That is a *second*, independent reason `ci_repair_attempts = 3` cannot bind under the shipped defaults: `review_cycles = 3` allows at most two CI cycles regardless of the global budget. Documented in `orchestrator/README.md` and in `Known Issues`, and the corresponding claim in the `Decisions` section was corrected.
- **The end-to-end budget test seeds a state production cannot produce.** A real workflow row with `ci_repair_attempts = 2` would also carry roughly eight agent executions, so with realistic companion counters the global budget would stop the loop first. The test is still valid and mutation-verified, but its docstring claimed more than it proves; it now states plainly that the seed exists to isolate the CI bound from the other two, and asserts `review_cycles = 2` alongside `total_agent_executions = 7` to show both had headroom.
- **`README.md` line 23, which this diff edited, called `pipeline` a node that "queues an agent execution"** — contradicting line 44 and `_NON_AGENT_ROLES` in the same repository. Reworded.
- **"A run that exhausted its local repair budget can still repair a CI failure" was too strong**: it can *dispatch* the CI repair, but the repaired tree still meets `route_after_pipeline`, which blocks on the exhausted local counter. Reworded.
- **Sharing the `repairer` role weakens `_dispatch`'s replay guard**, which identifies "my own queued request" by role and so can no longer distinguish a replayed `repair` from a replayed `ci_repair`. Neither review could construct a reachable sequence, and the reason is now written down in `nodes.py`: reaching the other repair node requires a terminal event for the first execution, and `_resolve_execution_identity` only accepts a terminal event for a request row still in `dispatched` — never one that offer recovery returned to `queued`. Documented rather than changed, because tightening the guard would touch the shared `_dispatch` path used by every node and would key on `execution_id`, which is not a durable column.
- **`ci_repair`'s node-level budget guard is defensive, not load-bearing** (the route gates on the same counter with the same limit and blocks first), while `repair`'s is genuinely reachable via `route_after_human_response`. The test docstring now says so instead of implying both are live product paths.
- **Three pre-existing defects surfaced** that bear on this change's practical value — pending checks are never re-polled, `merge` can report `blocked` and still fall through to `complete`, and repairer task packets carry no signal about which failure they are repairing. All three are recorded under `Known Issues` with evidence; none is introduced here and none is fixed here.

Not changed in response to review: the suggestion to make the `_dispatch` replay guard key on execution identity (shared code, non-durable key, no reachable defect), and the suggestion to lower `ci_repair_attempts` to 2 or raise the other budgets so the CI bound binds (a product decision, argued against in `Decisions`).

## Decisions

- Decision: two distinct nodes (`repair` and `ci_repair`) rather than a `repair_source` marker carried in state.
  - Context: issue #98 offers both and calls the split "arguably cleaner". The marker approach would have `wait_for_checks` write something like `repair_source: "ci"` into its updates, and the single `repair` node branch on it.
  - Alternatives considered: (1) the `repair_source` state marker; (2) leaving one node and having it pick a counter from `status`/`checks_passed`.
  - Reason: the marker adds a second, order-dependent piece of state that must be written by one node, read by another, and *cleared* — a stale `repair_source` would silently misattribute the next local repair, which is the same class of bug as the one being fixed. It also has no durable column, so after a resume it would live only in the LangGraph checkpoint while the counters it selects live in `app.workflow_runs`. With two nodes, the graph edge *is* the marker: it cannot go stale, it cannot be forgotten, and it is visible in the graph topology rather than in a state key. Option (3) is worse still — it re-derives intent from gates that other nodes own.
  - Consequences: `IssueWorkflowNodes` gains a `ci_repair` field and the graph gains one node and one edge. Both repair nodes dispatch the same `repairer` role and report the same `repairing` phase, so no runner, schema, API or UI change is required and `runner_events.py` translates their terminal events identically (`resolved_role == "repairer"` → `local_pipeline`, gate untouched). The cost is that "which repair is this?" is answered by the node, not by anything persisted: `workflow_events` shows two `repairing` transitions that differ only in which counter moved. That is enough, because the counter is exactly the thing the routing reads back.

- Decision: a CI repair is an agent execution and spends `total_agent_executions`.
  - Context: issue #90 established that the `pipeline` role is *not* an agent run and is excluded from the global budget via `_NON_AGENT_ROLES`. The new node had to be classified explicitly rather than by accident.
  - Alternatives considered: excluding `ci_repair` from the global budget so the configured CI bound of 3 becomes reachable under the default `total_agent_executions = 10`.
  - Reason: `ci_repair` dispatches the `repairer` role, which is an OpenCode agent run in `runner/internal/agents` exactly like the repairs the local pipeline triggers. `_NON_AGENT_ROLES` exists to name the one role the runner executes *without* an agent backend; adding an agent role to it would make the global budget stop meaning "agent runs" and would let the CI loop run 3 repair cycles beyond the cap. This required no code change — `_dispatch` counts every role outside `_NON_AGENT_ROLES` — but it is the load-bearing reason the change is safe, and it is asserted in `test_each_repair_node_spends_only_its_own_budget`.
  - Consequences: one CI repair cycle costs three agent runs (repairer, reviewer, push; the pipeline run is free) *and* one `review_cycles` unit, so under the shipped defaults `ci_repair_attempts = 3` is never the bound that stops the loop — see `Known Issues`. Recorded there and in `orchestrator/README.md`, and *not* papered over by raising the defaults, for the reason the #90 session already recorded: choosing numbers to make a gate reachable is how the accounting drifted in the first place. What this change fixes is the misattribution: the CI loop no longer drains the local pipeline's repair budget, and the CI bound is now enforceable — `test_the_ci_repair_budget_stops_the_failing_check_loop` blocks at `ci_repair_attempts = 3` with `total_agent_executions = 7` of 10 and `review_cycles = 2` of 3, proving the CI counter is what stopped it.

- Decision: the human-requested-changes repair keeps spending `pipeline_repair_attempts`.
  - Context: `route_after_human_response(approved=False, changes_requested=True)` also routes to a repair. It is a third failure source, after the pull request exists, so it could arguably belong with the CI repairs.
  - Alternatives considered: routing it to `CI_REPAIR`; adding a third counter.
  - Reason: a human asking for changes is not a CI failure, so counting it against the CI budget would recreate the exact defect this issue is about, one gate silently draining another's budget. A third counter needs a `RetryBudget` field and an `app.workflow_runs` column, i.e. a migration, for a source the issue does not mention. Leaving it on `pipeline_repair_attempts` is unchanged behaviour, so nothing regresses.
  - Consequences: `pipeline_repair_attempts` means "repairs requested before the work is accepted" rather than strictly "repairs after a local pipeline failure". Stated in the `Retry budgets` table in `orchestrator/README.md` and in a comment on `route_after_human_response`, so the next reader is not surprised.

## Validation Status

Record only validation that was actually run.

- Targeted tests: Passed — `PYTHONPATH=orchestrator/src .venv/bin/python3 -m unittest discover -s orchestrator/tests -p 'test_end_to_end.py' -v` → `Ran 14 tests ... OK`, including the two new `EndToEndWorkflowTests`.
- Service tests: Passed — `make test-orchestrator` → `Ran 366 tests in 1.216s ... OK (skipped=9)` (359 before this change; +7 new tests, the 9 skips are the pre-existing Postgres-integration skips).
- Mutation checks: two, each reintroducing the bug in a different place, run against the full `make test-orchestrator` and then restored (`git diff` re-checked, sources compared byte-for-byte against a pre-mutation copy).
  - `ci_repair` increments `pipeline_repair_attempts` again (the literal original defect) → `FAILED (failures=2, errors=2)`: `test_dispatch_nodes_increment_the_matching_attempt_and_total_budget`, `test_each_repair_node_spends_only_its_own_budget`, `test_failing_checks_spend_the_ci_budget_and_leave_the_pipeline_budget_alone`, `test_the_ci_repair_budget_stops_the_failing_check_loop`.
  - The failing-checks edge points back at the `repair` node (`"ci_repair": "repair"` in the `wait_for_checks` path map) → `FAILED (failures=2)`: both new end-to-end tests. This one is the important half: the node-level unit tests pass under it, so the end-to-end tests are what prove the *wiring*, not just the node.
- Lint: Passed — `make lint` → `All checks passed!`.
- Type checks: Passed — `make typecheck MYPY_CACHE=/tmp/moirai-mypy-cache-issue-98` → `Success: no issues found in 47 source files` (private cache so it cannot race sibling worktrees).
- Full repository tests: Not run. No Go, web, proto, Compose, schema or configuration file changed; `make test-runner` / `test-api` / `test-web` / `compose` / `proto-check` were deliberately skipped.
- Database migrations: Not applicable — `ci_repair_attempts` is an existing column (`orchestrator/migrations/001_initial.sql:116`) already mapped by `workflows/persistence.py::_DURABLE_COLUMNS`. Nothing persisted changes shape; the column simply stops being permanently 0.
- End-to-end workflow: Not run against live services. Exercised against the real compiled LangGraph graph in `orchestrator/tests/test_end_to_end.py`, driven one simulated runner terminal event at a time through `workflow_transition_for_terminal_event` and `PersistedWorkflowRuntime.run`, with a code host whose required checks fail.
- Adversarial review: two independent reviews against the full diff before committing, both mutation-testing on throwaway copies. Neither found a blocker. Both reproduced the budget arithmetic by driving the real graph, and one also verified checkpoint compatibility in both directions by resuming an old-topology checkpoint under the new graph and vice versa. Their findings and the resulting corrections are listed under `Post-review corrections`.

## Known Issues

- Issue: under the shipped default budgets, `ci_repair_attempts = 3` still cannot be the bound that stops a CI repair loop. The second consequence the issue describes — "the CI-repair bound of 3 can never trip on its own" — is therefore only **partially** addressed: the counter is now real, single-writer and enforceable, but two other budgets bind before it.
  - Severity: P3.
  - Impact: two independent caps come first. (1) Each CI cycle spends one `review_cycles` unit on its way back through AI review, so `review_cycles = 3` allows at most two CI cycles after the initial review, whatever else is configured. (2) Each CI cycle costs three agent runs (repairer, reviewer, push), so `total_agent_executions = 10` allows two cycles only for a run that reached its pull request on the cheapest path (4 runs); a run that already took one local repair reaches the pull request at 7 and affords one. `RetryBudget` is not wired to configuration at all — `build_persisted_runtime` constructs both the graph and the nodes with the defaults — so today `ci_repair_attempts` is a knob with no way to turn it.
  - Evidence: measured against the real compiled graph, not inferred. A from-scratch run with permanently failing checks completes exactly two CI cycles and then blocks in `_agent_budget_route` with `ci_repair_attempts = 2`, `pipeline_repair_attempts = 0`, `review_cycles = 3`, `total_agent_executions = 10`. `test_the_ci_repair_budget_stops_the_failing_check_loop` reaches the CI bound only because it seeds `ci_repair_attempts = 2` on the workflow row — a deliberately synthetic state, labelled as such in the test's docstring, that isolates the CI bound from the other two.
  - Suggested resolution: a follow-up that sizes the budgets deliberately (and probably makes `RetryBudget` configurable per project) — `total_agent_executions >= 13` and `review_cycles >= 4` would be needed for `ci_repair_attempts = 3` to bind. Explicitly **not** done here: the #90 session's decision record already argues that picking budget numbers to make a gate reachable is how this accounting drifted, and doing it in the same change as the attribution fix would confuse a correctness fix with a product decision. Both acceptance criteria of #98 are met without it.

- Issue: rolling **back** past this commit while a workflow is suspended on a `ci_repair` dispatch duplicates that repair.
  - Severity: P3 — operational, only on rollback.
  - Impact: the checkpoint's last node is `ci_repair`, which the older graph does not have; resuming under the old code silently runs `repair` instead and dispatches a second `repairer` for the same logical repair (`total_agent_executions` +1, `pipeline_repair_attempts` +1). No crash, no stuck run. The forward direction is safe — no node was removed or renamed, so every old checkpoint resumes cleanly under the new graph (verified by resuming an old-topology checkpoint against the new graph on the same saver and thread).
  - Evidence: reproduced by the adversarial review against a copy of the repository, in both directions.
  - Suggested resolution: drain in-flight runs before rolling back past this commit. Inherent to adding a node to a checkpointed graph; not worth code.

- Issue (pre-existing, not introduced here): a run parked on **pending** GitHub checks never re-polls them, so the failing-checks gate — and therefore `ci_repair` — is close to unreachable in production.
  - Severity: P2. It caps the practical value of this fix in the same way #114/#136 cap the value of the pipeline gate.
  - Impact: `wait_for_checks` writes `checks_pending = True` and `route_checks` ends the invocation. The stall sweeper re-invokes the run, but `aupdate_state` + `ainvoke(None, config)` re-evaluates only the *edge* out of `wait_for_checks` — the node itself never runs again, `required_checks` is never called a second time, and the stale `checks_pending` routes straight back to END. The failing-checks branch is then only reached when GitHub already reports a conclusive failure in the same invocation as `create_pull_request`, which a just-pushed branch normally does not.
  - Evidence: reproduced by the adversarial review on both `origin/main` and this branch — `checked_prs` stayed at 1 across three sweeper ticks.
  - Suggested resolution: wants its own issue (re-enter the node, or have the poller write fresh `checks_*` into the resume state). Possibly overlapping with [#94](https://github.com/alexandre-leites/moirai/issues/94)'s stalled-run recovery, which another agent owns right now, so it is recorded rather than filed from here.

- Issue (pre-existing, not introduced here): `merge` can report `blocked` and still fall through to `complete`.
  - Severity: P2.
  - Impact: `nodes.merge` returns `status: "blocked"` when the code host refuses the merge, but `graph.add_edge("merge", "complete")` is unconditional, so the run closes the issue and applies `agent:delivered` for a pull request that was never merged. This is the one edge where the #88 invariant "a node reporting `blocked` short-circuits to the terminal node" does not hold, because it is a plain edge rather than a `suspend_after_dispatch` one.
  - Evidence: `workflows/nodes.py` merge branch versus `workflows/issue_graph.py`'s `merge -> complete` edge.
  - Suggested resolution: its own issue; the fix is a conditional edge, but it is outside #98's scope and would change terminal behaviour.

- Issue (pre-existing, not introduced here): a repairer task packet carries no signal about *which* failure it is repairing.
  - Severity: P3.
  - Impact: `build_task_packet` derives everything from the request's role, so a CI repair and a local-pipeline repair produce byte-identical packets. The agent gets no CI logs and cannot behave differently. This change records *why* a repair happened in the counter, not in the work order.
  - Evidence: `persistence/control_plane.py` builds packets from `role` alone; both repair nodes dispatch `repairer`.
  - Suggested resolution: the natural follow-up to this split, and the point at which a distinct role or a packet field would start to earn its keep. Out of scope: it needs runner and schema changes, both owned elsewhere.

- Issue: `repair` and `ci_repair` are indistinguishable in the persisted phase (`repairing`) and in `app.workflow_execution_requests.role` (`repairer`).
  - Severity: P3.
  - Impact: an operator reading `workflow_events` cannot tell a CI repair from a local one except by which counter moved in the same transition payload. The web console shows both as "Repairing".
  - Evidence: `workflows/nodes.py` — both nodes call `self._dispatch(state, "repairer", "repairing", ...)`.
  - Suggested resolution: deliberate. A distinct status would have to be added to `runner_events.WorkflowPhase`, `domain/models.py`, `services/issue_sync.py`'s status list, the API and the web console's status map — several of them owned by other agents right now — for a labelling improvement. The counters already carry the information the routing needs.

## Next Recommended Implementation

Unchanged from the previous session: [#114](https://github.com/alexandre-leites/moirai/issues/114) (a write path for `app.project_pipeline_steps`, so the local pipeline gate runs actual commands) and [#136](https://github.com/alexandre-leites/moirai/issues/136) (stop force-resetting each execution's workspace to the default branch). Both are what make the pipeline verdict — and therefore the repair budgets this change now attributes correctly — mean something.
# Session: retain and commit failed work so retries can build on it (#100 / F13)

- Agent/session identifier: runner-agent-issue-100
- Last updated: 2026-07-29
- Branch: `issue-100`
- Scope: `runner/` only. No orchestrator, API, web, proto, or Compose file was touched.

## Done

- [x] Commit and publish the work of a run that does not complete (#100, review finding F13)
  - Completed: 2026-07-29
  - Relevant files: `runner/internal/dispatch/dispatch.go`, `runner/internal/dispatch/retention.go` (new), `runner/internal/dispatch/logtail.go` (new), `runner/internal/dispatch/control_loop.go`, `runner/internal/repository/delivery.go`, `runner/internal/repository/manager.go`, `runner/internal/config/config.go`, `runner/cmd/runner/main.go`, `runner/README.md`, and the matching tests.
  - Behavior delivered:
    - A run that fails or is blocked now commits what the agent produced (`wip(failed):` / `wip(blocked):`) and, when the packet grants `mayPush`, publishes it to a per-execution `wip/<executionId>` branch. `deliver` (the packet's branch, upstream set) is now reached only by a genuinely completed run — previously a `blocked` result still delivered.
    - The terminal payload of a non-delivering run carries `wipCommit`, `wipPushed`, `wipBranch`, and a bounded `logTail`; `branch`/`pushed` stay absent/false. `wipBranch`/`wipCommit`/`wipPushed` are also in the reduced payload the emit-failure retry sends; `logTail` deliberately is not.
    - The commit is anchored at `refs/moirai-wip/<executionId>` in the runner's own repository for **every** non-delivering run, published or not. Without that anchor the commit is unreachable as soon as the next preparation re-creates the execution branch from base (#136) — which matters most for the roles the orchestrator does not grant `mayPush`: today only `developer` has it, so a repairer's work is preserved locally only.
    - `LOOP_RUNNER_RETAIN_WORKSPACES` defaults to `failed`, so the worktree, `terminal-result.json`, and the agent logs survive a failed run — until the next execution of that same job prepares, which removes the directory it is about to reuse. Retention therefore covers inspection between a failure and the workflow's next attempt; the artefact that survives *across* attempts is the work-in-progress commit, not the workspace. Retention is registered in `<data>/retained` and released by a sweep bounded by age (`72h`), count (`10`), and free disk (`LOOP_RUNNER_MINIMUM_FREE_BYTES`).
    - The sweep only considers registered — therefore finished — workspaces, and skips any job claimed by a running execution. The claim is taken before the sweep in `Execute`, which is what makes it safe: one job ID serves every execution of a workflow run, so a record left by an earlier execution names the very path the next one prepares.
    - A retained workspace's HEAD is detached, so it can never be the reason a later `git worktree add -B` fails. A workspace that cannot be registered is cleaned up instead of kept; one that cannot be detached is still kept, because the collision it guards against is not reachable through today's `Prepare` (which removes the colliding directory first) and forensics are worth more than the guard.
  - Validation performed: see `Validation Status` below.

- [x] Fix `Manager.Commit`, which failed in every prepared workspace
  - Completed: 2026-07-29
  - Relevant files: `runner/internal/repository/delivery.go`, `runner/internal/repository/manager_test.go`, `runner/internal/repository/delivery_test.go`.
  - Behavior delivered: staging was `git add -A -- . :!.loop :!.loop/**`. Git reports an error and exits non-zero when a pathspec explicitly matches an ignored path, and `Prepare` puts `/.loop/` in the worktree's exclude file and then creates `.loop` — so *every* commit in a real workspace failed with `stage repository changes: exit status 1`. Staging is now `git add -A -- .` (the exclude file already keeps `.loop` out) followed by `git reset --quiet -- .loop`, which restores the belt-and-braces guarantee without the failure. Discovered while writing the real-git retention test; the existing tests missed it because none combined `Prepare` with `Commit`.
  - Note: this was a prerequisite, not a detour — with it unfixed, no work-in-progress commit could ever have been produced on a real runner.

## Decisions

- Decision: push semantics are decided by outcome — only a completed run writes to the packet's branch; failed and blocked runs publish to a per-execution `wip/<executionId>` ref; cancelled runs publish nothing.
  - Context: the issue requires the diff of a failed run to be recoverable, without letting a failed run look delivered. Three candidate targets existed: the packet's branch with a payload marker, a separate ref, or a local commit only.
  - Alternatives considered: (1) commit and push to the packet's branch with a `wip:` marker in the payload; (2) commit locally and never push.
  - Reason: (1) is unsafe twice over. `branch`/`pushed` in the terminal payload are the platform's "this run produced deliverable work" signal, and the next attempt of the same workflow re-creates that same branch name from the base revision (#136), so a later delivery push would be rejected as a non-fast-forward — a failed run would break the run that follows it. (2) leaves the work on one runner's disk, where a retry scheduled elsewhere cannot reach it. A per-execution ref is unique by construction, is never read as delivery, and cannot collide with the delivery branch. Because a *local* anchor costs one `git update-ref` and is the only thing that survives #136's branch reset, both are done: the anchor always, the push when permitted. Cancelled runs are excluded because their context is already cancelled: every git command would fail anyway, and the workspace is preserved by the `abandoned` retention policy instead.
  - Consequences: failed runs leave `wip/*` refs on the code host and `refs/moirai-wip/*` in the runner's repositories. Both are bounded by the number of non-delivering executions rather than by time, both are visible, and `LOOP_RUNNER_PUSH_WORK_IN_PROGRESS=false` suppresses the remote half for repositories where such refs are unwelcome. The push is `--force` because the ref belongs to exactly one execution and a redelivered execution must be able to replace its own earlier remains. Nothing yet deletes either; recovering and pruning them is orchestrator work (#106), noted in `runner/README.md`.
  - Scope note: `may_push = role == "developer"` (`workflows/task_packets.py`), so today only a developer failure is durable *across runners*. A repairer failure is durable only on the runner that produced it. Granting the repairer a publish path is orchestrator work and was out of scope here.

- Decision: retention is bounded by construction — a workspace is kept only if it can be registered, and the registry is swept by age, count, and free disk.
  - Context: the default had to change to keep failed workspaces, and "keep every failed run" on a long-lived runner fills the disk. `MinimumFreeBytes` already existed as a hard precondition for starting an execution, so unbounded retention would eventually have turned every offer into an "insufficient runner disk space" failure.
  - Alternatives considered: (1) keep failed workspaces with no bound and rely on operators to clean up; (2) scan `<data>/workspaces` directly instead of maintaining a registry; (3) sweep on the heartbeat.
  - Reason: (1) is the failure mode the issue explicitly rules out. (2) cannot distinguish a finished workspace from one belonging to a concurrently running execution (`LOOP_RUNNER_CAPACITY > 1`), so a sweep could delete a live worktree; a registry also carries the repository mode needed to unregister the worktree properly rather than merely unlinking it. A registry alone is *not* sufficient, though — see the consequences below. (3) adds a directory scan every 10s for a set that only grows when executions finish; startup plus pre-execution covers every moment new workspace disk is about to be consumed, and running it before the free-space check means retained forensics cost the next execution capacity instead of blocking it.
  - Consequences: retention now requires a data directory (`RetentionPolicy.Directory`); a `Dispatcher` constructed without one cleans up as before rather than leaking untracked workspaces. The registry is self-healing: records whose workspace is gone, or which are unparsable, are discarded on the next sweep. **The adversarial review found that dropping a reused job's record before `Prepare` was not enough on its own** — it is a check-then-act, and with `LOOP_RUNNER_CAPACITY > 1` another execution's sweep could have loaded that record first, blocked on the repository lock for the whole of the re-preparation, and then deleted the live worktree. `ActiveWorkspaces` closes it: `Execute` claims its job ID before it sweeps, and the sweep skips claimed jobs, so no workspace can be released while any execution owns it. An idle runner still holds workspaces past `MaxAge` until its next execution; the count bound keeps the footprint fixed regardless.

## Known Issues

- Issue: the recovery half of this feature is not implemented — the runner reports `wipBranch`/`wipCommit`, but nothing consumes them.
  - Severity: P2
  - Impact: acceptance criterion 1 is met (the diff is recoverable and named in the event payload), but a retry does not yet *automatically* build on it. Until the orchestrator fetches the ref, the benefit is manual recoverability plus the retained workspace.
  - Evidence: `grep -rn "wipBranch\|wipCommit" orchestrator/src` → no matches. `orchestrator/src/moirai/workflows/task_packets.py` still sends `currentCommit`/`diffSummary` empty.
  - Suggested resolution: [#106](https://github.com/alexandre-leites/moirai/issues/106) (Autonomy L3, populate task-packet context fields). Orchestrator-side, and orchestrator files were owned by other agents during this session, so deliberately untouched. The runner side is complete and documented in `runner/README.md`.

- Issue: pipeline commands never reach a file-modifying packet, so the issue's literal "pipeline-failed run retains its diff" path is unreachable through today's orchestrator.
  - Severity: P3 — naming, not behaviour.
  - Impact: none on the outcome the issue wants. `pipeline_task_execution` is the only producer of `pipeline=` and builds a `role="pipeline"` packet, which has `may_modify_files=False` and therefore no diff of its own to retain (the runner's own validator rejects a pipeline packet that could modify files). In production the retention path is exercised by a failing or blocked `developer`/`repairer` execution, which is where the discarded diff actually was. A developer packet carrying pipeline commands is legal, handled, and covered by tests; the orchestrator simply does not build it.
  - Evidence: `orchestrator/src/moirai/workflows/task_packets.py::pipeline_task_execution`; `runner/internal/taskpacket/taskpacket.go` (`RolePipeline` may not modify or push); `runner/internal/dispatch/dispatch.go` returns early for `RolePipeline` before any delivery.
  - Suggested resolution: none needed in the runner. Worth remembering when reading the F13 write-up, which describes the runner's developer-with-pipeline code path rather than a shape the orchestrator emits.

- Issue: a repairer's failed work is durable only on the runner that produced it.
  - Severity: P2
  - Impact: `may_push = role == "developer"`, so `retainWorkInProgress` cannot publish a repairer's commit. The local `refs/moirai-wip/<executionId>` anchor keeps it reachable in that runner's repository, and the terminal payload still reports `wipCommit`, but a retry scheduled on another runner cannot fetch it.
  - Evidence: `orchestrator/src/moirai/workflows/task_packets.py:131`.
  - Suggested resolution: orchestrator-side — either grant file-modifying roles a publish-only credential, or have #106 fetch from the reporting runner. Not attempted here: `orchestrator/` was owned by other agents this session, and widening `mayPush` in the runner would violate a packet constraint the runner is meant to enforce.

- Issue: interaction with [#136](https://github.com/alexandre-leites/moirai/issues/136) (`Prepare` force-resets the agent branch).
  - Severity: assessed, not blocking.
  - Impact: assessed and designed around, but it is the reason one piece exists. #136 means the work-in-progress commit's *branch* is rewound on the next preparation, and the retained *workspace* at `workspaces/job-<jobId>` is removed by that same preparation (one job ID per workflow run). So neither the branch nor the workspace is a durable carrier across attempts. What is durable: the pushed `wip/<executionId>` ref (developer runs), and the local `refs/moirai-wip/<executionId>` anchor (all non-delivering runs, same runner). The retained workspace covers inspection between the failure and the next attempt. This design does not depend on #136 being fixed, and does not break when it is.
  - Evidence: `TestRecordedWorkInProgressSurvivesTheNextPreparation` (real git) commits in a prepared workspace, anchors it, re-prepares the same job, and then asserts the anchor still resolves to the commit **and that the execution branch no longer does** — i.e. it fails if the anchor is removed, and self-invalidates if #136 is ever fixed underneath it. The branch-collision half is covered by `TestRetainedWorkspaceDoesNotBlockThePreparationOfTheNextExecution`; reproduced with plain git, `worktree add -B agent/x wt2 main` fails with `fatal: 'agent/x' is already used by worktree at .../wt1` while the first worktree holds it, and succeeds after `checkout --detach`.

## Validation Status

- Targeted tests: Passed — `go test ./internal/dispatch/ ./internal/repository/ ./internal/config/` in `runner/`.
- Service tests: Passed — `make test-runner` → all runner packages `ok` (`go test -race ./...`, 10 packages, `internal/metrics` has no test files).
- Formatting: Passed — `gofmt -l .` in `runner/` printed nothing.
- Static analysis: Passed — `go vet ./...` in `runner/` printed nothing.
- Real-git coverage (not stubs), because the guarantees are about refs and Git's own rules:
  - `TestRetainedWorkspaceDoesNotBlockThePreparationOfTheNextExecution` — `Prepare` → commit → `ReleaseBranch` → `Prepare` again with the same branch name; also asserts the commit excludes `.loop` and the retained worktree keeps its files.
  - `TestPushWorkInProgressPublishesFailedWorkWithoutAdvancingTheDeliveryBranch` — the work reaches `refs/heads/wip/execution-1` on the remote, the delivery branch does not exist there, no upstream is set, and a redelivered execution replaces its own ref.
- End-to-end within the runner: `TestControlLoopReportsRecoverableWorkAfterAPipelineFailure` drives an offer and lease acknowledgement through `ControlLoop`, and asserts the emitted `failed` event payload carries `wipBranch`, `wipCommit`, `wipPushed`, and `logTail`, carries no `branch`, that the agent branch was never pushed, that the work-in-progress push used the resolved `GITHUB_TOKEN` environment, and that the workspace was retained and registered.
- Payload bounds: `logTail` is sanitised (ANSI/control characters removed, invalid UTF-8 dropped) and cut to 2 KiB **as JSON encodes it**, not raw. The adversarial review found the raw bound insufficient: Go's encoder escapes `<`, `>`, and `&` to six bytes each, so 2 KiB of ordinary Python-traceback text (`<module>`) encoded to 12,302 bytes of the 16 KiB payload. `jsonEncodedSize` now models the encoder exactly (`TestJSONEncodedSizeMatchesTheEncoder` compares it against `json.Marshal` for quotes, backslashes, HTML-significant bytes, tabs, newlines, and multi-byte runes) and `TestLogTailBoundsAndSanitisesUnboundedOutput` asserts the encoded bound across four adversarial inputs. The failed terminal payload has 17 fields against the orchestrator's `MAX_PAYLOAD_FIELDS = 32`, guarded by an assertion in `TestTerminalPayloadReportsRetainedWorkSeparatelyFromDelivery`.
- Orchestrator, API, web, Compose, proto: not run. No file in those trees changed.
- Adversarial review: run against the full diff before the final commit. It found one P1 (the sweep/preparation TOCTOU above) and two P2s (the raw-vs-encoded log-tail bound, and documentation claiming retained workspaces survive into the next attempt when the same job's preparation removes them). All were fixed rather than documented away, except the last, which is a true limitation and is now stated as one. It also found that a stuck workspace aborted the disk-pressure loop (now stepped over), that discarding forensics when `ReleaseBranch` fails was the wrong trade (now retained with a warning), and that the age bound does not apply to an idle runner (claim corrected). Its verification that no failed run can reach the delivery branch, and that the `Manager.Commit` staging change is a fix rather than a regression, was reproduced independently with plain git.

## Next Recommended Implementation

[#106](https://github.com/alexandre-leites/moirai/issues/106) — consume the runner's new `wipBranch`/`wipCommit` in the orchestrator: fetch the work-in-progress ref when building a retry packet and populate `currentCommit`/`diffSummary` from it (`orchestrator/src/moirai/workflows/task_packets.py`, `runner_events.py`). That closes the loop this change opens: the runner now preserves and reports the work, but no retry yet starts from it. [#136](https://github.com/alexandre-leites/moirai/issues/136) should land alongside it so the fetched work is not reset away by the next preparation.
# Session: F5 / issue #92 — Circuit-breaker wedge states (branch `issue-92`)

## Current Status

- Overall status: Complete for finding F5.
- Current phase: Bug fix from the 2026-07-29 platform review (`docs/reviews/2026-07-29-platform-review.md`, F5, P1, marked *(verify)*).
- Active implementation: issue-92 agent session, 2026-07-29 — circuit-breaker wedge states.
- Last updated: 2026-07-29.
- Agent/session identifier: issue-92.

## Done

- [x] A half-open circuit can no longer be left without a live probe
  - Completed: 2026-07-29.
  - Relevant files: `orchestrator/src/moirai/persistence/circuits.py` (new), `orchestrator/src/moirai/persistence/control_plane.py`, `orchestrator/src/moirai/workflows/persistence.py`, `orchestrator/src/moirai/scheduler.py`, `orchestrator/src/moirai/services/issue_sync.py`, `orchestrator/tests/test_postgres_integration.py`, `orchestrator/tests/test_asyncpg_control_plane.py`, `orchestrator/tests/test_workflow_persistence.py`, `orchestrator/tests/test_scheduler_service.py`, `orchestrator/tests/test_issue_sync.py`, `README.md`, `orchestrator/README.md`.
  - Behavior delivered:
    - **Wedge 1 — partial claim.** `_claim_circuit_probes` now runs both claims inside its own `connection.transaction()`, which asyncpg issues as a `SAVEPOINT` because `schedule()` already holds the outer transaction. A claim that cannot complete raises `_CircuitProbeUnavailable`, which unwinds the savepoint and returns `False`; the caller's `return None` then commits an unchanged pair of circuit rows instead of a project stuck at `half_open` pointing at a workflow run that was never inserted.
    - The claim's `UPDATE` no longer requires `probe_workflow_run_id IS NULL`. The row is locked `FOR UPDATE` and `state = 'open'` means no probe is outstanding, so a pointer still on the row is stale by definition; requiring it to be NULL is what made the provider claim fail forever after a circuit had been closed and reopened (and is what triggered wedge 1 in production-shaped data).
    - **Wedge 2 — unresolved probes.** `cancelled` and `failed` now resolve a probe: `AsyncpgWorkflowPersistence._update_project_circuit` reopens the circuits the run was holding with a fresh `opened_at` and a cleared pointer. The two raw-SQL paths that take a run terminal without a workflow transition do the same — `_cancel_offered_job` when it cancels the run (the bootstrap run offer expiry cancels is typically the probe `schedule()` just claimed) and `_block_unanswered_run`. `_cancel_offered_job(cancel_run=False)` deliberately does not, because that run already resolved its circuits through `transition`.
    - Reopening is not counted as a new failure. A probe that was cancelled, failed, or never answered produced no evidence about the project or provider; it only restarts the cooldown so a later probe can run.
    - `AsyncpgWorkflowPersistence.load_state` releases the probe of a run it finds already terminal (except `completed`), which is the same compensation it already performs for the project lock. This covers the third terminal writer: `accept_event` sets `blocked`/`cancelled` directly on `app.workflow_runs`, and `PersistedWorkflowRuntime.run` returns early for a run that is already terminal, so `transition` — and every circuit write inside it — is never reached for that run. `completed` is excluded because a delivered probe must *close* its circuit, which only `transition` does.
    - **Reaper.** `AsyncpgControlPlane.reap_orphaned_circuit_probes(now)` reopens any `half_open` row claimed longer ago than the cooldown whose probe workflow is missing or already terminal, and returns per-table counts. `Scheduler.tick` calls it ahead of the candidate query, since a half-open circuit is precisely what excludes a project (or a whole provider) from scheduling; that pass is already leader-gated by `AsyncpgLeader` in `main.py`, so exactly one orchestrator reopens them. A probe workflow that is still running is never reaped.
    - **Wedge 3 — stale pointers.** `clear_provider_failure` and `record_provider_failure` both clear `probe_workflow_run_id`, and every statement that resolves a probe is now guarded on `state = 'half_open'` as well as the pointer. A workflow that outlived its claim can no longer reopen — or close — a circuit that was decided on newer evidence. `record_provider_failure` against a `half_open` circuit now reopens it with a fresh cooldown instead of silently closing it.
    - Every writer that only *releases* a probe — the `cancelled`/`failed` transition, `load_state`, both offer-release paths and the reaper — shares `orchestrator/src/moirai/persistence/circuits.py`. The two verdict-bearing statements in `workflows/persistence.py` (close on `completed`, reopen-and-count on `blocked`) necessarily keep their own SQL, and each repeats the `AND state = 'half_open'` guard; `test_a_stale_pointer_cannot_flip_a_circuit_that_is_no_longer_half_open` covers both independently.
    - **Provider-failure sources (step 5).** Label-reconciliation failures raise the new `LabelReconciliationError` and no longer open the provider circuit; see Decisions. The provider circuit is also decided once per sync pass, from provider-wide evidence, instead of per project — see Decisions and Post-review corrections, because the first attempt at this regressed.
  - Validation performed: ten PostgreSQL integration tests written first and confirmed failing against `origin/main`, then confirmed passing; nineteen unit tests; full orchestrator suite; lint; type check.
  - Commands executed (throwaway PostgreSQL 16 container on port 55492, removed afterwards):
    - Failing-test-first. Re-confirmed at the end against a pristine copy of `origin/main` at `30d8483` (`git archive origin/main orchestrator/src orchestrator/migrations` into a scratch tree, then `PYTHONPATH=<scratch>/orchestrator/src … -k CircuitBreaker` against a fresh database):
      → `Ran 10 tests … FAILED (failures=8, errors=2)` — every new integration test fails without the change. Per wedge:
      - Wedge 1: `test_a_partial_probe_claim_is_never_committed` → `AssertionError: 'half_open' != 'open'` (the project claim survived a failed provider claim), and `test_a_stale_provider_probe_pointer_does_not_wedge_the_project` → `AssertionError` on `scheduled is not None` (`schedule()` returned None and left the project half-open).
      - Wedge 2: `test_a_cancelled_probe_reopens_the_circuits_and_allows_a_later_probe` and `test_a_failed_probe_workflow_reopens_the_circuits` → `AssertionError: 'half_open' != 'open'`. `test_orphaned_half_open_probes_are_reaped_after_the_cooldown` and `test_a_live_or_recent_probe_is_never_reaped` → `AttributeError: 'AsyncpgControlPlane' object has no attribute 'reap_orphaned_circuit_probes'`.
      - Wedge 3: `test_a_closed_provider_circuit_is_not_reopened_by_a_stale_probe` and `test_recording_a_provider_failure_drops_the_probe_pointer` → `AssertionError: UUID('…') is not None` (the pointer survived closing the circuit); `test_a_stale_pointer_cannot_flip_a_circuit_that_is_no_longer_half_open` → `AssertionError: 'closed' != 'open'` (a stale workflow closed an open provider circuit). The last one was also checked against a copy with only the two `AND state = 'half_open'` guards removed, so it is not passing on the strength of the pointer clearing.
      - The `load_state` compensation: `test_a_terminal_status_written_outside_the_transition_path_releases_the_probe` → `AssertionError: 'half_open' != 'open'`. Also checked in isolation against a copy with only that one hunk removed, so it is not passing on the strength of the other fixes.
    - After the fix: `LOOP_TEST_DATABASE_URL=… make test-postgres-integration` → `Ran 19 tests … OK`.
    - `make test-orchestrator` → `Ran 388 tests … OK (skipped=19)`.
    - `make lint` → `All checks passed!`
    - `make typecheck MYPY_CACHE=/tmp/moirai-mypy-cache-issue-92` → `Success: no issues found in 48 source files`.
  - Notes: no migration and no new configuration. The probe cooldown is still `AsyncpgControlPlane(circuit_probe_cooldown=…)`, five minutes by default, and the reaper reuses it as its grace period.

## Post-review corrections

An adversarial review of the first commit (`a737315`) confirmed the three wedge fixes and the atomicity of the claim, but found one real regression and four inaccurate claims. All were fixed before the PR was opened.

- **Regression, now fixed (two rounds).** Moving the provider-circuit verdict out of the per-project loop removed the *clear* that used to happen on every healthy project's sync, but kept recording a failure per failing project. One permanently broken project beside a healthy one — a deleted repository, a bad URL — therefore accumulated a provider failure every pass with nothing clearing it, opening the shared circuit on the third pass and halting scheduling for every project on GitHub. `main` did not do this. Reproductions: `test_a_permanently_broken_project_never_accumulates_provider_failures` and `test_one_broken_project_never_opens_the_shared_provider_circuit`, both confirmed failing against that design (`AssertionError: Lists differ: [('github', 'issue tracker failed for proj…')] != []`).
  A second review round showed the first correction — record when every *attempted* project failed — still let the same thing happen through the backoff: a project already backing off is not attempted, so the projects that remain become the only evidence, and one of them failing reads as a fleet-wide failure. The reviewer's case used a token that cannot write `agent:*` labels, so the very error `LabelReconciliationError` exists to keep off the provider circuit opened it indirectly. The verdict now requires the whole enabled fleet: `test_a_backed_off_project_suppresses_the_provider_verdict` reproduces it and fails against the intermediate rule with the same `Lists differ` assertion, and `test_a_full_outage_records_a_failure_on_every_pass` pins the other side — a real outage still records on every pass and opens the circuit on the third.
- **Untested guard, now covered.** The review showed that reverting the `AND state = 'half_open'` guards in `workflows/persistence.py` broke no test: clearing the pointer everywhere removed the only route the existing tests took to a stale pointer. Added `test_a_stale_pointer_cannot_flip_a_circuit_that_is_no_longer_half_open`, which seeds the pointer directly the way an older orchestrator or a restored backup would. Without the guards it fails with `AssertionError: 'closed' != 'open'` — a workflow that no longer owned the probe closing an *open* provider circuit, re-enabling scheduling in the middle of an outage.
- **Near-vacuous test, now replaced.** `test_the_probe_claim_runs_in_its_own_savepoint` only counted `connection.transaction()` calls and passed whether or not the claim failed. The fake now injects a failing provider claim (`_DurablePool.provider_claim_fails`), and the test asserts the claim is reported as failed, that both statements were attempted, and that it ran in its own nested transaction; `test_schedule_places_no_offer_when_a_probe_claim_fails` covers the caller. What the savepoint actually discards is proved against PostgreSQL by `test_a_partial_probe_claim_is_never_committed`, and the unit test now says so instead of implying it proves it.
- **Three false or overstated claims in comments, corrected.** `circuits.py` said the cooldown grace "keeps a probe that is merely between its claim and its first write safe" — a live probe is protected by the `NOT EXISTS` clause at any age; the grace is for a *terminal* probe whose resolution is late. It also said a probe "deleted with its project" leaves an orphan on both tables; only `app.provider_circuit_state` can dangle, because `app.project_circuit_state` cascades with the project. `reopen_probe_circuits`'s docstring said it is called "when it is blocked", which contradicts the module's own rule that reopening is never counted as a failure — a blocked probe going through `transition` does not come here. `workflows/persistence.py` said "a project circuit is never opened by these statuses", conflating "not opened" with "not counted as a failure".

## Decisions

- Decision: label-reconciliation failures gate issue-sync backoff only, never the provider circuit.
  - Context: step 5 of issue #92. `sync_all_projects` caught every `IssueSyncError` alike and called `record_provider_failure("github", …)`; three of those open the provider circuit, and `schedule()` then excludes every project on that provider. `reconcile_project_labels` runs inside the same `try`, so a refused `gh issue edit --add-label` halted scheduling platform-wide.
  - Alternatives considered: (a) leave it, treating any tracker error as provider evidence; (b) drop the backoff for label failures too and retry labels every cycle; (c) give label failures their own circuit.
  - Reason: the provider circuit exists to stop burning agent executions against a broken provider. A failed label write is the opposite evidence — the pass had just listed that project's issues successfully, so the read path scheduling depends on is working, and the `agent:*` labels are a status mirror that no workflow consumes. (b) removes a real safeguard against hammering a tracker that is rejecting writes; (c) adds a durable state machine for something that self-heals on the next pass.
  - Consequences: `LabelReconciliationError` (a subclass, so `sync_project`'s existing contract is unchanged) backs that project's sync off with the existing exponential delay and is reported in the pass results, but the provider circuit is untouched. Scheduling is now only gated by failures to *read or persist* issues. A persistent label failure is visible in `app.issue_sync_state.last_error` and in the pass result rather than as an unexplained scheduling halt.

- Decision: the shared provider circuit is decided once per sync pass, from provider-wide evidence only.
  - Context: the review's second half of wedge 3 — `clear_provider_failure` ran inside the per-project success arm and `record_provider_failure` inside the per-project failure arm, so with several projects the same pass could record a failure and then erase it, decided by nothing but the order `list_enabled_projects` returned.
  - Alternatives considered: (a) keep per-project clearing and rely on the next failing pass to reopen the circuit; (b) clear only when the pass had a success *and* no failure, otherwise record per failing project; (c) record when every *attempted* project failed.
  - Reason: the row is global, so only a whole-pass verdict about *the provider* is meaningful. Two adversarial review rounds killed (b) and (c) before the PR, in the same direction each time — a fault belonging to one project must not be readable as a provider outage. Under (b), one permanently broken project (deleted repository, bad URL) beside healthy ones records a provider failure every pass with nothing ever clearing it, opening the shared circuit after three passes: a regression against `main`, where the healthy project's success cleared it each pass. (c) survives that but not the backoff: a chronically broken project drops out of later passes, so the projects still being attempted become the only evidence, and one of *them* failing reads as "every attempted project failed". The reviewer built that case from a token that cannot write `agent:*` labels — the very failure `LabelReconciliationError` exists to keep off the provider circuit, opening it indirectly through its own backoff. The verdict therefore requires the whole enabled fleet.
  - Consequences: any project that syncs clears the circuit (the provider answered); a failure is recorded, once, only when every enabled project was attempted and all of them failed; every other pass writes nothing. The accepted cost is the mirror error: a project stuck in a long backoff suppresses the verdict, so a genuine outage may not open the circuit at all. That is the safer failure — failing to open wastes agent executions that the per-project and per-run breakers still bound, while opening wrongly stops every project on the provider until a human intervenes, which is the wedge class this issue exists to remove. Measured, not assumed: during a full outage every project fails together, the first backoff delays (5s, 10s, 20s) are all shorter than the one-minute interval `main.py` runs issue sync on, so no project is skipped and the circuit opens on the third pass (`test_a_full_outage_records_a_failure_on_every_pass`). Detection is 1–2 passes slower than `main`, which recorded once per failing project and so opened on the first or second pass in a multi-project deployment. A single-project deployment is byte-identical to `main`: with one project, "every enabled project failed" and "this project failed" are the same statement.

- Decision: the orphan-probe reaper runs in `Scheduler.tick`, not in the workflow maintenance loop.
  - Context: `main.py` has a separate `_run_workflow_maintenance_loop` that drains the transition outbox and recovers stalled runs. `main.py` is owned by a concurrent agent on issue #94.
  - Alternatives considered: add it to the workflow maintenance loop.
  - Reason: it belongs with scheduling on the merits, not only for file ownership. What an orphaned probe blocks is the candidate query in `schedule()`, and `Scheduler.tick` already runs the other two pre-scheduling sweeps (`expire_offers`, `expire_leases`) under the same leader lock. It is attached via `getattr`, matching those two, so control-plane fakes without the method are unaffected.
  - Consequences: the reaper runs on the scheduler interval rather than the maintenance interval. Both are leader-gated in `main.py`, so there is no double execution.

- Decision: a claim never inspects `probe_workflow_run_id`; `state = 'open'` is the whole predicate.
  - Context: with the pointer cleared on every close and reopen, the only rows that can still carry a stale pointer are ones written before this change (or by an older orchestrator during a rolling restart).
  - Alternatives considered: keep `AND probe_workflow_run_id IS NULL` and rely on the reaper to clean up. The reaper only visits `half_open` rows, so an `open` row with a stale pointer would have stayed unclaimable forever.
  - Reason: `open` and `half_open` already encode "no probe outstanding" and "one probe outstanding". Making the pointer a second, weaker source of truth for the same fact is what produced wedge 1.
  - Consequences: a stale pointer on an `open` row is overwritten by the next claim instead of vetoing it. It cannot steal a live probe, because a live probe means `state = 'half_open'`, which the pre-check under `FOR UPDATE` rejects before any write.

## Validation Status

- Targeted tests: Passed — 10 new PostgreSQL integration tests (`CircuitBreakerIntegrationTests`) and 19 new unit tests. All 10 integration tests were re-confirmed failing against `origin/main` at `30d8483` after the merge: `Ran 10 tests … FAILED (failures=8, errors=2)`.
- Service tests: Passed, after merging `origin/main` (`30d8483`, issue #90) — `make test-orchestrator` → `Ran 388 tests … OK (skipped=19)`; `LOOP_TEST_DATABASE_URL=… make test-postgres-integration` → `Ran 19 tests … OK` against a fresh database.
- Full repository tests: Not run — no Go, proto, or web change. `make test` was deliberately not invoked.
- Build: Not applicable — Python only.
- Lint: Passed — `make lint`.
- Type checks: Passed — `make typecheck MYPY_CACHE=/tmp/moirai-mypy-cache-issue-92` (own cache directory, so it cannot collide with a sibling worktree).
- Database migrations: Not applicable — `probe_workflow_run_id` already exists (migration `006_circuit_half_open_probes.sql`). Migrations were run against the throwaway database by the integration suite.
- Docker Compose: Not run — no Compose or configuration change.
- End-to-end workflow: Not run.

## Known Issues

- Issue: `runner/internal/dispatch`'s disconnected-delivery tests are flaky on GitHub-hosted runners.
  - Severity: P2 — it failed all three CI runs of PR #138 and one re-run (4 failures in 6 job executions), so it will keep costing re-runs on unrelated PRs.
  - Impact: the `runner` job failed on both CI runs of this branch and passed on a re-run of the same commit each time. Unrelated to this change: the branch modifies zero Go files (`git diff origin/main...HEAD --name-only` lists only Python, tests and Markdown).
    - Run 30426255735: `--- FAIL: TestControlLoopDeliversTerminalEventAfterLeaseExpiryWhileDisconnected (0.62s)` → `control_loop_test.go:656: delivered events = []*runnerv1.ExecutionEvent(nil), want the terminal event`. Re-run: `runner pass 23s`.
    - Run 30426469159: `--- FAIL: TestControlLoopDeliversTerminalEventAfterLogsSaturateTheBufferWhileDisconnected (0.01s)` → `control_loop_test.go:718: delivered events = […two events…], want the terminal event last`. Re-run: `runner pass`.
    - Run 30426694743: `TestControlLoopDeliversTerminalEventAfterLeaseExpiryWhileDisconnected` again; the first re-run failed identically and the second passed. Final state of PR #138: all ten checks pass, `mergeStateStatus: CLEAN`.
  - Evidence: not reproducible locally — `cd runner && go test -race -count=20`, `GOMAXPROCS=2 -count=60`, and `GOMAXPROCS=1 -count=40` against six busy-loop processes all report `ok` for both tests. In the failures `client.events` is empty immediately after a synchronous `loop.FlushEvents()`, while the preceding assertion confirms the terminal event *was* in the crash-safe outbox — so the flush is not picking up an outbox entry it should, rather than the assertion racing ahead of a background sender. Both tests were added by issue #93 (`8956d84`); `main` was green on the same runner code at `30d8483`.
    Not investigated further here because `runner/` is outside this session's ownership and was being worked on concurrently.
  - Suggested resolution: belongs to whoever owns `runner/internal/dispatch`. Worth filing as its own issue: start from why `FlushEvents` can return without delivering an outbox entry that is present on disk, since that would be a durability defect rather than only a test defect.

- Issue: `make test-postgres-integration` is not repeatable against the same database.
  - Severity: P3
  - Impact: the second run of the file against one database fails `OfferExpiryIntegrationTests.test_expired_recovery_offer_returns_the_run_to_recovering` with two expired leases instead of one.
  - Evidence: pre-existing, and reproduced on unmodified `main` in this worktree — with the whole change stashed, run 1 was `Ran 9 tests … OK` and run 2 `Ran 9 tests … FAILED (failures=1)`. `PostgreSQLPersistenceIntegrationTests.test_control_plane_persists_project_runner_issue_and_lease_lifecycle` leaves a job in `preparing` with `lease_expires_at = _NOW + 10min`, which the next run's global `expire_leases` picks up. CI creates a fresh Postgres service per job, so it is green there. This is the same class of leftover already recorded under the issue #99 session. `CircuitBreakerIntegrationTests` is repeat-safe: it deletes both circuit tables and disables seeded projects in `asyncSetUp` and again on cleanup.
  - Suggested resolution: have that lifecycle test clean up its job and workflow run, or truncate the `app` schema in `asyncSetUp`.

- Issue: a `blocked` probe reported through the control plane's runner-event path is not counted as a project-circuit failure.
  - Severity: P3
  - Impact: `accept_event` writes some terminal statuses straight to `app.workflow_runs` (`workflows/runner_events.py` returns `new_status` `blocked` or `cancelled`), and `PersistedWorkflowRuntime.run` returns early for a run that is already terminal, so `AsyncpgWorkflowPersistence.transition` never runs for it. This change makes such a run *release* its probe (via `load_state`) so nothing wedges, but the failure counter that opens the circuit in the first place is still only incremented on the transition path.
  - Evidence: `orchestrator/src/moirai/persistence/control_plane.py` `accept_event`'s `UPDATE app.workflow_runs SET status = $2`; `orchestrator/src/moirai/workflows/runtime.py` `run()`'s `_TERMINAL_STATUSES` short-circuit before any `transition` call.
  - Suggested resolution: belongs with issue #94, which owns `accept_event` and the execution-request lifecycle — either have that path enqueue a real transition or let the runtime run the terminal transition instead of returning early. Deliberately not attempted here: it means restructuring `accept_event`, which this session was scoped out of.

## Next Recommended Implementation

Issue #96 (finding F9) — make transition replay idempotent. It is the highest-priority platform-review issue with no `ai-working` label as of this session (#90, #92, #94 and #100 were all claimed). The transition outbox is at-least-once, but `_dispatch` in `workflows/nodes.py` increments attempt counters and `total_agent_executions` even when it reuses a queued request, and creates a duplicate request for the same role when the previous one already moved to `dispatched`; outbox rows set to `processing` are never retried after a crash. Relevant files: `orchestrator/src/moirai/workflows/nodes.py`, `orchestrator/src/moirai/persistence/control_plane.py` (`drain_pending_transitions`, `_dispatch`). Expected behavior: replaying one transition twice leaves the same counters and the same single execution request. Targeted validation: new cases in `orchestrator/tests/test_workflow_nodes.py` and `orchestrator/tests/test_asyncpg_control_plane.py`, plus a PostgreSQL integration test that drains the same outbox row twice. Those budgets are what open a project circuit in the first place, so double-counting them is what makes the breaker above fire early.

---

# Session: F3 follow-up / issue #136 — an execution's workspace is force-reset to the default branch (branch `issue-136`)

- Agent/session identifier: runner-agent-issue-136
- Last updated: 2026-07-29
- Branch: `issue-136`
- Scope: `runner/internal/repository/` and `runner/README.md` only. No orchestrator, API, web, proto, or Compose file was touched, and no file under `runner/internal/dispatch/` or `runner/internal/agents/`.

## Done

- [x] Prepare a workspace from the job's execution branch instead of force-resetting it to the default branch (#136)
  - Completed: 2026-07-29
  - Relevant files: `runner/internal/repository/manager.go`, `runner/internal/repository/manager_test.go`, `runner/internal/repository/delivery_test.go`, `runner/README.md`.
  - Behavior delivered:
    - `Prepare` now resolves an explicit base revision before creating the worktree: the execution branch as published on the remote (found with `git ls-remote --heads origin refs/heads/<branch>`, then fetched) if it exists; otherwise the execution branch in this runner's own repository; otherwise the default branch. `git worktree add -B <branch> <workspace> <base>` is still what creates the worktree, but the start point it resets onto is now the branch's own tip whenever that tip exists, so the previous execution's commits survive.
    - `prepareBaseRevision` names the start point per repository mode: a managed cache is a `--mirror`, so the remote's branches *are* its `refs/heads/*`; an existing checkout keeps them under `refs/remotes/origin/*`, because its own `refs/heads/*` belong to whoever works there. Both modes are covered by real-git tests.
    - `git worktree prune` moved ahead of every fetch. Git refuses to fetch into a branch a worktree claims — including a *stale* registration whose directory `Prepare` has just removed — so pruning after the fetch would have made every second preparation of a job fail once the execution branch became a fetch target (`fatal: refusing to fetch into branch 'refs/heads/agent/…' checked out at …`, reproduced with real git before the ordering was changed).
    - The credential environment is validated before the workspace is removed or anything is fetched, so an unusable credential fails the execution instead of first destroying the workspace the previous one left behind.
    - "The remote branch does not exist" is read from the *output* of a successful `ls-remote`, and "the local branch does not exist" from the output of `for-each-ref`, never from an exit status. An unreachable or unauthenticated remote therefore fails the preparation loudly instead of degrading into "start from the default branch", which is the bug this issue is about.
  - Validation performed: see `Validation Status` below.
  - Adversarial review: a reviewer ran against the first version of this change and reproduced a defect in it — the published branch was fetched unconditionally over `refs/heads/<branch>`, which discarded the commit of a *completed* execution whose role may not push (no `refs/moirai-wip` anchor is written for a run that completes). The resolution rule and the fetch were both changed in response; see the second Decision below. Its other findings: the swallowed `worktree prune` error, the stale comments in `dispatch.go`, and the `managed_clone` push defect are recorded under `Known Issues`; its checks of the ls-remote/for-each-ref parsing, the credential handling, the lock coverage and the honesty of the rewritten #100 test found nothing.

- [x] Update #100's work-in-progress anchor test, whose premise this fix removes
  - Completed: 2026-07-29
  - Relevant files: `runner/internal/repository/delivery_test.go`.
  - Behavior delivered: `TestRecordedWorkInProgressSurvivesTheNextPreparation` asserted that *every* preparation rewound the execution branch off the failed run's commit; its author wrote it to self-invalidate if #136 were fixed, and it did — the guard fired on the first run of the new code. It is now `TestRecordedWorkInProgressSurvivesAPreparationThatMovesTheBranch`: another runner publishes the branch, this runner's preparation resets its local branch onto the published tip, and only `refs/moirai-wip/<executionId>` still reaches the failed run's commit. The anchor keeps a real job, narrowed to the case the branch cannot cover; the case it used to cover (a retry on the same runner inheriting the failed work) is now provided by the branch itself and is asserted in `TestPrepareResumesAJobFromThePreviousExecutionsWork`.

## Decisions

- Decision: the execution branch, not the workspace directory, carries a job's work between its executions. Every execution still gets a freshly created workspace; it is created from the branch tip.
  - Context: step 1 of the issue asks for the lifecycle to be chosen explicitly — one workspace per job kept across executions, or re-creation from the branch tip with a default-branch fallback for the first execution.
  - Alternatives considered: (1) keep `workspaces/job-<jobId>` across the executions of a job and re-use it; (2) re-create the workspace from the branch tip; (3) re-create from a per-execution ref such as `wip/<executionId>`.
  - Reason: (1) cannot work as the system is built. `Dispatcher.Execute` removes the workspace when an execution ends unless retention keeps it, retention is bounded by age, count and free disk, and — decisively — the executions of one job are leased independently, so the next one may run on a different runner that has never seen the directory. A directory is local; a branch is not. (3) makes the runner depend on the orchestrator naming the right predecessor execution, which nothing does today (#106), and the branch name is already stable per job. (2) needs nothing new: the orchestrator already gives every execution of a job the same `agent/<issueExternalId>/<jobId>` branch, and `deliver` already pushes it.
  - Consequences: preparation costs one extra `ls-remote` round trip per execution, and one extra `fetch` when the branch is published. A job's branch accumulates the commits of its executions rather than one commit rewritten repeatedly, so the delivery push is a fast-forward instead of the non-fast-forward #100 had to design around. The workspace directory keeps exactly the meaning it had before: scratch space plus forensics until the next execution of the same job removes it.

- Decision: the published tip decides the branch, except when this runner's own copy already contains it — then the local copy wins.
  - Context: **this decision was rewritten after the adversarial review, which reproduced a defect in its first form.** The first form was "the published branch always outranks the local copy", justified by "the only way the local branch legitimately leads the remote is work that was never published, and #100 anchors that". That justification is wrong: `retainWorkInProgress` writes a `refs/moirai-wip/<executionId>` anchor only for a run that *failed or blocked*. A `repairer` or any other file-modifying role without `mayPush` can **complete**, committing to the execution branch and publishing nothing, and no anchor is written for it. Force-fetching the published tip over that branch left the commit on no reference at all and the following pipeline execution validating the pre-repair tree — the exact symptom #136 is about, re-created inside its own fix. Reproduced by the review in both repository modes.
  - Alternatives considered: (1) the published tip always wins (the first form, now known to lose work); (2) the local copy always wins (a job's start point would depend on *where* it last ran, and a runner would ignore work another runner delivered); (3) prefer the local copy only when it already contains the published tip, remote otherwise.
  - Reason: (3) is the only rule under which no commit is silently dropped. A local copy that contains the published tip is that tip plus work only this runner has, so nothing is lost by keeping it and real work is lost by discarding it. In every other case — this runner behind, or the two diverged — the published tip is what every runner resolves identically, and the local commits it leaves behind came from runs that did not complete, which #100 does anchor. The comparison is `git rev-list --count <local>..<published>` rather than `git merge-base --is-ancestor`, whose answer is an exit status that a genuine Git failure is indistinguishable from.
  - Consequences: the published tip is fetched into `refs/moirai-remote/<branch>`, never over `refs/heads/<branch>`, and the fetch carries `--refmap=` — without it a mirror's configured `+refs/*:refs/*` force-updates the branch anyway (`+ c49f372...0fe5744 (forced update)`, reproduced), which is what made the first form destructive. The local tip is additionally read *before* the fetch and carried as a revision, so the start point cannot depend on the fetch having left the branch alone. Two tests pin this: `TestPrepareKeepsWorkBuiltOnTopOfThePublishedExecutionBranch` (both modes; fails without the containment check) and `TestPrepareBaseRevisionLeavesTheExecutionBranchWhereItIs` (fails without `--refmap=`). `refs/moirai-remote/*` accumulates one reference per job branch and nothing prunes it, like `refs/moirai-wip/*`. The accepted cost of the rule is its mirror image: a *deliberate* rewind of an execution branch — someone force-pushing it backwards to drop a commit — is ignored by a runner whose own copy still contains the older tip, because that is indistinguishable from "this runner has work nobody published". Execution branches are machine-owned and short-lived, and the alternative loses a completed repairer's work on every job, so the trade is made knowingly; a human who wants the runner to forget work should delete the branch, which resets the job to its first-execution behaviour.

- Decision: a `wip(failed):` commit inherited from a previous execution may reach the delivery branch, and that is accepted rather than filtered.
  - Context: `retainWorkInProgress` commits what a failed or blocked run produced onto the execution branch. Before this change the next preparation discarded it; now the next execution on the same runner starts from it, and if that execution completes, its delivery push publishes the `wip(failed):` commit as part of the branch's history. `dispatch.go`'s comment still says a work-in-progress commit on the delivery branch "would be rejected as a non-fast-forward push on the following attempt" — that was true only because the branch was reset; it is no longer.
  - Alternatives considered: (1) squash or drop `wip(*)` commits during preparation; (2) start from the published tip whenever the local tip is only work-in-progress.
  - Reason: (1) means the runner rewriting an agent's history on a heuristic (a commit-message prefix), and it destroys the very continuity `#100` asked for — its own comment says the commit exists "so a retry can build on it instead of starting from the base branch". (2) is the defect corrected in the decision above, in another disguise. The honest reading is that a `wip(failed):` commit *is* part of how the work reached its final state, and a reviewer seeing it in the branch history is being told the truth.
  - Consequences: a job's delivered history depends on which runner picked up its retries — the same runner inherits its own failed attempt's commit, another runner does not (it sees only what was published). That asymmetry is inherent to a fleet in which non-`developer` roles cannot push, and it disappears when they can (#106 or a `mayPush` grant). Recorded rather than hidden; a squash, if wanted, belongs to delivery or the orchestrator's pull-request step.

- Decision: the first execution's default-branch start point is stated, not implied.
  - Context: step 3 of the issue. `git worktree add -B <branch> <path> <defaultBranch>` produced the right result for a first execution and the wrong one for every execution after it, and the two cases were indistinguishable in the code.
  - Alternatives considered: `git worktree add` without `-B` and let Git decide.
  - Reason: dropping `-B` fails outright when the branch already exists elsewhere in the repository, and would leave "first execution starts from the default branch" as an accident of Git's checkout rules rather than a decision the runner makes. `prepareBaseRevision` now returns the default branch only after establishing that the execution branch exists neither on the remote nor locally, and says so.
  - Consequences: the branch resolution is one function with four named outcomes (published tip, local copy containing it, published tip on divergence, default branch), each covered by a test that fails if the outcome changes.

## Validation Status

- Targeted tests: Passed — `cd runner && go test -race ./internal/repository/`. Eight new real-git tests cover the two acceptance criteria in both repository modes (`TestPrepareStartsAJobWithoutAnExecutionBranchFromTheDefaultBranch`, `TestPrepareResumesAJobFromThePreviousExecutionsWork`, `TestPrepareResumesAJobFromTheBranchPublishedByAnotherRunner`, `TestPrepareResumesAnExistingPathJobFromItsExecutionBranch`, `TestPrepareResumesAnExistingPathJobWhoseCheckoutTracksOneBranch`), `TestPrepareKeepsWorkBuiltOnTopOfThePublishedExecutionBranch` (a sub-test per repository mode) pins the defect the adversarial review reproduced — a completed non-pushing execution's commit on top of a published branch — and fails in both modes when the containment check is removed; `TestPrepareBaseRevisionLeavesTheExecutionBranchWhereItIs` fails when `--refmap=` is dropped from the execution-branch fetch (`resolving the base revision moved the execution branch`); and `TestPrepareResumesAJobWhosePreviousWorkspaceWasNeverCleanedUp` pins the prune-before-fetch ordering — moving the prune back after the branch is resolved makes it fail with `fatal: refusing to fetch into branch 'refs/heads/agent/issue-7/run-1' checked out at …`. Plus stub tests pinning the command sequence and the credential environment of the two networked commands the fix adds. Re-run against the *unmodified* `manager.go` from `09a6c1b` in this worktree, the resume tests fail, each landing on the default-branch tip, while the first-execution test passes — the behaviour the fix had to preserve. The adversarial reviewer independently repeated that check and counted eight failures against `origin/main`'s logic. `TestPrepareResumesAnExistingPathJobWhoseCheckoutTracksOneBranch` was likewise confirmed to fail when the execution-branch fetch is weakened to `git fetch origin <branch>` (`fatal: invalid reference: refs/remotes/origin/agent/issue-7/run-1`).
- Service tests: Passed — `make test-runner` (`go test -race ./...`) on the merged tree at `21c985a`, all packages `ok`. This is the *only* validation available for the final commits: GitHub Actions is failing every job in the repository, `main` included (see `Known Issues`). The last CI run that executed normally, at `0d0e9e1`, passed all ten checks.
- Full repository tests: Not run — the change is Go-only, inside `runner/`. `make test` would additionally build the web and orchestrator suites, which no file here touches.
- Build: Passed — `cd runner && go build ./...`.
- Lint: Passed — `gofmt -l .` (no output) and `go vet ./...` in `runner/`.
- Type checks: Not applicable — Go only.
- Database migrations: Not applicable.
- Docker Compose: Not run — no Compose or configuration change.
- End-to-end workflow: Not run. The defect and the fix were both reproduced against real Git instead: the issue's plain-git reproduction was run first (`worktree add -B agent/42/abc ../wt2 main` after a commit in `wt1` yields `base`, and `agent/42/abc` equals `main`), and the equivalent through `Manager.Prepare` is now a test.

## Known Issues

- Issue: GitHub Actions is failing every job in this repository, on `main` as well as on this branch.
  - Severity: P1 for anyone reading CI, P0 for nobody's code — no job runs at all.
  - Impact: PR #146 cannot be shown green, and neither can `main`. Every job of a run ends `failure` after 1–10 seconds with **zero steps executed** and no log blob (`gh api .../logs` → `BlobNotFound`), which is what GitHub reports when a job is never allocated rather than when it runs and fails.
  - Evidence: run 30447670464 (this branch, `21c985a`) — all nine jobs `failure`, `steps=0`; re-running the failed jobs reproduced it identically. Run 30446858812 on **`main`** at `5903aa0` — the same nine jobs, `failure`, `steps=0`. The last run that executed normally was 30443831892 on this branch at `0d0e9e1`, where all ten checks passed. Nothing in between touched CI configuration except PR #150, which added one `make test-web` line to an otherwise unchanged `ci.yml` — and that cannot produce a zero-step failure in jobs that do not use it.
  - Suggested resolution: an account-level Actions problem (quota, billing, or runner availability) for a human to check. Nothing to change in the repository. Until it clears, validation for this branch is the local run recorded above; the branch's own code is identical to `main` outside `runner/` and `PROGRESS.md`, so a green `main` would be a green branch.

- Resolved during the session: two comments in `runner/internal/dispatch/dispatch.go` described behaviour this change removes.
  - What they said: the doc comment on `retainWorkInProgress` and the comment above its `RecordWorkInProgress` call both justified their design with "the next preparation of that job re-creates the branch from the base revision", and the former added that a work-in-progress commit on the delivery branch "would be rejected as a non-fast-forward push on the following attempt". Neither is true once the branch is continued rather than reset.
  - How it was handled: `dispatch.go` was owned by the concurrent issue #97 session when this work started, so the finding was first recorded here rather than fixed. #97 has since merged (PR #151) and PR #146 is the only open non-dependabot pull request, so the ownership constraint was re-checked with `gh pr list` and both comments were corrected in this branch. The change is comment-only; no behaviour in `dispatch.go` was touched. The equivalent comment in `runner/internal/repository/delivery.go` was corrected too, and `runner/README.md` documents the real ordering.

- Issue: every `git push` from a `managed_clone` workspace fails, so the execution branch is never published in that mode.
  - Severity: P1 — it breaks delivery itself, not only this issue's cross-runner half.
  - Impact: `Manager.Push`, `Manager.PushWorkInProgress` and `Manager.CleanupRemoteBranch` all push with a refspec, and the workspace is a worktree of a `git clone --mirror` cache, whose `remote.origin.mirror=true` makes Git reject any push that names one: `fatal: --mirror can't be combined with refspecs`. A completed developer execution therefore fails at delivery, and no execution branch reaches the code host. `existing_path` mode is unaffected (its source is an ordinary checkout).
  - Evidence: reproduced at the Go level with a throwaway test that ran `Prepare` → `Commit` → `Push` against a real repository in `managed_clone` mode — `push branch: git -C: exit status 128: fatal: --mirror can't be combined with refspecs` (git 2.43.0). Equivalent plain-git reproduction: `git clone --mirror <origin> cache.git && git --git-dir cache.git worktree add -B agent/42/abc wt main && (cd wt && git push --set-upstream origin agent/42/abc)`.
  - Suggested resolution: filed as [#147](https://github.com/alexandre-leites/moirai/issues/147). Left unfixed here deliberately: it is a distinct defect in `delivery.go`'s push semantics (the candidate fixes — `-c remote.origin.mirror=false` on each push, or cloning the cache as `--bare` with an explicit fetch refspec — change delivery or cache layout, not workspace preparation), and #136 is complete without it. Within a single runner the first acceptance criterion holds regardless, because preparation falls back to the local execution branch, which is exactly the path that this bug leaves as the only carrier.

- Issue: nothing prunes an execution branch, so a job's branch keeps every execution's commit.
  - Severity: P3
  - Impact: the branch now accumulates `developer`, `repairer` and `wip(failed)` commits for a job instead of a single rewritten commit. That is the point of the fix — the history is honest — but a reviewer or a pull request for a long-running job sees more commits than before.
  - Evidence: `retainWorkInProgress` (`runner/internal/dispatch/dispatch.go`) commits on the execution branch for every non-delivering run, and that commit is now inherited rather than discarded.
  - Suggested resolution: none needed for correctness. If squashing is wanted it belongs to whoever owns delivery or the orchestrator's pull-request step, not to workspace preparation.

## Next Recommended Implementation

Fix the `managed_clone` push failure recorded above ([#147](https://github.com/alexandre-leites/moirai/issues/147)) — no delivery reaches the code host in that mode today, which makes it strictly more urgent than anything left in this area. Relevant files: `runner/internal/repository/delivery.go` (`Push`, `PushWorkInProgress`, `CleanupRemoteBranch`) and, if the cache layout is chosen as the fix instead, `runner/internal/repository/manager.go`'s `prepareSource`. Expected behavior: a completed developer execution in `managed_clone` mode publishes its branch to the remote, and a failed one publishes `wip/<executionId>`. Targeted validation: a real-git test that prepares a `managed_clone` workspace, commits, pushes, and reads the branch back out of the origin repository — the existing push tests all run against an ordinary checkout, which is why the defect survived.
# Session: issue #144 — Require `ai-doable` on agent-opened issues; land the open pull requests (branch `issue-144`)

## Current Status

Done. `AGENTS.md` states the labelling rule, the three non-dependabot pull requests are merged, and every open issue carries `ai-doable`.

## Done

- `AGENTS.md` §1.3 (Shared state) gains one rule: whenever an agent opens a GitHub issue it must add the `ai-doable` label. Placed beside the existing "GitHub issues are the backlog" bullet, because that is where the document already explains what the backlog is and how an agent claims from it — an agent reading how to pick work reads the rule for filing it.
- Merged PR #139 (`issue-100`, retain and publish failed-run work) and PR #140 (`issue-98`, `ci_repair_attempts` as a real counter), both squashed, both 10/10 green at merge time.
- Merged PR #142 (`issue-94`, close execution requests so stalled-run recovery fires) after resolving its conflicts against `main`.
- Added `ai-doable` to the 17 open issues that lacked it: #110–#114, #116–#124, #136, #141, #143.

## Decisions

- Decision: `PROGRESS.md` conflicts are resolved by union, never by choosing a side.
  - Context: merging #139 put `issue-98` into conflict, and merging both put `issue-94` into conflict. In each case the only overlap was that two agents appended a session section at the end of the file.
  - Alternatives considered: take ours, take theirs, or hand-pick sections.
  - Reason: the file is append-only per agent and is the coordination point named in `AGENTS.md` §1.3. Dropping a section destroys another agent's handoff record, which is exactly what the file exists to prevent.
  - Consequences: resolved with `git merge-file --union`, then verified mechanically — no conflict markers, and zero lines lost from either side (`comm -23` against both index stages returned empty for each merge).

- Decision: the two integration-test suites that collided in `test_postgres_integration.py` were both kept in full, and the interaction between them was fixed rather than papered over.
  - Context: this branch appended `StalledRunRecoveryIntegrationTests` (5 tests) plus the `_PLANNER_READY` fixture and `_SingleIterationLeader` helper; `main` had appended `CircuitBreakerIntegrationTests` (10 tests) plus an `AsyncpgWorkflowPersistence` import, to the same end-of-file region. Git produced five interleaved conflict hunks because the two classes share unittest boilerplate.
  - Alternatives considered: (a) keep one side and re-add the other later; (b) scope the stall-recovery assertions to their own project so foreign rows are ignored; (c) give the circuit-breaker class the per-fixture teardown the module already expects.
  - Reason: (a) loses tested behaviour that CI had already proved on both PRs. (b) weakens the assertions of the very tests this branch exists to add — a stall sweep that only looks at its own project no longer tests the global sweep the orchestrator actually runs. (c) restores an invariant this module already documents: `StalledRunRecoveryIntegrationTests._delete_fixture` states that the control plane's sweeps "are global queries against a database shared with every other integration test, so the fixture cannot be left behind". The circuit-breaker class disabled its projects and cleared both circuit tables but never removed the projects, issues or workflow runs it seeded, including one deliberately left in `implementing`.
  - Consequences: the file was rebuilt deterministically from the three index stages rather than by editing the interleaved hunks — `main`'s header and shared body, then this branch's block, then `main`'s block — and the result was checked to contain the exact union of test names from both sides: 24 unique tests, none missing, none invented. `CircuitBreakerIntegrationTests` now records each seeded project and runner and deletes them on cleanup, locks and jobs first because their references to `workflow_runs` do not cascade. Both suites pass together.

- Decision: the merge is a merge commit into the branch, not a rebase.
  - Context: `issue-94` was already pushed and under review as PR #142.
  - Reason: rebasing a published branch rewrites commits other agents and the PR review may already reference.
  - Consequences: PR #142 carries an explicit merge commit recording how each conflict was resolved.

## Validation Status

- Targeted tests: Passed — the interference this merge exposed was reproduced and then fixed. Before the fix, `CircuitBreakerIntegrationTests` followed by `StalledRunRecoveryIntegrationTests` on a fresh database gave `Ran 15 tests … FAILED (failures=2)`; after, `Ran 15 tests … OK`. The stall-recovery class alone was `Ran 5 tests … OK` both before and after, which is what identified the cause as foreign rows rather than a bad resolution.
- Service tests: Passed — `make test-orchestrator` → `Ran 412 tests … OK (skipped=24)`; `make test-postgres-integration` → `Ran 24 tests … OK` against a throwaway PostgreSQL 16 container on a private port, repeated on three separate fresh databases.
- Full repository tests: Not run — the documentation change touches no code, and the merge resolution is Python and Markdown only.
- Build: Not applicable.
- Lint: Passed — `make lint`.
- Type checks: Passed — `make typecheck MYPY_CACHE=/tmp/moirai-mypy-cache-issue-144` (own cache directory, so it cannot collide with a sibling worktree).
- Database migrations: Not applicable — no schema change; migrations ran against the throwaway database via the integration suite.
- Docker Compose: Not run — no Compose or configuration change.
- End-to-end workflow: Not run.

## Known Issues

- Issue: adding `ai-doable` to #110–#124 reverses a deliberate curation.
  - Severity: P3 — process, not code.
  - Impact: those issues had `ai-doable` removed by the repository owner earlier the same day. Issue #144 asks for every open issue without the label to receive it, which is what was done, but the two intents contradict each other for exactly that range.
  - Evidence: all 17 issues read back as having no labels at all before the change; #136, #141 and #143 are ordinary unlabelled issues, whereas #110–#124 are the deliberately cleared ones.
  - Suggested resolution: for the owner to decide. Re-clearing them is a single `gh issue edit --remove-label ai-doable` per issue; this is called out in the comment on #144 so the decision is not silently buried.

- Issue: `make test-postgres-integration` is still not repeatable against the same database.
  - Severity: P3
  - Impact: a second run against an already-used database fails `OfferExpiryIntegrationTests.test_expired_recovery_offer_returns_the_run_to_recovering`.
  - Evidence: pre-existing and unrelated to this merge — reproduced on `main`'s own unmodified copy of the file, where run 1 was `Ran 19 tests … OK` and run 2 `Ran 19 tests … FAILED (failures=1)` on the same database. Already recorded under the issue #92 session, which attributes the leftover to `PostgreSQLPersistenceIntegrationTests`'s lifecycle test. CI creates a fresh Postgres service per job, so it is green there. The circuit-breaker teardown added in this merge fixes only the leak that affected the stall-recovery sweeps; it does not address this one.
  - Suggested resolution: unchanged from the earlier entry — have the lifecycle test clean up its job and workflow run, or truncate the `app` schema in `asyncSetUp`.

## Next Recommended Implementation

Issue #96 (finding F9) — make transition replay idempotent, still the highest-priority platform-review issue that no branch has claimed. It was already the recommendation of the issue #92 session and nothing in this session touched it. Relevant files: `orchestrator/src/moirai/workflows/nodes.py`, `orchestrator/src/moirai/persistence/control_plane.py` (`drain_pending_transitions`, `_dispatch`). Expected behavior: replaying one transition twice leaves the same counters and the same single execution request. Targeted validation: new cases in `orchestrator/tests/test_workflow_nodes.py` and `orchestrator/tests/test_asyncpg_control_plane.py`, plus a PostgreSQL integration test that drains the same outbox row twice.

---

# Session: issue #96 — transition replay idempotency (agent `issue-96`, 2026-07-29)

## Current Status

- Overall status: complete, pending review.
- Current phase: platform-review remediation (finding F9).
- Active implementation: none — issue #96 finished by agent session `issue-96` at 2026-07-29T08:20Z.
- Last updated: 2026-07-29
- Agent/session identifier: `issue-96`

## Done

- [x] Issue [#96](https://github.com/alexandre-leites/moirai/issues/96) — make transition replay idempotent (double-counted budgets, duplicate requests, stuck outbox rows).
  - Completed: 2026-07-29
  - Relevant files: `orchestrator/src/moirai/workflows/nodes.py`, `orchestrator/src/moirai/workflows/persistence.py`, `orchestrator/src/moirai/workflows/runtime.py`, `orchestrator/src/moirai/persistence/control_plane.py` (outbox drain only), `orchestrator/migrations/007_outbox_processing_lease.sql`, `orchestrator/README.md`, `README.md`, and the five test modules listed under Validation.
  - Behavior delivered:
    - **One open execution request per run, enforced where it is decided.** `AsyncpgWorkflowPersistence.dispatch` now looks for an open (`queued` *or* `dispatched`) request inside the transaction that already holds the workflow run's `FOR UPDATE` lock, and returns it with `created = False` instead of inserting a second row. The dispatching node reads `created` to decide whether an attempt was really spent. Closed rows (`completed`/`failed`/`cancelled`/`orphaned`/`expired`) are never open, so this composes with #94 rather than resurrecting finished work.
    - **The replay check moved ahead of the retry-budget gates.** The per-node counter checks (`planning_attempts`, `implementation_attempts`, `review_cycles`, `pipeline_repair_attempts`, `ci_repair_attempts`) and the `total_agent_executions` check now live inside `_dispatch`, after the replay lookup. This was a second, unlisted half of the same bug: the dispatch that spends the last unit of a budget writes the counter that makes the *same node* read "exhausted" on the way back in, so a replay blocked the run for retries that never happened. Each repair node still spends only its own counter (#98) and `pipeline` still spends none (#90).
    - **A node that finds another phase's request open suspends instead of dispatching.** It writes nothing: the committed phase belongs to the execution that is actually running.
    - **A replayed transition no longer advances the graph.** `PersistedWorkflowRuntime.run` re-asserts `awaiting_execution` when a caller *clearing* it (`awaiting_execution: False` — only a runner transition does that, which is also the only thing the outbox replays) hands in a run that still has an open request. This is what a duplicate delivery actually did before: it walked the graph one node *past* the execution it was waiting on, so the developer's own terminal event then resumed from the `pipeline` edge and routed out of the phase without the deterministic pipeline ever running — silently re-opening finding F3. Deliberately not stated more broadly than that: the gate is scoped to those callers so a human decision, which carries no such key, is never held back by a request the maintenance loop has not closed yet; and `plan`'s `plan_valid` short-circuit still clears the gate on its own, so a graph re-entered from `START` can walk one node before `_dispatch` stops it.
    - **A node reached while another phase's execution is open is logged, not swallowed.** That branch of `_reuse` writes nothing and suspends, which is the least destructive response, but it means an invariant the runtime is supposed to enforce was violated, so it emits an error with the run, the node's role and the open request's role.
    - **`processing` is a lease, not a state.** `drain_pending_transitions` stamps `processing_started_at` when it claims a row and reclaims any claim older than 90 seconds; `accept_event`'s inline delivery takes the same lease, so a maintenance tick landing between its commit and its delivery cannot deliver the same transition twice, and a drainer that loses the claim does nothing rather than duplicating work.
  - Validation performed: see Validation Status below. Failing-test-first evidence is recorded there.
  - Commands executed: `make dev-install`, `make test-orchestrator`, `make lint`, `make typecheck MYPY_CACHE=/tmp/moirai-mypy-cache-issue-96`, `LOOP_TEST_DATABASE_URL=… make test-postgres-integration`.
  - Adversarial review: run against the full diff before the PR was opened. It found no budget-accounting defect (it re-derived the counter/limit pairing for all seven dispatching nodes and confirmed each matches `origin/main` and `policy.py`'s routing gates), and confirmed both acceptance criteria are met — including by reverting each half of the fix in memory and watching the matching test fail. Six findings were acted on: the unfenced lease (P2-2, fixed), a silently-swallowed invariant violation (P2-4, now logged), the replay gate applying to the human-decision path (P2-5, scoped to callers clearing `awaiting_execution`), a replay test that passed with the runtime gate reverted (P2-6, now asserts on the log), the migration's index column order and a false claim in its comment (P3-2/P3-3, both fixed), and two wrong facts in this file (P3-1, corrected above). Two were answered with a recorded decision instead of a change: the unenforced one-open-request invariant (P2-1, see Decisions) and the arm-1-before-arm-3 ordering, which the review correctly identified as a consequence of two timeout constants rather than a guarantee — `orchestrator/README.md` and the `_OUTBOX_PROCESSING_LEASE` comment now say so. Two were left alone: deleting the dead `AsyncpgControlPlane.get_queued_execution_request` (P3-4) would edit `control_plane.py` outside this session's region for no functional gain, and arm 3's advancing branch losing its dedicated coverage is worth its own test but not a change to this fix.
  - Notes: `orchestrator/src/moirai/persistence/control_plane.py` was touched only in the outbox-drain region (`_drain_outbox_entry`, `drain_pending_transitions`, and the new `_OUTBOX_PROCESSING_LEASE` constant) to stay clear of the concurrent session on issue #111, which owns `set_runner_state` ~1100 lines away. `get_queued_execution_request` on `AsyncpgControlPlane` (line ~873) is dead code — nothing calls it — and was deliberately left alone for the same reason; the live copy is the one on `AsyncpgWorkflowPersistence`.

## Decisions

- Decision: the outbox uses a lease on `processing`, not a transaction that reverts to `pending` on failure.
  - Context: step 3 of issue #96 offered either. `drain_pending_transitions` already reverts to `pending` when the delivery *raises*; the transition that was lost forever is the one whose drainer died.
  - Alternatives considered: (a) hold the row in an open transaction for the whole delivery and let a rollback revert it; (b) never mark `processing` at all and rely on the delivery being idempotent; (c) a lease column with a reclaim window.
  - Reason: (a) cannot work — the delivery invokes the graph runtime, which reaches GitHub, so it would hold a database transaction open across an unbounded external call, and a killed process still commits nothing while blocking every other drainer on the row's lock. It also does not solve the actual failure: a process that dies does not roll back, it simply disappears, and the row's `processing` mark was committed before the delivery began. (b) leaves the row `pending` for its whole delivery, so every maintenance tick re-delivers a slow transition — safe now that replay is idempotent, but it converts a rare duplicate into a routine one. (c) is the only option that bounds recovery for a crashed drainer while still excluding a live one.
  - Consequences: a new nullable column, `app.workflow_transition_outbox.processing_started_at` (migration `007_outbox_processing_lease.sql`), and a 90-second constant, `_OUTBOX_PROCESSING_LEASE`. The lease must outlast a real delivery and stay inside the 2-minute stall window, so a stranded transition is always recovered by the drain arm rather than by the stalled-run arm — which recovers it *without* the state updates that rode on the row. A crashed drainer costs at most 90 seconds; a delivery slower than that is delivered twice, which is now harmless. The migration also hands back any row already sitting in `processing` with no claim time, since migrations run at startup before this process drains anything, so such a row is stranded by definition.

- Decision: the "don't advance past an open execution" rule lives in the runtime, not only in the nodes.
  - Context: the node-level guard alone satisfies the issue's acceptance criterion on counters, request rows and status — but the reproduction test showed the replay still dispatched an extra `pipeline` execution, because the graph had already advanced a node before any guard could run.
  - Alternatives considered: (a) node-level guard only, accepting that the graph position drifts by one node per duplicate delivery; (b) deduplicate deliveries so a transition is never replayed at all.
  - Reason: (b) is impossible — a crash after `on_transition` returns and before the row is marked processed is exactly the window the outbox exists to cover, so at-least-once is a property, not a defect. Under (a) the drifted position is not cosmetic: the run ends up suspended on the `pipeline` edge while the developer execution is still running, and that execution's terminal event then routes straight out of the phase, so the deterministic pipeline gate is skipped — the defect finding F3 exists to prevent. `test_a_replayed_transition_never_skips_the_pipeline_gate` pins it.
  - Consequences: one extra indexed lookup per graph invocation, and `WorkflowCheckpointStore` gains `get_open_execution_request`. It cannot wedge a run: the only ways a request stays open are an execution that is genuinely running and one the maintenance loop closes as `orphaned`. Terminal runs short-circuit before the check, so a leaked request cannot delay the checkpoint that records a finished run.

- Decision: the dedup key is "any open request for this run", not "an open request with this role".
  - Context: `repair` and `ci_repair` share the `repairer` role (#98), and `implement` and `push` share `developer`, so role alone is not a node identity.
  - Alternatives considered: persist a per-node dispatch key on the request row and guard the insert with `INSERT … ON CONFLICT`, as the issue's step 2 suggests.
  - Reason: a workflow run has at most one execution in flight — one job per run, one active run per project — so "this run has an open request" is the stronger and simpler invariant, and it needs no new column. `(workflow_run_id, role, attempt)` is already unique, but `attempt` is a per-role sequence and cannot be derived from a node's counter, so an `ON CONFLICT` guard would have needed a synthetic key for a case the run-level rule already covers.
  - Consequences: a role match additionally re-states the phase, so a first pass that queued the execution and then died before writing its transition still lands the status it was moving to; a role mismatch writes nothing and logs an error. One accepted gap, narrower than the bug being fixed: a crash *between* the request insert and the counter write leaves that attempt uncharged, because the replay correctly sees `created = False`. Under-counting is the safe direction — over-counting is what blocked runs for exhausted retries that never ran — and the execution itself still runs exactly once.

- Decision: "at most one open execution request per run" is enforced by `dispatch()`, not by a partial unique index.
  - Context: raised by the adversarial review as P2-1. The invariant is now load-bearing for budget accounting, dispatch dedup and the runtime's replay gate, and `dispatch()` is not its only writer — `_requeue_lost_execution` also inserts.
  - Alternatives considered: `CREATE UNIQUE INDEX ... ON app.workflow_execution_requests (workflow_run_id) WHERE status IN ('queued','dispatched')` in migration `007`.
  - Reason: the index would refuse to build on exactly the databases that most need this fix. The bug being fixed here *is* duplicate open requests, so any existing deployment that has hit it already holds rows the index cannot accept, and the migration — which runs at startup — would fail and stop the orchestrator from booting. Making it safe would mean a data-repair step deciding which of two open requests to close, on evidence that no longer exists. Beyond that, a unique violation at runtime surfaces as an `asyncpg.UniqueViolationError` out of a graph node, which `PersistedWorkflowRuntime._fail` turns into a `failed` workflow run: a hard stop for a condition the reuse path already handles softly.
  - Consequences: the invariant is a convention held by two writers rather than a constraint. `_requeue_lost_execution` only runs when the run's newest request is `orphaned`, so it cannot add a second open row; `dispatch()` checks under the run's row lock. If it is ever broken anyway, `_OPEN_EXECUTION_REQUEST_QUERY`'s `ORDER BY created_at DESC` picks the newest, and the role-mismatch branch of `_reuse` now logs an error rather than parking the run silently. Worth revisiting as a separate hardening issue with a repair step, once no deployment predates this fix.

- Decision: the outbox claim stamp is a fence, checked on completion and release.
  - Context: also from the adversarial review (P2-2). The first version matched `WHERE id = $1 AND status = 'processing'` on release and on `id` alone on completion, so a delivery that outlived its own 90-second lease could act on a row another drainer had since reclaimed.
  - Alternatives considered: leave it — duplicate delivery is now harmless — or hold the row in a transaction for the whole delivery.
  - Reason: harmless is not the same as correct, and the docstring and README both claimed the lease made the inline and background drains mutually exclusive. Without the fence the claimed property was false: a stale drainer releasing a row under a live claim hands the same transition to a third drainer. A transaction is not available — the delivery invokes the graph and can reach GitHub.
  - Consequences: `_claim_outbox_entry` returns the stamp it wrote, and `_complete_outbox_entry`/`_release_outbox_entry` both require `processing_started_at = $claim`. A drainer whose lease expired mid-delivery now silently does nothing, which is right: the row belongs to whoever holds it. `drain_pending_transitions` stamps every row in one pass with the same `now`, so one value fences the whole batch.

## Validation Status

- Targeted tests: Passed. Failing-test-first evidence, captured with only the new tests added and no source change, on the branch's then-base `09a6c1b` (`origin/main`'s tip when this branch was cut; the merge base is now `ffb4bc4`): `test_replaying_a_transition_is_identical_to_delivering_it_once` failed on both subtests with `AssertionError: Lists differ: ['planner', 'developer', 'pipeline'] != ['planner', 'developer']` (the replay queued a third execution), and `test_a_replayed_transition_never_skips_the_pipeline_gate` failed with `AssertionError: terminal developer event produced no workflow transition` (the run had been moved to `local_pipeline` behind the developer's back). `Ran 16 tests … FAILED (failures=3)`.
  Re-confirmed after the adversarial review, by neutralising `PersistedWorkflowRuntime._execution_in_flight` in memory and running only those two tests: `Ran 2 tests … FAILED (failures=3)` — three, because the identity test fails on both subtests. That second run is the one that matters: the review showed the identity test originally passed with the runtime gate reverted, because a drifted node writes no counter, no request row and no transition. It now asserts on the error `_reuse` logs when a node is reached while another phase's execution is open, which is the only observable trace such a node leaves.
- Service tests: Passed — `make test-orchestrator` → `Ran 431 tests … OK (skipped=27)`, against `Ran 412 tests … OK (skipped=24)` on `origin/main` (measured in a throwaway `git worktree` at `ffb4bc4`, since removed): **19 tests added**, across `test_end_to_end.py`, `test_workflow_nodes.py`, `test_workflow_persistence.py`, `test_workflow_runtime.py`, `test_asyncpg_control_plane.py` and `test_postgres_integration.py`. Two pre-existing node tests were renamed or replaced rather than added, so the count of new `def test_` lines in the diff (21) is larger than the net delta.
- Full repository tests: Not run — no Go, proto or web change; `make test` deliberately not invoked.
- Build: Not applicable — Python only.
- Lint: Passed — `make lint` → `All checks passed!`.
- Type checks: Passed — `make typecheck MYPY_CACHE=/tmp/moirai-mypy-cache-issue-96` → `Success: no issues found in 48 source files` (own cache directory, so it cannot collide with a sibling worktree).
- Database migrations: Passed — `007_outbox_processing_lease.sql` applied by `MigrationRunner` against a throwaway PostgreSQL 16 container bound to port 55496, removed afterwards. `test_migrations_are_recorded_and_idempotent` covers the version being recorded and the re-run being a no-op.
- Docker Compose: Not run — no Compose or configuration change.
- End-to-end workflow: Partially — `LOOP_TEST_DATABASE_URL=… make test-postgres-integration` → `Ran 27 tests in 4.161s … OK` against a fresh database. Four of those exercise the real drain: a redelivered outbox row (queued and dispatched variants), a row stranded in `processing`, and the maintenance loop's recovery of a run whose graph invocation was lost.

## Known Issues

- Issue: `007` is the next free migration version, and nothing prevents a concurrent session from claiming it too.
  - Severity: P3
  - Impact: two files with the same numeric prefix do not conflict in git, but `MigrationRunner._discover_migrations` raises `ValueError: duplicate migration version: 7` and the orchestrator fails to start. This has happened in this repository before.
  - Evidence: at the time of writing, `git ls-tree origin/<branch> orchestrator/migrations/` shows nothing above `006` on any of `issue-97`, `issue-111`, `issue-113`, `issue-136`, `issue-109` or `issue-144`, and no open PR touches `orchestrator/migrations/`.
  - Suggested resolution: whoever merges second renumbers to the next free version. `test_discovery_rejects_duplicate_versions` already fails loudly rather than silently skipping a migration.

- Issue: arm 3's advancing branch no longer has a dedicated test.
  - Severity: P3
  - Impact: `test_committed_transition_without_a_graph_invocation_is_recovered` used to be the only test of `recover_stalled_workflow_run`'s "the execution reported, only the graph invocation was lost" branch. With the lease in place, arm 1 reclaims the stranded outbox row first, so that test now exercises the drain instead. The branch is still reachable — the outbox row can genuinely be `processed` while the graph invocation was lost — but nothing pins it.
  - Evidence: raised by the adversarial review; confirmed by reading the rewritten test, which now asserts `_outbox_statuses(...) == ["processed"]` and a `developer` request rather than a second `planner` attempt.
  - Suggested resolution: a test that marks the outbox row `processed` before running the maintenance loop, so only arm 3 can repair the run. Left out here because it belongs to issue #94's recovery code rather than to this fix.

- Issue: `make test-postgres-integration` is still not repeatable against the same database.
  - Severity: P3
  - Impact: unchanged from the issue #92 session — the second run against one database fails `OfferExpiryIntegrationTests.test_expired_recovery_offer_returns_the_run_to_recovering` with two expired leases instead of one.
  - Evidence: re-confirmed pre-existing in this worktree. With the whole change stashed: run 1 `Ran 24 tests … OK`, run 2 `Ran 24 tests … FAILED (failures=1)`. With the change applied the behaviour is identical (run 1 `Ran 27 tests … OK`, run 2 same single failure), so it is not a regression. CI creates a fresh PostgreSQL service per job.
  - Suggested resolution: as previously recorded — have `PostgreSQLPersistenceIntegrationTests.test_control_plane_persists_project_runner_issue_and_lease_lifecycle` clean up its job and workflow run, or truncate the `app` schema in `asyncSetUp`.

## Next Recommended Implementation

Issue [#101](https://github.com/alexandre-leites/moirai/issues/101) (finding F14) — fix non-progress fingerprinting. `_record_progress_evidence` in `orchestrator/src/moirai/persistence/control_plane.py` is the closest remaining orchestrator-side correctness item to the work just finished: it decides when a workflow is blocked for repeating itself, using the same terminal-event path this session's outbox lease now delivers exactly once. Relevant files: `orchestrator/src/moirai/persistence/control_plane.py` (`_record_progress_evidence`, `_success_outcome_hash`, `_failure_outcome_hash`), `orchestrator/tests/test_asyncpg_control_plane.py` (`NonProgressEvidenceTests`). Expected behavior: an outcome identity that distinguishes real repetition from incidental payload variation, so a workflow making genuine progress is never blocked and a stuck one still is. Targeted validation: extend `NonProgressEvidenceTests`, which already replays whole terminal-event sequences against a stateful fake.
# Session: F10 / issue #97 — Forward blocked results, summary and remainingWork (branch `issue-97`)

## Current Status

- Overall status: Complete for finding F10.
- Current phase: Bug fix from the 2026-07-29 platform review (`docs/reviews/2026-07-29-platform-review.md`, F10, P2, marked *(verify)*).
- Active implementation: issue-97 agent session, 2026-07-29 — none remaining.
- Last updated: 2026-07-29.
- Agent/session identifier: issue-97.

## Done

- [x] An agent-reported block reaches the orchestrator with its reason and remaining work intact
  - Completed: 2026-07-29.
  - Relevant files: `runner/internal/dispatch/control_loop.go`, `runner/internal/dispatch/dispatch.go`, `runner/internal/control/events.go`, `orchestrator/src/moirai/workflows/runner_events.py`, their tests, `runner/internal/agents/opencode_test.go`, `runner/README.md`, `orchestrator/README.md`.
  - Behavior delivered:
    - **Runner — the block is no longer flattened.** A clean agent exit whose result document says `blocked` reports `status: "blocked"` and `blocked: true` in the terminal payload. The event *type* stays `failed`; see Decisions. A failing *process* never reports a block whatever its document claims, so a crash and a deliberate stop stay distinguishable (`TestControlLoopReportsAProcessFailureAsAFailureEvenWhenTheDocumentSaysBlocked`).
    - **Runner — the agent's account crosses the wire.** `terminalPayload` attaches `result` (the raw document), `summary`, and `remainingWork` for every outcome the agent itself reached, where previously `result` was attached only for `completed` and the other two never. A `cancelled` run reports none of them: it reached no outcome of its own, and see Decisions for the fingerprint reason.
    - **Runner — all three are bounded as JSON encodes them.** `boundedAgentText`/`boundedList` sanitise terminal escapes, control bytes and invalid UTF-8, then keep the longest prefix fitting a 2 KiB *encoded* budget (20 entries for the list), reusing `logtail.go`'s `jsonEncodedSize`/`sanitizeLogText`. `minimalTerminalPayload` keeps the three fields on the reduced-payload retry and re-bounds every field it keeps, including `error`, which no other path measures.
    - **Runner — a non-`completed` agent result skips the packet's pipeline commands**, so a pipeline verdict cannot overwrite the agent's status, reason and remaining work with a generic failure.
    - **Event redaction — two real holes closed.** `redactPayloadWithPrefixes` had a `[]any` arm but no `[]string` arm, and terminal payloads are built in Go, so every `[]string` value bypassed redaction entirely: `changedFiles` and `commandsRun` always did, and `remainingWork` would have. Redaction now also requires a token boundary before a prefix — `sk-` matched inside `task-runner.py`, `disk-usage.ts` and `make task-build`, which would have corrupted the path and command lists the new arm routes through it.
    - **Orchestrator.** `validate_runner_event` parses the result document for every terminal event and validates `blocked`/`summary`/`remainingWork`; `RunnerEventSummary` carries them. `_terminal_event_transition` routes a block ahead of the generic failure arm, to the terminal `blocked` status with a `blocking_reason` composed from the agent's summary and outstanding work (bounded to 1024 characters, the width the circuit writer stores), clearing the gate the reporting role owns. The `pipeline` role is resolved first, so the deterministic gate cannot be diverted.
  - Validation performed: failing-test-first on both sides, then the runner suite with the race detector, the full orchestrator suite, lint, type check, `gofmt`, `go vet`.
  - Commands executed:
    - Failing-test-first (runner), before any behaviour change: `cd runner && go test ./internal/dispatch/ -run 'TestTerminalPayload…|TestControlLoopReportsAnAgentReportedBlock…'` → `--- FAIL: TestControlLoopReportsAnAgentReportedBlockDistinctlyFromAFailure` with the payload printed as `{… "error":"the deployment credential is missing", "status":"failed"}` — no `result`, no `summary`, no `remainingWork`, no `blocked`: the flattening, reproduced. Also `--- FAIL: TestTerminalPayloadCarriesTheAgentAccountForEveryOutcome: terminalPayload(completed) summary = <nil>` and `--- FAIL: TestMinimalTerminalPayloadKeepsTheBlockedExplanation: reduced payload lost the block: map[…]{"status":"blocked"}`.
    - Failing-test-first (orchestrator): `PYTHONPATH=orchestrator/src .venv/bin/python3 -m unittest discover -s orchestrator/tests -p test_runner_events.py` → `Ran 45 tests … FAILED (failures=2, errors=28)`, including `test_result_document_is_parsed_for_every_terminal_event: AssertionError` (`summary.result is None` on a `failed` event) and `test_rejects_malformed_block_fields: RunnerEventError not raised`.
    - Failing-test-first (dispatcher pipeline skip): `go test -run TestDispatcherSkipsThePipelineWhenTheAgentReportedABlock` → `Execute() error = execute agent: pipeline command failed with exit code 1: go test ./..., want a blocked result rather than a failure`.
    - Encoded-size regression, proved twice by reverting only the measurement to raw bytes: `TestTerminalPayloadBoundsTheAgentAccount/angle_brackets` → `summary encodes to 6113 bytes`, `/ansi_escapes` → `3648 bytes`; and end to end, `TestReducedTerminalPayloadIsAcceptedByTheEventReporter` → `ERROR terminal execution event lost … error="execution event payload is too large"` followed by `timed out waiting for execution events`. Both pass with the encoded-size bound.
    - `make test-runner` (`go test -race ./...`) → all 10 packages `ok`.
    - `make test-orchestrator` → `Ran 425 tests … OK (skipped=24)`.
    - `make lint` → `All checks passed!`
    - `make typecheck MYPY_CACHE=/tmp/moirai-mypy-cache-issue-97` → `Success: no issues found in 48 source files`.
    - `cd runner && gofmt -l .` → no output; `go vet ./...` → clean.
  - Notes: no migration, no new configuration, no proto change. `runner/internal/agents/opencode.go` needed no change — #89 already returns the document's own status when the process exits cleanly — so it only gained a characterisation test pinning that contract at the source of the chain.

## Post-review corrections

An adversarial review of the first draft confirmed the routing and the delivery semantics but found two P1 defects, two P2 defects and several P3 issues. All were fixed before the PR was opened.

- **P1, the exact trap #93 left behind.** `summary`, `remainingWork` and `error` were bounded by *raw* byte length. Go's encoder spends six bytes on each `<`, `>`, `&` and control byte, so three raw-measured 2 KiB fields of angle brackets encode to ~36 KiB — over the 16 KiB cap the reduced-payload retry exists to get under. A blocked agent whose summary was angle-bracket-heavy, ANSI-coloured, or not valid UTF-8 therefore had its terminal event *rejected twice* and logged as `terminal execution event lost`: the run's outcome destroyed by the change meant to enrich it. Fixed by measuring the JSON-encoded size and sanitising first, reusing `logtail.go`'s existing `jsonEncodedSize` and `sanitizeLogText` rather than writing a second bound. `logtail.go:16-23` had already documented this exact rule.
- **P1, the test that claimed to guard it could not fail.** `TestTerminalPayloadBoundsTheAgentAccount` asserted the right thing — reduced payload under 16 KiB encoded — but filled with characters that encode 1:1 and omitted `error`, the largest contributor and the one field `terminalPayload` never builds. It passed against the defect above. Rewritten as a table over plain / angle-bracket / invalid-UTF-8 / ANSI fills, including `error` and `failureFingerprint`, plus `TestReducedTerminalPayloadIsAcceptedByTheEventReporter`, which drives the real reporter end to end and fails with `terminal execution event lost` against the raw-byte bound.
- **P2, the new `[]string` redaction arm corrupted ordinary data.** Routing `changedFiles`/`commandsRun` through `redactKnownSecretValues` exposed a pre-existing substring match: `sk-` matches inside `task-runner.py`, `docs/risk-register.md`, `src/disk-usage.ts` and `make task-build`, rewriting them to `ta[REDACTED].py` and so on, into `app.executions.result`, the success outcome hash, and the UI. Fixed at the root with a token-boundary check rather than by narrowing the arm — `commandsRun` can legitimately contain a bearer token, so it must be redacted. Both sides are pinned: `TestEventReporterLeavesOrdinaryPathsAndCommandsIntact` and `TestEventReporterStillRedactsSecretsAtTokenBoundaries`.
- **P2, attaching `summary` to a cancelled payload destabilised its fingerprint.** The orchestrator's `_failure_message` prefers `error`, then `summary`. A cancelled payload has no `error`, so it used to fall through to a stable `cancelled exit=N`; adding `summary` replaced that with free text that varies per run, so repeated cancellations would no longer match and the non-progress breaker would never trip. Fixed by reporting no agent account on `cancelled` at all — which is also the honest reading, since an interrupted agent reached no conclusion of its own.
- **P3, fixed.** `boundedList` emitted `["", ""]` for an all-blank list; it now drops blank entries and returns nothing when none remain. Five inaccurate README claims were corrected, including one that asserted the very property the P1 defect broke, and two payload-table rows that were wrong by omission. A tautological orchestrator test (`assertLessEqual` on a dict literal defined 20 lines above) was replaced with one that proves the widened payload is still accepted whole and that one more field would be rejected.

## Decisions

- Decision: an agent-reported block is a `failed` event refined by a `blocked` payload marker, not a new event type.
  - Context: step 1 of issue #97 asks to check `VALID_EVENT_TYPES` (`workflows/runner_events.py`) before adding a type.
  - Alternatives considered: (a) add `blocked` to the event vocabulary; (b) reuse `failed` and carry the distinction in the payload.
  - Reason: the vocabulary is not one list. It is replicated across `control.validEventType` and `eventPriority`/`IsTerminalEventType` in the runner, the `ExecutionEvent` proto and its generated Go and Python bindings, `VALID_EVENT_TYPES`/`TERMINAL_EVENT_TYPES` in the orchestrator, the `app.jobs.status` and `app.workflow_execution_requests.status` check constraints that `accept_event` writes the event type straight into, and the web UI. Adding a type means a migration and a coordinated change across four modules — three of which are owned by concurrent agents this session — to express something that is a *refinement* of an existing outcome: the run did not deliver. The issue itself notes the payload is the smaller protocol change.
  - Consequences: no migration, no proto change, no orchestrator persistence change, and an old orchestrator reading a new runner's event still classifies it correctly as a failure rather than rejecting it as an unknown type. The marker is honoured only on `failed`, since a `completed` or `cancelled` event claiming a block contradicts the outcome the runner reported and the outcome wins. Cost: `RunnerEventSummary.failed` stays true for a block, so any future consumer must check `blocked` first, as `_terminal_event_transition` now does.

- Decision: step 4 of the issue — "should a blocked developer result still push its branch?" — was already settled by #100 and was deliberately not re-decided.
  - Context: the issue text (written before #100 merged) reports `dispatch.go:230` as a bonus defect: a blocked result still commits and pushes the delivery branch because `executeErr` is nil.
  - Reason: verified against the current code, not the issue text. `dispatch.go` gates delivery on `executeErr == nil && result.Status == "completed"`, with the comment "Only a completed run delivers to the agent branch"; a blocked run instead goes through `retainWorkInProgress`, whose `workInProgressCommitMessage` maps the status to an explicit `wip(blocked): …`, anchors it at `refs/moirai-wip/<executionId>`, and publishes it to `wip/<executionId>` when the packet grants `mayPush`. `terminalPayload` already recorded `wipCommit`/`wipBranch`/`wipPushed`. The issue's requirement — "if kept, make it explicit rather than accidental, and record the branch in the payload" — is met in full.
  - Consequences: no change was made to the delivery path. `TestDispatcherRetainsWorkInProgressWithoutDeliveringWhenExecutionFails` already covered the blocked case; it was extended to assert the block also survives the dispatcher with its summary, so the property is pinned as a *blocked* property and not only as a *failed* one.

- Decision: a blocked payload is not attached to a `cancelled` event.
  - Context: found by adversarial review — see Post-review corrections.
  - Alternatives considered: attach the agent account uniformly to all four outcomes; attach it to everything except the summary; attach nothing on cancellation.
  - Reason: two arguments agree. A cancelled execution was interrupted, so whatever its half-written result document says is not a conclusion the agent reached. And `_failure_message` (`persistence/control_plane.py`) prefers `error` then `summary`, so a summary on a cancelled payload displaces the stable `cancelled exit=N` text that makes repeated cancellations fingerprint identically — silently disabling the non-progress breaker for a cancellation loop.
  - Consequences: cancelled payloads are byte-identical to before this change. The asymmetry is documented in the runner README's push-semantics table and in `terminalPayload`'s comment.

- Decision: an agent-reported block is terminal, not a retry.
  - Context: before this change, a developer or repairer whose document said `blocked` produced `recovering`, which stalled-run recovery re-dispatches. It now ends the run at `blocked`. Raised by the adversarial review as a behaviour change worth stating.
  - Alternatives considered: (a) route a block to `recovering` and keep retrying; (b) gate it on `implementation_attempts`/`pipeline_repair_attempts` the way the planner's block is gated; (c) terminal `blocked`.
  - Reason: (a) is the "same prompt, count to 3, block" pattern the platform review's executive summary calls out as the core autonomy failure — re-dispatching an identical packet against an agent that has explained why it cannot proceed cannot succeed, and burns budget doing it. (b) adds a counter for a signal that is not attempt-shaped: the agent is not failing, it is telling the operator something is missing. (c) matches the precedent the file already sets for the planner's `blocked` and the reviewer's `human_required`, both terminal, and produces the state a human can act on — `blocking_reason` populated, the `agent:blocked` label, the project circuit's failure reason.
  - Consequences: one agent writing `"status": "blocked"` ends the workflow. That is the intended trade — it is the difference between an informed escalation and a blind retry — but it does make the block a load-bearing word in the agent prompt. The escalation ladder that should eventually park such a run on a *question* rather than end it is issue [#107](https://github.com/alexandre-leites/moirai/issues/107) (Autonomy L4), which the review lists as depending on this issue.

- Decision: the secret-prefix set was not widened, only the matching rule was corrected.
  - Context: the review noted that forwarding `summary`, `remainingWork` and the result document on failed runs widens the surface for a credential an agent echoes into its own prose, and that the redactor only knows `ghp_`, `github_pat_`, `glpat-`, `sk-` plus operator-configured prefixes — not AWS keys, Slack tokens, JWTs, PEM blocks or `://user:pass@` URLs.
  - Alternatives considered: add `AKIA`, `xox` and `-----BEGIN ` here; leave the set alone.
  - Reason: the default prefix set is a platform-wide security policy that applies to every field of every event on every runner, and `-----BEGIN ` in particular does not work with the token-run algorithm at all (it would redact `BEGIN` and leave the key body). Changing it belongs in a change that can be reasoned about and tested as a security change, not smuggled into a feature. `LOOP_RUNNER_REDACTION_PREFIXES` already lets an operator add their own today. The marginal widening here is genuinely small: `error` and `logTail` — the agent's own stderr — already crossed the wire on failed runs under exactly this redactor.
  - Consequences: recorded in Quality Backlog below. What this session did change is strictly a correctness fix in both directions: `[]string` values are redacted at all now, and prefixes no longer match mid-identifier.

## Quality Backlog

- [ ] Widen the runner's default secret-prefix set and give it a non-prefix rule
  - Category: security hardening.
  - Risk: low to implement, moderate to get wrong — a rule that is too eager corrupts ordinary output, which is exactly the defect the token-boundary fix above had to repair.
  - Expected benefit: `AKIA…`, `xox[baprs]-…`, JWTs, `-----BEGIN … PRIVATE KEY-----` blocks and `scheme://user:pass@host` credentials are currently forwarded verbatim in `error`, `logTail`, and now `summary`/`remainingWork`/`result` whenever an agent quotes them.
  - Recommended timing: as its own change, with tests that pin both the redaction and the non-corruption of ordinary text. The PEM and URL cases need a delimiter-aware rule rather than a prefix plus token run.

## Validation Status

- Targeted tests: Passed. New: 9 Go tests in `internal/dispatch` (block routing, the agent account on every outcome, encoded-size bounding across four hostile fills, the end-to-end reduced-payload delivery, blank-entry handling, the rune-boundary and sanitisation properties), 3 in `internal/control` (list redaction, path/command non-corruption, boundary redaction), 1 in `internal/agents`, 1 in `internal/dispatch` for the pipeline skip; 14 orchestrator tests across `ValidateRunnerEventTests`, `WorkflowTransitionTests` and the new `AgentBlockEndToEndTests`. Every behavioural one was confirmed failing first — outputs quoted under Commands executed.
- Service tests: Passed — `make test-runner` (race detector, 10 packages `ok`); `make test-orchestrator` → `Ran 425 tests … OK (skipped=24)`.
- Full repository tests: Not run — `make test-api` and `make test-web` were deliberately not invoked; no `api/` or `web/` file is touched.
- Build: Covered by `go test` and `go vet ./...`.
- Lint: Passed — `make lint` → `All checks passed!`
- Type checks: Passed — `make typecheck MYPY_CACHE=/tmp/moirai-mypy-cache-issue-97` (own cache directory, so it cannot collide with a sibling worktree).
- Database migrations: Not applicable — no schema change, by design (see Decisions).
- Docker Compose: Not run — no Compose or configuration change.
- End-to-end workflow: Not run. `AgentBlockEndToEndTests` covers the wire shape the runner builds through `validate_runner_event` to `workflow_transition_for_terminal_event`, but the payload there is a literal, not one produced by the Go code.

## Known Issues

- Issue: the runner's terminal payload and the orchestrator's parser agree only by hand.
  - Severity: P3
  - Impact: `AgentBlockEndToEndTests.BLOCKED_PAYLOAD` transcribes what `dispatch.terminalPayload` builds. A future field rename on either side passes both suites and fails in production.
  - Evidence: `orchestrator/tests/test_runner_events.py` and `runner/internal/dispatch/control_loop.go` share no generated artefact; the `ExecutionEvent` proto types the payload as an opaque JSON string.
  - Suggested resolution: a golden-file fixture written by a Go test and read by the Python one, or a schema for the terminal payload alongside `schemas/agent-result.schema.json`.

- Issue: `EventReporter.Flush` is not a delivery barrier — root-caused this session, filed as [#152](https://github.com/alexandre-leites/moirai/issues/152).
  - Severity: P2 — it is the cause of the runner CI flake recorded by the issue #92 session, and it may be a real durability defect at shutdown.
  - Impact: `Flush()` returns `nil` while another goroutine is mid-`SendExecutionEvent`, having delivered nothing itself. `ControlLoop.FlushEvents()` propagates that, so every caller that treats it as "the queue is drained" — including `cmd/runner`'s shutdown path, the last chance to drain the outbox before exit — can be wrong.
  - Evidence: reproduced deterministically with a throwaway probe in `internal/dispatch` using a client whose first send blocks → `Flush() returned nil having delivered 0 events while a send was in flight`. In the flaky test the window opens because `waitForOutboxEvent` observes the terminal event on disk as soon as `Emit` calls `persistLocked()`, which is *before* `Emit` releases the mutex and enters `flush()`; the test then clears `sendErr` and calls `FlushEvents()` while the execute goroutine still holds `sending`. That matches the symptom the issue #92 session recorded exactly and answers the question it left open. This PR's CI hit it once (`TestControlLoopDeliversTerminalEventAfterLogsSaturateTheBufferWhileDisconnected`, `delivered events = [one event], want the terminal event last`) and passed on re-run with all 10 checks green; the diff touches neither that test nor its code path, and `GOMAXPROCS=2 go test -race -count=12 ./internal/dispatch/` is `ok` locally.
  - Suggested resolution: see #152 — make `Flush` wait on the in-flight send (a `sync.Cond` on `sending`, or a single-flight token a waiter can block on) and pin the barrier property with a test. Not attempted here: it is a concurrency change to the event reporter, issue #93's area, and #97 touches none of that path.

- Issue: the planner result document cannot satisfy both schemas it is validated against.
  - Severity: P2 — it makes the planner's `ready` verdict unreachable, independently of this issue.
  - Impact: `schemas/agent-result.schema.json` requires `status ∈ {completed, blocked, failed}` and the runner enforces it in `readResultDocument`; `schemas/planner-result.schema.json` requires `status ∈ {ready, human_required, blocked, invalid}` and `_schema_field` enforces that. One `status` field serves both, so `blocked` is the only value that satisfies them together. A planner writing `ready` has its document rejected by the runner as invalid; one writing `completed` fails `planner-result` validation in the orchestrator and is routed back to `planning` with `plan_valid = False`.
  - Evidence: `runner/internal/agents/opencode.go` `readResultDocument`'s status switch; `orchestrator/src/moirai/workflows/runner_events.py` `_schema_field(summary.result, "planner-result", "status")`; the two schema files.
  - Suggested resolution: separate the transport status from the role verdict — keep `status` for the agent-result envelope and give the planner its own `verdict` field, as `review-result` already does. Out of scope here: it changes the agent protocol and both schemas, and this issue's acceptance criteria are met without it (the block path is role-independent by construction, precisely so it does not depend on this).

## Next Recommended Implementation

Issue [#104](https://github.com/alexandre-leites/moirai/issues/104) (Autonomy L1) — the runner-side goal gate and session-resume continuation loop. It is the review's highest-priority runner task, it depends only on #89 (merged), and it is the natural continuation of this session: after the agent exits, check that the result document is valid, `status == "completed"`, `remainingWork` is empty, and — for mutating roles — that a non-empty diff exists; if the gate fails and a continuation budget remains, re-invoke the agent resuming the captured `sessionId` with a prompt naming the missing evidence. Relevant files: `runner/internal/dispatch/dispatch.go`, `runner/internal/agents/opencode.go` (`Result.SessionID` is already captured and still unused), `runner/internal/config`. Expected behaviour: an agent that stops early with outstanding `remainingWork` is continued rather than reported, and the continuation is bounded and counted separately from the workflow's retry budgets. Targeted validation: new cases in `runner/internal/dispatch/dispatch_test.go` covering gate pass, gate fail with budget, and budget exhaustion. This session makes the failing case legible — `remainingWork` now reaches the orchestrator — and #104 is what acts on it inside the run.
# Session: issue #113 — Web UI has no runners page although `GET /api/v1/runners` already exists (branch `issue-113`)

## Current Status

- Overall status: Complete for issue #113.
- Current phase: MVP web surface — `PROJECT.md:62,90` require a runner status view that did not exist.
- Active implementation: issue-113 agent session, 2026-07-29 — `/runners` page.
- Last updated: 2026-07-29.
- Agent/session identifier: issue-113.

## Done

- [x] `/runners` lists the runner fleet
  - Completed: 2026-07-29.
  - Relevant files: `web/src/api.ts`, `web/src/runner-status.ts` (new), `web/src/runners.tsx` (new), `web/src/main.tsx`, `web/src/styles.css`, `web/src/vite-env.d.ts` (new), `web/src/runner-status.test.ts` (new), `web/src/runners.test.tsx` (new), `web/src/runners-page.test.tsx` (new), `web/vitest.config.ts` (new), `web/package.json`, `web/package-lock.json`, `web/README.md`, `README.md`, `Makefile`, `.github/workflows/ci.yml`.
  - Behavior delivered:
    - **Integration only, no new server surface.** `ListRunners`, `GET /api/v1/runners` and the `Runner` schema in `api/openapi.yaml` already existed; nothing in `web/` called them. No API, orchestrator, proto or schema file was touched. (Issue #57 covered the same ground and was auto-closed by PR #81, whose merge produced a tree identical to its first parent, so none of it was on `main`. Written from scratch.)
    - `api.ts` gains a `Runner` type and `listRunners(signal?)`. Two boundary decisions: a body without a `runners` array raises instead of unwrapping to `[]`, because returning `[]` would render "no runner is registered" for what is really a broken response; and `labels` is normalized from the wire's `null` to `[]`, because the handler marshals the protobuf's repeated field directly and Go writes a nil slice as `null`, contradicting the OpenAPI schema.
    - `runner-status.ts` holds the fleet's pure logic — heartbeat age formatting, the staleness rule, the status-pill mapping, `countOnline`, error copy, and a `loadRunners` that resolves to an error result rather than rejecting.
    - `runners.tsx` splits into `RunnersView` (pure, every state directly renderable) and `RunnersPage` (fetching). One row per runner: name + short id, status pill, labels, draining flag, heartbeat age with the absolute time in `title`. Stale rows carry a warn stripe, a "Stale" badge beside the age, and a "Stale" status pill — three signals, none of them colour alone. Loading, empty and failure states are explicit; a failed *refresh* keeps the last known rows behind a warning instead of blanking them.
    - Polling per specification §4.5 interim mode: 10s while the tab is visible, paused on `document.hidden`, resumed on `visibilitychange`, one request at a time, cleaned up on unmount.
    - `/runners` is routed inside `ProtectedRoute` and linked from the nav and the dashboard link list.
  - Validation performed: 53 web unit tests, all three acceptance criteria mutation-tested, typecheck, lint, production build.
  - Commands executed:
    - `make test-web` → `tsc --noEmit` clean; `eslint .` → `✖ 10 problems (0 errors, 10 warnings)`, all ten pre-existing in `auth.tsx`/`main.tsx`/`tokens.tsx` and none in new files; `vitest run` → `Test Files 3 passed (3) / Tests 53 passed (53)`.
    - `make build-web` → `tsc && vite build` → `✓ 26 modules transformed … built in 48ms`.
    - Mutation testing (each mutation applied to the source, suite re-run, source restored) — every one is caught, so no acceptance criterion rests on a vacuous test:
      - `stale` forced to `false` → 10 failures. `heartbeat.stale` ignored by the status pill → 2. `countOnline` replaced by `runners.length` → 2. The heartbeat cell's `title` removed → 1. The draining cell blanked → 1.
      - The error block never rendered → 7 failures. A malformed body returning `[]` instead of raising → 1.
      - The in-flight guard removed → 2. `setNow(requestedAt)` changed to `setNow(Date.now())` → 1. `clearInterval` removed → 1. The `!document.hidden` pause removed → 1. The `cancelled` guard removed → 1. The `labels ?? []` normalization removed → 1.
      - Restored tree: `Tests 53 passed (53)`.
  - Notes: no migration, no Compose change. One new build-time variable, `VITE_RUNNER_HEARTBEAT_INTERVAL_MS`, documented in `web/README.md`.

- [x] Web unit tests now exist and run in CI
  - Completed: 2026-07-29.
  - Behavior delivered: Vitest is wired in (`web/vitest.config.ts`, `npm test`), `make test-web` runs `tsc --noEmit`, `eslint .` and `vitest run`, and the CI `build-web` job now runs `make test-web` before `make build-web`. Until this change CI ran neither eslint nor `tsc` for `web/` — only the production build. Two new dev dependencies: `vitest` and `jsdom` (169 packages, no package removed, no unrelated version bump, `npm audit --audit-level=high` clean; the two pre-existing moderate `react-router` advisories are unchanged).
  - Notes: this is a deliberately minimal slice of what `docs/design/web-console/tasks.md` C2 and issue #123 ask for. There is no MSW layer — components take an `ApiClient` prop, so a stub object suffices. Whoever takes #123 should widen this rather than start over.

## Decisions

- Decision: the page follows specification §5.5's information architecture and status vocabulary but keeps the existing pages' chrome, rather than porting the console design system.
  - Context: `docs/design/web-console/` does specify a runners view (§5.5, task D5). D5 is written against a widened `GET /api/v1/runners` (task A12: capacity, activeJobs, reservedOffers, version) and a new `POST /api/v1/runners/{id}/state` (task B1), and it assumes the C-phase foundation — design tokens (C3) and the component library (C4) — which is not ported. `web/` is still the old four-page SPA the console is meant to replace.
  - Alternatives considered: (a) port the C3 token sheet and C4 components for this one page; (b) build the §5.5 fleet cards with the fields that do exist and leave the capacity meter out; (c) invent a layout.
  - Reason: the issue is explicit that no API change is needed, and A12/B1 are other people's tasks; (a) is C3+C4, multi-day, and would leave one page in a visual language no other page speaks. (c) is what the design package exists to prevent. What §5.5 *can* be honoured on today's payload is its information architecture (name, status pill Online/Draining/Offline, heartbeat age, labels) and §5's global rules (relative timestamps with the absolute time in `title`, explicit loading/empty/error states, "never a silent blank"), and those are followed exactly. The card-vs-table shape follows the issue's own acceptance criterion ("one row per runner") and the three existing pages.
  - Consequences: capacity, version, backend and "Working on" are absent, and there are no drain/disable/revoke controls. When A12 and B1 land, D5 replaces this page rather than extending it; the pure logic in `runner-status.ts` survives that.

- Decision: a stale heartbeat overrides `runners.status` in the pill and in the online count.
  - Context: `runners.status` is a lagging column. `record_heartbeat` sets it to `online`, but only `expire_leases` (600s, and only for a runner holding a lease) or revocation sets it back to `offline`; `sessions.disconnect()` is in-memory and never writes. An *idle* runner that is killed keeps `status = 'online'` forever.
  - Alternatives considered: render the pill straight off `status` and let the "Stale" badge carry the contradiction.
  - Reason: an operator opening this page is asking "is anything actually connected". A green `Online` pill beside "7d ago" answers that wrongly, and the header count would have said `3/3 online` for a fleet with nothing alive. Warn rather than crit, because §5.1 assigns warn to a stale probe.
  - Consequences: `describeRunnerStatus` takes the heartbeat as a second argument and `countOnline` takes `now`. A runner whose row genuinely is `offline` still reads `Offline`; stale only outranks `online`.

- Decision: the staleness budget is a build-time constant with an escape hatch, not a hard-coded 30s.
  - Context: the rule is three missed heartbeats, and the interval is `LOOP_RUNNER_HEARTBEAT_INTERVAL` — per-runner env configuration that `GET /api/v1/runners` does not report.
  - Alternatives considered: hard-code the 10s default; wait for A12.
  - Reason: hard-coding means a fleet configured to 60s renders every runner permanently stale, which is a page that cries wolf until nobody reads it. `VITE_RUNNER_HEARTBEAT_INTERVAL_MS` costs eight lines and removes that failure mode.
  - Consequences: it is read at build time (Vite), so a Compose deployment overriding it must rebuild the image. Documented as such. It is deleted when A12 puts the interval in the payload.

- Decision: heartbeat ages are measured from when the request was sent, not when it returned.
  - Context: found by the adversarial review. `setNow(Date.now())` ran after `await`, so every age carried the full round-trip.
  - Reason: with a 30s budget, roughly 6s of latency was enough to flip a runner that beat 25s ago into "Stale" — and it fires precisely when the orchestrator is struggling, which is when the page is being read. Sampling before the request is both correct and free.
  - Consequences: ages can lag by up to one poll interval, which is honest: they are as fresh as the answer they came from.

## Post-review corrections

An adversarial review of the first commit found six P2s and a list of P3s. All were fixed before the PR.

- **Ages inflated by request latency** (above). Reproduced by the reviewer under fake timers: a runner 2s old at request time rendered `37s ago` + stale when the response took 35s. Now covered by `measures heartbeat age from when the orchestrator was asked, not when it answered`, which fails against the old code.
- **Polls could stack and resolve out of order.** With nothing resolving, four concurrent requests had accumulated after 30s, and resolving the newest then the oldest left the *oldest* snapshot on screen. Fixed with a one-request-at-a-time gate and a per-request `AbortController`; `never stacks requests when the orchestrator is slow to answer` and `does not let an abandoned request overwrite a newer one` pin both halves. The related wedge — `loading` stuck `true` disabling Refresh forever behind a hung request — is fixed by never disabling the control; a click restarts the load from scratch.
- **The pill contradicted the badge** (above).
- **The 10s heartbeat interval was hard-coded** (above).
- **The abort test was vacuous.** `expect(container.textContent).toBe("")` after `root.unmount()` is trivially true, and React 18 removed the setState-after-unmount warning, so deleting the guard left all tests green. Replaced with the out-of-order test on a live component.
- **`labels` is typed non-nullable but arrives as `null`.** Go marshals a nil `[]string` as `null`, so an unlabelled runner would have thrown on `labels.length` — and because `main.tsx` wraps the whole app in the ErrorBoundary, that replaces the *entire console*, nav included, with "Something went wrong". The view's `?? []` was load-bearing while the type said the branch was unreachable and no test covered it. Now normalized in the API client and covered by `normalizes the null the Go handler emits for a runner with no labels`.
- **P3s fixed:** an unknown future `status` value now renders `idle` rather than painting the fleet critical; an unreadable timestamp says "unknown" instead of being conflated with "never reported"; the malformed-body rejection is a plain `Error` rather than an `ApiError` claiming status 200 (which the server never sent); the clock-skew comment named the wrong two clocks (the timestamp is stamped by the orchestrator, not the runner); the stale row tint uses `color-mix` on `--warning` instead of a literal rgba; the count is in an `aria-live="polite"` region; the `vitest.config.ts` comment claimed no test needs a DOM, which the same commit contradicted.
- **P3s accepted, not fixed:** `eslint .` exits 0 on warnings, so `react-hooks/exhaustive-deps` cannot fail CI; `--max-warnings 0` needs the ten pre-existing warnings resolved first and belongs with #123. `npm ci` now runs twice in the `build-web` job (once per make target), costing a few seconds. The job is still named `build-web` though it now gates tests; renaming it would break `validate`'s `needs` list and any branch protection.

## Validation Status

- Targeted tests: Passed — `make test-web` → `Tests 53 passed (53)` across `runner-status.test.ts` (26), `runners.test.tsx` (14) and `runners-page.test.tsx` (13). Thirteen mutations of the source were each confirmed to fail the suite (see Commands executed).
- Service tests: Not applicable beyond `web/` — no Python or Go file changed. `make test-orchestrator`, `make test-runner` and `make test-api` were deliberately not invoked.
- Full repository tests: Not run.
- Build: Passed — `make build-web`.
- Lint: Passed — `eslint .`, 0 errors. Ten warnings, all pre-existing and none in the new files.
- Type checks: Passed — `tsc --noEmit`.
- Database migrations: Not applicable.
- Docker Compose: Not run — no Compose change. The new `VITE_RUNNER_HEARTBEAT_INTERVAL_MS` is optional and defaults to the runner's own default, so the existing image build is unaffected.
- End-to-end workflow: Not run. The page was not exercised against a live orchestrator in a browser; the fleet, empty, stale, failed-load and failed-refresh states were verified by mounting the real component under jsdom with a stubbed `ApiClient`, not by clicking through the Compose stack.

## Known Issues

- Issue: the runners page cannot know a runner's real heartbeat interval.
  - Severity: P3
  - Impact: the staleness rule assumes the runner default (10s) unless the bundle is built with `VITE_RUNNER_HEARTBEAT_INTERVAL_MS`. A fleet running a longer interval without that build flag reports every runner stale.
  - Evidence: `LOOP_RUNNER_HEARTBEAT_INTERVAL` is runner-side env configuration (`runner/README.md`); `GET /api/v1/runners` does not carry it.
  - Suggested resolution: `docs/design/web-console/tasks.md` A12 already plans to widen the payload — add the interval there, then delete the variable.

- Issue: `runners.status` cannot distinguish a disconnected idle runner from a live one.
  - Severity: P3 — worked around in the UI, not fixed at the source.
  - Impact: the column stays `online` indefinitely for a killed runner that held no lease, so every consumer of it other than this page (including the scheduler's `JOIN app.runners AS r ON r.status = 'online'`) treats it as connected.
  - Evidence: `record_heartbeat` writes `status = 'online'`; only `expire_leases` and revoke write it back; `sessions.disconnect()` is in-memory only.
  - Suggested resolution: belongs to whoever owns `orchestrator/` — either flip the column on stream disconnect, or have the scheduler's candidate query gate on `last_seen_at` freshness rather than the column.

## Next Recommended Implementation

Issue #123 (web test infrastructure) is now much cheaper: Vitest and jsdom are installed, `make test-web` runs them, and CI gates on them. What remains of `docs/design/web-console/tasks.md` C2 is Testing Library, MSW, and raising `eslint` to `--max-warnings 0` after clearing the ten existing warnings. Doing it before more D-phase views land means each view arrives with the "renders / empty / error / primary action" bar the specification asks for rather than retrofitting it.
---

# Session: issue #111 — Orchestrator aborts the runner stream when a runner reports draining (branch `issue-111`)

## Current Status

- Overall status: Complete for issue #111.
- Current phase: P1 bug fix found while auditing the MVP end-to-end flow for #87.
- Active implementation: issue-111 agent session, 2026-07-29 — runner-reported drain state.
- Last updated: 2026-07-29.
- Agent/session identifier: issue-111.

## Done

- [x] A runner-reported drain no longer aborts the control stream, and now stops scheduling
  - Completed: 2026-07-29.
  - Relevant files: `orchestrator/src/moirai/grpc/runner_control.py`, `orchestrator/src/moirai/persistence/control_plane.py`, `orchestrator/src/moirai/domain/control_plane.py`, `orchestrator/tests/test_runner_grpc.py`, `orchestrator/tests/test_postgres_integration.py`, `docs/architecture.md`.
  - Behavior delivered:
    - `RunnerControlService._handle_message` has a `runner_draining` branch. Before it, `RunnerToOrchestrator.runner_draining` fell through to the catch-all `_StreamFailure(INVALID_ARGUMENT, "runner stream message is invalid")`, which `Connect` turns into `context.abort` — so a runner announcing it was draining ended its own bidirectional stream as a protocol violation, and nothing was recorded.
    - The branch persists the reported flag and returns, leaving the stream open. That matters beyond tidiness: a draining runner still has to renew leases and report execution events for work it already holds, and aborting the stream took that channel away mid-execution.
    - `AsyncpgControlPlane.set_runner_draining(runner_id, draining)` writes `app.runners.draining` and nothing else. All three placement queries already gate on `r.draining = false` (`schedule()` at `control_plane.py:1118`, `schedule_execution()` at `:1247`, `recover_one()` at `:1647`), so the next scheduling pass simply stops considering the runner. `draining=false` clears it.
    - It is narrower than `set_runner_state` *in the columns it touches* — that method also writes `enabled`, `revoked_at` and `status`, so routing a runner's report through it would let a runner re-enable a runner an operator had deliberately disabled, in both directions. It is **not** narrower in `draining` itself; see Known Issues.
    - A revoked runner (`revoked_at IS NOT NULL`) matches no row, and a report that matches no row raises `ValueError("runner is unknown or revoked")`, which the handler maps to `FAILED_PRECONDITION` rather than letting an unhandled exception take down the receive task. The stream ends, which is what that runner's next message would do anyway — `_load_runner_credential` refuses revoked rows.
    - The handler logs `runner reported its drain state` with the runner id and the reported value. Nothing else records the transition and it silently stops all placement on that runner, so without it an operator has no trace of why a runner went quiet.
    - `InMemoryControlPlane.set_runner_draining` mirrors it so the domain reference implementation stays in parity; `Runner.available` already excludes `draining`, so the in-memory scheduler drops the runner for the same reason the SQL one does.
  - Validation performed: two gRPC tests against a real `grpc.aio` server and three PostgreSQL integration tests, all five confirmed failing without the change; full orchestrator suite; full PostgreSQL integration suite; lint; type check.
  - Commands executed (all after merging `origin/main` at `ffb4bc4`):
    - `make test-orchestrator` → `Ran 417 tests in 1.303s / OK (skipped=27)`.
    - `LOOP_TEST_DATABASE_URL=postgresql://loop:loop-test-password@127.0.0.1:55411/loop_test make test-postgres-integration` → `Ran 27 tests in 3.975s / OK`, against a fresh throwaway `postgres:16-alpine` container on port 55411, removed afterwards.
    - `make lint` → `All checks passed!`.
    - `make typecheck MYPY_CACHE=/tmp/moirai-mypy-cache-issue-111` → `Success: no issues found in 48 source files`.
    - Regression proof, gRPC: with `orchestrator/src/moirai/grpc/runner_control.py` restored from `origin/main` and the new tests kept, `PYTHONPATH=orchestrator/src .venv/bin/python3 -m unittest discover -s orchestrator/tests -p test_runner_grpc.py` → `Ran 7 tests / FAILED (failures=2)`, both new tests failing on the aborted stream.
    - Regression proof, SQL: with `AsyncpgControlPlane.set_runner_draining` monkeypatched to a no-op, `RunnerDrainIntegrationTests` → `Ran 3 tests / FAILED (failures=3)`; the placement test fails with `schedule()` handing back the drained runner. None of the three assert anything the production method does not have to do.
  - Notes: `proto/runner_control.proto` already defined `RunnerDraining` (`:21`, `:40`) and the generated code already carried it, so no contract changed. Orchestrator-initiated drain (`OrchestratorToRunner.drain`) and the operator drain/revoke API stayed out of scope — issue #119.

## Decisions

- Decision: a new `set_runner_draining` rather than reusing `set_runner_state("drain")`.
  - Context: the issue offered either. `set_runner_state` maps `"drain"` to `(enabled=True, draining=True)` and `"enable"` to `(enabled=True, draining=False)`, and also writes `revoked_at` and `status`.
  - Alternatives considered: (a) call `set_runner_state(runner_id, "drain" if draining else "enable", None, now)`; (b) add a `draining`-only branch inside `set_runner_state`.
  - Reason: (a) is wrong on the clear path — a runner reporting `draining=false` would re-`enable` a runner an operator had deliberately disabled, and on the drain path it would re-enable a disabled runner too. The runner is authoritative about its own drain state and about nothing else. (b) widens a method #119 is about to build the operator API on, for a caller that shares none of its audit or revocation behavior.
  - Consequences: two methods write the same column with different scopes, which is the point. `set_runner_state` keeps its operator semantics untouched for #119; the runner path cannot reach `enabled`, `revoked_at` or `status`.

- Decision: an unknown or revoked runner fails the stream rather than being ignored.
  - Context: `set_runner_draining` raises `ValueError` when the `UPDATE` matches no row.
  - Alternatives considered: swallow it, since the runner authenticated moments earlier and the case is near-unreachable.
  - Reason: near-unreachable is not unreachable — a concurrent revoke between `authenticate_runner` and the write produces exactly this. Silently discarding it would leave the runner believing the orchestrator agreed to stop sending it work.
  - Consequences: the handler maps `ValueError` to `_StreamFailure(FAILED_PRECONDITION, "runner drain report was rejected")`, matching how `offer_accepted`, `offer_rejected` and `lease_renewal` already report rejected control messages. The stream ends, which is correct for a runner that no longer exists.

- Decision: work already leased to a draining runner is left alone.
  - Context: the acceptance criteria only require the flag and the open stream, and the obvious extra step would be to release or recover the runner's in-flight leases.
  - Alternatives considered: cancel or requeue the runner's `offered`/`preparing`/`running` jobs on the drain report.
  - Reason: draining means "no *new* work". The runner's own `Drain()` finishes what it holds and then exits (`WaitForIdle`), so cancelling on the orchestrator side would destroy work that is about to succeed, and would do it on a message the runner sends routinely. If the runner dies instead, `expire_leases` already recovers the job on the lease clock. See Known Issues for the one hazard this leaves.
  - Consequences: no change to in-flight behavior; drain is purely a placement signal.

## Post-review corrections

An adversarial review of the first draft was run before committing. It found ten items; the substantive ones and what was done about each:

- **`draining` has two writers and one bit** (major). The first draft's documentation claimed "only the runner clears it", which is false: `set_runner_state("enable")` also writes `draining`, so a runner reporting `draining=false` clears an operator drain and an operator `enable` clears a runner's. The claim was wrong, not the code — the issue's own acceptance criteria specify that a runner's `draining=false` clears the flag, and giving the two owners separate bits needs a second column plus a change to the three placement predicates, which is #119's decision and outside this session's region of `persistence/control_plane.py`. The false claim is removed from `docs/architecture.md` and from the docstring, both of which now state the conflict explicitly, and it is recorded under Known Issues. Nothing depends on it today: `set_runner_state` still has no callers.
- **Permanent-wedge risk** (major). Recorded under Known Issues with the full evidence chain and escalated on the issue rather than fixed here; the sound fix is in `runner/`, which this session does not own. `docs/architecture.md` now carries it as an explicit warning to whoever changes the runner's shutdown ordering.
- **The persistence test was vacuous** (major). Every assertion in the first draft's integration test would also have passed against `set_runner_state("drain")`/`("enable")`, so it did not test the method that was written. Replaced: `test_a_drain_report_writes_only_the_draining_column` disables the runner first and asserts `enabled` stays `false` across a report in *both* directions, which is exactly what `set_runner_state` would break.
- **The load-bearing claim had no test** (minor). "The scheduler stops offering work" was asserted only against the in-memory control plane; the predicate that matters in production is `r.draining = false` in the SQL. `test_a_drained_runner_is_not_selected_and_a_cleared_one_is` now drives real `schedule()` calls either side of a drain report, and fails when the write is removed.
- **The docstring described behavior the code did not have** (minor). It said a revoked runner "is left alone"; the code raises, which ends the stream. The docstring and the doc now say what happens, and the error message reads `runner is unknown or revoked` instead of `runner is unknown`.
- **`OrchestratorToRunner.drain` described as "not wired"** (minor). One-sided: the runner end is wired (`control_loop.go` calls `Drain()` on receipt) and is in fact the only path that delivers a drain report over a live stream today. Corrected.
- **No trace of the transition** (nit). Added the `runner reported its drain state` log line.
- Two items were accepted as-is: the in-memory control plane has no runner revocation to refuse (a comment now says so), and the integration test leaves a consumed registration-token row behind, which every test in that file does.

## Validation Status

- Targeted tests: Passed — `test_connect_drain_report_keeps_the_stream_open_and_stops_scheduling` and `test_connect_drain_report_of_false_clears_the_flag` (`orchestrator/tests/test_runner_grpc.py`), both confirmed failing with the handler reverted; the three cases in the new `RunnerDrainIntegrationTests` (`orchestrator/tests/test_postgres_integration.py`), all three confirmed failing with the persistence method neutered.
- Service tests: Passed — `make test-orchestrator` → `Ran 417 tests … OK (skipped=27)`; `make test-postgres-integration` → `Ran 27 tests … OK` against a fresh throwaway database.
- Full repository tests: Not run — no Go, proto, or web change. `make test` was deliberately not invoked.
- Build: Not applicable — Python only.
- Lint: Passed — `make lint`.
- Type checks: Passed — `make typecheck MYPY_CACHE=/tmp/moirai-mypy-cache-issue-111` (own cache directory, so it cannot collide with a sibling worktree).
- Database migrations: Not applicable — `app.runners.draining` already exists (migration `001_initial.sql`). Migrations were run against the throwaway database by the integration suite.
- Docker Compose: Not run — no Compose or configuration change.
- End-to-end workflow: Not run.

## Known Issues

- Issue: `app.runners.draining` has two writers and one bit, so a runner's report and an operator's drain overwrite each other.
  - Severity: P2 — no impact today, a correctness problem the moment #119 ships.
  - Impact: a runner reporting `draining=false` clears a drain an operator set with `set_runner_state("drain")`, and `set_runner_state("enable")` clears a drain a runner reported. Whichever wrote last wins, with no record that the other owner disagreed.
  - Evidence: `set_runner_state` maps `"drain"` to `(enabled=True, draining=True)` and `"enable"` to `(enabled=True, draining=False)`; `set_runner_draining` writes the same column. Harmless today only because `set_runner_state` has no callers — the operator API it exists for is #119.
  - Suggested resolution: belongs to #119, which owns the operator side and has to pick the model. A separate `runner_reported_draining` column with the three placement predicates gating on `draining OR runner_reported_draining` separates the owners cleanly, and would also make "clear the runner's bit when a fresh control stream connects" sound, which is what closes the wedge below. Not attempted here: it needs a migration and edits to `schedule()`, `schedule_execution()` and `recover_one()`, none of which are in this session's region of the file. This session's change was implemented exactly as issue #111 specifies (`draining=false` clears the flag) rather than pre-empting that design.

- Issue: nothing clears a runner-reported drain automatically, so a runner that reports draining and later restarts comes back with `draining = true`.
  - Severity: P2 — latent today, live as soon as the runner's shutdown ordering is fixed or #119 lands.
  - Impact: the scheduler would never offer that runner work again until an operator cleared the flag, and the operator API that would clear it (#119) does not exist yet.
  - Evidence: `ControlLoop.Drain()` (`runner/internal/dispatch/control_loop.go:260`) only ever calls `SetDraining(true)`; nothing in `runner/` sends `draining: false`. The wedge does not fire today only by accident: `StreamSupervisor.Run` calls `s.Client.Disconnect()` on `ctx.Done()` (`runner/internal/control/stream.go:103-106`) *before* `main` reaches `loop.Drain()` (`runner/cmd/runner/main.go:300-308`), so `Client.send` hits `c.stream == nil` and returns `ErrNotConnected` (`runner/internal/control/client.go:196-205`) — the SIGTERM path the issue describes never actually delivers the message. The path that does deliver it is the orchestrator-initiated drain at `control_loop.go:208`, which is unwired pending #119.
  - Suggested resolution: belongs in `runner/`, which is outside this session's ownership and was being worked on concurrently (#97, #136) — have the runner report `draining: false` once a fresh control stream is established, so its own state is what the orchestrator mirrors. Fixing it orchestrator-side is not sound while `draining` is one shared bit: clearing on reconnect would also erase an operator drain, and the stream reconnects every few seconds during a network blip. Tracked as [#148](https://github.com/alexandre-leites/moirai/issues/148) so it is not buried here.
- Issue: a scheduling pass that selected a runner just before its drain report still delivers that offer.
  - Severity: P3
  - Impact: the offer is placed on a runner that is going away. No work is lost — the runner rejects it (`control_loop.go:198`, `"runner is draining"`) and `reject_offer` requeues the run.
  - Evidence: the drain report and the candidate query are separate transactions; there is no lock spanning them.
  - Suggested resolution: none needed. This is the ordinary offer-rejection path, and the unanswered-offer bounds already cover the case where the runner dies without answering.

---

# Session: issue #147 — Every git push from a managed_clone workspace fails: `--mirror can't be combined with refspecs` (branch `issue-147`)

## Current Status

- Overall status: Complete for issue #147.
- Current phase: P0 bug fix. In the default repository mode no runner could deliver anything.
- Active implementation: issue-147 agent session, 2026-07-29 — mirror-safe pushes from a managed_clone workspace.
- Last updated: 2026-07-29.
- Agent/session identifier: issue-147.

## Done

- [x] Every push from a `managed_clone` workspace now names its refspec without tripping Git's mirror rule
  - Completed: 2026-07-29.
  - `runner/internal/repository/delivery.go`: new `pushCommand` helper builds every push as `git -C <workspace> -c remote.origin.mirror=false push …`. Applied to all three refspec-bearing pushes — `Push` (`--set-upstream origin <branch>`), `PushWorkInProgress` (`--force origin HEAD:refs/heads/<branch>`) and `CleanupRemoteBranch` (`origin --delete <branch>`).
  - `runner/internal/repository/delivery_mirror_test.go` (new): four real-git tests that prepare an actual `managed_clone` workspace through `Manager.Prepare` and read the result back out of the origin repository.

- [x] Reproduced the failure before fixing it
  - Plain git 2.43.0, exactly as the issue describes:

    ```
    git init -b main origin && (cd origin && echo base > f && git add -A && git commit -m base)
    git clone --mirror origin cache.git
    git --git-dir cache.git worktree add -B agent/42/abc wt main
    git -C wt push --set-upstream origin agent/42/abc
    # fatal: --mirror can't be combined with refspecs
    ```

    `git --git-dir cache.git config --get remote.origin.mirror` → `true`; `remote.origin.fetch` → `+refs/*:refs/*`. The same `fatal:` is produced by `git -C wt push --force origin HEAD:refs/heads/wip/x` and by `git -C wt push origin --delete agent/42/abc`.
  - The same three failures reproduce through the Go API: with the fix reverted, the three new push tests fail with `push branch: git -C: exit status 128: fatal: --mirror can't be combined with refspecs`, `push work-in-progress branch: … same`, and the delete path never being reached.

- [x] Answered the issue's open question: which other refspec-bearing commands the mirror config breaks
  - None. `remote.<name>.mirror` is a push-side setting only. Verified with real git from inside a mirror worktree and from the mirror cache itself:
    - `git fetch --prune origin main` — succeeds from both the cache and the worktree.
    - `git ls-remote --heads origin refs/heads/<branch>` — succeeds (this is the guard `CleanupRemoteBranch` runs before deleting).
    - `git update-ref`, `git worktree add|prune|remove` — never contact a remote, so the setting cannot reach them.
  - `TestManagedCloneMirrorConfigurationDoesNotBreakFetchOrLsRemote` pins that conclusion so a later change does not "fix" commands that were never broken.

## Decisions

- Decision: neutralise the mirror setting per push invocation (`git -c remote.origin.mirror=false push`) rather than stop cloning the cache as a mirror.
  - Context: the issue offered both. Candidate (2) was `git clone --bare` plus an explicit `remote.origin.fetch`.
  - Alternatives considered: (a) `git clone --bare` in `Manager.prepareSource`; (b) `git -C <cache> config remote.origin.mirror false` once, after the clone; (c) `git push --no-mirror`.
  - Reason: (a) and (b) both live in `runner/internal/repository/manager.go`, which open PR #146 (issue #136) owns and is actively editing — and (a) additionally changes the on-disk layout of every cache already cloned as a mirror, so it needs a migration path for caches that exist in the field. (c) does not work at all: `git push --no-mirror origin HEAD:refs/heads/probe` from a mirror worktree still fails with `fatal: --mirror can't be combined with refspecs`, because the configured value is applied regardless. Only a config override clears it. Confirmed empirically with git 2.43.0.
  - Consequences: the cache keeps `remote.origin.mirror=true` and stays a faithful mirror for fetching; no cache on disk has to be migrated; `existing_path` workspaces, which never carry the setting, are unaffected either way (`git -c remote.origin.mirror=false` on a repository that has no such key simply sets a key nothing reads). The cost is that the override has to be remembered at each push site, which is why all three go through one helper with the reason written next to it.

- Decision: keep the override on the command line rather than writing it into the workspace's config.
  - Context: `git config remote.origin.mirror false` in the prepared worktree would also have worked, and would not need repeating.
  - Alternatives considered: write it in `Manager.Prepare`, or in `excludeLoopArtifacts` alongside the other per-worktree setup.
  - Reason: both are in `manager.go` (not this session's to edit), and a worktree shares its config with the cache — so writing it there silently converts the shared project cache away from a mirror as a side effect of preparing one job. A per-invocation `-c` cannot leak into any other command.
  - Consequences: three call sites carry it, enforced by one helper. A fourth push added later without the helper would reintroduce the bug; the comment on `pushCommand` says so.

- Decision: disabling the mirror flag is a correctness fix, not only an availability one.
  - Context: a mirror push publishes *every* local ref and deletes remote refs the local repository lacks.
  - Reason: `RecordWorkInProgress` writes private anchors under `refs/moirai-wip/<executionId>` in the shared cache. Under mirror semantics a refspec-less push would have published those to the code host and pruned remote branches the cache had not fetched. Verified: `git -C wt push origin` from a mirror worktree succeeds and mirror-pushes everything.
  - Consequences: the new tests assert the *exact* set of references present in origin after each push, not merely that the expected branch arrived — an assertion that only looked for the expected branch would pass against a mirror push.

## Post-review corrections

The new tests were mutation-tested against four deliberate defects injected into a throwaway copy of the tree, before committing. Two of them initially survived, and both were fixed:

- **A regression to a refspec-less mirror push was not caught** (major). Replacing `push --set-upstream origin <branch>` with a bare `push origin` *succeeds* under `remote.origin.mirror=true` — it mirror-pushes everything — so the delivery branch still arrived in origin and `TestPushFromManagedCloneWorkspacePublishesTheDeliveryBranch` passed. That is the exact "reintroduce the bug and call it fixed" shape. The test now writes a `refs/moirai-wip/execution-earlier` anchor before pushing, as a prior failed execution would, and asserts the *exact* set of references origin holds. The mutation now fails with `origin references = [… refs/moirai-wip/execution-earlier], want exactly [refs/heads/agent/issue-147/run-1 refs/heads/main]`.
- **Dropping `--force` from `PushWorkInProgress` was not caught** (minor). The redelivery step added a commit on top of the one already published, so the second push was a fast-forward and succeeded without the flag — the test's own comment claimed it proved the opposite. The retry is now built on the base revision (`reset --hard HEAD~1`), making it a genuine non-fast-forward, and the test asserts that shape rather than assuming it. The mutation now fails on the rejected push.

Two further mutations were already caught and needed no change: removing `-c remote.origin.mirror=false` (the original bug — fails with the issue's exact `fatal:`), and making `Push` report `Pushed: true` without running git at all (caught by reading origin rather than trusting the return value).

Two independent adversarial reviews of the committed diff were then run. Both confirmed the fix itself is complete and correct — the three pushes in `delivery.go` are the only `git push` invocations in the repository's production code (the orchestrator moves refs through `gh api repos/.../git/refs/...`, never `git push`), `-c` and the `GIT_CONFIG_*` credential injection provably coexist (one review captured the `AUTHORIZATION: basic …` header on the wire against a local HTTP listener), `pushCommand`'s `append` cannot alias across calls, and the error-message prefix is byte-identical. What they found, and what was done:

- **`TestManagedCloneMirrorConfigurationDoesNotBreakFetchOrLsRemote` asserted only exit status 0** (minor, raised by both). A fetch that updates nothing also exits 0, so the test could not fail for the reason its comment gave. It now advances origin, asserts the cache's `refs/heads/main` really moves to the new tip, and asserts a planted `refs/moirai-wip/*` anchor survives the `--prune`. That last assertion guards a real hazard: a mirror's refspec is `+refs/*:refs/*`, and `git fetch --prune origin '+refs/*:refs/*'` does delete the anchor (`- [deleted] (none) -> refs/moirai-wip/exec-1`), while the `git fetch --prune origin <branch>` the runner actually issues preserves it. `ls-remote` is now asserted to report the branch, not merely to succeed.
- **`isAncestor` swallowed errors, and `false` was the passing direction** (minor). `git merge-base --is-ancestor` exits 1 for "not an ancestor" but 128 for a bad revision or unusable directory, so any git-level error silently *satisfied* the non-fast-forward guard protecting the `--force` assertion. It now returns false only on exit 1 and is fatal otherwise.
- **`readGitConfiguration` conflated "key unset" with "git failed"** (minor). Its caller turns `""` into "the cache is no longer a mirror", so an unreadable repository could masquerade as that verdict. Same treatment: exit 1 is an answer, anything else is fatal.
- **Two comments claimed more than the code did** (minor). The `CleanupRemoteBranch` test said a failed delete "leaves an abandoned branch for every execution"; in fact `CleanupRemoteBranch` has no production caller at all — it is absent from `dispatch.DeliveryManager`. And `pushCommand`'s comment read as "we write nothing to the cache config", when `--set-upstream` does write `branch.<name>.remote`/`.merge` there. Both corrected rather than the code changed.
- **One review argued the `remote.origin.mirror == "true"` precondition should be *established* rather than asserted**, so that a future `--bare` clone in `manager.go` does not turn four tests red. Kept as an assertion deliberately: if the cache stops being a mirror, these tests genuinely stop covering the bug they were written for, and a loud failure saying so is the correct signal — silently forcing the setting would leave a suite that no longer tests what the runner does. The misleading-diagnosis half of that objection was real and is fixed above.
- **A dead assertion was noted** (`!push.Pushed` cannot fail, since `Push` returns `Pushed: true` unconditionally on its success path). Kept: the issue's acceptance criterion is worded as "reports `pushed: true`", so the assertion is traceable to the criterion and costs nothing.

Both reviews also surfaced adjacent, pre-existing defects that this change does not cause; see Known Issues.

## Validation Status

- Targeted tests: Passed — the four tests in `runner/internal/repository/delivery_mirror_test.go`. The three push tests were each confirmed failing with the `-c remote.origin.mirror=false` removed from `pushCommand`, with the exact `fatal: --mirror can't be combined with refspecs` from the issue. See Post-review corrections above for the full mutation matrix.
- Service tests: Passed — `make test-runner` (`cd runner && go test -race ./...`), all packages `ok`.
- Full repository tests: Not run — the change is confined to `runner/internal/repository`. No proto, orchestrator, API, or web change.
- Build: Covered by `go test -race ./...`.
- Lint: Passed — `gofmt -l .` in `runner/` reports nothing.
- Type checks: Passed — `go vet ./...` in `runner/`.
- Database migrations: Not applicable.
- Docker Compose: Not run — no Compose or configuration change.
- End-to-end workflow: Not run. The acceptance criteria are covered at the layer the bug lives in: the tests drive `Manager.Prepare` in `managed_clone` mode and then the same `Commit` → `Push` and `Commit` → `RecordWorkInProgress` → `PushWorkInProgress` sequences that `Dispatcher.deliver` and `Dispatcher.retainWorkInProgress` issue, with a resolved `GITHUB_TOKEN` in the environment (#109), and read the published branch out of the origin repository.

## Known Issues

- Issue: the mirror setting still lives in the cache, so any future push added outside `pushCommand` reintroduces this bug.
  - Severity: P3 — a latent trap, not a live defect.
  - Impact: a new refspec-bearing push written as `manager.git(ctx, "-C", workspace.Repository, "push", …)` would fail in `managed_clone` mode and pass every test that uses an ordinary checkout, exactly as this bug did.
  - Evidence: `Manager.prepareSource` (`runner/internal/repository/manager.go`) still clones with `--mirror`; nothing prevents a push from bypassing the helper.
  - Suggested resolution: the root-cause fix is candidate (2) from the issue — clone the cache with `git clone --bare` and an explicit `remote.origin.fetch = +refs/heads/*:refs/heads/*`. It belongs in `manager.go`, which PR #146 owns, and needs a migration path for caches already on disk (detect `remote.origin.mirror=true` and rewrite the remote, or re-clone). Worth doing once #146 has landed; the per-invocation override above is correct in the meantime and does not conflict with it.

- Issue: the second delivery to a workflow's execution branch is rejected non-fast-forward, so only the first completed execution of a workflow ever reaches the code host.
  - Severity: P1 — pre-existing and not caused by this change, but it is the next wall in the default repository mode now that pushes work at all.
  - Impact: every repair-loop iteration and every retry after the first delivery fails in `deliver`. Affects `existing_path` identically; it was simply unreachable in `managed_clone` while every push failed earlier.
  - Evidence: `Manager.Prepare` force-resets the branch to the base revision (`git worktree add -B <branch> <path> <base>`, `manager.go:103`), while the orchestrator reuses one `branch_name` per workflow run (`orchestrator/src/moirai/workflows/persistence.py:250`). Reproduced in plain git: execution 1 pushes fine, execution 2 gets `! [rejected] agent/x -> agent/x (non-fast-forward)`.
  - Suggested resolution: tracked as [#156](https://github.com/alexandre-leites/moirai/issues/156). Needs a decision rather than a patch — `--force-with-lease` on the delivery push, or preparing from the published branch (which is what #136 / PR #146 is about, and would make repair loops cumulative). Not attempted here: the fix lives in `manager.go` or the orchestrator, neither of which this session owns.

- Issue: `CleanupRemoteBranch` runs its `ls-remote` and its delete push without the resolved credential environment.
  - Severity: P3 — unreachable today.
  - Impact: against a real GitHub remote both would fail on authentication rather than on anything else. It is the one of the three pushes that never reaches the authenticated path added by #109.
  - Evidence: `delivery.go` uses `manager.gitOutput`/`manager.git` there, not `gitWithEnv`, and the method takes no `environment` argument. It also has no production caller — it is absent from `dispatch.DeliveryManager` — so nothing exercises it yet.
  - Suggested resolution: give it the same `environment map[string]string` parameter the other two take, at the same time as whatever starts calling it. Deliberately not widened here: changing its signature for no caller is churn, and this session's change is a bug fix.

- Issue: `runner/README.md:118` describes a completed delivery as leaving `origin/<branch>` with upstream set, which is not true in `managed_clone` mode.
  - Severity: P3 — documentation only.
  - Impact: a mirror has no `refs/remotes/origin/*` at all, so there is no `origin/<branch>`; `--set-upstream` records `branch.<name>.merge = refs/heads/<name>` in the shared cache config instead. Misleading now that the push actually runs.
  - Evidence: verified against a real mirror worktree — `git rev-parse --symbolic-full-name @{upstream}` returns `refs/heads/agent/…`, not `refs/remotes/origin/…`.
  - Suggested resolution: correct that row. Not done here because `runner/README.md` is owned by open PR #146.

- Issue: `runner/README.md` describes the workspace lifecycle but not the mirror constraint on pushes.
  - Severity: P3 — documentation only.
  - Impact: the next person to add a git command against `origin` has to read `pushCommand` to learn the rule.
  - Evidence: `runner/README.md:55` mentions `git clone --mirror` only as the thing a credential authenticates.
  - Suggested resolution: one sentence in `runner/README.md`. Not written here because that file is owned by open PR #146 and editing it would have conflicted; the reasoning is instead carried in full on `pushCommand` in `delivery.go`.

## Next Recommended Implementation

- Continue with the highest-priority open `ai-doable` issue. If #146 has merged, the follow-up above (clone the cache `--bare` with an explicit fetch refspec, plus a migration for existing mirror caches) is a small, well-understood cleanup that removes this class of surprise at its source.

# Session: issue #148 — A runner never clears its drain flag, so it is stranded after a restart (branch `issue-148`)

## Current Status

- Overall status: Complete for issue #148.
- Current phase: P2 bug fix; unblocks any change to the runner's shutdown ordering (#102).
- Active implementation: issue-148 agent session, 2026-07-29 — report the runner's drain state on connect.
- Last updated: 2026-07-29.
- Agent/session identifier: issue-148.

## Done

- [x] The runner reports its actual drain state on every control stream it establishes
  - Completed: 2026-07-29.
  - Relevant files: `runner/internal/dispatch/control_loop.go`, `runner/internal/dispatch/control_loop_drain_test.go` (new), `runner/internal/dispatch/control_loop_test.go`, `runner/cmd/runner/main.go`, `runner/cmd/runner/main_stream_test.go` (new), `docs/architecture.md`.
  - Behavior delivered:
    - `ControlLoop.Resume()` reports `RunnerDraining{draining: Draining()}` and then flushes buffered events. It is wired as `StreamSupervisor.OnConnected`, replacing the bare `loop.FlushEvents`, so it runs on every stream the runner establishes — including the first one after a restart, where `Draining()` is `false` and the report clears an `app.runners.draining = true` left behind by the previous incarnation of the same runner identity. No orchestrator change was needed: the #111 handler already persists whichever boolean arrives.
    - It reports `Draining()`, never a bare `false`. A runner that reconnects while genuinely draining — a transport blip mid-drain, or an orchestrator-initiated drain whose report never left the dying stream — re-asserts the drain instead of advertising itself as available.
    - Reading the state and sending it are one critical section (`ControlLoop.drainReports`), so a `Drain()` landing during a reconnect cannot be overtaken by the reconnect's already-sampled `false`. Deliberately a second mutex rather than `loop.mu`: a report blocks for as long as a gRPC send does, and `loop.mu` also guards the active-execution map that `WaitForIdle` and every terminal execution touch. Nothing acquires `drainReports` while holding `loop.mu`, so the two cannot deadlock, and the ordering argument does not rely on `draining` being monotonic — it survives a future un-drain, because the state write lands under `loop.mu` before the writer queues on `drainReports` and every reporter re-reads inside that lock.
    - The report precedes the event flush, so the orchestrator stops placing work before the runner spends the fresh stream on a backlog. A failed report fails the resume rather than running a whole connection on a stale view; `StreamSupervisor` then drops the stream and retries when the failure is transient (`ErrNotConnected` and `io.EOF` both classify as `codes.Unknown`, which `isTransientTransportError` treats as transient), and stops the runner when it is not — the same treatment the flush already had.
    - `Drain()` now reports through the same path. Its failure is still only a warning: the drain holds locally regardless, so no offer is accepted, and the next `Resume()` re-asserts it. That recovery is new — before this change a report lost on a dying stream was lost for good.
    - `SetDraining(bool) error` moved into the `ControlClient` interface and the optional `drainingClient` type assertion was deleted. A client that could not report its drain state used to be a silent no-op, which is exactly the stranding this issue is about; it is now a compile error.
    - `run()`'s `StreamSupervisor` literal was extracted into `controlStreamSupervisor(...)` in `runner/cmd/runner/main.go` so the wiring itself is testable. Without that, reverting `OnConnected` to `loop.FlushEvents` — a one-token full reintroduction of this bug — passed the entire runner suite.
  - Validation performed: five new dispatch tests plus one `package main` wiring test, all confirmed failing against nine separate mutations; `make test-runner`; `gofmt`; `go vet`.
  - Commands executed:
    - `make test-runner` → `cd runner && go test -race ./...`, all 10 packages `ok` (`internal/metrics` has no test files).
    - `cd runner && gofmt -l .` → no output.
    - `cd runner && go vet ./...` → no output.
    - `cd runner && go test ./internal/dispatch/ ./cmd/runner/ -race -count=3` → `ok`, `ok`.
  - Mutation testing (each applied alone, then reverted; every one is killed):
    - Sample the drain state before taking the report lock → `TestControlLoopResumeCannotReportAStaleDrainState` fails.
    - Remove the serialization entirely → same test fails.
    - `Resume` sends a literal `false` → 4 tests fail across both packages.
    - `Resume` does not report at all (pre-fix behaviour) → 5 tests fail across both packages.
    - `Resume` flushes events before reporting → the ordering test fails.
    - `Resume` swallows a failed drain report → the delivery-failure test fails.
    - `main.go` wired back to `OnConnected: loop.FlushEvents` → the `package main` wiring test fails.
    - `Resume` swallows the flush error → the ordering test fails.
    - `Drain` drops its already-draining short circuit → the state test fails.
  - Notes: `proto/runner_control.proto` already defined `RunnerDraining` in both directions and the orchestrator handler landed in #111, so no contract and no orchestrator code changed.

## Decisions

- **Report `Draining()`, not `false`.** The issue's suggested approach allows either. Sending a bare `false` on connect is the one way to get the second acceptance criterion wrong, and it would have made an orchestrator-initiated drain (`OrchestratorToRunner.drain`, already handled by `control_loop.go`) evaporate on the next transport blip.
- **A second mutex, not `loop.mu`.** See above: correctness needs the read and the send to be atomic with respect to other reporters, but `loop.mu` is on the hot path of `WaitForIdle`, `finish`, and offer handling, and a gRPC send can block on flow control.
- **`SetDraining` promoted into `ControlClient`.** The type assertion it replaced degraded silently to "never report", which is the failure mode of this very issue. One test fake needed the method; nothing else implements the interface.
- **`controlStreamSupervisor` extracted from `run()`.** Adversarial review showed the fix's only production wiring was a line no test could defend. The extraction is the smallest change that makes `OnConnected` observable.
- **Did not pre-empt #119's column split.** See Known Issues: the two-writers problem is real and this change interacts with it, but resolving it needs a migration and edits to three placement predicates in `orchestrator/src/moirai/persistence/control_plane.py`, which is both out of scope and owned elsewhere.

## Post-review corrections

An adversarial review of the diff found four issues that were fixed before commit:

- **The production wiring was untested** (high). Reverting `OnConnected: loop.Resume` to `loop.FlushEvents` — a complete reintroduction of the bug in the shipped binary — passed `go test -race ./...` in `runner/`. The supervisor test built its own `control.StreamSupervisor` literal, so it proved `Resume` works *as* an `OnConnected`, not that anything used it that way. Fixed by extracting `controlStreamSupervisor` and adding `runner/cmd/runner/main_stream_test.go`.
- **The concurrency test could degrade into a vacuous pass** (medium). Its first version used a 100 ms sleep to let a goroutine queue on the report lock, and nothing asserted the interleaving happened. With a 300 ms scheduling delay injected in front of that goroutine — a loaded CI box — the "state sampled outside the lock" mutant survived. Replaced with a `runtime.Stack` barrier that waits for a goroutine to actually park inside `reportDrainState`; re-verified that the mutant now dies 3/3 *with* the 300 ms delay applied, and that the unmutated code still passes with it. The `t.Errorf`-from-a-goroutine hazard on the failure path went away with it: the queued resume now returns its error over a channel.
- **A behaviour regression had no test** (medium-low). `Resume` swallowing the flush error passed everything, even though the hook it replaced (`loop.FlushEvents`) failed the connect on exactly that. The ordering test now drives a resume whose drain report succeeds and whose flush fails, and asserts the error propagates.
- **Two claims were overstated** (low). "`StreamSupervisor` drops the stream and retries" is only the transient branch — a non-transient status unwinds `Run` and stops the runner. "The last value the orchestrator receives is always the runner's final one" describes ordering of *delivered* reports; a `Drain()` whose report dies with the stream leaves the orchestrator briefly wrong until the next connect. Both the code comments and `docs/architecture.md` now say so.

Two review findings were accepted as-is: the `reportDrainState` nil-client guard is unreachable in production (its comment now says so rather than overselling it), and `StreamSupervisor` resets its backoff on every successful `Connect`, so a hook that always fails pins retries at `ReconnectMin` — pre-existing, identical in shape to the heartbeat path, and unreachable here since nothing makes the drain report fail while the heartbeat succeeds.

## Validation Status

- Targeted tests: Passed — `TestControlLoopResumeReportsTheDrainStateTheRunnerActuallyHas`, `TestControlLoopResumeCannotReportAStaleDrainState`, `TestControlLoopResumeReportsDrainStateBeforeFlushingBufferedEvents`, `TestControlLoopResumeFailsWhenTheDrainReportCannotBeDelivered`, `TestStreamSupervisorReportsTheRunnersDrainStateOnConnect` (`runner/internal/dispatch/control_loop_drain_test.go`) and `TestControlStreamSupervisorReportsTheRunnersDrainStateOnConnect` (`runner/cmd/runner/main_stream_test.go`). All confirmed failing against the mutations listed above.
- Service tests: Passed — `make test-runner` (race detector on), all packages `ok`.
- Full repository tests: Not run — no orchestrator, API, web, or proto change. `make test` was deliberately not invoked.
- Build: Passed — `cd runner && go build ./...`.
- Lint: Passed — `cd runner && gofmt -l .` produced no output.
- Type checks: Passed — `cd runner && go vet ./...`.
- Database migrations: Not applicable — no schema change; `app.runners.draining` already exists.
- Docker Compose: Not run — no Compose or configuration change.
- End-to-end workflow: Not run. The orchestrator half is covered by #111's existing tests, including `orchestrator/tests/test_runner_grpc.py::test_connect_drain_report_of_false_clears_the_flag`, which already proves a `draining: false` report clears the column and restores placement.

## Known Issues

- Issue: `app.runners.draining` still has two writers and one bit, and this change makes `draining: false` a *frequent* write where it was previously never sent.
  - Severity: P2 — no impact today, a constraint on #119 the moment it ships.
  - Impact: before this change nothing in `runner/` ever sent `false`, so an operator drain written straight to the column would have survived. Now a runner that is not draining writes `false` on every connect, and the control stream reconnects on any transport blip — so an operator drain applied by writing the column alone would be cleared within seconds. An operator drain that also sends `OrchestratorToRunner.drain` stays consistent: the runner mirrors it into its own state via `ControlLoop.Drain()` and re-asserts it on every subsequent stream.
  - Evidence: `set_runner_state` maps `"drain"` to `(enabled=True, draining=True)` and `"enable"` to `(enabled=True, draining=False)`; `set_runner_draining` writes the same column. Harmless today only because `set_runner_state` has no callers.
  - Suggested resolution: unchanged from #111 — belongs to #119, which owns the operator side. A separate `runner_reported_draining` column with the three placement predicates gating on `draining OR runner_reported_draining` separates the owners cleanly. Until then, #119 must drive an operator drain through `OrchestratorToRunner.drain` (already handled by the runner) rather than through the column alone. Recorded on #119 as a comment and in `docs/architecture.md`. Deliberately not resolved here: it needs a migration and edits to `schedule()`, `schedule_execution()` and `recover_one()`, none of which are in this session's scope.

- Issue: the SIGTERM path still never delivers its drain report.
  - Severity: P3 — cosmetic now that the report is self-correcting, but it means a graceful shutdown tells the orchestrator nothing.
  - Impact: the orchestrator learns the runner is gone from the heartbeat timeout rather than from the drain report, so it may place one more offer that nobody answers. The unanswered-offer bounds already cover that.
  - Evidence: `StreamSupervisor.Run` calls `s.Client.Disconnect()` on `ctx.Done()` (`runner/internal/control/stream.go:103-106`) before `main` reaches `loop.Drain()` (`runner/cmd/runner/main.go:288`), so `Client.send` hits `c.stream == nil` and returns `ErrNotConnected`.
  - Suggested resolution: it belongs to #102's shutdown hardening, and this issue was the prerequisite — with the drain state now reported on connect, fixing that ordering no longer strands a runner after its first graceful shutdown.

## Next Recommended Implementation

- #102 (runner lifecycle hardening) is now unblocked on its shutdown-ordering item: delivering the SIGTERM drain report can no longer strand a runner, because the next connect clears it.
- #119 (operator drain/revoke API) should decide the `draining` column ownership before it ships. See Known Issues above for the constraint this change adds.

---

# Session: issue #141 — `accept_offer` clobbered the workflow phase, so successful developer executions produced no transition (branch `issue-141`)

## Current Status

- Overall status: complete, pending review.
- Current phase: core-loop correctness.
- Active implementation: none — issue #141 delivered (session `issue-141`, 2026-07-29).
- Last updated: 2026-07-29.
- Agent/session identifier: `issue-141`.

## Done

- [x] `app.workflow_runs.current_phase` belongs to the graph alone, so a terminal developer event can transition.
  - Completed: 2026-07-29.
  - Relevant files: `orchestrator/src/moirai/persistence/control_plane.py`, `orchestrator/tests/test_asyncpg_control_plane.py`, `orchestrator/tests/test_postgres_integration.py`, `orchestrator/README.md`, `docs/design/web-console/specification.md`.
  - Behavior delivered:
    - `accept_offer` writes only `app.workflow_runs.status = 'preparing'`. It no longer touches `current_phase`. Acceptance is a fact about the *job* (`app.jobs.status` already records it); the phase is the graph's, and it is the only durable record of which node a suspended run is waiting on.
    - `accept_event` decides the terminal-event transition from `w.current_phase`, not `w.status`. `implement` and `push` both dispatch the `developer` role, so the phase is the one thing that separates their terminal events; the status is `preparing` for the whole life of an execution and can never read `implementing` or `pushing` when the event lands.
    - `expire_leases` and `recover_one` likewise write only `status` (`recovering`, then `offered`). This is not cosmetic: a recovery re-offer carries the **same** `dispatched` execution request, so the phase that queued it has to survive for the terminal event it eventually produces to mean anything. Without this the fix would have held for the first attempt and the identical loop would have reappeared one lease expiry deeper.
    - The invariant is now: `status` is the scheduling lifecycle (`offered` / `preparing` / `recovering` / the committed phase / terminal); `current_phase` is written only by `AsyncpgWorkflowPersistence.transition` and by `accept_event`'s own transition. `_cancel_offered_job` and `_block_unanswered_run` still write both, because `cancelled` and `blocked` are genuine terminal workflow outcomes rather than job lifecycle events.
    - Consequence, deliberate and documented: `_release_unanswered_offer`'s `bootstrap` predicate reads `current_phase = 'offered'`, which now means exactly "no graph node has ever committed a phase for this run" instead of "`accept_offer` has never run". A run whose very first offer was accepted by a runner that then died without reporting anything is therefore cancelled and its issue returned to the global queue rather than re-offered until the unanswered-offer limit blocks it. The predicate's other guards (no branch, no pull request, `total_agent_executions = 0`, no `app.executions` row, no execution request) are unchanged, so a run with any work at all still cannot match it.
    - No migration: both columns already exist (`001_initial.sql`) and no schema change was needed. `preparing` and `recovering` remain valid `status` values, so `find_stalled_workflow_runs`' allow-list, `services/issue_sync.py`'s active-status list, `recover_one`'s `w.status = 'recovering'` predicate and the console's status pills are all untouched.
  - Validation performed: failing-test-first against real PostgreSQL and against the query fakes, then the full orchestrator suite, the Postgres integration suite on a fresh database, lint and type checks.
  - Commands executed:
    - Failing-first, real PostgreSQL, before the fix — the two new integration tests failed, and instrumenting `test_successful_developer_execution_advances_to_the_local_pipeline` printed exactly the outcome issue #141 describes:
      `AFTER DEVELOPER EVENT -> run status: preparing | phase: preparing | job status: running | graph nodes run since dispatch: 0 | requests: [('developer', 1, 'completed'), ('planner', 1, 'completed')] | outbox: ['processed']`
      No transition, no new outbox row, no `pipeline` request, job left `running` for `expire_leases` to sweep.
    - Failing-first, fakes, before the fix — `PYTHONPATH=orchestrator/src .venv/bin/python3 -m unittest discover -s orchestrator/tests -p test_asyncpg_control_plane.py` → `Ran 75 tests ... FAILED (failures=5)`, the five being the new `test_successful_developer_event_advances_the_implementing_phase`, `..._the_pushing_phase`, `test_accept_offer_records_readiness_without_clobbering_the_phase`, `test_expire_leases_preserves_the_phase_of_the_run_it_fences`, `test_recovery_reoffer_preserves_the_phase_it_is_recovering`.
    - `make test-orchestrator` → `Ran 458 tests in 1.372s ... OK (skipped=33)` (449 before this change).
    - `LOOP_TEST_DATABASE_URL="postgresql://moirai:moirai@127.0.0.1:55141/moirai" make test-postgres-integration` → `Ran 33 tests in 5.054s ... OK`, against a freshly created throwaway PostgreSQL 16 container on a port unique to this session, removed afterwards. (30 before this change.)
    - Each half of the fix was proved load-bearing independently. With only the `accept_offer`/`accept_event` half applied and the two recovery sweeps reverted, `test_developer_execution_recovered_from_a_lease_expiry_still_transitions` fails with `'recovering' != 'implementing'` while the direct test passes. With the `app.job_offers` guard removed, `test_an_accepted_run_that_reported_nothing_is_never_treated_as_bootstrap` fails with `'cancelled' != 'recovering'`. With the production `SELECT` mutated back to `w.status AS workflow_phase`, the two developer-transition unit tests fail.
    - `make lint` → `All checks passed!`
    - `make typecheck MYPY_CACHE=/tmp/moirai-mypy-cache-issue-141` → `Success: no issues found in 48 source files` (own cache directory, so it cannot collide with a sibling worktree).
  - Notes:
    - The issue's claim held exactly as written on `5903aa0`, including the reproduction it quotes. Its "How to tackle" section is stale in one detail: it says `current_phase = 'preparing'` is read by `_release_unanswered_offer`'s bootstrap predicate, but #91 reworked that predicate to read `current_phase = 'offered'`. That is what made option 1 (stop the clobber) viable without touching `runner_events.py` at all — the whole fix is inside `persistence/control_plane.py`.
    - Option 2 from the issue (make the `developer` branch phase-independent) was rejected: `implement` and `push` dispatch the same role with the same request shape, so nothing in the request distinguishes them. Deriving it from the graph's suspension point would mean reading the LangGraph checkpoint from the control plane, which is a much larger coupling than keeping one column honest.

## Post-review corrections

An adversarial review of the diff was run before committing. Its substantive finding, and what was done about it:

- **The widened bootstrap arm turned a bounded failure into an unbounded loop** (major, self-inflicted). `_release_unanswered_offer`'s `bootstrap` predicate reads `current_phase = 'offered'`, which used to mean "never accepted a job" only because `accept_offer` overwrote the phase. Preserving the phase made that proxy also match a run whose first offer *was* accepted by a runner that then died silently — no `started` event, so no `app.executions` row, no branch, no request — and the first draft accepted that as "nothing to preserve, so cancelling is fine". It is not fine, and the review proved why against real PostgreSQL: cancelling releases the project lock and leaves the issue eligible, so `schedule()` immediately builds a **new** run with a **new** job, and the unanswered-offer streak is counted per job — so `unanswered_offer_limit`, the bound that is supposed to stop exactly this, resets on every cycle. A bad runner image would churn runs forever instead of blocking one. Fixed by grounding the predicate in the fact it actually means: `NOT EXISTS (SELECT 1 FROM app.job_offers WHERE job_id = j.id AND status = 'accepted')`, keeping `current_phase = 'offered'` alongside it. That restores the pre-change reachability of the arm exactly. Covered by `test_an_accepted_run_that_reported_nothing_is_never_treated_as_bootstrap` (PostgreSQL) and `test_expire_offers_never_cancels_a_run_a_runner_already_accepted` (fake), both confirmed failing with the new guard removed.
- **The unit fakes did not test the SQL** (minor). The review demonstrated that changing the production `SELECT` to `w.status AS workflow_phase` — the exact bug, reintroduced — left the whole `test_asyncpg_control_plane.py` suite green, because the fakes returned a `workflow_phase` key regardless of what the query asked for. Only the PostgreSQL tests caught it. The three fakes that answer that query now resolve the alias from the statement (`_selected_phase`), so whichever `app.workflow_runs` column the SELECT names is what they serve; the same mutation now fails `test_successful_developer_event_advances_the_implementing_phase` and `..._the_pushing_phase`. `_DurableConnection` likewise now parses the literal each column is actually assigned (`_assigned_literal`) instead of assuming `current_phase` always receives the same value as `status`.
- **Three documentation claims were wrong or overstated** (minor). `status` is not `offered` "while an offer is outstanding" — `schedule_execution` leaves the committed phase in place until the runner accepts; the `phase` field is `workflow_runs.current_phase` in the workflow *list* only, since `SubmitHumanDecision` echoes the graph state's status into both fields; and the paragraph describing the widened bootstrap arm described behaviour that the fix above removes. All three corrected.
- Two findings were confirmed as clean by the review and needed no change: no remaining job-lifecycle writer of `current_phase`, and no consumer of either column that breaks now that they can differ (`find_stalled_workflow_runs`, `close_orphaned_execution_requests`, `recover_one`'s candidate query, `schedule`/`schedule_execution`, `issue_sync`, `load_state` and the runtime all read `status`, which is unchanged).

## Validation Status

- Targeted tests: Passed — three new PostgreSQL integration tests (`test_successful_developer_execution_advances_to_the_local_pipeline`, `test_developer_execution_recovered_from_a_lease_expiry_still_transitions`, `test_an_accepted_run_that_reported_nothing_is_never_treated_as_bootstrap`) and six new unit tests, every one confirmed failing with the production change it covers reverted.
- Service tests: Passed — `make test-orchestrator` → `Ran 458 tests ... OK (skipped=33)`; `make test-postgres-integration` → `Ran 33 tests ... OK` against a fresh throwaway database.
- Full repository tests: Not run — no Go, proto, or web change. `make test` was deliberately not invoked.
- Build: Not applicable — Python only.
- Lint: Passed — `make lint`.
- Type checks: Passed — `make typecheck MYPY_CACHE=/tmp/moirai-mypy-cache-issue-141`.
- Database migrations: None added. `MigrationRunner` ran `001`–`007` against the throwaway database as part of the integration suite.
- Docker Compose: Not run — no Compose or configuration change.
- End-to-end workflow: Partially — `test_successful_developer_execution_advances_to_the_local_pipeline` drives seed → `schedule` → `accept_offer` → graph → planner event → `schedule_execution` → `accept_offer` → developer event → `pipeline` dispatch through the real control plane, real graph runtime and real PostgreSQL, with nothing about `app.workflow_runs` patched by hand.

## Known Issues

- Issue: `recover_stalled_workflow_run` hands `wr.status` to the graph runtime, which can now be a scheduling state rather than a phase.
  - Severity: P3 — pre-existing, unchanged by this session.
  - Impact: the "transition committed but never invoked" branch calls `on_transition(run, status, {"awaiting_execution": False})`, so the graph state's `status` key can read `preparing`. Harmless today: nothing routes on `status` except `issue_graph`'s `blocked` check and `runtime`'s terminal-status check, and the node the graph resumes into writes its own status immediately.
  - Evidence: `persistence/control_plane.py` `recover_stalled_workflow_run` selects `wr.status`; that branch is only reachable when the run has no job in `offered`/`preparing`/`running`/`recovering`, which is exactly when the status and the phase are already in agreement.
  - Suggested resolution: pass `current_phase` instead, once something depends on the distinction. Not changed here: it would widen this diff into #94's recovery machinery with no demonstrated defect.

- Issue: `_release_unanswered_offer`'s two early-return arms commit no `offer_unanswered` workflow event.
  - Severity: P3 — pre-existing, unchanged by this session.
  - Impact: a bootstrap run cancelled by an unanswered offer, and an offer released for an already-terminal run, leave no entry in `app.workflow_events`. The audit trail only records the requeue/block arms, so the console shows a run vanishing with `terminal_reason = 'offer_expired'` and nothing explaining the decision.
  - Evidence: `persistence/control_plane.py` `_release_unanswered_offer` — both `return True` branches sit above the `INSERT INTO app.workflow_events ... 'offer_unanswered'`. Identical at `5903aa0`.
  - Suggested resolution: emit the event before each early return. Not done here: this session narrowed that arm rather than widening it, so it adds no new instances, and the change belongs with #91's paths.

- Issue: `InMemoryControlPlane` cannot express the status/phase split, so it still models the bug this issue fixes.
  - Severity: P3 — test double only, never constructed by `main.py`.
  - Impact: `domain/control_plane.py` stores one `WorkflowStatus` per run, sets it to `PREPARING` on `accept_offer`, and feeds it to `workflow_transition_for_terminal_event`. Any future test written against that double would see a developer terminal event produce no transition, and would "prove" behaviour the real control plane no longer has.
  - Evidence: `domain/control_plane.py` — `accept_offer` assigns `WorkflowStatus.PREPARING`; there is no phase field to preserve.
  - Suggested resolution: give the double a `current_phase` alongside `status` and mirror the invariant. Not done here: the file is outside this issue's ownership and the fix needs no change to it.
