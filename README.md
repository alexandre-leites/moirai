# Moirai Platform

Moirai is a self-hosted control plane for durable, autonomous software-engineering workflows. See [`PROJECT.md`](PROJECT.md) for the architecture and product scope.

## Architecture

- `orchestrator/` is the Python gRPC control plane. It owns PostgreSQL state, scheduling, issue synchronization, and workflow recovery.
- `runner/` is the Go execution agent. It registers with the control plane and performs one or more eligible executions according to its configured capacity.
- `api/` is the Go HTTP gateway. It authenticates browser sessions and proxies management operations to the orchestrator over gRPC.
- `web/` is a React dashboard served by nginx. In Compose, nginx proxies `/api/` to the API service.
- `proto/` contains the gRPC contracts; `api/openapi.yaml` is the maintained HTTP contract.
- `docs/design/web-console/` is the approved management-console design package: benchmark mockup, UI specification, and implementation task breakdown.

## Quick start

```bash
docker compose up -d
```

Then open <http://localhost:3000> and sign in as `admin` / `Moirai-Local-1`.

That is the whole installation. `compose.yaml` is complete and self-contained: it pulls the
published images, brings up all five services in the right order, and every setting has a working
default, so nothing has to be prepared first — no `.env`, no `secrets/`, no build step. Copy
[`.env.example`](.env.example) to `.env` when you want to change something; it documents every
knob and its default.

In **Portainer**: *Stacks → Add stack → Web editor*, paste `compose.yaml`, and put any overrides
under *Environment variables*. The repository is private, so the packages are too — add
credentials for `ghcr.io` first under *Registries → Add registry → Custom*, or every pull fails
with `403 Forbidden`.

**You do not need a GitHub token to bring it up.** The stack runs and the console works without
one. Moirai just cannot do any *work* — reading issues, cloning, pushing and opening pull requests
all happen as you — so set `MOIRAI_GITHUB_TOKEN` (scopes `repo`, `workflow`) when you want it to
run something.

### Credentials for one project

`MOIRAI_GITHUB_TOKEN` is deployment-wide: every project is reached as whoever it belongs to. A
project can instead carry its own, under *Projects → Credentials*, which is how you reach a
private repository the shared token cannot see. Anything Moirai does for that project — issue
sync, clone, push, pull requests — then runs as that identity.

Stored credentials are encrypted, so this needs a key:

```bash
echo "MOIRAI_SECRET_KEY=$(openssl rand -base64 32)" >> .env
```

Without one the stack still runs; only per-project credentials are refused, with a message
saying so. **Back the key up.** It is not derived from anything and is stored nowhere else, so
losing it means every stored credential has to be entered again.

A value is never shown again once saved — the console reports which credentials are set and
when, and replacing one is the only way to change it.

Two halves of the system use these, and they have different requirements:

| Who | What it does with it | Needs |
|---|---|---|
| Orchestrator | Issue sync, opening and merging pull requests | Nothing extra |
| Runner | Cloning, fetching and pushing the repository | TLS on the control stream |

The runner half needs TLS because the credential has to travel to reach it, and the
orchestrator refuses to send one over a channel it cannot encrypt. Turn it on with the
overlay, which generates a self-signed certificate for the internal network on first start:

```bash
docker compose -f compose.yaml -f compose.tls.yaml up -d
```

Without it nothing breaks — runners keep using the `GITHUB_TOKEN` configured on them, which
is the same credential for every project. With it a runner needs no token at all: each job
is handed its own project's credential, for as long as it holds that job's lease, and the
key material is written to tmpfs and removed when the job ends.

An SSH remote (`git@github.com:owner/repo.git`) uses the project's SSH private key instead of
its token; the remote's scheme decides which, so a project may carry both.

Only port 3000 is published; nginx proxies `/api/` to the API over an internal network. That is
what lets the same file work unchanged against a remote Portainer host.

Startup order is enforced by health checks rather than by timing: postgres has to report ready
before the orchestrator starts, and the orchestrator has to report its database reachable and
migrations applied before the API and runner do. `docker compose up --wait` returns only once all
five are healthy.

### Three overlays

```bash
# build the images from this checkout instead of pulling them
docker compose -f compose.yaml -f compose.build.yaml up --build -d

# read every secret from a file under ./secrets/ instead of the environment
docker compose -f compose.yaml -f compose.secrets.yaml up -d

# encrypt the control stream, so runners can be handed per-project credentials
docker compose -f compose.yaml -f compose.tls.yaml up -d
```

They compose: `-f compose.yaml -f compose.tls.yaml -f compose.secrets.yaml` is a valid stack.

**In Portainer**, which takes one file and has no `-f`, paste
[`compose.tls-stack.yaml`](compose.tls-stack.yaml) instead — the same thing as `compose.yaml +
compose.tls.yaml`, rendered as a single document. It is generated by `make compose-tls-stack`
and `make compose-overlays` fails if it has drifted from its sources, so it cannot quietly
diverge from the stack that gets tested.

Environment values are visible to `docker inspect` and to anything that renders a container's
configuration, Portainer's UI included. [`compose.secrets.yaml`](compose.secrets.yaml) moves them
into files; its header lists the five it expects. It needs files on the Docker host, so it suits a
machine you have a shell on rather than a pasted Portainer stack.

**The defaults are public knowledge and the console is plain HTTP.** That is fine on a private
host and nowhere else: change `MOIRAI_ADMIN_PASSWORD` and `MOIRAI_POSTGRES_PASSWORD` if anyone
else can reach it, put it behind TLS before exposing it, and set `MOIRAI_COOKIE_SECURE=true` when
you do.

## Published images

`v0.2.0` is the current release: `ghcr.io/alexandre-leites/moirai/{orchestrator,api,runner,web}`
at `0.2.0`, `0.2`, `0`, `latest` and `sha-<short sha>`, each a `linux/amd64` + `linux/arm64`
manifest list. `v0.1.0` remains available at `0.1.0` and `0.1`.

`compose.yaml` pulls `latest` by default. Pin `MOIRAI_IMAGE_TAG=0.2.0` for anything reproducible;
`latest` moves with every release. `MOIRAI_IMAGE_PREFIX` points the stack at a fork or mirror.

The packages inherit the repository's visibility, so while it is private a pull needs a token with
`read:packages`:

```bash
echo "$GHCR_TOKEN" | docker login ghcr.io -u <your-github-username> --password-stdin
```

## Releases

[`docs/release.md`](docs/release.md) is the contract: image names, the trigger-to-tag mapping, the tagging strategy, and how to cut a release. In short:

| Trigger | Image tags |
| --- | --- |
| push to `release/X.Y.Z` | `X.Y.Z-rc.<run number>`, `X.Y.Z-rc`, `sha-<short sha>` |
| published GitHub Release `vX.Y.Z`, newest | `X.Y.Z`, `X.Y`, `X`, `latest`, `sha-<short sha>` |
| published GitHub Release `vX.Y.Z`, not newest | `X.Y.Z`, `X.Y`, `sha-<short sha>` |
| published Release flagged pre-release, or tagged `vX.Y.Z-<id>` | that exact version, `sha-<short sha>` |
| manual `workflow_dispatch` | builds everything, publishes nothing |

Pushing a git tag on its own publishes nothing; a GitHub Release has to be published. `make test-release-tags` runs the executable specification of that mapping, and [`.github/workflows/release.yml`](.github/workflows/release.yml) runs it before deriving a single tag.

## Workflow recovery guarantees

- The issue workflow is event-driven: a node that queues an agent execution ends the graph invocation, and the runner's terminal event resumes it from that same edge. One terminal event advances the workflow by at most one queued execution, so retry budgets are only spent on executions that actually ran. This requires the durable LangGraph checkpointer (see [orchestrator workflow execution model](orchestrator/README.md#workflow-execution-model)).
- No workflow gate is decided by another role's exit code. The local pipeline — the deterministic completion gate — is dispatched on every entry into that phase, after the implementation and again after every repair, and its own terminal event is the only writer of `pipeline_passed`. Two runner/configuration gaps still limit what that gate is worth in practice; both are named under [gate ownership](orchestrator/README.md#gate-ownership).
- Delivering the same workflow transition twice is observationally identical to delivering it once. The transition outbox is at-least-once by design, so a replay is expected: a run that already has an open execution request adopts it instead of queueing a second one, no attempt counter or agent budget moves, and the graph stays suspended where the first delivery left it. Outbox rows claimed for delivery hold a 90-second lease rather than a permanent `processing` mark, so a drainer that dies mid-delivery cannot strand a transition (see [execution request lifecycle](orchestrator/README.md#execution-request-lifecycle)).
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
