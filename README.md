# Moirai Platform

Moirai is a self-hosted control plane for durable, autonomous software-engineering workflows. See [`PROJECT.md`](PROJECT.md) for the architecture and product scope.

## Architecture

- `orchestrator/` is the Python gRPC control plane. It owns PostgreSQL state, scheduling, issue synchronization, and workflow recovery.
- `runner/` is the Go execution agent. It registers with the control plane and performs one or more eligible executions according to its configured capacity.
- `api/` is the Go HTTP gateway. It authenticates browser sessions and proxies management operations to the orchestrator over gRPC.
- `web/` is a React dashboard served by nginx. In Compose, nginx proxies `/api/` to the API service.
- `proto/` contains the gRPC contracts; `api/openapi.yaml` is the maintained HTTP contract.
- `docs/design/web-console/` is the approved management-console design package: benchmark mockup, UI specification, and implementation task breakdown.

## Local stack

Prerequisites: Docker Engine with the Compose plugin and a GitHub token that can access the repository you intend to automate.

```bash
cp .env.example .env
mkdir -p secrets
printf '%s\n' 'choose-a-strong-postgres-password' > secrets/postgres_password
printf '%s\n' 'postgresql+asyncpg://loop:choose-a-strong-postgres-password@postgres:5432/loop' > secrets/database_url
printf '%s\n' 'choose-a-strong-admin-password' > secrets/initial_admin_password
printf '%s\n' 'github-token-with-repo-and-workflow-scopes' > secrets/github_token
openssl rand -hex 32 > secrets/runner_registration_token
chmod 600 secrets/*
docker compose up --build -d
docker compose ps
curl --fail http://localhost:3000/
curl --fail http://localhost:8080/ready
```

`docker compose ps` reports every service as healthy once startup completes. The API port is bound to loopback for local diagnostics; use the web endpoint on port 3000 for normal browser access. Stop the stack with `docker compose down`. Add `-v` only when you intentionally want to remove local database and runner data.

Compose reads passwords, tokens, and registration credentials only from the files in `secrets/`. Do not put their values in `.env` or shell environment variables. The `.env` file contains non-secret configuration only; its `${...}` values are the variables Compose reads.

`secrets/github_token` is mounted into both the orchestrator and the runner. The orchestrator uses it for issue synchronization and pull requests; the runner uses it to authenticate `git clone`, `git fetch`, and `git push` for the repositories it works on. A task packet only names the credential it needs, and the runner resolves the value locally, so the token never travels over the control stream. `LOOP_RUNNER_ALLOWED_ENVIRONMENT` must list `GITHUB_TOKEN` for that resolution to be permitted; a packet naming a variable that is not allowed or not configured fails the execution instead of running unauthenticated.

## Workflow recovery guarantees

- The issue workflow is event-driven: a node that queues an agent execution ends the graph invocation, and the runner's terminal event resumes it from that same edge. One terminal event advances the workflow by at most one queued execution, so retry budgets are only spent on executions that actually ran. This requires the durable LangGraph checkpointer (see [orchestrator workflow execution model](orchestrator/README.md#workflow-execution-model)).
- No workflow gate is decided by another role's exit code. The local pipeline — the deterministic completion gate — is dispatched on every entry into that phase, after the implementation and again after every repair, and its own terminal event is the only writer of `pipeline_passed`. Two runner/configuration gaps still limit what that gate is worth in practice; both are named under [gate ownership](orchestrator/README.md#gate-ownership).
- An execution request is closed the moment its execution is over, and a maintenance loop closes the ones nothing can ever report on. That is what lets the same loop detect a run whose status says an agent execution should be in flight while none is, and repair it: a lost execution is re-queued for the same phase — never skipped by advancing the graph past it — while a run whose execution did report is handed back to the graph at the status that was committed. Runs parked on a GitHub check or a human decision are never recovered this way (see [execution request lifecycle](orchestrator/README.md#execution-request-lifecycle)).
- Terminal runner events persist an outcome identity: a diff hash for successes, a failure fingerprint for failures. Both are scoped to the execution's role, so a zero-diff planner success is never mistaken for a zero-diff pipeline success, and each kind is only ever compared with its own kind. The failure fingerprint is the stable one the runner emits, so identical failures still match when durations differ. Four identical terminal outcomes for the same role block the workflow instead of retrying indefinitely.
- Three consecutive blocked workflows with the same project failure reason open that project's circuit. After a five-minute cooldown, one durable half-open probe is allowed; its delivery closes the circuit and a blocked probe reopens it. Provider failures use the same durable circuit state.
- A half-open circuit is never left without a live probe. The probe is claimed on the project and the provider together or not at all, and every terminal outcome resolves it: delivered closes the circuit, blocked reopens it, and cancelled, failed, or an offer nobody answered reopens it with a fresh cooldown because the probe reported nothing. Closing a circuit always drops its probe pointer, so a workflow that outlived its claim can no longer reopen a circuit that was closed on newer evidence, and each scheduling pass reopens any half-open circuit that has been claimed for longer than the cooldown by a probe workflow that is missing or already terminal.
- The provider circuit gates scheduling for every project on that provider, so a sync pass decides it once, from the pass as a whole: any project that syncs clears it, and a failure is recorded only when every enabled project was attempted and every one of them failed. One broken project among healthy ones backs itself off instead of halting the provider, and a refused `agent:*` label write never counts against it.
- Task packets carry acceptance criteria, prior failures, revision and diff context. Reviewer prompts are independently generated and exclude developer plans or reasoning.
- Issue reconciliation revisits terminal workflows so `agent:blocked` and `agent:delivered` labels converge after retries or restarts. Each issue is reconciled against its newest workflow run, so historical runs cannot flip a converged terminal label.
- Reconciliation only adds and removes labels in the `agent:*` namespace. Triage labels and the user-supplied `agent-priority:N` label are never deleted by a sync pass. See [issue label ownership](orchestrator/README.md#issue-label-ownership).

The API is published at `http://localhost:8080`; the dashboard is published at `http://localhost:3000`. Compose disables the API secure-cookie setting only because this development topology terminates no TLS.

## Development

Run `make help` for available targets. `make test` runs orchestrator, runner, API, and web validation. Run `make dev-install` before Python-only targets on a clean checkout.

Each service README lists its executable configuration surface:

- [orchestrator configuration](orchestrator/README.md#configuration)
- [API configuration and HTTP contract](api/README.md#configuration)
- [runner configuration](runner/README.md#configuration)
- [web pages, development and proxying](web/README.md#pages)
