# Moirai Web Dashboard

The web service is a React application built by Vite and served by nginx. Its nginx configuration serves the application on port `8080`, proxies `/api/` to the Compose `api:8080` service, and falls back to `index.html` for client-side routes.

## Pages

Every page except `/login` requires a session; `/tokens` additionally requires the `admin` role.

| Route | Page | API |
|---|---|---|
| `/login` | Sign in | `POST /api/v1/auth/login` |
| `/` | Dashboard link list | — |
| `/projects` | Projects | `GET/POST/PUT /api/v1/projects…` |
| `/runners` | Runner fleet | `GET /api/v1/runners` |
| `/tokens` | Runner registration tokens (admin) | `GET/POST/DELETE /api/v1/runner-tokens…` |
| `/workflows` | Workflows and human decisions | `GET /api/v1/workflows`, `POST /api/v1/workflows/{id}/decision` |

The runner fleet page is read-only: it lists every registered runner with its status, labels,
drain state and heartbeat age, and marks a runner stale once it has missed three heartbeats
(`3 × LOOP_RUNNER_HEARTBEAT_INTERVAL`, 30s with the default runner configuration — see
[`runner/README.md`](../runner/README.md)). It refreshes every 10 seconds while the tab is
visible, and has a Refresh button. Drain, disable and revoke controls need
`POST /api/v1/runners/{id}/state`, which the API does not serve yet
([`docs/design/web-console/tasks.md`](../docs/design/web-console/tasks.md) B1).

A runner that has stopped heartbeating shows as `Stale`, not `Online`, even though the
orchestrator's `runners.status` column still says `online`: that column only returns to `offline`
through lease expiry or revocation, so an idle runner that is killed would otherwise be reported
connected indefinitely. The header count (`n/m online`) excludes stale runners for the same reason.

## Configuration

| Variable | Default | Meaning |
|---|---|---|
| `VITE_RUNNER_HEARTBEAT_INTERVAL_MS` | `10000` | The heartbeat interval the runners page assumes, in milliseconds. Set it to match `LOOP_RUNNER_HEARTBEAT_INTERVAL` when the fleet does not use the runner's 10s default, otherwise every runner is reported stale. |

This is a Vite variable, so it is read at **build** time (`npm run build` / `make build-web` /
`docker build`), not at container start. It goes away once `GET /api/v1/runners` reports the
interval itself ([`docs/design/web-console/tasks.md`](../docs/design/web-console/tasks.md) A12).

## Development

```bash
npm ci
npm run dev
```

The Vite development server has no API proxy configuration. Use the Compose dashboard for `/api/` proxying, or configure a development reverse proxy outside this package.

## Build and validation

```bash
npm run typecheck
npm run lint
npm test
npm run build
```

`make test-web` runs the first three from the repository root; `make build-web` runs the last.

Tests are [Vitest](https://vitest.dev) files next to the code they cover (`src/*.test.ts`,
`src/*.test.tsx`). They render components either with `react-dom/server` (no DOM needed) or, for
components that fetch, with `react-dom/client` under the per-file `// @vitest-environment jsdom`
docblock. There is no MSW layer yet: components take an `ApiClient` as a prop, so a stub object is
enough. Widening this into the full test infrastructure the design package asks for is issue #123.
