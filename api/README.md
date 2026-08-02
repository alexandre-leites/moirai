# Moirai Public API

The API is the HTTP gateway for the orchestrator gRPC control plane. It serves browser sessions, validates JSON requests, and exposes management resources under `/api/v1`.

## HTTP contract
## Operations

`GET /metrics` exposes Prometheus metrics. API request IDs are forwarded to the orchestrator as gRPC `x-request-id` metadata.

### Exported metrics

The API exports only what it can populate itself. Queue depth, active workflow counts, and the fleet-wide runner heartbeat age are orchestrator-owned state derived from the database, and the API has no database access, so it does not re-export them — the orchestrator does, on `LOOP_METRICS_BIND` ([#124](https://github.com/alexandre-leites/moirai/issues/124), and [orchestrator/README.md](../orchestrator/README.md#metrics)).

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `moirai_api_requests_total` | counter | `method`, `route`, `status` | HTTP requests served, recorded once each request completes. |
| `moirai_api_request_duration_seconds` | histogram | `method`, `route` | Request duration, from the same measurement the request log line carries. |
| `moirai_api_orchestrator_calls_total` | counter | `rpc`, `code` | Orchestrator gRPC calls issued, by RPC method name and resulting gRPC status code. |

Every label is bounded by construction, so no caller can grow the series count:

- `route` is the `net/http` route **pattern** that matched, not the requested path, so `/api/v1/projects/{project_id}` stays one series. A request that matched no route is labelled `unmatched`, and so is one that matched a path but not its method — the 405 the mux answers comes from a handler it never registered, so it carries no pattern. The `status` label is what separates those two cases.
- `method` is one of the standard HTTP verbs; anything else is labelled `other`, since a client may put any token on a request line. The list is deliberately wider than the verbs the API registers routes for, so a request it does not serve is still reported under the method it used.
- `rpc` is the method name from the generated orchestrator client, fixed by the proto service; `code` is a gRPC status code, a closed enum.

These series appear at their first request rather than at startup. Unlike the runner's — whose label sets are small and known at construction, so every child is materialised at zero — the API's route set is not known when the server is built (handlers register their routes afterwards) and the RPC-by-status-code cross product is large, so pre-materialising them is not possible or not worthwhile. Alert on `absent()` or on a rate over a window rather than assuming a series exists on a freshly started process.

`/metrics` is served through the same middleware, so Prometheus's own scrapes appear in these series under `route="/metrics"`.

For TLS to the orchestrator, set `LOOP_ORCHESTRATOR_TLS=true`; optionally set `LOOP_ORCHESTRATOR_TLS_CA_FILE` and `LOOP_ORCHESTRATOR_TLS_SERVER_NAME`.

## Testing


[`openapi.yaml`](openapi.yaml) is the maintained OpenAPI 3.1 contract for every HTTP endpoint registered by this service. It documents health probes, authentication, projects, runner tokens, runners, and workflows.

Login creates `loop_session` and `loop_csrf` cookies. Authenticated mutations require the `loop_session` cookie and an `X-CSRF-Token` request header equal to the `loop_csrf` cookie.

## Configuration

| Variable | Default | Purpose |
| --- | --- | --- |
| `LOOP_API_BIND` | `:8080` | HTTP bind host and port. |
| `LOOP_ORCHESTRATOR_ENDPOINT` | `orchestrator:50051` | Orchestrator gRPC endpoint. |
| `LOOP_API_COOKIE_SECURE` | `true` | Whether session and CSRF cookies require HTTPS. |
| `LOOP_API_COOKIE_KEY` | unset | Optional cookie key; when set, it must be at least 32 printable bytes. |
| `LOOP_API_MAX_BODY_BYTES` | `1048576` | Maximum request size, from 1024 through 16777216 bytes. |
| `LOOP_API_TRUSTED_PROXIES` | private RFC1918 ranges, loopback | Comma-separated IP addresses or CIDRs trusted by request rate limiting. Set an empty value to trust no proxy. |

The service exits if its bind address or gRPC endpoint does not contain a valid host and port. The supplied HTTP-only Compose development stack sets `LOOP_API_COOKIE_SECURE=false`; production deployments should terminate TLS and retain the secure default.

## Validation

```bash
go test ./...
go build ./cmd/api
```
