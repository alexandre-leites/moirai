# Implementation Progress

## Current Status

- Overall status: MVP surface substantially complete — 34 of 35 roadmap items in
  `tasks/todo.md` are done (three with caveats). #197 is fixed and can be closed; 4 issues
  remain open.
  **CI is green for the first time**: all 12 jobs passed on `238a9c4`, so `main` is
  independently verified rather than merely believed.
- Current phase: Implementation. #197 landed with the CI work, so 4 issues remain open.
- Active implementation: Go V1 orchestrator replacement.
- Last updated: 2026-08-02
- Agent/session identifier: refactor/go-orchestrator-v1 / 2026-08-02

## In Progress

_Nothing is claimed._

## Go V1 Orchestrator Refactor

- [x] Replace Python/LangGraph orchestrator with Go V1
  - Completed: 2026-08-02
  - Relevant files: `orchestrator/`, `Makefile`, `compose.build.yaml`, `compose.yaml`, `runner/`
  - Behavior delivered: deleted Python/LangGraph source and tests; Go control-plane and runner-stream gRPC services preserve existing protobuf contracts, auth, projects, credentials, runner tokens, runner secret fencing, workflow events and operator controls. The state machine dispatches runner-owned worktree/OpenCode/commit/push work, creates PRs, waits for green checks, merges, and completes issues. No automatic retries or deadlines; manual retry remains. `timeoutSeconds: 0` means no runner execution deadline.
  - Migration: standard `golang-migrate` replaces handwritten migration logic; it baselines compatible legacy `app.schema_version` databases without replaying SQL.
  - Not implemented in V1, though specified in `PROJECT.md`: planning, deterministic local pipeline, independent AI review, repair loops, human approval. `SubmitHumanDecision` returns `FailedPrecondition`.

- [x] Review remediation for the Go V1 orchestrator
  - Completed: 2026-08-02
  - Relevant files: `.github/workflows/ci.yml`, `.github/dependabot.yml`, `Makefile`, `orchestrator/`, `README.md`, `PROJECT.md`, `docs/architecture.md`
  - Why: the branch reported zero CI checks and three of the suites `make test` claims to run were silently skipped, so nothing about the rewrite had actually been verified by CI.
  - Verification fixed first: `ci.yml` was invalid YAML (one over-indented step), so Actions parsed no workflow at all; `test-runner`, `test-api` and `test-web` were `.PHONY` with no recipe, which make reports as success, so CI's web job passed without running typecheck, eslint or vitest.
  - Correctness and safety fixed: empty or unrecognised GitHub check rollups were read as green, which squash-merged agent code before CI had run — an empty rollup is now pending and legacy `StatusContext` entries are decoded; the only task packet built was a `planner`, which the runner forbids from modifying or pushing, so the agent branch was never published and every pull request was opened from a branch the remote did not have; `RetryWorkflow` took the project lock and parked the run in a `recovering` status nothing reads, permanently wedging that project; a failed run left its issue eligible, so the scheduler re-created it every tick despite "no automatic retries"; accepting an offer had no job-status guard, so a late acceptance revived an administratively cancelled job with a fresh lease and regained secret access.
  - Reliability added: a recovery sweep at startup and every 30s reclaims expired leases, resumes deliveries interrupted between a runner's completion and its pull request, and marks disconnected runners offline — each of those otherwise held a project lock forever with no in-product recovery. Offer-delivery cleanup is now one transaction on a shutdown-proof context. Issue sync runs on a timer instead of only when an operator presses "Sync now".
  - Security: gRPC TLS is served again (`compose.tls.yaml` configured a certificate the Go server never read, so api and runner were told to require TLS against a plaintext listener); half-configured TLS is refused rather than downgraded; `gh` failures are redacted before reaching operator-visible fields; loader-hijacking names (`PATH`, `LD_PRELOAD`, `GIT_SSH_COMMAND`, …) are refused as agent credential names again; `agent:blocked` and `agent:delivered` stop autonomous work again.
  - Also: the legacy migration ledger check would have failed every startup once migration 019 landed; `GetSystemVersion` started requiring a session the API's public health endpoint cannot present, blanking the reported version; the container healthcheck dialled a hardcoded port; the unused `internal/workflow` package (whose state vocabulary the server never writes) was deleted; orchestrator tests now run under `-race`.
  - Validation performed: `make lint`, `make typecheck`, `make test-orchestrator` (with `-race`), `make test-runner`, `make test-api`, `make test-web` (typecheck + eslint + 221 tests), `make compose`, `make compose-overlays`, `make test-release-tags` all pass; all four workflow/dependabot YAML files parse; every internal markdown link and anchor resolves; a fresh isolated Compose build reaches healthy for PostgreSQL, orchestrator, API, runner and web, and the CI smoke checks pass against it; the dispatched task packet was fed through the runner's own `taskpacket.Parse` and accepted as a developer packet with `mayPush`.
  - Notes: the runner race suite has a pre-existing unrelated failure in `TestEventReporterRestoresEvictedEventWhenPersistFails` only when run as root.

## Done

- [x] #244 — use full available width for main app content
  - Completed: 2026-08-02
  - Relevant files: `web/src/styles.css`
  - Behavior delivered: removed both the 1240px cap and temporary 90% rule; block-level main children use their default full available width.
  - Research: existing grid, table, form, detail, log, sidebar, padding, and mobile breakpoint rules remain parent-relative or independent of this width.
  - Validation performed: TypeScript typecheck, ESLint (0 errors; 16 existing warnings), production build, and `git diff --check`.
  - Commands executed: `npm run typecheck`, `npm run lint`, `npm run build`, `git diff --check`.

- [x] Workflow execution-error visibility
  - Completed: 2026-08-02
  - Relevant files: `web/src/status.ts`, `web/src/workflow-detail.tsx`, `web/src/workflow-detail.test.tsx`
  - Behavior delivered: workflow detail deduplicates and renders terminal runner error payloads in an `Execution errors` card; no card appears when events have no error.
  - Validation performed: focused workflow-detail test (15 pass), TypeScript typecheck, ESLint (0 errors; 16 existing warnings), `git diff --check`.
  - Commands executed: `npm test -- workflow-detail.test.tsx`, `npm run typecheck`, `npm run lint`, `git diff --check`.

- [x] Unknown system-version fallback
  - Completed: 2026-08-02
  - Behavior delivered: remote Git-context Docker builds no longer require a build SHA; core
    System versions rows render `Unknown` when metadata is absent, while blank runner versions stay hidden.
  - Validation performed: `docker compose -f compose.yaml -f compose.build.yaml build api web` with no SHA;
    12 overview tests and web typecheck.

- [x] Non-blocking CI deployment webhook
  - Completed: 2026-08-02
  - Relevant files: `.github/workflows/ci.yml`
  - Behavior delivered: HTTP webhook errors emit `::warning::` but do not fail CI.
  - Validation performed: Python YAML parse, `git diff --check`, and a failing curl shell check pass.
  - Commands executed: `python3 -c 'import yaml; yaml.safe_load(open(".github/workflows/ci.yml"))'`, `bash -e -c 'curl() { return 22; }; if ! curl ...; then ...; fi'`, `git diff --check`.


- [x] CI deployment webhook notification
  - Completed: 2026-08-02
  - Relevant files: `.github/workflows/ci.yml`
  - Behavior delivered: successful `main` CI aggregate validation posts repository, ref, SHA, actor, and run ID to `DEPLOY_WEBHOOK_URL`, with optional Cloudflare Access service-token headers.
  - Validation performed: Python YAML parse and `git diff --check` pass.
  - Commands executed: `python3 -c 'import yaml; yaml.safe_load(open(".github/workflows/ci.yml"))'`, `git diff --check`.


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

- #244 targeted validation: `npm run typecheck`, `npm run lint` (0 errors; 16 existing warnings), `npm run build`, and `git diff --check` pass for full-width main content.
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

## Known Issues — Go V1 orchestrator (reported, not fixed on this branch)

- `StreamEvents` replays the entire `workflow_events` table when a client
  connects without a cursor, one workflow query per row, and never emits runner
  events, so the console's runner page has no live updates.
- `workflow_runs.status` is an untyped string vocabulary spelled out in raw SQL
  in several places, with no Go type and no CHECK constraint — the one status
  column in the schema without one. The console additionally carries ten
  statuses the orchestrator never writes.
- `completed` means two things: "the runner finished" and "the pull request is
  merged". They are distinguished only by whether a project lock still exists,
  which is why the stranded-delivery sweep needs an age bound. Splitting out a
  `delivering` status would let a partial unique index on `workflow_runs`
  replace the hand-maintained `app.project_locks` table entirely.
- Retry creates a new workflow run with no link to the one it replaces, so the
  console cannot show attempts of the same logical work, and the retry toast
  still promises prior-failure context that V1 does not carry.
- `app.issues.eligible` now has two writers: the `agent:ready` label on first
  sync, and the orchestrator's own lifecycle afterwards. Writing the label back
  to the tracker, or a `superseded_at` column on the run, would restore a single
  owner. `docs/architecture.md` records the same hazard for `runners.draining`.
- `databaseError` collapses every cause into `"database operation failed"`, and
  is applied to JSON decoding failures too. There is no logging in the `server`
  package, so production failures are undiagnosable.
- `CancelWorkflow`/`BlockWorkflow`/`SetRunnerState` update the database but never
  send `CancelExecution`/`DrainRunner` to the runner, so in-flight work keeps
  running until its lease lapses.
- `existing_path` projects are accepted at creation but can neither sync issues
  nor deliver, because both paths require a repository URL that the mode forbids.
- The human approval gate is unimplemented while the console still renders its
  decision panel.
- The hex branch of `LOOP_SECRET_KEY` decoding is unreachable: a 64-character
  hex key is also valid base64, so it decodes to 48 bytes and is rejected.

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

---

## Issue #288 — Documentation and comments still describe the deleted Python/LangGraph engine (2026-08-03)

- Branch/worktree: `issue-288` in `.claude/worktrees/issue-288`. Documentation and comments only —
  no runtime behavior changed; the only regenerated artifact is a proto comment carried into
  `gen/go`.
- `AGENTS.md` (done first — it is the file that misdirects future agents):
  - §4 "A required LangGraph node or transition" → "orchestrator workflow phase or transition";
    §5 "LangGraph nodes without routing" → "Workflow phases the orchestrator can enter but never
    leave"; §5 vertical-slice flow `issue → LangGraph → agent` → `issue → orchestrator state
    machine → agent`.
  - §6 implementation sequence: "5. Python orchestrator foundation" → "Go orchestrator
    foundation"; "27. LangGraph workflow state" / "28. LangGraph checkpoint persistence" → "Go
    workflow state machine" / "PostgreSQL-persisted workflow state and restart recovery"; items
    29–32 renamed from LangGraph "node" to "phase".
  - §7 "changed Python workflow node → run workflow routing and checkpoint tests" → "changed an
    orchestrator workflow transition → run orchestrator state-machine and recovery tests".
  - §17 MVP completion criterion "LangGraph persists and resumes workflows" **replaced, not
    deleted**, with the property the Go engine actually provides: "The orchestrator's Go state
    machine persists every workflow transition to PostgreSQL and recovers interrupted runs after a
    restart: resuming the ones that can continue, and releasing the project lock for the ones that
    cannot." Verified against `orchestrator/cmd/orchestrator/main.go` (`ReconcileDatabaseOnce` at
    startup, then `RecoverOnce` every 30s and `ObserveWorkflows` every 15s) and
    `orchestrator/internal/server/recovery.go` (`resumeStrandedDeliveries` resumes;
    `reclaimExpiredLeases` terminates and frees the lock).
  - The engineering rules at §12 ("structured planner, developer, and reviewer results", "bound
    retries and repair loops") were left as-is: they name roles the task-packet contract still
    defines (`runner/internal/taskpacket/taskpacket.go` — planner, developer, pipeline, reviewer,
    repairer), so they are forward-looking rules rather than stale LangGraph references.
- `runner/README.md`:
  - `LOOP_RUNNER_MAX_CONTINUATIONS` and the "Bounds" paragraph no longer claim `timeoutSeconds`
    bounds an execution. New paragraph states the truth: the orchestrator hardcodes
    `timeoutSeconds: 0` (#276), `execution.Supervisor`/`execution.DockerExecutor` attach a
    deadline only when the timeout is positive, and the zero deadline is already spent when the
    first attempt ends, so no continuation is ever funded and an undelivered run reports
    `execution time budget exhausted`. Verified empirically with a throwaway in-package test
    (`TimeoutSeconds = 0` → `invocations=1 continuations=0 gateVerdict="not delivered (execution
    time budget exhausted): the agent reported remaining work" agentTimeout=0s`); the scratch file
    was deleted, not committed. Fixing the timeout itself is #276 and stayed out of scope.
  - Deleted Python symbols replaced with the Go ones: `workflows/runner_events.py` → the truth
    that `persistExecutionEvent` (`orchestrator/internal/server/server.go`) ends a run at the
    terminal event's own type, so an agent block lands as `failed` with `blocked: true` preserved
    in the `app.workflow_events` payload; `expire_leases` → `reclaimExpiredLeases`
    (`orchestrator/internal/server/recovery.go`).
  - Two more stale claims found in the same file and fixed: "A planning node is allowed two", and
    "The orchestrator exports a series of the same name" — the Go orchestrator has no Prometheus
    surface (`grep -rl prometheus orchestrator/` is empty).
- `PROJECT.md`: the review/repair/approval references at the three sites without a caveat now
  carry one, consistent with the two that already did. The approval note names the observable
  fact — `SubmitHumanDecision` returns `FailedPrecondition "V1 has no approval phase"`
  (`orchestrator/internal/server/management.go`).
- `docs/design/web-console/specification.md`: §2.1 only — "workflows run on LangGraph
  `thread_id`s" → each run is a durable thread of state carrying its own `thread_id` in
  `app.workflow_runs` (the Go orchestrator sets it at insert, `server.go`). The approved design
  contract was not restructured or re-scoped. Its remaining Python symbol references (§4.2
  `persistence/control_plane.py`, §4.3 `domain/scheduling.py`, `health.py`) are deliberately
  untouched and filed as #298.
- `docs/design/web-console/tasks.md`: A1/A5/A8 pointed implementers at deleted Python files.
  Pointers corrected to the Go locations; A8's acceptance criterion no longer requires parity with
  a health file that no longer exists. No task was re-scoped.
- Live source comments citing deleted Python paths, all comment-only:
  `proto/control_plane.proto` (+ regenerated `gen/go/gen/control/v1/control_plane.pb.go`),
  `api/internal/http/server.go`, `runner/internal/metrics/metrics.go`, `web/src/status.ts` (×3,
  including the LangGraph "node"/"checkpoint" wording at `reachedPhase`), `web/src/format.ts`,
  `web/src/runner-status.test.ts`. Also `runner/internal/dispatch/goalgate.go`, which made the
  same unbounded-timeout claim as the README.
- `orchestrator/migrations/001_initial.sql` and `002_langgraph_checkpointer.sql`: comments only,
  marking the `langgraph` schema as dead-but-unrewritable so the next agent grepping `langgraph`
  does not read it as work to do. No DDL changed.
- Commands executed (all from the worktree):
  - `make lint` → pass. `make typecheck` (`go vet ./...`) → pass.
  - `make test-orchestrator` → ok (4 packages). `make test-runner` → ok (12 packages).
    `make test-api` → ok (5 packages). `make test-web` → typecheck + lint (16 pre-existing
    `react-refresh` warnings, 0 errors) + 221 tests in 16 files, all passing.
  - `make proto-generate` then `make proto-check` → pass (the only `gen/go` delta is the comment).
  - `make test-release-tags` → 21/21. `make compose` → pass. `make validate` → passes except
    `compose-overlays`' `render-tls-stack.sh --check`, which fails **only** because
    `docker compose config` derives the project name from the checkout directory: the rendered
    file says `name: issue-288` where the committed one says `name: moirai`. Proven with
    `diff <(docker compose -f compose.yaml -f compose.tls.yaml config --no-interpolate)
    <(tail -n +25 compose.tls-stack.yaml)` — five lines, all the project name — and
    `COMPOSE_PROJECT_NAME=moirai sh scripts/render-tls-stack.sh --check` →
    "compose.tls-stack.yaml is up to date". A worktree-path artifact, not a regression; CI checks
    out as `moirai`.
- Known issues found while verifying (filed, not fixed — out of scope here):
  - #296 — the Go orchestrator exports no Prometheus metrics, so the queue-depth, active-workflow and
    fleet-heartbeat series #124 moved off the API and runner are exported by nobody.
  - #297 — a runner's agent-declared block ends its run as `failed`; the orchestrator never
    derives the terminal `blocked` status or `blocking_reason` from it.
- Adversarial self-review of the diff (separate agent, told to hunt for corrections that are still
  false, missed references, a deleted rather than replaced MVP criterion, and behavior smuggled
  into a comment edit). It found five real defects in my own rewrite; all are fixed on this
  branch:
  1. The `describeEvent` comment claimed "every branch is driven by a field the writers actually
     store". False: the branches were written against the Python envelope
     (`{job_id, runner_id, …, payload: {…}}`), while `persistExecutionEvent` stores the runner's
     payload flat and the API passes it through verbatim
     (`api/internal/http/handlers/workflows.go`). So `executionError` reads a `payload.payload`
     that never exists, `started` reads `runner_id`, `failed` reads `exit_code` against the
     runner's `exitCode`, and three branches key on event types the Go orchestrator never writes
     while `pull_request.created`/`pull_request.merged`/`delivery.failed` have no branch at all.
     The comment now states that mismatch, `logText`'s envelope note with it, and the rendering
     bug is filed as #300. (`log` events do still render: `EmitLog` writes a top-level `message`.)
  2. `web/src/format.ts` claimed the unmatched hold reasons were "the specified set it is to
     grow". They are neither: they are what the Python queue emitted
     (`git grep provider_circuit_open 7132e24^`), and the specification names a third vocabulary
     again. Corrected to say the map is deliberately the union.
  3. `runner/README.md` "the planning phase is specified to allow two" — nothing specifies it in
     V1, and the consequence was wrong: one non-delivering execution parks the issue (`parkIssue`)
     rather than two. Reframed as the Python-era budget it was, with V1's harsher rule stated.
  4. `VALID_EVENT_TYPES` (deleted with `workflows/runner_events.py`) survived six lines above the
     reference I did fix, plus two live Go comments. All three now name `validEventType`.
  5. Two internal contradictions of my own: "increments most of them" vs "never increments"
     (never is right), and `tasks.md` A1's "plus" list naming fields `ListWorkflows` already
     returns.
  The review confirmed clean: no MVP criterion dropped (15 bullets before and after), and nothing
  outside comments changed in `.go`/`.ts`/`.proto`/`.sql`/`gen` — the sole non-comment edit is one
  `it(...)` description rename in `web/src/runner-status.test.ts`, assertions untouched.
  It also flagged two overclaims I had already caught and fixed independently (AGENTS.md §17
  "resumes" → recovers-and-releases, PROJECT.md's repository-change gate).
- Second validation pass after the review fixes: `make lint`, `make typecheck`,
  `make test-orchestrator`, `make test-runner`, `make test-api`, `make test-web` — all pass. CI on
  PR #299 was green on the first push (12/12 including `validate`, `compose-smoke` and
  `test-postgres-integration`) and re-ran on each subsequent push.

## Issue #296 — The orchestrator exports no Prometheus metrics (2026-08-03)

- Agent/session identifier: gh-issue-loop agent, branch `issue-296`, worktree
  `.claude/worktrees/issue-296`, started from `origin/main` at `8b47529`.
- Problem, verified before writing anything: `grep -rl prometheus orchestrator/` returned nothing.
  #124 removed `moirai_queue_depth`, `moirai_active_workflow_count` and
  `moirai_runner_heartbeat_age_seconds` from the API and the runner because they were gauges
  `Set(0)` once at construction and never written again, on the grounds that only the orchestrator
  — which owns the database — may export them. #247 then deleted the Python orchestrator that did
  (`orchestrator/src/moirai/observability.py`, confirmed with `git show 7132e24^:...`), and the Go
  rewrite shipped with no metrics surface at all. So the three series existed nowhere.
- Behavior delivered — the orchestrator now serves Prometheus metrics:
  - `orchestrator/internal/metrics/metrics.go` (new package). A `prometheus.Collector` that reads
    the database **at scrape time** and emits `moirai_queue_depth`, `moirai_active_workflows`,
    `moirai_scheduled_jobs`, `moirai_enabled_runners` and the fleet-wide
    `moirai_runner_heartbeat_age_seconds`, plus process-side
    `moirai_orchestrator_loop_runs_total{loop,result}`,
    `moirai_orchestrator_loop_last_success_age_seconds{loop}` and
    `moirai_orchestrator_metrics_scrape_errors_total`. `Server.Start()` binds eagerly and serves in
    a goroutine; `Shutdown(ctx)` stops it.
  - `orchestrator/internal/config/config.go`: `LOOP_METRICS_BIND`, defaulting to `0.0.0.0:9090` —
    the port the deleted Python orchestrator used, so an existing scrape config keeps working, and
    distinct from the runner's `:9091`. `os.LookupEnv`, not `os.Getenv`: unset means the default
    (metrics are on unless turned off, because nothing else exports these series), and an
    explicitly empty value disables the listener. A value that is not `host:port` is refused the
    same way `LOOP_GRPC_BIND` is.
  - `orchestrator/internal/server/server.go`: `GetSchedulerMetrics`'s query became
    `readSchedulerSnapshot`, now also returning the enabled-runner count and the oldest enabled
    heartbeat age; `MetricsSnapshot` maps it for the exporter. **One query, one round trip, shared
    by both callers** — the RPC and the scrape cannot disagree about what "queue depth" means, and
    no second query path was added.
  - `orchestrator/cmd/orchestrator/main.go`: starts the listener after bootstrap and before
    `grpcServer.Serve`, shuts it down in a `defer` placed so `/metrics` stays scrapeable through
    the whole gRPC graceful drain and closes before the pool does. The recovery sweep and issue
    sync are wrapped by `observed(...)`, which reports each pass and passes the error through
    untouched.
- Decisions, and why:
  - **Read at scrape time, never cached.** The previous orchestrator refreshed four gauges from a
    snapshot loop that raised on every tick, so all four served their initial zero for the life of
    the process (#195). A collector has no tick to fail.
  - **A failed read omits the state series instead of exporting zero**, and counts it in
    `moirai_orchestrator_metrics_scrape_errors_total`. Exporting zero would say "the queue is
    empty" and "every runner just checked in", which is the exact lie #124 exists to remove. The
    loop series survive a failed read, because they come from process state.
  - **Heartbeat age is `MIN` over `enabled AND revoked_at IS NULL`** — oldest, not newest, and not
    over runners an operator took out of service. Computed by PostgreSQL from its own clock, so
    orchestrator clock skew cannot make a heartbeat look fresher than it is. With no enabled
    runner the series is absent rather than zero; `moirai_enabled_runners` is what makes that
    absence readable.
  - **A bind failure stops the process** rather than being logged and dropped (the runner's metrics
    server discards its `ListenAndServe` error). An endpoint that silently never listens is
    indistinguishable from a healthy one nothing is scraping — the same failure mode as this issue.
    `LOOP_METRICS_BIND=""` is the supported way to serve none.
  - **The scrape-error counter is emitted by the collector, not registered as a `prometheus.Counter`.**
    A registered counter is gathered concurrently with the collector that increments it, so the
    scrape that failed could report the count from *before* its own failure. This was caught by a
    failing integration test, not by inspection.
  - **No age series for the `unknown` loop bucket.** It absorbs a mislabelled count; nothing ever
    succeeds under that name, so its age would grow forever by construction and fire any alert
    written against the family. Caught while reading a live scrape.
  - **A pass cut short by shutdown is not counted as a failure.** `observed` skips recording when
    `ctx.Err() != nil`, matching what `every` already does with its logging; counting it would make
    every clean restart look like a reconciliation failure.
  - `moirai_active_workflows`, not the Python-era `moirai_active_workflow_count`: the issue
    specifies the shorter name, it matches the proto field, and `_count` is a reserved suffix.
  - **`moirai_queue_depth` counts the scheduler's candidate set, not "work not yet started".** An
    issue stays `eligible` while its workflow runs — `parkIssue`/the delivery path clear it only at
    the end — so at most one in-flight issue per project is included. The help text and the README
    row say so rather than implying a pure backlog; it is the same number the console shows,
    because it is the same query.
- Relevant files: `orchestrator/internal/metrics/metrics.go` (+ `metrics_test.go`),
  `orchestrator/internal/config/config.go` (+ test), `orchestrator/internal/server/server.go`,
  `orchestrator/internal/server/integration_test.go`, `orchestrator/cmd/orchestrator/main.go`
  (+ `main_test.go`, covering `observed`'s success, failure and cancelled-pass paths),
  `orchestrator/go.mod`/`go.sum` (adds `prometheus/client_golang v1.24.1`, the version the API and
  runner already pin; `golang.org/x/net` moved 0.56.0 → 0.57.0 indirectly, matching those two
  modules), `orchestrator/README.md`, `runner/README.md`, `api/README.md`,
  `runner/internal/metrics/metrics.go` and `api/internal/http/server.go` (comments #288 left saying
  nobody exports these series now point at the orchestrator's surface),
  `.github/workflows/ci.yml` (compose smoke now scrapes the running orchestrator container).
- Validation performed — commands and their results, all from the worktree:
  - `make lint` → pass. `make typecheck` (`go vet ./...`) → pass.
  - `make test-orchestrator` → ok, 5 packages (11 new tests in `internal/metrics`, 4 in
    `internal/config`, 2 in `cmd/orchestrator`). `make test-runner` → ok, 12 packages.
    `make test-api` → ok, 5 packages.
  - `make test-postgres-integration` against a throwaway PostgreSQL on a **unique** port
    (`docker run -d --name moirai-pg-issue-296 -p 55296:5432 postgres:16-alpine`,
    `LOOP_TEST_DATABASE_URL=postgresql://loop:loop-test-password@localhost:55296/loop_test`) → ok.
    Two new cases: `TestMetricsSnapshotReportsTheDatabaseState` seeds a state where every count
    differs (queue 2, active workflows 1, scheduled jobs 1, enabled runners 2) plus four runners —
    enabled at 600 s, enabled at 5 s, disabled at 9 h, revoked at 20 h — and asserts the exported
    age is ~600 s, so picking the newest enabled runner or an out-of-service one fails it; it then
    starts a real listener and scrapes it over HTTP. `TestScrapeSurvivesAnUnreachableDatabase`
    scrapes with a closed pool and asserts the state series are absent, the error counter is 1, the
    response is still 200, and a live pool still answers afterwards.
  - **End-to-end against the built container image** (`docker build -f orchestrator/Dockerfile`,
    run against the throwaway database), the same check CI now runs:
    `docker exec … curl -fsS http://127.0.0.1:9090/metrics` →
    `moirai_active_workflows 1`, `moirai_enabled_runners 2`, `moirai_queue_depth 2`,
    `moirai_runner_heartbeat_age_seconds 660.169329`, `moirai_scheduled_jobs 1`.
  - **The values are live, not constant.** Against the local binary on `127.0.0.1:19296`: first
    scrape `moirai_queue_depth 3 / moirai_enabled_runners 2 / moirai_runner_heartbeat_age_seconds
    420.047427 / moirai_active_workflows 0 / moirai_scheduled_jobs 0` (two enabled runners at 420 s
    and 5 s plus one disabled at 10 h — the 420 s one is what is reported); after inserting a
    workflow run and a job and making one issue ineligible, `moirai_queue_depth 2`,
    `moirai_active_workflows 1`, `moirai_scheduled_jobs 1`,
    `moirai_runner_heartbeat_age_seconds 427.567459` — every series moved, and the age aged.
  - **Database down** (`docker stop moirai-pg-issue-296`): `GET /metrics` → `HTTP 200`, the five
    state series absent, `moirai_orchestrator_metrics_scrape_errors_total 1`, process still alive;
    after `docker start`, the next scrape served `moirai_queue_depth 2` again.
  - **Bind failure**: with `127.0.0.1:19296` already held, the process exited 1 with
    `serve metrics on "127.0.0.1:19296": listen tcp 127.0.0.1:19296: bind: address already in use`.
  - **Disabled**: `LOOP_METRICS_BIND=""` logged
    `INFO metrics endpoint disabled reason="LOOP_METRICS_BIND is empty"`, opened no listener, and
    the process still ran and stopped cleanly on SIGTERM.
  - `make compose` → pass. `make proto-check` → pass (no proto changed). `make test-release-tags` →
    pass. `make compose-overlays` passes with `COMPOSE_PROJECT_NAME=moirai`; without it,
    `render-tls-stack.sh --check` fails only because `docker compose config` derives the project
    name from the checkout directory (`name: issue-296` vs `name: moirai`) — the same worktree
    artifact recorded under #288, not a regression.
  - `make test-web` was not run: nothing under `web/` changed.
  - Throwaway containers `moirai-pg-issue-296` and `moirai-orch-issue296` and the local image were
    removed after validation.
- Known issues / notes for the next agent:
  - The orchestrator publishes no port in `compose.yaml`, so `/metrics` is reachable from inside
    the Compose networks only. That is deliberate — the console's published surface is port 3000 —
    and a deployment that scrapes it should attach Prometheus to the `control` network rather than
    publish 9090.
  - Out of scope and untouched: #297 (blocked-status derivation), #298 (web console spec symbols),
    #300 (console event envelope).
