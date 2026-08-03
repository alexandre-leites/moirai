# Implementation Progress

Sections below are appended by individual agent sessions working from separate
worktrees on separate issues. Each section is self-contained; do not
restructure this file or edit another session's section — append a new one.

## Session: issue-360 (web console missing four workflow statuses)

- Completed: 2026-08-03
- Agent/session identifier: issue-360 worktree agent, session_013bUD8GkWQmVEZZAyf8sAUg
- Relevant files:
  - `web/src/status.ts`
  - `web/src/status.test.ts`
- Behavior delivered:
  - Added `delivering`, `waiting_ai_review`, `repairing` and `pipeline_failed`
    to `STATUS_META` with labels/pill variants/pulse/phase mirroring
    `orchestrator/internal/server/status.go:16-137`'s documented semantics:
    - `delivering`: run (pulse), phase `pr` — agent succeeded, PR is being opened.
    - `waiting_ai_review`: run (pulse), phase `review` — independent reviewer executing.
    - `repairing`: warn (pulse), phase `implement` — bounded repair attempt in flight.
    - `pipeline_failed`: warn (no pulse), phase `pipeline` — deterministic
      pipeline command failed; a brief hand-off to the repair-or-block decision.
  - Deliberate divergence from a literal mirror of status.go, called out in
    code comments: status.go's own `terminalStatuses` (used by its Prometheus
    active-workflow gauge) excludes `delivering`, `waiting_ai_review`,
    `repairing` **and** `pipeline_failed` — all four hold the project lock and
    are active work by the orchestrator's own accounting. The console's
    `TERMINAL_STATUSES` answers a different, display-only question (should the
    sidebar/workflow-list count this as something a human needs to keep
    watching), so only `pipeline_failed` was added to it: it is a bounded,
    deterministic hand-off (status.go's own words) that resolves to
    `repairing` or `blocked` before an operator can act on it as "in flight."
    `delivering`/`waiting_ai_review`/`repairing` were intentionally **not**
    added to `TERMINAL_STATUSES` — they are ordinary in-progress work, exactly
    like `preparing`/`planning`.
  - Also added `pipeline_failed` to `CUT_STATUSES` (not explicitly named in
    the issue, but required for correctness): without it, `overview.tsx`'s
    `feedTone` and the phase thread would treat a `pipeline_failed` run as a
    plain finished run once it fell into `TERMINAL_STATUSES` and paint its
    event-feed entry and thread green instead of showing the failure. Verified
    by reasoning through `feedTone`/`deriveGates`/thread cut logic in
    `overview.tsx`, `ui/thread.tsx` and `status.ts`.
  - Updated the doc comments above `STATUS_META`, `TERMINAL_STATUSES`,
    `CUT_STATUSES` and `reachedPhase` to describe all thirteen statuses the Go
    orchestrator now writes and correct stale claims (the old comment said the
    orchestrator "never increments the attempt counters," which was already
    false for planningAttempts/reviewCycles/ciRepairAttempts/
    pipelineRepairAttempts — verified against `orchestrator/internal/server/
    {server,review,repair}.go`).
- Validation performed: targeted `status.test.ts` run, then full frontend
  suite, typecheck and lint.
- Commands executed (from `web/`, `npm_config_cache` namespaced to
  `/tmp/moirai-npm-cache-issue-360` to avoid clashing with sibling agents):
  - `npm ci`
  - `npx vitest run src/status.test.ts` → 31 passed
  - `npm test` (`vitest run`) → 248 passed, 17 files
  - `npm run typecheck` (`tsc --noEmit`) → clean, no errors
  - `npm run lint` (`eslint .`) → 0 errors, 16 pre-existing warnings
    (react-refresh/only-export-components in unrelated files, not touched by
    this change)
- Notes: Only `web/src/status.ts` and `web/src/status.test.ts` were touched,
  per this session's file ownership (`web/src/status.ts`). No Go, proto or CI
  files were changed.
