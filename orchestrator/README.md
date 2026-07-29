# Moirai Orchestrator

The orchestrator is the durable gRPC control plane. It applies database migrations, owns scheduling and workflow state, synchronizes GitHub issues, and receives runner events on port `50051`.

## Run locally

```bash
python3 -m venv .venv
.venv/bin/pip install -e '.[dev]'
LOOP_DATABASE_URL='postgresql://loop:password@localhost/loop' PYTHONPATH=src .venv/bin/python -m moirai.main
```

The startup path requires PostgreSQL and durable LangGraph checkpointing. `LOOP_ALLOW_NO_CHECKPOINTER=true` is only for reduced-capability tests and development environments.

## Bootstrap

After migrations, startup seeds the initial admin user, the seed project, and the runner registration token. The three steps are independent: each one re-checks its own row on every start, none is gated on another having run, and each insert is also guarded by `ON CONFLICT DO NOTHING` so simultaneous instances cannot collide. A start interrupted partway — or one that ran before a secret was configured — is completed by the next start instead of leaving the database permanently half-seeded.

The consequence is that a seeded row deleted by hand is re-created on the next start. Unset `RUNNER_REGISTRATION_TOKEN` or set `LOOP_SEED_PROJECT_NAME` to an empty value to stop seeding that resource. Registration tokens are single-use, so the token step deliberately does not re-seed a hash that already exists in any state, including used or expired.

## Workflow execution model

The issue workflow is an event-driven state machine, not a run-to-completion function. Every node that queues an execution (`plan`, `implement`, `pipeline`, `review`, `repair`, `ci_repair`, `push` — `pipeline` runs the project's commands rather than an agent, but suspends the same way) sets `awaiting_execution` and its outgoing edge ends the graph invocation. The gates the downstream nodes read (`plan_valid`, `pipeline_passed`, `review_approved`) only exist once that execution reports back, so continuing would route on stale defaults, queue phantom executions, and exhaust the retry budget before any agent ran.

The runner's terminal event clears `awaiting_execution` (`workflows/runner_events.py`), and `PersistedWorkflowRuntime.run` resumes the graph from the same edge with `aupdate_state` + `ainvoke(None, config)`. One terminal event therefore advances the workflow by at most one queued execution.

Resuming from that edge is a checkpointer capability. Without a checkpointer the only way forward is replaying the graph from its start node, which would re-enter nodes whose executions already ran, so a suspended run is left untouched and a warning is logged instead. Deployments that must make progress require a checkpointer; Compose never sets `LOOP_ALLOW_NO_CHECKPOINTER`.

### Execution request lifecycle

`app.workflow_execution_requests` is the queue between a dispatching node and the scheduler. A row is `queued` when the node creates it, `dispatched` when `schedule_execution` places it on a runner, and closed when the execution is over:

| Terminal status | Written by | Meaning |
| --- | --- | --- |
| `completed` / `failed` / `cancelled` | `accept_event` | The runner reported this execution's terminal event; the status mirrors the event. |
| `queued` (back from `dispatched`) | `_release_unanswered_offer` | The offer was never answered; the same request is re-offered on a later tick. |
| `expired` | `_block_unanswered_run` | The run was blocked after too many unanswered offers. |
| `orphaned` | `close_orphaned_execution_requests` | Nothing can execute or report on the row any more: its run is terminal, or its `dispatched` row has no job left that could deliver a terminal event. |

Only `queued` and `dispatched` count as open. Leaving a finished request open is what previously made stalled-run recovery unreachable (issue #94), because the detector requires a run to have no open request.

A workflow run has at most one open request. `AsyncpgWorkflowPersistence.dispatch` enforces that for its own inserts rather than assuming it: it looks for an open request inside the transaction that already holds the run's row lock and, if it finds one, returns it with `created = False` instead of inserting; the dispatching node then adopts that request and charges no attempt counter. It is not a schema constraint — the maintenance loop's lost-execution repair also inserts rows, and a unique index would refuse to build on any database that the pre-fix duplicate-dispatch bug had already left with two open rows. This is what makes a replayed transition harmless — the outbox is at-least-once, so every delivery may happen twice (issue #96). Matching `queued` rows only was not enough: the scheduler claims a request the moment it offers the work, so a replay landing after that claim used to queue a second request for the same role, offering the same agent work twice while spending a second unit of retry budget on it.

`PersistedWorkflowRuntime.run` applies the same rule one level up. A caller clearing `awaiting_execution` for a run that still has an open request is delivering a transition that was already applied, so the gate is re-asserted and the graph stays where it is. Without that, the second delivery would advance the graph one node past the execution it is waiting on — entering, for example, `pipeline` while the developer execution is still running, so the developer's own terminal event would then resume from the `pipeline` edge and route straight out of the phase without the deterministic pipeline ever having run.

The open row is also what tells `build_task_packet` which role an offer is for. A job offered for a run that has queued executions before but has no `dispatched` request is therefore refused rather than sent the bootstrap planner packet: that packet carries the execution ID `{job_id}-plan`, which `accept_event` is guaranteed to reject, and the runner's control stream would abort on every retry. A refused packet makes the scheduler skip the candidate and lets the offer expire on its own TTL, which the unanswered-offer limit bounds.

### Workflow maintenance loop

One elected instance runs a 30-second maintenance loop (`main._run_workflow_maintenance_loop`) with three arms, in order:

1. Drain `app.workflow_transition_outbox` — at-least-once delivery for transitions committed by `accept_event` but never invoked.
2. Close orphaned execution requests, so runs holding a dead row stop looking busy.
3. Repair stalled runs: a run whose status says an agent execution should be in flight, untouched for longer than the stall window (2 minutes), with no open execution request and no job in `offered`, `preparing`, `running`, or `recovering`. At most 20 per tick, each isolated from the others' failures.

Arm 3 does one of two things, decided by how the run's most recent execution request was closed:

| Last request | What happened | Repair |
| --- | --- | --- |
| `orphaned` | The execution was lost — no runner will ever report it. | Write a fresh `queued` request for the **same role** and leave the graph suspended. `schedule_execution` re-offers it, and its terminal event resumes the graph through whatever path that role's terminal event normally takes. |
| `completed` / `failed` / `cancelled` | The execution reported and `accept_event` committed the new status; only the graph invocation was lost. | Re-enter the graph with `awaiting_execution` cleared, so the suspended edge can move. |

The distinction is load-bearing, not tidiness. Three of the six dispatching nodes (`implement`, `repair`, `push`) have **unconditional** outgoing edges, so clearing `awaiting_execution` on a run whose execution was lost would not re-run the phase — it would *skip* it. On `push` that means creating a pull request for a branch that was never pushed, and then merging it.

The re-queued attempt spends no additional retry budget: the counters were charged when the node dispatched the execution that was lost, so the replacement delivers an attempt that was already paid for. Because no retry budget can therefore ever trip, the replacement is bounded separately — after five lost attempts for the same role the loop stops replacing it and logs, rather than buying unbounded agent executions.

Runs the loop touched but could not repair have their `updated_at` bumped anyway. The detector reads that column, so a permanently failing run backs off by a full stall window instead of occupying the bounded batch on every tick.

The advancing branch does not replay the state updates that rode on the outbox row, so a gate the lost invocation would have set (`plan_valid`, `pipeline_passed`, `review_approved`) stays as it was and the graph re-runs that phase rather than advancing on a verdict it never saw. That is safe and bounded by the same retry budgets, but it does cost one repeated execution. Arm 1 usually spares the run that cost: a stranded outbox row is reclaimed after 90 seconds and delivered with its state updates intact, which on a loop ticking at its normal 30 seconds lands before the 2-minute stall window opens, so arm 3 finds nothing left to repair. That ordering is a consequence of the two timeouts, not an enforced property — a slow tick can invert it, and `recover_stalled_workflow_run` does not consult the outbox — so the lossy branch above remains reachable and remains correct.

#### The outbox processing lease

Arm 1 marks a row `processing` and commits before calling out to the graph runtime, because the delivery can reach GitHub and must not be held inside a database transaction. A drainer that dies mid-delivery therefore cannot release its own row, and a drain that selected `pending` alone dropped that transition forever ([#96](https://github.com/alexandre-leites/moirai/issues/96)). `processing` is consequently a lease, not a state: the claim stamps `processing_started_at`, and any drainer reclaims a claim older than 90 seconds.

The claim stamp is also a fence. Completing a row, and releasing it after a failed delivery, both require that exact stamp to still be present, so a delivery that outlived its own lease cannot mark a row processed that somebody else is now delivering, nor release a claim it no longer holds.

The inline delivery `accept_event` performs takes the same lease, so a maintenance tick landing between its commit and its delivery does not deliver the same transition twice; a drainer that loses the claim simply does nothing, because the holder is delivering the identical transition. A delivery slower than the lease is still delivered twice, by design — that is what bounds a crashed drainer — and it is harmless, because nodes adopt the execution request they already have.

Runs parked on something other than an execution — `waiting_github_checks` and `waiting_human` (a GitHub check or a human decision), `pr_created` and `merging` (transient statuses on unconditional edges) — are deliberately not recovered this way. A run held at the `wait_for_human` interrupt carries the DB status `waiting_github_checks`, and re-entering its graph would run `wait_for_human` with no decision recorded, which `route_after_human_response` resolves as "not approved" and blocks the run. Unanswered offers (`offered`) and lease expiry (jobs in `recovering`) have their own owners, `expire_offers` and `recover_one`.

## Gate ownership

No role's exit code is ever read as another role's verdict. Each gate is decided by the thing that produced its evidence:

| Gate | Decided by |
| --- | --- |
| `plan_valid` | the planner execution's terminal event, from a schema-valid `planner-result` |
| `pipeline_passed` | the pipeline execution's terminal event, and nothing else |
| `review_approved` | the reviewer execution's terminal event, from a schema-valid `review-result` |
| `checks_passed` / `checks_pending` | the `wait_for_checks` node, from the code host's required checks |

`pipeline_passed` is the strictest of these: `grep -rn '"pipeline_passed":' orchestrator/src` returns exactly one hit, the `resolved_role == "pipeline"` branch of `workflows/runner_events.py`. The `pipeline` node dispatches a real pipeline execution every time the phase is entered — after the developer implements, and again after every repair — and it neither reads nor writes the gate. A developer or repairer terminal event moves the run to `local_pipeline` with `pipeline_passed` untouched.

That strictness is deliberate: the local pipeline is the platform's deterministic completion gate. Inferring it from the developer's exit code, as the orchestrator used to, skipped the deterministic checks in exactly the case they exist for — the agent believing it succeeded — and let a repaired tree inherit the verdict of the pipeline run that preceded the repair.

The pipeline execution does not spend `total_agent_executions`: it runs the project's commands, not an agent. It is bounded instead by the phases that lead into it, since the `pipeline` node is reachable only from `implement`, `repair` and `ci_repair`, each of which dispatches a counted agent execution first. An exhausted agent budget still blocks at the `pipeline` node rather than paying for a verdict that has nowhere to route.

Two caveats about what the gate is worth today, neither of them in the orchestrator:

- The commands come from `app.project_pipeline_steps` (`required = true`, in `position` order), which nothing in this repository writes yet ([#114](https://github.com/alexandre-leites/moirai/issues/114)). A project with no required steps has an empty gate: the execution reports success without running anything.
- The runner rebuilds the workspace from the default branch for every execution (`repository.Manager.Prepare` runs `git worktree add -B <agent-branch> … <default-branch>`, which force-resets the branch), so a pipeline execution currently validates the base branch rather than the implementation it follows ([#136](https://github.com/alexandre-leites/moirai/issues/136)).

## Retry budgets

`RetryBudget` (`workflows/policy.py`) is the single source of truth for the bounds, and each counter has exactly one node that increments it. The node a repair is dispatched from is the only record of *why* it was dispatched, so the two repair sources are two nodes rather than one:

| Counter | Default | Incremented by | Dispatched when |
| --- | --- | --- | --- |
| `planning_attempts` | 2 | `plan` | the planner has not produced a valid plan yet |
| `implementation_attempts` | 3 | `implement` | the plan is valid |
| `review_cycles` | 3 | `review` | the local pipeline passed |
| `pipeline_repair_attempts` | 3 | `repair` | the local pipeline failed, AI review requested changes, or a human requested changes |
| `ci_repair_attempts` | 3 | `ci_repair` | the pull request's required GitHub checks failed |
| `total_agent_executions` | 10 | every dispatch of an agent role | any of the above, plus `push` |

`repair` and `ci_repair` dispatch the same `repairer` role and report the same `repairing` phase — the runner does identical work and `runner_events.py` translates their terminal events identically, back to `local_pipeline` so the repaired tree earns a fresh pipeline verdict. Only the budget differs. A CI repair is a real agent run, so it spends `total_agent_executions` like any other repair.

The counters are independent by construction: a run that exhausted its local repair budget can still *dispatch* a CI repair (it will still block at the `pipeline` node if the repaired tree fails locally), and a CI failure can no longer make a later local-pipeline failure block with a misleading reason.

`ci_repair_attempts` is nonetheless not the bound that stops a CI loop under the shipped defaults, because one CI cycle — `ci_repair` → `pipeline` → `review` → `push` → `create_pull_request` → `wait_for_checks` — costs three agent runs *and* one `review_cycles` unit:

- `review_cycles = 3` allows at most two CI cycles on top of the first review, whatever the other budgets say.
- `total_agent_executions = 10` allows two CI cycles only for a run that reached its pull request on the cheapest path (4 agent runs); a run that took one local repair reaches the pull request at 7 and affords one.

So `ci_repair_attempts` behaves as a configuration knob that becomes operative when those two budgets are raised, and as a correct attribution of what a workflow spent in either case. `RetryBudget` is not wired to configuration today: `build_persisted_runtime` constructs the graph and the nodes with the defaults. Sizing the budgets so the CI bound can bind is a separate, deliberate decision — it is not made here, because picking numbers that make one gate reachable is how the accounting drifted in the first place.

## Operations

Logs are JSON and retain structured fields passed with Python logging `extra`. Metrics are served at `LOOP_METRICS_BIND` (default `0.0.0.0:9090`) on `/metrics`.

The gRPC listener stays insecure by default for local development. Set `LOOP_GRPC_TLS_CERT_FILE` and `LOOP_GRPC_TLS_KEY_FILE` to enable TLS. Set `LOOP_GRPC_TLS_CLIENT_CA_FILE` too to require runner mTLS certificates.

## Circuit breakers

`app.project_circuit_state` and `app.provider_circuit_state` hold one row per project and per issue provider. `open` keeps the project (or every project on that provider) out of `schedule()` for the probe cooldown, five minutes by default (`AsyncpgControlPlane(circuit_probe_cooldown=...)`); once it elapses, the next scheduling pass claims a single `half_open` probe by writing the workflow run it just created into `probe_workflow_run_id`. `half_open` keeps everything else out until that probe resolves.

Both circuits are claimed in one savepoint, so a claim that cannot complete leaves neither row changed. Every way a probe can end resolves it:

| Probe outcome | Circuit |
| --- | --- |
| delivered (`completed`) | closed, pointer cleared |
| `blocked` through the workflow transition path | reopened with a fresh cooldown, counted as a failure |
| `cancelled`, `failed`, an offer nobody answered, or a terminal status written straight to `app.workflow_runs` | reopened with a fresh cooldown, not counted — the probe reported nothing |

Closing or reopening a circuit always clears `probe_workflow_run_id`, so a workflow that outlived its claim cannot decide a circuit twice. As a backstop, each leader-gated scheduler pass reopens any `half_open` row whose probe workflow is missing or already terminal and which has been claimed for longer than the cooldown, and logs `reopened orphaned circuit probes` when it does.

The provider circuit is shared by every project on that provider, so an issue-sync pass decides it once, from the pass as a whole, and only on evidence about the provider: any project that syncs clears it, and a failure is recorded — once — only when every enabled project was attempted and all of them failed. Any other pass, including one that skipped a project because it was backing off, writes nothing. One project failing beside a healthy one is a project fault (a deleted repository, a bad URL, a revoked token) and is handled by that project's own `app.issue_sync_state` backoff rather than by halting scheduling everywhere. A failed `agent:*` label write does not count against the provider at all: the read the pass depends on succeeded, and the labels mirror status onto issues rather than feeding any workflow.

The whole-fleet requirement is deliberate, and it is the reason a project stuck in a long backoff can stop a genuine outage from opening the circuit. Without it, a chronically broken project drops out of later passes and the projects still being attempted become the only evidence, so one of *them* failing reads as a provider outage and halts every project on GitHub. Between those two errors, failing to open only wastes agent executions — which the per-project and per-run breakers still bound — while opening wrongly stops all delivery until someone intervenes. During a real outage every project fails together, so their backoffs stay in step and the first delays (5s, 10s, 20s) are all shorter than the one-minute sync interval: no project is skipped, and the circuit opens on the third pass.

## Issue label ownership

Issue sync owns exactly one label namespace: `agent:*` (`agent:ready`, `agent:running`, `agent:blocked`, `agent:delivered`, `agent:human-approval`). A reconciliation pass only ever adds or removes labels inside that namespace.

Every other label on the issue belongs to humans and survives every pass, including `agent-priority:N`. That prefix is deliberately outside the managed namespace because the scheduler reads it as user input; deleting it would silently reset the issue's priority to the default. `LabelPolicy` rejects any configuration that puts a state label outside the managed namespace or the priority prefix inside it.

Labels are reconciled against a single authoritative workflow run per issue — the newest by `created_at` — so terminal labels converge deterministically no matter how many historical runs an issue accumulated.

## Testing


## Configuration

| Variable | Default | Purpose |
| --- | --- | --- |
| `LOOP_DATABASE_URL` or `LOOP_DATABASE_URL_FILE` | required | PostgreSQL connection URL. Configure exactly one. |
| `LOOP_GRPC_BIND` | `0.0.0.0:50051` | gRPC bind host and port. |
| `LOOP_GITHUB_TOKEN` or `LOOP_GITHUB_TOKEN_FILE` | unset | Token passed to `gh` as `GH_TOKEN` for GitHub issue, pull request, and check operations. When set, startup verifies `gh auth status`. |
| `LOOP_INITIAL_ADMIN_USERNAME` | `admin` | Username created only when the user table is empty. |
| `LOOP_INITIAL_ADMIN_PASSWORD` or `LOOP_INITIAL_ADMIN_PASSWORD_FILE` | unset | Initial administrator password. Without it, bootstrap skips the admin user and still runs its remaining steps. |
| `RUNNER_REGISTRATION_TOKEN` | unset | Raw registration token seeded when no token row carries its hash; it must match the runner's registration token. Unset skips only this step. |
| `LOOP_SEED_PROJECT_NAME` | `demo` | Seed project name, created when no project has that name. Set it empty to disable seed-project bootstrap. |
| `LOOP_SEED_PROJECT_REPOSITORY_URL` | `https://github.com/example/demo.git` | Initial project repository URL. |
| `LOOP_SEED_TOKEN_LABELS` | `linux` | Comma-separated labels permitted by the seeded registration token. |
| `LOOP_SEED_ISSUE_TITLE` | unset | Optional initial issue title. |
| `LOOP_SEED_ISSUE_BODY` | unset | Initial issue body. |
| `LOOP_ALLOW_NO_CHECKPOINTER` | unset | Permit an unavailable checkpointer only when `true`, `yes`, or `1`. Workflows then cannot resume after dispatching an execution, so runs suspend permanently; tests and reduced-capability environments only. |

Secret values accept direct or `_FILE` forms, but not both. Secret files must be regular files no larger than 16 KiB.

Planner and reviewer result schemas are package resources in `src/moirai/workflows/schemas/`, so an image built with `orchestrator/` as its Docker context contains the schemas it validates.

## Validation

```bash
PYTHONPATH=src python3 -m unittest discover -s tests
python3 -m ruff check src tests
python3 -m mypy src
```
