# Moirai Runner

Isolated execution environment for software-engineering tasks.

## Configuration

All runner settings use `LOOP_RUNNER_*`; orchestrator transport settings use `LOOP_ORCHESTRATOR_*`.

| Variable | Default | Description |
| --- | --- | --- |
| `LOOP_ORCHESTRATOR_ENDPOINT` | `orchestrator:50051` | Orchestrator gRPC endpoint. |
| `LOOP_RUNNER_DATA_DIR` | `/data` | Absolute directory for runner state and workspaces. |
| `LOOP_RUNNER_NAME` | hostname | Runner identity name. |
| `LOOP_RUNNER_REGISTRATION_TOKEN` | | One-time registration token. |
| `LOOP_RUNNER_LABELS` | | Comma-separated capability labels. |
| `LOOP_RUNNER_CAPACITY` | `1` | Concurrent execution capacity. |
| `LOOP_RUNNER_ALLOWED_ENVIRONMENT` | | Comma-separated task environment variable allow-list. |
| `LOOP_RUNNER_HEARTBEAT_INTERVAL` | `10s` | Heartbeat and local lease-expiry check interval. |
| `LOOP_RUNNER_RECONNECT_MIN` / `LOOP_RUNNER_RECONNECT_MAX` | `1s` / `1m` | Control-stream exponential-backoff bounds. |
| `LOOP_RUNNER_RECONNECT_GRACE` | `1m` | Reconnection grace configuration. |
| `LOOP_RUNNER_LEASE_DURATION` / `LOOP_RUNNER_LEASE_RENEWAL_LEAD` | `1m` / `15s` | Lease request duration and renewal lead. |
| `LOOP_RUNNER_OFFER_TIMEOUT` | `30s` | Job-offer timeout configuration. |
| `LOOP_RUNNER_EVENT_BUFFER_SIZE` | `128` | Bounded queued execution-event count. |
| `LOOP_RUNNER_EVENT_PAYLOAD_BYTES` / `LOOP_RUNNER_LOG_CHUNK_BYTES` | `16384` / `6144` | Event payload and log-chunk limits. |
| `LOOP_RUNNER_MAX_LOG_BYTES` | `4194304` | Per-stream persisted agent-log limit. |
| `LOOP_RUNNER_TERMINATION_GRACE` | `5s` | Local process termination grace configuration. |
| `LOOP_RUNNER_REDACTION_PREFIXES` | | Comma-separated additional secret prefixes redacted from events. |
| `LOOP_RUNNER_RETAIN_WORKSPACES` | | Comma-separated `succeeded`, `failed`, and `abandoned` retention policy. |
| `LOOP_RUNNER_MINIMUM_FREE_BYTES` | `1073741824` | Minimum free workspace-disk bytes. |
| `LOOP_RUNNER_REPOSITORY_LOCK_POLL` | `25ms` | Repository worktree-lock retry interval. |
| `LOOP_RUNNER_CLEANUP_ATTEMPTS` / `LOOP_RUNNER_CLEANUP_RETRY_DELAY` | `3` / `250ms` | Workspace cleanup retry policy. |
| `LOOP_RUNNER_GIT_COMMITTER_NAME` / `LOOP_RUNNER_GIT_COMMITTER_EMAIL` | `moirai-runner` / `moirai-runner@localhost` | Git identity used for delivery commits. |
| `LOOP_RUNNER_DOCKER_ENABLED` | `false` | Run pipeline commands in Docker. Requires `LOOP_RUNNER_AGENT_DOCKER_IMAGE`. |
| `LOOP_RUNNER_AGENT_BACKEND` | `opencode` | `opencode`, `cli`, or `docker`. |
| `LOOP_RUNNER_AGENT_BINARY` | | CLI backend executable. |
| `LOOP_RUNNER_AGENT_ARGUMENTS` | | Comma-separated agent arguments. |
| `LOOP_RUNNER_AGENT_DOCKER_IMAGE` | | Agent/pipeline image for Docker execution. |
| `LOOP_RUNNER_DOCKER_CPU_LIMIT` / `LOOP_RUNNER_DOCKER_MEMORY_LIMIT` | | Docker resource limits. |
| `LOOP_RUNNER_DOCKER_NETWORK` | `bridge` | Docker network mode; use a network with provider access for autonomous agents. |
| `LOOP_RUNNER_DOCKER_STOP_TIMEOUT` | `10s` | Docker graceful-stop timeout. |

`GITHUB_TOKEN` may be included in the allowed task environment. Delivery configures Git's GitHub HTTPS authorization header from this environment without putting the token in the `git push` argument list. Docker task secrets are supplied through a temporary `0600` env-file rather than Docker command-line environment arguments.

TLS settings are `LOOP_ORCHESTRATOR_TLS`, `LOOP_ORCHESTRATOR_TLS_CA_FILE`, `LOOP_ORCHESTRATOR_TLS_CLIENT_CERT_FILE`, `LOOP_ORCHESTRATOR_TLS_CLIENT_KEY_FILE`, and `LOOP_ORCHESTRATOR_TLS_SERVER_NAME`.

## Testing

Run tests: `go test -race ./...`

Run static analysis: `go vet ./...`
