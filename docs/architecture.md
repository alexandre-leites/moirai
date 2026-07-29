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

Repeated failure is bounded rather than infinite: once a job accumulates `unanswered_offer_limit` consecutive unanswered offers (default 5) that have been failing for longer than `unanswered_offer_grace` (default 15 minutes), the run is blocked with `blocking_reason = 'unanswered_offer_limit'`, its outstanding execution requests are expired, and the project lock is released. Accepting an offer resets the streak. Both bounds are constructor arguments of `AsyncpgControlPlane`.

The scheduler applies the same rule in memory (`scheduler.py`): a task-packet build error is an orchestrator-side fault, so the candidate is skipped and its offer is left to expire on its own TTL; an undelivered offer is released through the control plane. Neither stops the tick, which keeps placing offers for other runners until the per-tick budget or `max_consecutive_failures` (default 3) is reached.
