# Implementation Progress

## Current Status

- Overall status: MVP surface substantially complete — 34 of 35 roadmap items in
  `tasks/todo.md` are done (three with caveats). #197 is fixed and can be closed; 4 issues
  remain open.
  **CI is green for the first time**: all 12 jobs passed on `238a9c4`, so `main` is
  independently verified rather than merely believed.
- Current phase: Implementation. #197 landed with the CI work, so 4 issues remain open.
- Active implementation: none. Per-project credentials are complete on both sides — the
  orchestrator uses a project's own credential for issue sync and pull requests, and a runner
  is handed one per job over TLS rather than being provisioned with a shared token.
- Last updated: 2026-08-01
- Agent/session identifier: per-project-credentials / 2026-08-01

## In Progress

_Nothing is claimed. The next agent should take the first item under Pending Implementation._

## Done

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

---

## Issue #118 — SSE dashboard updates

- Completed: 2026-08-02
- Agent/session: issue-118-b
- Delivered: authenticated `GET /api/v1/events`; `StreamEvents` server-streaming ControlPlane RPC; PostgreSQL commit-aligned notifications from `workflow_events` and `runners`; generated stubs; EventSource-backed console snapshot updates; nginx streaming proxy settings; OpenAPI contract.
- Tests: API handler test covers 401, event delivery, keepalive, and disconnect cancellation. Orchestrator gRPC test covers authenticated stream delivery. Web workflow test covers in-place pushed update; unavailable EventSource leaves initial fetch intact.
- Validation: `make test-orchestrator` (594 tests), `make lint`, `make typecheck`, `make proto-check`, `docker compose config --quiet`; API suite passed in `golang:1.25`; web typecheck/lint and 196 tests passed in `node:24`.
- Environment: direct `make test-api` cannot run because host has no `go`; direct `make test-web` cannot run because host Node 18 lacks `node:util.styleText`. Container equivalents passed.
- Review: adversarial staged diff review found no unresolved auth, teardown, payload, or transaction-boundary defects.
