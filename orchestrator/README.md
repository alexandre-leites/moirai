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

The issue workflow is an event-driven state machine, not a run-to-completion function. Every node that queues an agent execution (`plan`, `implement`, `pipeline`, `review`, `repair`, `push`) sets `awaiting_execution` and its outgoing edge ends the graph invocation. The gates the downstream nodes read (`plan_valid`, `pipeline_passed`, `review_approved`) only exist once that execution reports back, so continuing would route on stale defaults, queue phantom executions, and exhaust the retry budget before any agent ran.

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

### Workflow maintenance loop

One elected instance runs a 30-second maintenance loop (`main._run_workflow_maintenance_loop`) with three arms, in order:

1. Drain `app.workflow_transition_outbox` — at-least-once delivery for transitions committed by `accept_event` but never invoked.
2. Close orphaned execution requests, so runs holding a dead row stop looking busy.
3. Re-enter the graph for stalled runs: a run whose status says an agent execution should be in flight, untouched for longer than the stall window (2 minutes), with no open execution request and no job in `offered`, `preparing`, `running`, or `recovering`.

Arm 3 clears `awaiting_execution` when it re-enters the graph — the detector has already established that nothing is in flight, and the flag is what suspends the graph on the edge out of a dispatching node. Nothing else is invented: the gates the lost execution would have produced are left as they were, so the graph re-runs the phase (bounded by the same retry budgets) instead of advancing past it on a verdict nobody reported.

Runs parked on an external event rather than an execution — `pr_created`, `waiting_github_checks`, `waiting_human`, `merging` — are deliberately not recovered this way: re-entering the graph for one would resolve a pending human-approval interrupt as "not approved". Unanswered offers (`offered`) and lease expiry (jobs in `recovering`) have their own owners, `expire_offers` and `recover_one`.

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

The pipeline execution does not spend `total_agent_executions`: it runs the project's commands, not an agent. It is bounded instead by the phases that lead into it, since the `pipeline` node is reachable only from `implement` and `repair`, both of which dispatch a counted agent execution first. An exhausted agent budget still blocks at the `pipeline` node rather than paying for a verdict that has nowhere to route.

Two caveats about what the gate is worth today, neither of them in the orchestrator:

- The commands come from `app.project_pipeline_steps` (`required = true`, in `position` order), which nothing in this repository writes yet ([#114](https://github.com/alexandre-leites/moirai/issues/114)). A project with no required steps has an empty gate: the execution reports success without running anything.
- The runner rebuilds the workspace from the default branch for every execution (`repository.Manager.Prepare` runs `git worktree add -B <agent-branch> … <default-branch>`, which force-resets the branch), so a pipeline execution currently validates the base branch rather than the implementation it follows ([#136](https://github.com/alexandre-leites/moirai/issues/136)).

## Operations

Logs are JSON and retain structured fields passed with Python logging `extra`. Metrics are served at `LOOP_METRICS_BIND` (default `0.0.0.0:9090`) on `/metrics`.

The gRPC listener stays insecure by default for local development. Set `LOOP_GRPC_TLS_CERT_FILE` and `LOOP_GRPC_TLS_KEY_FILE` to enable TLS. Set `LOOP_GRPC_TLS_CLIENT_CA_FILE` too to require runner mTLS certificates.

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
