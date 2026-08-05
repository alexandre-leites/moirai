# Platform review — logical bugs, competitor gaps, autonomy roadmap

> **Historical.** This review covers the Python/LangGraph orchestrator, which
> was replaced by the pure-Go state machine in #247. It is kept for the
> record of findings and the roadmap it produced; do not use it as a
> description of the current codebase.

Date: 2026-07-29
Scope: `orchestrator/`, `runner/`, workflow engine, scheduling/persistence, recovery guarantees.
Method: full manual read of the workflow core (`orchestrator/src/moirai/workflows/*`, `scheduler.py`) plus two focused deep reviews of the Go runner and the orchestrator persistence/services layers. Findings marked *(verify)* were produced by the deep reviews and should be re-confirmed with a failing test before large fixes are built on them.

## Executive summary

The architecture is sound: lease fencing, transactional outbox, per-project locks, and circuit breakers are the right skeleton. But three systemic problems undermine the product goal of durable autonomous delivery:

1. **The workflow graph is invoked as a run-to-completion function, not an event-driven state machine.** A single `ainvoke` walks all downstream nodes against state the runner has not produced yet, dispatching phantom executions and typically ending in `blocked: workflow retry budget exhausted` (F1).
2. **"Success" is not evidence-based.** The runner equates process-exit-0 with completion (F2), the orchestrator equates developer-exit-0 with "pipeline passed" (F3), and nothing anywhere checks that a diff exists or that `remainingWork` is empty. An agent whose reasoning loop stops early is recorded as a *successful* execution — the exact opposite of the stated principle "an agent cannot declare success by itself".
3. **Retries carry no knowledge.** The task-packet protocol has fields for plan, prior failures, diff context, failed checks, and review findings — none is populated by any caller. Retries re-send the first attempt's prompt plus a sha256 digest. Autonomy today is "same prompt, count to 3, block."

The remediation is tracked as GitHub issues (see the task breakdown at the end). Recommended order: F1 → F2/F3 → autonomy layers 1+3 → durability wedges (F5, F4-offer, F6-events) → the rest.

## Findings

Severity: **P1** = breaks the core loop or loses work; **P2** = degrades durability/recovery; **P3** = hardening.

### F1 (P1) — Workflow graph runs to END in one invocation
`workflows/runtime.py:39-51`, `workflows/issue_graph.py:155-166`, `workflows/nodes.py:184-207`.
Nothing suspends the graph after a node dispatches an execution (`interrupt_before` covers only `wait_for_human`). One `ainvoke` triggered by a planner success walks `implement → pipeline → repair → pipeline → …` against unset gate values until a budget blocks the run, dispatching a fresh `workflow_execution_requests` row at each pass. `test_end_to_end.py` masks this by pre-seeding all gate booleans.

### F2 (P1) — Runner reports success when the result document is missing/invalid
`runner/internal/agents/opencode.go:130-154`.
When `.loop/result.json` is absent or fails validation, the error is discarded and status is derived from exit code alone: exit 0 → `completed` with empty summary, no changed files, no result payload. The CLI and Docker backends propagate the document error; only the default opencode backend swallows it. `remainingWork` and `sessionId` are captured in the schema but used for nothing.

### F3 (P1) — `pipeline_passed` inferred from developer exit code; local pipeline skippable
`workflows/runner_events.py:241-249`, `workflows/nodes.py:82-85`.
A developer terminal event sets `pipeline_passed = (exit_code == 0)`, and the `pipeline` node skips dispatching a real pipeline execution when `pipeline_passed is True`. Combined with F2, a do-nothing agent advances with "pipeline passed".

### F4 (P1) — Offer expiry cancels in-flight workflows *(verify)*
`persistence/control_plane.py:1165-1215`, `scheduler.py:100-115`.
`expire_offers` sets the workflow to terminal `cancelled` for any unanswered offer, with no distinction between the bootstrap offer and a mid-workflow re-offer; the project lock is dropped and the dispatched request row leaks. The scheduler's exception arm reaches the same path, so a transient packet-build error is terminal. One runner disconnect during review kills a run with a valid PR in flight.

### F5 (P1) — Circuit-breaker wedges *(verify)*
`persistence/control_plane.py:894-935` (partial half-open claim committed on failure → project permanently unschedulable with a probe pointing at a never-created workflow); `workflows/persistence.py:109-134` (probes resolved only by `completed`/`blocked`; a `cancelled` or `failed` probe — e.g. the offer-expiry path — wedges `half_open` forever; no orphan reaper). Also: `clear_provider_failure` leaves stale probe pointers that can reopen the provider circuit later, and any project's sync success clears a circuit another project's failure just opened.

### F6 (P1) — Terminal runner events can be silently lost *(verify)*
`runner/internal/control/events.go:86-98` (lease expiry purges the job's queued events from memory *and* the crash-safe outbox — exactly when the stream is down); `events.go:131-134` + `dispatch/control_loop.go:360-379` (log and terminal events share one 128-slot buffer; a chatty agent during a disconnect makes the terminal emit fail, and every terminal `Emit` error is discarded with `_, _ =`).

### F7 (P2) — Execution requests never closed → stalled-run recovery is dead code *(verify)*
`persistence/control_plane.py:1882-1904`, `:1118-1128`.
No code path marks a `workflow_execution_requests` row finished when its terminal event lands, but `find_stalled_workflow_runs` requires the absence of `queued`/`dispatched` rows. Any run that ever dispatched an execution permanently fails the predicate; the maintenance recovery arm never fires.

### F8 (P2) — Pending offer reservations leak runner capacity *(verify)*
`runner/internal/control/offer.go:88-106`.
Nothing ages out un-acked reservations (`Config.OfferTimeout` is validated but wired nowhere). A lost ack leaves the runner rejecting all future offers with "runner is busy" while heartbeats still advertise it as idle (`Busy()` counts only active leases).

### F9 (P2) — Transition replay double-counts budgets and duplicates requests
`workflows/nodes.py:196-206`, `persistence/control_plane.py:1827-1880`.
The transition outbox is at-least-once, but `_dispatch` increments attempt counters and `total_agent_executions` even when reusing a queued request; if the request already moved to `dispatched`, a duplicate request for the same role is created. Also, outbox rows set to `processing` are never retried after a crash.

### F10 (P2) — `blocked` agent results never reach the orchestrator *(verify)*
`runner/internal/dispatch/control_loop.go:357-424`.
Any result status ≠ `completed` is flattened to `failed`, and the raw result document is attached only for `completed` — so the orchestrator's planner-`blocked` handling (`runner_events.py:223-231`) is unreachable from an agent-reported block, and `summary`/`remainingWork` never cross the wire. The runner also still commits and pushes the branch for a `blocked` result.

### F11 (P2) — `ci_repair_attempts` is a dead counter
`workflows/policy.py:82`, `workflows/nodes.py:92-95`.
`route_after_checks` gates CI repairs on `ci_repair_attempts`, but no node increments it — `repair` only increments `pipeline_repair_attempts`. CI repairs silently consume the local-pipeline budget; the CI gate can never trip on its own.

### F12 (P2) — Label reconciliation deletes user labels; nondeterministic workflow selection *(verify)*
`services/issue_sync.py:158-181`, `persistence/control_plane.py:265-284`.
Desired labels (agent namespace only) are diffed against the issue's *entire* label set, so `bug`, `agent-priority:N`, etc. are removed — which also resets priority on the next sync. `list_active_workflows_for_project` has no status filter and orders by UUID, so which historical run's terminal label wins is nondeterministic.

### F13 (P2) — Failed work is discarded
`runner/internal/dispatch/dispatch.go:213-240`, retention default (`config.go:377-380`).
A failing pipeline means the (possibly nearly-correct) diff is never committed, and the default retention deletes the worktree, terminal result, and agent logs. Retries start from a clean base branch with a sha256 digest as their only context.

### F14 (P3) — Non-progress fingerprinting is ineffective *(verify)*
`persistence/control_plane.py:1520-1529`, `:1683-1730`.
The diff hash covers only the changed-file list: every zero-diff success collides on `sha256("[]")` across phases, while genuinely identical failures rarely match because volatile payload fields (durations, timestamps) land in the fingerprint. The comparison also mixes diff hashes with failure fingerprints, and the README's "four identical outcomes" is five in code (and normally preempted by the retry budget).

### F15 (P3) — Runner lifecycle hardening *(verify)*
Grouped: `control/client.go:107-122` (each reconnect leaks a gRPC stream — no cancel/CloseSend); `cmd/runner/main.go:255-292` (SIGTERM waits unbounded — `WaitForIdle` has no deadline, `TerminationGrace` unwired, execution contexts detached from `runCtx`); `agents/recovery.go` (crash recovery deletes execution manifests without killing orphaned agent PIDs, which survive via `Setpgid` and can mutate the workspace while `Prepare` runs `RemoveAll` under them; no terminal event for the interrupted execution); `control/offer.go:142-171` (`RenewDue` silently deletes expired leases, bypassing cancellation/reporting; dropped renewals never retried); `persistence/control_plane.py:1249-1252` (one expired lease marks the whole runner `offline`); `execution/local.go:139-149` (`TMPDIR` points at a never-created directory); `agents/opencode.go:122` (whole prompt as one argv element can exceed Linux's 128 KiB arg limit); `control_loop.go:142-148` + `events.go:40-67` (one malformed ack or a corrupt outbox file crash-loops the whole runner); `events.go:177-192` (ANSI-heavy log chunks exceed the JSON payload limit and are silently dropped).

### F16 (P3) — Bootstrap `NameError` on partial prior bootstrap
`main.py:96-130`. `uuid4` is imported inside a conditional but used unconditionally; startup fails when the seed project exists but no users do.

## What competitors do better

- **Session continuity** (Claude Code `--resume`, OpenHands condensation, Devin): Moirai deliberately discards agent conversation state but does not compensate with rich packets. opencode's own session id is captured and unused.
- **Stuck detection during a run** (OpenHands `StuckDetector`): Moirai's only equivalent is the post-hoc diff-hash counter, which is broken (F14) and preempted by budgets.
- **Feedback into the same attempt** (Copilot coding agent iterating on CI failures and review comments; Aider's edit→test→fix inner loop): Moirai has the outer repair loop but feeds it no content — review findings and pipeline output are persisted and never read back.
- **Verification-before-done** (Claude Code's goal loop / stop-hook pattern): nothing in Moirai asks "was the objective met?" before advancing; the gates exist but their evidence chain is broken (F2/F3).
- **Escalation ladders** (retry → new strategy → ask a human *a question*): Moirai's `human_required` is a terminal `blocked` with no notification or resume-with-answer path.

## Autonomy roadmap — continue when the agent's reasoning loop stops

Goal: when the agent stops without the goal being met, the *system* notices and continues (Claude-Code-style goal loop), while preserving the principle that deterministic gates decide completion.

- **L1 — Runner-side goal gate + continuation loop.** After the agent exits: result doc valid, `status=="completed"`, `remainingWork` empty, and (for mutating roles) non-empty `git diff`. If the gate fails and a continuation budget remains, re-invoke the agent *resuming the captured `sessionId`* with a continuation prompt naming the missing evidence. No orchestrator changes needed.
- **L2 — Non-delivery as a first-class workflow outcome.** In `runner_events.py`, distinguish *delivered* / *returned-without-evidence* (stay in phase, continuation attempt on a separate counter) / *agent-reported-blocked* (requires F10). Depends on F1.
- **L3 — Populate the task-packet context fields.** `plan` (persist planner result), `reviewFindings` (`app.ai_reviews`), `failedChecks` + pipeline output tail (`app.pipeline_runs`), `diffSummary`/`currentCommit` (retained branch), human-readable `previousFailures`. Pure read-side wiring; the protocol fields already exist and are validated.
- **L4 — Escalation ladder.** continuation → fresh attempt with enriched packet → different agent backend/model → `waiting_human` with a concrete question + notification. `human_required` parks, never kills.
- **L5 — Goal-verifier execution.** A read-only verifier role before the pipeline phase answers "is the objective plausibly met; what remains?" — advisory input to L2's continuation prompt, not a gate, so agents still don't declare success.

## Task breakdown

Each task is filed as a GitHub issue (numbers filled in below). Priority = suggested order of attack within its track.

| # | Task | Finding | Area | Priority | Issue |
|---|------|---------|------|----------|-------|
| 1 | Make the workflow graph event-driven | F1 | orchestrator | P1 | [#88](https://github.com/alexandre-leites/moirai/issues/88) |
| 2 | Runner: missing/invalid result doc must not be success | F2 | runner | P1 | [#89](https://github.com/alexandre-leites/moirai/issues/89) |
| 3 | Always run the local pipeline; stop inferring `pipeline_passed` | F3 | orchestrator | P1 | [#90](https://github.com/alexandre-leites/moirai/issues/90) |
| 4 | Offer expiry must not cancel in-flight workflows | F4 | orchestrator | P1 | [#91](https://github.com/alexandre-leites/moirai/issues/91) |
| 5 | Fix circuit-breaker wedges | F5 | orchestrator | P1 | [#92](https://github.com/alexandre-leites/moirai/issues/92) |
| 6 | Runner: stop losing terminal events | F6 | runner | P1 | [#93](https://github.com/alexandre-leites/moirai/issues/93) |
| 7 | Close execution requests so stalled-run recovery fires | F7 | orchestrator | P2 | [#94](https://github.com/alexandre-leites/moirai/issues/94) |
| 8 | Runner: expire pending offer reservations (wire OfferTimeout) | F8 | runner | P2 | [#95](https://github.com/alexandre-leites/moirai/issues/95) |
| 9 | Make transition replay idempotent (budgets, duplicate requests) | F9 | orchestrator | P2 | [#96](https://github.com/alexandre-leites/moirai/issues/96) |
| 10 | Runner: forward blocked results, summary, remainingWork | F10 | runner | P2 | [#97](https://github.com/alexandre-leites/moirai/issues/97) |
| 11 | Increment `ci_repair_attempts` where CI repairs are dispatched | F11 | orchestrator | P2 | [#98](https://github.com/alexandre-leites/moirai/issues/98) |
| 12 | Label reconciliation: manage only the agent namespace | F12 | orchestrator | P2 | [#99](https://github.com/alexandre-leites/moirai/issues/99) |
| 13 | Retain and commit failed work for retries | F13 | runner | P2 | [#100](https://github.com/alexandre-leites/moirai/issues/100) |
| 14 | Fix non-progress fingerprinting | F14 | orchestrator | P3 | [#101](https://github.com/alexandre-leites/moirai/issues/101) |
| 15 | Runner lifecycle hardening (streams, shutdown, orphans, env) | F15 | runner | P3 | [#102](https://github.com/alexandre-leites/moirai/issues/102) |
| 16 | Fix bootstrap `NameError` | F16 | orchestrator | P3 | [#103](https://github.com/alexandre-leites/moirai/issues/103) |
| 17 | Autonomy L1: runner goal gate + session-resume continuation | — | runner | P1 | [#104](https://github.com/alexandre-leites/moirai/issues/104) |
| 18 | Autonomy L2: non-delivery as first-class outcome | — | orchestrator | P2 | [#105](https://github.com/alexandre-leites/moirai/issues/105) |
| 19 | Autonomy L3: populate task-packet context fields | — | orchestrator | P1 | [#106](https://github.com/alexandre-leites/moirai/issues/106) |
| 20 | Autonomy L4: escalation ladder / park on waiting_human | — | orchestrator | P3 | [#107](https://github.com/alexandre-leites/moirai/issues/107) |
| 21 | Autonomy L5: goal-verifier execution role | — | both | P3 | [#108](https://github.com/alexandre-leites/moirai/issues/108) |

Dependency notes: #105 depends on #88 (and benefits from #97); #104 and #106 are independent and high-leverage; #108 depends on #105. #104 depends on #89; #107 depends on #88/#97/#105/#106.

Recommended order of attack: #88 → #89/#90 → #104 + #106 → #92/#91/#93 → the P2 track → P3 hardening → #107/#108.
