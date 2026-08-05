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

## Session: issue-362 (SSE workflow events blank out row data)

- Completed: 2026-08-05
- Agent/session identifier: issue-362 worktree agent, session_01RJrCZXw86ZYEfQdzsMcNE2
- Relevant files:
  - `web/src/events.ts`
  - `web/src/console-data.tsx`
  - `web/src/console-data.test.tsx` (new)
- Root cause: `api/internal/http/handlers/events.go`'s `writeSSEEvent` sends
  `workflowPayload(workflow)` for every workflow SSE event — the same
  4-field lifecycle shape (`id`/`projectId`/`status`/`phase`) the control
  endpoints (retry/cancel/block/decision) answer with, documented on
  `workflowPayload` itself (`workflows.go:196-207`) and already given its own
  type, `WorkflowLifecycle`, in `web/src/api.ts:177`. `web/src/events.ts`
  ignored that existing type and declared `DashboardEvent.workflow` as the
  full `Workflow`, and `console-data.tsx`'s `replaceByID` swapped the whole
  row for whatever arrived — so every SSE workflow event silently wiped
  `issueTitle`, `pullRequestUrl`, `blockingReason`, every attempt counter,
  `planSummary`, `createdAt` and `updatedAt` down to `""`/`0` until the next
  10s poll repaired it.
  - Considered sending `workflowDetailPayload` (the full row) over SSE
    instead. Rejected: the orchestrator's `workflowsByIDs`
    (`orchestrator/internal/server/events.go:218-240`), which backs the
    event stream, never selects `plan_summary` from `app.workflow_runs` — so
    switching the API handler to `workflowDetailPayload` would still wipe
    `planSummary` on every SSE event, just narrowing the bug to one field
    instead of fixing it, and would require an orchestrator query change
    plus a proto/API contract change outside this issue's file ownership.
    Patching the existing row client-side needs no server or contract
    change and matches the type the codebase had already modeled for this
    exact shape.
- Behavior delivered:
  - `web/src/events.ts`: `DashboardEvent.workflow` is now typed
    `WorkflowLifecycle` (imported from `./api`) instead of `Workflow`, with a
    comment pointing at `workflowPayload` in
    `api/internal/http/handlers/events.go` as the reason, and at
    `console-data.tsx`'s merge as the fix.
  - `web/src/console-data.tsx`: added `mergeByID`, which patches the
    matching row's fields (`{ ...item, ...patch }`) instead of replacing it,
    and inserts the patch as-is when no row matches (nothing to wipe for a
    workflow the snapshot hasn't loaded yet — the next poll fills it in,
    same as before). The workflow branch of the SSE handler in
    `ConsoleDataProvider` now calls `mergeByID` instead of `replaceByID`.
    `replaceByID` itself is untouched and still used for runner events,
    whose SSE payload (`runnerPayload` in `api/internal/http/handlers/
    runners.go`) is already the complete row.
  - `web/src/console-data.test.tsx` (new): two tests using a fake
    `EventSource` (`vi.stubGlobal`) to drive `ConsoleDataProvider` directly —
    one proves a lifecycle-only SSE event patches status/phase on an
    existing row while leaving `issueTitle`/`pullRequestUrl`/
    `planningAttempts` untouched (the exact regression the issue reported);
    the other proves a lifecycle event for a workflow not yet in the
    snapshot still inserts a (necessarily partial) row rather than being
    dropped.
- Validation performed: targeted new test file, then full frontend suite,
  typecheck and lint, all from `web/`.
- Commands executed (from `web/`):
  - `npm ci` → clean install
  - `npx vitest run src/console-data.test.tsx` → 2 passed
  - `npm run typecheck` (`tsc --noEmit`) → clean, no errors
  - `npm run lint` (`eslint .`) → 0 errors, 16 pre-existing warnings
    (react-refresh/only-export-components in files this change did not
    touch)
  - `npm test` (`vitest run`) → 250 passed, 18 files (was 248/17 before this
    change)
- Notes: No Go, proto or CI files were changed — the fix is entirely
  client-side, patching the row instead of replacing it, so no orchestrator
  or API contract change was needed. `api/internal/http/handlers/events.go`
  was read but left as-is (see root cause above for why sending the full
  detail payload was rejected).

## Session: issue-374 (add open-source project files)

- Completed: 2026-08-05
- Agent/session identifier: issue-374 worktree agent, session_01RJrCZXw86ZYEfQdzsMcNE2
- Relevant files (all new):
  - `LICENSE`
  - `CONTRIBUTING.md`
  - `SECURITY.md`
  - `CODE_OF_CONDUCT.md`
  - `CODEOWNERS`
  - `.github/ISSUE_TEMPLATE/bug_report.yml`
  - `.github/ISSUE_TEMPLATE/feature_request.yml`
  - `.github/ISSUE_TEMPLATE/question.yml`
  - `.github/ISSUE_TEMPLATE/config.yml`
  - `.github/PULL_REQUEST_TEMPLATE.md`
- Behavior delivered:
  - `LICENSE`: MIT, copyright Alexandre Leites (the GitHub org/repo owner —
    `gh repo view` reports `licenseInfo: null` and `owner.login:
    alexandre-leites`, and `git log` shows Alexandre Leites as the sole
    non-agent, non-dependabot human author). MIT chosen because neither
    `README.md` nor `PROJECT.md` states a license preference and the project
    ships permissively-licensed Docker images intended for open redistribution
    (`ghcr.io/alexandre-leites/moirai/*`); MIT is the least restrictive common
    choice and imposes no copyleft obligation on downstream users of the
    published images.
  - `CONTRIBUTING.md`: dev setup, the actual `make` targets from `Makefile`
    (`test`, `test-orchestrator`, `test-postgres-integration`, `test-runner`,
    `test-api`, `test-web`, `lint`, `lint-go`, `typecheck`, `validate`,
    `sqlc-generate`/`sqlc-check`, `proto-generate`/`proto-check`), the sqlc
    and proto workflows from `AGENTS.md` §12, and a PR process matching what
    `.github/workflows/ci.yml` actually gates (no invented steps).
  - `SECURITY.md`: private disclosure via GitHub Security Advisories (no
    fabricated email address), scope notes specific to this project's threat
    model (GitHub tokens, per-project credentials, agent provider keys,
    runner lease fencing, GitHub CLI adapter injection), and a
    pre-1.0/best-effort supported-versions statement consistent with
    `docs/release.md`'s tagging scheme referenced from `README.md`.
  - `.github/ISSUE_TEMPLATE/{bug_report,feature_request,question}.yml`: GitHub
    issue-forms (current YAML convention, not legacy Markdown), each scoped
    to the four real components (`orchestrator`/`api`/`runner`/`web`) plus
    proto/gen and docs. `config.yml` keeps blank issues enabled.
  - `.github/PULL_REQUEST_TEMPLATE.md`: checklist matching this repo's real
    gates — targeted tests, `lint`/`lint-go`/`typecheck`, `proto-check`/
    `sqlc-check` when relevant, doc updates, `PROGRESS.md` updates, no
    secrets/debug code.
  - `CODE_OF_CONDUCT.md`: standard Contributor Covenant v2.1, enforcement
    contact routed through GitHub Security Advisories (no fabricated email).
  - `CODEOWNERS`: single catch-all rule (`* @alexandre-leites`), matching
    that they are the only non-agent human contributor in `git log`.
  - Did not touch `.github/workflows/ci.yml` or `Makefile` (out of scope per
    task ownership — other in-flight PRs edit those). Skipped `CHANGELOG.md`
    per the issue's own guidance (explicitly lower priority, "currently
    hand-written per GitHub Release" — deriving one from `git log` cheaply
    was not attempted since release notes are already handled via GitHub
    Releases per `docs/release.md`).
- Validation performed:
  - `python3 -c "import yaml; yaml.safe_load(...)"` on each new
    `.github/ISSUE_TEMPLATE/*.yml` — all parse.
  - `make lint` (gofmt check across tracked/untracked `*.go` files) — passes;
    no Go files were touched by this change.
  - Confirmed `.github/workflows/ci.yml` has no markdown-lint gate, so no
    additional formatting check applies to the new docs.
  - No code changes, so no orchestrator/runner/api/web test suites were run.
- Notes: Pure documentation/meta-files change. No Go, proto, SQL, or web
  source files were touched. `.github/workflows/ci.yml` and `Makefile` were
  read-only references, per this session's ownership boundaries.
