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

