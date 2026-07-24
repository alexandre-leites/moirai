# Production Readiness Review

## Review metadata

- Date: 2026-07-25
- Reviewer: Production-readiness review agent
- Repository revision: `0ebd54e`
- Review scope: actual source, schema, protocol, deployment, documentation, tests, and configured validation.
- Validation environment: Linux with Go, Python, Node, npm, Docker CLI, and Make. `grpcio`, PostgreSQL test infrastructure, Buf/protoc, Ruff, mypy, pytest, and web dependencies were unavailable.
- Important limitations: no live PostgreSQL, non-skipped gRPC, Docker runtime/image, OpenCode, GitHub CLI, browser, migration, or end-to-end workflow execution was performed.

## Executive summary

**Ready only for development.** The repository contains several good foundations—transactional scheduling intent, lease generation/sequence fencing, scrypt-based password storage, private runner identity storage, strict task packets, argument-vector process/GitHub invocations, and process-group cancellation. Those foundations are not yet connected into a durable product workflow.

The operational delivery path is incomplete: the LangGraph graph is composed of placeholder nodes; no migration lifecycle or operational scheduler/recovery loop starts; the API and UI are stubs; the Compose runner cannot bootstrap or execute Git/OpenCode; and there is no demonstrated workflow from an issue to an approved merge. It is not suitable for unattended homelab operation or write-capable GitHub credentials.

## Overall readiness assessment

**Ready only for development.**

## Highest risks

1. The durable engineering workflow, checkpointing, gates, PR lifecycle, approval, merge, and completion path are absent.
2. A fresh deployment has no migration execution/readiness gate; the shipped runner lacks a registration token, Git, and OpenCode.
3. Agent processes inherit runner environment secrets, can access runner-owned state, and runtime role restrictions are not enforced.
4. Expected restarts, disconnects, and duplicate delivery lack an operating expiry/reconciliation/idempotency path.
5. The public operator plane and observability are insufficient to control or diagnose the system.

## System inventory

| Component | Classification | Evidence |
|---|---|---|
| Web UI | stubbed | `web/src/main.tsx:7-15` renders a health placeholder and calls unavailable `/api/v1/health`. |
| Public API | stubbed | `api/cmd/api/main.go:9-22` only serves unconditional `/live` and `/ready`. |
| Internal control plane | partially implemented | Authenticated list/token RPC foundation in `orchestrator/src/moirai/grpc/control_plane.py:35-122`; no API client or public transport. |
| Authentication/session persistence | partially implemented | scrypt, hashed session/CSRF storage, validation, and login audit in `persistence/authentication.py:39-267`; no browser cookie/origin/rate-limit boundary. |
| PostgreSQL persistence/schema | partially implemented | initial schema in `orchestrator/migrations/001_initial.sql`; asyncpg control plane exists but migrations are not executed at startup. |
| Runner registration/identity | implemented foundation | hash-based registration and `0600` atomic identity storage; no Compose enrollment/rotation path. |
| Runner streams/offers/leases | partially implemented | runner control and stream logic exist; no production reconciler or durable outbox/replay lifecycle. |
| Scheduler/project locks | partially implemented | transactional selection logic exists; `main.py:29-61` starts no scheduler/sweeper/leadership service. |
| Issue synchronization | partial/test-only | parsing and GitHub adapter seams exist; no periodic service or durable reconciliation wiring. |
| Repository/worktree manager | partially implemented | managed clone/existing-path manager in `runner/internal/repository/manager.go:42-177`; unsafe root policy/late cleanup remain. |
| Local/Docker execution | partial | local supervisor is active; Docker executor exists but bootstrap selects local OpenCode only (`runner/cmd/runner/main.go:53-62`). |
| OpenCode backend | partially implemented | adapter/result validation exists; final runner image excludes `opencode`. |
| LangGraph workflow | stubbed | `workflows/issue_graph.py:48-69` uses lambdas and ends after `push`, without a checkpointer. |
| Pipeline/review/repair | test-only foundation | policy routing is unit-tested but not invoked by the graph. |
| GitHub PR/check/merge | partial | adapter seams exist but are not wired into workflow execution. |
| Human approval/automatic merge/issue completion | documented but absent | absent from graph and public control path. |
| Logging/metrics/audit | partial/absent | startup JSON line in `main.py:51-57`; authentication audit writes exist; no common correlation, metrics, retention, or operator query path. |
| Health/deployment/operations | partial | PostgreSQL-only health check in `compose.yaml:11-15`; API readiness is unconditional. |

## Critical workflow assessment

| Workflow | Actual assessment |
|---|---|
| UI → API → orchestrator → PostgreSQL | Broken. UI requests an unavailable endpoint; API has no gRPC client/resource handlers. |
| Registration → runner lifecycle | Foundations exist, but Compose supplies no token and runner image lacks runtime dependencies. Stream reconnection/recovery is not end-to-end proven. |
| Issue sync → scheduling → offer | Candidate selection and offer persistence are foundations; no sync/scheduler/reconciliation loop invokes them. |
| Issue → engineering delivery → merge | Absent. Workspace/OpenCode invocation exists but the graph has no real nodes/checkpoints/PR/check/approval/merge path. |
| Failure/recovery | Partial process cancellation only. No durable expiry sweeper, restart reconciliation, migration gate, or verified idempotent external-side-effect recovery. |

## Validation executed

| Command | Result | Notes |
|---|---|---|
| `go vet ./... && go test -race ./...` in `runner/` | Passed | Unit/race coverage; no real gRPC/Docker/OpenCode test. |
| `go vet ./... && go test ./...` in `api/` | Passed | No API tests; only health handlers exist. |
| `go vet ./... && go test ./...` in `gen/go/` | Passed | Generated packages have no tests. |
| `PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=src python3 -m unittest discover -s tests -v` in `orchestrator/` | Passed: 60, skipped: 9 | All gRPC cases skipped because `grpcio` is unavailable; asyncpg tests use fakes, not PostgreSQL. |
| `PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=src python3 -m compileall -q src tests` in `orchestrator/` | Passed | Syntax/bytecode only. |
| `docker compose config --quiet` | Passed | Syntax only; stack/images were not run. |
| `npm run build` in `web/` | Could not run | `tsc: not found`; dependencies are absent. |
| `make validate` | Not run | Would require unavailable Ruff, mypy, Docker/Buf workflow dependencies. |

## Findings summary

| ID | Severity | Category | Title | Status |
|---|---|---|---|---|
| PRR-001 | Critical | Workflow durability | Durable engineering workflow is placeholder-only | confirmed |
| PRR-002 | Critical | Deployment/data integrity | Fresh deployment has no schema migration lifecycle | confirmed |
| PRR-003 | Critical | Runner reliability | Shipped Compose runner cannot enroll or execute work | confirmed |
| PRR-004 | High | Agent safety | Agent execution inherits secrets and lacks enforced role isolation | confirmed |
| PRR-005 | High | Coordination/recovery | No operating scheduler, expiry sweeper, or restart reconciliation | confirmed |
| PRR-006 | High | API/operator plane | API/UI request path is stubbed and misleadingly ready | confirmed |
| PRR-007 | High | Transport security | Credential-bearing gRPC is insecure by default | confirmed |
| PRR-008 | High | Observability/audit | No actionable correlated operations telemetry or truthful readiness | confirmed |
| PRR-009 | High | Testing/delivery | CI and live boundary coverage are absent | confirmed |
| PRR-010 | Medium | Idempotency | Offer/event retry semantics are brittle | strongly indicated |
| PRR-011 | Medium | Data integrity | Key ownership/lease invariants lack database enforcement | strongly indicated |
| PRR-012 | Medium | Filesystem safety | Worktree artifacts can follow repository-controlled symlinks | strongly indicated |
| PRR-013 | Medium | GitHub integration | Issue listing/replay handling are incomplete | confirmed |
| PRR-014 | Medium | Operations/docs | Compose hardening and operator contract are incomplete | documentation mismatch |

## Detailed findings

### PRR-001 — Durable engineering workflow is placeholder-only

- Severity: Critical
- Category: LangGraph workflow durability
- Components: Orchestrator workflows, runner dispatch, GitHub adapters
- Status: confirmed
- Evidence:
  - `orchestrator/src/moirai/workflows/issue_graph.py:48-69`
  - `orchestrator/src/moirai/workflows/policy.py:7-94`
- Current behavior: Graph nodes only mutate simple counters/status and execution ends after `push`; it has no PostgreSQL checkpointer, real execution request, PR/check/approval/merge/closure node, or replay-safe side effects.
- Failure scenario: A restart or partial provider call cannot resume safely; the promised delivery gates are never authoritatively exercised.
- Impact: No reliable issue-to-merge capability; unsafe ad hoc wiring would risk duplicate/invalid external actions.
- Likelihood: Certain for the intended workflow.
- Production-readiness consequence: Release blocker.
- Recommended direction: Implement persisted, idempotent graph nodes synchronized with application state and provider reconciliation.
- Suggested validation: PostgreSQL-backed restart/replay tests at every side-effecting node, including approval and lost-runner recovery.
- Related findings: PRR-005, PRR-010, PRR-013.

### PRR-002 — Fresh deployment has no schema migration lifecycle

- Severity: Critical
- Category: Deployment and data integrity
- Components: PostgreSQL, orchestrator startup, Compose
- Status: confirmed
- Evidence:
  - `orchestrator/migrations/001_initial.sql:1`
  - `orchestrator/src/moirai/main.py:38-50`
  - `compose.yaml:17-25`
- Current behavior: Startup opens the control-plane persistence and starts gRPC without applying or verifying versioned migrations.
- Failure scenario: A fresh volume lacks application tables; upgrades have no serialized schema procedure.
- Impact: Deployment failure and unsafe data evolution.
- Likelihood: Certain on a clean installation.
- Production-readiness consequence: Release blocker.
- Recommended direction: Introduce versioned migration execution/verification protected by an advisory lock and readiness failure on incompatibility.
- Suggested validation: Fresh-volume, upgrade, concurrent-startup, failure, and restore drills.
- Related findings: PRR-005, PRR-011, PRR-014.

### PRR-003 — Shipped Compose runner cannot enroll or execute work

- Severity: Critical
- Category: Runner reliability and deployment
- Components: Compose, runner image, runner bootstrap
- Status: confirmed
- Evidence:
  - `compose.yaml:40-45` supplies no `LOOP_RUNNER_REGISTRATION_TOKEN`.
  - `runner/internal/control/identity.go:24-37` requires a token when no identity exists.
  - `runner/Dockerfile:6-11` contains only the runner binary; no Git or OpenCode executable.
  - `runner/internal/repository/manager.go:180-193` and `runner/internal/agents/opencode.go:72-78` require those executables.
- Current behavior: A first-run runner has no identity and cannot register; even provisioned identity cannot perform repository/OpenCode work in the final image.
- Failure scenario: Compose looks healthy enough to start but no runner can claim/execute work.
- Impact: Complete absence of an executable worker fleet.
- Likelihood: Certain in the supplied deployment.
- Production-readiness consequence: Release blocker.
- Recommended direction: Define secure bootstrap enrollment and runtime image/executor mode requirements; add startup dependency/readiness checks.
- Suggested validation: Fresh-volume Compose runner enrollment and a controlled task execution smoke test.
- Related findings: PRR-002, PRR-004, PRR-014.

### PRR-004 — Agent execution inherits secrets and lacks enforced role isolation

- Severity: High
- Category: Agent execution safety
- Components: Local executor, OpenCode backend, task packets, runner identity
- Status: confirmed
- Evidence:
  - `runner/internal/execution/local.go:135-140` starts from `os.Environ()`.
  - `runner/internal/control/identity.go:49-105` retains credential under runner data.
  - `runner/internal/taskpacket/taskpacket.go:106-117` validates declared permissions but `runner/cmd/runner/main.go:53-62` creates one local OpenCode path for roles.
- Current behavior: Agent/repository processes inherit runner environment and use a writable local workspace without runtime role-specific capabilities.
- Failure scenario: Prompt-injected/repository code reads credentials or mutates/pushes during planner/reviewer work.
- Impact: Runner impersonation and unauthorized repository changes.
- Likelihood: High when processing untrusted issue/repository content.
- Production-readiness consequence: Block write-capable automation.
- Recommended direction: Use a minimal allowlisted environment, separate job identity/mounts, enforce role capabilities at executor level, and redact/bound outputs.
- Suggested validation: Adversarial fixture proving secrets are inaccessible and reviewer/planner writes, pushes, and forbidden networking fail.
- Related findings: PRR-007, PRR-008, PRR-012.

### PRR-005 — No operating scheduler, expiry sweeper, or restart reconciliation

- Severity: High
- Category: Distributed coordination and recovery
- Components: Scheduler, leases, sessions, orchestrator lifecycle
- Status: confirmed
- Evidence:
  - `orchestrator/src/moirai/main.py:29-61` starts only gRPC.
  - `orchestrator/src/moirai/grpc/sessions.py:49-86` is in-memory.
  - `orchestrator/src/moirai/persistence/control_plane.py:249-487` contains persistence operations but no lifecycle service.
- Current behavior: Scheduling/offer/lease operations exist as callable foundations; no started loop performs issue sync, delivery retry, offer expiry, heartbeat TTL, lease recovery, or restart reconciliation.
- Failure scenario: A disconnect/restart leaves work or locks stranded, or a later manual retry duplicates work.
- Impact: Stuck projects and violated ownership/recovery expectations.
- Likelihood: High during normal homelab restarts/network loss.
- Production-readiness consequence: Release blocker for unattended use.
- Recommended direction: Add leader-protected scheduled reconciliation and explicit recoverable/blocked outcomes.
- Suggested validation: Real PostgreSQL fake-clock tests for disconnect, restart, expiry, reconnect, and stale completion.
- Related findings: PRR-001, PRR-010, PRR-011.

### PRR-006 — API/UI request path is stubbed and readiness is misleading

- Severity: High
- Category: Public API and operator experience
- Components: API, web, Compose
- Status: confirmed
- Evidence:
  - `web/src/main.tsx:9-12` calls `/api/v1/health`.
  - `api/cmd/api/main.go:14-21` has no matching route and returns readiness unconditionally.
  - `web/Dockerfile:6-8` contains no API reverse-proxy configuration.
- Current behavior: No login, project, runner, queue, workflow, approval, log, or recovery operation reaches the orchestrator.
- Failure scenario: Operators see running containers but cannot configure, inspect, or recover actual work.
- Impact: No usable management plane.
- Likelihood: Certain.
- Production-readiness consequence: Release blocker for web-managed homelab operation.
- Recommended direction: Build the authenticated API→gRPC boundary and minimum operational UI vertical slice with truthful readiness.
- Suggested validation: Compose browser/API smoke through login, project configuration, runner status, and workflow action/error flows.
- Related findings: PRR-008, PRR-009.

### PRR-007 — Credential-bearing gRPC is insecure by default

- Severity: High
- Category: Transport security
- Components: Orchestrator, runner, Compose
- Status: confirmed
- Evidence:
  - `orchestrator/src/moirai/main.py:42-45` uses `add_insecure_port`.
  - `runner/internal/config/config.go:67-69` defaults TLS off.
  - `runner/internal/control/dialer.go:19-23` permits insecure transport.
- Current behavior: Runner registration and bearer credentials traverse plaintext control transport.
- Failure scenario: A control-network observer/peer captures or replays credentials.
- Impact: Runner impersonation/control-plane compromise.
- Likelihood: Moderate on isolated single host; high before remote runners.
- Production-readiness consequence: Blocks remote/shared-network runner operation.
- Recommended direction: Require TLS/mTLS for remote endpoints; make insecure mode explicit, development-only, and documented.
- Suggested validation: TLS/mTLS acceptance, rejection, rotation, and no-plaintext remote configuration tests.
- Related findings: PRR-004, PRR-014.

### PRR-008 — Operations telemetry, audit coverage, and readiness are insufficient

- Severity: High
- Category: Observability and auditability
- Components: Orchestrator, runner, API, UI
- Status: confirmed
- Evidence:
  - `orchestrator/src/moirai/main.py:51-57` emits startup only.
  - `api/cmd/api/main.go:18-20` is dependency-independent readiness.
  - `orchestrator/migrations/001_initial.sql:254-263` defines audit storage while authentication is the only evidenced writer (`persistence/authentication.py:154-162`).
- Current behavior: No shared request/workflow/job/execution/runner correlations, metrics, retention, operator log/event query path, or comprehensive audit trail.
- Failure scenario: Operator cannot diagnose stuck work, provider failure, or sensitive changes; raw agent output may retain secrets.
- Impact: Extended outage and unsafe manual recovery.
- Likelihood: High.
- Production-readiness consequence: Release blocker for continuous operation.
- Recommended direction: Add structured/redacted/bounded logs, homelab metrics, dependency-aware health, and append-only audit events at every sensitive transition.
- Suggested validation: Dependency outage, metric scrape, redaction, correlation, retention, and audit authorization tests.
- Related findings: PRR-004, PRR-005, PRR-014.

### PRR-009 — CI and live boundary coverage are absent

- Severity: High
- Category: Testing and delivery assurance
- Components: CI, all services
- Status: confirmed
- Evidence:
  - No `.github` workflow was found.
  - `Makefile:3-26` omits Go/API/web builds and runtime integration.
  - Review validation skipped nine gRPC tests and could not build web dependencies.
- Current behavior: A merge can avoid real database, gRPC, Compose, Docker/OpenCode, API, and browser validation.
- Failure scenario: boundary regressions ship unnoticed.
- Impact: No defensible release gate.
- Likelihood: High.
- Production-readiness consequence: Release blocker.
- Recommended direction: Establish reproducible CI with live dependencies and clear unit/integration/e2e strata.
- Suggested validation: Clean CI run with intentional generated-contract/integration failures.
- Related findings: PRR-001 through PRR-008.

### PRR-010 — Offer and event retries are not safely acknowledged

- Severity: Medium
- Category: Idempotency and correctness
- Components: Control-plane persistence, runner stream
- Status: strongly indicated
- Evidence:
  - `orchestrator/src/moirai/persistence/control_plane.py:351-379` accepts an offer through one state transition.
  - `orchestrator/src/moirai/persistence/control_plane.py:457-477` rejects non-increasing event sequence rather than recognizing matching replay.
- Current behavior: A lost acknowledgement can turn a valid retry into a failure; skipped sequence handling is ambiguous.
- Failure scenario: At-least-once delivery causes stuck job/recovery behavior.
- Impact: Network fragility and duplicate manual recovery.
- Likelihood: Moderate.
- Production-readiness consequence: Must close with recovery work.
- Recommended direction: Persist idempotency identity/payload digest and acknowledge exact repeats while rejecting conflicts; server-cap renewal TTL.
- Suggested validation: Lost ack, duplicate, conflicting replay, out-of-order, and stale-generation integration tests.
- Related findings: PRR-005, PRR-011.

### PRR-011 — Key ownership and lease invariants lack database enforcement

- Severity: Medium
- Category: Data integrity and concurrency
- Components: Schema, scheduler, leases
- Status: strongly indicated
- Evidence:
  - `orchestrator/migrations/001_initial.sql:169-194` does not make `job_offers.job_id` unique.
  - `persistence/control_plane.py:434-455` accepts client-supplied future renewal time.
- Current behavior: Important cross-row ownership, active offer, and recovery fencing rules depend primarily on application behavior.
- Failure scenario: Future recovery/retry paths create inconsistent job/offer/project relationships.
- Impact: Duplicate work or invalid locks.
- Likelihood: Moderate as features are wired.
- Production-readiness consequence: Required before reassignment/recovery.
- Recommended direction: Add constraints/indexes and atomically server-issued/capped lease lifetimes/fencing increments.
- Suggested validation: Migration and concurrent PostgreSQL violation tests plus query-plan inspection.
- Related findings: PRR-005, PRR-010.

### PRR-012 — Workspace artifact write safety and cleanup are incomplete

- Severity: Medium
- Category: Filesystem safety and retention
- Components: Runner repository manager, OpenCode artifacts
- Status: strongly indicated
- Evidence:
  - `runner/internal/repository/manager.go:71-77` leaves a worktree when `.loop` creation fails.
  - `runner/internal/repository/manager.go:82-97,238-242` accepts any absolute existing path without an allowed root.
  - Artifact path validation is lexical before later filesystem operations in `runner/internal/agents/opencode.go`.
- Current behavior: Repository-controlled filesystem state can influence artifact paths; failures can leak worktrees; existing path has no configured containment root.
- Failure scenario: Symlink/path confusion writes outside expected task space or accumulated stale workspaces exhaust disk.
- Impact: Runner-local filesystem risk and reliability degradation.
- Likelihood: Moderate.
- Production-readiness consequence: Required before untrusted repository processing.
- Recommended direction: descriptor/symlink-safe creation, configured existing-path roots, compensating cleanup, and bounded retention/quarantine.
- Suggested validation: Symlink escape, path-root, failure injection, and retention tests.
- Related findings: PRR-003, PRR-004.

### PRR-013 — GitHub synchronization/replay behavior is incomplete

- Severity: Medium
- Category: GitHub integration
- Components: Issue tracker, code host, workflow
- Status: confirmed
- Evidence:
  - `orchestrator/src/moirai/issue_trackers/github_cli.py:57-75` lists without explicit pagination and closes without already-closed reconciliation.
- Current behavior: Large issue sets may be omitted and recovery may fail after a successful close.
- Failure scenario: Global queue silently misses eligible issues or a retry blocks after external success.
- Impact: Missing work and fragile completion.
- Likelihood: High for larger queues; moderate on replay.
- Production-readiness consequence: Required before multi-project scheduling claim.
- Recommended direction: Add explicit bounded pagination, error classification/backoff, reconciliation, and graph-level idempotency keys.
- Suggested validation: Multi-page, already-closed/merged/existing-PR, rate-limit, and outage fixtures.
- Related findings: PRR-001, PRR-005.

### PRR-014 — Compose deployment and operator contract are incomplete

- Severity: Medium
- Category: Deployment, operations, and documentation
- Components: Compose, Dockerfiles, README, `.env.example`
- Status: documentation mismatch
- Evidence:
  - `compose.yaml:1-60` has no restart policies, resource/log controls, service health checks beyond PostgreSQL, or configured runner bootstrap.
  - `README.md:5-19` and `.env.example:1-2` do not describe the actual migration/enrollment/recovery contract.
  - `orchestrator/Dockerfile:1-8` runs default root; `web/Dockerfile:6-8` has no explicit hardened runtime policy.
- Current behavior: Documented startup does not produce a usable platform and lacks backup/restore/upgrade/rollback guidance.
- Failure scenario: Routine service restart, disk pressure, or operator recovery leads to extended outage or unsafe improvisation.
- Impact: Homelab instability.
- Likelihood: Moderate to high.
- Production-readiness consequence: Required for controlled homelab release.
- Recommended direction: Harden the single-host deployment proportionately and publish tested runbooks bound to implemented behavior.
- Suggested validation: Fresh volume, restart, resource/log, backup/restore, and documentation walkthrough drills.
- Related findings: PRR-002, PRR-003, PRR-008.

## Positive findings

- PostgreSQL is isolated to the internal database network in `compose.yaml:25,47-50`.
- Password verification uses salted scrypt and constant-time comparison in `persistence/authentication.py:39-81`.
- Sessions, CSRF values, runner registration tokens, and runner credentials are persisted as hashes.
- Runner identity writes use a private temporary file, sync, atomic rename, and restrictive mode.
- Scheduling/offer persistence takes transactional locks and event persistence fences runner, generation, and sequence.
- Task packets reject unknown fields/traversal/unsafe repository sources and Git/GitHub commands use argument vectors.
- Local process supervision creates and terminates Unix process groups; Docker defaults to disabled network.

## Coverage gaps and unverified assumptions

- No live PostgreSQL migration, constraint, locking, backup, or restore test.
- All gRPC service tests skipped due to missing `grpcio`.
- No Compose build/start, Docker executor, OpenCode, GitHub CLI, TLS, or browser test.
- No dependency vulnerability scan or secret scanning tool was available/configured; no vulnerability claim is made.
- Existing uncommitted implementation work was reviewed as current source but was not modified.

## Documentation mismatches

- `PROJECT.md` describes target architecture; current source implements foundations and stubs described above.
- `README.md` startup instructions do not cover current schema initialization, runner enrollment, required execution dependencies, TLS boundary, or recovery.
- `.env.example` is not an evident complete Compose/service configuration contract.

## Recommended release gates

1. Close PRR-001 through PRR-009 and demonstrate a secure fake-provider workflow before any write-capable credentials.
2. Demonstrate fresh Compose migration, enrollment, truthful readiness, and one bounded execution with telemetry.
3. Demonstrate orchestrator and runner loss/restart, duplicate delivery, existing PR, failed checks, and approval recovery without duplicate side effects.
4. Require CI coverage for actual PostgreSQL, gRPC, Compose, API, and browser boundaries.
5. Execute documented backup/restore and upgrade/rollback drills.

## Recommended review follow-up

Re-review after the Now tasks in `PRODUCTION_READY_TASKS.md` are complete, using real PostgreSQL, gRPC TLS, Compose runtime, browser, sandboxed OpenCode, and fake-provider recovery suites.
