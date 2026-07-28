# Moirai Runner

The runner is the isolated execution environment for software-engineering tasks. It accepts one job at a time and connects outbound to the orchestrator.

## Configuration

All values are optional unless noted. Comma-separated lists ignore surrounding whitespace.

| Variable | Default | Purpose |
|---|---|---|
| `LOOP_ORCHESTRATOR_ENDPOINT` | `orchestrator:50051` | Orchestrator host and port. |
| `LOOP_RUNNER_DATA_DIR` | `/data` | Absolute directory for identity, event outbox, and workspaces. |
| `LOOP_RUNNER_NAME` | hostname | Runner display identity. |
| `LOOP_RUNNER_LABELS` | empty | Comma-separated runner capability labels. |
| `LOOP_RUNNER_REGISTRATION_TOKEN_FILE` | empty | Absolute path to the one-time registration-token secret file. |
| `LOOP_RUNNER_HEARTBEAT_INTERVAL` | `10s` | Heartbeat interval. |
| `LOOP_RUNNER_RECONNECT_MIN` | `1s` | Initial reconnect backoff. |
| `LOOP_RUNNER_RECONNECT_MAX` | `1m` | Maximum reconnect backoff. |
| `LOOP_RUNNER_CAPACITY` | `1` | Concurrent execution capacity; must be positive. |
| `LOOP_RUNNER_MINIMUM_FREE_BYTES` | `1073741824` | Required free space in the runner data directory. |
| `LOOP_RUNNER_ALLOWED_ENVIRONMENT` | empty | Comma-separated environment variable names allowed into agent processes. |
| `LOOP_RUNNER_REDACTION_PREFIXES` | empty | Comma-separated sensitive-variable prefixes removed from logs. |
| `LOOP_RUNNER_RETAIN_WORKSPACES` | empty | Comma-separated terminal states to retain: `succeeded`, `failed`, `abandoned`. |
| `LOOP_RUNNER_DOCKER_ENABLED` | `false` | Allow Docker-backed execution. |
| `LOOP_RUNNER_AGENT_BACKEND` | `opencode` | Agent backend: `opencode`, `cli`, or `docker`. |
| `LOOP_RUNNER_AGENT_BINARY` | empty | Required executable name when `LOOP_RUNNER_AGENT_BACKEND=cli`. |
| `LOOP_RUNNER_AGENT_ARGUMENTS` | empty | Comma-separated agent arguments. |
| `LOOP_RUNNER_AGENT_DOCKER_IMAGE` | empty | Required image when `LOOP_RUNNER_AGENT_BACKEND=docker`. |
| `LOOP_ORCHESTRATOR_TLS` | `false` | Enable TLS for the orchestrator connection. |
| `LOOP_ORCHESTRATOR_TLS_CA_FILE` | empty | Absolute CA bundle path; requires TLS. |
| `LOOP_ORCHESTRATOR_TLS_CLIENT_CERT_FILE` | empty | Absolute client certificate path; requires TLS and a matching key. |
| `LOOP_ORCHESTRATOR_TLS_CLIENT_KEY_FILE` | empty | Absolute client key path; requires TLS and a matching certificate. |
| `LOOP_ORCHESTRATOR_TLS_SERVER_NAME` | empty | TLS server name override; requires TLS. |

Registration credentials must be provided through `LOOP_RUNNER_REGISTRATION_TOKEN_FILE`; raw registration-token environment variables are not read. In Compose, this is mounted as a Docker secret at `/run/secrets/runner_registration_token`.

## Health Probes

`runner live` verifies the process can start. `runner ready` additionally validates its configuration, data directory capacity, and selected agent backend prerequisites.

The Compose runner uses the OpenCode backend installed by its image and persists `/data` in the `runner-data` volume.

Run tests with `go test -race ./...` from this directory.
