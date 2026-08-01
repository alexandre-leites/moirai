# Implementation Progress

## Current Status

- Overall status: MVP surface substantially complete — 34 of 35 roadmap items in
  `tasks/todo.md` are done (three with caveats), 5 GitHub issues remain open (2 of them P1).
  The blocking problem is not a feature: **CI has not completed a run on `main` in over a
  week**, so nothing shipped in that period was independently verified.
- Current phase: Implementation, with an infrastructure blocker taking precedence.
- Active implementation: none — the console revamp completed and was pushed to `main` as
  `acd7ad3` on 2026-08-01.
- Last updated: 2026-08-01
- Agent/session identifier: console-revamp / 2026-08-01

## In Progress

_Nothing is claimed. The next agent should take the first item under Pending Implementation._

## Done

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

- [ ] Continuous integration is not executing on `main`
  - Reason: `ci.yml` targets `[self-hosted, linux]`. No runner appears to be picking jobs up.
  - Evidence: the eleven most recent CI runs on `main` all ended `cancelled`; the run for
    `acd7ad3` (2026-08-01 11:40Z) was still `queued` twenty minutes after creation. The only
    `success` runs on the repository are Dependabot and Dependency Graph jobs, not CI. With
    `concurrency.cancel-in-progress: true`, a run that never starts is cancelled by the next
    push, which matches the pattern exactly.
  - Attempts made: inspected run history and `ci.yml`; `gh api .../actions/runners` returns
    404, so the runner registration could not be inspected with the available token.
  - Required resolution: a human with repository admin access needs to confirm whether a
    self-hosted runner is registered and online, and either bring one up or move `ci.yml` to
    GitHub-hosted runners.
  - Independent work still available: yes — everything under Pending Implementation. Run
    `make test`, `make lint`, `make typecheck` and `make proto-check` locally in the
    meantime, and do not treat a closed issue as evidence of working software.

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

- [ ] #197 (P2) — enforce user role on runner and workflow mutations server-side
  - Priority: next. Security-adjacent and currently misleading.
  - Dependencies: none.
  - Expected behavior: a `viewer` session receives 403 from every mutating endpoint. The
    console already hides these controls, so this closes the gap between presentation and
    enforcement.
  - Definition of done: `test_runner_controls_require_admin_csrf_and_persist_actor` passes —
    it is currently failing for exactly this reason.

- [ ] #118 (P1) — Server-Sent Events end to end
  - Priority: after the two above.
  - Dependencies: none, but it replaces the interim polling in `web/src/poll.ts`, which was
    written behind an interface so the swap touches the hook and not any view.
  - Expected behavior: a workflow transition appears in an open console without a reload;
    killing the stream falls back to polling; reconnect replays from `Last-Event-ID`.
  - Definition of done: `StreamEvents` RPC → API SSE proxy → `useEventStream` hook, plus
    `proxy_buffering off` for `/api/v1/events/` in `web/nginx.conf`.

## Quality Backlog

- [ ] Fix the four pre-existing failures on `main`
  - Category: correctness / CI hygiene
  - Risk: low; all four are small and localized
  - Expected benefit: a green local `make validate`, which is currently unreachable and
    therefore hides new breakage
  - Recommended timing: alongside whichever task next touches these files
  - Detail: `test_metrics` (2 cases) expects a snapshot without the `scheduled_jobs` key the
    code now returns; `test_runner_controls_require_admin_csrf_and_persist_actor` is #197;
    one ruff error (`except asyncio.TimeoutError` → `except TimeoutError`) and two mypy
    errors (`metrics_snapshot` missing from the `ControlPlane` protocol, an unawaited
    coroutine), all in `orchestrator/src/moirai/grpc/control_plane.py`.

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

Recorded for the 2026-08-01 session only. **No CI run has confirmed any of this** — see
Blocked.

- Targeted tests: `web` — 187 vitest tests, all passing
- Service tests: `api` — `go test ./...` passing; `orchestrator` — 518 tests, 3 failures, all
  pre-existing and unrelated (verified by stashing the change set and re-running)
- Full repository tests: not run (`make test` includes the runner suite, not exercised)
- Build: `npm run build` and `go build ./...` pass
- Lint: `eslint` clean; `ruff` has 1 pre-existing error
- Type checks: `tsc --noEmit` clean; `mypy` has 2 pre-existing errors, down from 5
- Database migrations: not run this session
- Docker Compose: not run this session
- End-to-end workflow: not run this session; the console was exercised against a stub API,
  not a live orchestrator

## Known Issues

- Issue: CI is not executing on `main`
  - Severity: critical (process)
  - Impact: nothing merged in the last week was independently verified. Two broken commits
    reached `main` as a direct result — unresolved conflict markers in `web/src/workflows.tsx`
    and a gutted `api/internal/http/handlers/workflows.go` that left the Go API uncompilable.
  - Evidence: eleven consecutive `cancelled` runs; the newest run queued 20+ minutes.
  - Suggested resolution: see Blocked.

- Issue: the local pipeline gate passes by default
  - Severity: high
  - Impact: the product's central claim — deterministic checks decide completion, not the
    agent — is not enforced for any project.
  - Evidence: `app.project_pipeline_steps` is read at
    `orchestrator/src/moirai/persistence/control_plane.py:1382` and written nowhere.
  - Suggested resolution: #114, first item under Pending Implementation.

- Issue: role separation is presentation-only
  - Severity: medium
  - Impact: a `viewer` cannot see admin controls in the console but can still call the
    endpoints directly. Do not describe roles as a security boundary until fixed.
  - Evidence: #197; `test_runner_controls_require_admin_csrf_and_persist_actor` fails.
  - Suggested resolution: #197, second item under Pending Implementation.

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
