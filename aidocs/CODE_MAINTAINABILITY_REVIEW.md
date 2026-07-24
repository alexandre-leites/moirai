# Code Maintainability Review

## Review metadata

- Date: 2026-07-25
- Repository revision: `0ebd54e` (working tree already contained implementation changes before this review)
- Review scope: Go API and runner, Python orchestrator and LangGraph workflow, React web UI, PostgreSQL schema, Protocol Buffers, Compose, and build tooling
- Languages: Go, Python, TypeScript/React, SQL, Protocol Buffers, YAML
- Tools executed: source/test/config inspection; Git working-tree and whitespace check; focused static review
- Limitations: Python dependencies, Docker/Buf, `ruff`, `mypy`, and `rg` are unavailable in this environment. No live PostgreSQL, gRPC, Compose, or browser tests were run. Existing unrelated working-tree changes were not modified.

## Executive summary

**Significant maintainability risk.** The runner has useful small boundaries and focused tests, while the durable scheduler transaction is cohesive. The highest-cost risks are unfinished workflow/runtime wiring, state transitions split across placeholders and persistence, and lifecycle code that can block on external I/O. The web/API are still intentionally minimal, but their current deployment path is inconsistent and should be made testable before they expand.

## Maintainability assessment

- Significant maintainability risk

## Highest-risk maintainability hotspots

1. Placeholder LangGraph graph is neither runtime-owned nor able to advance a normal plan.
2. Lease expiry records recovery but no component can re-offer recovery work while retaining its lock.
3. Task construction hard-codes a planner execution, duplicating/fragmenting workflow transition responsibility.
4. Runner network and process cancellation paths can block while holding state or using unbounded contexts.
5. Protocol contracts use untyped JSON/string fields where stable state vocabulary is needed.

## Codebase and dependency map

| Area | Primary entry point | Current dependency direction |
|---|---|---|
| Go API | `api/cmd/api/main.go` | process/config → HTTP; currently no orchestrator client |
| Go runner | `runner/cmd/runner/main.go` | config → control/dispatch → repository, agents, execution → generated runner contract |
| Python orchestrator | `orchestrator/src/moirai/main.py` | config → gRPC/scheduler → persistence/domain; provider adapters live separately |
| Workflow | `orchestrator/src/moirai/workflows/issue_graph.py` | graph/router currently isolated from runtime and persistence |
| Web | `web/src/main.tsx` | React root → direct browser fetch |
| Contracts | `proto/*.proto`, `schemas/task-packet.schema.json` | generated Go/Python clients consumed by services |
| Persistence | `orchestrator/migrations/001_initial.sql` | schema plus `AsyncpgControlPlane` SQL application boundary |

Intended architecture in `PROJECT.md` is broadly reflected by the service split and the orchestrator-only PostgreSQL access. The strongest boundary mismatch is the isolated workflow graph: runtime scheduling and runner events do not have one application service that owns phase transition, task construction, and checkpoint persistence.

## Language-specific assessment

### Go API

The API is deliberately a 22-line health server (`api/cmd/api/main.go:9-21`), so there is no large-method hotspot. It does not yet establish the intended API/orchestrator boundary; this is feature-incomplete rather than a refactoring candidate. Keep future HTTP handlers outside `main` before routes are added.

### Go runner

The runner separates task-packet validation, repository management, agent execution, offer state, and control dispatch. Its hotspots are lifecycle ownership: stream reconnect, lock-held event send, offer admission, and cancellation/cleanup time bounds.

### Python orchestrator

The asyncpg scheduling transaction is cohesive and correctly keeps candidate selection, lock creation, job creation, and offer creation together. Workflow phase ownership is fragmented: the graph is a placeholder, the task builder fixes one phase, and runner events append generic records.

### LangGraph

`build_issue_graph` is a small but incomplete graph that cannot currently run a valid plan path. It lacks injected nodes/checkpoint ownership and is not called by `main.py`.

### React and TypeScript

The UI is intentionally a single health screen. Its direct fetch, non-reproducible dependencies, and missing proxy make it a poor base for adding authenticated features. Avoid prematurely creating generic UI layers; first establish one typed client and a testable health feature.

### Persistence and SQL

Constraints/indexes encode several key invariants, including repository-mode validity and one project lock per project. The only `001_initial.sql` file is an idempotent bootstrap script, so it cannot express or verify an incremental production schema history.

### Protocol Buffers and gRPC

Services remain small. Domain state is represented by ambiguous strings and JSON envelopes (`task_packet_json`, `payload_json`), creating repeated parsing and runtime validation at multiple boundaries. Make an additive, versioned contract change rather than replacing fields in place.

## Complexity hotspots

- `AsyncpgControlPlane.schedule` (`orchestrator/src/moirai/persistence/control_plane.py:401-508`) is long because it must own one transaction; its responsibilities are coherent and should not be split into generic repository calls.
- `ControlLoop.Handle`/`execute` (`runner/internal/dispatch/control_loop.go:86-230`) coordinate offers, dispatch, cancellation, and terminal reporting. It is manageable today but depends on synchronous send/cancellation helpers, which create the lifecycle risks in CMR-006 and CMR-007.
- `Scheduler.tick` (`orchestrator/src/moirai/scheduler.py:66-78`) is short but combines durable scheduling, packet generation, and network delivery without an exception policy.

## Coupling and boundary issues

- The workflow graph is not connected to the runtime (`CMR-001`).
- Task-packet generation embeds phase policy in persistence (`CMR-003`).
- Recovery state is written by persistence but has no recovery application service (`CMR-002`).
- Browser route expectations and nginx deployment are not owned by one web/API boundary (`CMR-008`).

## Duplication

No confirmed duplication is severe enough to justify consolidation before workflow wiring. The runner’s repeated background cancellation (`runner/internal/dispatch/control_loop.go:163,246`) and Docker stop logic (`runner/internal/execution/docker.go:52,74`) should share a bounded cancellation policy when CMR-007 is addressed.

## Error-handling consistency

- `Scheduler.tick` handles a `False` delivery result but does not define cleanup for exceptions from packet construction or transport (`CMR-004`).
- Docker cancellation uses an unbounded background context (`CMR-007`).
- The runner otherwise wraps low-level errors with useful operational context, for example `DockerExecutor.stop` (`runner/internal/execution/docker.go:134-139`).

## Testability

The runner has focused unit tests around task packets, offer state, event buffering, dispatch, executors, and repository handling. Riskier changes need characterization tests first: graph routing/runtime, recovery re-offer, asynchronous delivery failures, blocked event send, and blocking Docker stop. The web has no component tests, so CMR-008 requires basic behavior characterization before UI restructuring.

## Configuration maintainability

The orchestrator uses a configuration model and Compose isolates the database network. Web dependencies/configuration are not reproducible: package versions are `latest`, no lockfile is committed, and the health fetch has no configured API base/proxy (`CMR-008`, `CMR-009`).

## Dead code and stale abstractions

- `build_issue_graph` is a confirmed dormant implementation: it is not referenced by `main.py`, and its placeholder handlers cannot complete the ordinary route (`CMR-001`).
- `ControlPlaneService.ListWorkflows` gracefully exposes `UNIMPLEMENTED` when a control plane lacks the method (`orchestrator/src/moirai/grpc/control_plane.py:191-227`); `AsyncpgControlPlane` has no implementation. This is a confirmed unfinished boundary, but it is feature backlog work rather than a standalone maintainability refactor.
- No confirmed unused interfaces or commented-out code were found in the reviewed areas.

## Positive findings

- The scheduler candidate transaction uses row locks, `SKIP LOCKED`, and creates workflow, project lock, job, and offer atomically (`orchestrator/src/moirai/persistence/control_plane.py:408-508`).
- Runner task-packet validation rejects unknown fields and unsafe paths before execution (`runner/internal/taskpacket/taskpacket.go:68-118`).
- Runner session replacement enforces one identity per stream and closes superseded sessions (`orchestrator/src/moirai/grpc/sessions.py:54-67`, `grpc/runner_control.py:95-113`).
- Schema constraints meaningfully encode repository mode and single-lock invariants (`orchestrator/migrations/001_initial.sql:27-42,130-135`).

## Findings summary

| ID | Severity | Language | Component | Title | Status |
|---|---|---|---|---|---|
| CMR-001 | High | Python | LangGraph workflow | Placeholder graph is disconnected and cannot advance a valid plan | confirmed |
| CMR-002 | High | Python/SQL | Scheduler recovery | Lease recovery preserves a lock without a recover-and-reoffer owner | confirmed |
| CMR-003 | High | Python | Task orchestration | Persistence hard-codes planner packet and fragments phase ownership | confirmed |
| CMR-004 | Medium | Python | Scheduler | Packet/delivery exceptions strand durable offers and locks | confirmed |
| CMR-005 | Medium | Go | Runner control | Event reporter performs network I/O while holding its state mutex | confirmed |
| CMR-006 | Medium | Go | Runner offer state | Offer admission calls the transport while holding its mutex | confirmed |
| CMR-007 | High | Go | Docker execution | Cancellation can wait forever for `docker stop` | confirmed |
| CMR-008 | High | TypeScript/YAML | Web deployment | Health client route has no Compose/nginx routing owner and no tests | confirmed |
| CMR-009 | Medium | TypeScript/Build | Web tooling | Floating dependencies and `npm install` make builds non-reproducible | confirmed |
| CMR-010 | Medium | Proto/SQL | Contracts and migrations | Unstructured protocol fields and bootstrap migration obscure evolution | strongly indicated |

## Detailed findings

### CMR-001 — Placeholder graph is disconnected and cannot advance a valid plan

- Severity: High
- Language: Python
- Component: `workflows`
- Category: workflow ownership, dead/incomplete structure
- Status: confirmed
- Evidence:
  - `orchestrator/src/moirai/workflows/issue_graph.py:48-69` — `build_issue_graph`
  - `orchestrator/src/moirai/workflows/issue_graph.py:52-59` — placeholder lambdas
  - `orchestrator/src/moirai/workflows/issue_graph.py:24-29` — `route_plan` requires `plan_valid`, which no node sets
  - `orchestrator/src/moirai/main.py:30-87` — production startup never builds or invokes the graph
- Current structure: Graph construction owns simple routing and placeholder state updates, while the runtime owns gRPC/scheduler startup independently.
- Maintainability problem: A normal run increments planning attempts, loops once, then blocks; the apparent graph is not an executable source of workflow behavior.
- Why future changes are risky or expensive: Adding nodes or policies can create a second, divergent workflow model that is not exercised by production paths or checkpoint tests.
- Recommended refactoring direction: Make one workflow application service own graph construction, injected async node handlers, checkpoint configuration, and invocation/resume. Keep pure routing functions together.
- Suggested extraction boundaries: Pure route/policy functions; node handler protocol/application service; LangGraph assembly/checkpointer adapter; runtime startup wiring.
- Behavior that must remain unchanged: Existing route limits (`2` planning and `3` repair/review cycles) until a separately approved policy change; terminal blocked behavior.
- Tests required before refactoring: Characterize all current router outcomes; graph integration cases for plan→implement→pipeline→review, blocked limits, and checkpoint resume.
- Related findings: CMR-002, CMR-003, CMR-004

### CMR-002 — Lease recovery preserves a lock without a recover-and-reoffer owner

- Severity: High
- Language: Python/SQL
- Component: persistence and scheduler recovery
- Category: state ownership, incomplete recovery boundary
- Status: confirmed
- Evidence:
  - `orchestrator/src/moirai/persistence/control_plane.py:562-599` — `expire_leases` moves jobs/workflows to `recovering`, fences the job, and retains project locks
  - `orchestrator/src/moirai/persistence/control_plane.py:425-428` — `schedule` rejects every project with a lock
  - `orchestrator/src/moirai/scheduler.py:66-78` — tick expires offers only and schedules fresh candidates
- Current structure: Recovery persistence protects project exclusivity, but no application service turns a recovering workflow into a fenced replacement offer.
- Maintainability problem: Recovery semantics are split between a durable status update and a scheduler eligibility exclusion with no transition owner.
- Why future changes are risky or expensive: Developers may bypass the lock to make work schedulable, breaking the one-active-workflow invariant, or introduce recovery paths in several nodes.
- Recommended refactoring direction: Add one transactional recovery/re-offer application service that validates workflow state, selects a compatible runner, increments/uses fencing correctly, and records a recovery event.
- Suggested extraction boundaries: Lease expiry detection stays in persistence; recovery eligibility/transition service owns policy; repository command owns atomic recovery offer and lock continuity; scheduler invokes that command.
- Behavior that must remain unchanged: An expired lease invalidates old runner events and retains the project lock until recovery reaches a terminal state.
- Tests required before refactoring: Characterize expiry updates and scheduler exclusion; integration-style fake-DB tests for expired runner → replacement runner → completion, stale event rejection, and no concurrent project offer.
- Related findings: CMR-001, CMR-003

### CMR-003 — Persistence hard-codes a planner packet and fragments phase ownership

- Severity: High
- Language: Python
- Component: `AsyncpgControlPlane`
- Category: separation of concerns, difficult extension
- Status: confirmed
- Evidence:
  - `orchestrator/src/moirai/persistence/control_plane.py:360-399` — `build_task_packet`
  - `orchestrator/src/moirai/persistence/control_plane.py:388-398` — fixed `-plan` execution ID, `planner` role, fixed timeout and no-write constraints
  - `orchestrator/src/moirai/persistence/control_plane.py:707-737` — `accept_event` only appends generic events
- Current structure: SQL row loading, branch construction, task-packet serialization, phase selection, and execution defaults share one persistence class.
- Maintainability problem: Planner-specific behavior has no extensible phase model, while execution events do not map to structured execution/workflow state.
- Why future changes are risky or expensive: Every phase adds conditionals to persistence and risks divergent packet fields, event mappings, and workflow counters.
- Recommended refactoring direction: Introduce a typed workflow execution request/task-packet factory consumed by persistence data loaders; one transition service maps validated runner results to the next phase.
- Suggested extraction boundaries: Repository query/data record; pure packet factory with role/constraints; workflow transition service; persistence commands for executions/events.
- Behavior that must remain unchanged: Current planner packet field values and repository source validation; scheduling transaction and runner fencing.
- Tests required before refactoring: Characterize existing planner packet byte-level fields, runner acceptance, and event persistence; unit test packets per phase before wiring real nodes.
- Related findings: CMR-001, CMR-002, CMR-010

### CMR-004 — Packet and delivery exceptions strand durable offers and locks

- Severity: Medium
- Language: Python
- Component: scheduler
- Category: error handling, transaction aftermath
- Status: confirmed
- Evidence:
  - `orchestrator/src/moirai/scheduler.py:66-78` — `Scheduler.tick`
  - `orchestrator/src/moirai/scheduler.py:73-74` — packet construction and delivery exceptions bypass `reject_offer`
  - `orchestrator/src/moirai/persistence/control_plane.py:640-682` — `reject_offer` is the current atomic cancellation/lock-release path
- Current structure: A false delivery result cleans up; exceptions propagate after a durable offer was created.
- Maintainability problem: Callers must infer whether to retry, record failure, or release state; transient and deterministic failure behavior is undefined.
- Why future changes are risky or expensive: More delivery transports can each implement incompatible cleanup/retry behavior, leaving locks during failures.
- Recommended refactoring direction: Define a scheduler delivery result/error policy. On expected operational exception, atomically record reason and reject/requeue; only propagate truly fatal scheduler failures.
- Suggested extraction boundaries: Offer scheduling; packet factory; delivery adapter; durable delivery-failure/retry command.
- Behavior that must remain unchanged: Successful delivery leaves the offer active; a known `False` delivery releases the same job/project lock as today.
- Tests required before refactoring: Characterize false delivery; add packet-factory exception, transport exception, reject exception, and retry/idempotency cases.
- Related findings: CMR-002, CMR-003

### CMR-005 — Event reporter performs network I/O while holding its state mutex

- Severity: Medium
- Language: Go
- Component: runner event reporting
- Category: concurrency, testability
- Status: confirmed
- Evidence:
  - `runner/internal/control/events.go:78-102` — `Emit` holds `mu` through `flushLocked`
  - `runner/internal/control/events.go:105-121` — `Flush` holds `mu` through `SendExecutionEvent`
  - `runner/internal/control/events.go:114-120` — external client call inside `flushLocked`
- Current structure: One mutex protects lease, sequence, and pending queue, and also serializes synchronous transport sends.
- Maintainability problem: A blocked network call prevents abandonment, pending inspection, or any new event state update.
- Why future changes are risky or expensive: Disconnect/cancellation handling can deadlock operational progress and tests require timing-sensitive mocks.
- Recommended refactoring direction: Keep queue mutation under the mutex and serialize sends outside it, using either a dedicated sender or a carefully claimed-front event mechanism. Do not add an unbounded goroutine per event.
- Suggested extraction boundaries: Lease/sequence queue state; send loop/transport adapter; explicit retry policy.
- Behavior that must remain unchanged: Per-lease strictly increasing event sequences, FIFO delivery, payload redaction/size limit, and bounded pending capacity.
- Tests required before refactoring: Characterize sequence/FIFO semantics; blocked-send concurrent `Abandon`; failed send retains front event; reconnect flushes once.
- Related findings: CMR-007

### CMR-006 — Offer admission calls the transport while holding its mutex

- Severity: Medium
- Language: Go
- Component: runner offer state
- Category: concurrency, hidden callback coupling
- Status: confirmed
- Evidence:
  - `runner/internal/control/offer.go:74-82` — second admission check invokes `AcceptOffer` while `mu` is held
  - `runner/internal/control/offer.go:76-77` — busy rejection is also sent under the lock
- Current structure: State validation, packet parsing, transport acknowledgment, and pending state mutation are interleaved.
- Maintainability problem: Transport latency and re-entrant fake/client behavior become part of mutex ownership semantics.
- Why future changes are risky or expensive: A future client that reports state or blocks can deadlock offer admission, obscuring one-job admission guarantees.
- Recommended refactoring direction: Reserve a pending admission state under lock, call accept/reject outside the lock, then commit or roll back only if the reservation still matches.
- Suggested extraction boundaries: Pure offer/packet validation; reservation transition; transport call; success/failure finalization.
- Behavior that must remain unchanged: At most one active/pending offer, malformed packets rejected, and acceptance occurs before lease acknowledgement processing.
- Tests required before refactoring: Characterize busy and malformed rejection; add re-entrant client callback, blocked accept, failed accept rollback, and concurrent admit cases.
- Related findings: CMR-005

### CMR-007 — Docker cancellation can wait forever for `docker stop`

- Severity: High
- Language: Go
- Component: Docker executor
- Category: cancellation, external-process lifecycle
- Status: confirmed
- Evidence:
  - `runner/internal/execution/docker.go:44-65` — monitor waits for `stop` before returning
  - `runner/internal/execution/docker.go:52` — timeout monitor uses `context.Background()`
  - `runner/internal/execution/docker.go:69-82` — explicit `Cancel` also calls stop with `context.Background()`
- Current structure: Timeout/cancellation starts a background monitor whose stop command has no deadline, and execution waits for that monitor.
- Maintainability problem: A stalled Docker CLI or daemon can turn a bounded execution into an unbounded runner goroutine/result wait.
- Why future changes are risky or expensive: Shutdown/recovery tests and production incidents cannot reason about terminal timing; duplicated cancellation policy will diverge across executors.
- Recommended refactoring direction: Inject/configure a finite Docker stop timeout and derive a bounded stop context independent of the already-cancelled execution context. Consolidate executor cancellation timeouts in one runner execution policy.
- Suggested extraction boundaries: Docker command construction; bounded stop helper; supervisor execution; configured cancellation policy.
- Behavior that must remain unchanged: `docker stop --time 5` remains attempted on timeout/cancel, then client process cancellation is attempted; successful command result behavior stays the same.
- Tests required before refactoring: Characterize current command vector; blocking fake Docker stop returns by configured deadline; explicit cancellation and execution timeout both terminate; combined stop/client errors preserve context.
- Related findings: CMR-005

### CMR-008 — Health client route has no deployment owner and no behavior tests

- Severity: High
- Language: TypeScript/YAML
- Component: web deployment/client
- Category: boundary mismatch, testability
- Status: confirmed
- Evidence:
  - `web/src/main.tsx:7-12` — `App` directly fetches same-origin `/api/v1/health`
  - `web/Dockerfile:6-7` — stock nginx only copies static assets
  - `compose.yaml:27-38` — API is separate on port `8080`; web exposes port `3000`
- Current structure: One component owns fetch, state transition, and rendering; neither a runtime API base nor nginx proxy is configured.
- Maintainability problem: Browser behavior depends on undeclared routing and cannot be unit-tested independently from rendering.
- Why future changes are risky or expensive: Adding authenticated endpoints can spread URL/error/CSRF behavior through components and fail only after deployment.
- Recommended refactoring direction: Establish a small typed API client/runtime base configuration, then keep a health hook/container separate from presentational status. Choose and document either same-origin nginx proxy or explicit public API base.
- Suggested extraction boundaries: Runtime API configuration; API client/error normalization; health query hook; health/status component.
- Behavior that must remain unchanged: Existing three visible status outcomes (`checking`, `healthy`/`unhealthy`, `unavailable`) and static shell text.
- Tests required before refactoring: Characterize loading, non-OK response, network failure, and configured endpoint behavior; add a Compose/browser smoke test after routing choice.
- Related findings: CMR-009

### CMR-009 — Floating web dependencies and `npm install` make builds non-reproducible

- Severity: Medium
- Language: TypeScript/Build
- Component: web tooling
- Category: configuration, developer experience
- Status: confirmed
- Evidence:
  - `web/package.json:1` — all dependencies use `latest`
  - `web/Dockerfile:3-5` — Docker build copies no lockfile and runs `npm install`
  - `Makefile:5-26` — validation omits web build/lint and Go service tests
- Current structure: The effective web compiler/framework version changes over time and the top-level validation does not exercise the image’s frontend build.
- Maintainability problem: Reproducing an old build or isolating a web regression depends on the current package registry state.
- Why future changes are risky or expensive: A dependency release can break production builds without a source change; contributors use inconsistent validation commands.
- Recommended refactoring direction: Pin semver ranges suitable for the project, commit a lockfile, use `npm ci`, and add explicit web/API/runner targets to the root validation orchestration.
- Suggested extraction boundaries: Keep package manager choice/build scripts local to web; Make targets coordinate but do not duplicate service commands.
- Behavior that must remain unchanged: The resulting static bundle and command names, except the deterministic install command.
- Tests required before refactoring: Clean install/build from lockfile; TypeScript check; Docker build; root target invokes each service target.
- Related findings: CMR-008

### CMR-010 — Unstructured protocol fields and bootstrap migration obscure evolution

- Severity: Medium
- Language: Proto/SQL
- Component: shared contracts and schema delivery
- Category: versioning, abstraction quality
- Status: strongly indicated
- Evidence:
  - `proto/runner_control.proto:31,37` — `task_packet_json` and `payload_json`
  - `proto/control_plane.proto:22-29,37,41-44,51` — lifecycle/time/state values modeled as unconstrained strings
  - `orchestrator/migrations/001_initial.sql:1-265` — one idempotent initial schema script using `IF NOT EXISTS`
- Current structure: Generated transport types wrap opaque JSON and strings that consumers parse/validate independently; schema changes would need edits to an already-applied bootstrap script.
- Maintainability problem: Protocol semantic validation and payload mapping leak into handlers/runner code, and deployment cannot determine whether every incremental schema change has been applied.
- Why future changes are risky or expensive: New fields/states can be misspelled or parsed differently across Go/Python; production schema drift is hard to diagnose.
- Recommended refactoring direction: Before adding multiple workflow phases, introduce additive typed envelopes/enums or a documented `google.protobuf.Struct` boundary with payload versioning, plus timestamp types where semantics matter. Adopt a migration-version mechanism and new incremental migrations; do not rewrite applied schema history.
- Suggested extraction boundaries: Domain state-to-proto mappers; typed task/event payload schema; generated contract tests; migration runner/version table.
- Behavior that must remain unchanged: Existing field numbers, wire compatibility, JSON task-packet compatibility during a migration window, and schema constraints/data.
- Tests required before refactoring: Go/Python wire-compatibility and malformed-payload tests; old/new packet adapter tests; fresh-install, upgrade, and repeated-apply migration checks.
- Related findings: CMR-003

## Recommended refactoring order

1. CMT-001 characterization and graph/runtime ownership.
2. CMT-002 recovery/re-offer state transition, followed by CMT-003 phase packet/transition boundary.
3. CMT-004 scheduler delivery exception policy.
4. CMT-005 bounded Docker cancellation and CMT-006/CMT-007 runner concurrency seams.
5. CMT-008 web routing/client seam and CMT-009 reproducible frontend validation.
6. CMT-010 additive contract and migration evolution plan before protocol expansion.

## Risks of refactoring

The workflow/recovery work affects durable locks and fencing: characterization tests and transactional test doubles must precede structural changes. Protocol/migration changes require a compatibility window. Runner mutex refactors must preserve FIFO event ordering and one-job admission. Web routing changes must avoid exposing the internal control network.

## Areas intentionally left unchanged

- Cohesive scheduling SQL transaction was not recommended for generic repository splitting.
- Task-packet security validation, repository worktree isolation, and generated-code layout were not flagged for stylistic refactoring.
- The minimal API/UI are treated as implementation-incomplete, not over-architected.
