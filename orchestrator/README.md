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

The provider circuit is shared, so an issue-sync pass decides it once at the end rather than per project: it opens on a failure to read or persist issues, and is cleared only when the pass had at least one success and no failure. A failed `agent:*` label write backs that project's sync off but never opens it — the labels mirror status onto issues and are not an input to any workflow.

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
