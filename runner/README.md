# Moirai Runner

Isolated execution environment for software-engineering tasks.

## Configuration

- `LOOP_ORCHESTRATOR_ENDPOINT`: Orchestrator address (default `orchestrator:50051`).
- `LOOP_RUNNER_DATA_DIR`: Data directory (default `/data`).
- `LOOP_RUNNER_REGISTRATION_TOKEN`: Token for registration.
- `LOOP_RUNNER_METRICS_BIND`: Metrics listener (default `:9091`), serving `/metrics`.
- `LOOP_ORCHESTRATOR_TLS`: Enable TLS to the orchestrator. Configure CA, client certificate, client key, and server name with the matching `LOOP_ORCHESTRATOR_TLS_*` variables when required.

## Testing

Run tests: `go test ./...`
