# Moirai Runner

The runner registers with the orchestrator, maintains a bidirectional gRPC connection, and executes work in its local data directory. It is configured through environment variables and stores runner identity and its event outbox below `LOOP_RUNNER_DATA_DIR`.

## Configuration

| Variable | Default | Purpose |
| --- | --- | --- |
| `LOOP_ORCHESTRATOR_ENDPOINT` | `orchestrator:50051` | Orchestrator host and port. |
| `LOOP_RUNNER_DATA_DIR` | `/data` | Absolute directory for persistent identity, outbox, and workspaces. |
| `LOOP_RUNNER_NAME` | hostname | Runner display name. |
| `LOOP_RUNNER_LABELS` | unset | Comma-separated capability labels. |
| `LOOP_RUNNER_REGISTRATION_TOKEN` | unset | Token exchanged for the runner credential during registration. |
| `LOOP_RUNNER_CAPACITY` | `1` | Positive maximum concurrent executions. |
| `LOOP_RUNNER_HEARTBEAT_INTERVAL` | `10s` | Positive heartbeat interval. |
| `LOOP_RUNNER_RECONNECT_MIN` | `1s` | Positive initial reconnect backoff. |
| `LOOP_RUNNER_RECONNECT_MAX` | `1m` | Maximum reconnect backoff; it cannot be smaller than the minimum. |
| `LOOP_RUNNER_MINIMUM_FREE_BYTES` | `1073741824` | Positive free-space floor before accepting work. |
| `LOOP_RUNNER_RETAIN_WORKSPACES` | unset | Comma-separated retained terminal workspace states: `succeeded`, `failed`, and/or `abandoned`. |
| `LOOP_RUNNER_ALLOWED_ENVIRONMENT` | unset | Comma-separated safe variable names that task packets may resolve. |
| `LOOP_RUNNER_REDACTION_PREFIXES` | unset | Comma-separated prefixes redacted from runner logs. |
| `LOOP_RUNNER_AGENT_BACKEND` | `opencode` | Agent backend: `opencode`, `cli`, or `docker`. |
| `LOOP_RUNNER_AGENT_BINARY` | unset | Required executable path/name for the `cli` backend. |
| `LOOP_RUNNER_AGENT_ARGUMENTS` | unset | Comma-separated agent arguments. |
| `LOOP_RUNNER_AGENT_DOCKER_IMAGE` | unset | Required Docker image for the `docker` backend. |
| `LOOP_RUNNER_DOCKER_ENABLED` | `false` | Enable Docker execution support. |
| `LOOP_ORCHESTRATOR_TLS` | `false` | Enable TLS for the gRPC connection. |
| `LOOP_ORCHESTRATOR_TLS_CA_FILE` | unset | Absolute CA bundle path; requires TLS. |
| `LOOP_ORCHESTRATOR_TLS_CLIENT_CERT_FILE` | unset | Absolute mTLS certificate path; requires the client key. |
| `LOOP_ORCHESTRATOR_TLS_CLIENT_KEY_FILE` | unset | Absolute mTLS key path; requires the client certificate. |
| `LOOP_ORCHESTRATOR_TLS_SERVER_NAME` | unset | TLS server-name override; requires TLS. |

The Compose runner uses the OpenCode backend installed by its image and persists `/data` in the `runner-data` volume.

## Validation

```bash
go test -race ./...
go build ./cmd/runner
```
