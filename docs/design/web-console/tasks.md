# Moirai Console — implementation task breakdown

Derived from [`specification.md`](specification.md) (the contract) and [`mockup.html`](mockup.html)
(the visual benchmark). Tasks are written to be picked up independently — each names its layers,
dependencies, and acceptance criteria. Suggested issue labels: `web-console` + phase label; tasks
that fit the autonomous loop can be labeled `ai-doable`.

Sizing: S ≈ ≤half day, M ≈ a day, L ≈ multi-day.

Cross-cutting rule for every task that adds an endpoint: proto message/RPC + orchestrator
servicer + persistence query + Go handler + `api/openapi.yaml` + typed `web/src/api.ts` client +
tests at each layer (spec §4).

---

## Phase A — Contracts and read APIs (backend, no UI changes)

- [ ] **A1 (M) Widen the `Workflow` proto message and `GET /api/v1/workflows`.**
  Spec §4.2. Carry everything `list_workflows()` already returns (PR id/url, blocking reason,
  five attempt counters) plus project name, issue number/title, runner id, branch, timestamps,
  budgets; add `filter`, `projectId`, `q`, pagination params.
  *Accept:* response matches spec shape; `filter=needs_attention` returns exactly
  `waiting_human ∪ blocked`; old 4-field consumers keep working during migration.

- [ ] **A2 (M) `GET /api/v1/workflows/{id}` with gates.**
  Spec §4.3. New `GetWorkflow` RPC; gates derived server-side as
  `passed|failed|pending|not_reached`.
  *Accept:* every gate state reachable in tests (fixture per status); 404 problem+json for
  unknown id.

- [ ] **A3 (M) Workflow events read API.**
  Spec §4.3: `ListWorkflowEvents` RPC + `GET /workflows/{id}/events?after=&limit=` over
  `app.workflow_events`, with server-rendered human-readable `message` per event type.
  *Accept:* descending order; `after` returns only newer rows; message strings exist for all
  three `event_type` values and both severities.

- [ ] **A4 (S) Agent log tail endpoint.**
  Spec §4.3: `GET /workflows/{id}/log?tailBytes=` assembling log-type runner execution events.
  *Accept:* plain-text response, size-capped, empty 200 when no logs.

- [ ] **A5 (M) Queue read API with hold reasons.**
  Spec §4.3: `GET /api/v1/queue` in true scheduler order (reuse `domain/scheduling.py`
  ordering), computing `holdReason` (`project_busy`, `circuit_open` + probe ETA,
  `no_compatible_runner`, `behind_in_project`, `none`).
  *Accept:* order matches the scheduler for the same fixtures; each hold reason covered by a test.

- [ ] **A6 (S) Issue-sync state read API.**
  Spec §4.3: `GET /api/v1/issue-sync` over `app.issue_sync_state` joined to projects.
  *Accept:* backoff fields (`consecutiveFailures`, `nextRetryAt`, `lastError`) round-trip.

- [ ] **A7 (S) Circuits read API.**
  Spec §4.3: `GET /api/v1/circuits` unioning project + provider circuit tables, with
  `probeEtaSeconds`.
  *Accept:* returns open/half-open/closed rows from both tables with scope tags.

- [ ] **A8 (M) System health over gRPC.**
  Spec §4.3: expose the `health.py` snapshot via a `GetSystemHealth` RPC +
  `GET /api/v1/system/health`, plus counters: outbox pending/processing, open execution
  requests, oldest dispatched-request age (the F7 signal).
  *Accept:* endpoint serves the same fields the health file contains today, without reading
  the file through the API container.

- [ ] **A9 (S) Outcome and queue-depth stats.**
  Spec §4.3: `GET /stats/outcomes?days=` (per-day delivered/blocked/failed from
  `workflow_runs.completed_at`) and `GET /stats/queue-depth?days=` (daily rollup written by the
  metrics loop; single current point if no history).
  *Accept:* day bucketing is UTC-stable; cancelled runs excluded from outcomes.

- [ ] **A10 (S) Audit read API.**
  Spec §4.3: `GET /api/v1/audit` paginated over `app.audit_events`.
  *Accept:* newest first; cursor pagination; payload passed through as JSON.

- [ ] **A11 (S) Widen `GET /api/v1/projects`.**
  Spec §4.2: mode/source/branch/merge/labels + `activeWorkflowId` (from `project_locks`) +
  embedded circuit state.
  *Accept:* response shape per spec; no N+1 (single query with joins).

- [ ] **A12 (S) Widen `GET /api/v1/runners`.**
  Spec §4.2: capacity, activeJobs, `reservedOffers` (pending `job_offers` — the F8 field),
  version, capabilities, registeredAt.
  *Accept:* a pending offer increments `reservedOffers` in an integration test.

## Phase B — Control actions (backend)

- [ ] **B1 (M) Runner state control.**
  Spec §4.4: `SetRunnerState` RPC + `POST /api/v1/runners/{id}/state`
  (enable/disable/drain/undrain/revoke) wiring the existing `set_runner_state()` persistence
  method; revoke invalidates the credential and closes the stream. Audited.
  *Accept:* each action verified against runner rows; viewer gets 403; audit row written.

- [ ] **B2 (L) Workflow retry.**
  Spec §4.4: `POST /workflows/{id}/retry` from terminal states — new run for the same issue with
  prior-failure context (fingerprint, diff hash) in the task packet; label reset; 409 when the
  project lock is held. Audited.
  *Accept:* retried run's packet contains prior failures; blocked issue becomes schedulable;
  409 path tested.

- [ ] **B3 (M) Workflow cancel.**
  Spec §4.4: cancel non-terminal runs — CancelExecution to the runner, run → `cancelled` with
  attributed `terminal_reason`, project lock released, issue back to `agent:ready`. Audited.
  *Accept:* lock released and issue eligible again in integration test; idempotent second call
  returns current state.

- [ ] **B4 (S) Workflow manual block.**
  Spec §4.4: non-terminal → `blocked` with attributed reason + optional comment; issue labeled
  `agent:blocked`. Audited.
  *Accept:* blocked run not rescheduled; reason includes username and comment.

- [ ] **B5 (M) Circuit reset.**
  Spec §4.4: `POST /circuits/{scope}/{id}/reset` forcing `closed`, clearing failures and the
  probe pointer — must work when the probe references a nonexistent workflow (F5 wedge). Audited.
  *Accept:* a wedged half-open fixture (dangling probe id) becomes schedulable after reset.

## Phase C — Web foundation

*Delivered 2026-08-01. Phase D landed alongside it for every view whose data the current API
serves; the sections still gated on Phase A/B are listed under Phase D below.*

- [x] **C1 (S) Fix the API client 401/403 split.**
  Spec §3.2: 401 → session expired/login; 403 → toast, stay put. (Pre-existing bug,
  `web/src/api.ts`.)
  *Accept:* MSW tests for both codes; a viewer clicking an admin action is not logged out.

- [x] **C2 (M) Web test infrastructure.**
  vitest + Testing Library + MSW; `make test-web` runs vitest + eslint + `tsc --noEmit`, wired
  into CI (also closes the "eslint never runs in CI" gap).
  *Accept:* CI fails on a failing component test; C1's tests run under it.

- [x] **C3 (M) Design tokens + theming.**
  Spec §2.2–2.4: port the mockup's token sheet (light/dark, `prefers-color-scheme` +
  `data-theme` override persisted in localStorage); base styles; no literal colors in components.
  *Accept:* theme toggle + OS preference both work; grep finds no hex colors outside the token
  sheet.

- [x] **C4 (L) Component library.**
  Spec §2.5: Pill, StatTile, Meter, Table (row-link a11y), FilterChips, AttentionItem, FeedItem,
  Banner, DecisionPanel, Buttons, Toast, KV, HealthStrip, ConfirmDialog, skeletons/empty/error
  blocks — visually matching the mockup.
  *Accept:* component tests; keyboard + focus-visible verified for row-links, chips, dialogs.

- [x] **C5 (M) Phase-thread component.**
  Spec §2.6: full + mini variants, spun/ahead/current/cut geometry, labels, legend, aria-label,
  reduced-motion.
  *Accept:* snapshot/geometry tests per status class (running, waiting, completed, blocked-at-
  pipeline, failed-with-PR).

- [x] **C6 (M) App shell, routing, data layer.**
  Spec §3.1, §5.7, §4.5-interim: sidebar with live counts, mobile drawer, route titles, 404;
  data layer with per-view polling (10s, paused when hidden) behind an interface SSE can later
  implement.
  *Accept:* nav counts update after a poll tick; drawer focus-trap; unknown route renders 404.

## Phase D — Views (each depends on C-phase + listed APIs)

*Partly delivered 2026-08-01: every view ships, built from what the current API serves. The
boxes stay open because each still has sections gated on Phase A/B — named per task below.*

- [ ] **D1 (L) Overview** — needs A1, A5, A7, A8, A9, A12. Spec §5.1 (health strip, tiles,
  Needs-you triage, outcomes chart with hover tooltip, in-flight mini-threads, event feed).
  *Accept:* triage ordering per spec; chart matches §2.7 marks; all empty/error states.

- [ ] **D2 (M) Queue** — needs A5, A6. Spec §5.2.
  *Accept:* hold-reason strings humanized per spec; sync backoff row shows error + next retry.

- [ ] **D3 (M) Workflows list** — needs A1. Spec §5.3.
  *Accept:* filter + search in URL; row keyboard activation; PR link does not trigger the row.

- [ ] **D4 (L) Workflow detail** — needs A2, A3, A4; controls need B2–B4 + existing decision
  endpoint. Spec §5.4 (thread, banners, decision panel, events with load-more + live append, log
  tail, gates, budgets, controls with confirm).
  *Accept:* each status renders its correct banner/panel/controls; decision posts comment +
  CSRF; 409 handled by refetch.

- [ ] **D5 (M) Runners** — needs A12, B1 + existing token endpoints. Spec §5.5.
  *Accept:* capacity meter counts reserved offers; token shown once with copy; all actions
  confirm + toast; viewer sees no action buttons.

- [ ] **D6 (M) Projects** — needs A11, B5 + existing project endpoints. Spec §5.6.
  *Accept:* create/edit form validates mode-xor-source; circuit note + reset only when open.

- [ ] **D7 (M) System** — needs A7, A8, A10, B5. Spec §5.7.
  *Accept:* execution-request tile flips to warning per the 30-min rule; circuits table reset
  works; audit paginates.

- [x] **D8 (S) Retire the old pages** — delete the link-list dashboard, old flat workflows page,
  old tokens page once D1–D7 ship; redirect `/tokens` → `/runners`.
  *Accept:* no dead routes; nav is the §3.1 set.

## Phase E — Live updates and polish

- [ ] **E1 (L) SSE end-to-end.**
  Spec §4.5: `StreamEvents` RPC → API SSE proxy → `useEventStream` with `Last-Event-ID`;
  nginx `proxy_buffering off`; data layer switches from polling to patching, falling back to
  polling on stream failure.
  *Accept:* a workflow transition appears in an open Overview/detail without reload; kill the
  stream → polling takes over; events replay from Last-Event-ID on reconnect.

- [ ] **E2 (M) Accessibility + polish pass.**
  Spec §6 checklist end-to-end: focus order, aria-live regions, contrast audit both themes,
  reduced motion, form validation messages, document titles.
  *Accept:* axe-core (or equivalent) clean on all views in both themes.

- [ ] **E3 (S) Human question/answer escalation (future-facing).**
  Platform review L4: extend the decision panel to render a *question* from the agent with a
  free-text answer that resumes the workflow (needs backend escalation support from issue #107 —
  keep the UI slot designed but gated).
  *Accept:* UI renders the question variant behind a capability flag; no dead controls when the
  backend lacks support.

---

### Dependency sketch

```
A1 ─┬─ D3
    ├─ A2 ─ A3 ─ A4 ── D4 (controls: B2 B3 B4)
    └─ D1 (also A5 A7 A8 A9 A12)
A5 A6 ── D2          A11 B5 ── D6
A12 B1 ── D5         A7 A8 A10 B5 ── D7
C1 → C2 → C3 → C4 → C5 → C6 → all D
D1–D7 → D8 → E1 → E2
```

Phases A and B are pure backend and can proceed in parallel with C. The D views land one at a
time behind the existing nav; D8 flips the console over.
