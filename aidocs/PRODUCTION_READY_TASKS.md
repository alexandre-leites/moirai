# Production-Ready Engineering Backlog

## Backlog metadata

- Generated from review: `PRODUCTION_READINESS_REVIEW.md` on 2026-07-25
- Repository revision: `0ebd54e` with pre-existing uncommitted implementation changes
- Total tasks: 12
- Critical: 3
- High: 6
- Medium: 3

## Executive priority order

1. PRT-001 — Implement checkpointed, idempotent engineering workflow orchestration.
2. PRT-002 — Establish versioned migrations and database readiness.
3. PRT-003 — Make the runner image and enrollment path executable.
4. PRT-004 — Isolate agent execution and enforce task-role capabilities.
5. PRT-005 — Implement scheduler/recovery/replay lifecycle.
6. PRT-006 — Deliver authenticated API/UI operator vertical slice.
7. PRT-007 — Secure remote runner transport and credential lifecycle.
8. PRT-008 — Add correlated telemetry, audit coverage, and truthful health.
9. PRT-009 — Establish CI and real integration release gates.
10. PRT-010 — Enforce database coordination invariants.
11. PRT-011 — Harden GitHub reconciliation and side effects.
12. PRT-012 — Harden filesystem isolation, Compose operations, and runbooks.

## Dependency map

```text
PRT-002 ─┬─> PRT-001 ─> PRT-011
         ├─> PRT-005 ─> PRT-010
         └─> PRT-009
PRT-003 ─> PRT-004 ─> PRT-001
PRT-007 ─> PRT-003
PRT-005 ─> PRT-001
PRT-008 ─> PRT-012
PRT-006 ─> PRT-009
```

## Now

PRT-001 through PRT-009 are required before production-style repository automation.

## Next

PRT-010 and PRT-011 must follow before unattended multi-project operation.

## Later

PRT-012 completes hardening and documented operations; do not defer its filesystem safety portion when processing untrusted repository content.

## Tasks by category

## Release blockers

## PRT-001 — Implement checkpointed, idempotent engineering workflow orchestration

- Priority: P0 / Now
- Severity source: Critical
- Category: Workflow durability and correctness
- Related findings: PRR-001, PRR-005, PRR-010, PRR-013
- Components: Orchestrator workflow graph, persistence, runner dispatch, providers
- Estimated size: XL
- Dependencies: PRT-002, PRT-004, PRT-005
- Can run in parallel with: PRT-006, PRT-008, PRT-009
- Blocks: Any claim of automated issue delivery, review, merge, or completion
- Production risk addressed: Placeholder workflow cannot perform or recover a gated delivery process.

### Problem

`issue_graph.py` provides placeholder state mutations and terminates at push. There is no checkpointer, real node execution, external-side-effect identity, approval interrupt, check monitoring, merge, or issue completion behavior.

### Evidence

- `orchestrator/src/moirai/workflows/issue_graph.py:48-69`
- `orchestrator/src/moirai/workflows/policy.py:7-94`
- PRR-001

### Required implementation

Implement persisted graph nodes for prepare, plan, implementation, local pipeline, independent review, repair, push, PR creation/find, checks, human interrupt, merge, and issue completion. Synchronize graph and application state transactionally around each external effect; use durable idempotency/reconciliation keys; bound attempts, execution count, age, and cancellation; retain fresh reviewer context.

### Acceptance criteria

- [ ] Every workflow phase persists checkpoint and application state before/after external effects.
- [ ] Pipeline, review, checks, and human approval gates prevent merge until recorded for the current commit.
- [ ] Replay cannot duplicate branch, PR, comment, merge, or closure.
- [ ] Approval invalidates on reviewed-commit change.
- [ ] Provider/runner failure, cancellation, and retry exhaustion become observable recoverable or blocked states.

### Required tests

- [ ] PostgreSQL-backed restart/replay test for every side-effecting node.
- [ ] Pipeline/review/check repair-bound and fresh-reviewer integration tests.
- [ ] Human interrupt/resume/invalidation test.
- [ ] Existing PR, already merged, partial push, runner loss, and cancellation tests.

### Validation commands

```bash
cd orchestrator && PYTHONPATH=src python3 -m unittest discover -s tests -v
# Run the new PostgreSQL-backed workflow integration suite.
```

### Operational considerations

Persist side-effect markers through backup/restore. Never reassign uncommitted work automatically without reconciliation; expose audited blocked/manual recovery.

### Definition of done

A fake-provider workflow survives restart at each node and reaches merge/issue completion only after all deterministic and human gates pass.

## PRT-002 — Establish versioned migrations and database readiness

- Priority: P0 / Now
- Severity source: Critical
- Category: Deployment and data integrity
- Related findings: PRR-002, PRR-011, PRR-014
- Components: Migrations, orchestrator startup, Compose
- Estimated size: M
- Dependencies: None
- Can run in parallel with: PRT-003, PRT-004, PRT-006
- Blocks: PRT-001, PRT-005, PRT-009, safe upgrades
- Production risk addressed: Fresh installs/upgrades have no authoritative schema lifecycle.

### Problem

The initial SQL schema is not applied or verified by startup. PostgreSQL health only proves server reachability, not schema compatibility.

### Evidence

- `orchestrator/migrations/001_initial.sql:1`
- `orchestrator/src/moirai/main.py:38-50`
- `compose.yaml:11-25`

### Required implementation

Adopt a versioned migration mechanism, serialized by a database advisory lock. Create an explicit Compose job or controlled startup migration phase. Refuse readiness/control-plane work when database connection, migration history, or expected schema is invalid. Document additive upgrade, backup, repair, and rollback behavior.

### Acceptance criteria

- [ ] Fresh volume migrates through one supported documented path.
- [ ] Concurrent migration attempts serialize safely.
- [ ] Migration failure/incompatible version makes orchestrator non-ready without serving work.
- [ ] Applied versions are queryable and upgrade behavior is documented.
- [ ] Backup/restore and rollback boundaries are documented.

### Required tests

- [ ] Fresh PostgreSQL migration integration test.
- [ ] Advisory-lock concurrency test.
- [ ] Upgrade-from-prior-schema fixture test.
- [ ] Database unavailable/migration failure readiness test.

### Validation commands

```bash
docker compose up --build --wait
# Run the new fresh-volume migration integration command.
docker compose down -v
```

### Operational considerations

Back up before non-additive changes; migration credentials must not be exposed to runners. Do not silently run destructive migration steps.

### Definition of done

A clean Compose stack migrates and becomes ready; upgrade and restore tests provide recorded evidence.

## PRT-003 — Make runner enrollment and execution image production-functional

- Priority: P0 / Now
- Severity source: Critical
- Category: Runner reliability and deployment
- Related findings: PRR-003, PRR-014
- Components: Compose, runner image, runner bootstrap/configuration
- Estimated size: M
- Dependencies: PRT-007
- Can run in parallel with: PRT-002, PRT-006, PRT-008
- Blocks: Runner fleet operation and PRT-001 execution nodes
- Production risk addressed: The shipped runner cannot first-register or execute Git/OpenCode.

### Problem

Compose provides no first-use enrollment token. The final runner image contains no Git or OpenCode though dispatch requires both.

### Evidence

- `compose.yaml:40-45`
- `runner/internal/control/identity.go:24-37`
- `runner/Dockerfile:6-11`
- `runner/internal/repository/manager.go:180-193`
- `runner/internal/agents/opencode.go:72-78`

### Required implementation

Provide a secure operator enrollment workflow that does not permanently place plaintext registration tokens in Compose configuration. Build a supported runner image or explicitly use an isolated executor image containing required tools. Add checks for identity, Git, backend, Docker mode, writable disk, and capability reporting before accepting work.

### Acceptance criteria

- [ ] A new runner enrolls through a documented one-time operator procedure.
- [ ] The supported execution mode provides Git and the configured agent backend.
- [ ] Runner rejects work with a diagnostic health/capability state when prerequisites are missing.
- [ ] Runner restart reuses protected identity without requiring token replay.
- [ ] No registration credential is emitted in logs, image layers, or persistent Compose configuration.

### Required tests

- [ ] Fresh-volume enrollment integration test.
- [ ] Container/image smoke test for Git/backend availability.
- [ ] Missing dependency/disk/identity health-state tests.
- [ ] Restart identity reuse and token-replay rejection tests.

### Validation commands

```bash
docker compose build runner
docker compose run --rm runner --help
cd runner && go vet ./... && go test -race ./...
```

### Operational considerations

Prefer explicit secret injection or interactive enrollment to environment-file credentials. Pin execution tool versions and publish image update/rollback policy.

### Definition of done

A clean runner volume securely enrolls, becomes ready only with all dependencies, and completes a controlled non-production task in Compose.

## Security

## PRT-004 — Isolate agent execution and enforce task-role capabilities

- Priority: P0 / Now
- Severity source: High
- Category: Runner and agent security
- Related findings: PRR-004, PRR-012
- Components: Local/Docker executor, OpenCode backend, workspace manager
- Estimated size: L
- Dependencies: PRT-003
- Can run in parallel with: PRT-002, PRT-005, PRT-006
- Blocks: Write-capable agent execution and PRT-001
- Production risk addressed: Agent/repository content can access runner secrets or violate planner/reviewer restrictions.

### Problem

Local jobs inherit `os.Environ()`, runner control identity is runner-readable under data storage, and declared role permissions do not form a runtime sandbox.

### Evidence

- `runner/internal/execution/local.go:135-140`
- `runner/internal/control/identity.go:49-105`
- `runner/cmd/runner/main.go:53-62`
- `runner/internal/taskpacket/taskpacket.go:106-117`

### Required implementation

Use minimal per-job environment allowlists; isolate runner identity/control sockets from execution mounts; enforce per-role write/push/network/credential policy in the executor; make Docker/local selection explicit and security-visible; redact and bound retained output. Do not mount a host Docker socket or reusable broad GitHub credential into job containers.

### Acceptance criteria

- [ ] Jobs cannot read runner identity, registration credential, unrelated data, or unapproved environment values.
- [ ] Planner/reviewer cannot mutate repository, push, merge, or use forbidden network paths.
- [ ] Developer credentials are least-privilege and never retained in artifacts/logs.
- [ ] Executor isolation mode and residual local-mode risk are validated and documented.
- [ ] Cancellation terminates complete process/container groups after isolation.

### Required tests

- [ ] Adversarial environment/credential visibility test.
- [ ] Planner/reviewer write/push/network denial test.
- [ ] Output redaction/size-limit test.
- [ ] Local/Docker cancellation and resource-limit integration tests.

### Validation commands

```bash
cd runner && go vet ./... && go test -race ./...
# Run the new sandbox/adversarial executor integration suite.
```

### Operational considerations

Treat issue, prompt, repository, and agent output as adversarial. Keep production credentials unavailable to agents.

### Definition of done

Adversarial tests prove no runner-secret escape and runtime role restrictions in the supported executor modes.

## PRT-005 — Implement scheduler, lease expiry, and restart-safe stream recovery

- Priority: P0 / Now
- Severity source: High
- Category: Distributed coordination and recovery
- Related findings: PRR-005, PRR-010, PRR-011
- Components: Scheduler, persistence, runner sessions, runner stream
- Estimated size: L
- Dependencies: PRT-002
- Can run in parallel with: PRT-004, PRT-006, PRT-008
- Blocks: PRT-001 and unattended runner operation
- Production risk addressed: Restarts/disconnects strand locks or turn valid retries into failures.

### Problem

The process starts gRPC only; no leader-protected scheduling, issue wakeup, offer expiry, heartbeat TTL, lease recovery, reconnect inventory, durable outbox, or replay acknowledgement exists operationally.

### Evidence

- `orchestrator/src/moirai/main.py:29-61`
- `orchestrator/src/moirai/grpc/sessions.py:49-86`
- `orchestrator/src/moirai/persistence/control_plane.py:249-487`
- `runner/internal/control/client.go:103-192`

### Required implementation

Add a PostgreSQL leader guard and service loop. Sweep offer/lease expiry transactionally, mark offline runners by heartbeat TTL, reconcile active jobs before scheduling after restart, add durable event/outbox/reconnect semantics, acknowledge exact matching retries, reject conflicting retry payloads, and atomically advance fences on reassignment. Explicitly classify retriable versus blocked work.

### Acceptance criteria

- [ ] No expired offer/lease leaves a project locked indefinitely.
- [ ] Reconnect/restart reconciles active work before new project work is scheduled.
- [ ] Exact accepted-offer/event retries return original authoritative result; conflicts fail safely.
- [ ] Stale generation completions cannot alter recovered work.
- [ ] Recovery produces correlated logs, metrics, and audit events.

### Required tests

- [ ] Fake-clock real-PostgreSQL offer/lease sweeper tests.
- [ ] Lost acknowledgement and duplicate/conflicting event tests.
- [ ] Runner/orchestrator restart/reconnect inventory tests.
- [ ] Stale completion after reassignment test.

### Validation commands

```bash
cd orchestrator && PYTHONPATH=src python3 -m unittest discover -s tests -v
cd runner && go test -race ./internal/control ./internal/dispatch
```

### Operational considerations

Do not automatically reassign uncommitted work whose safety cannot be established. Provide audited manual recovery state.

### Definition of done

Integration evidence demonstrates loss/restart/replay recovery with no duplicate project ownership or terminal external side effect.

## API and protocol hardening

## PRT-006 — Deliver authenticated API and operational web vertical slice

- Priority: P0 / Now
- Severity source: High
- Category: API and operator experience
- Related findings: PRR-006, PRR-008, PRR-009
- Components: API, web, gRPC client, Compose proxy/configuration
- Estimated size: XL
- Dependencies: Authentication foundation; stable ControlPlane operations
- Can run in parallel with: PRT-002, PRT-003, PRT-005
- Blocks: Human approval and safe operator recovery
- Production risk addressed: Operators have no functional management plane.

### Problem

API has only unconditional health handlers and UI calls an unavailable endpoint. There is no REST/gRPC gateway, cookie/CSRF boundary, resource API, SSE, or real UI flow.

### Evidence

- `api/cmd/api/main.go:9-22`
- `web/src/main.tsx:7-15`
- `web/Dockerfile:6-8`
- `orchestrator/src/moirai/grpc/control_plane.py:54-105`

### Required implementation

Build an API-only gRPC client with deadlines/cancellation and no database access. Add secure HTTP middleware, login/logout, sessions/cookies/CSRF/origin/rate limits, safe errors, request IDs, validated REST resources, pagination, authorized SSE cursors/backpressure, and phase-valid actions. Configure web-to-API routing and implement minimum login/project/runner/queue/workflow/log/approval/recovery screens.

### Acceptance criteria

- [ ] Browser API requests route through Compose and return typed usable errors.
- [ ] Mutations require authentication, authorization, CSRF, validation, and audit identity.
- [ ] Lists paginate/filter without secrets; SSE handles cursor, reconnect, slow clients, and cleanup.
- [ ] UI reflects actual dependency/workflow state and exposes only valid actions.
- [ ] API contains no PostgreSQL credentials/direct database path.

### Required tests

- [ ] HTTP handler tests for auth, validation, errors, pagination, and authorization.
- [ ] SSE cursor/reconnect/slow-client tests.
- [ ] Component tests for login/forms/action-state boundaries.
- [ ] Compose browser/API smoke test.

### Validation commands

```bash
cd api && go vet ./... && go test ./...
cd web && npm ci && npm run lint && npm run build
# Run new Compose browser/API integration suite.
```

### Operational considerations

Keep internal gRPC private; establish proxy/TLS/cookie contract before public port exposure. Rate-limit login and streaming.

### Definition of done

An authenticated operator can configure a project, inspect runner/workflow state, and execute permitted recovery/approval actions through a clean Compose deployment.

## PRT-007 — Secure runner transport and credential lifecycle

- Priority: P0 / Now
- Severity source: High
- Category: Transport security
- Related findings: PRR-007, PRR-004
- Components: Runner config/dialer, orchestrator gRPC, Compose, enrollment
- Estimated size: M
- Dependencies: None
- Can run in parallel with: PRT-002, PRT-005, PRT-006
- Blocks: PRT-003 remote/enrolled runner operation
- Production risk addressed: Bearer control credentials currently travel over insecure transport.

### Problem

Orchestrator binds insecure gRPC and runner defaults to insecure credentials. TLS has no configured trust material/server name/client authentication lifecycle.

### Evidence

- `orchestrator/src/moirai/main.py:42-45`
- `runner/internal/config/config.go:67-69`
- `runner/internal/control/dialer.go:14-28`
- `compose.yaml:17-45`

### Required implementation

Require TLS/mTLS for remote runners; define CA, server-name, certificate/key, rotation/revocation, and expiration configuration. Restrict plaintext mode to explicit documented single-host development with visible warning and no remote endpoint support. Add runner credential expiry/rotation/revocation administration.

### Acceptance criteria

- [ ] Remote endpoint cannot start/connect in insecure mode.
- [ ] Trusted runner connects; untrusted/expired/revoked credential fails closed.
- [ ] Certificate/credential rotation is atomic and recoverable.
- [ ] Insecure development mode is explicit, observable, and documented.

### Required tests

- [ ] TLS/mTLS valid/missing/untrusted/expired/rotated tests.
- [ ] Credential expiry, revocation, and reconnect tests.
- [ ] Regression test preventing credential output in logs/errors.

### Validation commands

```bash
cd runner && go test -race ./internal/config ./internal/control
cd orchestrator && PYTHONPATH=src python3 -m unittest discover -s tests -v
```

### Operational considerations

Protect CA/private key distribution; define secure bootstrap for remote runner trust. Do not confuse Docker internal networking with a remote transport boundary.

### Definition of done

A remote runner uses mutually authenticated encrypted control traffic and rotation/revocation has integration evidence.

## Observability and logging

## PRT-008 — Add correlated telemetry, audit coverage, and truthful health

- Priority: P0 / Now
- Severity source: High
- Category: Observability and auditability
- Related findings: PRR-008, PRR-014
- Components: All services, schema, Compose
- Estimated size: L
- Dependencies: PRT-002
- Can run in parallel with: PRT-003 through PRT-007
- Blocks: Safe unattended operation
- Production risk addressed: Operators cannot diagnose failures or investigate security-sensitive changes.

### Problem

Only minimal startup logging and authentication audit are evidenced. No common correlations, metrics, real readiness, bounded/redacted event logs, or operator query path exists.

### Evidence

- `orchestrator/src/moirai/main.py:51-57`
- `api/cmd/api/main.go:18-20`
- `orchestrator/src/moirai/persistence/authentication.py:154-162`
- `orchestrator/migrations/001_initial.sql:254-263`

### Required implementation

Define shared structured fields for request/project/issue/workflow/job/execution/runner/generation/outcome. Implement redaction and size/retention controls before persistence/streaming. Provide liveness/readiness based on actual dependencies/migration state and metrics for queue, runners, leases, workflows, execution/provider outcomes, connection pools, and log drops. Append/query-authorize audit events for all sensitive actions.

### Acceptance criteria

- [ ] An operator can identify why work is stalled from health, metrics, logs, and authorized UI/API state.
- [ ] Readiness changes correctly for database/migration/provider dependencies.
- [ ] Audit captures who/did what/to which resource/when/result without secrets.
- [ ] Secret fixtures are redacted from logs, artifacts, errors, and streams.
- [ ] Event/log storage and streaming have explicit bounds/retention.

### Required tests

- [ ] Correlation/redaction/retention unit tests.
- [ ] Audit append/query authorization test.
- [ ] Dependency-outage readiness recovery integration test.
- [ ] Metrics scrape/assertion test.

### Validation commands

```bash
# Run new service health/log/metric/audit tests.
docker compose up --build --wait
```

### Operational considerations

Avoid high-cardinality metric labels; restrict raw agent logs; choose retention for homelab disk capacity.

### Definition of done

A documented runner-loss/DB-outage incident can be diagnosed using captured correlated telemetry and audit evidence.

## Testing

## PRT-009 — Establish CI and real integration release gates

- Priority: P0 / Now
- Severity source: High
- Category: Testing and delivery assurance
- Related findings: PRR-009
- Components: CI, Makefile/scripts, all services
- Estimated size: M
- Dependencies: PRT-002; PRT-006 for browser gate
- Can run in parallel with: PRT-001, PRT-003 through PRT-008
- Blocks: Defensible release promotion
- Production risk addressed: Boundary failures can ship without detection.

### Problem

No CI configuration exists; configured validation omits Go/API/web coverage and live integrations. Current gRPC tests skip and web build lacks dependencies.

### Evidence

- `Makefile:3-26`
- `web/package.json:1-14`
- No `.github` workflow files found
- Validation results in `PRODUCTION_READINESS_REVIEW.md`

### Required implementation

Create pinned reproducible CI/local commands covering protocol freshness, Python lint/types/tests, Go vet/race/tests, web install/lint/build/tests, configured dependency/secret scans, PostgreSQL migration/integration, non-skipped gRPC, Compose smoke, and browser/API smoke. Publish artifacts/logs and make relevant checks required.

### Acceptance criteria

- [ ] Clean checkout validates every supported service.
- [ ] Database/gRPC tests execute rather than skip.
- [ ] Compose smoke proves migration/readiness.
- [ ] Stale generated protocol output fails a gate.
- [ ] Reports distinguish unit, integration, and browser/e2e tests.

### Required tests

- [ ] CI/local-equivalent self-test.
- [ ] Stale generated output fixture.
- [ ] PostgreSQL and live gRPC job.
- [ ] Authenticated API/UI smoke job after PRT-006.

### Validation commands

```bash
make validate
cd runner && go vet ./... && go test -race ./...
cd api && go vet ./... && go test ./...
cd web && npm ci && npm run lint && npm run build
```

### Operational considerations

Pin versions/images and never provide production secrets to CI. Preserve failure artifacts without logs containing credentials.

### Definition of done

A clean CI run performs all declared validations and intentionally failing protocol/integration fixtures fail their respective gates.

## Data integrity and concurrency

## PRT-010 — Enforce scheduler and lease invariants in PostgreSQL

- Priority: P1 / Next
- Severity source: Medium
- Category: Data integrity and concurrency
- Related findings: PRR-010, PRR-011
- Components: Schema, persistence, scheduler
- Estimated size: M
- Dependencies: PRT-002, PRT-005
- Can run in parallel with: PRT-008, PRT-011
- Blocks: Safe reassignment and durable multi-runner recovery
- Production risk addressed: Application bugs/races can create inconsistent ownership.

### Problem

Active-offer uniqueness, cross-entity ownership, server-capped lease expiry, and recovery fence progression are insufficiently database-enforced.

### Evidence

- `orchestrator/migrations/001_initial.sql:169-194`
- `orchestrator/src/moirai/persistence/control_plane.py:434-477`

### Required implementation

Add additive constraints/indexes/composite relations suitable for workflow/job/project consistency and unique active offers. Server-issue/cap lease expirations. Advance fencing atomically during recovery/reassignment. Verify queue/active-lock query plans.

### Acceptance criteria

- [ ] Database rejects invalid ownership and duplicate active offer states.
- [ ] Reassignment atomically increments fence and stale generation cannot update state.
- [ ] Client renewal cannot exceed policy TTL.
- [ ] Queue/lock queries have verified supporting indexes.

### Required tests

- [ ] Migration constraint tests.
- [ ] Concurrent PostgreSQL violation attempts.
- [ ] Query-plan/index inspection.
- [ ] Capped renewal/stale generation regression tests.

### Validation commands

```bash
# Run new PostgreSQL schema/concurrency integration suite.
cd orchestrator && PYTHONPATH=src python3 -m unittest discover -s tests -v
```

### Operational considerations

Preflight existing data before constraints; document repair procedure for inconsistent historic rows.

### Definition of done

Each documented ownership/fencing invariant has database-backed enforcement and concurrency evidence.

## Workflow durability and recovery

## PRT-011 — Harden GitHub synchronization and external side-effect reconciliation

- Priority: P1 / Next
- Severity source: Medium
- Category: GitHub integration
- Related findings: PRR-001, PRR-013
- Components: Issue tracker, code host, workflow nodes
- Estimated size: M
- Dependencies: PRT-001, PRT-005
- Can run in parallel with: PRT-010, PRT-012
- Blocks: Reliable multi-project issue delivery
- Production risk addressed: Queues omit issues and recovery breaks on completed provider side effects.

### Problem

Issue listing lacks explicit pagination; issue closure is not reconciled as an already-completed action; provider retry/error behavior is not operationally classified.

### Evidence

- `orchestrator/src/moirai/issue_trackers/github_cli.py:57-75`
- `orchestrator/src/moirai/code_hosts/github_cli.py`
- PRR-013

### Required implementation

Implement bounded pagination/sync, provider error classification/backoff/circuit policy, and idempotent reconciliation for labels, comments, branches, PRs, checks, merges, and closure through workflow-owned identifiers. Preserve CLI argument-vector and redaction safeguards.

### Acceptance criteria

- [ ] All eligible multi-page issues are synchronized exactly once per reconciliation policy.
- [ ] Repeated provider actions reconcile instead of creating duplicate side effects.
- [ ] Rate-limit/auth/outage outcomes enter bounded retry/blocked states with telemetry.
- [ ] Required-check and mergeability interpretation is documented/tested.

### Required tests

- [ ] Multi-page fixture test.
- [ ] Already closed/merged/existing PR replay tests.
- [ ] Rate-limit/outage/auth classification tests.
- [ ] Fake-provider workflow replay integration.

### Validation commands

```bash
cd orchestrator && PYTHONPATH=src python3 -m unittest tests.test_github_cli tests.test_github_code_host -v
# Run new fake-provider workflow integrations.
```

### Operational considerations

Use least-privilege GitHub credentials and command timeouts. Unit CI must use fakes, not live write access.

### Definition of done

Fake-provider evidence demonstrates a multi-page issue completing idempotently through replay after every provider side effect.

## Deployment and operations

## PRT-012 — Harden workspace filesystem safety, Compose operation, and homelab runbooks

- Priority: P1 / Later
- Severity source: Medium
- Category: Runner reliability, deployment, documentation
- Related findings: PRR-012, PRR-014
- Components: Repository manager, OpenCode artifacts, Compose, Dockerfiles, README
- Estimated size: L
- Dependencies: PRT-003, PRT-008
- Can run in parallel with: PRT-010, PRT-011
- Blocks: Controlled homelab release and safe untrusted repository processing
- Production risk addressed: Filesystem escape/stale workspace risk plus unsupported operational recovery.

### Problem

Path containment is lexical, existing-path has no configured allowed root, a late task-directory failure leaks a worktree, and Compose lacks restart/resource/log/security controls or tested backup/upgrade procedures.

### Evidence

- `runner/internal/repository/manager.go:71-77,82-97,238-242`
- `runner/internal/agents/opencode.go`
- `compose.yaml:1-60`
- `README.md:5-19`

### Required implementation

Implement symlink-safe owned artifact directories, configured existing-path root policy, compensating worktree cleanup, retention/quarantine. Harden Compose proportionately with health/restart/resource/log/least-privilege settings and publish tested first-run, enrollment, backup/restore, upgrade/rollback, cleanup, TLS, and troubleshooting runbooks.

### Acceptance criteria

- [ ] Artifact creation cannot escape workspace through symlink/traversal races.
- [ ] Every preparation failure cleans or records a recoverable cleanup item.
- [ ] Existing path must resolve under configured allowed roots.
- [ ] Workspace/log retention is bounded/configurable/observable.
- [ ] Documented clean-host deployment, restart, backup/restore, and upgrade/rollback drills work.

### Required tests

- [ ] Symlink escape and allowed-root tests.
- [ ] Failure injection at each worktree preparation stage.
- [ ] Retention/cleanup tests for managed/existing modes.
- [ ] Compose restart/resource/log and documentation walkthrough drills.

### Validation commands

```bash
cd runner && go vet ./... && go test -race ./internal/agents ./internal/repository ./internal/dispatch
docker compose config --quiet
docker compose up --build --wait
# Execute documented backup/restore and upgrade/rollback drills.
```

### Operational considerations

Do not claim enterprise orchestration; clearly document single-host, Docker, and remote-runner trust limits. Keep deletion path-identity safe.

### Definition of done

Security/failure-injection tests prove workspace safety and a new operator can perform documented deployment/recovery drills without undocumented steps.

## Performance

No task is prioritized solely for performance before correctness/recovery. PRT-008 must make queue/execution/log measurements visible; PRT-010 must inspect query plans. At 10 projects/10 runners, use those measurements before adding scale machinery.

## Scalability

After PRT-008 and PRT-010, measure scheduler latency, connection-pool use, event/log growth, SSE clients, GitHub sync cadence, and workspace retention. Do not introduce distributed infrastructure without observed homelab limits.

## Auditability

PRT-006 and PRT-008 own access-controlled audit query/write paths. Authentication, configuration, runner administration, workflow control, approval, merge, and recovery must retain actor, target, timestamp, outcome, and no secrets.

## Documentation

PRT-012 owns operator runbooks; each task must update its security/configuration contract. Published commands must be run from a clean environment before documentation completion.

## Developer experience

PRT-009 owns one local validation entry point equivalent to CI. PRT-006 must provide actionable operator-facing error states; avoid unrelated tooling before release blockers close.

## Future considerations

- Add rate-limit/circuit dashboards after PRT-008 yields real provider metrics.
- Reassess a stronger sandbox/remote runner trust domain after PRT-004 validation.
- Partition/archive logs/events only after retained-volume measurements show need.

## Parallel work suggestions

- Team A: PRT-002 migrations/readiness.
- Team B: PRT-007 transport and PRT-003 enrollment/image.
- Team C: PRT-004 runner sandbox and filesystem portion of PRT-012.
- Team D: PRT-005 recovery/idempotency and PRT-010 constraints.
- Team E: PRT-006 API/UI after stable control contracts.
- Team F: PRT-008 telemetry and PRT-009 CI harnesses.

## Recommended implementation waves

### Wave 1 — Release blockers

PRT-002, PRT-003, PRT-004, PRT-005, PRT-007, baseline PRT-008 and PRT-009. Do not grant write-capable credentials until this wave is verified.

### Wave 2 — Reliable delivery

PRT-001, PRT-006, PRT-010, and PRT-011. Demonstrate end-to-end fake-provider recovery before GitHub write use.

### Wave 3 — Controlled homelab operation

Complete PRT-008, PRT-009, and PRT-012. Execute runbooks and re-review with live integrations.

### Wave 4 — Performance and scale

Tune only from measured homelab data; prioritize retention/index/scheduler cadence before new architecture.

### Wave 5 — Documentation and developer experience

Finalize tested operator/developer procedures alongside Waves 1–3; documentation is not complete until each command is executed.

## Completion guidance

Do not mark a task complete from in-memory unit tests alone when it owns durable state or external side effects. Completion requires its acceptance criteria, required tests, validation evidence, operational documentation, and recorded migration/rollback impact in `PROGRESS.md`. Re-run this review after Waves 1–3.
