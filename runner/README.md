# Moirai Runner

The runner is the isolated execution environment for software-engineering tasks. It accepts one job at a time and connects outbound to the orchestrator.

## Configuration

All values are optional unless noted. Comma-separated lists ignore surrounding whitespace.
- `LOOP_ORCHESTRATOR_ENDPOINT`: Orchestrator address (default `orchestrator:50051`).
- `LOOP_RUNNER_DATA_DIR`: Data directory (default `/data`).
- `LOOP_RUNNER_REGISTRATION_TOKEN`: Token for registration.
- `LOOP_RUNNER_METRICS_BIND`: Metrics listener (default `:9091`), serving `/metrics`.
- `LOOP_ORCHESTRATOR_TLS`: Enable TLS to the orchestrator. Configure CA, client certificate, client key, and server name with the matching `LOOP_ORCHESTRATOR_TLS_*` variables when required.
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

## Agent Result Document

Every agent execution must write the result document named by the task packet's `expectedOutput` (default `.loop/result.json`, validated against `schemas/agent-result.schema.json`). The runner treats it as the only evidence of what the agent did: it must be valid JSON with `protocolVersion` `1.0`, an `executionId` matching the execution, a non-empty `summary`, and a `status` of `completed`, `blocked`, or `failed`.

Exiting successfully is not a result. Every backend — `opencode`, `cli`, and `docker` — reports a `failed` terminal event when the document is missing or invalid, naming the missing evidence (for example `agent exited 0 without a valid result document (.loop/result.json): agent result was not written`). A process that fails outright reports the process failure instead, so the orchestrator receives distinct failure fingerprints for "the agent crashed" and "the agent claimed nothing".

## Execution Events

Execution events are queued in a bounded in-memory buffer (`LOOP_RUNNER_EVENT_BUFFER_SIZE`) that is mirrored to a crash-safe outbox in `LOOP_RUNNER_DATA_DIR`, and delivered in order on every reconnect. Terminal events (`completed`, `failed`, `cancelled`) carry the only record of a run's outcome, so they are never dropped silently:

- When the buffer is full, a terminal event evicts the oldest queued lower-priority event (`log` and `progress` first, then `started`) instead of being rejected. Log and progress events are never allowed to evict anything.
- Lease expiry discards a job's queued log and progress events but keeps its terminal events, still fenced by their lease generation, and keeps the expired lease just long enough for the still-running execution to report its outcome.
- The effective buffer size is raised to `LOOP_RUNNER_CAPACITY` when it is configured lower, so every concurrent execution keeps a terminal-event slot.
- A terminal event that still cannot be queued is logged at `ERROR` with `msg="terminal execution event lost"` plus the job, execution, lease generation, and reason. One that is queued but not yet delivered is logged at `WARN` and retried from the outbox.

The orchestrator remains authoritative: it fences every event on the lease generation and currently rejects events whose lease has expired. The runner's job is to make sure the outcome is durably recorded and offered for delivery.

## Health Probes

`runner live` verifies the process can start. `runner ready` additionally validates its configuration, data directory capacity, and selected agent backend prerequisites.

The Compose runner uses the OpenCode backend installed by its image and persists `/data` in the `runner-data` volume.

Run tests with `go test -race ./...` from this directory.
Run tests: `go test -race ./...`

Run static analysis: `go vet ./...`

