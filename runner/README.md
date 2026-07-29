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
| `LOOP_RUNNER_ALLOWED_ENVIRONMENT` | | Comma-separated task environment variable allow-list; set `GITHUB_TOKEN` for GitHub-backed projects. |
| `LOOP_RUNNER_HEARTBEAT_INTERVAL` | `10s` | Heartbeat, local lease-expiry, and offer-reservation expiry check interval. |
| `LOOP_RUNNER_RECONNECT_MIN` / `LOOP_RUNNER_RECONNECT_MAX` | `1s` / `1m` | Control-stream exponential-backoff bounds. |
| `LOOP_RUNNER_RECONNECT_GRACE` | `1m` | Reconnection grace configuration. |
| `LOOP_RUNNER_LEASE_DURATION` / `LOOP_RUNNER_LEASE_RENEWAL_LEAD` | `1m` / `15s` | Lease request duration and renewal lead. |
| `LOOP_RUNNER_OFFER_TIMEOUT` | `30s` | How long an accepted offer waits for its lease acknowledgement before the runner releases the capacity slot. The slot is freed on the first expiry sweep at or after this point, so the effective wait is up to one `LOOP_RUNNER_HEARTBEAT_INTERVAL` longer. |
| `LOOP_RUNNER_EVENT_BUFFER_SIZE` | `128` | Bounded queued execution-event count. |
| `LOOP_RUNNER_EVENT_PAYLOAD_BYTES` / `LOOP_RUNNER_LOG_CHUNK_BYTES` | `16384` / `6144` | Event payload and log-chunk limits. |
| `LOOP_RUNNER_MAX_LOG_BYTES` | `4194304` | Per-stream persisted agent-log limit. |
| `LOOP_RUNNER_TERMINATION_GRACE` | `5s` | Local process termination grace configuration. |
| `LOOP_RUNNER_REDACTION_PREFIXES` | | Comma-separated additional secret prefixes redacted from events. |
| `LOOP_RUNNER_RETAIN_WORKSPACES` | `failed` | Comma-separated terminal outcomes whose workspace is kept: `succeeded`, `failed`, `abandoned`. Use `none` to keep nothing. |
| `LOOP_RUNNER_RETENTION_MAX_AGE` | `72h` | How long a retained workspace survives before the sweep releases it. |
| `LOOP_RUNNER_RETENTION_MAX_WORKSPACES` | `10` | How many retained workspaces may coexist; the oldest are released first. |
| `LOOP_RUNNER_PUSH_WORK_IN_PROGRESS` | `true` | Publish a failed run's commit to `wip/<executionId>` when the packet allows pushing. |
| `LOOP_RUNNER_MINIMUM_FREE_BYTES` | `1073741824` | Minimum free workspace-disk bytes. Also the level below which the retention sweep releases retained workspaces. |
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

TLS settings are `LOOP_ORCHESTRATOR_TLS`, `LOOP_ORCHESTRATOR_TLS_CA_FILE`, `LOOP_ORCHESTRATOR_TLS_CLIENT_CERT_FILE`, `LOOP_ORCHESTRATOR_TLS_CLIENT_KEY_FILE`, and `LOOP_ORCHESTRATOR_TLS_SERVER_NAME`.

## Task credentials

A task packet declares the credentials it needs as `environmentRefs` — a variable name plus an opaque audit reference. The runner resolves each name against its own environment before the workspace is prepared, so the same credential authenticates the `git clone --mirror` and `git fetch` that populate the workspace and the `git push` that delivers the branch. Values are resolved from either the plain variable or a Docker-style `<NAME>_FILE` path to a mounted secret; secret values never travel in the packet.

A reference that is missing from `LOOP_RUNNER_ALLOWED_ENVIRONMENT`, or that the runner cannot resolve, fails the execution with a terminal `failed` event naming the variable. The runner never falls back to an unauthenticated Git operation.

`GITHUB_TOKEN` must be included in the allowed task environment for GitHub-backed projects. Delivery configures Git's GitHub HTTPS authorization header from this environment without putting the token in the `git push` argument list. Docker task secrets are supplied through a temporary `0600` env-file rather than Docker command-line environment arguments.

In Compose the runner receives it as `GITHUB_TOKEN_FILE=/run/secrets/github_token` with `LOOP_RUNNER_ALLOWED_ENVIRONMENT=GITHUB_TOKEN`.



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
| `LOOP_RUNNER_ALLOWED_ENVIRONMENT` | empty | Comma-separated environment variable names a task packet may request. |
| `GITHUB_TOKEN` / `GITHUB_TOKEN_FILE` | empty | Code-host credential resolved for task packets that declare `GITHUB_TOKEN`; the `_FILE` form reads a mounted secret. |
| `LOOP_RUNNER_REDACTION_PREFIXES` | empty | Comma-separated sensitive-variable prefixes removed from logs. |
| `LOOP_RUNNER_RETAIN_WORKSPACES` | `failed` | Comma-separated terminal states to retain: `succeeded`, `failed`, `abandoned`, or `none`. |
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

Registration credentials must be provided through `LOOP_RUNNER_REGISTRATION_TOKEN_FILE`; raw registration-token environment variables are not read. In Compose, this is mounted as a Docker secret at `/run/secrets/runner_registration_token`, alongside the shared `github_token` secret at `/run/secrets/github_token`.

## Agent Result Document

Every agent execution must write the result document named by the task packet's `expectedOutput` (default `.loop/result.json`, validated against `schemas/agent-result.schema.json`). The runner treats it as the only evidence of what the agent did: it must be valid JSON with `protocolVersion` `1.0`, an `executionId` matching the execution, a non-empty `summary`, and a `status` of `completed`, `blocked`, or `failed`.

Exiting successfully is not a result. Every backend — `opencode`, `cli`, and `docker` — reports a `failed` terminal event when the document is missing or invalid, naming the missing evidence (for example `agent exited 0 without a valid result document (.loop/result.json): agent result was not written`). A process that fails outright reports the process failure instead, so the orchestrator receives distinct failure fingerprints for "the agent crashed" and "the agent claimed nothing".

## Workspace Preparation

Every execution gets a fresh workspace at `workspaces/job-<jobId>`: the previous one is removed when its execution ends, and the next execution of the same job may be leased by another runner. The directory is therefore not what carries a job's work from one execution to the next — the execution branch is. Every execution of a job shares one branch name (`agent/<issueExternalId>/<jobId>`), and preparation re-creates the workspace from that branch's tip, looked for in this order:

1. the branch as published on the remote, found with `git ls-remote` and fetched — the one state of the job every runner can see, so it decides the tip;
2. otherwise the branch in this runner's own repository, which is where an execution that could not push leaves its work;
3. otherwise the default branch — the start point of a job's first execution, stated rather than implied, because `git worktree add -B` would just as happily rewind an existing branch onto it ([#136](https://github.com/alexandre-leites/moirai/issues/136)).

This is what makes the mandatory local pipeline ([#90](https://github.com/alexandre-leites/moirai/issues/90)) mean anything: the pipeline execution runs the project's commands against the tree the developer execution produced, and a reviewer execution reads that tree rather than base-branch code.

## Failed Work and Workspace Retention

Iterative repair needs the previous attempt's work, so a run that does not complete is not discarded.

**Push semantics.** What a run may write to the repository depends on its outcome, and the two are never confused:

| Outcome | Commit | Anchored locally | Pushed | Terminal payload |
| --- | --- | --- | --- | --- |
| `completed` | delivery message, on the packet's branch | — | `origin/<branch>`, upstream set, when `mayPush` | `branch`, `pushed: true` |
| `failed` | `wip(failed): …` | `refs/moirai-wip/<executionId>` | `wip/<executionId>` when `mayPush` and `LOOP_RUNNER_PUSH_WORK_IN_PROGRESS` | `wipBranch`, `wipCommit`, `wipPushed`, `logTail` |
| `blocked` | `wip(blocked): …` | as `failed` | as `failed` | as `failed` |
| cancelled / abandoned | none — the context is already cancelled | — | — | `status: cancelled` |

Only a completed run writes to the packet's branch, so a non-delivery can never be mistaken for a delivery. A failed or blocked run publishes to a per-execution `wip/<executionId>` ref instead, which cannot collide with the branch the next attempt continues from. That ref is force-pushed: it belongs to exactly one execution, which must be able to replace its own earlier remains after a redelivery.

The commit itself sits on the execution branch, which the next preparation of that job continues from, so a retry on this runner inherits the failed run's work directly. The local `refs/moirai-wip/<executionId>` anchor — written for every non-delivering run — covers what the branch cannot: when the branch has been published elsewhere, preparation resets the local branch onto the published tip, and only a reference outside `refs/heads` still reaches the failed run's commit. This matters most for roles the orchestrator does not grant `mayPush`: today that is every file-modifying role except `developer`, so a **repairer**'s work is preserved only locally, on the runner that produced it, until #106 or a `mayPush` grant lets it be published.

`logTail` is a sanitised excerpt of the failing pipeline command's output, or of the agent's log, bounded to 2 KiB *as JSON encodes it* rather than raw, since `<`, `>`, and `&` cost six bytes each in the encoded payload.

Note that pipeline commands currently reach the runner only on `role=pipeline` packets, which may not modify files and so have nothing to retain; in practice the paths above are exercised by a failing or blocked `developer`/`repairer` execution. A developer packet carrying pipeline commands is valid and handled (the pipeline failure then retains the agent's diff), but the orchestrator does not build that shape today.

**Retention.** `LOOP_RUNNER_RETAIN_WORKSPACES` defaults to `failed`, keeping the worktree, `terminal-result.json`, and the agent's `*.stdout.log` / `*.stderr.log`. Retention is bounded, because keeping everything would eventually fill the disk:

- Every retained workspace is registered in `<LOOP_RUNNER_DATA_DIR>/retained`. A workspace that cannot be registered is cleaned up instead, so nothing is kept that the sweep could not later release.
- The sweep runs at startup and before every execution — the moments at which new workspace disk is about to be consumed — and releases workspaces older than `LOOP_RUNNER_RETENTION_MAX_AGE`, then the oldest beyond `LOOP_RUNNER_RETENTION_MAX_WORKSPACES`, then the oldest remaining while free disk is under `LOOP_RUNNER_MINIMUM_FREE_BYTES`. An idle runner therefore holds up to `LOOP_RUNNER_RETENTION_MAX_WORKSPACES` workspaces past their age bound until it next starts an execution.
- A job whose execution is running is never swept, even when a retained record still names its ID — one job ID serves every execution of a workflow run, so the record of an earlier execution names the path the next one prepares.
- A retained workspace's HEAD is detached, so it can never be the reason a later preparation fails to fetch or to re-create that branch.

How long the forensics last: a retained workspace lives at `workspaces/job-<jobId>`, and the *next execution of the same job* removes that directory when it prepares. Retention therefore covers inspection after a failure and up to the workflow's next attempt — the durable artefact across attempts is the work-in-progress commit, not the workspace.

Consuming any of this is still the orchestrator's to do: the runner reports `wipBranch`/`wipCommit`, but nothing yet turns them into a retry packet's `currentCommit`/`diffSummary` (see [#106](https://github.com/alexandre-leites/moirai/issues/106)). Nothing prunes `wip/*` on the code host or `refs/moirai-wip/*` in the runner's repositories either; both grow with the number of non-delivering executions.

## Execution Events

Execution events are queued in a bounded in-memory buffer (`LOOP_RUNNER_EVENT_BUFFER_SIZE`) that is mirrored to a crash-safe outbox in `LOOP_RUNNER_DATA_DIR`, and delivered in order on every reconnect. Terminal events (`completed`, `failed`, `cancelled`) carry the only record of a run's outcome, so they are never dropped silently:

- When the buffer is full, a terminal event evicts the oldest queued lower-priority event (`log` and `progress` first, then `started`) instead of being rejected. Log and progress events are never allowed to evict anything.
- Lease expiry discards a job's queued log and progress events but keeps its terminal events, still fenced by their lease generation, and keeps the expired lease just long enough for the still-running execution to report its outcome. Each expired generation is retained separately, so a job that is re-offered at the next generation does not displace the outcome still owed by the superseded execution.
- The effective buffer size is raised to twice `LOOP_RUNNER_CAPACITY` when configured lower, covering each running execution plus one whose lease expired while it was still winding down. This is a floor, not a guarantee.
- A terminal event that cannot be queued is retried once stripped to its classification fields (`status`, `exitCode`, `error`, `failureFingerprint`, `durationMs`, `branch`), each truncated to 2 KiB, so neither an oversized result document nor an agent that dumps its stderr into the returned error costs the run its outcome — only its detail.
- A terminal event that still cannot be queued is logged at `ERROR` with `msg="terminal execution event lost"` plus the job, execution, lease generation, and reason. One that is queued but not yet delivered is logged at `WARN` and retried from the outbox.

The orchestrator remains authoritative: it fences every event on the lease generation. Note that it currently *rejects* an event whose lease has expired — `expire_leases` bumps the generation, so a terminal event reported after expiry is discarded and the control stream is aborted. The runner still records and offers the outcome; accepting it as recovery evidence is tracked as the orchestrator half of #93.

## Health Probes

`runner live` verifies the process can start. `runner ready` additionally validates its configuration, data directory capacity, and selected agent backend prerequisites.

The Compose runner uses the OpenCode backend installed by its image and persists `/data` in the `runner-data` volume.

Run tests with `go test -race ./...` from this directory.
Run tests: `go test -race ./...`

Run static analysis: `go vet ./...`

