# Moirai Orchestrator

Go gRPC control plane for projects, runners, issue scheduling, GitHub delivery, and PostgreSQL state.

## Local checks

```bash
go test ./...
go vet ./...
```

## Run locally

```bash
LOOP_DATABASE_URL='postgresql://loop:password@localhost/loop' \
LOOP_INITIAL_ADMIN_PASSWORD='Moirai-Local-1' \
RUNNER_REGISTRATION_TOKEN='local-runner-token' \
go run ./cmd/orchestrator
```

The process applies SQL migrations from `migrations/`, serves gRPC on port `50051` and Prometheus metrics on port `9090`, and runs the scheduler, the check observer, the recovery sweep, and the issue sync loop. `gh` is required for issue sync, pull requests, checks, and merges.

Migrations are read from `migrations/` relative to the working directory, which is why the commands above are run from `orchestrator/`; the image copies them next to the binary and sets `WORKDIR /app`.

## Configuration

Every secret below can be supplied either directly or as `<NAME>_FILE` pointing at a file to read it from. Setting both forms of the same secret is refused rather than resolved, so a half-migrated deployment fails loudly instead of using the one you did not mean.

| Variable | Required | Meaning |
|---|---|---|
| `LOOP_DATABASE_URL` | yes | PostgreSQL connection URL. A `postgresql+asyncpg://` scheme is accepted and normalised, so connection strings written for the previous Python orchestrator keep working. |
| `LOOP_GRPC_BIND` | no | `host:port` to serve gRPC on. Defaults to `0.0.0.0:50051`. The container healthcheck derives its port from this. |
| `LOOP_METRICS_BIND` | no | `host:port` to serve Prometheus metrics on. Defaults to `0.0.0.0:9090`; set it to an empty value to serve none. A port that cannot be bound stops the process rather than being logged and ignored. |
| `LOOP_SECRET_KEY` | for credentials | 32-byte key, base64 or hex, used to encrypt project credentials at rest. Storing a credential without it is refused. |
| `LOOP_INITIAL_ADMIN_USERNAME` / `LOOP_INITIAL_ADMIN_PASSWORD` | first boot | Seeds the first admin account. |
| `RUNNER_REGISTRATION_TOKEN` | no | Seeds a single-use runner registration token valid for 15 minutes. Its expiry is re-armed on every start, so a restart does not leave an expired token behind. |
| `LOOP_SEED_TOKEN_LABELS` | no | Comma-separated labels the seeded token may register. Defaults to `linux`. |
| `LOOP_GITHUB_TOKEN` | no | Token handed to `gh` for issue sync, pull requests, checks and merges. |
| `LOOP_ISSUE_SYNC_INTERVAL` | no | How often issues are re-read from the tracker, as a Go duration. Defaults to `2m`. |
| `MOIRAI_BUILD_VERSION` | no | Build identifier reported by `GetSystemVersion` and shown in the console. |

## Metrics

`LOOP_METRICS_BIND` (default `0.0.0.0:9090`) serves `GET /metrics` in the Prometheus text format. The orchestrator is the only service that can export these: they are derived from its database, which the API and the runners cannot reach. The API and the runner each used to register the first three as gauges set to zero once and never written again — a series that never moves cannot raise an alert — and [#124](https://github.com/alexandre-leites/moirai/issues/124) removed those placeholders rather than keep them wrong.

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `moirai_queue_depth` | gauge | | Eligible open issues in enabled projects: the scheduler's candidate set. An issue stays eligible while its workflow runs — it is parked only when the run delivers or ends — so this includes work already in flight, at most one issue per project. Identical to the console's queue depth, which reads the same query. |
| `moirai_active_workflows` | gauge | | Workflow runs that have not reached a terminal status. |
| `moirai_scheduled_jobs` | gauge | | Jobs offered to, being prepared by, or running on a runner. |
| `moirai_enabled_runners` | gauge | | Runners an operator has neither disabled nor revoked. |
| `moirai_runner_heartbeat_age_seconds` | gauge | | Seconds since the **least** recently seen enabled runner checked in — the fleet-wide worst case. Absent when no runner is enabled. |
| `moirai_orchestrator_loop_runs_total` | counter | `loop`, `result` | Passes of the recovery sweep and the issue sync, by `success` or `failure`. |
| `moirai_orchestrator_loop_last_success_age_seconds` | gauge | `loop` | Seconds since that loop last completed without error, counting from process start until its first success. |
| `moirai_orchestrator_metrics_scrape_errors_total` | counter | | Scrapes that could not read the database and therefore omitted the state series. |

Four properties are deliberate:

- **The state series are read at scrape time, from the same single query `GetSchedulerMetrics` runs**, so the console and Prometheus cannot report different numbers for the same word, and neither can serve a value a background timer left behind. The previous orchestrator refreshed four gauges from a snapshot loop that raised on every tick, so all four served their initial zero for the life of the process ([#195](https://github.com/alexandre-leites/moirai/issues/195)); a collector has no tick to fail.
- **A failed read omits the state series rather than reporting zero**, and counts the omission in `moirai_orchestrator_metrics_scrape_errors_total`. "The database did not answer" and "the queue is empty" must not look the same. The scrape reuses the shared connection pool, is bounded by a five-second timeout, and cannot take the process down: a panic below it is contained and counted.
- **The heartbeat age is the oldest, over enabled and unrevoked runners only.** A fleet where one runner is healthy and nine are gone is a broken fleet, so a maximum or an average would hide exactly the case worth alerting on; runners an operator disabled or revoked are excluded, or the series would be permanently and correctly alarming. It is computed by PostgreSQL from its own clock, so orchestrator clock skew cannot make a heartbeat look fresher than it is.
- **`moirai_runner_heartbeat_age_seconds` is also exported by each runner**, where it means that runner's own heartbeat rather than the fleet's oldest. Prometheus separates them by `job`/`instance`.

`LOOP_RUNNER_METRICS_BIND` (default `:9091`) is the runner's equivalent knob, and the API serves its own metrics on its HTTP port.

### TLS

The gRPC endpoint is plaintext unless a certificate is configured. `LOOP_GRPC_TLS_CERT_FILE` and `LOOP_GRPC_TLS_KEY_FILE` must be set together — setting one alone is refused rather than silently downgraded, because an operator who configured half of it asked for an encrypted endpoint. `LOOP_GRPC_TLS_CLIENT_CA_FILE` additionally requires client certificates, and is meaningless without server TLS.

Runner secret delivery (`ResolveJobSecret`, `StoreJobSecret`) is refused over a plaintext peer, so a deployment that uses project credentials needs TLS configured here and on the runner. `compose.tls.yaml` wires both ends together.
