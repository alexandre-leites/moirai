# Implementation Progress

## Current Status

- Overall status: MVP foundation established; full MVP remains incomplete
- Current phase: All 166 orchestrator tests and all Go tests pass.
- Active task: Proceed with next pending implementation (O11, O28, or additional API endpoints).
- Task type: Cross-component implementation
- Last updated: 2026-07-27
- Planning review: Completed and recorded a source/progress review; added 20 scoped implementation tasks plus 15 further tasks each for Runner, Orchestrator, API, and Web UI.
- Agent/session identifier: primary-implementation

- [x] Added ListRunners proto, Python gRPC service, Go API handler, and composable endpoint
  - Completed: 2026-07-27
  - Relevant files: proto/control_plane.proto, gen/go/gen/control/v1/control_plane.pb.go, gen/go/gen/control/v1/control_plane_grpc.pb.go, orchestrator/src/moirai/protocols/proto/control_plane_pb2.py, orchestrator/src/moirai/protocols/proto/control_plane_pb2_grpc.py, orchestrator/src/moirai/grpc/control_plane.py, orchestrator/src/moirai/persistence/control_plane.py, api/internal/orchestrator/client.go, api/internal/http/handlers/runners.go, api/cmd/api/main.go
  - Behavior delivered: Added `ListRunners` RPC to `ControlPlane` proto with `Runner` message (id, name, enabled, draining, status, labels, last_seen_at). Regenerated Python and Go stubs via `make proto-generate`. Implemented `ListRunners` in Python `ControlPlaneService` gRPC handler and `AsyncpgControlPlane.list_runners()` persistence query. Added Go orchestrator client method and `GET /api/v1/runners` HTTP handler. `GET /api/v1/runners` returns the registered runner with status `online` and current `lastSeenAt`. Docker Compose verified the registered runner reconnects after orchestrator restart.
  - Validation performed: `PYTHONPATH=src python3 -m unittest discover -s tests -v` (166 pass, 12 skipped); `go test ./...` in `api/` (all pass); `go test ./...` in `runner/` (all pass); `docker compose exec api` GET `/api/v1/runners` returns one `online` runner.

- [x] Fixed wait\_for\_human workflow routing with conditional route\_human edge
  - Completed: 2026-07-27
  - Relevant files: orchestrator/src/moirai/workflows/issue_graph.py, orchestrator/src/moirai/workflows/nodes.py, orchestrator/tests/test_issue_graph.py, orchestrator/tests/test_workflow_nodes.py
  - Behavior delivered: `route_human` function routes to `merge`, `repair`, or `blocked` based on `human_approved` and `human_changes_requested` state fields. `wait_for_human` node records human response with `human_approved: False` default. The graph uses a conditional edge from `wait_for_human` instead of a static edge to `END`.
  - Validation performed: `PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=src python3 -m unittest discover -s tests -v` passed (153 tests, 9 skipped).

- [x] Added ListWorkflows REST endpoint to the Go API
  - Completed: 2026-07-27
  - Relevant files: api/internal/http/handlers/workflows.go, api/cmd/api/main.go
  - Behavior delivered: `GET /api/v1/workflows` returns authenticated workflow list from orchestrator gRPC, mapped to a stable public JSON model.
  - Validation performed: `go vet ./... && go test ./...` passed from `api/`.

- [x] Built Web UI authenticated app shell with login, projects, tokens, and workflows views
  - Completed: 2026-07-27
  - Relevant files: web/src/api.ts, web/src/auth.tsx, web/src/login.tsx, web/src/main.tsx, web/src/projects.tsx, web/src/tokens.tsx, web/src/workflows.tsx, web/src/styles.css, web/package.json
  - Behavior delivered: React app with `react-router-dom` routing, `AuthProvider` context for login/logout state, `ProtectedRoute` component for authenticated views, login page with form validation, projects list/create/enable-disable view, runner token create/list/revoke view, and workflows list view. Dark-themed CSS with table forms and navigation header.
  - Validation performed: `npx tsc --noEmit` (TypeScript compiles); `npx vite build` (production build succeeds).

- [x] Add LangGraph PostgresSaver checkpointer integration with interrupt_after/interrupt_before and migration runner
  - Started: 2026-07-27
  - Relevant files: orchestrator/src/moirai/workflows/runtime.py, orchestrator/src/moirai/workflows/issue_graph.py, orchestrator/src/moirai/main.py, orchestrator/src/moirai/persistence/migrations.py, orchestrator/migrations/002_langgraph_checkpointer.sql, orchestrator/tests/test_workflow_runtime.py, orchestrator/tests/test_migrations.py
  - Behavior delivered: `build_issue_graph` accepts `interrupt_after` (dispatch nodes) and `interrupt_before` (wait_for_human) when a checkpointer is present. `build_persisted_runtime` passes these when checkpointer is available. `PersistedWorkflowRuntime` uses `update_state` for checkpointer-aware graph resumption vs state injection for legacy graphs. `main.py` builds a psycopg-backed `AsyncPostgresSaver` checkpointer and wires it through the runtime. A `MigrationRunner` applies numbered SQL migrations in order with tracking in `app.schema_version`, wired into orchestrator startup. Migration 002 creates LangGraph PostgresSaver tables in the `langgraph` schema.
  - Validation: `PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=src python3 -m unittest discover -s tests -v` passed (152 tests, 9 grpcio skips); `go test ./...` passes in runner/ and api/.
  - Notes: `interrupt_after` is set for `("plan", "implement", "review", "repair", "push")` so the graph returns control to the orchestrator after each dispatch phase, allowing the scheduler to assign a runner. `interrupt_before` is set for `("wait_for_human",)` so human approval pauses the workflow at the LangGraph interrupt boundary.

- [x] Complete orchestrator phase-transition logic (pipeline, review, repair, etc) in `nodes.py`
  - Started: 2026-07-27
  - Relevant files: orchestrator/src/moirai/workflows/nodes.py, orchestrator/src/moirai/workflows/runner_events.py, orchestrator/src/moirai/persistence/control_plane.py
  - Behavior delivered: Integrated `local_pipeline`, `ai_review`, and `pushing` phases into workflow nodes. Terminal runner events now trigger phase-aware transitions (e.g., developer completion transitions to pipeline; review completion transitions to push).
  - Validation: `PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=src python3 -m unittest tests.test_task_packets tests.test_scheduler_service tests.test_asyncpg_control_plane -v`; `PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=src python3 -m compileall -q src tests`.

  - Started: 2026-07-27
  - Relevant files: orchestrator/src/moirai/persistence/control_plane.py, orchestrator/src/moirai/workflows/task_packets.py, orchestrator/src/moirai/scheduler.py, orchestrator/tests/test_asyncpg_control_plane.py, orchestrator/tests/test_task_packets.py
  - Behavior delivered: The scheduler now claims queued execution requests, fences lease generations, and builds role-scoped developer packets. Orchestrator persistence maps terminal runner events to continued queued execution requests.
  - Validation: `PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=src python3 -m unittest tests.test_task_packets tests.test_scheduler_service tests.test_asyncpg_control_plane -v`; `PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=src python3 -m compileall -q src tests`.
  - Notes: A major vertical slice towards autonomous continuation is complete.

- [x] Harden Docker Compose for runner deployment
  - Started: 2026-07-27
  - Relevant files: runner/Dockerfile, compose.yaml
  - Behavior delivered: Runner image installs `git` and `opencode-ai` via npm; Compose runner service accepts a token environment variable.
  - Validation: `go test ./... && go vet ./...` in `runner/`.

- [x] CMT-003: Separate phase packet construction from persistence and generic event append
  - Completed: 2026-07-25
  - Relevant files: orchestrator/src/moirai/workflows/task_packets.py, orchestrator/src/moirai/workflows/runner_events.py, orchestrator/src/moirai/workflows/issue_graph.py, orchestrator/src/moirai/persistence/control_plane.py, orchestrator/src/moirai/grpc/runner_control.py, orchestrator/tests/test_task_packets.py, orchestrator/tests/test_runner_events.py, orchestrator/tests/test_issue_graph.py
  - Behavior delivered: Added validated runner lifecycle-event parsing, role-aware terminal transition mapping, transactionally fenced job/workflow updates, and execution lifecycle writes while preserving planner packet behavior.
  - Validation performed: `PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=src python3 -m unittest discover -s tests -v` passed (111 tests, 9 grpcio skips); `PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=src python3 -m compileall -q src tests` passed.
  - Notes: Live asyncpg and grpcio integrations remain unavailable locally.

- [x] O6: Complete durable issue synchronization and label reconciliation
  - Completed: 2026-07-25
  - Relevant files: orchestrator/src/moirai/services/issue_sync.py, orchestrator/src/moirai/issue_trackers/github_cli.py, orchestrator/src/moirai/persistence/control_plane.py, orchestrator/src/moirai/main.py, orchestrator/migrations/001_initial.sql, orchestrator/tests/test_issue_sync.py
  - Behavior delivered: Enabled projects derive GitHub tracker identity from validated remotes, synchronize priority/eligibility snapshots, mark missing open issues ineligible, persist successful label reconciliation, and use durable bounded retry state restored on restart. The loop is cancelled with orchestrator shutdown.
  - Validation performed: `PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=src python3 -m unittest tests.test_issue_sync tests.test_github_cli -v` passed (15 tests); `PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=src python3 -m compileall -q src tests` passed.
  - Notes: Live PostgreSQL and credentialed GitHub CLI integration remain unavailable locally.

- [x] O3: Implement durable project configuration CRUD and runner-token administration
  - Completed: 2026-07-25
  - Relevant files: proto/control_plane.proto, gen/go/gen/control/v1/, orchestrator/src/moirai/persistence/control_plane.py, orchestrator/src/moirai/grpc/control_plane.py, orchestrator/tests/test_asyncpg_control_plane.py, orchestrator/tests/test_control_plane_grpc.py
  - Behavior delivered: Administrator-authenticated internal RPCs now create, update, enable/disable, list, and revoke project/runner-token resources. Registration-token creation/revocation and project create/update/enable/disable record actor-linked audit events; all new RPC bindings are generated.
  - Validation performed: `make proto-lint proto-generate`; `PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=src python3 -m unittest tests.test_asyncpg_control_plane -v`; prior full dependency-free suite and generated Go contract tests passed; `git diff --check` passed.
  - Notes: Live gRPC tests remain skipped until grpcio is installed. Public API resource exposure is A3.

## Done

- [x] Fixed 3 pre-existing test failures — all 166 orchestrator tests pass
  - Completed: 2026-07-27
  - Relevant files: orchestrator/tests/test_control_plane_grpc.py, orchestrator/tests/test_end_to_end.py
  - Behavior delivered: `_MissingListProjects` mock with `validate_session` + session metadata in gRPC call returns `UNIMPLEMENTED` instead of `UNAUTHENTICATED`. Added `plan_valid`, `pipeline_passed`, `review_approved`, `checks_passed` gate flags to LangGraph test states so graphs route through all gates to completion. Changed `interrupt_before` to `interrupt_after` for `wait_for_human` so the node runs and sets `status: "waiting_human"` before pausing.
  - Validation performed: `python -m pytest tests/ -v` — 166 passed, 6 subtests passed.
  - Notes: All fixes are in test files only; no production code changes.

- [x] Fixed InMemoryControlPlane.accept_event async regression and 2 pre-existing test bugs
  - Completed: 2026-07-27
  - Relevant files: orchestrator/src/moirai/domain/control_plane.py, orchestrator/src/moirai/workflows/issue_graph.py, orchestrator/tests/test_end_to_end.py
  - Behavior delivered: Reverted `InMemoryControlPlane.accept_event` from `async def` to `def` (sync), fixing 2 test_control_plane.py regressions. Fixed `_FakeDispatcher.__call__` → `dispatch` to match `ExecutionDispatcher` protocol. Fixed `interrupt_before`/`interrupt_after` tuple→list conversion in `build_issue_graph` for langgraph v1.2.9 compatibility.
  - Validation performed: `python -m pytest orchestrator/tests/test_control_plane.py -v` (6/6 pass); `python -m pytest orchestrator/tests/ -v` (163/166 pass, 3 pre-existing failures confirmed on clean main).
  - Notes: All 3 failures (1 gRPC auth mock, 2 LangGraph routing) were confirmed pre-existing by testing against clean main without my changes.

- [x] R-MANUAL-003: Manually validated real orchestrator-to-runner offer delivery and OpenCode lifecycle responses
  - Completed: 2026-07-26
  - Relevant files: orchestrator/src/moirai/grpc/runner_control.py, orchestrator/src/moirai/domain/control_plane.py, runner/cmd/runner/main.go
  - Behavior delivered: A dependency-complete Python container ran the real `RunnerControlService`; a host runner registered through that service, heartbeated into availability, received an orchestrator-scheduled offer, accepted its lease, ran a real OpenCode session, and returned ordered fenced `started` and `completed` events. The service authenticated and accepted both events.
  - Validation performed: Isolated manual control-plane run produced event sequences `[1, 2]`, event types `["started", "completed"]`, and the requested worktree proof artifact; `docker run --rm -v "/mnt/development/github/moirai/orchestrator:/app" -w /app -e PYTHONPATH=src moirai-orchestrator-manual python -m unittest tests.test_runner_grpc -v` passed (5 tests); `git diff --check` passed.
  - Notes: This validation intentionally used `InMemoryControlPlane`, so it proves gRPC registration, scheduling, offer delivery, lease acknowledgement, and event intake but not AsyncPG/PostgreSQL transaction persistence or LangGraph phase continuation. Full Compose durable lifecycle validation remains pending.

- [x] R-MANUAL-002: Bound OpenCode sessions to their runner workspace and manually validated a live provider execution
  - Completed: 2026-07-26
  - Relevant files: runner/internal/agents/opencode.go, runner/internal/agents/opencode_test.go
  - Behavior delivered: The OpenCode backend now passes its prepared workspace through `opencode run --dir`, preventing the CLI's local server from defaulting outside the job worktree. A real OpenCode `--auto` coding session completed through an isolated gRPC runner simulation, wrote the requested repository artifact and protocol result document, and emitted fenced `started` then `completed` events.
  - Validation performed: Direct OpenCode workspace probe completed with exit code 0 and a valid result document; `go test ./cmd/runner -run '^TestManualOpenCodeRunnerSession$' -count=1 -v` passed after the fix; `go test ./cmd/runner ./internal/agents`; `go vet ./...`; and `git diff --check` passed from `runner/`.
  - Notes: OpenCode provider sessions can take tens of seconds. The initial failed manual test had no explicit `--dir`, causing agent artifacts to be written outside the runner workspace and validation to fail.

- [x] Added consistent custom arguments for every agent backend
  - Completed: 2026-07-26
  - Relevant files: runner/cmd/runner/main.go, runner/internal/agents/opencode.go, runner/internal/agents/opencode_test.go
  - Behavior delivered: `LOOP_RUNNER_AGENT_ARGUMENTS` is now passed as an argument vector to OpenCode as well as generic CLI and Docker agents. For OpenCode, arguments are placed after `run` and before the generated task prompt, supporting documented flags such as `--auto`, `--model`, and `--agent` without shell interpolation.
  - Validation: `go test ./internal/agents ./cmd/runner`; `go test -race ./...`; `go vet ./...`; and `git diff --check` passed from `runner/`.

- [x] R39/R40: Persist and reconcile execution manifests
  - Completed: 2026-07-26
  - Relevant files: runner/internal/execution/local.go, runner/internal/execution/local_test.go, runner/internal/agents/manifest.go, runner/internal/agents/recovery.go, runner/internal/agents/recovery_test.go, runner/internal/agents/cli.go, runner/internal/agents/opencode.go, runner/internal/agents/docker.go, runner/internal/execution/docker.go, runner/cmd/runner/main.go
  - Behavior delivered: All execution modes write private workspace manifests with backend, execution ID, PID, and start time. Runner startup validates and removes stale records before connecting or accepting work; it deliberately does not signal manifest PIDs because agent-writable workspaces cannot be trusted as an authority for process termination.
  - Validation: `go test ./internal/agents ./cmd/runner`; `go test -race ./...`; `go vet ./...`; and `git diff --check` passed from `runner/`.

- [x] R38: Bounded stdout/stderr artifact retention and checksum metadata
  - Completed: 2026-07-26
  - Relevant files: runner/internal/agents/log.go, runner/internal/agents/log_test.go, runner/internal/agents/cli.go, runner/internal/agents/opencode.go, runner/internal/agents/docker.go
  - Behavior delivered: CLI, OpenCode, and Docker agent logs retain at most 4 MiB per stream while SHA-256 hashing all observed output. Each backend writes private JSON metadata containing checksums, retained bytes, and truncation flags next to its logs.
  - Validation: `go test -race ./...`; `go vet ./...`; and `git diff --check` passed from `runner/`.

- [x] R34: Add structured commit, push, and branch-cleanup execution adapters
  - Completed: 2026-07-26
  - Relevant files: runner/internal/repository/delivery.go, runner/internal/repository/delivery_test.go, runner/internal/repository/manager_test.go
  - Behavior delivered: Repository adapters validate workspace, commit message, and branch refs; commits are no-change idempotent, pushes establish the branch upstream, and remote cleanup first probes `ls-remote` so an already absent branch is a successful no-op.
  - Validation: `go test ./internal/repository -v`; `go test -race ./...`; `go vet ./...`; and `git diff --check` passed from `runner/`.
  - Notes: Invocation from a delivery-specific task packet remains an orchestrator/protocol integration task.

- [x] R29: Reload non-secret runner control settings without cancelling an active execution
  - Completed: 2026-07-26
  - Relevant files: runner/cmd/runner/main.go, runner/cmd/runner/main_probe_test.go, runner/internal/control/stream.go, runner/internal/control/stream_test.go
  - Behavior delivered: SIGHUP reloads validated runner configuration into a locked control-stream settings holder without restarting the dispatcher or replacing identity, endpoint, TLS, registration, or agent configuration. Labels apply on the next heartbeat; heartbeat and reconnect bounds apply safely to future connection/retry cycles.
  - Validation: `go test ./internal/control ./cmd/runner -v`; `go test -race ./...`; and `go vet ./...` passed from `runner/`. Tests prove dynamic labels, isolated settings copies, and reloaded reconnect-delay bounds.

- [x] R-MANUAL-001: Validated the runner against a simulated gRPC orchestrator and corrected managed-clone worktree creation
  - Completed: 2026-07-26
  - Relevant files: runner/cmd/runner/main_integration_test.go, runner/internal/repository/manager.go, runner/internal/repository/manager_test.go
  - Behavior delivered: A real Runner instance now registers through the gRPC protocol, persists its returned identity, connects outbound, heartbeats, accepts an offer, waits for a lease acknowledgement, prepares an actual Git mirror/worktree, invokes a CLI agent, reports ordered fenced started/completed events, terminates an active execution when the simulated orchestrator cancels its lease, and reconnects after an injected `Unavailable` control-stream interruption without losing the active execution. Managed mirror sources now create worktrees from their local default-branch ref rather than the absent `origin/<branch>` tracking ref; existing-path sources retain `origin/<branch>` behavior.
  - Validation performed: `go test ./cmd/runner -run 'TestRunnerAgainstSimulatedOrchestrator' -v`; `go test -race ./...`; and `go vet ./...` passed from `runner/`.
  - Notes: This is an isolated runner verdict, not a full-stack MVP verdict. It covers success, cancellation, and transient-stream reconnection but not durable event outbox persistence across process restart, Docker execution, or production TLS/authentication.

- [x] CMT-002: Added fenced transactional lease recovery re-offers
  - Completed: 2026-07-25
  - Relevant files: orchestrator/src/moirai/persistence/control_plane.py, orchestrator/src/moirai/scheduler.py, orchestrator/tests/test_asyncpg_control_plane.py, orchestrator/tests/test_scheduler_service.py, CODE_MAINTENANCE_TASKS.md
  - Behavior delivered: Expired accepted leases are fenced, then the scheduler prioritizes one transactionally re-offered recovery job on a compatible idle runner. The existing project lock and workflow ID remain, event sequence resets for the new generation, and an auditable recovery-offered event is appended.
  - Validation performed: `PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=src python3 -m unittest tests.test_asyncpg_control_plane tests.test_scheduler_service -v`; `PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=src python3 -m unittest tests.test_asyncpg_control_plane -v` passed.
  - Notes: Tests use a PostgreSQL-shaped fake; live PostgreSQL transaction validation remains unavailable in this environment.

- [x] CMT-004: Defined scheduler offer-delivery failure aftermath
  - Completed: 2026-07-25
  - Relevant files: orchestrator/src/moirai/scheduler.py, orchestrator/tests/test_scheduler_service.py, CODE_MAINTENANCE_TASKS.md
  - Behavior delivered: Packet construction and runner-session delivery errors now reject the already persisted offer through the same lock-release path as a declined delivery. An explicit `OfferDeliveryError` preserves delivery or cleanup failure context for scheduler supervision.
  - Validation performed: `PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=src python3 -m unittest tests.test_scheduler_service -v`; `PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=src python3 -m unittest discover -s tests -v`; `PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=src python3 -m compileall -q src tests` from `orchestrator/`.
  - Notes: CMT-001 (workflow characterization and runtime ownership) remains the next high-value maintenance task.

- [x] CMT-006/CMT-007: Decoupled runner control state from synchronous transport calls
  - Completed: 2026-07-25
  - Relevant files: runner/internal/control/events.go, runner/internal/control/events_test.go, runner/internal/control/offer.go, runner/internal/control/offer_test.go, CODE_MAINTENANCE_TASKS.md
  - Behavior delivered: Event queue mutation and transport sends are now separated by a serialized sender, allowing matching leases to be abandoned while a send blocks without losing FIFO/sequence behavior. Offer admission reserves state before acceptance and performs remote calls outside the state lock, rolling the reservation back on failure.
  - Validation performed: `gofmt -w internal/control/events.go internal/control/events_test.go internal/control/offer.go internal/control/offer_test.go`; `go test -race ./internal/control`; `go vet ./...` from `runner/`.
  - Notes: CMT-001 (workflow characterization and runtime ownership) is the next highest-value maintenance task.

- [x] CMT-005: Bound Docker cancellation and stop operations
  - Completed: 2026-07-25
  - Relevant files: runner/internal/execution/docker.go, runner/internal/execution/docker_test.go, CODE_MAINTENANCE_TASKS.md
  - Behavior delivered: Docker timeout and explicit cancellation now share a finite stop deadline. The stop subprocess runs in a separate process group so deadline cancellation terminates shell descendants instead of waiting indefinitely.
  - Validation performed: `gofmt -w internal/execution/docker.go internal/execution/docker_test.go`; `go test ./internal/execution`; `go vet ./...`; `go test -race ./internal/execution` from `runner/`.
  - Notes: CMT-006 (event reporter lock/I/O separation) is the next independent maintainability task.

- [x] Added the internal ControlPlane gRPC service and mounted it beside RunnerControl
  - Type: MVP API-to-orchestrator boundary
  - Completed: 2026-07-25
  - Relevant files: orchestrator/src/moirai/grpc/control_plane.py, orchestrator/src/moirai/main.py, orchestrator/tests/test_control_plane_grpc.py
  - Behavior delivered: The orchestrator registers generated ControlPlane and RunnerControl services on one graceful gRPC lifecycle. ControlPlane validates login and runner-token input, maps rejection and unavailable capabilities to typed gRPC errors, emits typed project/workflow responses, and injects clock behavior for deterministic tests.
  - Validation performed: `PYTHONPATH=src python3 -m unittest tests.test_control_plane_grpc tests.test_runner_grpc -v`; `PYTHONPATH=src python3 -m unittest discover -s tests -v`; `PYTHONPATH=src python3 -m compileall -q src tests`; and `git diff --check` passed from `orchestrator/`.
  - Notes: All 52 dependency-free tests passed. The eight gRPC integration cases, including the three new ControlPlane cases, were correctly skipped because grpcio is unavailable locally; real gRPC serving remains to be exercised after dependencies are installed.

- [x] Implemented runner cancellation, drain admission, and lease-expiry recovery
  - Type: MVP runner recovery implementation
  - Completed: 2026-07-25
  - Relevant files: runner/cmd/runner/main.go, runner/internal/control/stream.go, runner/internal/control/stream_test.go, runner/internal/dispatch/control_loop.go, runner/internal/dispatch/control_loop_test.go, runner/internal/dispatch/dispatch.go
  - Behavior delivered: The runner reconciles active leases before every heartbeat, renews valid leases, cancels process-backed agent executions on matching orchestrator cancellation or lease expiry, emits a fenced cancelled terminal event, rejects new offers once draining while allowing an active execution to finish, and preserves buffered active events for reconnection before lease expiry.
  - Validation performed: `go test ./internal/control ./internal/dispatch ./internal/agents ./internal/execution`, `go vet ./...`, and `go test -race ./...` passed from `runner/`; `git diff --check` passed from the repository root.
  - Notes: The current protocol has no Drain or reconnect-state envelope, so drain is exposed as a control-loop admission state for the later protocol command task; terminal event outbox persistence remains R8.

- [x] Wired acknowledged leases to runner dispatch and fenced terminal lifecycle events
  - Type: MVP runner execution event integration
  - Completed: 2026-07-25
  - Relevant files: runner/cmd/runner/main.go, runner/internal/control/stream.go, runner/internal/dispatch/control_loop.go, runner/internal/dispatch/control_loop_test.go
  - Behavior delivered: The runner now receives offers, admits only a valid one, dispatches it after the matching lease acknowledgement, emits ordered fenced `started` and terminal events without agent-result summaries, reports busy heartbeats while leased, and flushes buffered events on stream reconnection.
  - Validation performed: `go test ./internal/control ./internal/dispatch`, `go vet ./...`, and `go test -race ./...` passed from `runner/`; `git diff --check` passed from the repository root.
  - Notes: Cancel messages are deliberately ignored until R7 adds backend/executor cancellation and terminal cancellation reporting. Lease timing currently uses the MVP defaults (60-second duration, 15-second renewal lead) at runner bootstrap.

- [x] Implemented accepted-task workspace dispatch and agent execution
  - Type: MVP runner task execution
  - Completed: 2026-07-25
  - Relevant files: runner/internal/dispatch/dispatch.go, runner/internal/dispatch/dispatch_test.go, runner/internal/repository/manager.go, runner/internal/repository/manager_test.go
  - Behavior delivered: An acknowledged lease now maps to a dedicated managed-clone or existing-path worktree, repository-local `.loop` task packet and prompt artifacts, a configured agent backend invocation with bounded timeout, durable terminal result artifacts, and mandatory cleanup. Secret references fail closed without a resolver.
  - Validation performed: `go test ./internal/dispatch ./internal/repository`; `gofmt -w internal/dispatch/dispatch.go internal/dispatch/dispatch_test.go internal/repository/manager.go internal/repository/manager_test.go`; `go vet ./... && go test -race ./...` passed from `runner/`.
  - Notes: R6 will wire the dispatcher into inbound control messages and report its lifecycle/terminal outcomes through generation-fenced events.

- [x] Implemented runner offer admission and lease renewal state machine
  - Type: MVP runner offer and lease safety
  - Completed: 2026-07-24
  - Relevant files: runner/internal/control/offer.go, runner/internal/control/offer_test.go
  - Behavior delivered: The runner validates task packets before accepting offers, admits exactly one pending or active job, rejects invalid/busy offers, applies only matching authoritative lease acknowledgements, requests one renewal before expiry, and abandons stale or expired leases.
  - Validation performed: `go test ./internal/control`; `gofmt -w internal/control/offer.go internal/control/offer_test.go internal/control/client.go`; `go vet ./... && go test -race ./...`; and `git diff --check` passed from `runner/`.

- [x] Implemented validated runner task-packet contract
  - Type: MVP runner task admission foundation
  - Completed: 2026-07-24
  - Relevant files: schemas/task-packet.schema.json, runner/internal/taskpacket/taskpacket.go, runner/internal/taskpacket/taskpacket_test.go, README.md
  - Behavior delivered: Packets now carry validated repository source, role, task artifact paths, timeout, secret references, and permissions. The runner rejects unknown versions/fields, traversal, unsafe source inputs, unauthorized write/push/merge operations, malformed environment references, and multiple JSON values.
  - Validation performed: `python3 -m json.tool ../schemas/task-packet.schema.json`; `go vet ./... && go test -race ./...`; and `git diff --check` passed from `runner/`.

- [x] Implemented reconnecting authenticated runner control-stream supervision
  - Type: MVP runner connection and heartbeat lifecycle
  - Completed: 2026-07-24
  - Relevant files: runner/internal/control/stream.go, runner/internal/control/stream_test.go, runner/cmd/runner/main.go
  - Behavior delivered: The runner owns one authenticated outbound stream, sends an initial heartbeat, continues periodic heartbeats, disconnects stale streams, and retries transient failures with capped exponential backoff plus bounded jitter until cancellation.
  - Validation performed: `gofmt -w cmd/runner/main.go internal/control/stream.go internal/control/stream_test.go`; `go vet ./... && go test -race ./...`; and `git diff --check` passed from `runner/`.

- [x] Implemented runner runtime configuration, dialer, bootstrap, and graceful shutdown
  - Type: MVP runner startup
  - Completed: 2026-07-24
  - Relevant files: runner/cmd/runner/main.go, runner/internal/config/config.go, runner/internal/config/config_test.go, runner/internal/control/dialer.go, runner/internal/control/client.go, README.md
  - Behavior delivered: Startup validates endpoint, data directory, name, labels, TLS, heartbeat, and reconnect settings; securely loads or registers an identity; initializes the typed gRPC client; and closes the gRPC connection on SIGINT or SIGTERM.
  - Validation performed: `gofmt -w cmd/runner/main.go internal/config/config.go internal/config/config_test.go internal/control/client.go internal/control/dialer.go`; `go vet ./... && go test -race ./...`; and `git diff --check` passed from `runner/`.

- [x] Implemented runner identity registration-or-load boundary
  - Type: MVP runner registration startup
  - Completed: 2026-07-24
  - Relevant files: runner/internal/control/identity.go, runner/internal/control/identity_test.go
  - Behavior delivered: Startup reuses a valid persisted identity without a token, or registers exactly once when the identity is absent and saves the returned credential atomically. Missing identity without a token fails closed.
  - Validation performed: `gofmt -w internal/control/identity.go internal/control/identity_test.go`; `go vet ./internal/control`; and `go test -race ./internal/control` passed from `runner/`.
  - Notes: The next implementation is a reconnecting stream loop that consumes this boundary.

- [x] Implemented secure persistent runner identity storage
  - Type: MVP runner registration foundation
  - Completed: 2026-07-24
  - Relevant files: runner/internal/control/identity.go, runner/internal/control/identity_test.go
  - Behavior delivered: Runner identity and credential records are validated, written through a private temporary file, synchronized, atomically renamed, and rejected when group/other-readable, malformed, non-regular, or incomplete.
  - Validation performed: `gofmt -w internal/control/identity.go internal/control/identity_test.go` and `go vet ./... && go test -race ./...` passed from `runner/`.
  - Notes: The storage boundary is ready for startup registration-or-load logic; runner control stream connection remains pending.

- [x] Implemented runner offer decisions, lease acknowledgements, and typed Go control client
  - Type: MVP runner-control transport and lease safety
  - Completed: 2026-07-24
  - Relevant files: proto/runner_control.proto, gen/go/gen/runner/v1/, orchestrator/src/moirai/protocols/proto/, orchestrator/src/moirai/grpc/runner_control.py, orchestrator/src/moirai/grpc/sessions.py, orchestrator/src/moirai/domain/control_plane.py, orchestrator/src/moirai/persistence/control_plane.py, runner/internal/control/client.go
  - Behavior delivered: Runners can explicitly accept or reject only their currently assigned offer, renew an owned lease by generation, and receive authoritative lease acknowledgements. Offer rejection atomically cancels the offered job and workflow and releases the project lock. The Go control client stamps every message with the persisted identity and exposes registration, connection, heartbeats, offer decisions, renewal, and receive operations.
  - Validation performed: `make proto-check`; `go vet ./... && go test -race ./...` in `runner/`; `go vet ./... && go test ./...` in `gen/go` and `api/`; `PYTHONPATH=src python3 -m unittest discover -s tests -v`; `PYTHONPATH=src python3 -m compileall -q src tests`; and `git diff --check` passed. The full orchestrator suite passed 49 tests; five gRPC integration cases remain skipped because grpcio is unavailable.
  - Notes: Lease timestamps use integer UTC Unix milliseconds, avoiding a protobuf well-known-type dependency. Python Ruff and mypy remain unavailable locally.

- [x] Implemented connected authenticated runner-session offer delivery
  - Type: MVP runner connection and job-offer delivery
  - Completed: 2026-07-24
  - Relevant files: orchestrator/src/moirai/grpc/sessions.py, orchestrator/src/moirai/grpc/runner_control.py, orchestrator/src/moirai/grpc/__init__.py, orchestrator/tests/test_runner_sessions.py
  - Behavior delivered: The orchestrator now maintains replaceable authenticated runner sessions, delivers an offer only to an active matching session, limits each session to one outstanding offer, rejects delivery while unavailable or busy, and closes stale sessions on reconnection or stream cleanup. RunnerControl exposes a task-packet offer delivery operation without exposing credentials.
  - Validation performed: `PYTHONPATH=src python3 -m unittest tests.test_runner_sessions -v`; `PYTHONPATH=src python3 -m unittest discover -s tests -v`; `PYTHONPATH=src python3 -m compileall -q src tests`; and `git diff --check` passed from `orchestrator/` (46 tests; four gRPC tests skipped because grpcio is unavailable).
  - Notes: Explicit acceptance and renewal messages require an additive protocol change and regenerated stubs; the existing generic event envelope remains fenced.

- [x] Implemented transactional PostgreSQL scheduling, offers, leases, and event fencing
  - Type: MVP control-plane persistence
  - Completed: 2026-07-24
  - Relevant files: orchestrator/src/moirai/persistence/control_plane.py, orchestrator/migrations/001_initial.sql, orchestrator/tests/test_asyncpg_control_plane.py
  - Behavior delivered: A serializable transaction locks one eligible issue, enabled project, and compatible idle runner; creates the offered workflow, project lock, job, and offer; and returns the matching domain packet. Offer acceptance transitions the job/workflow together. Lease renewal rejects stale ownership, and event persistence atomically fences generations and strictly increasing sequence numbers while recording workflow events.
  - Validation performed: `PYTHONPATH=src python3 -m unittest tests.test_asyncpg_control_plane -v`; `PYTHONPATH=src python3 -m unittest discover -s tests -v`; `PYTHONPATH=src python3 -m compileall -q src tests`; and `git diff --check` passed from `orchestrator/` (43 tests; four gRPC tests skipped because grpcio is unavailable).
  - Notes: PostgreSQL integration remains pending because asyncpg and PostgreSQL are unavailable locally. The new `last_event_sequence` job column is part of the initial schema and must be present before applying the migration to a new environment.

- [x] Implemented durable runner registration and orchestrator gRPC lifecycle
  - Type: MVP control-plane persistence and runner connectivity
  - Completed: 2026-07-24
  - Relevant files: orchestrator/src/moirai/persistence/control_plane.py, orchestrator/src/moirai/persistence/__init__.py, orchestrator/src/moirai/main.py, orchestrator/src/moirai/grpc/runner_control.py, orchestrator/tests/test_asyncpg_control_plane.py
  - Behavior delivered: The asyncpg adapter atomically consumes one-time scoped registration tokens, stores only credential hashes, authenticates and marks runner heartbeats online, and uses constant-time credential comparison. RunnerControl accepts both sync reference and async durable control-plane methods. The service creates and closes its database pool around a gracefully stopped gRPC server.
  - Validation performed: `PYTHONPATH=src python3 -m unittest tests.test_asyncpg_control_plane tests.test_control_plane tests.test_config -v`; `PYTHONPATH=src python3 -m unittest discover -s tests -v`; and `PYTHONPATH=src python3 -m compileall -q src tests` passed from `orchestrator/` with 40 tests, four gRPC cases skipped because grpcio is unavailable. `git diff --check` passed.
  - Notes: PostgreSQL integration remains blocked by unavailable local dependencies and exhausted Docker storage. The next vertical slice must persist scheduler offers and lease fencing.

- [x] Implemented authenticated runner control-stream intake
  - Type: MVP runner connection and stale-event safety
  - Completed: 2026-07-24
  - Relevant files: orchestrator/src/moirai/grpc/runner_control.py, orchestrator/tests/test_runner_grpc.py
  - Behavior delivered: `Connect` now authenticates every inbound runner message without exposing credentials, fixes a stream to one runner identity, marks valid heartbeat senders connected and healthy, validates event payload JSON, and forwards events through the existing lease-generation and sequence fencing invariant. Invalid credentials receive `UNAUTHENTICATED`; malformed or stale events receive safe generic gRPC failures.
  - Validation performed: `PYTHONPATH=src python3 -m compileall -q src tests`; `PYTHONPATH=src python3 -m unittest tests.test_runner_grpc -v`; and `PYTHONPATH=src python3 -m unittest discover -s tests -v` from `orchestrator/` passed with 38 tests, including four gRPC cases skipped because `grpcio` is not installed locally; `git diff --check` passed.
  - Notes: The stream does not yet emit offers because durable job-offer persistence and connected-session delivery are pending.

- [x] Added tested runner-registration gRPC service boundary
  - Type: MVP control-plane implementation
  - Completed: 2026-07-24
  - Relevant files: orchestrator/src/moirai/grpc/runner_control.py, orchestrator/src/moirai/grpc/__init__.py, orchestrator/src/proto/__init__.py, orchestrator/tests/test_runner_grpc.py, orchestrator/pyproject.toml
  - Validation performed: Container-backed Python suite passed 36 tests with gRPC registration coverage; local dependency-free suite passed 36 tests with the two gRPC tests correctly skipped; generated bindings imported in the prior validated container image; protocol lint/generation and generated Go module tests passed.
  - Evidence: `RegisterRunner` accepts only protocol version 1.0, validates runner input, exchanges a one-time scoped token for a per-runner credential, and returns generic rejection errors that do not disclose tokens. Tests verify registration, token replay rejection, unsupported versions, and invalid input.
  - Notes: The service is intentionally not mounted by `main.py` until an asyncpg-backed control-plane replaces the in-memory seam. The generated bindings require the newly declared protobuf runtime dependency.

- [x] Added reproducible shared Go Protocol Buffer generation
  - Type: MVP infrastructure and integration
  - Completed: 2026-07-24
  - Relevant files: buf.yaml, buf.gen.yaml, gen/go/, proto/control_plane.proto, proto/runner_control.proto, Makefile, README.md
  - Validation performed: `docker compose config --quiet`, `make proto-lint proto-generate`, `make proto-check`, `go vet ./... && go test ./...` from `gen/go`, Python unit tests, API Go tests, runner race tests, and `git diff --check` passed.
  - Evidence: Pinned Buf 1.50.0 runs in Docker; it lints protocol definitions and generates checked-in bindings from a shared `github.com/loop-engineering/contracts` Go module. The Makefile provides repeatable lint, generate, and stale-output checks.
  - Notes: The flat `proto/` directory and required `ControlPlane`/`RunnerControl` service names are documented as narrow Buf lint exceptions. Python gRPC stubs and service wiring remain pending.

- [x] Added secure orchestrator runtime configuration loading
  - Type: Security and operational reliability
  - Completed: 2026-07-24
  - Relevant files: orchestrator/src/moirai/config.py, orchestrator/src/moirai/main.py, orchestrator/tests/test_config.py
  - Validation performed: `PYTHONPATH=src python3 -m unittest discover -s tests -v && PYTHONPATH=src python3 -m compileall -q src tests` passed with 34 tests; a one-second `python3 -m moirai.main` smoke test confirmed structured startup output without logging the database URL; `go vet ./... && go test ./...` passed in `api/`; `go vet ./... && go test -race ./...` passed in `runner/`; `git diff --check` passed.
  - Evidence: Runtime configuration requires exactly one direct or file-backed database URL, rejects empty, oversized, unreadable, non-regular, or NUL-containing secrets, validates gRPC bind ports, and logs only the bind address at startup.
  - Notes: Python Ruff and mypy remain unavailable in the execution environment; configuration loading does not connect to PostgreSQL until the durable persistence adapter is implemented.

- [x] Added validated control-plane lease renewal
  - Type: Reliability and stale-lease prevention
  - Completed: 2026-07-24
  - Relevant files: orchestrator/src/moirai/domain/leases.py, orchestrator/src/moirai/domain/control_plane.py, orchestrator/tests/test_leases.py, orchestrator/tests/test_control_plane.py
  - Validation performed: `PYTHONPATH=src python3 -m unittest discover -s tests -v && PYTHONPATH=src python3 -m compileall -q src tests` passed with 29 tests; `go vet ./... && go test ./...` passed in `api/`; `go vet ./... && go test -race ./...` passed in `runner/`; `git diff --check` passed before unavailable lint tooling stopped the combined check.
  - Evidence: Renewals require the runner assigned to an accepted offer, the current lease generation, an unexpired lease, and a future expiration. Regression coverage rejects stale generation, non-future expiry, and unauthorized runner renewal.
  - Notes: The in-memory reference behavior now represents the runner heartbeat/renewal invariant that PostgreSQL transactions and the gRPC stream must preserve.

- [x] Hardened deterministic workflow gate routing
  - Type: Correctness and invalid-merge prevention
  - Completed: 2026-07-24
  - Relevant files: orchestrator/src/moirai/workflows/policy.py, orchestrator/tests/test_workflow_policy.py
  - Validation performed: `PYTHONPATH=src python3 -m unittest discover -s tests -v && PYTHONPATH=src python3 -m compileall -q src tests` passed with 27 tests; `go vet ./... && go test ./...` passed in `api/`; `go vet ./... && go test -race ./...` passed in `runner/`; `git diff --check` passed.
  - Evidence: Regression tests prove that AI review cannot start after the total agent budget is exhausted and that passing GitHub checks cannot route to human approval or merge without recorded pipeline and independent-review approval.
  - Notes: The policy now defensively protects merge routing even if a caller attempts an out-of-order workflow transition.

- [x] Implemented and tested existing-local-path repository worktree isolation
  - Type: MVP runner execution foundation and command-security hardening
  - Completed: 2026-07-24
  - Relevant files: runner/internal/repository/manager.go, runner/internal/repository/manager_test.go, README.md
  - Validation performed: `gofmt -w internal/repository/manager.go internal/repository/manager_test.go`; `go vet ./... && go test -race ./... && go test ./...` passed from `runner/`; `git diff --check` passed.
  - Commands executed: Existing paths are resolved through symlinks, required to be absolute directories without control characters, fetched by argument vector, and used only as the Git worktree source; all job workspaces remain below the runner data directory.
  - Evidence: Fake-Git tests verify existing-path fetch and worktree commands, cleanup through the mounted source, and rejection of relative, control-character, ambiguous, and unsupported repository configuration.
  - Notes: `managed_clone` remains the default for compatibility. `CleanupExisting` makes the source explicit so a mounted repository is never inferred from an untrusted workspace path.

- [x] Implemented and tested runner-managed Git worktree isolation
  - Type: MVP execution foundation and command-security hardening
  - Completed: 2026-07-24
  - Relevant files: runner/internal/repository/manager.go, runner/internal/repository/manager_test.go
  - Validation performed: `gofmt -w internal/repository/manager.go internal/repository/manager_test.go`; `go vet ./... && go test -race ./...` passed from `runner/`.
  - Commands executed: The manager invokes Git with argument vectors for mirror cloning, pruning fetches, worktree creation, and forced worktree removal.
  - Evidence: Fake-Git tests verify the managed clone layout, worktree command sequence, `.loop` task directory, cleanup, and rejection of traversal, option-like URLs, malformed refs, and control characters.
  - Notes: The manager supports the specified managed-clone mode; runner-control integration and retention scheduling remain pending.

- [x] Implemented and tested GitHub CLI code-host pull-request boundary
  - Type: MVP integration boundary and reliability hardening
  - Completed: 2026-07-24
  - Relevant files: orchestrator/src/moirai/code_hosts/github_cli.py, orchestrator/src/moirai/code_hosts/__init__.py, orchestrator/tests/test_github_code_host.py
  - Validation performed: `PYTHONPATH=src python3 -m unittest discover -s tests -v && PYTHONPATH=src python3 -m compileall -q src tests` passed with 26 tests; `go vet ./... && go test ./...` passed in `api/` and `runner/`; `git diff --check` passed.
  - Commands executed: Fake-command coverage verifies existing-PR reuse, workflow marker and issue-closing syntax, all five check states, and refusal to merge before checks pass.
  - Evidence: The adapter builds `gh` argument vectors only, redacts CLI failures, searches before PR creation, does not use `--admin`, and permits only merge/rebase/squash methods.
  - Notes: GitHub CLI remains isolated from issue-tracking and domain code. Branch push/worktree operations remain runner responsibilities.

- [x] Implemented and tested restricted Docker execution mode
  - Type: MVP implementation and security hardening
  - Completed: 2026-07-24
  - Relevant files: runner/internal/execution/docker.go, runner/internal/execution/docker_test.go
  - Validation performed: `gofmt -w internal/execution/docker.go internal/execution/docker_test.go`; `go vet ./... && go test ./...` from `runner/` passed.
  - Commands executed: Docker commands are invoked as argument vectors with deterministic environment ordering and a hash-derived container name.
  - Evidence: Unit tests cover argument construction, default no-network behavior, resource limits, environment validation, and timeout-triggered container stop using a fake Docker executable.
  - Notes: Jobs run with `--rm`, `--init`, an explicit workspace bind mount, and network disabled unless configured. Cancellation attempts both `docker stop` and client-process termination.

- [x] Implemented tested runner process supervision and a validated OpenCode backend boundary
  - Completed: 2026-07-24
  - Relevant files: runner/internal/execution/local.go, runner/internal/execution/local_test.go, runner/internal/agents/opencode.go, runner/internal/agents/opencode_test.go
  - Validation performed: `gofmt -w internal/agents/opencode.go internal/agents/opencode_test.go internal/execution/local.go internal/execution/local_test.go`; `go vet ./... && go test ./...` from `runner/` passed.
  - Notes: Commands use a distinct Unix process group, are cancelled on timeout or explicit cancellation, and retain one active execution ID. OpenCode is checked before execution, receives argument-vector invocation in its assigned workspace, captures bounded local stdout/stderr files, refuses result paths escaping the workspace, and validates a protocol-versioned result document before reporting success.

- [x] Implemented a testable runner registration, scheduling, offer, lock, and lease control-plane slice
  - Completed: 2026-07-24
  - Relevant files: orchestrator/src/moirai/domain/control_plane.py, orchestrator/src/moirai/domain/__init__.py, orchestrator/tests/test_control_plane.py
  - Validation performed: `PYTHONPATH=src python3 -m unittest discover -s tests -v` passed: 10 tests; `PYTHONPATH=src python3 -m compileall -q src tests` passed.
  - Notes: One-time scoped registration tokens are exchanged for hashed durable-credential representations; offers reserve runner capacity and project locks; expiration releases both; only the assigned runner can accept and report fenced lease events.

- [x] Expanded the initial PostgreSQL schema to cover the specified logical MVP model
  - Completed: 2026-07-24
  - Relevant files: orchestrator/migrations/001_initial.sql
  - Validation performed: SQL reviewed for dependency order and constraints; PostgreSQL unavailable for execution.
  - Notes: Adds authentication/session, project configuration, issue sync, workflow/event/lock, runner credential/token, job/offer/execution, delivery, approval, and audit tables under `app`, preserving `langgraph` schema creation.

- [x] Acquired implementation lock and reviewed authoritative specification
  - Completed: 2026-07-24
  - Relevant files: AILOCK.md, PROJECT.md
  - Validation performed: Verified AILOCK.md contains exactly `1`; read all 4,134 lines of PROJECT.md.
  - Notes: No prior PROGRESS.md or implementation files existed.

- [x] Established monorepo source, deployment, and contract foundation
  - Completed: 2026-07-24
  - Relevant files: compose.yaml, Makefile, .env.example, README.md, proto/, schemas/, api/, orchestrator/, runner/, web/
  - Validation performed: Parsed all JSON schemas; checked repository diff for whitespace errors.
  - Notes: Compose isolates PostgreSQL on an internal network and supplies database material through Docker secrets.

- [x] Implemented and tested scheduler priority, project-lock, runner matching, and lease-fencing domain primitives
  - Completed: 2026-07-24
  - Relevant files: orchestrator/src/moirai/domain/, orchestrator/tests/test_scheduling.py, orchestrator/tests/test_leases.py
  - Validation performed: `PYTHONPATH=orchestrator/src python3 -m compileall -q orchestrator/src` and `PYTHONPATH=orchestrator/src python3 -m unittest discover -s orchestrator/tests -v` passed: 7 tests.
  - Notes: Scheduling follows priority DESC, issue creation time, queue time, project ID, and external issue ID. Lease events reject wrong runner, stale generation, expiration, and non-increasing sequence numbers.

- [x] Implemented and validated issue synchronization, workflow gate policy, GitHub CLI adapter seams, and accepted-lease expiry recovery
  - Completed: 2026-07-24
  - Relevant files: orchestrator/src/moirai/domain/issues.py, orchestrator/src/moirai/domain/control_plane.py, orchestrator/src/moirai/workflows/policy.py, orchestrator/src/moirai/issue_trackers/github_cli.py, orchestrator/tests/test_issues.py, orchestrator/tests/test_workflow_policy.py, orchestrator/tests/test_github_cli.py, orchestrator/tests/test_control_plane.py
  - Validation performed: `PYTHONPATH=src python3 -m unittest discover -s tests -v` passed: 21 tests; `PYTHONPATH=src python3 -m compileall -q src tests` passed.
  - Notes: Synchronization derives priority, eligibility, human approval, invalid-label warnings, and deterministic label deltas. GitHub calls use argument vectors and JSON. Expired accepted leases move workflows to recovery, preserve the project lock, free the runner, and bump the fence generation.

## Blocked

- [ ] R37: Bounded no-progress watchdogs
  - Reason: Current `agents.Backend.Execute` returns only after completion and has no output/progress callback, so the runner cannot distinguish a healthy silent command from a stalled one before its existing execution timeout.
  - Required resolution: Extend the backend/executor contract with redacted progress callbacks or periodic activity observations, then reset a watchdog from those observations and from valid lease heartbeats.
  - Independent work still available: R38 bounded artifact retention/checksums, R39 process manifests, R40 restart reconciliation, R41 safe update/drain, R42 metrics, and protocol-generated R13/R14.

- [ ] Full Protocol Buffer, Docker, dependency, and integration validation
  - Blocked since: 2026-07-24
  - Reason: The execution environment has Go, Python, Node, and npm but no `make`, `protoc`, `buf`, `docker`, `pytest`, `ruff`, `mypy`, or installed application dependencies such as LangGraph.
  - Evidence: `python3 -m ruff --version` failed with `No module named ruff`; `python3 -m mypy --version` failed with `No module named mypy`; Docker/protobuf tool discovery returned no executable.
  - Attempts made: Ran all standard-library Python tests and bytecode compilation plus `go vet ./... && go test ./...` in both Go modules.
  - Required resolution: Install Make, Docker Compose, Buf or protoc, and the Python project dependencies; then run `make validate`, generate stubs, and execute integration tests.

## Pending Implementation

### Runner first

### Orchestrator second

- [x] O2: Implement durable user, password, session, CSRF, and audit repositories
  - Completed: 2026-07-25
  - Relevant files: orchestrator/src/moirai/persistence/authentication.py, orchestrator/src/moirai/persistence/control_plane.py, orchestrator/migrations/001_initial.sql, orchestrator/tests/test_authentication.py
  - Behavior delivered: Local users use salted scrypt password hashes; login creates independently hashed opaque session and CSRF credentials with expiration, session validation/revocation updates durable state, and login/audit records exclude credential values.
  - Validation performed: `PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=src python3 -m unittest tests.test_authentication -v`; `PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=src python3 -m unittest discover -s tests -v`; `PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=src python3 -m compileall -q src tests`; and `git diff --check` passed from the repository root.
  - Notes: gRPC authorization, browser cookie/CSRF transport, bootstrap administration, and live PostgreSQL validation remain part of O3/API/security backlog work.

- [ ] O3: Implement durable project configuration CRUD and runner-token administration
  - Priority: Highest
  - Dependencies: O1, O2
  - Expected behavior: Validate project modes/configuration, create/update/disable projects, expose runner status, and issue/revoke scoped one-time registration tokens.
  - Definition of done: RPC/repository tests cover validation, disabled-project scheduling exclusion, existing-path labels, token expiry, and audit records.

- [ ] O4: Implement scheduler service, offer delivery retries, and expiration reconciliation
  - Priority: Highest
  - Dependencies: durable scheduler persistence, RunnerControl session registry
  - Current state: Added `scheduler.py` with a clock-driven tick, cancellable periodic loop, and dedicated-connection PostgreSQL advisory-lock leader lifecycle. It expires in-memory offers, schedules one candidate, runs only while leader, delivers an injected task packet, atomically rejects/releases a job when delivery fails, and guarantees advisory-lock release on shutdown or connection-query failure. AsyncPG transactionally cancels expired offers and releases their locks; expired accepted leases advance fencing, mark runners offline, move workflows to recovery, and preserve project locks. Focused tests cover delivery, rollback, expiry, leader acquire/release/failure, cancellation, shutdown cleanup, durable offer expiry, and durable lease recovery.
  - Remaining work: Add session reconnect handling and real PostgreSQL fake-clock coverage.
  - Expected behavior: Run one leader-protected scheduling loop, build task packets, deliver only to active compatible sessions, recover undelivered/expired offers, and retain project locks correctly.
  - Definition of done: Fake-clock async tests cover global ordering, delivery failure, offer expiry, session reconnect, and no duplicate project workflow.

- [ ] O5: Persist execution lifecycle into jobs, executions, workflows, and events
  - Priority: Highest
  - Dependencies: R6, O4
  - Expected behavior: Map runner events into execution rows and workflow phase transitions, reject invalid phase/event combinations, and expose durable log cursors.
  - Definition of done: Persistence tests prove event sequencing, terminal idempotency, and recovery after process restart.

- [ ] O6: Implement issue sync service and idempotent label reconciliation
  - Priority: High
  - Dependencies: project CRUD, GitHub issue adapter
  - Expected behavior: Periodically synchronize every enabled project, derive eligibility/priority/approval requirements, persist snapshots, and reconcile running/failed/delivered labels.
  - Definition of done: Fake GitHub tests cover priority ties, invalid labels, changed issues, transient retry, and idempotent label operations.

- [ ] O7: Replace placeholder LangGraph nodes with persisted task orchestration
  - Priority: High
  - Dependencies: O5, LangGraph PostgreSQL checkpointer
  - Relevant files: orchestrator/src/moirai/workflows/issue_graph.py, orchestrator/src/moirai/workflows/nodes.py
  - Current state: Added injected persisted workflow-node handlers for prepare, planning, implementation, pipeline, review, repair, push, PR creation, check wait, human wait, merge, completion, and block transitions. Check routing enforces pipeline/review gates before human approval or merge. Each dispatch node persists its phase and bounded-attempt counters through explicit persistence/dispatch ports. Added an asyncpg-backed transition/event store, versioned durable workflow checkpoints with latest-state restore validation, queued execution-request outbox with locked role attempts, and a concrete LangGraph graph factory/runtime that resumes/checkpoints state through a stable `ainvoke` contract.
  - Updated 2026-07-27: Refactored `accept_event` in control_plane.py to load current workflow status, accept an `on_transition` callback, and remove the planner→developer special case. Changed pipeline node from `_dispatch` to `_transition` (pipeline runs inline). Added `pipeline_passed` to developer-completed transition. Removed duplicate execution-request creation from persistence.py::transition(). Wired `workflow_runtime` into `RunnerControlService` via `_advance_workflow`. All 109 orchestrator tests pass.
  - Expected behavior: Drive prepare, plan, implement, local pipeline, review, repair, push, PR, checks, approval, merge, and issue completion using runner executions and deterministic routing.
  - Definition of done: Checkpoint-resume tests cover bounded attempts, fresh review context, human interrupt, and prohibition of merge before all gates pass.

- [x] O8: Integrate GitHub PR/check/merge and workflow recovery reconciliation
  - Priority: High
  - Dependencies: O6, O7, code-host adapter
  - Started: 2026-07-27
  - Completed: 2026-07-27
  - Relevant files: orchestrator/src/moirai/workflows/nodes.py, orchestrator/src/moirai/workflows/issue_graph.py, orchestrator/src/moirai/code_hosts/__init__.py, orchestrator/src/moirai/issue_trackers/__init__.py, orchestrator/src/moirai/workflows/runtime.py, orchestrator/src/moirai/main.py, orchestrator/tests/test_workflow_nodes.py
  - Behavior delivered: Added `CodeHost` protocol with create_or_find_pull_request, required_checks, merge_pull_request. Added `IssueTracker` protocol with close_issue, add_labels. Updated `PersistedWorkflowNodes` to accept optional code_host and issue_tracker; `create_pull_request` calls code_host and stores PR details; `wait_for_checks` polls required checks and sets checks_passed; `merge` calls code_host merge; `complete` closes issue and adds agent:delivered label. All fall back gracefully when adapters are absent. Wired adapters through `build_persisted_runtime` into `main.py`.
  - Expected behavior: Idempotently push/find/create PRs, poll required checks, repair failures within bounds, merge only after approval gates, complete the issue, and reconcile after restart/lease expiry.
  - Definition of done: Fake-provider workflow tests cover duplicate retries, check failure repair, human approval, merge failure, and lost-runner recovery.
  - Validation: All 152 orchestrator tests pass (17 dedicated O8 node tests); go vet and go test pass in runner/ and api/; PYTHONDONTWRITEBYTECODE=1 python3 -m compileall -q src tests

- [x] O45: Docker Compose genuinely runnable — multi-module builds, health checks, bootstrap, secrets
  - Started: 2026-07-27
  - Completed: 2026-07-27
  - Relevant files: compose.yaml, api/Dockerfile, runner/Dockerfile, orchestrator/Dockerfile, orchestrator/src/moirai/main.py, secrets/postgres_password, secrets/database_url, .env.example
  - Behavior delivered:
    - API and runner Dockerfiles now use the monorepo root as build context, correctly copying `gen/go/` alongside module sources so that the `replace github.com/loop-engineering/contracts => ../gen/go` directive resolves during `go build`.
    - orchestrator Dockerfile includes `migrations/` directory so `MigrationRunner` discovers and applies SQL during startup.
    - compose.yaml uses proper build contexts: `context: . dockerfile: api/Dockerfile` and `context: . dockerfile: runner/Dockerfile` for Go modules. All services have health checks — postgres uses `pg_isready`, orchestrator uses a Python TCP socket check on the gRPC port, API and web use `pidof` checks.
    - secrets/ directory created with `postgres_password` and `database_url` files for Docker Secrets.
    - .env.example documents every configuration variable: `RUNNER_REGISTRATION_TOKEN`, `LOOP_INITIAL_ADMIN_*`, `LOOP_SEED_*`, `LOOP_RUNNER_*`.
    - `_bootstrap_initial_setup()` in main.py automatically creates an admin user, a seed project, and a long-lived registration token when the database is fresh (no users exist). Controlled by `LOOP_INITIAL_ADMIN_USERNAME`, `LOOP_INITIAL_ADMIN_PASSWORD`, `LOOP_SEED_PROJECT_NAME`, `LOOP_SEED_PROJECT_REPOSITORY_URL`, and `LOOP_SEED_TOKEN_LABELS` environment variables.
  - Validation performed: `go build ./...` passes in `api/` with multi-module Dockerfile; `go build ./cmd/runner` passes in `runner/`; `python3 -m compileall -q src` passes in `orchestrator/`.

- [ ] O9: Add scheduler, runner, workflow, and dependency health/readiness observability
  - Priority: Medium
  - Dependencies: O1–O8
  - Expected behavior: Publish health/readiness state, structured redacted logs, and minimal metrics for queue depth, runners, leases, and workflow outcomes.
  - Definition of done: Unit/service tests cover unhealthy dependency, ready transition, graceful shutdown, and no secret leakage.

### API third

- [x] A1: Create a typed internal ControlPlane gRPC client with connection lifecycle
  - Completed: 2026-07-25
  - Relevant files: api/internal/orchestrator/client.go, api/internal/orchestrator/client_test.go, api/cmd/api/main.go
  - Behavior delivered: The API dials only the configured orchestrator with bounded startup context, maps gRPC failures to stable sentinel errors, forwards opaque session metadata, and closes the connection during shutdown.
  - Validation: `go vet ./... && go test ./...` passed from `api/`; tests cover status mapping, deadline mapping, empty endpoint rejection, and metadata propagation.

- [ ] A2: Implement secure HTTP server foundation and session middleware
  - Priority: High
  - Dependencies: A1, O2
  - Relevant files: api/internal/http/, api/internal/auth/, api/internal/orchestrator/
  - Current state: Added secure session/CSRF cookie helpers, fail-closed session and mutation CSRF middleware, a context-to-gRPC session metadata bridge, and mounted login/project/token handler seams in API bootstrap. Cookie attributes, rejection paths, and mutation protection are unit tested.
  - Expected behavior: Provide `/live`, `/ready`, versioned routing, secure cookie settings, auth/session loading, CSRF validation, JSON limits, request IDs, and safe error responses.
  - Definition of done: HTTP tests cover unauthenticated/forbidden requests, CSRF rejection, malformed JSON, cookie attributes, and error redaction.

- [ ] A3: Implement login/logout, project, runner-token, and runner-status REST endpoints
  - Priority: High
  - Dependencies: A1, A2, O3
  - Expected behavior: Translate validated REST resources to internal RPCs; reveal registration token plaintext once; never leak credentials in list/status payloads.
  - Definition of done: Handler tests cover request validation, stable models, one-time token response, and authorization.

- [ ] A4: Implement queue, workflow, approval, and control-action REST endpoints
  - Priority: High
  - Dependencies: A1, A2, O5, O7
  - Expected behavior: List queue/workflows/log cursors and provide retry, resume, cancel, block, and approval commands with audit identity.
  - Definition of done: Handler tests cover filtering/pagination, phase-valid command rejection, and action authorization.

- [ ] A5: Implement filtered workflow-event SSE gateway
  - Priority: Medium
  - Dependencies: A1, A2, O5
  - Expected behavior: Stream authorized events with event IDs, cursor resume, heartbeat, bounded client buffering, and disconnect cleanup.
  - Definition of done: HTTP streaming tests cover filtering, resume cursor, slow-client handling, and cancellation.

### UI last

- [ ] U1: Build authenticated React application shell and API client
  - Priority: Deferred until A2–A3
  - Expected behavior: Login/logout, protected routes, CSRF-aware mutations, health banner, loading/error boundaries.
  - Definition of done: Typecheck and component tests.

- [ ] U2: Build project and runner administration pages
  - Priority: Deferred until A3
  - Expected behavior: Project CRUD/configuration, one-time token display, runner capability/status view.
  - Definition of done: Form validation and API interaction tests.

- [ ] U3: Build queue, workflow, logs, approval, and control dashboard
  - Priority: Deferred until A4–A5
  - Expected behavior: Live queue/workflow views, event logs, approval and recovery controls.
  - Definition of done: SSE and action-state integration tests.

## Implementation Review (2026-07-25)

- Review scope: Inspected the source inventory across all four product components (24 Go runner files, 38 Python orchestrator files, one Go API entry point, and one React entry point), current `PROJECT.md`, protocol/schema files, and this progress record.
- Runner: The implementation covers validated task packets, secure identity persistence/registration, authenticated reconnecting control streams, offer and lease state, managed-clone and existing-path worktrees, local and Docker executors, OpenCode invocation, and dispatch artifacts. The executable currently starts only the control stream (`runner/cmd/runner/main.go`); R6 remains required to connect accepted leases to dispatch and generation-fenced lifecycle events.
- Orchestrator: The implementation covers configuration, in-memory and asyncpg control-plane seams, registration/credential validation, scheduling/locks/offers/leases/event fencing, runner session delivery, GitHub issue/code-host adapters, and deterministic workflow-gate policy. The process mounts only `RunnerControl` (`orchestrator/src/moirai/main.py`); internal control-plane RPCs, durable workflow execution, scheduler service, authentication administration, and recovery reconciliation remain pending.
- API: The service currently provides only unauthenticated `/live` and `/ready` responses (`api/cmd/api/main.go`). It has no internal gRPC client, public `/api/v1` routes, authentication, session/CSRF middleware, REST resources, or SSE gateway.
- Web UI: The application currently renders a single health-status screen (`web/src/main.tsx`). It has no authenticated routes, typed API client, configuration forms, administration pages, workflow views, live logs, or control actions.
- Progress-record assessment: The active R6/R7 and O1–O9, A1–A5, U1–U3 roadmap accurately reflects the implementation boundary; this review adds a separately numbered 20-task expansion for each component below.

## Expanded Delivery Backlog

The following are additional implementation tasks discovered during the 2026-07-25 implementation review. They refine, rather than replace, the active R6 → R7 → O1–O9 → A1–A5 → U1–U3 dependency order.

### Runner — 20 additional tasks

- [x] R8: Add persistent event-outbox files with atomic writes and startup replay.
  - Completed: 2026-07-26
  - Relevant files: runner/internal/control/outbox.go, runner/internal/control/events.go, runner/internal/control/events_test.go, runner/internal/dispatch/control_loop.go, runner/internal/config/config.go, runner/cmd/runner/main.go
  - Behavior delivered: Every unsent lifecycle event is atomically persisted with private permissions under `<data-dir>/outbox/events.json`; it is replayed during the next runner startup before new transport work. Successful sends remove the persisted entry, corrupt/unsupported outbox data fails closed, and terminal events are retained for replay after the active lease completes.
  - Validation: `go test ./internal/control ./internal/dispatch ./internal/config ./cmd/runner -v`; `go test -race ./...`; `go vet ./...`; and `git diff --check` passed from `runner/`.
- [x] R9: Add event payload size limits, log chunking, and UTF-8-safe truncation.
  - Completed: 2026-07-25
  - Relevant files: runner/internal/control/events.go, runner/internal/control/events_test.go
  - Behavior delivered: Existing 16 KiB event limit is preserved; `EmitLog` chunks large messages into ordered UTF-8 boundary-safe 6 KiB fragments with explicit chunk metadata and preserves fence sequencing.
  - Validation: `gofmt -w internal/control/events.go internal/control/events_test.go && go test ./internal/control && go vet ./...` passed from `runner/`.
- [x] R10: Add redaction rules for environment values, tokens, and configured secret patterns.
  - Completed: 2026-07-26
  - Relevant files: runner/internal/control/events.go, runner/internal/control/events_test.go, runner/internal/config/config.go, runner/internal/config/config_test.go, runner/internal/dispatch/control_loop.go, runner/cmd/runner/main.go
  - Behavior delivered: Event payload maps redact sensitive key names recursively, known token prefixes, and operator-configured prefixes supplied through `LOOP_RUNNER_REDACTION_PREFIXES`; every dispatched event, including executor-derived logs, uses the configured reporter redaction path before transport.
  - Validation: `gofmt -w cmd/runner/main.go internal/config/config.go internal/config/config_test.go internal/control/events.go internal/control/events_test.go internal/dispatch/control_loop.go && go test ./internal/control ./internal/config ./internal/dispatch && go test -race ./internal/control ./internal/config ./internal/dispatch && go vet ./... && git diff --check` passed from `runner/`.
- [ ] R11: Add protocol handling for `StartExecution` commands distinct from job-offer admission.
- [ ] R12: Add protocol handling for `CancelExecution` with idempotent acknowledgements.
- [ ] R13: Add protocol handling for `Drain`, including persisted draining state across restarts.
- [ ] R14: Add protocol handling for credential-rotation commands and atomic credential replacement.
- [ ] R15: Send capability-change messages when executor, backend, or repository availability changes.
- [x] R16: Add startup health checks for Git, configured agent backend, Docker mode, and writable data paths.
  - Completed: 2026-07-26
  - Relevant files: runner/cmd/runner/main.go, runner/internal/config/config.go, runner/internal/config/config_test.go, runner/internal/health/prerequisites.go, runner/internal/health/prerequisites_test.go
  - Behavior delivered: Runner fails closed before dialing when Git, OpenCode, a writable private data directory, or Docker for explicitly enabled Docker mode are unavailable. `LOOP_RUNNER_DOCKER_ENABLED` selects Docker prerequisite validation.
  - Validation: `gofmt -w cmd/runner/main.go internal/config/config.go internal/config/config_test.go && go test ./cmd/runner ./internal/config ./internal/health ./internal/control ./internal/dispatch && go test -race ./internal/config ./internal/health ./internal/control ./internal/dispatch && go vet ./... && git diff --check` passed from `runner/`.
- [x] R17: Add runner disk-space monitoring and reject work before workspace allocation is unsafe.
  - Completed: 2026-07-25
  - Relevant files: runner/internal/health/disk.go, runner/internal/config/config.go, runner/internal/dispatch/dispatch.go, runner/cmd/runner/main.go
  - Behavior delivered: Runner enforces configurable minimum free capacity at startup and again immediately before workspace preparation, rejecting unsafe work before Git/artifact side effects.
  - Validation: `gofmt -w cmd/runner/main.go internal/config/config.go internal/config/config_test.go internal/health/disk.go internal/health/disk_test.go internal/dispatch/dispatch.go internal/dispatch/dispatch_test.go && go test ./internal/dispatch ./internal/health ./internal/config && go vet ./...` passed from `runner/`.
- [x] R18: Add workspace retention policies for successful, failed, and abandoned executions.
  - Completed: 2026-07-26
  - Relevant files: runner/internal/config/config.go, runner/internal/config/config_test.go, runner/cmd/runner/main.go, runner/internal/dispatch/dispatch.go, runner/internal/dispatch/dispatch_test.go
  - Behavior delivered: `LOOP_RUNNER_RETAIN_WORKSPACES` accepts a unique comma-separated selection of `succeeded`, `failed`, and `abandoned`. The dispatcher applies the policy after execution, retaining matching workspace state while keeping the default cleanup behavior for all other outcomes.
  - Validation: `gofmt -w cmd/runner/main.go internal/config/config.go internal/config/config_test.go internal/dispatch/dispatch.go internal/dispatch/dispatch_test.go && go test ./cmd/runner ./internal/config ./internal/dispatch && go test -race ./... && go vet ./... && git diff --check` passed from `runner/`.
- [x] R19: Add bounded cleanup retries and quarantine records for workspaces that cannot be removed.
  - Completed: 2026-07-26
  - Relevant files: runner/internal/repository/manager.go, runner/internal/repository/manager_test.go
  - Behavior delivered: Repository workspace cleanup retries a bounded number of times (default three) with an injectable delay. Exhausted cleanup failures atomically persist a job-scoped JSON quarantine record under the runner data directory while preserving the workspace for operator inspection.
  - Validation: `gofmt -w internal/repository/manager.go internal/repository/manager_test.go && go test ./internal/repository && go test -race ./internal/repository && go vet ./... && git diff --check` passed from `runner/`.
- [x] R20: Add branch-name generation and validation based on issue and workflow identifiers.
  - Completed: 2026-07-26
  - Relevant files: runner/internal/repository/branch.go, runner/internal/repository/branch_test.go, runner/internal/taskpacket/taskpacket.go, runner/internal/dispatch/dispatch.go, runner/internal/dispatch/dispatch_test.go
  - Behavior delivered: A task packet can omit its branch; dispatch deterministically generates a Git-safe branch from its issue external ID and workflow/execution ID before repository preparation. Explicit safe branches remain supported and unsafe explicit branches are rejected.
  - Validation: `gofmt -w internal/taskpacket/taskpacket.go internal/dispatch/dispatch.go internal/dispatch/dispatch_test.go && go test ./internal/taskpacket ./internal/dispatch && go test -race ./internal/taskpacket ./internal/dispatch && go vet ./... && git diff --check` passed from `runner/`.
- [x] R21: Add pre-execution repository revision capture and post-execution diff/commit summaries.
  - Completed: 2026-07-26
  - Relevant files: runner/internal/repository/manager.go, runner/internal/dispatch/dispatch.go, runner/internal/dispatch/dispatch_test.go, runner/cmd/runner/main.go
  - Behavior delivered: The production dispatcher captures Git HEAD and porcelain workspace changes immediately before and after the agent backend runs. Terminal results now retain initial/final revisions and merge backend-reported changes with repository-observed changed paths.
  - Validation: `gofmt -w cmd/runner/main.go internal/repository/manager.go internal/dispatch/dispatch.go internal/dispatch/dispatch_test.go && go test ./cmd/runner ./internal/repository ./internal/dispatch && go test -race ./... && go vet ./... && git diff --check` passed from `runner/`.
- [x] R22: Add deterministic local-pipeline command execution with per-command timeout results.
  - Completed: 2026-07-26
  - Relevant files: runner/internal/pipeline/pipeline.go, runner/internal/pipeline/pipeline_test.go, runner/internal/taskpacket/taskpacket.go, runner/internal/taskpacket/taskpacket_test.go, runner/internal/dispatch/dispatch.go, runner/internal/dispatch/dispatch_test.go
  - Behavior delivered: Validated task packets may define up to 32 sequential local pipeline commands with bounded individual timeouts. The runner records command output, duration, exit code, and timeout status; it stops deterministically on the first failure and fails the terminal result while preserving completed command results.
  - Validation: `gofmt -w internal/pipeline/pipeline.go internal/pipeline/pipeline_test.go internal/taskpacket/taskpacket.go internal/taskpacket/taskpacket_test.go internal/dispatch/dispatch.go internal/dispatch/dispatch_test.go && go test ./internal/pipeline ./internal/taskpacket ./internal/dispatch && go test -race ./... && go vet ./... && git diff --check` passed from `runner/`.
- [x] R23: Add a generic CLI agent backend implementing the portable backend contract.
  - Completed: 2026-07-26
  - Relevant files: runner/internal/agents/cli.go, runner/internal/agents/cli_test.go, runner/internal/config/config.go, runner/internal/config/config_test.go, runner/internal/health/prerequisites.go, runner/cmd/runner/main.go
  - Behavior delivered: A portable `CLIBackend` implements the backend contract with configured binary/arguments, supervised execution, cancellation, structured result validation, and isolated logs. `LOOP_RUNNER_AGENT_BACKEND=cli` plus `LOOP_RUNNER_AGENT_BINARY` selects it and startup health checks the selected executable instead of assuming OpenCode.
  - Validation: `gofmt -w cmd/runner/main.go internal/agents/cli.go internal/agents/cli_test.go internal/config/config.go internal/config/config_test.go internal/health/prerequisites.go && go test ./cmd/runner ./internal/agents ./internal/config ./internal/health && go test -race ./... && go vet ./... && git diff --check` passed from `runner/`.
- [x] R24: Add a Docker CLI agent backend with image, mount, and network policy validation.
  - Completed: 2026-07-26
  - Relevant files: runner/internal/agents/docker.go, runner/internal/agents/docker_test.go, runner/internal/execution/docker.go, runner/internal/config/config.go, runner/internal/config/config_test.go, runner/cmd/runner/main.go
  - Behavior delivered: `DockerCLIBackend` runs a structured-result CLI agent inside the existing restricted Docker executor, retaining bind-mounted workspace isolation, default `none` network policy, image validation, timeout/cancellation handling, and result/log collection. `LOOP_RUNNER_AGENT_BACKEND=docker` plus `LOOP_RUNNER_AGENT_DOCKER_IMAGE` selects it and forces Docker prerequisite validation.
  - Validation: `gofmt -w cmd/runner/main.go internal/agents/docker.go internal/agents/docker_test.go internal/config/config.go internal/config/config_test.go && go test ./cmd/runner ./internal/agents ./internal/config ./internal/execution ./internal/health && go test -race ./... && go vet ./... && git diff --check` passed from `runner/`.
- [x] R25: Add executor resource-accounting events for duration, exit state, and bounded usage metadata.
  - Completed: 2026-07-26
  - Relevant files: runner/internal/dispatch/control_loop.go, runner/internal/dispatch/control_loop_test.go
  - Behavior delivered: Every terminal runner event now reports bounded execution accounting fields: duration milliseconds, exit state, changed-file count, command count, and pipeline-command count. Existing terminal payloads retain their status and path/command details without exposing agent summaries.
  - Validation: `gofmt -w internal/dispatch/control_loop.go internal/dispatch/control_loop_test.go && go test ./internal/dispatch && go test -race ./... && go vet ./... && git diff --check` passed from `runner/`.
- [x] R26: Add runner structured JSON logging with execution, job, lease, and correlation identifiers.
  - Completed: 2026-07-26
  - Relevant files: runner/cmd/runner/main.go, runner/internal/dispatch/control_loop.go, runner/internal/dispatch/control_loop_test.go
  - Behavior delivered: The runner process configures JSON `slog` output and records startup, offer receipt, acknowledgement, execution start, terminal outcome, cancellation, lease expiry, and dispatch-loop failures with runner/job/execution/lease correlation fields without logging agent result summaries or raw transport errors.
  - Validation: `go test ./cmd/runner ./internal/dispatch -v` passed from `runner/`; structured-log assertions cover job and lease fields.
- [x] R27: Add runner `/live` and `/ready` HTTP health endpoints or an equivalent health probe command.
  - Completed: 2026-07-26
  - Relevant files: runner/cmd/runner/main.go, runner/cmd/runner/main_probe_test.go
  - Behavior delivered: `runner live` exits successfully without loading configuration; `runner ready` validates configuration, writable data paths, Git/agent/Docker prerequisites, and minimum free disk capacity before returning JSON readiness.
  - Validation: `go run ./cmd/runner live`; `go test ./cmd/runner ./internal/dispatch -v`; `go test -race ./...`; and `go vet ./...` passed from `runner/`; tests cover liveness, invalid commands, and fail-closed readiness configuration.

### Orchestrator — 20 additional tasks

- [x] O10: Add a PostgreSQL advisory-lock scheduler leadership guard with loss detection.
  - Completed: 2026-07-27
  - Relevant files: orchestrator/src/moirai/scheduler.py, orchestrator/tests/test_scheduler_service.py
  - Behavior delivered: Fixed `_held` caching bug that would mask connection-recycle lock loss (`_held = bool(acquired)` replaces `_held = bool(acquired) or self._held`). Each `is_leader()` call re-checks the advisory lock via `pg_try_advisory_lock`, so a lost lock (connection drop, pool recycle, or another session acquiring it) is detected on the next tick. Added `leadership_epoch` counter that increments on every leader transition (non-leader → leader), providing downstream consumers with a detectable leadership-change signal.
  - Validation: 6 dedicated tests cover acquisition, repeated same-session re-acquisition, lock loss detection, epoch increment on re-acquisition, query-failure connection cleanup, and lock release on close. All 152+ orchestrator tests pass.
- [ ] O11: Add scheduler wake-up notifications for issue sync, runner availability, and offer expiry.
- [ ] O12: Add project pause and global maintenance-mode persistence and scheduling exclusion.
- [ ] O13: Add runner disable, drain, revoke, delete, and credential-rotation command services.
- [ ] O14: Add runner capability snapshots and compatibility diagnostics for each project.
- [ ] O15: Add durable execution-log chunk storage with sequence, cursor, and retention bounds.
- [ ] O16: Add deterministic execution-event-to-workflow transition validation.
- [ ] O17: Add job cancellation, retry, resume, and block commands with phase-aware authorization.
- [ ] O18: Add durable workflow-event publication for internal subscribers and API streaming.
- [ ] O19: Add project configuration validation for labels, repository modes, pipeline commands, and budgets.
- [ ] O20: Add project configuration versioning and safe workflow snapshots at offer creation.
- [ ] O21: Add durable issue snapshot upsert, stale-issue detection, and deleted/closed issue reconciliation.
- [ ] O22: Add GitHub CLI transient-error classification, retry policy, and provider circuit breaker.
- [ ] O23: Add idempotent issue state-label reconciliation for workflow phase changes.
- [ ] O24: Add task-packet construction from workflow state and execution-phase requests.
- [ ] O25: Add result-document validation and mapping into structured workflow artifacts.
- [ ] O26: Add workflow no-progress, age, repeated-failure, and total-execution budget enforcement.
- [x] O27: Add PostgreSQL-backed LangGraph checkpoint initialization and checkpoint migration handling.
  - Completed: 2026-07-27
  - Relevant files: orchestrator/src/moirai/persistence/migrations.py, orchestrator/migrations/002_langgraph_checkpointer.sql, orchestrator/src/moirai/main.py, orchestrator/tests/test_migrations.py
  - Behavior delivered: `MigrationRunner` discovers numbered SQL files from `migrations/`, applies them in order, and tracks completed migrations in `app.schema_version`. Wired into orchestrator startup before any database operations. Migration 002 creates the LangGraph checkpointer tables (`checkpoints`, `checkpoint_writes`) in the `langgraph` schema. `_build_checkpointer` in main.py builds an `AsyncPostgresSaver` with a psycopg connection pool.
- [ ] O28: Add workflow recovery reconciliation for a restart during offer, execution, checks, approval, or merge.
- [ ] O29: Add audit events for authentication, configuration, runner administration, and workflow control actions.

### API — 20 additional tasks

- [ ] A6: Add API runtime configuration validation for bind address, orchestrator endpoint, cookie keys, and limits.
  - Current state: Added validated bind/orchestrator endpoint configuration, cookie-secure boolean parsing, optional validated cookie key material, and bounded request body limits in `api/internal/config`; API bootstrap applies bind/body limits and fails closed on invalid runtime values.
  - Remaining work: Consume the orchestrator endpoint in client bootstrap and use cookie key material for the session middleware.
  - Validation: `gofmt -w cmd/api/main.go internal/config/config.go internal/config/config_test.go internal/http/server.go && go vet ./... && go test ./...` passed from `api/`.
- [x] A7: Add request-ID, structured access-log, panic-recovery, and safe error middleware.
  - Completed: 2026-07-25
  - Relevant files: api/internal/http/server.go, api/internal/http/middleware_test.go
  - Behavior delivered: Middleware preserves valid caller IDs or emits cryptographically random request IDs, applies anti-sniff/frame headers and body limits, writes structured method/path/status/duration logs, and recovers panics with redacted problem responses.
  - Validation: `gofmt -w internal/http/server.go internal/http/middleware_test.go && go vet ./... && go test ./...` passed from `api/`.
- [x] A8: Add per-session and per-IP rate limiting for authentication and mutating routes.
  - Completed: 2026-07-25
  - Relevant files: api/internal/auth/rate_limit.go, api/internal/auth/rate_limit_test.go, api/internal/http/handlers/
  - Behavior delivered: Fixed-window IP limits protect login; session-keyed limits protect all CSRF-protected mutations. Both return bounded 429 responses and are concurrency-safe.
  - Validation: `gofmt -w internal/auth/rate_limit.go internal/auth/rate_limit_test.go internal/http/handlers/handlers.go && go vet ./... && go test ./...` passed from `api/`.
- [ ] A9: Add secure password-login and session-refresh REST contracts.
- [ ] A10: Add current-user and session-expiry REST endpoints.
- [ ] A11: Add application-health and version REST endpoints backed by orchestrator readiness.
- [ ] A12: Add project list/detail REST queries with stable public models.
- [ ] A13: Add project create/update/enable/disable REST commands with validation mapping.
- [ ] A14: Add runner registration-token create/list/revoke REST endpoints.
- [ ] A15: Add runner list/detail, capability, and drain/disable/revoke REST endpoints.
- [ ] A16: Add global queue list and queue-item detail REST endpoints with filtering and pagination.
- [ ] A17: Add workflow list/detail and artifact-summary REST endpoints.
- [ ] A18: Add workflow event-log cursor REST endpoint with redacted payload mapping.
- [ ] A19: Add workflow retry, resume, cancel, and block REST command endpoints.
- [ ] A20: Add workflow human-approval decision REST endpoint with CSRF protection.
- [ ] A21: Add application-settings query/update REST endpoints with audit identity propagation.
- [ ] A22: Add strict JSON content-type, body-size, unknown-field, and pagination validation middleware.
- [ ] A23: Add public API error-code and RFC-compatible problem response mapping.
- [ ] A24: Add orchestrator client connection-health tracking and bounded retry behavior for safe reads.
- [ ] A25: Add SSE authentication refresh, event authorization filtering, and Last-Event-ID validation.

### Web UI — 20 additional tasks

- [ ] U4: Add Vite runtime API-base configuration and production-safe environment validation.
- [ ] U5: Add typed REST client models, request cancellation, CSRF handling, and problem-response parsing.
- [ ] U6: Add application router with protected-route redirects and not-found handling.
- [ ] U7: Add accessible login form with validation, loading state, and authentication failure handling.
- [ ] U8: Add logout action and session-expiry redirect behavior.
- [ ] U9: Add persistent application layout with navigation, health banner, and error boundary.
- [ ] U10: Add project-list page with status, repository mode, and scheduling summary.
- [ ] U11: Add project-create form for managed-clone and existing-path repository modes.
- [ ] U12: Add project-edit form for issue labels, runner labels, budgets, and merge policy.
- [ ] U13: Add project enable/disable action with confirmation and immediate status refresh.
- [ ] U14: Add one-time runner-token creation dialog that prevents accidental token redisplay.
- [ ] U15: Add runner fleet page with connectivity, health, capacity, capabilities, and drain state.
- [ ] U16: Add runner administrative actions with confirmation, authorization errors, and optimistic-state rollback.
- [ ] U17: Add global queue page with priority, eligibility, lock, runner-match, and issue-link information.
- [ ] U18: Add workflow-list page with phase, attempt counters, runner, and terminal outcome filters.
- [ ] U19: Add workflow-detail timeline with phase transitions, artifacts, and recovery context.
- [ ] U20: Add live log viewer using SSE with cursor resume, bounded rendering, and reconnect indication.
- [ ] U21: Add workflow retry, resume, cancel, and block controls guarded by valid action state.
- [ ] U22: Add human-approval panel with approve, request-changes, and reject decision paths.
- [ ] U23: Add settings and system-health page with dependency readiness and safe configuration summaries.

## Additional Delivery Backlog

Following the existing component-oriented backlog, these are 15 further implementation tasks for each requested component.

### Runner — 15 further tasks

- [x] R28: Add configurable TLS certificate authority, client-certificate, and server-name handling for remote runner control endpoints.
  - Completed: 2026-07-26
  - Relevant files: runner/internal/config/config.go, runner/internal/config/config_test.go, runner/internal/control/dialer.go, runner/internal/control/dialer_test.go, runner/cmd/runner/main.go
  - Behavior delivered: `LOOP_ORCHESTRATOR_TLS_CA_FILE`, `LOOP_ORCHESTRATOR_TLS_CLIENT_CERT_FILE`, `LOOP_ORCHESTRATOR_TLS_CLIENT_KEY_FILE`, and `LOOP_ORCHESTRATOR_TLS_SERVER_NAME` now configure verified TLS. Defaults retain system trust roots; a configured CA replaces roots, client credentials are loaded atomically as a pair, and malformed/incomplete/unsecured settings fail before connection.
  - Validation: `go test ./internal/config ./internal/control ./cmd/runner -v`; `go test -race ./...`; `go vet ./...`; and `git diff --check` passed from `runner/`.
- [ ] R29: Add runner configuration reload for non-secret operational settings without interrupting an active job.
- [x] R30: Add validated environment allowlists and execution-mode-specific environment construction.
  - Completed: 2026-07-26
  - Relevant files: runner/internal/config/config.go, runner/internal/config/config_test.go, runner/internal/dispatch/dispatch.go, runner/internal/dispatch/dispatch_test.go, runner/internal/execution/local.go, runner/internal/execution/local_test.go, runner/internal/execution/docker_test.go, runner/cmd/runner/main.go
  - Behavior delivered: `LOOP_RUNNER_ALLOWED_ENVIRONMENT` is a validated uppercase-name allowlist. Task environment references fail closed unless explicitly allowed; resolver output must exactly match allowed requested names. Local-process agents receive only a minimal PATH/HOME/TMPDIR baseline plus resolved task values, while Docker receives only explicit `--env` values and never needs runner host environment.
  - Validation: `go test ./internal/config ./internal/dispatch ./internal/execution ./cmd/runner -v`; `go test -race ./...`; `go vet ./...`; and `git diff --check` passed from `runner/`.
- [x] R31: Add per-project workspace concurrency guards that defend against duplicate commands after protocol retries.
  - Completed: 2026-07-26
  - Relevant files: runner/internal/dispatch/dispatch.go, runner/internal/dispatch/dispatch_test.go, runner/cmd/runner/main.go
  - Behavior delivered: A shared runner-process project guard wraps dispatch from pre-workspace preparation through terminal cleanup. It rejects a concurrent duplicate project execution and guarantees release on all dispatcher return paths.
  - Validation: `go test ./internal/dispatch ./cmd/runner -v`; `go test -race ./...`; `go vet ./...`; and `git diff --check` passed from `runner/`.
- [x] R32: Add repository cache locking to prevent concurrent clone, fetch, or maintenance operations.
  - Completed: 2026-07-26
  - Relevant files: runner/internal/repository/manager.go, runner/internal/repository/manager_test.go
  - Behavior delivered: Managed-clone and existing-path repository operations acquire hashed, private, context-aware advisory file locks under `<data-dir>/locks`. Locks cover clone/fetch/worktree preparation and cleanup, prevent cross-process cache mutation races, release on every return path, and honor caller cancellation while waiting.
  - Validation: `go test ./internal/repository ./internal/dispatch ./cmd/runner`; `go test -race ./...`; `go vet ./...`; and `git diff --check` passed from `runner/`.
- [x] R33: Add repository cache integrity checks and safe re-clone recovery after Git corruption.
  - Completed: 2026-07-26
  - Relevant files: runner/internal/repository/manager.go, runner/internal/repository/manager_test.go
  - Behavior delivered: Existing managed mirrors run `git fsck --no-dangling` before fetch. A failed integrity check discards the cache while holding its repository lock, reclones the configured origin, then resumes normal fetch/worktree preparation.
  - Validation: `go test ./internal/repository -v`; `go test -race ./...`; `go vet ./...`; and `git diff --check` passed from `runner/`.
- [ ] R34: Add commit, push, and branch-cleanup execution adapters with structured outcomes.
- [x] R35: Add a safe command-template parser for configured local pipelines without shell interpolation.
  - Completed: 2026-07-26
  - Relevant files: runner/internal/pipeline/pipeline.go, runner/internal/pipeline/pipeline_test.go, runner/internal/dispatch/dispatch_test.go
  - Behavior delivered: Pipeline templates are tokenized into direct argument vectors and executed without `sh -c`. Shell chaining, redirection, substitution, quoting, control characters, and oversized templates are rejected before execution.
  - Validation: `go test ./internal/pipeline -v`; `go test -race ./...`; `go vet ./...`; and `git diff --check` passed from `runner/`.
- [x] R36: Add failure fingerprints for executor, backend, pipeline, Git, and workspace errors.
  - Completed: 2026-07-26
  - Relevant files: runner/internal/dispatch/fingerprint.go, runner/internal/dispatch/fingerprint_test.go, runner/internal/dispatch/control_loop.go
  - Behavior delivered: Failed execution terminal events include stable component-qualified SHA-256-derived fingerprints. Error text is normalized and secret-bearing suffixes are excluded before hashing, avoiding raw failure details on the control stream.
  - Validation: `go test ./internal/dispatch -v`; `go test -race ./...`; `go vet ./...`; and `git diff --check` passed from `runner/`.
- [ ] R37: Add no-progress watchdogs based on execution output and heartbeat activity.
- [ ] R38: Add bounded stdout/stderr artifact retention and content-addressed checksum metadata.
- [ ] R39: Add execution manifest persistence for process/container identifiers and recovery diagnostics.
- [ ] R40: Add restart recovery that reconciles persisted active executions with surviving local processes or containers.
- [ ] R41: Add runner self-update notification handling that drains safely before a version change.
- [ ] R42: Add runner metrics for control connectivity, offers, execution duration, cleanup failures, and resource pressure.

### Orchestrator — 15 further tasks

- [ ] O30: Add internal ControlPlane RPC request/response schemas for authentication, projects, runners, queue, workflows, and settings.
- [ ] O31: Add internal ControlPlane service authorization boundaries for API-originated administrative actions.
- [ ] O32: Add query repositories for runner status, global queue, workflow detail, logs, and system health.
- [ ] O33: Add cursor pagination, stable sort ordering, and filter validation to control-plane query services.
- [ ] O34: Add persistent project secret-reference validation without resolving secret plaintext into workflow state.
- [ ] O35: Add application setting persistence for maintenance mode, scheduler cadence, and retention policy.
- [ ] O36: Add delivery-attempt records and exponential backoff for undelivered runner offers.
- [ ] O37: Add orphaned-session detection and runner offline transitions after heartbeat grace expires.
- [ ] O38: Add runner-lease expiry scanning and recovery-job creation with a new fencing generation.
- [ ] O39: Add workflow artifact persistence for plans, results, pipelines, reviews, and failure reports.
- [ ] O40: Add independent-review task-packet generation that excludes developer conversation context.
- [ ] O41: Add pipeline result aggregation and deterministic repair-routing input construction.
- [ ] O42: Add PR polling schedules, check timeout policy, and bounded CI-repair dispatch.
- [ ] O43: Add human-approval timeout, reminder, resume, and rejection workflow handling.
- [ ] O44: Add retention cleanup for terminal workflow events, execution logs, expired tokens, and revoked sessions.

### API — 15 further tasks

- [ ] A26: Add OpenAPI description generation or a checked-in API contract for all public REST and SSE endpoints.
- [ ] A27: Add API compatibility tests that prevent accidental public response-model changes.
- [ ] A28: Add origin checks, CORS policy, and secure reverse-proxy header handling.
- [ ] A29: Add secure cookie key rotation and clear-session behavior for invalid or expired credentials.
- [ ] A30: Add audit-correlation middleware that propagates authenticated user and request identity to RPC commands.
- [ ] A31: Add endpoint-level permission checks that preserve an MVP administrator-only authorization policy.
- [ ] A32: Add conditional GET, cache-control, and ETag handling for safe read-only resources.
- [ ] A33: Add long-polling or retry-after behavior for transiently unavailable orchestrator query paths.
- [ ] A34: Add consistent UTC timestamp, duration, and identifier serialization in public response mappers.
- [ ] A35: Add request validation error localization fields without exposing internal validation implementation.
- [ ] A36: Add configurable trusted-proxy and HTTPS enforcement behavior for self-hosted deployments.
- [ ] A37: Add API metrics for request rate, latency, errors, authenticated sessions, and SSE clients.
- [ ] A38: Add API readiness checks that require a healthy internal gRPC connection instead of unconditional success.
- [ ] A39: Add graceful HTTP shutdown that drains SSE clients and closes the orchestrator client cleanly.
- [ ] A40: Add endpoint fuzz tests for JSON decoding, identifier parsing, CSRF checks, and pagination bounds.

### Web UI — 15 further tasks

- [ ] U24: Add a shared accessible form-control library for text, select, checkbox, numeric, and array fields.
- [ ] U25: Add client-side route-level code splitting and loading skeletons for administration views.
- [ ] U26: Add request retry and stale-data indicators for safe query failures without repeating mutations.
- [ ] U27: Add an application-wide toast and inline-problem presentation system with accessible announcements.
- [ ] U28: Add unsaved-change protection for project and settings forms.
- [ ] U29: Add project-configuration import/export previews with strict client-side schema validation.
- [ ] U30: Add runner capability and project requirement comparison views with compatibility explanations.
- [ ] U31: Add queue filtering, deterministic priority sorting, and URL-synchronized pagination state.
- [ ] U32: Add workflow artifact viewers for plan, result, pipeline, review, and failure documents with redaction labels.
- [ ] U33: Add log search, follow-tail toggle, virtualized rendering, and download control for authorized logs.
- [ ] U34: Add workflow action confirmations that show expected effects and terminal-state restrictions.
- [ ] U35: Add responsive layouts and keyboard navigation for dashboard tables, timelines, and dialogs.
- [ ] U36: Add frontend error telemetry that excludes credentials, task packet secrets, and raw log payloads.
- [ ] U37: Add component tests for authentication, project forms, runner actions, queue filtering, and workflow controls.
- [ ] U38: Add browser-level workflow tests using mocked REST/SSE contracts for login, configuration, approval, and recovery paths.

## Quality Backlog

- [ ] Add orchestrator liveness/readiness endpoints and graceful shutdown once the gRPC server exists
  - Category: Observability and reliability
  - Risk or problem: The current process has no independently verifiable readiness state and waits indefinitely.
  - Expected benefit: Compose and operators can distinguish process liveness from database/control-plane readiness and safely stop the service.
  - Suggested validation: grpc.aio integration tests for health state transitions and SIGTERM shutdown.

- [ ] Validate the corrected orchestrator wheel build after freeing Docker storage
  - Category: Deployment reliability
  - Risk or problem: The Dockerfile now copies source before installation so the wheel contains the application, but the validation rebuild exhausted the 9.8 GB filesystem (`0B` free).
  - Expected benefit: Confirms the packaged image runs without source bind mounts.
  - Suggested validation: free Docker cache/images after human approval, run `docker build -t loop-engineering-orchestrator:dev ./orchestrator`, then run the installed-package import command.

## Decisions

- Decision: Complete runner execution transport before expanding orchestrator administration, API, or UI.
  - Context: Review found the runner executable is still an infinite placeholder while runner primitives, durable offers, and session delivery already exist.
  - Alternatives considered: Start API/UI scaffolding now or implement broad LangGraph nodes first.
  - Reason: A live runner stream and event lifecycle unblock the end-to-end execution contract that orchestrator workflows and public APIs must expose.
  - Consequences: Pending work is explicitly ordered R1–R7, O1–O9, A1–A5, then U1–U3; UI has no priority until its API dependencies exist.

- Decision: Keep GitHub CLI behavior behind an injected async command runner.
  - Context: GitHub CLI and credentials are unavailable in the implementation environment, but issue synchronization and idempotent command construction need deterministic coverage.
  - Alternatives considered: Invoke `gh` directly from domain code or defer adapter work until a credentialed environment exists.
  - Reason: The adapter isolates provider-specific JSON/CLI mapping, enforces timeouts, avoids shell construction, redacts common token prefixes, and supports fake-command contract tests.

- Decision: Express workflow gates and retry limits as dependency-free policy functions.
  - Context: LangGraph is not installed, while bounded routing and human approval are core workflow invariants.
  - Alternatives considered: Embed all routing inside untestable LangGraph nodes or defer behavior entirely.
  - Reason: The policy functions are directly testable and can be consumed by LangGraph nodes without coupling workflow truth to the agent backend.

- Decision: Use a pure-Python domain core with repository ports and optional LangGraph graph construction.
  - Context: The repository was empty and database/LangGraph services are unavailable locally.
  - Alternatives considered: Couple domain logic directly to a database or defer all implementation until containers are available.
  - Reason: Pure domain logic enables deterministic unit testing while retaining PostgreSQL ownership in the orchestrator adapter.

- Decision: Preserve strict service ownership in the foundation.
  - Context: PROJECT.md requires PostgreSQL access only through the orchestrator and a separate API-to-orchestrator boundary.
  - Alternatives considered: Direct API database access or a combined backend.
  - Reason: Compose network topology, service modules, and protobuf contracts retain the required boundary for subsequent implementation.

- Decision: Require explicit, validated secret configuration before orchestrator startup.
  - Context: Compose supplies the database URL through a Docker secret file, while accidental direct environment configuration and unvalidated bind endpoints risk secret leakage or a silently broken service.
  - Alternatives considered: Read environment variables inline in `main.py` or defer validation to the asyncpg connection attempt.
  - Reason: A dependency-free configuration boundary fails fast without including secret values in errors or logs and works both in Compose and local development.
  - Consequences: The orchestrator now requires `LOOP_DATABASE_URL` or `LOOP_DATABASE_URL_FILE` even before the persistence adapter is connected.

- Decision: Model registration, runner capacity, offers, project locks, and leases together in a dependency-free control-plane reference.
  - Context: Runtime dependencies and Go/protobuf tooling are unavailable, while these invariants are required before connecting transport and persistence.
  - Alternatives considered: Implement only SQL schema or defer all behavior to asyncpg/gRPC services.
  - Reason: The in-memory implementation gives deterministic coverage for security-sensitive token exchange and scheduling recovery invariants; it can guide PostgreSQL repository transactions without violating service ownership.

- Decision: Keep runner repository lifecycle limited to validated managed-clone and explicitly mounted existing-path sources.
  - Context: Job IDs, repository URLs, branches, and local repository paths enter a process that invokes Git and creates filesystem paths.
  - Alternatives considered: Shell-based Git commands, unrestricted caller paths, or direct shared repository execution.
  - Reason: Validated identifiers and refs, absolute control-character-free local paths, resolved symlinks, and data-directory-contained workspaces prevent traversal and option injection while dedicated worktrees preserve project isolation.
  - Consequences: Existing-path cleanup requires the configured mounted source explicitly; retention scheduling remains pending; all Git calls remain argument vectors.

- Decision: Authenticate every runner-to-orchestrator stream message and retain runner identity for the stream lifetime.
  - Context: The bidirectional protocol carries runner credentials on every message and must not accept a credential-switching stream or bypass lease fencing.
  - Alternatives considered: Authenticate only the first message or defer stream validation until durable repositories exist.
  - Reason: Per-message authentication is simple, supports reconnect safety, and makes in-memory transport behavior match the durable control-plane security boundary.
  - Consequences: Accepted and renewed leases are returned only through the authenticated runner session.

- Decision: Represent lease expiry fields on the runner protocol as UTC Unix milliseconds.
  - Context: Acceptance and renewal acknowledgements must carry a portable, unambiguous expiry without adding an unnecessary protobuf dependency.
  - Alternatives considered: `google.protobuf.Timestamp` or runner-local durations.
  - Reason: Integer UTC milliseconds are deterministic, easily validated at the transport boundary, and preserve the control plane's authoritative absolute expiry.
  - Consequences: The orchestrator rejects non-positive or unrepresentable renewal timestamps before persistence.

- Decision: Store task artifacts in the repository-local `.loop` directory.
  - Context: The OpenCode backend executes from the dedicated repository worktree and task packet paths are repository-relative.
  - Alternatives considered: Store `.loop` beside the repository workspace root.
  - Reason: Repository-local artifacts make the backend working directory, prompt artifact, expected result path, and cleanup scope consistent.
  - Consequences: Repository manager workspaces expose `Loop` as `<worktree>/repository/.loop`.

- Decision: Start dispatch only after the authoritative lease acknowledgement and treat agent summaries as non-reportable terminal payload data.
  - Context: A runner may admit an offer but has no valid execution lease until the orchestrator acknowledges it; agent output can contain sensitive strings.
  - Alternatives considered: Dispatch on offer acceptance or include the raw agent result in lifecycle events.
  - Reason: Acknowledgement-gated dispatch preserves lease fencing, while a minimal terminal payload avoids exposing agent output through the control stream.
  - Consequences: Lifecycle events contain only identifiers, exit code, changed files, commands, and status; R7 adds a fenced cancelled terminal event.

- Decision: Use the local branch ref for managed-mirror worktree creation.
  - Context: The isolated runner gRPC simulation used a real bare remote and exposed that `git clone --mirror` stores fetched branches as local refs, not `origin/<branch>` tracking refs.
  - Alternatives considered: Create remote-tracking refs in every mirror or use `origin/<branch>` for both repository modes.
  - Reason: `<defaultBranch>` is the valid refreshed mirror ref and preserves direct Git semantics; an existing checked-out local repository continues to use its `origin/<branch>` tracking ref.
  - Consequences: Managed-clone execution now reaches the agent backend; repository command expectations distinguish the two source modes.

## Validation Status

- Build: `PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=src python3 -m compileall -q src tests` passed; `go vet ./... && go test ./...` passed in `api/`; `go vet ./... && go test -race ./...` passed in `runner/`; `go vet ./... && go test ./...` passed in `gen/go`. `make proto-generate` (Buf 1.50.0 via Docker) passed.
- Go race detector: `go test -race ./...` passed in `runner/`.
- All orchestrator tests: 166 pass (12 skipped — grpcio/langgraph not installed).
- Go tests: All pass in api/, runner/, gen/go/.
- Docker Compose: All 5 services (postgres, orchestrator, api, web, runner) running and healthy. Full Compose stack functional:
  - Login works, returns session cookie + CSRF token
  - `GET /api/v1/projects` returns seeded demo project
  - `GET /api/v1/runners` returns registered runner with online status and current heartbeat timestamp
  - `GET /api/v1/runner-tokens` returns available tokens
  - Web UI serves React app at port 3000
  - Runner registers, persists identity, connects bidirectional gRPC stream, heartbeats, reconnects after orchestrator restart
  - OpenCode 1.18.5 available in runner container
  - Git 2.54.0 available in runner container
- Database: PostgreSQL 16 running with migrations 001 and 002 applied.
- Proto generation: Buf protocol lint and generate pass through Docker.
- Lint: Go formatting passes. Python ruff/mypy remain unavailable in local environment.
- Database migrations: Both migrations applied on orchestrator startup.

## Known Issues

- Issue: No real GitHub issues configured — scheduler has no eligible issues to schedule.
  - Impact: End-to-end scheduling, offer, execution, and merge workflow cannot be validated without a real GitHub repository with issues and a configured `GITHUB_TOKEN`.
  - Suggested resolution: Configure `LOOP_SEED_PROJECT_REPOSITORY_URL` with a real GitHub repo, install `gh` CLI in the orchestrator container, and set `GITHUB_TOKEN`. Alternatively, provide a test mechanism to inject synthetic issues.

- Issue: GitHub CLI (`gh`) is not installed in the orchestrator Docker image.
  - Impact: Issue sync, PR creation, and code-host operations are unavailable.
  - Suggested resolution: Add `gh` to the orchestrator Dockerfile, install `GITHUB_TOKEN` secret.

- Issue: Local Python environment lacks application, lint, and type-check dependencies.
  - Impact: gRPC tests are skipped, Ruff/mypy cannot run locally.
  - Suggested resolution: Install dev dependencies or use the container-backed test suite.

## Next Recommended Action

Agent execution now works end-to-end (OpenCode free model creates files, runner reports completion). The next task is **workflow advancement**: the orchestrator receives "completed" events from the runner but doesn't advance the workflow state through planning→implementing→pipeline→review→push. Fix the event handler to trigger workflow state transitions on terminal execution events.

Alternatively, fix the orchestrator to stop creating duplicate execution requests (gen=2) after receiving the gen=1 "completed" event.
