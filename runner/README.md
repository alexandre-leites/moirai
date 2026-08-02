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
| `LOOP_ORCHESTRATOR_HEADERS` | | JSON object of headers sent with every orchestrator RPC. Requires TLS. |
| `LOOP_ORCHESTRATOR_HEADERS_FILE` | | Absolute path to a JSON header object. Preferred for secret values; read again for each RPC so rotated credentials apply on reconnect. Requires TLS. |
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
| `LOOP_RUNNER_MAX_CONTINUATIONS` | `3` | How many times one execution may re-engage its agent after the goal gate finds the objective unmet. `0` switches the goal loop off; the maximum is `10`. Continuations run inside the same lease and inside the packet's own `timeoutSeconds`, so this changes neither the orchestrator's budgets nor an execution's wall-clock *bound* — though a continuing execution does occupy its runner and project slot for longer within that bound. |
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

### Cloudflare Access

Cloudflare Tunnel must expose the orchestrator origin as gRPC with HTTP/2 end to end. Enable runner TLS and mount a header file readable only by the runner:

```json
{"CF-Access-Client-Id":"<service-token-id>.access","CF-Access-Client-Secret":"<service-token-secret>"}
```

```sh
LOOP_ORCHESTRATOR_ENDPOINT=orchestrator.example.com:443
LOOP_ORCHESTRATOR_TLS=true
LOOP_ORCHESTRATOR_HEADERS_FILE=/run/secrets/orchestrator_headers
```

The runner lowercases metadata keys on the wire, so Cloudflare receives `cf-access-client-id` and `cf-access-client-secret`. A rejected service token returns an authentication status and stops reconnecting; a broken Tunnel or non-gRPC origin returns a transport status and retries with backoff. Header values are never logged. Do not set headers without TLS: configuration rejects it before dialing.

## Task credentials

A task packet declares the credentials it needs as `environmentRefs` — a variable name plus an opaque audit reference. The runner resolves each name against its own environment before the workspace is prepared, so the same credential authenticates the `git clone --mirror` and `git fetch` that populate the workspace and the `git push` that delivers the branch. Values are resolved from either the plain variable or a Docker-style `<NAME>_FILE` path to a mounted secret; secret values never travel in the packet.

A reference that is missing from `LOOP_RUNNER_ALLOWED_ENVIRONMENT`, or that the runner cannot resolve, fails the execution with a terminal `failed` event naming the variable. The runner never falls back to an unauthenticated Git operation.

`GITHUB_TOKEN` must be included in the allowed task environment for GitHub-backed projects. Delivery configures Git's GitHub HTTPS authorization header from this environment without putting the token in the `git push` argument list. Docker task secrets are supplied through a temporary `0600` env-file rather than Docker command-line environment arguments.

In Compose the runner receives it as `GITHUB_TOKEN_FILE=/run/secrets/github_token` with `LOOP_RUNNER_ALLOWED_ENVIRONMENT=GITHUB_TOKEN`.



Registration credentials must be provided through `LOOP_RUNNER_REGISTRATION_TOKEN_FILE`; raw registration-token environment variables are not read. In Compose, this is mounted as a Docker secret at `/run/secrets/runner_registration_token`, alongside the shared `github_token` secret at `/run/secrets/github_token`.

## Agent Result Document

Every agent execution must write the result document named by the task packet's `expectedOutput` (default `.loop/result.json`, validated against `schemas/agent-result.schema.json`). The runner treats it as the only evidence of what the agent did: it must be valid JSON with `protocolVersion` `1.0`, an `executionId` matching the execution, a non-empty `summary`, and a `status` of `completed`, `blocked`, or `failed`.

Exiting successfully is not a result. Every backend — `opencode`, `cli`, and `docker` — reports a `failed` terminal event when the document is missing or invalid, naming the missing evidence (for example `agent exited 0 without a valid result document (.loop/result.json): agent result was not written`). A process that fails outright reports the process failure instead, so the orchestrator receives distinct failure fingerprints for "the agent crashed" and "the agent claimed nothing".

**An agent-reported block is not a crash.** An agent that finishes cleanly and writes `status: blocked` stopped deliberately and said why, so its account crosses the wire intact rather than being flattened into an anonymous failure:

- The terminal event type stays `failed` — the vocabulary is a shared contract with the orchestrator (`VALID_EVENT_TYPES`), the transport, and `app.jobs.status` — and the payload carries `status: "blocked"` with `blocked: true`.
- The result document (`result`), the agent's `summary`, and its `remainingWork` are attached to every terminal payload for an outcome the agent itself reached — `completed`, `failed`, and `blocked` alike, where previously only `completed` carried the document. A `cancelled` run reached no outcome of its own and still reports none; that also keeps repeated cancellations identifiable, since the orchestrator fingerprints them from a stable `cancelled exit=N` text derived only when no `error` or `summary` is present.
- `summary` is bounded to 2 KiB and `remainingWork` to 20 entries / 2 KiB of agent text, measured *as JSON encodes it* and sanitised of terminal escapes, control bytes, and invalid UTF-8 first — the same rule and the same helpers as `logTail` below, for the same reason. Truncation is marked. This is what lets the reduced-payload retry keep them while dropping the unbounded `result` document.
- A failing *process* never reports a block, whatever its document claims: a crashed agent's account of why it stopped is not evidence. Its executor error is reported instead.
- An agent result that is not `completed` skips the packet's pipeline commands, since a pipeline verdict would replace the agent's status, reason, and remaining work with a generic failure.

The orchestrator routes the block to the terminal `blocked` status with `blocking_reason` composed from the agent's summary and remaining work, clearing the gate the reporting role owns (`workflows/runner_events.py`).

## Goal Gate and Continuations

A process exit is not evidence that the objective was met. The most common autonomy failure is an agent ending its own reasoning loop early — half the work done, "I will now do X" followed by an exit, or a clean refusal — and reporting each of those as a terminal outcome after one shot throws away a run the runner could still finish. So after the agent exits, the runner asks a deterministic question of its own and, while the answer is "no" and budget remains, continues the agent in the same session ([#104](https://github.com/alexandre-leites/moirai/issues/104)).

**The gate.** A run counts as *delivered* only when all of these hold:

1. the result document is valid — an absent or invalid one is already a failure rather than a clean exit (see above);
2. its `status` is `completed`;
3. its `remainingWork` is empty;
4. for a role that may modify files, the workspace shows a change — either uncommitted changes or a moved `HEAD`.

Check 4 does not apply to a role that may not modify files, so planners and reviewers never enter the loop; it is also skipped, rather than failed, when the diff cannot be measured, since an unmeasurable workspace is not evidence of an idle agent.

**The continuation.** A failed gate with budget left re-invokes the agent through the backend's `Continue`, resuming the `sessionId` the result document reported, so the agent keeps its own reasoning context instead of re-deriving it. The prompt states the missing evidence ("No result document was written at `.loop/result.json`", "You listed work as still remaining: …", "No file in the repository was changed"), asks the agent to continue rather than start over, and then repeats the role's whole original prompt unchanged — that matters because the fallback path starts a *fresh* agent, and it must be held to the same role, plan, prior failures, and review findings the first run was. Each prompt is kept in the workspace as `.loop/continuation-<n>.md`. Backends without sessions — `cli` and `docker` — and an execution whose agent never reported a session ID take that fallback.

Each attempt is judged on evidence it produced itself: the finished attempt's result document is moved to `.loop/result-attempt-<n>.json` before the next one starts. Without that, a continuation that exits without writing anything would be assessed against its predecessor's document — the "a clean exit is not a result" hole, re-opened inside one execution. A document that cannot be moved aside stops the loop rather than being continued around.

**The loop guard.** Continuing is only worthwhile while something changes. The gate's verdict is fingerprinted from its reason codes, the agent's remaining-work list, and the repository's revision plus changed-path set; two consecutive identical fingerprints mean the agent is wedged, and the run is reported terminal with what it has. A declared block therefore usually costs a single continuation — the blocker is sometimes clearable once asked, and re-declaring the same block over an unchanged workspace trips the guard on the next pass.

**A continuation can never make a run worse.** An attempt that ended in an executor error — a signal, a timeout, or a clean exit with no result document — never overrides an attempt that did not, so a crashed or timed-out continuation cannot replace a completed run's delivery, or an agent-declared block's stated reason, with an anonymous failure. Outcomes the agent itself reached are not ranked against each other: an agent that completes and then, asked to continue, declares itself blocked has retracted its own claim with a stated reason, and the retraction stands. `continuations` is what says another attempt was made.

**Bounds.** The loop terminates on five independent limits: the continuation budget, the loop guard, the execution context (a cancelled or expired lease is never answered with another agent process), a result document that cannot be set aside, and the packet's `timeoutSeconds`, which bounds the *total* agent wall clock rather than each invocation — the first run receives the whole packet timeout and every continuation only what is left, and a remainder too small to fund a real run ends the loop instead. Because the total is unchanged, lease renewal needs nothing new: `StreamSupervisor`'s heartbeat drives `ControlLoop.Reconcile` → `OfferState.RenewDue` on its own ticker while executions run in their own goroutines, so renewal never depended on how long an execution takes, and an execution still cannot outlive the bound it always had.

**Reporting.** Terminal payloads carry `continuations` (omitted when zero) and `gateVerdict`, so the orchestrator can tell "delivered after 2 continuations" from "gave up after 3" — a distinction the terminal status alone does not make. Both are runner-derived and drawn from a closed vocabulary (`delivered`, `not delivered (continuation budget exhausted): …`, `identical verdict repeated`, `execution time budget exhausted`, `execution cancelled`, `previous evidence could not be set aside`), so they are bounded, stable across executions, and safe for the failure fingerprints. They survive the reduced-payload retry an oversized event falls back to. All of this happens inside one lease and one execution, so no orchestrator budget, protocol message, or workflow state changes. The gate never rewrites an outcome into a different one either: it decides whether to continue, which attempt is reported, and what travels alongside it — the terminal status is still whatever that attempt reached.

## Workspace Preparation

Every execution gets a fresh workspace at `workspaces/job-<jobId>`: the previous one is removed when its execution ends, and the next execution of the same job may be leased by another runner. The directory is therefore not what carries a job's work from one execution to the next — the execution branch is. Every execution of a job shares one branch name (`agent/<issueExternalId>/<first 8 characters of jobId>`, built by the orchestrator's task-packet builder for every role), and preparation re-creates the workspace from that branch's tip, looked for in this order:

1. When `git ls-remote` finds the branch on the remote, its tip is fetched into `refs/moirai-remote/<branch>` — a reference of the runner's own, with `--refmap=` so the fetch cannot also write `refs/heads/<branch>`. A managed cache is a mirror, and its configured `+refs/*:refs/*` would otherwise force-update the branch whatever destination the refspec names, discarding an unpushed commit.
2. This runner's own copy of the branch is what the workspace starts from when that copy already contains the published tip. That copy is the *only* record of the work of an execution whose role was not granted `mayPush` — today every file-modifying role except `developer` — because such an execution can also *complete* without publishing, and every such commit is anchored at `refs/moirai-wip/<executionId>`.
3. Otherwise the published tip decides: the branch has diverged, or this runner is behind, and the published tip is the state every runner resolves identically. A local commit left behind that way came from a run whose work was not published, which is anchored at `refs/moirai-wip/<executionId>`.
4. A job whose branch exists neither on the remote nor here starts from the default branch — its first execution. That start point is stated rather than implied, because `git worktree add -B` would just as happily rewind an existing branch onto it ([#136](https://github.com/alexandre-leites/moirai/issues/136)).

This is what makes the mandatory local pipeline ([#90](https://github.com/alexandre-leites/moirai/issues/90)) mean anything: the pipeline execution runs the project's commands against the tree the developer execution produced, and a reviewer execution reads that tree rather than base-branch code.

Two limits are worth knowing. Until [#147](https://github.com/alexandre-leites/moirai/issues/147) is fixed, no push from a `managed_clone` workspace succeeds, so in that mode nothing is ever published: step 1 does not fire, this runner's own copy of the branch is the only carrier, and a job resumed on a *different* runner still starts from the default branch. And nothing prunes `refs/moirai-remote/*`, which grows with the number of job branches a repository has seen, alongside the `refs/moirai-wip/*` anchors already noted below.

## Failed Work and Workspace Retention

Iterative repair needs the previous attempt's work, so a run that does not complete is not discarded.

**Push semantics.** What a run may write to the repository depends on its outcome, and the two are never confused:

| Outcome | Commit | Anchored locally | Pushed | Terminal payload |
| --- | --- | --- | --- | --- |
| `completed` | delivery message, on the packet's branch | — | `origin/<branch>`, upstream set, when `mayPush` | `branch`, `pushed: true`, `summary`, `remainingWork`, `result` |
| `failed` | `wip(failed): …` | `refs/moirai-wip/<executionId>` | `wip/<executionId>` when `mayPush` and `LOOP_RUNNER_PUSH_WORK_IN_PROGRESS` | `wipBranch`, `wipCommit`, `wipPushed`, `logTail`, `error`, `failureFingerprint`, `summary`, `remainingWork`, `result` |
| `blocked` | `wip(blocked): …` | as `failed` | as `failed` | as `failed`, plus `status: blocked` and `blocked: true` |
| cancelled / abandoned | none — the context is already cancelled | — | — | `status: cancelled` and the usage counters only — no agent account |

Only a completed run writes to the packet's branch, so a non-delivery can never be mistaken for a delivery. A failed or blocked run publishes to a per-execution `wip/<executionId>` ref instead, which cannot collide with the branch the next attempt continues from. That ref is force-pushed: it belongs to exactly one execution, which must be able to replace its own earlier remains after a redelivery.

The commit itself sits on the execution branch, which the next preparation of that job continues from, so a retry on this runner inherits the failed run's work directly. The local `refs/moirai-wip/<executionId>` anchor — written for every non-delivering run — covers what the branch cannot: when the branch has been published elsewhere, preparation resets the local branch onto the published tip, and only a reference outside `refs/heads` still reaches the failed run's commit. This matters most for roles the orchestrator does not grant `mayPush`: today that is every file-modifying role except `developer`, so a **repairer**'s work is preserved only locally, on the runner that produced it, until #106 or a `mayPush` grant lets it be published.

`logTail` is a sanitised excerpt of the failing pipeline command's output, or of the agent's log, bounded to 2 KiB *as JSON encodes it* rather than raw, since `<`, `>`, and `&` cost six bytes each in the encoded payload.

Note that pipeline commands currently reach the runner only on `role=pipeline` packets, which may not modify files and so have nothing to retain; in practice the paths above are exercised by a failing or blocked `developer`/`repairer` execution. A developer packet carrying pipeline commands is valid and handled (the pipeline failure then retains the agent's diff), but the orchestrator does not build that shape today. The pipeline runs only after a *completed* agent result: a failed or blocked agent has nothing worth validating, and its outcome must not be overwritten by the pipeline's.

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
- A terminal event that cannot be queued is retried once stripped to its classification fields (`status`, `exitCode`, `error`, `failureFingerprint`, `durationMs`, `branch`, the work-in-progress pointers, and the block fields `blocked`/`summary`/`remainingWork`), each re-bounded to 2 KiB *as JSON encodes it*, so neither an oversized result document nor an agent that dumps its stderr into the returned error costs the run its outcome — only its detail. Measuring raw bytes here would not be enough: three 2 KiB raw fields of `<`, `>`, or `&` encode to 36 KiB and the reduced event would be rejected too, losing the outcome the reduction exists to save. The block fields are kept because they are bounded; the raw `result` document is dropped because nothing bounds it.
- A terminal event that still cannot be queued is logged at `ERROR` with `msg="terminal execution event lost"` plus the job, execution, lease generation, and reason. One that is queued but not yet delivered is logged at `WARN` and retried from the outbox.

The orchestrator remains authoritative: it fences every event on the lease generation. Note that it currently *rejects* an event whose lease has expired — `expire_leases` bumps the generation, so a terminal event reported after expiry is discarded and the control stream is aborted. The runner still records and offers the outcome; accepting it as recovery evidence is tracked as the orchestrator half of #93.

## Metrics

`LOOP_RUNNER_METRICS_BIND` (default `:9091`) serves `GET /metrics` in the Prometheus text format. The runner exports only values it holds itself; queue depth and active workflow counts are orchestrator-owned state it cannot populate, and it no longer exports placeholders for them ([#124](https://github.com/alexandre-leites/moirai/issues/124)).

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `moirai_runner_heartbeat_age_seconds` | gauge | | Seconds since this runner last completed a control-stream heartbeat. Computed at scrape time from the process clock's monotonic reading, so a wall-clock correction cannot affect it, and counting from process start until the first heartbeat, so a runner that never connects still ages. |
| `moirai_runner_busy` | gauge | | `1` when the runner is at `LOOP_RUNNER_CAPACITY` and would reject the next offer, `0` when it can accept work. Published from the same predicate the offer admission uses, so the two cannot disagree. |
| `moirai_runner_executions_started_total` | counter | | Executions started. |
| `moirai_runner_executions_completed_total` | counter | `outcome` | Executions that reached a terminal outcome: `completed`, `failed`, `blocked`, or `cancelled`. |
| `moirai_runner_pending_events` | gauge | | Execution events queued for delivery, republished from the queue at every change. |
| `moirai_runner_events_dropped_total` | counter | `reason` | Execution events discarded rather than delivered. |

`reason` is one of:

| `reason` | Meaning |
| --- | --- |
| `buffer_full` | The bounded buffer had no room and nothing lower-priority to evict. |
| `evicted` | A queued event gave up its slot to a higher-priority one, in practice a terminal event. |
| `lease_expired` | A job's queued log and progress events were discarded when its lease expired. Its terminal events are kept. |
| `invalid` | The event type or its payload was unusable — most often a payload over the size limit. |
| `no_lease` | No lease of the reported generation was held: either none at all, or one at a different generation. |
| `persist_failed` | The crash-safe outbox write failed and the event was rolled back out of the queue. |
| `unknown` | The event was lost for a reason outside this vocabulary — in practice, the remainder of a multi-chunk log message abandoned after the control stream failed mid-delivery. |

Both label sets are closed: an unrecognised value is counted as `unknown` rather than creating a new series.

**A rejected event is not always a lost one, and only lost ones are counted.** A terminal event whose payload exceeds the limit is re-emitted stripped to its classification fields, and that retry usually succeeds — so it is counted only if the retry fails too. This is what keeps `rate(moirai_runner_events_dropped_total[5m]) > 0` a usable alert instead of something that fires whenever an agent is verbose. Log output has no such retry, so a rejected log chunk is counted immediately, together with the chunks of the same message that are consequently never attempted.

One drop path is **not** yet counted: when agent output arrives faster than it can be forwarded, the runner's in-process log forwarder discards chunks before they reach the event buffer, and reports that only as a `runner log events dropped` warning at the end of the execution. See [#124](https://github.com/alexandre-leites/moirai/issues/124).

Note that this runner's `moirai_runner_heartbeat_age_seconds` reports *its own* last heartbeat. The orchestrator exports a series of the same name meaning the age of the oldest heartbeat across the fleet; the scrape's `job`/`instance` labels distinguish them.

## Health Probes

`runner live` verifies the process can start. `runner ready` additionally validates its configuration, data directory capacity, and selected agent backend prerequisites.

The Compose runner uses the OpenCode backend installed by its image and persists `/data` in the `runner-data` volume.

Run tests with `go test -race ./...` from this directory.
Run tests: `go test -race ./...`

Run static analysis: `go vet ./...`

