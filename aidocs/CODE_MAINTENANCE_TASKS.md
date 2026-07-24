# Code Maintenance Backlog

## Executive summary

Ten behavior-preserving tasks address the confirmed maintainability risks in `CODE_MAINTAINABILITY_REVIEW.md`. Start with workflow characterization and recovery ownership because they protect project locks, leases, and durable state. Keep protocol evolution additive and defer it until the workflow’s typed execution boundary is understood.

## Highest-value refactors

1. CMT-001 — Make the LangGraph workflow executable and runtime-owned.
2. CMT-002 — Add one recovery/re-offer transition owner.
3. CMT-003 — Move phase-specific packet creation and event transition decisions out of persistence.
4. CMT-005 — Bound Docker cancellation/stop behavior.
5. CMT-008 — Establish the web-to-API routing and typed client seam.

## Dependency map

`CMT-001 → CMT-002/CMT-003 → CMT-004`; `CMT-003 → CMT-010`; `CMT-008 → CMT-009`; `CMT-005`, `CMT-006`, and `CMT-007` may proceed independently. CMT-010 must precede new protocol-phase expansion but not the internal characterization work.

## Cross-language architecture

The orchestrator should own durable phase transitions, recovery, and task construction policy. Runner code should own bounded execution and preserve delivery ordering without holding locks through transport I/O. The web should have one configured public API boundary. Generated protocol types should carry typed, versioned semantic values rather than pushing arbitrary parsing into both services.

## Go API

No standalone refactoring task: the current API is intentionally minimal. Preserve the future direction `HTTP transport → typed orchestrator client → stable public model` when implementation begins.

## Go runner

## CMT-005 — Bound Docker cancellation and stop operations

- Priority: High
- Source severity: High
- Language: Go
- Component: `runner/internal/execution/DockerExecutor`
- Category: cancellation and process lifecycle
- Related findings: CMR-007
- Estimated size: S
- Dependencies: None
- Can be parallelized: Yes
- Behavior change allowed: No
- Status: Completed 2026-07-25

### Problem

Timeout and explicit cancellation call `docker stop` using `context.Background()`, and the timeout monitor waits for it. A stuck Docker CLI can therefore make an execution terminal path unbounded.

### Current evidence

`runner/internal/execution/docker.go:44-65,69-82,134-139`.

### Refactoring objective

Make every external stop operation return within an explicit, configurable runner policy deadline.

### Proposed boundaries

Keep Docker argument construction in `DockerExecutor`; extract a bounded stop-context helper/policy shared by timeout and explicit cancellation.

### Constraints

- Preserve public behavior.
- Preserve API and protocol compatibility unless separately approved.
- Preserve database behavior.
- Preserve workflow behavior.
- Avoid unrelated cleanup.

### Implementation steps

1. Add characterization tests where needed.
2. Extract or reorganize the smallest coherent responsibility.
3. Keep commits reviewable.
4. Run targeted tests.
5. Remove obsolete code only after replacement is validated.

### Acceptance criteria

- [x] Stop calls have an explicit finite deadline.
- [x] Timeout cleanup returns when the Docker CLI blocks; explicit cancellation shares that bounded helper.
- [x] Existing command vector and error context remain intact.

### Required tests

- [x] Characterization tests.
- [x] Unit tests for extracted logic.
- [x] Existing regression tests.
- [ ] Integration tests when boundaries change.

### Validation commands

```bash
cd runner && go test ./internal/execution && go vet ./...
```

### Risks

A deadline that is too short can leave containers running; retain the current five-second Docker graceful-stop argument and separately bound the client invocation.

### Definition of done

A blocking fake Docker stop cannot prevent cancellation completion past the configured deadline, and targeted tests pass.

## CMT-006 — Decouple event queue state from transport sends

- Priority: High
- Source severity: Medium
- Language: Go
- Component: `runner/internal/control/EventReporter`
- Category: concurrency and transport ownership
- Related findings: CMR-005
- Estimated size: M
- Dependencies: None
- Can be parallelized: Yes
- Behavior change allowed: No
- Status: Completed 2026-07-25

### Problem

`EventReporter` holds its mutex during `SendExecutionEvent`, blocking lease abandonment and all queue-state operations when transport blocks.

### Current evidence

`runner/internal/control/events.go:78-121`.

### Refactoring objective

Keep lease/sequence/FIFO queue mutation synchronized while performing transport I/O outside the queue lock.

### Proposed boundaries

Separate queue-state transition from a serialized sender/claimed-front delivery mechanism; keep retries explicit and bounded by existing pending capacity.

### Constraints

- Preserve public behavior.
- Preserve API and protocol compatibility unless separately approved.
- Preserve database behavior.
- Preserve workflow behavior.
- Avoid unrelated cleanup.

### Implementation steps

1. Add characterization tests where needed.
2. Extract or reorganize the smallest coherent responsibility.
3. Keep commits reviewable.
4. Run targeted tests.
5. Remove obsolete code only after replacement is validated.

### Acceptance criteria

- [x] Event sequence numbers remain strictly increasing per lease.
- [x] FIFO send and failed-front retention remain intact.
- [x] A blocked send does not prevent a concurrent abandonment.

### Required tests

- [x] Characterization tests.
- [x] Unit tests for extracted logic.
- [x] Existing regression tests.
- [ ] Integration tests when boundaries change.

### Validation commands

```bash
cd runner && go test -race ./internal/control ./internal/dispatch && go vet ./...
```

### Risks

Unlocking around a send can reorder concurrent flushes; ensure exactly one sender owns the front event.

### Definition of done

Race-enabled tests prove ordering, failed-send retention, and cancellation/abandonment responsiveness.

## CMT-007 — Make offer admission a reservation before transport acceptance

- Priority: Medium
- Source severity: Medium
- Language: Go
- Component: `runner/internal/control/OfferState`
- Category: concurrency and state transitions
- Related findings: CMR-006
- Estimated size: S
- Dependencies: None
- Can be parallelized: Yes
- Behavior change allowed: No
- Status: Completed 2026-07-25

### Problem

`OfferState.Admit` invokes acceptance/rejection transport calls while it owns its mutex.

### Current evidence

`runner/internal/control/offer.go:74-82`.

### Refactoring objective

Represent local pending admission as a short lock-protected reservation and perform remote calls outside the lock.

### Proposed boundaries

Validation → reservation → transport accept/reject → matching commit/rollback.

### Constraints

- Preserve public behavior.
- Preserve API and protocol compatibility unless separately approved.
- Preserve database behavior.
- Preserve workflow behavior.
- Avoid unrelated cleanup.

### Implementation steps

1. Add characterization tests where needed.
2. Extract or reorganize the smallest coherent responsibility.
3. Keep commits reviewable.
4. Run targeted tests.
5. Remove obsolete code only after replacement is validated.

### Acceptance criteria

- [x] Only one pending/active offer is admitted.
- [x] Failed acceptance removes its reservation.
- [x] Re-entrant control callbacks cannot deadlock state operations.

### Required tests

- [x] Characterization tests.
- [x] Unit tests for extracted logic.
- [x] Existing regression tests.
- [ ] Integration tests when boundaries change.

### Validation commands

```bash
cd runner && go test -race ./internal/control && go vet ./...
```

### Risks

An acknowledgement can arrive around acceptance completion; commit logic must identify the exact reservation/generation.

### Definition of done

Concurrent/re-entrant client tests pass with the existing one-job admission contract preserved.

## Python orchestrator

## CMT-001 — Characterize and runtime-own the LangGraph issue workflow

- Priority: Highest
- Source severity: High
- Language: Python
- Component: `workflows/issue_graph.py`, orchestrator startup
- Category: workflow ownership and testability
- Related findings: CMR-001
- Estimated size: L
- Dependencies: LangGraph runtime/checkpointer availability for integration verification
- Can be parallelized: No
- Behavior change allowed: No
- Status: In progress — placeholders removed and routes delegated to shared policy 2026-07-25; runtime/checkpoint wiring remains

### Problem

The graph has placeholder nodes, never sets `plan_valid`, and is not invoked by production startup.

### Current evidence

`orchestrator/src/moirai/workflows/issue_graph.py:24-69`; `orchestrator/src/moirai/main.py:30-87`.

### Refactoring objective

Create one testable workflow runtime boundary with injected async node handlers, pure routes, and checkpoint/invocation ownership.

### Proposed boundaries

Retain route functions as pure policy; add node-handler protocol/application service; keep LangGraph assembly/checkpointer adapter separate from startup wiring.

### Constraints

- Preserve public behavior.
- Preserve API and protocol compatibility unless separately approved.
- Preserve database behavior.
- Preserve workflow behavior.
- Avoid unrelated cleanup.

### Implementation steps

1. Add characterization tests where needed.
2. Extract or reorganize the smallest coherent responsibility.
3. Keep commits reviewable.
4. Run targeted tests.
5. Remove obsolete code only after replacement is validated.

### Acceptance criteria

- [x] Current router limits have characterization coverage.
- [x] A configured graph runs injected plan/implement/pipeline/review handlers.
- [ ] Resume/checkpoint ownership is explicit.
- [ ] Runtime invokes one workflow boundary rather than parallel state models.


### Required tests

- [x] Characterization tests.
- [x] Unit tests for extracted routing.
- [x] Existing regression tests.
- [ ] Integration tests when dependencies are available.

### Validation commands

```bash
cd orchestrator && PYTHONPATH=src python3 -m unittest tests.test_workflow_policy -v
cd orchestrator && PYTHONPATH=src python3 -m unittest discover -s tests -v
```

### Risks

Changing routing or checkpoints can alter replay behavior; preserve state keys and limits until policy changes are explicitly approved.

### Definition of done

Graph transition and restart/resume tests protect existing policy and startup has a single documented workflow invocation seam.

## CMT-002 — Add a transactional recovery-to-reoffer transition service

- Priority: Highest
- Source severity: High
- Language: Python/SQL
- Component: scheduler and persistence
- Category: durable state transition
- Related findings: CMR-002
- Estimated size: L
- Dependencies: CMT-001 characterization; runner session registry
- Can be parallelized: No
- Behavior change allowed: No
- Status: Implemented 2026-07-25; PostgreSQL integration validation remains

### Problem

Expired leases become `recovering` while their project lock blocks all scheduling; there is no owner to reassign recovery work safely.

### Current evidence

`orchestrator/src/moirai/persistence/control_plane.py:562-599,425-428`; `orchestrator/src/moirai/scheduler.py:66-78`.

### Refactoring objective

Centralize recovery eligibility, fencing, compatible-runner selection, replacement offer creation, and recovery audit event in one transaction/application command.

### Proposed boundaries

Keep expiry detection in persistence; introduce recovery transition service; add a repository command that retains the existing project lock while creating the replacement offer.

### Constraints

- Preserve public behavior.
- Preserve API and protocol compatibility unless separately approved.
- Preserve database behavior.
- Preserve workflow behavior.
- Avoid unrelated cleanup.

### Implementation steps

1. Add characterization tests where needed.
2. Extract or reorganize the smallest coherent responsibility.
3. Keep commits reviewable.
4. Run targeted tests.
5. Remove obsolete code only after replacement is validated.

### Acceptance criteria

- [x] Old lease generations remain fenced after expiry.
- [x] A compatible replacement runner can resume the same locked workflow.
- [x] No second workflow/lock is created for the project.
- [x] Recovery actions are idempotent and recorded as workflow events.

### Required tests

- [x] Characterization tests.
- [x] Unit tests for extracted logic.
- [x] Existing regression tests.
- [ ] PostgreSQL integration tests when dependencies are available.

### Validation commands

```bash
cd orchestrator && PYTHONPATH=src python3 -m unittest tests.test_asyncpg_control_plane tests.test_scheduler_service -v
```

### Risks

Incorrect lock replacement risks concurrent repository work; run PostgreSQL transaction tests before enabling production recovery.

### Definition of done

A lease-expiry-to-replacement-runner scenario passes without releasing project exclusivity or accepting stale events.

## CMT-003 — Separate phase packet construction from persistence and generic event append

- Priority: Highest
- Source severity: High
- Language: Python
- Component: `AsyncpgControlPlane`
- Category: separation of concerns and workflow model
- Related findings: CMR-003
- Estimated size: L
- Dependencies: CMT-001, CMT-002
- Can be parallelized: No
- Behavior change allowed: No
- Status: In progress — packet construction extracted 2026-07-25; result transition mapping remains

### Problem

Persistence constructs a fixed planner packet while event acceptance only records opaque generic events; neither owns a reusable phase transition model.

### Current evidence

`orchestrator/src/moirai/persistence/control_plane.py:360-399,707-737`.

### Refactoring objective

Use typed execution requests and a workflow transition service so persistence loads/stores records rather than deciding agent role, constraints, and next phase.

### Proposed boundaries

Repository data loader; pure task-packet factory; result validator/mapper; transition application service; transactional execution/event repository commands.

### Constraints

- Preserve public behavior.
- Preserve API and protocol compatibility unless separately approved.
- Preserve database behavior.
- Preserve workflow behavior.
- Avoid unrelated cleanup.

### Implementation steps

1. Add characterization tests where needed.
2. Extract or reorganize the smallest coherent responsibility.
3. Keep commits reviewable.
4. Run targeted tests.
5. Remove obsolete code only after replacement is validated.

### Acceptance criteria

- [x] Existing planner packet is reproduced exactly through the new factory.
- [x] New roles can be represented without persistence conditionals in task construction.
- [ ] Validated runner results update structured execution/workflow state.
- [ ] Fencing/event ordering remains transactional.

### Required tests

- [x] Characterization tests.
- [x] Unit tests for extracted logic.
- [x] Existing regression tests.
- [ ] Integration tests when boundaries change.

### Validation commands

```bash
cd orchestrator && PYTHONPATH=src python3 -m unittest tests.test_asyncpg_control_plane tests.test_runner_grpc -v
```

### Risks

Packet or transition changes can make current runners reject work; retain current JSON contract during the transition.

### Definition of done

Packet construction, result mapping, and phase transition tests demonstrate unchanged planner behavior and extensible phase ownership.

## CMT-004 — Define scheduler delivery failure aftermath

- Priority: High
- Source severity: Medium
- Language: Python
- Component: `Scheduler.tick`
- Category: error policy and idempotency
- Related findings: CMR-004
- Estimated size: S
- Dependencies: CMT-003 task-packet factory seam preferred
- Can be parallelized: Yes
- Behavior change allowed: No
- Status: Implemented 2026-07-25

### Problem

Exceptions from task construction or offer transport bypass the sole cleanup path and can leave an offer/project lock stranded.

### Current evidence

`orchestrator/src/moirai/scheduler.py:66-78`; `persistence/control_plane.py:640-682`.

### Refactoring objective

Make delivery success, retryable failure, terminal failure, and fatal scheduler error explicit and durable.

### Proposed boundaries

Packet factory and delivery adapter return structured outcomes; one durable command records/requeues/releases the offer according to policy.

### Constraints

- Preserve public behavior.
- Preserve API and protocol compatibility unless separately approved.
- Preserve database behavior.
- Preserve workflow behavior.
- Avoid unrelated cleanup.

### Implementation steps

1. Add characterization tests where needed.
2. Extract or reorganize the smallest coherent responsibility.
3. Keep commits reviewable.
4. Run targeted tests.
5. Remove obsolete code only after replacement is validated.

### Acceptance criteria

- [x] `False` delivery keeps existing cleanup behavior.
- [x] Packet and delivery exceptions produce deterministic durable aftermath.
- [x] Retried ticks cannot retain a failed offer or project lock after classified failure.

### Required tests

- [x] Characterization tests.
- [x] Unit tests for extracted logic.
- [x] Existing regression tests.
- [ ] Integration tests when boundaries change.

### Validation commands

```bash
cd orchestrator && PYTHONPATH=src python3 -m unittest tests.test_scheduler_service -v
```

### Risks

Over-catching hides fatal errors; preserve error visibility and catch only classified operational failures.

### Definition of done

Focused fake-clock tests cover every callback outcome and leave no stranded offer/lock.

## LangGraph workflows

CMT-001 through CMT-003 are the workflow refactoring wave. Do not independently rewrite node functions before their characterization tests and state-transition owner are in place.

## React and TypeScript Web UI

## CMT-008 — Establish a configured and testable web API boundary

- Priority: High
- Source severity: High
- Language: TypeScript/YAML
- Component: web client, nginx/Compose deployment
- Category: boundary ownership and testability
- Related findings: CMR-008
- Estimated size: M
- Dependencies: API health route contract decision
- Can be parallelized: Yes
- Behavior change allowed: No
- Status: In progress — proxy and client boundary implemented 2026-07-25; frontend dependencies/tests unavailable

### Problem

The web fetches a same-origin API URL but Compose/nginx does not provide that route; fetch/render/error behavior is one inline component with no tests.

### Current evidence

`web/src/main.tsx:7-12`; `web/Dockerfile:6-7`; `compose.yaml:27-38`.

### Refactoring objective

Choose one public API routing strategy and establish a small typed client plus health query/presentation seam.

### Proposed boundaries

Runtime configuration or nginx proxy; API client/error normalization; health query hook/container; presentational health status component.

### Constraints

- Preserve public behavior.
- Preserve API and protocol compatibility unless separately approved.
- Preserve database behavior.
- Preserve workflow behavior.
- Avoid unrelated cleanup.

### Implementation steps

1. Add characterization tests where needed.
2. Extract or reorganize the smallest coherent responsibility.
3. Keep commits reviewable.
4. Run targeted tests.
5. Remove obsolete code only after replacement is validated.

### Acceptance criteria

- [x] Health URL has one nginx-owned deployment path through the public API.
- [ ] Loading, non-OK, and network-error UI states are tested.
- [x] Components do not directly own URL/error parsing.

### Required tests

- [ ] Characterization tests.
- [ ] Unit tests for extracted logic.
- [ ] Existing regression tests.
- [ ] Compose/browser integration test when dependencies are available.

### Validation commands

```bash
cd web && npm run lint && npm run build
# after Compose is available: docker compose config && browser/HTTP health smoke test
```

### Risks

A proxy can accidentally expose internal paths; proxy only the public versioned API and preserve control-network isolation.

### Definition of done

The built web image reaches the API health endpoint through the chosen configuration and tests cover visible states.

## CMT-009 — Make web dependencies and top-level validation reproducible

- Priority: Medium
- Source severity: Medium
- Language: TypeScript/Build
- Component: web and root Makefile
- Category: reproducibility and developer tooling
- Related findings: CMR-009
- Estimated size: S
- Dependencies: CMT-008 may define test scripts
- Can be parallelized: Yes
- Behavior change allowed: No

### Problem

Frontend builds resolve `latest` versions using `npm install`, and root validation omits frontend/API/runner service checks.

### Current evidence

`web/package.json:1`; `web/Dockerfile:3-5`; `Makefile:5-26`.

### Refactoring objective

Lock frontend dependency resolution and make each independently buildable service visible from the root validation workflow.

### Proposed boundaries

Service-local scripts remain authoritative; Make targets compose those scripts without duplicating commands.

### Constraints

- Preserve public behavior.
- Preserve API and protocol compatibility unless separately approved.
- Preserve database behavior.
- Preserve workflow behavior.
- Avoid unrelated cleanup.

### Implementation steps

1. Add characterization tests where needed.
2. Extract or reorganize the smallest coherent responsibility.
3. Keep commits reviewable.
4. Run targeted tests.
5. Remove obsolete code only after replacement is validated.

### Acceptance criteria

- [ ] A committed lockfile resolves the same dependency graph.
- [ ] Docker uses deterministic installation.
- [ ] Root validation invokes explicit web, API, runner, and orchestrator checks.

### Required tests

- [ ] Characterization tests.
- [ ] Unit tests for extracted logic.
- [ ] Existing regression tests.
- [ ] Integration tests when boundaries change.

### Validation commands

```bash
cd web && npm ci && npm run lint && npm run build
make validate
```

### Risks

Lockfile generation can update packages unintentionally; pin the current resolved versions and review the generated lockfile.

### Definition of done

Clean installs and container builds are repeatable, and root validation fails when a service-level check fails.

## PostgreSQL and persistence

CMT-002 and CMT-003 change persistence ownership; CMT-010 changes schema delivery. Keep scheduling’s existing single transaction cohesive.

## Protocol Buffers and gRPC

## CMT-010 — Plan additive typed contract and migration evolution

- Priority: Medium
- Source severity: Medium
- Language: Protocol Buffers/SQL
- Component: contracts, generated bindings, migrations
- Category: versioning and model clarity
- Related findings: CMR-010
- Estimated size: L
- Dependencies: CMT-003 phase model
- Can be parallelized: No
- Behavior change allowed: No

### Problem

Task/event content and lifecycle values are opaque JSON or unconstrained strings, while schema delivery is a mutable idempotent bootstrap file.

### Current evidence

`proto/runner_control.proto:31,37`; `proto/control_plane.proto:22-29,37,41-44,51`; `orchestrator/migrations/001_initial.sql:1-265`.

### Refactoring objective

Adopt an additive compatibility plan for typed payload/state fields and an incremental migration/versioning mechanism.

### Proposed boundaries

Domain-to-proto mapper; typed payload schema/version; compatibility adapter for existing JSON fields; migration version runner and immutable incremental files.

### Constraints

- Preserve public behavior.
- Preserve API and protocol compatibility unless separately approved.
- Preserve database behavior.
- Preserve workflow behavior.
- Avoid unrelated cleanup.

### Implementation steps

1. Add characterization tests where needed.
2. Extract or reorganize the smallest coherent responsibility.
3. Keep commits reviewable.
4. Run targeted tests.
5. Remove obsolete code only after replacement is validated.

### Acceptance criteria

- [ ] Existing field numbers and current runner interoperability are preserved.
- [ ] New lifecycle values are typed/documented and validated consistently.
- [ ] Fresh and upgraded databases record applied migration versions.
- [ ] JSON payload removal occurs only after compatibility validation.

### Required tests

- [ ] Characterization tests.
- [ ] Unit tests for extracted logic.
- [ ] Existing regression tests.
- [ ] Integration tests when boundaries change.

### Validation commands

```bash
make proto-check
cd gen/go && go test ./...
cd orchestrator && PYTHONPATH=src python3 -m unittest discover -s tests -v
# with PostgreSQL available: fresh-install and upgrade migration checks
```

### Risks

Changing protobuf fields or an applied migration in place breaks deployed services; use additive fields and new migration files only.

### Definition of done

Compatibility tests prove Go/Python old/new paths interoperate and migration tests prove fresh/upgrade/repeat behavior.

## Tests and test infrastructure

The characterization requirements are embedded in CMT-001 through CMT-010. Prioritize deterministic fake-clock/database tests for state transitions, blocked-transport tests for runner concurrency, and browser/Compose smoke tests only after routing exists.

## Configuration

CMT-008 centralizes web API endpoint ownership; CMT-009 makes build configuration reproducible. The current orchestrator configuration model was not found to need a separate refactor.

## Build and developer tooling

CMT-009 owns root validation breadth and deterministic frontend installation. Do not make unavailable optional tools mandatory until they are declared/provisioned in project tooling.

## Dead code and cleanup

CMT-001 replaces the dormant placeholder graph after tests validate its intended successor. No deletion task is recommended for generated bindings or security validation code.

## Recommended refactoring waves

### Wave 1 — Reduce critical complexity

CMT-001, CMT-002, CMT-005.

### Wave 2 — Clarify architectural boundaries

CMT-003, CMT-004, CMT-008.

### Wave 3 — Improve testability

CMT-006, CMT-007, CMT-009.

### Wave 4 — Remove duplication and dead code

Retire placeholder graph code only as part of CMT-001 after compatibility coverage; consolidate runner cancellation policy as part of CMT-005.

### Wave 5 — Consistency and developer experience

CMT-010 and the root validation expansion in CMT-009.
