# Architecture

Moirai separates public HTTP, control-plane state, and execution environments.

```text
Browser -> web nginx -> API -> orchestrator gRPC -> PostgreSQL
                                  ^
                                  |
                              runner gRPC
```

The API does not access PostgreSQL directly. The orchestrator owns migrations, authentication/session validation, project configuration, scheduling, workflow state, GitHub integration, and runner leases. Runners receive offers over an outbound gRPC stream and maintain local identity and event delivery state. The web service is a static UI with nginx proxying `/api/` to the API service in Compose.

Shared gRPC definitions live in `proto/`; generated Go code is in `gen/go/`. The HTTP surface is separately described by [`api/openapi.yaml`](../api/openapi.yaml).

## Job offer lifecycle

A V1 workflow run receives exactly one offer. The scheduler picks the highest-priority eligible issue whose project holds no lock, creates the workflow run, the job and the offer in one transaction, and enqueues the offer on the runner's control stream. `app.jobs.workflow_run_id` is unique, so a run that has had its execution cannot be given a second one — reopening work means reopening the issue and letting the scheduler create a fresh run, which is what `RetryWorkflow` does.

| Outcome | Result |
| --- | --- |
| Runner never receives the offer (stream gone between commit and send) | Job, offer and run are cancelled and the project lock released, in one transaction on a context that shutdown cannot cancel. The issue stays eligible: no execution ran, so nothing was spent and the work is offered again. |
| Runner rejects the offer | Job and run are cancelled and the project lock released. |
| Runner accepts | The job moves to `preparing` under a lease. Acceptance is guarded on the job still being `offered`, so an acceptance that arrives after an operator cancelled the work is refused rather than reviving it with a fresh lease. |
| Runner accepts and stops renewing | The recovery sweep reclaims the lease, fails the run, releases the lock and parks the issue. The runner cannot do this itself: its own write paths are all fenced on an unexpired lease. |

Cancelling or blocking a run also withdraws any offer still outstanding, because offers do not age out on their own.

Repeated failure is bounded by not retrying at all. A run that fails, or whose delivery fails, marks its issue ineligible; only an explicit retry reopens it. This is the V1 replacement for the previous engine's attempt counters and circuit breakers, none of which the Go orchestrator implements.

The runner has a reclamation of its own, on a different clock. Accepting an offer reserves a capacity slot before any lease exists, so `control.OfferState` stamps the reservation and releases it once `LOOP_RUNNER_OFFER_TIMEOUT` (default 30s) has passed without a `LeaseAcknowledged`, on the next expiry sweep (`LOOP_RUNNER_HEARTBEAT_INTERVAL`, default 10s). Release is purely local. The orchestrator reclaims the job separately, through the recovery sweep against `app.jobs.lease_expires_at`. The two clocks are deliberately unequal: the runner frees its slot in seconds so it can keep taking work, and only later does the orchestrator conclude the job is lost. In between, an acknowledgement that arrives after the reservation expired is discarded rather than starting an execution whose slot the runner may already have given away.

`ControlLoop.Busy()` counts reservations as well as active leases, so the runner cannot report availability it would refuse: it is the same predicate `OfferState.Admit` uses. Note that the orchestrator does not read the `Heartbeat.busy` field, and does not read `runners.capacity` either — placement allows one in-flight job per runner — so this is an internal consistency guarantee rather than a scheduling signal.

## Runner drain

A runner reports its own drain state on the control stream (`RunnerToOrchestrator.runner_draining`), both when it starts draining (`ControlLoop.Drain()`) and on every control stream it establishes (`ControlLoop.Resume()`, wired as `StreamSupervisor.OnConnected`). The orchestrator writes it to `app.runners.draining` and keeps the stream open: a draining runner still has to renew leases and report events for the work it already holds, so treating the report as a protocol violation — which is what an unhandled message type does — would abort the stream mid-execution. Every placement query gates on `r.draining = false`, so the next scheduling pass simply stops considering the runner; nothing is revoked, and work already leased runs to completion or expires on the ordinary lease clock.

The runner's own drain report is narrower than the operator's `SetRunnerState` in the columns it touches: a runner reports one fact about itself, so `enabled` and `revoked_at` — the operator's decisions — are untouched, and a report against a revoked runner matches no row and is rejected, which ends that runner's stream (the same thing its next message would do, since revoked credentials no longer authenticate).

It is **not** narrower in `draining` itself. That column has two writers and one bit, so `RunnerDraining{draining: false}` clears an operator drain and `SetRunnerState("enable")` clears a runner's. Giving each owner its own bit needs a second column and a change to the three placement predicates; [#119](https://github.com/alexandre-leites/moirai/issues/119), which owns the operator drain/revoke API, has to decide how the two reconcile. `SetRunnerState` is a live RPC, so the two writers do now coexist; an operator drain applied by writing the column alone is cleared by that runner's next reconnect.

Since the runner now reports its state on connect, `draining: false` is no longer rare — a runner that is not draining writes it on every reconnect, and the stream reconnects on any transport blip. That does not conflict with anything today, but it constrains [#119](https://github.com/alexandre-leites/moirai/issues/119): an operator drain applied by writing the column alone would be cleared by that runner's next reconnect, within seconds. An operator drain that also sends `OrchestratorToRunner.drain` stays consistent, because the runner mirrors it into its own state and re-asserts it on every stream. Separate columns remain the clean answer.

### Reporting the drain state on connect

`ControlLoop.Resume()` reports `Draining()` on every stream the runner establishes, which is what keeps the orchestrator's copy of the flag from outliving the runner that set it. The runner is not the only writer of `draining` any more ([#119](https://github.com/alexandre-leites/moirai/issues/119) added `SetRunnerState`), but it is the one that re-asserts on connect — so a runner that reported `true`, exited, and came back under the same identity would otherwise reconnect into a permanent `true` and never be offered work again; clearing it took a manual `UPDATE` ([#148](https://github.com/alexandre-leites/moirai/issues/148)).

Two properties are load-bearing:

- It reports `Draining()`, never a bare `false`. A runner that reconnects while genuinely draining — a network blip mid-drain, or an orchestrator-initiated drain whose report never left the dying stream — re-asserts the drain instead of advertising itself as available.
- The read of the state and the send are one critical section (`ControlLoop.drainReports`), so a drain landing during a reconnect cannot be overtaken by the reconnect's already-sampled `false`. This orders delivery, not success: a `Drain()` whose report dies with the stream leaves the orchestrator briefly believing the runner is available — it keeps offering, the runner keeps rejecting with `"runner is draining"` — until the next connect re-asserts it.

The report precedes the buffered-event flush that `Resume()` also performs, so the orchestrator stops placing work before the runner spends the fresh stream on a backlog. A failed report fails the resume rather than running a whole connection on a stale view; `StreamSupervisor` then drops the stream and retries if the failure is transient, and stops the runner if it is not — the same treatment the flush already got.

This also removes the wedge that made the runner's shutdown ordering dangerous to touch. `StreamSupervisor` still disconnects on `ctx.Done()` before `main` reaches `Drain()`, so the SIGTERM drain report is still dropped with `ErrNotConnected` — that ordering sits inside the shutdown region [#102](https://github.com/alexandre-leites/moirai/issues/102) is hardening — but a report that *does* get delivered is now cleared by the runner's next connect instead of stranding it for good.

One more thing worth knowing: the orchestrator never sends `OrchestratorToRunner.drain` yet, but the runner end is already wired (`control_loop.go` calls `Drain()` on receipt). That is the one path that delivers a `draining: true` over a live stream today.
