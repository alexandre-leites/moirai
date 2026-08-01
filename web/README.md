# Moirai Console

The web service is a React application built by Vite and served by nginx. Its nginx configuration
serves the application on port `8080`, proxies `/api/` to the Compose `api:8080` service, and falls
back to `index.html` for client-side routes.

It implements the approved design package in
[`docs/design/web-console/`](../docs/design/web-console/): `mockup.html` is the visual benchmark and
`specification.md` is the contract. Where this README and the specification disagree, the
specification wins.

## Views

Every view except `/login` requires a session. Read access is the same for both roles; every
mutating control is **hidden** from a `viewer`, never shown-but-disabled (specification §3.2).

| Route | View | Reads |
|---|---|---|
| `/login` | Sign in | `POST /api/v1/auth/login` |
| `/` | Overview — health strip, tiles, "Needs you" triage, in-flight threads, event feed | `/health`, `/scheduler/metrics`, `/sync/status`, plus the shared snapshot |
| `/queue` | Global queue and issue-sync state | `/queue`, `/sync/status` |
| `/workflows` | Workflow list; filter and search live in the query string | `/workflows` |
| `/workflows/:id` | Detail: thread, decision panel, events, agent log, gates, budgets, controls | `/workflows/{id}`, `/workflows/{id}/events` |
| `/runners` | Runner fleet and registration tokens | `/runners`, `/runner-tokens` |
| `/projects` | Project cards and configuration | `/projects` |
| `/account` | Display name, email, password | `/auth/me`, `/auth/account` |

`/tokens` redirects to `/runners`, which is where registration tokens now live.

## How it is put together

- **`styles.css`** is the whole design system: the token sheet from the mockup (both themes) plus
  every component rule. Components carry no literal colours. The theme follows
  `prefers-color-scheme` and is overridden by `<html data-theme>`, persisted in `localStorage`
  (`theme.ts`).
- **`ui/`** is the component library — pills, meters, tiles, tables with keyboard-activatable row
  links, banners, toasts, focus-trapped dialogs, and the empty/error/skeleton blocks.
- **`ui/thread.tsx`** is the phase thread, the console's signature element: a workflow drawn as a
  thread through its phases, gold where spun, dashed ahead, and visibly cut where a blocked or
  failed run ended.
- **`poll.ts`** is the data layer. Every view polls on a 10s interval while the document is
  visible, one request in flight at a time. Components see `{data, error, loading, refresh}` and
  nothing about the transport, so the SSE stream in specification task E1 can replace the inside of
  the hook without touching a view.
- **`console-data.tsx`** polls the four collections nearly every view needs (workflows, runners,
  queue, projects) once for the whole console, in parallel.
- **`status.ts`** holds the domain vocabulary: statuses and their pills, the phase path, and the
  attempt budgets mirrored from the orchestrator's `RetryBudget`.

## Two derivations to retire

The console derives two things the API does not serve yet. Both are isolated in `status.ts` and
documented at the function:

- **Gates and thread position** come from the attempt counters and the pull request, because
  `workflow_runs.current_phase` is overwritten with `blocked`/`failed` when a run ends badly and so
  cannot say where it stopped. Specification task A2 replaces this with server-derived gate state.
- **Event sentences** are rendered from `event_type` and payload in `describeEvent()`. Task A3 moves
  that rendering to the orchestrator so every client agrees on the wording.

A runner that has stopped heartbeating shows as `Stale`, not `Online`, even though the
orchestrator's `runners.status` column still says `online`: that column only returns to `offline`
through lease expiry or revocation, so an idle runner that is killed would otherwise be reported
connected indefinitely. The sidebar count excludes stale runners for the same reason.

## Not built yet

These are in the mockup but have no data source; they are left out rather than stubbed, because a
placeholder that looks like a reading is worse than an absent one (specification §6):

| Missing | Needs |
|---|---|
| 14-day outcomes chart, queue-depth sparkline | A9 (`/stats/outcomes`, `/stats/queue-depth`) |
| Circuit-breaker state and reset | A7, B5 |
| System view (outbox, execution requests, loops, audit) | A8, A10 |
| Runner capacity, reserved offers, version, "working on" | A12 |
| Queue waiting age, labels, richer hold reasons | A5 |
| Merge method named in the decision panel | A11 |
| Live updates without polling | E1 |

## Configuration

| Variable | Default | Meaning |
|---|---|---|
| `VITE_RUNNER_HEARTBEAT_INTERVAL_MS` | `10000` | The heartbeat interval the runners view assumes, in milliseconds. Set it to match `LOOP_RUNNER_HEARTBEAT_INTERVAL` when the fleet does not use the runner's 10s default, otherwise every runner is reported stale. |

This is a Vite variable, so it is read at **build** time (`npm run build` / `make build-web` /
`docker build`), not at container start. It goes away once `GET /api/v1/runners` reports the
interval itself (tasks.md A12).

## Development

```bash
npm ci
npm run dev
```

The Vite development server has no API proxy configuration. Use the Compose dashboard for `/api/`
proxying, or configure a development reverse proxy outside this package.

## Build and validation

```bash
npm run typecheck
npm run lint
npm test
npm run build
```

`make test-web` runs the first three from the repository root; `make build-web` runs the last.

Tests are [Vitest](https://vitest.dev) files next to the code they cover. `src/test-dom.ts` mounts
and drives components; `src/test-console.tsx` supplies a stub `ApiClient` whose every method
resolves, plus `mountView`, which wraps a view in the providers it gets in production (router,
auth, toasts, the shared snapshot). A test only stubs the calls it cares about. There is no MSW
layer: views take an `ApiClient`, so a stub object is enough.
