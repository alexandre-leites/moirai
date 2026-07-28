# Moirai Public API

Public REST API gateway.

## Operations API

Authenticated users can read `/api/v1/workflows`, `/api/v1/workflows/{id}`, `/api/v1/runners`, `/api/v1/queue`, and the SSE stream at `/api/v1/events`. Administrators can retry, cancel, or block workflows and enable, disable, drain, or revoke runners. Mutations require the session CSRF header.

`401` denotes an invalid or expired session; `403` denotes an authenticated user without the required administrator role. Unary orchestrator RPCs receive a 15-second deadline; the event stream remains request-scoped.

## Testing

Run tests: `go test ./...` and `go vet ./...`
