# Moirai Public API

Public REST API gateway.

## Operations

`GET /metrics` exposes Prometheus metrics. API request IDs are forwarded to the orchestrator as gRPC `x-request-id` metadata.

For TLS to the orchestrator, set `LOOP_ORCHESTRATOR_TLS=true`; optionally set `LOOP_ORCHESTRATOR_TLS_CA_FILE` and `LOOP_ORCHESTRATOR_TLS_SERVER_NAME`.

## Testing

Run tests: `go test ./...`
