# Moirai Console — UI specification

Status: **approved design benchmark** (2026-07-29). The visual and behavioral reference is
[`mockup.html`](mockup.html) — a self-contained static mockup with sample data. Open it in a
browser; it is the source of truth for look, feel, layout, and copy tone. This document is the
source of truth for data mappings, API contracts, and behavior. Where the two disagree, this
document wins.

Implementation is broken into ordered tasks in [`tasks.md`](tasks.md). Each task is written to be
executable independently by an implementer (human or model) without re-reading the whole spec.

---

## 1. Purpose and scope

Replace the current 4-page SPA (`web/src/`) with a full management console for the Moirai control
plane. The console's organizing principle: **an autonomous system's UI exists for the exceptions.**
Human decisions, blocked workflows, and open circuits come before charts and lists.

In scope: everything an operator needs to observe and control the loop — overview/triage, global
queue, workflow list + detail (phases, gates, attempts, events, logs, controls), runner fleet and
registration tokens, project configuration and circuits, system internals (health, circuits,
outbox, audit). Out of scope: user management UI, multi-tenant concerns, mobile-first layouts
(the console must be *usable* at narrow widths, not optimized for them).

Relationship to existing plans: this spec is the concrete form of `tasks/todo.md` Phase 3
("Finish the MVP surface") and of `PROJECT.md`'s web-administration goal. It also gives operator
answers to platform-review findings F4 (why was this run cancelled), F5 (wedged circuits need a
reset), F7 (leaked execution requests need visibility), and F8 (offer reservations must count as
used capacity).

## 2. Design system

### 2.1 Identity

Moirai is named for the three Fates who spin, measure, and cut the thread of life; workflows run
on LangGraph `thread_id`s. The signature element of the console is the **phase thread** (§2.6):
each workflow drawn as a thread spun through its phases — gold where spun, dashed where ahead,
visibly *cut* where a blocked/failed run ended. Everything else stays quiet and disciplined.

Voice: plain verbs, sentence case, no filler. Buttons say exactly what happens ("Approve & merge",
"Drain", "Reset circuit"). Errors state what went wrong and what to do next. Empty states invite
action ("Nothing needs a decision. The Fates are spinning on their own.").

### 2.2 Color tokens

All colors are CSS custom properties. Both themes ship; `prefers-color-scheme` is the default
signal and an explicit `data-theme="light|dark"` attribute on `:root` overrides it. Exact values
(copy from `mockup.html`, which is authoritative):

| Token | Light | Dark | Role |
|---|---|---|---|
| `--ground` | `#f3f2f5` | `#14161f` | page background |
| `--surface` | `#fbfbfa` | `#1b1e2b` | cards, sidebar |
| `--raised` | `#ffffff` | `#222635` | buttons, popovers |
| `--border` / `--border-soft` | `#dddbe2` / `#e8e6ec` | `#313548` / `#272b3c` | strokes / row dividers |
| `--ink` / `--ink-2` / `--ink-3` | `#23222b` / `#5c5a68` / `#8c8998` | `#e6e4dc` / `#a5a2b2` / `#767386` | primary / secondary / muted text |
| `--thread` | `#9a7420` | `#d4a752` | brand accent: actively-spinning gold. Text-safe. |
| `--thread-mark` | `#b98a2e` | `#d4a752` | thread strokes/marks (non-text) |
| `--thread-soft` | `#f0e6cf` | `#322c1d` | accent wash (active nav, running pills) |
| `--ok` / `--ok-soft` | `#34965c` / `#e2f0e7` | `#57a06b` / `#1e2b23` | success / delivered |
| `--crit` / `--crit-soft` | `#93294a` / `#f4e2e8` | `#c25c7c` / `#33202a` | blocked, failed, offline, open circuit |
| `--warn` / `--warn-soft` | `#a8641c` / `#f4e9dc` | `#c48229` / `#322818` | repairing, draining, backoff, half-open |
| `--wait` / `--wait-soft` | `#4a6fb5` / `#e3e9f5` | `#6b8fd4` / `#1f2536` | waiting on checks/human; also links and focus ring |
| `--idle` / `--idle-soft` | `#8c8998` / `#ebeaef` | `#767386` / `#262a39` | neutral/terminal-neutral |
| `--chart-ok` / `--chart-crit` / `--chart-warn` | `#34965c` / `#93294a` / `#a8641c` | `#57a06b` / `#b04565` / `#c48229` | chart series only (see §2.7) |
| `--focus` | `#4a6fb5` | `#6b8fd4` | `:focus-visible` outline |

The chart triples were **validated** with the dataviz palette validator (six checks: lightness
band, chroma floor, CVD separation, normal-vision floor, contrast vs surface) against surfaces
`#f3f2f5` (light) and `#1b1e2b` (dark) in stacking order *ok → crit → warn*. Do not reorder
stacked series and do not substitute values without re-validating.

Rules: text never wears a series/status color as its font color except for short state words and
counts; status is never conveyed by color alone (every pill has a label, every chart a legend).

### 2.3 Typography

No webfonts — system stacks only:

- **Display** (`.serif`): `"Iowan Old Style", "Palatino Linotype", Palatino, Georgia, serif` —
  wordmark and view titles (`h1`, 26px/600). This carries the mythological identity; use nowhere else.
- **Body**: `system-ui, "Segoe UI", sans-serif`, 14px/1.55.
- **Data** (`.mono` / `.num`): `ui-monospace, "SF Mono", "Cascadia Code", Menlo, Consolas, monospace`
  with `font-variant-numeric: tabular-nums` — ids, branches, counts, timestamps, tile values.

Labels/eyebrows: 10.5–11.5px, uppercase, letter-spacing 0.1–0.14em, `--ink-3`.

### 2.4 Surfaces and spacing

Cards: `--surface`, 1px `--border-soft`, radius 10px, shadow `0 1px 3px` (see tokens). Card header
(13px/650 title + right-aligned muted hint) and body paddings per mockup. Grid gap 14px. Sidebar
208px fixed, sticky, hidden below 820px (a slide-over drawer replaces it — see §5.7). Main column
max-width 1240px.

### 2.5 Component inventory

All components exist in the mockup; port them as the shared component library (`web/src/ui/`).

- **Pill** — status chip with 7px dot + label. Variants: `run` (thread wash, dot pulses),
  `ok`, `bad`, `warn`, `wait`, `idle`. Pulse animation disabled under `prefers-reduced-motion`.
- **Stat tile** — uppercase label, 27px tabular-mono value, small note line, optional sparkline.
  Note may be `bad` (crit color) when it carries a warning.
- **Meter** — 54×5px rounded track showing `used/budget`; fill is `--thread`, switching to
  `--crit` when `used >= budget`; count rendered beside it as `n/m` mono. Used for attempt
  budgets and runner capacity.
- **Table** — uppercase 11px headers, 1px soft row dividers, no zebra. Row-as-link rows
  (`tr.rowlink`) get hover wash, `tabindex="0"`, Enter activation, and an `aria-label`.
  Every table sits inside an `overflow-x: auto` wrapper.
- **Filter chips** — pill-shaped toggle buttons with `aria-pressed`; selected = thread wash.
- **Attention item** — 3px colored stripe + title + one-line reason + right-aligned actions.
- **Feed item** — time (mono, fixed width) + colored dot + one-line text with bold subjects.
- **Banner** — full-width crit-tinted callout with bold lead word ("Blocked.", "Failed.") and
  right-aligned action buttons. Used for terminal reasons on workflow detail.
- **Decision panel** — wait-tinted card containing the human gate: context sentence naming the
  merge method and issue, optional comment textarea, primary "Approve & merge" +
  secondary "Request changes".
- **Buttons** — default (raised + border), `primary` (thread background), `danger` (crit outline,
  crit wash on hover), `sm` size. Focus ring: 2px `--focus` with 2px offset, everywhere.
- **Toast** — bottom-center, `role="status"`, `aria-live="polite"`, auto-dismiss ~2.6s. One at a time.
- **KV list** — 2-col definition list for detail metadata.
- **Health strip** — horizontal row of dot+label+mono-value probes inside a card.

### 2.6 The phase thread (signature component)

An SVG rendering of a workflow's progress along the happy path
`prepare → plan → implement → pipeline → review → push → pr → checks → human → merge → done`
(11 display phases; the graph's `repair` and `blocked` nodes are not thread stops — repairs show
as status/pills, blocked as a cut).

Full variant (workflow detail, ~980×70):

- **Spun** segments (phases completed): 2.5px `--thread-mark` stroke drawn as gentle cubic curves
  with a ±2.2px alternating wobble — it must read as thread, not a progress bar.
- **Ahead** segments: 2px `--border`, dashed `2 5`, straight.
- **Current phase**: 4.5px filled gold dot inside a 8px 50%-opacity gold ring (the spindle).
- **Done/past nodes**: 3.5px gold dots. **Future nodes**: 3px ground-filled, border-stroked dots.
- **Cut** (status `blocked`/`failed`/`cancelled`): thread stops at the last reached phase with
  three short frayed strokes in `--crit` and a 4.5px crit node; the following segment is a gap
  (nothing drawn), then dashed "ahead" resumes visually never (remaining segments stay dashed).
  Cut position: pipeline node if `pipeline_passed === false`, checks node if a PR exists,
  implement node otherwise (refine with real data when phase history is available — use the last
  `workflow_events.phase` reached).
- Labels under every node, 10.5px; current/cut label is bold and colored (gold / crit); first
  label start-anchored, last end-anchored, rest middle.
- Legend beneath: "spun / ahead / cut" swatches.
- Accessibility: the SVG carries `role="img"` and an `aria-label` of the form
  "Workflow phase progress: <status label>".

Mini variant (~170×12 in tables, ~280×14 in overview cards): same geometry, 2px stroke,
no labels, no rings — only the current-phase 3px dot and the cut fray.

Reduced motion: the spindle pulse (if animated) is disabled under `prefers-reduced-motion`.

### 2.7 Charts

Two chart types only; both hand-rolled SVG (no chart library):

- **Outcomes stacked bars** (overview): last 14 days of terminal workflows per day. Series and
  stacking order fixed: Delivered (`--chart-ok`), Failed (`--chart-crit`), Blocked
  (`--chart-warn`). 18px bars, 2px surface stroke between stacked segments, 3px rounded top on
  the top segment only, recessive horizontal gridlines every 2 units with mono tick labels,
  every-other-day x labels. Legend always present. Hover: full-column invisible hit rects show a
  tooltip (fixed-position card) listing all three series with swatches and mono values;
  `pointerleave` hides it.
- **Sparkline** (queue-depth tile): single gold line with soft area fill and an emphasized
  endpoint dot. `aria-hidden` (the tile's number is the accessible value).

Never add a second y-axis. Any new chart goes through the dataviz procedure (form → color →
validate → marks → hover → a11y).

## 3. Information architecture

### 3.1 Routes

| Route | View | Access |
|---|---|---|
| `/login` | Login form | public |
| `/` | Overview | session |
| `/queue` | Queue | session |
| `/workflows` | Workflow list (filter state in query string: `?filter=active\|needs_you\|terminal\|all&q=`) | session |
| `/workflows/:id` | Workflow detail | session |
| `/runners` | Runner fleet + registration tokens | session (mutations admin) |
| `/projects` | Project cards | session (mutations admin) |
| `/system` | System internals | session |
| `*` | 404 with a link back to Overview | session |

Sidebar order: Overview, Queue, Workflows, Runners, Projects, System, under an "Operate" section
label. Nav rows show live counts: Queue (queue depth), Workflows (active count — rendered in crit
style when anything is `waiting_human` or `blocked`), Runners (`online/total`), System (crit `!`
marker when any circuit is open). Footer: `username · role` and orchestrator reachability.

### 3.2 Roles

`viewer` sees every view; every mutating control (buttons in §5) is hidden for viewers — not
disabled, hidden. `admin` sees everything. The API client must treat **401 as
session-expired (redirect to login) and 403 as forbidden (toast "You need the admin role for
that")** — today's client logs the user out on both, which breaks viewer accounts
(`web/src/api.ts`). All mutations send `X-CSRF-Token` from the `loop_csrf` cookie (unchanged).

### 3.3 Status vocabulary

UI labels for `workflow_runs.status` (domain enum → pill label / variant). In the workflow list
(`ListWorkflows` → `GET /api/v1/workflows`) the `phase` field is `workflow_runs.current_phase`,
which is *not* a copy of the status: the status carries the scheduling lifecycle (`preparing`
while a runner holds the job, `recovering`/`offered` while a fenced job is being re-placed)
while the phase stays on the workflow node the graph committed, so a run whose developer
execution is in flight reads `preparing` / `implementing`. Both are worth showing. (The
`SubmitHumanDecision` response is the exception: it echoes the resumed graph state's status in
both fields.) See "Run status versus run phase" in `orchestrator/README.md`.

| Status | Label | Pill |
|---|---|---|
| `offered` | Offered | idle |
| `preparing` | Preparing | run (pulse) |
| `planning` | Planning | run |
| `implementing` | Implementing | run |
| `local_pipeline` | Pipeline | run |
| `repairing` | Repairing | warn (pulse) |
| `ai_review` | AI review | run |
| `pushing` | Pushing | run |
| `pr_created` | PR open | wait |
| `waiting_github_checks` | Waiting on checks | wait |
| `waiting_human` | Needs decision | wait |
| `merging` | Merging | run |
| `recovering` | Recovering | warn |
| `completed` | Delivered | ok |
| `blocked` | Blocked | bad |
| `failed` | Failed | bad |
| `cancelled` | Cancelled | idle |

Terminal set: `completed, blocked, failed, cancelled`.

## 4. Data and API requirements

The SPA talks only to the Go API (`/api/v1`); the API proxies to orchestrator gRPC. Every new
HTTP endpoint therefore needs: a gRPC RPC (+ message types in `proto/control_plane.proto`), an
orchestrator servicer method backed by a persistence query, a Go handler + `api/openapi.yaml`
entry, and a typed client function in `web/src/api.ts`. Errors stay RFC-7807 problem+json.
List endpoints take `limit` (default 50, max 200) and opaque `cursor` pagination.

### 4.1 Existing endpoints reused as-is

`POST /auth/login`, `POST /auth/logout`, `GET /auth/me`, `GET /health`,
`POST /projects`, `PUT /projects/{id}`, `POST /projects/{id}/enable|disable`,
`GET/POST/DELETE /runner-tokens…`, `POST /workflows/{id}/decision`.

### 4.2 Endpoints to widen (existing route, richer payload)

- **`GET /api/v1/workflows`** — today returns `{id, projectId, status, phase}`. The orchestrator's
  `list_workflows()` already computes more (`persistence/control_plane.py`); widen the proto
  `Workflow` message and response to:
  `id, projectId, projectName, issueNumber, issueTitle, status, phase, runnerId, branchName,
  pullRequest {number, url, checksState}, attempts {planning, implementation, pipelineRepair,
  reviewCycles, ciRepair, totalExecutions — each as {used, budget}}, blockingReason,
  terminalReason, createdAt, updatedAt, completedAt`.
  Query params: `filter=active|needs_attention|terminal|all` (server-side mapping of §3.3 sets;
  `needs_attention` = `waiting_human ∪ blocked`), `projectId`, `q` (matches issue number/title/
  branch/PR number), pagination.
- **`GET /api/v1/projects`** — widen from `{id, name, enabled}` to include
  `repositoryMode, repositoryUrl|localRepositoryPath, defaultBranch, mergeMethod,
  requiredRunnerLabels, activeWorkflowId` (from `project_locks`), and
  `circuit {state, consecutiveFailures, lastFailureReason, openedAt, probeWorkflowRunId}`.
- **`GET /api/v1/runners`** — add `capacity, activeJobs (count of jobs in non-terminal states),
  reservedOffers (pending job_offers — F8: these count against capacity in the UI), version,
  capabilities, registeredAt, revokedAt`. Existing fields stay.

### 4.3 New read endpoints

- **`GET /api/v1/workflows/{id}`** — single workflow, same shape as the widened list item plus
  `gates {planValid, pipelinePassed, reviewApproved, checksPassed, humanApproval}` where each gate
  is `passed|failed|pending|not_reached` (derived from checkpoint state / result tables), and
  `baseCommit, currentCommit, threadId`.
- **`GET /api/v1/workflows/{id}/events?after=<id>&limit=`** — from `app.workflow_events`:
  `{id, eventType, phase, severity, message, payload, createdAt}`. `message` is a human-readable
  one-liner rendered server-side (orchestrator) from `event_type` + payload so the web layer never
  parses raw payloads; `payload` included for an expandable raw view. Ordered descending; `after`
  enables incremental polling.
- **`GET /api/v1/workflows/{id}/log?tailBytes=16384`** — concatenated log-type runner execution
  events for the workflow's current/last execution (already redacted and size-capped by the
  runner). Plain text response.
- **`GET /api/v1/queue`** — the scheduler's eligible-issue view, in true global priority order
  (reuse the ordering in `domain/scheduling.py` — priority desc, then external created-at, queued
  time, project id, external id). Item: `{position, projectId, projectName, issueNumber, title,
  url, labels, priority, waitingSince, holdReason}`. `holdReason` is computed server-side, one of:
  `project_busy {activeWorkflowId}`, `circuit_open {probeEtaSeconds}`, `no_compatible_runner`,
  `behind_in_project`, `none` (next to schedule).
- **`GET /api/v1/issue-sync`** — per enabled project from `app.issue_sync_state`:
  `{projectId, projectName, lastSyncedAt, consecutiveFailures, nextRetryAt, lastError, disabled}`.
- **`GET /api/v1/circuits`** — union of project and provider circuit tables:
  `{scope: "project"|"provider", id, name, state, consecutiveFailures, lastFailureReason,
  openedAt, probeWorkflowRunId, probeEtaSeconds}`.
- **`GET /api/v1/system/health`** — the orchestrator health snapshot (`health.py`) over gRPC
  instead of the file: `{healthy, checkpointerEnabled, dbOk, dbCheckedSecondsAgo,
  schedulerLastTickSecondsAgo, issueSyncLastRunSecondsAgo, deadLoops, updatedAt}` plus counters
  the System view needs: `{outboxPending, outboxProcessing, openExecutionRequests,
  oldestDispatchedExecutionRequestAgeSeconds}`.
- **`GET /api/v1/stats/outcomes?days=14`** — per-day terminal counts:
  `{date, delivered, blocked, failed}` (group `workflow_runs.completed_at` by day, statuses
  completed/blocked/failed; cancelled excluded).
- **`GET /api/v1/stats/queue-depth?days=14`** — daily queue-depth samples for the sparkline
  (simplest source: a small daily rollup written by the metrics loop; if absent, return the
  current depth as a single point and the tile hides the sparkline).
- **`GET /api/v1/audit?limit=&cursor=`** — from `app.audit_events`:
  `{createdAt, actorType, actorId, actorName, action, targetType, targetId, payload}`.

### 4.4 New control endpoints (all admin + CSRF, all audited)

- **`POST /api/v1/workflows/{id}/retry`** — allowed from `blocked`/`failed`/`cancelled`. Creates a
  fresh workflow run for the same issue with prior-failure context in the task packet
  (fingerprints, last diff hash), resets issue labels to ready-equivalent, returns the new
  workflow. 409 problem+json if the project already has an active workflow.
- **`POST /api/v1/workflows/{id}/cancel`** — allowed from any non-terminal status. Cancels the
  job/execution via the runner stream, marks run `cancelled` with `terminal_reason =
  "cancelled by <user>"`, releases the project lock, resets the issue to `agent:ready`.
- **`POST /api/v1/workflows/{id}/block`** — allowed from any non-terminal status. Marks run
  `blocked` with `blocking_reason = "manually blocked by <user>: <comment>"` (optional comment in
  body), labels the issue `agent:blocked` so it is not rescheduled.
- **`POST /api/v1/runners/{id}/state`** — body `{action: "enable"|"disable"|"drain"|"undrain"|
  "revoke"}`. Wire to the existing `set_runner_state()` persistence method (currently exposed by
  no RPC). `revoke` also invalidates the runner credential and terminates its stream. New gRPC
  RPC `SetRunnerState`.
- **`POST /api/v1/circuits/{scope}/{id}/reset`** — force circuit to `closed`, clear failure count
  and probe pointer. This is the F5 remediation control; it must work even when the probe points
  at a nonexistent workflow.

### 4.5 Live updates

Target: SSE end-to-end (`tasks/todo.md` Phase 3): server-streaming RPC
`StreamEvents(stream ConsoleEvent)` in `control_plane.proto` → API proxy at
`GET /api/v1/events/stream` (SSE; `proxy_buffering off` for `/api/v1/events/` in
`web/nginx.conf`) → web `useEventStream()` hook on `EventSource` with automatic reconnect and
`Last-Event-ID`. Event envelope:
`id` (monotonic), `event` (`workflow.updated`, `workflow.event`, `runner.updated`,
`queue.changed`, `circuit.changed`, `health.updated`), `data` (the affected resource in its §4.2/4.3
shape). Consumers patch their local state; a `queue.changed`/unknown event triggers a refetch of
the affected view.

**Interim mode (until SSE ships):** every view polls its endpoints on a 10s interval while
visible (pause on `document.hidden`), and workflow detail polls events with `after=` at 5s. The
UI must be written against a data layer that hides polling-vs-SSE from components.

## 5. Views

Layout, spacing, and copy: match `mockup.html`. This section specifies data sources and behavior.
Global rules: every view has loading (skeleton cards — no spinners-only), error (inline problem
detail + "Retry" button; never a silent blank), and empty states with the copy from the mockup.
Timestamps render as relative age ("12s ago") with the absolute time in `title`.

### 5.1 Overview (`/`)

Sections, top to bottom:

1. **Health strip** — from `GET /system/health` + `GET /circuits`: Database, Scheduler tick,
   Issue sync, Outbox pending, Circuits (n open + first name; crit dot when > 0), right-aligned
   "durable checkpointing on/off". Each probe: ok dot when fresh, warn when stale (> 3× its loop
   interval), crit when failed/dead.
2. **Stat tiles** (4): Queue depth (+ sparkline from `stats/queue-depth`, note "top priority PN");
   Active workflows (note: "n waiting on you · m spinning"); Runners online (`online/total`, note
   lists draining/offline; note style `bad` when any offline); Delivered this week (from
   `stats/outcomes`, note "n blocked · m failed").
3. **Needs you** — the triage list, assembled client-side in this priority order:
   `waiting_human` workflows ("<project> #<issue> is ready to merge" + waiting time, action
   "Review & decide" → detail), `blocked` workflows (blocking_reason, "Inspect"), open circuits
   ("Reset circuit" + "probe in <eta>"), `failed` workflows with an open PR (terminal_reason,
   "Retry workflow" / "Inspect"). Empty copy: "Nothing needs a decision. The Fates are spinning on
   their own."
4. **Outcomes chart** — §2.7, from `stats/outcomes?days=14`.
5. **In flight** — all non-terminal workflows: link, status pill, title, mini thread. Header
   action "All workflows".
6. **Recent events** — most recent ~10 console events across projects (from the SSE feed buffer,
   or interim: latest `workflow.events` merged across active workflows). Dot color: gold =
   progress, ok = delivered/PR opened, wait = waiting, crit = blocked/circuit.

### 5.2 Queue (`/queue`)

- Main table from `GET /queue`: position, priority pill (P≥6 bad, P≥3 warn, else idle), issue
  (bold `project #n` + muted title), labels (mono, dot-separated), waiting age, **"Why it hasn't
  started"** — the humanized `holdReason` ("Project busy — one workflow per project (wf-… active)",
  "Project circuit open — probe in 2m 40s", "Behind #104 in project queue").
- **Issue sync card** from `GET /issue-sync`: project, last sync age, consecutive failures,
  status pill (healthy / backing off + error + next retry / project disabled).

### 5.3 Workflows (`/workflows`)

- Filter chips (Active / Needs you / Terminal / All) mapped to the `filter` query param; search
  box mapped to `q` (debounced 300ms). Both reflected in the URL.
- Table from widened `GET /workflows`: issue (bold + truncated title), status pill, mini thread,
  total-attempts meter, runner id (mono), PR (link + "checks ✓/…" pill; stopPropagation so the
  link doesn't trigger the row), updated age. Row click/Enter → `/workflows/:id`.

### 5.4 Workflow detail (`/workflows/:id`)

From `GET /workflows/{id}` + `/events` + `/log`:

1. Breadcrumb ("Workflows /"), `h1` = "<project> #<issue>", status pill; subtitle = issue title.
2. **Terminal banner** when blocked ("Blocked." + `blockingReason`; actions "Retry with fresh
   context", danger "Cancel & release issue") or failed ("Failed." + `terminalReason`; action
   "Retry workflow").
3. **The thread** card — full phase thread (§2.6); hint shows current phase + updated age.
4. **Decision panel** when `waiting_human` (§2.5): names the PR, the merge method, and the issue
   that will close; optional comment (recorded in audit log and posted to the PR);
   "Approve & merge" / "Request changes" → `POST /workflows/{id}/decision`. On success: toast,
   refetch. On 409 (already decided): refetch and show current state.
5. **Events** card — timeline rows: time (mono), phase (uppercase muted), message (warning rows
   in `--warn`). "Load more" pagination; live-append via SSE/polling.
6. **Agent log tail** card — `<pre>`-style mono block from `/log`; hint "streamed from
   <runner> · redacted".
7. Right rail: **Gates** (five rows: Plan valid, Local pipeline, AI review, GitHub checks, Human
   approval → ✓ passed / ✗ failed / in-progress word / "not reached"); **Attempt budgets** (six
   meters incl. bold Total row); **Details** KV (workflow id, runner, branch, PR, started);
   **Controls** card for active non-waiting runs: "Cancel workflow", "Block & hold issue"
   (admin only; confirm dialog on both).

### 5.5 Runners (`/runners`)

- **Fleet cards** from widened `GET /runners`: name (mono) + status pill (Online pulsing ok /
  Draining warn / Offline bad) + version; KV: heartbeat age, capacity meter where
  **used = activeJobs + reservedOffers** with the caption "n running, m reserved by offer" (F8),
  labels as idle pills, backend, "Working on" links to workflow details. Optional warn note line
  (e.g. draining explanation, offline cause). Actions: Drain/Stop draining, Disable/Enable,
  danger Revoke (confirm dialog; explains the credential is invalidated) →
  `POST /runners/{id}/state`.
- **Registration tokens** card: existing endpoints. Columns: token id, allowed labels ("any" when
  empty), expires, used-by, action (Revoke) or state pill (consumed/revoked). "New token" opens a
  small form (allowed labels multi-input) → modal showing the token **once** with copy button and
  the warning "copy it now, it is shown only once".

### 5.6 Projects (`/projects`)

- Header action "Add project" → form (name; mode radio managed clone/existing path; repository
  URL xor local path; default branch; required runner labels; merge method select) →
  `POST /projects`. Same form for "Configure" → `PUT /projects/{id}`.
- **Project cards** from widened `GET /projects`: name + "Scheduling"/"Paused" pill + pulsing
  "Circuit open" pill when open; KV: source (mono), mode + base branch + merge method, runner
  labels, active thread (link or "none"). When circuit open: crit note with failure count, reason,
  probe ETA. Actions: Configure, Pause/Resume scheduling, danger Reset circuit.

### 5.7 System (`/system`) and app shell

- Tiles: Scheduler (last tick age + leader-lock note), Issue sync (last pass age), Outbox
  (pending + processed rate), **Execution requests** (open count; note goes `bad` with
  "1 dispatched > 30 min — investigate" when `oldestDispatchedExecutionRequestAgeSeconds` exceeds
  1800 — the F7 leak surfaced).
- **Circuit breakers** table from `GET /circuits`: scope-tagged name, state pill
  (Open pulsing bad / Half-open warn / Closed ok), failures, last reason, Reset action.
  Hint documents the policy: "3 blocked opens · 5 min cooldown · 1 half-open probe".
- **Audit log** table from `GET /audit` (paginated).
- **Loops** card + **Retention** card from `/system/health` and constants.
- App shell: sidebar (§3.1) with live counts; below 820px the sidebar collapses behind a
  hamburger button into a slide-over drawer (focus-trapped, Esc closes). `<title>` per route
  ("Queue — Moirai Console"). 404 view for unknown routes.

## 6. Non-functional requirements

- **Accessibility**: visible `:focus-visible` on all interactives; row-links keyboard-activatable;
  toasts and live counts in `aria-live="polite"` regions; SVGs labeled or hidden; decision/confirm
  dialogs focus-trapped with Esc; color never the only signal; contrast ≥ 4.5:1 for text in both
  themes; `prefers-reduced-motion` kills pulse/marching animations.
- **Theming**: both themes via tokens only — no literal colors in components. Default follows
  `prefers-color-scheme`; a `data-theme` attribute overrides (persisted in `localStorage`).
- **Error handling**: no silent `.catch(() => undefined)` anywhere (a lint rule or review gate);
  every fetch surfaces problem+json `title/detail` in the view or a toast; destructive actions
  (revoke runner/token, cancel workflow, reset circuit) confirm first and name the consequence.
- **Performance**: fetch-on-navigate with stale-while-revalidate caching in the data layer;
  no fetch waterfalls per view (parallelize); lists virtualize only if they exceed ~200 rows.
- **Testing**: vitest + Testing Library + MSW (per `tasks/todo.md` Phase 2). Minimum bar per view:
  renders from mocked data, empty state, error state, primary action fires the right request with
  CSRF header. Component tests for the thread (each status class renders the right geometry) and
  the API client (401 vs 403 split, problem+json mapping). `make test-web` runs vitest + eslint +
  `tsc --noEmit` and is wired into CI.
- **Stack**: keep React 18 + TypeScript + Vite + react-router. Add nothing heavier than a small
  data-fetching layer (hand-rolled or TanStack Query) — no component framework, no CSS framework;
  styles stay hand-written CSS (CSS modules or a single tokens + components stylesheet like the
  mockup).

## 7. Copy reference

Reuse the mockup's copy verbatim where present; its tone is part of the approved design.
Notable strings: view subtitles; "Needs you"; "Why it hasn't started"; "Your decision gates the
merge"; empty-queue/attention copy; toast messages (e.g. "Circuit closed — project schedulable
again", "Token created — copy it now, it is shown only once", "Draining — finishes current work,
then accepts no offers"). The proposal-note footer in the mockup is mockup-only; do not ship it.
