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

A runner reports its own drain state on the control stream (`RunnerToOrchestrator.runner_draining`, sent by `ControlLoop.Drain()`). The orchestrator writes it to `app.runners.draining` through `set_runner_draining()` and keeps the stream open: a draining runner still has to renew leases and report events for the work it already holds, so treating the report as a protocol violation — which is what an unhandled message type does — would abort the stream mid-execution. Every placement query gates on `r.draining = false`, so the next scheduling pass simply stops considering the runner; nothing is revoked, and work already leased runs to completion or expires on the ordinary lease clock.

`set_runner_draining()` is narrower than `set_runner_state()` in the columns it touches: a runner reports one fact about itself, so `enabled` and `revoked_at` — the operator's decisions — are untouched, and a report against a revoked runner matches no row and is rejected, which ends that runner's stream (the same thing its next message would do, since revoked credentials no longer authenticate).

It is **not** narrower in `draining` itself. That column has two writers and one bit, so `RunnerDraining{draining: false}` clears an operator drain and `set_runner_state("enable")` clears a runner's. Giving each owner its own bit needs a second column and a change to the three placement predicates; [#119](https://github.com/alexandre-leites/moirai/issues/119), which owns the operator drain/revoke API, has to decide how the two reconcile. Nothing depends on it today: `set_runner_state` has no callers.

Two consequences worth knowing before building on this:

- `ControlLoop.Drain()` only ever reports `draining: true`; nothing in the runner reports `false`. A runner that reports draining and later restarts therefore comes back with the flag still set, and with no operator API yet there is no supported way to clear it. This does not fire today only because the shutdown path never delivers the report — `StreamSupervisor` disconnects the client on `ctx.Done()` before `main` reaches `Drain()`, so `SetDraining` returns `ErrNotConnected`. **Fixing that ordering without also having the runner report `draining: false` on a fresh stream would strand every runner after its first graceful shutdown.**
- The orchestrator never sends `OrchestratorToRunner.drain` yet, but the runner end is already wired (`control_loop.go` calls `Drain()` on receipt). That is the one path that delivers a drain report over a live stream today, so it is where both consequences above first become reachable.
