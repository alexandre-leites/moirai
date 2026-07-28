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
