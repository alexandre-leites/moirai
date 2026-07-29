# Architecture

Moirai separates public HTTP, control-plane state, and execution environments.

```text
Browser -> web nginx -> API -> orchestrator gRPC -> PostgreSQL
                                  ^
                                  |
                              runner gRPC
```

The API does not access PostgreSQL directly. The orchestrator owns migrations, authentication/session validation, project configuration, scheduling, workflow state, GitHub integration, and runner leases. Runners receive offers over an outbound gRPC stream and maintain local identity and event delivery state. The web service is a static UI with nginx proxying `/api/` to the API service in Compose.

Shared gRPC definitions live in `proto/`; generated Go code is in `gen/go/` and Python code is under `orchestrator/src/moirai/protocols/`. The HTTP surface is separately described by [`api/openapi.yaml`](../api/openapi.yaml).

## Job offer lifecycle

A workflow run receives many offers over its life: one bootstrap offer from `schedule()`, one per queued execution request from `schedule_execution()`, and one per lease recovery from `recover_one()`. A missed acknowledgement is a routine distributed-systems event, so an unanswered offer never decides the fate of the run's work (`persistence/control_plane.py`):

| Offer | Unanswered (expired, rejected, or undeliverable) |
| --- | --- |
| Bootstrap — run has no branch, pull request, execution, or execution request | Run is cancelled with `terminal_reason`, the project lock is released, and the issue returns to the global queue immediately. |
| Execution re-offer — a `dispatched` execution request exists | Request returns to `queued`, the job is released, the workflow keeps its status, phase, and project lock, and `schedule_execution()` re-offers the same job on a later tick. |
| Recovery re-offer — no execution request to requeue | Job returns to `recovering` with a fenced lease generation, the run returns to `recovering` keeping its phase and project lock, and `recover_one()` re-offers it. |

Every one of those outcomes leaves the execution request in a defined state, because "is there an open request?" is the predicate the stalled-run detector uses to decide whether a workflow still has work in flight. `accept_event` closes the request in the same transaction that records the runner's terminal event, and the maintenance loop closes the rows nothing can ever report on before it looks for stalled runs. See [execution request lifecycle](../orchestrator/README.md#execution-request-lifecycle).

Repeated failure is bounded rather than infinite: once a job accumulates `unanswered_offer_limit` consecutive unanswered offers (default 5) that have been failing for longer than `unanswered_offer_grace` (default 15 minutes), the run is blocked with `blocking_reason = 'unanswered_offer_limit'`, its outstanding execution requests are expired, and the project lock is released. Accepting an offer resets the streak. Both bounds are constructor arguments of `AsyncpgControlPlane`.

The scheduler applies the same rule in memory (`scheduler.py`): a task-packet build error is an orchestrator-side fault, so the candidate is skipped and its offer is left to expire on its own TTL; an undelivered offer is released through the control plane. Neither stops the tick, which keeps placing offers for other runners until the per-tick budget or `max_consecutive_failures` (default 3) is reached.

The runner has a matching reclamation of its own, on a different clock. Accepting an offer reserves a capacity slot before any lease exists, so `control.OfferState` stamps the reservation and releases it once `LOOP_RUNNER_OFFER_TIMEOUT` (default 30s) has passed without a `LeaseAcknowledged`, on the next expiry sweep (`LOOP_RUNNER_HEARTBEAT_INTERVAL`, default 10s). Release is purely local: acceptance has already moved the offer row to `accepted`, which `expire_offers` (`status = 'offered'` only) no longer matches, and `reject_offer` on it raises `OfferError` → `FAILED_PRECONDITION` → an aborted control stream. The orchestrator reclaims the job separately, through `expire_leases` against `app.jobs.lease_expires_at` on the scheduler's offer TTL (600s), which also marks the runner offline until its next heartbeat. The two clocks are deliberately unequal: the runner frees its slot in seconds so it can keep taking work, and only later does the orchestrator conclude the job needs recovering. In between, an acknowledgement that arrives after the reservation expired is discarded rather than starting an execution whose slot the runner may already have given away.

`ControlLoop.Busy()` counts reservations as well as active leases, so the runner cannot report availability it would refuse: it is the same predicate `OfferState.Admit` uses. Note that the orchestrator does not currently read the `Heartbeat.busy` field — placement is gated on its own `app.jobs` count against `runners.capacity` — so this is an internal consistency guarantee rather than a scheduling signal.

## Runner drain

A runner reports its own drain state on the control stream (`RunnerToOrchestrator.runner_draining`), both when it starts draining (`ControlLoop.Drain()`) and on every control stream it establishes (`ControlLoop.Resume()`, wired as `StreamSupervisor.OnConnected`). The orchestrator writes it to `app.runners.draining` through `set_runner_draining()` and keeps the stream open: a draining runner still has to renew leases and report events for the work it already holds, so treating the report as a protocol violation — which is what an unhandled message type does — would abort the stream mid-execution. Every placement query gates on `r.draining = false`, so the next scheduling pass simply stops considering the runner; nothing is revoked, and work already leased runs to completion or expires on the ordinary lease clock.

`set_runner_draining()` is narrower than `set_runner_state()` in the columns it touches: a runner reports one fact about itself, so `enabled` and `revoked_at` — the operator's decisions — are untouched, and a report against a revoked runner matches no row and is rejected, which ends that runner's stream (the same thing its next message would do, since revoked credentials no longer authenticate).

It is **not** narrower in `draining` itself. That column has two writers and one bit, so `RunnerDraining{draining: false}` clears an operator drain and `set_runner_state("enable")` clears a runner's. Giving each owner its own bit needs a second column and a change to the three placement predicates; [#119](https://github.com/alexandre-leites/moirai/issues/119), which owns the operator drain/revoke API, has to decide how the two reconcile. Nothing depends on it today: `set_runner_state` has no callers.

Since the runner now reports its state on connect, `draining: false` is no longer rare — a runner that is not draining writes it on every reconnect, and the stream reconnects on any transport blip. That does not conflict with anything today, but it constrains [#119](https://github.com/alexandre-leites/moirai/issues/119): an operator drain applied by writing the column alone would be cleared by that runner's next reconnect, within seconds. An operator drain that also sends `OrchestratorToRunner.drain` stays consistent, because the runner mirrors it into its own state and re-asserts it on every stream. Separate columns remain the clean answer.

### Reporting the drain state on connect

`ControlLoop.Resume()` reports `Draining()` on every stream the runner establishes, which is what keeps the orchestrator's copy of the flag from outliving the runner that set it. The runner is the only writer of `draining` in practice — there is no operator API yet ([#119](https://github.com/alexandre-leites/moirai/issues/119)) — so a runner that reported `true`, exited, and came back under the same identity would otherwise reconnect into a permanent `true` and never be offered work again; clearing it took a manual `UPDATE` ([#148](https://github.com/alexandre-leites/moirai/issues/148)).

Two properties are load-bearing:

- It reports `Draining()`, never a bare `false`. A runner that reconnects while genuinely draining — a network blip mid-drain, or an orchestrator-initiated drain whose report never left the dying stream — re-asserts the drain instead of advertising itself as available.
- The read of the state and the send are one critical section (`ControlLoop.drainReports`), so a drain landing during a reconnect cannot be overtaken by the reconnect's already-sampled `false`. This orders delivery, not success: a `Drain()` whose report dies with the stream leaves the orchestrator briefly believing the runner is available — it keeps offering, the runner keeps rejecting with `"runner is draining"` — until the next connect re-asserts it.

The report precedes the buffered-event flush that `Resume()` also performs, so the orchestrator stops placing work before the runner spends the fresh stream on a backlog. A failed report fails the resume rather than running a whole connection on a stale view; `StreamSupervisor` then drops the stream and retries if the failure is transient, and stops the runner if it is not — the same treatment the flush already got.

This also removes the wedge that made the runner's shutdown ordering dangerous to touch. `StreamSupervisor` still disconnects on `ctx.Done()` before `main` reaches `Drain()`, so the SIGTERM drain report is still dropped with `ErrNotConnected` — that ordering sits inside the shutdown region [#102](https://github.com/alexandre-leites/moirai/issues/102) is hardening — but a report that *does* get delivered is now cleared by the runner's next connect instead of stranding it for good.

One more thing worth knowing: the orchestrator never sends `OrchestratorToRunner.drain` yet, but the runner end is already wired (`control_loop.go` calls `Drain()` on receipt). That is the one path that delivers a `draining: true` over a live stream today.
