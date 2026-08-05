# Moirai Platform

Moirai is a self-hosted control plane for durable, autonomous software-engineering workflows. See [`PROJECT.md`](PROJECT.md) for the architecture and product scope.

## Architecture

- `orchestrator/` is the Go gRPC control plane. It owns PostgreSQL state, scheduling, issue synchronization, and workflow delivery.
- `runner/` is the Go execution agent. It registers with the control plane and performs one execution at a time.
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

### Running against a paid or subscription model

The default model, `opencode/deepseek-v4-flash-free`, needs no credential — which is why the
free tier is what works out of the box. A paid provider needs three things, and missing any one
of them fails the execution with a message naming it.

**1. Declare it**, so the task packet asks for it. Names only; no value goes near this setting:

```bash
echo "MOIRAI_AGENT_CREDENTIAL_REFS=OPENROUTER_API_KEY" >> .env
echo "MOIRAI_AGENT_CREDENTIAL_NAMES=OPENROUTER_API_KEY" >> .env
```

The first tells the orchestrator to request it in every task packet; the second adds it to the
runner's allow-list, which is what the runner will accept a request for.

**2. Supply a value**, in one of two places:

| Where | How | Applies to |
|---|---|---|
| Deployment | `OPENROUTER_API_KEY=...` in `.env`, read by the runner | Every project that has no key of its own |
| Project | *Projects → Credentials → Add provider credential* | That project, overriding the deployment key |

A per-project key travels to the runner over the control stream, so it needs the TLS overlay for
the same reason a per-project GitHub token does.

**3. Point the agent at the model**, which is `MOIRAI_AGENT_ARGUMENTS`:

```bash
echo "MOIRAI_AGENT_ARGUMENTS=--model,openrouter/anthropic/claude-sonnet-4,--auto" >> .env
```

A resolved key is added to the runner's redaction set before it reaches the agent, so nothing
the agent echoes carries it into the console log, and no value ever travels in a task packet.

**A subscription harness** keeps its credentials in a file rather than a variable. Give the
credential a path and it is written below the home directory the runner builds for the
execution — which is the only `~` the agent sees, because the runner overrides `HOME` to keep
an execution from inheriting anything it was not granted:

```bash
echo "MOIRAI_AGENT_CREDENTIAL_REFS=OPENCODE_AUTH=.local/share/opencode/auth.json" >> .env
```

The named variable then carries the path rather than the value. If the harness refreshes the
token inside a run, the runner notices and writes the new value back to the project's
credential, so the next execution starts from a live token instead of re-authorizing. That
write-back needs the credential to be stored per project — a deployment-wide key in the
runner's environment has nowhere durable to go back to.

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

- The issue workflow is event-driven. The orchestrator dispatches one agent execution and advances the run only when the runner reports a terminal event for it, so a run never moves on evidence the runner did not send.
- There are no automatic retries and no execution deadline. A run whose execution fails, or whose delivery to GitHub fails, marks its issue ineligible and stops there; without that the scheduler would re-create the same run from the still-eligible issue on the next tick, forever. `RetryWorkflow` reopens the issue, and the scheduler then starts a fresh run whose history stands alongside the failed one.
- One workflow per project at a time, held by a row in `app.project_locks`. Every terminal path releases it, including the ones that end before the runner ever receives the offer, and those releases are made in the same transaction as the state change they accompany so a failure cannot commit half of it.
- A recovery sweep runs at startup and every 30 seconds, because the locks above are only as good as the process that releases them. It reclaims jobs whose runner stopped renewing its lease — a runner cannot rescue those itself, since every write path it has is fenced on an unexpired lease — resumes deliveries interrupted between a runner's completion and its pull request, and marks runners offline that this process holds no stream for.
- Merging requires GitHub to have reported the checks green. An empty check rollup is pending, never green: GitHub reports no checks for the first seconds of a pull request's life and reports none at all for a repository with no CI configured, and treating either as success would merge unverified code. Entries whose shape the orchestrator does not recognise are pending for the same reason.
- Job events are fenced on the lease generation, the job status, and a monotonic sequence number, so a stale or replayed event from a runner that has lost its lease is rejected rather than applied.
- Issues are re-read from the tracker on a timer as well as on demand, so an unattended deployment discovers new work without an operator pressing "Sync now". `agent:ready` opts an issue in; `agent:blocked` and `agent:delivered` opt it back out.

Planning, a deterministic local pipeline, independent AI review, bounded repair loops, and the human approval gate are implemented as opt-in per-project gates. A V1 workflow dispatches an implementation execution (optionally after a planning phase), opens a pull request, waits for checks, and merges; projects that opt in also gate on the deterministic pipeline, an independent AI review, and bounded repair cycles. See [`PROJECT.md`](PROJECT.md) for the full delivery flow.

The API is published at `http://localhost:8080`; the dashboard is published at `http://localhost:3000`. Compose disables the API secure-cookie setting only because this development topology terminates no TLS.

## Development

Run `make help` for available targets. `make test` runs orchestrator, runner, API, and web validation.

`make test` deliberately excludes the orchestrator's PostgreSQL integration suites — most of the workflow state machine — because they need a real database. It says so on screen when it finishes, naming the suites it left out; run them with `make test-postgres-integration` and a `LOOP_TEST_DATABASE_URL`. See [orchestrator local checks](orchestrator/README.md#local-checks).

Each service README lists its executable configuration surface:

- [orchestrator configuration](orchestrator/README.md#configuration)
- [API configuration and HTTP contract](api/README.md#configuration)
- [runner configuration](runner/README.md#configuration)
- [web pages, development and proxying](web/README.md#pages)
