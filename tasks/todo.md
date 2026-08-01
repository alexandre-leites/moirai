# Moirai Improvement Plan

## Status as of 2026-08-01

Reconciled against the GitHub issue tracker and the working tree. Of the 35 items below,
**34 are done and one remains** (SSE, Phase 3). Three of the done items carry caveats — the
pipeline gate, circuit-breaker visibility and scheduler metrics — noted in place.

One warning about what a ticked box here means: **a closed issue is not proof.** Two closed
items shipped broken and were only caught by reading the code on 2026-08-01 —
`web/src/workflows.tsx` carried unresolved conflict markers, and #206 cut
`api/internal/http/handlers/workflows.go` to a stub while leaving its routes registered, so
the Go API had not compiled for days. Both predate CI running.

**CI now runs and is green** (run `30701717203`, all 12 jobs, commit `238a9c4`) — the first
completed run on `main`. It had not executed for over a week because `ci.yml` targeted a
self-hosted pool with nothing online, so every run queued until the next push cancelled it.
Both workflows now use GitHub-hosted runners; the self-hosted lines are commented out in
place. The first run that executed failed six of nine jobs, every one a real defect, all
fixed in `f15d6ef` and `238a9c4`.

## Platform review remediation (2026-07-29) — issues #88–#108

Source: `docs/reviews/2026-07-29-platform-review.md` (findings F1–F16, competitor analysis,
autonomy roadmap L1–L5).

**All 21 issues are closed.** The three systemic problems the review named are addressed:
the graph now suspends after dispatching an execution (#88), the runner no longer reports a
missing result document as success (#89), and `pipeline_passed` is no longer inferred from
the developer's exit code (#90). Autonomy layers L1–L5 all landed (#104–#108).

Verified in the tree: `suspend_after_dispatch` wraps every dispatching node in
`workflows/issue_graph.py`; `readResultDocument` returns an error for a missing document in
`runner/internal/agents/opencode.go`.

---

Based on a four-way assessment (orchestrator, runner, API/web, cross-cutting) against
PROJECT.md, 2026-07-28. Ordering principle: first make the product actually work end-to-end,
then make failures visible, then finish MVP features, then harden.

## Phase 0 — Ship blockers (product cannot run today) — complete

- [x] **Fix duplicate migration version 003.** (#37) Verified: the outbox migration is now
  `004_workflow_transition_outbox.sql` and versions 001–010 are unique.
- [x] **Seed LangGraph state from the DB.** (#38) `project_id`, `issue_id`, `branch_name`,
  `human_approval_required` and merge config now reach graph state, so PR creation, check
  polling, merge, issue close and human approval are reachable.
- [x] **Release project lock on terminal workflow status.** (#39)
- [x] **Ship `schemas/` into the orchestrator image.** (#40)
- [x] **Make runner delivery actually able to commit and push.** (#41, #2) Git identity and
  push credentials are injected; #109 later added GitHub credentials to the task packet.
- [x] **Fix web healthcheck port and align Go builder images.** (#42)
- [x] **Fix README quickstart.** (#43)

## Phase 1 — Correctness and safety of what exists — complete

- [x] **Lease fencing while disconnected.** (#44)
- [x] **Unbreak the outbox retry path.** (#45) Transient errors now retry instead of
  permanently failing a workflow.
- [x] **Pending GitHub checks must wait, not repair.** (#46, #155) Pending is tri-state and
  re-polled; `push` no longer consumes `ci_repair_attempts`; merge treats already-merged as
  success (#121 added merge verification).
- [x] **Implement the real local-pipeline node.** (#47, #90) The node reads
  `app.project_pipeline_steps`, dispatches a pipeline execution and persists `pipeline_runs`.
  **Caveat — the gate is still vacuous in practice:** nothing can write
  `project_pipeline_steps`; the table is read-only across the codebase, so every project has
  an empty step list and the strongest gate in the product passes by default. Tracked as
  **#114 (P1, open)** and listed under "Remaining" below.
- [x] **Stop crashing the runner on recoverable races.** (#48, #102)
- [x] **Split 403 from 401 in the API client.** (#49) Re-verified 2026-08-01 with tests in
  `web/src/api.test.ts`: 401 signs the user out, 403 raises without clearing the session.
- [x] **Graceful runner shutdown + drain.** (#50; #148 later fixed a stranded drain flag)

## Phase 2 — Make failure visible (testing & CI) — complete

- [x] **Real Postgres integration tests.** (#51) `make test-postgres-integration` exists and
  runs real migrations against `AsyncpgControlPlane`.
- [x] **CI builds images + compose smoke test.** (#52) `build-web` and `compose-smoke` jobs
  exist in `ci.yml` and both pass; `compose-smoke` builds all four images, waits for health
  and probes the readiness endpoints.
- [x] **Graph-level end-to-end test.** (#53)
- [x] **Web test infrastructure.** (#54, #123) vitest + jsdom; `make test-web` runs
  `tsc --noEmit`, eslint and 187 tests, and is wired into `ci.yml`.
- [x] **CI hygiene.** (#55) `permissions:`, concurrency group, dependabot and the audit jobs
  are all present in `ci.yml`.

## Phase 3 — Finish the MVP surface (per AGENTS.md, implementation-first)

> **Console design approved (2026-07-29):** specified in `docs/design/web-console/` (mockup +
> spec + task breakdown A1–E3). Phases C and D of that package landed 2026-08-01; the sections
> still gated on its phases A and B are listed in `web/README.md`.

- [ ] **SSE end-to-end.** Still unimplemented. #56 was closed but the work did not land, and
  it was re-filed as **#118 (P1, open)**. Verified 2026-08-01: no `text/event-stream`,
  `EventSource` or `StreamEvents` anywhere in `api/`, `orchestrator/src`, `web/src` or
  `proto/`. The console polls on a 10s interval instead (`web/src/poll.ts`), which is the
  interim mode the spec allows. Needs: streaming RPC in `control_plane.proto` → orchestrator
  event stream → API SSE proxy → web `useEventStream` hook, plus `proxy_buffering off` in
  `web/nginx.conf`.
- [x] **Operational control RPCs + UI.** (#57, #110, #112, #113, #117, #119, #120, #192)
  Workflow retry/cancel/block, runner drain/revoke, the queue endpoint and page, the runners
  page, project configuration, and the workflow detail route all exist.
- [x] **Human approval path.** (#58, #5) The `wait_for_human` interrupt is reachable and the
  decision panel on workflow detail resolves it.
- [x] **Progress detection & failure fingerprints.** (#59, #101)
- [x] **Circuit breakers.** (#60, #92) Consulted by `schedule()`. **Not yet visible to an
  operator** — no read API, so the console cannot show or reset an open circuit
  (`docs/design/web-console/tasks.md` A7 and B5).
- [x] **Richer task packet & prompts.** (#61, #106, #115) Plan, prior failures, review
  findings and failed checks are populated; the reviewer gets a fresh independent prompt.
- [x] **Issue-tracker/code-host surface completion.** (#62, #99)
- [x] **Docker execution mode for real.** (#63) Resource and network configuration are no
  longer hardcoded.

## Phase 4 — Hardening, observability, DX — complete

- [x] **Structured orchestrator logging.** (#64) Verified: `configure_logging()` from
  `moirai.observability`, called at `main.py:18`.
- [x] **Metrics + request-ID propagation.** (#65, #124) Prometheus endpoints exist and
  `CorrelationLoggingInterceptor` propagates request IDs. **Caveat:** scheduler summary
  metrics are not fully surfaced to the console — **#195 (open)**; the console consumes
  `GET /api/v1/scheduler/metrics` for the health strip but not the full set.
- [x] **TLS for gRPC.** (#66)
- [x] **Secrets & compose hardening.** (#67)
- [x] **Promote hardcoded timings to config.** (#68)
- [x] **Runner event-path efficiency.** (#69; #152 later fixed a `Flush()` delivery flake)
- [x] **Docs.** (#70) Per-service READMEs now total ~590 lines; `api/openapi.yaml` covers the
  public API; `make help` and an aggregate `make test` exist.
- [x] **Web polish pass.** (#71) Superseded by the console revamp: error/loading/empty states
  per view, focus-visible throughout, `aria-live` on live counts, per-route document titles,
  form validation and a 404 route.

## Remaining — the whole open list

Four open issues after #197, one of them P1. In the order I would take them:

1. **#114 (P1) — project pipeline steps cannot be configured.** The deterministic gate that
   the product's core promise rests on ("an agent cannot declare success by itself") passes by
   default today. Needs a write path: schema/API/UI for `app.project_pipeline_steps`.
2. **#118 (P1) — SSE.** See Phase 3 above.
3. **#195 — scheduler summary metrics for the dashboard.** Half done: the orchestrator's
   gauges were stuck at zero because the metrics loop failed on every tick, fixed in
   `238a9c4`. What remains is surfacing the full set in the console.
4. **#143 — flaky `TestControlLoopDeliversTerminalEventAfterLogsSaturateTheBufferWhileDisconnected`.**

**#197 is fixed** (`f15d6ef`) and can be closed: `_require_admin` called
`ServicerContext.abort` without awaiting it, so on `grpc.aio` the abort never ran and
`SetRunnerState` accepted any authenticated viewer.

The four pre-existing failures listed here earlier — two `test_metrics` cases, the
runner-controls role test, one ruff error and two mypy errors — are all fixed.

## Review

### 2026-08-01 — Console revamp (design package phases C, D, plus the backend it needed)

Replaced the ad-hoc `web/src/` SPA with the approved console from
`docs/design/web-console/`. Two blockers on `main` had to be cleared first — both were
pre-existing breakage, not part of the design work:

- `web/src/workflows.tsx` still carried `<<<<<<< HEAD` conflict markers and
  `web/src/runners.tsx` had corrupted JSX, so `tsc` failed and the app did not build.
- Commit `9312ee5` (#206) cut `api/internal/http/handlers/workflows.go` from 224 lines to a
  31-line stub, deleting `get`, `listEvents`, `submitDecision`, `retry`, `cancel` and `block`
  while leaving their routes registered, and pointing its imports at a module path that does
  not exist. The Go API did not compile, and `queue.go` lost the `queryInt64` helper with it.

**Backend (the minimum the console needed to be real).** Restored the workflow handlers.
Widened the workflow list to the same projection as the detail read — `list_workflows` now
shares `get_workflow`'s query, `ListWorkflows` maps it with `_workflow_detail_message`, and
the Go handler emits the full payload — so the list, the overview and the phase threads read
issue titles, pull requests, attempts and timestamps without a request per row. Added
`total_agent_executions` to the proto `Workflow` message (the only attempt counter missing
from the wire) and regenerated. `make proto-check` was already failing on `main` because the
committed Go stubs predated `GetSchedulerMetrics`; regenerating fixed that too, and three of
the five pre-existing mypy errors with it.

**Console.** Token sheet and component library ported from the mockup, the phase thread in
both variants, a polling data layer that hides the transport from views, and the six views
the current API can answer honestly: overview, queue, workflow list, workflow detail,
runners (with registration tokens folded in), and projects. Sidebar counts are live, the
drawer is focus-trapped, routes set the document title, unknown routes 404.

**Left out on purpose,** because no endpoint serves them and a placeholder that looks like a
reading is worse than an absent one: the 14-day outcomes chart and sparkline (A9), circuits
(A7, B5), the System view (A8, A10), runner capacity and reserved offers (A12), queue
waiting age and richer hold reasons (A5), merge method in the decision panel (A11), SSE
(E1). `web/README.md` carries the same table.

**Two derivations to retire.** Gate state and thread position are derived from the attempt
counters and the pull request, because `current_phase` is overwritten with `blocked`/`failed`
when a run ends badly; event sentences are rendered client-side from `event_type`. Both are
isolated in `web/src/status.ts` and documented there, and tasks A2 and A3 replace them.

**Verified.** `go build ./... && go test ./...`, `make test-orchestrator` (no new failures),
`make proto-check`, `make test-web` (176 tests, `tsc --noEmit`, eslint), `npm run build`, and
a walk through every route in both themes and at narrow width against a stub API.

**Still red on `main`, untouched here** (unrelated to this work): `test_metrics` expects a
`scheduled_jobs` key the snapshot now returns, `test_runner_controls_require_admin_csrf_and_
persist_actor` expects a viewer rejection that does not happen, one ruff error
(`asyncio.TimeoutError` alias in `grpc/control_plane.py`), and two mypy errors
(`metrics_snapshot` missing from the `ControlPlane` protocol, an unawaited coroutine).

### 2026-08-01 — Roadmap reconciliation

Reconciled this file and `PROGRESS.md` against the tracker and the working tree. Every box in
Phases 0–4 was unticked despite 34 of 35 items having shipped, and `PROGRESS.md` still named
a task merged three commits earlier as in progress. Neither file was a usable progress signal.
The CI finding above came out of that reconciliation.

### 2026-08-01 — CI restored, and the six failures it found

`ci.yml` and `release.yml` targeted a self-hosted pool with nothing online, so every run
queued until the next push cancelled it — eleven consecutive cancellations and no completed
run on `main` for over a week. Both moved to GitHub-hosted runners, with the self-hosted
declarations commented out one line above each replacement.

The first run that actually executed failed six of nine jobs. Every one was a real defect,
not a CI artefact:

- `_require_admin` called `ServicerContext.abort` without awaiting it. On `grpc.aio` that
  builds a coroutine and drops it, so the admin check did nothing and `SetRunnerState`
  accepted any authenticated viewer (#197). The same defect was mypy's unused-coroutine
  error and the failing runner-controls test.
- `schedule()` had lost its f-string prefix in #205 and sent `WHERE {…}` to Postgres, so
  every scheduling path raised a syntax error — 28 of 37 integration tests.
- `flush_test_2.go` did not end in `_test.go`, so Go compiled a test into the production
  package and broke the runner build and the vulnerability scan. It also redeclared a test
  from `issue_152_test.go`; both were artefacts of #213 and #215 landing the same fix twice.
- Three tests had gone stale against correct product code: the continuation prompt moved to
  `.loop/prompt.md` in #185, `test_metrics` predated the `scheduled_jobs` gauge, and the
  integration suite's developer result carried no agent result document, which #105 requires
  as delivery evidence.
- `react-router` 7.18.2 sat inside a high-severity advisory. Moved to `react-router` 8.3.0,
  which is above the range; npm's own suggestion was a downgrade into five older advisories.
- `compose-smoke` wrote an admin password the bootstrap policy rejects, so the orchestrator
  refused to start. The README quickstart had the same defect.

Verifying that last one against a live stack exposed a bug the smoke test cannot see, because
the containers still go healthy: the metrics loop failed on every 15s tick, so all four
Prometheus gauges stayed at zero for the life of every process. Two causes stacked — the loop
started before the migrations it depends on, and `update_snapshot` read `scheduled_job_count`
while the snapshot reports `scheduled_jobs`. Both fixed, with a test that reads gauge values
back out of the registry.

Run `30701717203` is green across all 12 jobs.
