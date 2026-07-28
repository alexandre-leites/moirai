# Moirai Public API

The API is the HTTP gateway for the orchestrator gRPC control plane. It serves browser sessions, validates JSON requests, and exposes management resources under `/api/v1`.

## HTTP contract
## Operations

`GET /metrics` exposes Prometheus metrics. API request IDs are forwarded to the orchestrator as gRPC `x-request-id` metadata.

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
