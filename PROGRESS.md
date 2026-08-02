# Implementation Progress

## Current Status

- Overall status: MVP surface substantially complete — 34 of 35 roadmap items in
  `tasks/todo.md` are done (three with caveats). #197 is fixed and can be closed; 4 issues
  remain open.
  **CI is green for the first time**: all 12 jobs passed on `238a9c4`, so `main` is
  independently verified rather than merely believed.
- Current phase: Implementation. #197 landed with the CI work, so 4 issues remain open.
- Active implementation: none.
- Last updated: 2026-08-02
- Agent/session identifier: system-versions / 2026-08-02

## In Progress

_Nothing is claimed. The next agent should take the first item under Pending Implementation._

## Done

- [x] System-version reporting
  - Completed: 2026-08-02
  - Relevant files: service Dockerfiles, `compose.build.yaml`, release/CI workflows,
    `api/internal/http/server.go`, runner control protocol, and `web/src/overview.tsx`.
  - Behavior delivered: `make build-images`, CI, and release derive a 12-character Git SHA;
    every service image refuses an empty build version. Health reports API/orchestrator versions;
    runner heartbeats persist their version; Overview shows non-empty Web, API, Orchestrator,
    and runner versions, truncated to 12 chars.
  - Validation performed: API Go suite; focused runner version test; 663 orchestrator tests;
    orchestrator lint/typecheck; web typecheck, lint (16 existing warnings), and overview tests;
    all four Docker image builds.
  - Commands executed: `make proto-generate`, `make build-images`, `make test-orchestrator`,
    `make lint`, `make typecheck`, Dockerized `go test ./...` for API.

- [x] Per-project GitHub credentials, encrypted at rest and used for that project's `gh`
      calls (migration 015, `persistence/secrets.py`, `workflows/code_host_factory.py`,
      three gRPC RPCs, three REST endpoints, console section). Details in `tasks/todo.md`.
- [x] Dumb runners: `ResolveJobSecret` hands a runner one secret for a job it currently
      holds, lease-fenced and refused over an insecure channel. SSH keys land on tmpfs at
      0600 and are removed when the job ends. `compose.tls.yaml` turns it on.
- [x] Replace the web console with the approved design package (phases C and D of
      `docs/design/web-console/tasks.md`)
  - Completed: 2026-08-01 (`acd7ad3` on `main`)
  - Relevant files: `web/src/` (rewritten — `styles.css`, `ui/`, `shell.tsx`, `poll.ts`,
    `status.ts`, `console-data.tsx`, `overview.tsx`, `queue.tsx`, `workflows.tsx`,
    `workflow-detail.tsx`, `runners.tsx`, `projects.tsx`), `web/README.md`
  - Behavior delivered: the mockup's token sheet in both themes, the phase-thread signature
    component, a polling data layer that hides the transport from views, and six views —
    overview with triage, queue with issue-sync state, workflow list with filter/search in
    the URL, workflow detail with decision panel and operator controls, runners with
    registration tokens, and projects. Live sidebar counts, focus-trapped mobile drawer,
    per-route document titles, 404. `/tokens` redirects to `/runners`.
  - Validation performed: 187 web tests, `tsc --noEmit`, eslint, production build, plus a
    manual walk of every route in both themes and at narrow width against a stub API.
  - Commands executed: `make test-web`, `npm run build`, `cd api && go build ./... &&
    go test ./...`, `make test-orchestrator`, `make proto-check`
  - Notes: sections with no data source were deliberately omitted rather than stubbed; the
    list is in `web/README.md`.

- [x] Restore the Go workflow handlers and widen the workflow list
  - Completed: 2026-08-01 (same commit)
  - Relevant files: `api/internal/http/handlers/workflows.go`,
    `orchestrator/src/moirai/persistence/control_plane.py`,
    `orchestrator/src/moirai/grpc/control_plane.py`, `proto/control_plane.proto`,
    `api/openapi.yaml`
  - Behavior delivered: `get`, `listEvents`, `submitDecision`, `retry`, `cancel` and `block`
    restored — commit `9312ee5` (#206) had cut them while leaving their routes registered, so
    the Go API did not compile. `list_workflows` now shares `get_workflow`'s projection, so
    the console reads issue titles, pull requests, attempts and timestamps without a request
    per row. Added `total_agent_executions` to the proto `Workflow` message.
  - Validation performed: `go build ./...` and `go test ./...` pass; orchestrator suite shows
    no new failures against the pre-change baseline; `make proto-check` passes (it was
    failing on `main` before, because the committed Go stubs predated `GetSchedulerMetrics`).
  - Commands executed: `make proto-generate`, `make proto-check`, `go test ./...`,
    `make test-orchestrator`

- [x] Get CI executing, then fix everything it found (closes #197)
  - Completed: 2026-08-01 (`42aa47c`, `f15d6ef`, `238a9c4`)
  - Relevant files: `.github/workflows/*.yml`, `orchestrator/src/moirai/grpc/control_plane.py`,
    `orchestrator/src/moirai/persistence/control_plane.py`,
    `orchestrator/src/moirai/{main,observability}.py`, `runner/internal/control/`,
    `web/package.json`, `README.md`
  - Behavior delivered: both workflows moved onto GitHub-hosted runners. The first run that
    executed failed six of nine jobs, every one a real defect:
    `_require_admin` never awaited `context.abort`, so the admin check did nothing and
    `SetRunnerState` was open to any authenticated viewer (#197); `schedule()` had lost its
    f-string prefix and sent `{...}` to Postgres, breaking every scheduling path;
    a file named `flush_test_2.go` compiled a test into the production package and broke the
    runner build; three tests had gone stale against correct product code; `react-router` sat
    inside a high-severity advisory; and the smoke test's admin password failed the bootstrap
    policy — as did the README's, so the documented quickstart could not boot.
    Standing the stack up to verify that last one exposed a metrics loop that had been
    failing every tick, leaving all four Prometheus gauges at zero.
  - Validation performed: CI run `30701717203`, all 12 jobs green.
  - Commands executed: `make lint`, `make typecheck`, `make test-orchestrator`,
    `make test-web`, `go test -race ./...`, `make proto-check`,
    `make test-postgres-integration` against a real Postgres, `docker compose up --wait`

- [x] Reconcile `tasks/todo.md` and this file against the tracker and the working tree
  - Completed: 2026-08-01
  - Relevant files: `tasks/todo.md`, `PROGRESS.md`
  - Behavior delivered: every roadmap box in Phases 0–4 was unticked despite 34 of 35 items
    having shipped, and this file named a task merged three commits earlier as in progress.
    Both now reflect reality, with per-item issue references and explicit caveats where a
    closed issue was superseded by an open one.
  - Validation performed: each tick cross-checked against the closed-issue list; several
    spot-verified in the tree (migration numbering, `suspend_after_dispatch`, absence of SSE,
    `configure_logging`, read-only `project_pipeline_steps`).

## Blocked

_Nothing is blocked._ The CI blocker recorded here earlier is resolved: `ci.yml` and
`release.yml` moved off the self-hosted pool onto `ubuntu-latest` (the self-hosted
declarations are commented out in place, one line above each replacement, so moving back is a
one-line edit per job). The first run that actually executed failed six of nine jobs; those
are fixed in `f15d6ef` and `238a9c4`, and run `30701717203` is green across all 12.

## Pending Implementation

- [ ] #114 (P1) — make project pipeline steps configurable
  - Priority: highest of the code work. This is the gate the product's core claim rests on.
  - Dependencies: none.
  - Expected behavior: an operator can define the ordered shell steps for a project (e.g.
    `npm test`, `ruff check`) and the local-pipeline node runs them. Today
    `app.project_pipeline_steps` is only ever read — there is no insert or update anywhere in
    the codebase — so every project has an empty step list and the pipeline gate passes by
    default.
  - Definition of done: steps can be set through the API and the console's project form;
    the pipeline node executes them; a failing step blocks the workflow and is visible on
    workflow detail; an integration test covers a project whose pipeline fails.

- [ ] #118 (P1) — Server-Sent Events end to end
  - Priority: after the two above.
  - Dependencies: none, but it replaces the interim polling in `web/src/poll.ts`, which was
    written behind an interface so the swap touches the hook and not any view.
  - Expected behavior: a workflow transition appears in an open console without a reload;
    killing the stream falls back to polling; reconnect replays from `Last-Event-ID`.
  - Definition of done: `StreamEvents` RPC → API SSE proxy → `useEventStream` hook, plus
    `proxy_buffering off` for `/api/v1/events/` in `web/nginx.conf`.

## Quality Backlog

- [ ] Retire the console's two client-side derivations
  - Category: correctness
  - Risk: low
  - Expected benefit: the console stops inferring what the server can state
  - Recommended timing: with `docs/design/web-console/tasks.md` A2 and A3
  - Detail: gate state and thread position are derived from attempt counters and the pull
    request because `current_phase` is overwritten with `blocked`/`failed` on a bad ending;
    event sentences are rendered client-side from `event_type`. Both are isolated and
    documented in `web/src/status.ts`.

## Decisions

- Decision: ship no default `LOOP_SECRET_KEY`
  - Context: per-project credentials are encrypted with a key from deployment configuration.
    A stack that "just works" from `docker compose up` argues for a default.
  - Alternatives considered: a key committed to `compose.yaml`; generating one on first
    start and persisting it to a volume.
  - Reason: a committed key is public, so encrypting with it is theatre — and silent
    theatre, which is worse than none. A generated key in a volume is real, but an operator
    who deletes the volume loses every credential without ever having been told the key
    existed.
  - Consequences: the stack starts and runs normally without a key; only per-project
    credentials are refused, with a 422 naming the variable and the command that generates
    one. The key must be backed up — losing it makes stored credentials unopenable, and
    they have to be entered again.

- Decision: refuse to serve a secret over an insecure channel rather than warn
  - Context: dumb runners reverse the previous design, in which the secret value never left
    the runner host. For a runner to be handed one, the value has to travel.
  - Alternatives considered: send it anyway and log a warning; require TLS globally.
  - Reason: sending it over plaintext would be strictly worse than the runner-side
    environment it replaces, and a warning is not a control. Requiring TLS globally would
    break every existing deployment on upgrade.
  - Consequences: a deployment without TLS is completely unchanged — runners resolve from
    their own environment and simply have no per-project credentials. Turning it on is one
    extra `-f compose.tls.yaml`. The gate defaults to closed, so a caller that forgets to
    pass the flag refuses rather than leaks.

- Decision: an unopenable credential is an error, not a fallback to the shared token
  - Context: `ProjectCodeHostFactory` falls back to the deployment-wide runner when a
    project has no credential. The same path could absorb a credential that fails to
    decrypt (a rotated key, an altered row).
  - Alternatives considered: log and fall back, so a project that used to work keeps
    working.
  - Reason: the fallback identity is the one that could not see the repository in the first
    place. GitHub answers 404, and the operator goes looking for a permissions problem that
    does not exist. Raising puts "stored secret could not be opened" in the project's sync
    error instead.
  - Consequences: rotating `LOOP_SECRET_KEY` without re-entering credentials stops sync for
    the affected projects, visibly, rather than degrading them quietly.

- Decision: omit console sections that have no data source rather than stubbing them
  - Context: the mockup shows an outcomes chart, circuit breakers, a System view and runner
    capacity meters; none has an endpoint (design tasks A7–A12).
  - Alternatives considered: placeholder cards; fabricating values from the data at hand.
  - Reason: `specification.md` §6 forbids silent failures, and a placeholder that looks like
    a reading is worse than an absent one — an operator cannot tell "zero" from "unknown".
  - Consequences: the console is smaller than the mockup until phases A and B land. The full
    list of omissions is in `web/README.md` so nothing is lost.

- Decision: widen the workflow list to the full detail projection instead of adding a
  separate summary shape
  - Context: the console's list, overview and phase threads all need issue titles, pull
    requests and attempt counters.
  - Alternatives considered: keep the four-field summary and fetch detail per row.
  - Reason: one request per row on a list view is a waterfall; the orchestrator already
    computes the fields in a single query.
  - Consequences: `GET /api/v1/workflows` returns a larger payload. Pagination is not yet
    implemented (design task A1), so a very large installation will want it.

## Validation Status

CI run `30701717203` on `238a9c4` — **all 12 jobs green**. This is the first CI run to
complete on `main`; everything below was confirmed by it, not only locally.

- Targeted tests: `web` — 195 vitest tests
- Service tests: `api`; `runner` under `-race`; `orchestrator` — 577 tests
- Postgres integration: 55 tests against a real database, re-runnable and verified against a
  virgin one — the suite used to pass only against the latter
- End to end on the built stack: credentials stored, replaced, removed and re-read through
  nginx → api → gRPC → orchestrator; the plaintext appears in no database row and no
  container log; a missing key is refused with a message naming the variable
- End to end with `compose.tls.yaml`: a runner carrying no GITHUB_TOKEN registers and
  connects over TLS, and `ResolveJobSecret` authenticates its real credential, refuses a job
  it does not hold and refuses an unbacked name. On the same stack without the overlay, the
  identical call is refused for the insecure channel
- Full repository tests: yes, via CI
- Build: `build-web` and `go build`
- Lint: `ruff` and `eslint` clean
- Type checks: `mypy` and `tsc --noEmit` clean
- Database migrations: run by the integration job and by `compose-smoke`
- Docker Compose: `compose-smoke` builds all four images, waits for health and probes both
  readiness endpoints; also exercised locally
- End-to-end workflow: not run — no agent has been driven through a real issue this session.
  The console was checked against a live orchestrator (login, and every endpoint it reads),
  which is short of a delivery.
- Dependency audit: `govulncheck` and `npm audit --audit-level=high` both clean

## Known Issues

- Issue: the local pipeline gate passes by default
  - Severity: high
  - Impact: the product's central claim — deterministic checks decide completion, not the
    agent — is not enforced for any project.
  - Evidence: `app.project_pipeline_steps` is read at
    `orchestrator/src/moirai/persistence/control_plane.py:1382` and written nowhere.
  - Suggested resolution: #114, first item under Pending Implementation.

## Next Recommended Implementation

**#114 — make project pipeline steps configurable.**

Relevant files: `orchestrator/migrations/` (a write path for `app.project_pipeline_steps` if
the schema needs it), `orchestrator/src/moirai/persistence/control_plane.py` (insert/update
alongside the existing read at :1382), `proto/control_plane.proto` and
`orchestrator/src/moirai/grpc/control_plane.py` (carry steps on the project messages),
`api/internal/http/handlers/handlers.go` and `api/openapi.yaml` (project create/update),
`web/src/api.ts` and `web/src/projects.tsx` (the project form already exists — add an ordered
step list to it).

Expected behavior: an operator defines ordered shell steps per project; the local-pipeline
node dispatches them as a pipeline execution; a failing step fails the gate and shows on
workflow detail.

Targeted validation: an orchestrator test for a project whose pipeline fails and must not
advance; an API handler test for round-tripping steps; a `web` test that the project form
submits them. Run `make test-orchestrator`, `cd api && go test ./...` and `make test-web` —
and note that CI will not confirm any of it until the Blocked item is resolved.

## Issue #114 — Project Pipeline Steps

- Status: implemented; awaiting final commit and PR.
- Fail-closed decision: option (b). A terminal pipeline event with
  `pipelineCommandCount: 0` writes `pipeline_passed = false` and
  `blocking_reason = no_pipeline_steps_configured`, routing to repair rather than review.
- Delivered: typed pipeline steps across protobuf, persistence, gRPC, REST, and project UI;
  create/update atomically replace ordered rows in `app.project_pipeline_steps`.
- Validation: `make test-orchestrator` passed; API `go test ./...` passed in `golang:1.25`;
  web typecheck/lint/tests passed in `node:24`; `make lint` passed; `make typecheck` passed.
- Environment note: host `make test-api` fails because Go is absent; host `make test-web` fails
  at ESLint because Node 18 is below its required version. Container equivalents passed.
- Next: stage generated protobuf outputs, run `make proto-check`, commit, push, open and merge PR.

---

## Issue #114 — Pipeline Configuration Merge Completion (2026-08-02)

- Branch/worktree: `issue-114-1` in `.claude/worktrees/issue-114-1`.
- Behavior: project pipeline steps persist and round-trip through protobuf, PostgreSQL, gRPC, REST/OpenAPI, and UI. Required steps populate pipeline task packets in position order.
- Fail-closed: no required steps block immediately with `pipeline_passed = false` and `project has no required pipeline steps`; runner terminal handling also treats an empty pipeline as failed, never passed.
- Validation: `make test-orchestrator` passed (594, 61 skipped); `ruff`, `mypy`, and `make proto-check` passed; API tests passed in `golang:1.25`; web typecheck/lint and 195 tests passed in `node:22`; CI run `30725136308` passed all jobs after one corrected integration-test assertion.
- Review: adversarial review verified contract round-trip, transactional replacement, required-step packet filtering, empty-gate failure, and UI submission. Position preservation was fixed before review completion.

---

## Issue #219 — Per-project execution images (2026-08-02)

- Branch/worktree: `issue-219-b` in `.claude/worktrees/issue-219-b`.
- Delivered: `executionImage` round-trips through project API, gRPC, persistence configuration, and task packets. A configured image selects Docker for both agent and pipeline; empty keeps existing runner-selected execution. Resolved credentials are passed to both; SSH key paths are mounted read-only and key cleanup remains dispatcher-owned.
- Refusal: unavailable Docker returns `execution image <name> cannot run on this runner: Docker executable is unavailable` before workspace preparation.
- Validation: `make test-orchestrator` passed (597, 61 skipped); focused runner packages and integration passed; API Go suite passed; web typecheck, focused 57 tests, and lint passed (16 existing warnings); `git diff --check` passed.
- Pending: commit, push, PR, CI, squash merge.

---

## Provider and subscription credentials for the agent (issue #230)

- Completed: 2026-08-02
- Session: gh-issue-loop / issue-230
- Behavior delivered: a paid or subscription model provider can now be used. A
  credential can be declared, stored per deployment or per project, requested by
  the task packet, resolved by the runner, delivered as a variable or as a file
  under the agent's own home directory, redacted before it reaches the harness,
  and — for a rotating subscription token — written back durably mid-execution.

### What was blocking it, and what changed at each layer

The issue named four layers. All four gave:

1. **`CredentialKind` was `github_token | ssh_private_key`.** Added
   `agent:<NAME>`, where `<NAME>` is the environment variable a harness reads
   the credential from. One namespace, so a stored kind is self-describing and
   the database CHECK is one regular expression
   (`orchestrator/migrations/018_agent_credentials.sql`,
   `orchestrator/src/moirai/domain/credentials.py`). `HOME`, `PATH`, `TMPDIR`,
   the loader variables and the two git names are reserved: a credential under
   one of those would repoint the execution rather than authenticate it.
2. **The packet only ever requested git credentials.** `task_packets.py` now
   takes `agent_credential_refs` and appends them for *every* role — a planner
   without its provider key is as stuck as a developer. The names come from two
   places: `LOOP_AGENT_CREDENTIAL_REFS` on the orchestrator, and whatever the
   project has stored, read from `app.project_credentials` when the packet is
   built. Derived rather than listed twice, so a stored credential cannot go
   unrequested.
3. **`LOOP_RUNNER_ALLOWED_ENVIRONMENT` was a gate with nothing to gate.** It
   stays a gate — a runner decides what it accepts — but the refusal now names
   the variable and prints what is currently on it (`dispatch.go`).
4. **`MinimalEnvironment` overrode `HOME` to the checkout.** It now overrides it
   to `<workspace>/home`, a sibling of the checkout, created 0700 and discarded
   with the workspace. That is the `~` the agent sees, so a file-delivered
   credential written there is found — and, unlike before, nothing a tool writes
   to `~` lands in the tree the agent is about to commit
   (`runner/internal/dispatch/credentials.go`).

### Rotation

`StoreJobSecret` (new RPC) is the write-back, fenced on exactly the same lease
predicate as `ResolveJobSecret` and refused over an insecure channel for the
same reason. It is an **update, never an insert**: a runner may replace a
credential the project already gave it and nothing else. The runner re-reads
each file-delivered credential every 30s and once more when the execution ends,
so a token refreshed inside a run — a developer packet is budgeted at 3600s —
is persisted while the run is still going.

### Redaction

Every resolved value is registered with the event reporter before the workspace
is prepared, and released by `Reporter.Finish`, which runs *after* the terminal
event is built. A provider key matches none of the token prefixes the redactor
knew, which is exactly why the set is fed by what was actually resolved. For a
JSON credentials document each string leaf of 16+ characters is registered too,
so a harness echoing one field of its own config cannot leak it.

### Validation performed

```
make validate MYPY_CACHE=/tmp/moirai-mypy-cache-issue-230   # 663 orchestrator tests, ruff, mypy, compose, overlays, release tags, proto-check
make test-runner                                            # go test -race ./... — all packages
make test-api                                               # go test ./...
make test-web                                               # typecheck, lint, 218 tests
LOOP_TEST_DATABASE_URL=... make test-postgres-integration    # 70 tests, on a throwaway container on port 54823
```

The migration was applied against a real PostgreSQL 16 and its constraints
probed directly: `agent:lowercase`, `nonsense`, and file paths `/etc/passwd`,
`../escape`, `a/../b`, `a//b`, `has space`, `trailing/`, `~/x`, `back\slash`
were each rejected by the CHECK, and a `file_path` on a git kind was rejected.

### Notes

- Per-project provider credentials inherit the TLS requirement that per-project
  git credentials already had: the value travels on the control stream, and the
  orchestrator will not send one in the clear. Stated in the README rather than
  discovered when a key silently fails to resolve.
- Compose cannot name an environment variable it does not know, so
  `OPENROUTER_API_KEY` and `ANTHROPIC_API_KEY` are wired explicitly and any other
  provider name needs a line in an override file. `MOIRAI_AGENT_CREDENTIAL_NAMES`
  appends to the runner allow-list; `MOIRAI_AGENT_CREDENTIAL_REFS` declares them
  on the orchestrator.
- A deployment-wide key lives in the runner's environment and therefore has
  nowhere durable for a rotation to go back to. Rotation write-back requires the
  credential to be stored per project; the runner logs the rotation it could not
  persist rather than failing the execution over it.
## Issue #218 — Execution environment declaration (2026-08-02)

- Branch/worktree: `issue-218` in `.claude/worktrees/issue-218`.
- Scope: the piece of #218 that is concrete now that #219 has landed — declaring the available
  toolchain to the agent, and extending the CI image assertion to the agent-facing toolchain.
  Execution modes (#220/#221/#222) and mise (#223) stay out of scope, and no shared contract
  (`proto/`, `gen/`, migrations, task packets) was touched: the image declares its own contents.
- Delivered:
  - `runner/toolchain.json` — the runner image's own declaration, published in the image at
    `/etc/moirai/toolchain.json`. It lists the tools the image offers *and* the ones it
    deliberately lacks (`python3`, `make`, `go`, `docker`, `bash`, `curl`, `jq`, `gcc`, `cargo`,
    `java`, `pip`, `patch`), each with a reason, which is the half that stops the probe.
  - `runner/internal/toolchain` — loads, validates, verifies and renders a declaration. Rejects
    unknown fields, a later `schemaVersion`, and a tool declared both present and absent.
  - `runner/internal/dispatch/toolchain.go` + one call site in `goalgate.go` — appends an
    `# EXECUTION ENVIRONMENT` section to `.loop/prompt.md` and to the prompt handed to backends
    that take one on their command line, continuations included. Resolved once per execution.
    When the agent runs in a container image instead of the runner's filesystem (packet
    `executionImage`, or a configured `docker` backend), the image name and the conventional
    manifest path are declared rather than the runner's own contents — describing the wrong
    machine would be worse than describing none.
  - `runner toolchain [--verify]` — one implementation, three readers: the image build, CI, and
    anyone reading the declaration back. `--verify` resolves on the PATH the *agent* is given
    (`execution.MinimalEnvironmentMap`) and fails both ways: declared-but-missing, and
    installed-but-declared-absent.
  - `runner/Dockerfile` — copies the manifest to `/etc/moirai/toolchain.json` and runs
    `/runner toolchain --verify`, so an image whose declaration lies cannot be built.
  - `.github/workflows/ci.yml` — new step `Verify the toolchain the runner image declares to the
    agent`, asserting the declaration against the started container. `c2dd82e` asserted `git`,
    `ssh`, `opencode` — what the *runner* shells out to; this closes the agent-facing half.
  - `runner/README.md` — new "Execution Environment Declaration" section; `runner toolchain`
    documented under Health Probes.
- Validation actually run (all passing):
  - `make test-runner` (`go test -race ./...`, all 12 packages ok).
  - `cd runner && go vet ./...`, `gofmt -l` clean on every file touched.
  - `docker build -f runner/Dockerfile -t moirai-runner-issue-218:test .` → exit 0; the build-time
    `--verify` step printed the declaration and passed against the real image.
  - Negative proof, against the built image with a tampered manifest bind-mounted:
    declaring `python3` present → exit 1, `declared but not installed: python3`; declaring `node`
    absent → exit 1, `declared absent but installed: node`. Shipped manifest → exit 0.
  - `docker run … /runner live` → `{"status":"live"}`, so the new subcommand does not shadow the
    health probes.
  - `make test-orchestrator`, `make lint`, `make typecheck MYPY_CACHE=/tmp/moirai-mypy-cache-issue-218`,
    `make compose`, `make compose-overlays`, `make test-release-tags`, `make proto-check`.
- Decision: the declaration lives in the image, not in the task packet. It keeps the control plane
  out of describing an image it did not build, works unchanged under any of #220/#221/#222, and is
  the only design under which a per-project execution image can say what it is.
- Known limitation: the runner does not read a *remote* execution image's manifest — that would
  cost a container start per execution — so for those the agent is pointed at
  `/etc/moirai/toolchain.json` inside its own image instead of being handed the contents.

---

## Issue #226 — Orchestrator proxy headers (2026-08-02)

- Branch/worktree: `issue-226-b` in `.claude/worktrees/issue-226-b`.
- Delivered: `LOOP_ORCHESTRATOR_HEADERS` and preferred `_FILE` JSON sources; metadata keys normalize to lowercase, `PerRPCCredentials` adds them to unary and stream RPCs, and file values reload at each RPC for token rotation and reconnect.
- Safety: headers reject without TLS during load even when their file is unreadable; header file failures expose no values; authentication statuses stay permanent while transport statuses retry through existing stream supervision.
- Documentation: `runner/README.md` has Cloudflare Access service-token and HTTP/2 Tunnel origin example.
- Validation: focused race tests for config, dialer, control stream/reconnect, and main passed in `golang:1.25`; `make build-runner` passed in `golang:1.25`; `make lint typecheck` passed. Full control suite currently fails pre-existing `TestEventReporterRestoresEvictedEventWhenPersistFails` with `Emit(completed) = (3, disconnected), want a persist failure`.
- Review: adversarial review found TLS refusal was masked by an unreadable header file; fixed and added coverage.
- Pending: commit progress entry, push, PR, CI, merge.
