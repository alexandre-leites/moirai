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

## Session: issue-372 (zero coverage reporting infrastructure)

- Completed: 2026-08-05
- Agent/session identifier: issue-372 worktree agent
- Relevant files:
  - `scripts/go-coverage.sh` (new)
  - `Makefile` (`coverage-go`, `coverage-web`, `coverage` targets)
  - `.github/workflows/ci.yml` (`coverage-go`, `coverage-web` jobs)
  - `web/package.json`, `web/package-lock.json` (`@vitest/coverage-v8` devDependency)
  - `web/vitest.config.ts` (`test.coverage` block)
  - `.gitignore` (`coverage.out`)
- Behavior delivered:
  - Go: `scripts/go-coverage.sh <module-dir> <floor-percent>` runs
    `go test -coverprofile=coverage.out ./...` for one module, prints
    `go tool cover -func=coverage.out` (per-function + total) to the log, then
    fails if the module's total statement coverage is below its floor.
    `make coverage-go` runs it for `orchestrator`, `runner` and `api` (the
    three Go modules with tests; `gen/go` is generated protobuf code with no
    tests and was deliberately left out).
  - Web: `@vitest/coverage-v8@^4.1.10` (pinned to match the installed
    `vitest@4.1.10`) added to devDependencies; `vitest.config.ts` gained a
    `test.coverage` block (`provider: "v8"`, `reporter: ["text", "text-summary"]`,
    thresholds statements 70/branches 65/functions 70/lines 75). Only active
    behind `--coverage`, so `npm test` is unchanged; `npm run test --
    --coverage` (the exact invocation the issue specified) or
    `make coverage-web` (`npm ci && npm run test -- --coverage`) both print
    the per-file and summary coverage table to the log and enforce the
    thresholds.
  - CI: two new jobs mirroring the existing `lint-go`/`test-web` job-per-check
    pattern -- `coverage-go` (needs Go, runs `make coverage-go`) and
    `coverage-web` (needs Node, runs `make coverage-web`) -- both added to
    `validate`'s `needs` list so a coverage regression below floor fails the
    PR the same way a lint or test failure would, visible as its own named
    GitHub check rather than folded into `lint-go`/`test-web`.
  - Thresholds are deliberately conservative floors, not the issue's
    aspirational 70%, chosen from real measurements taken on this branch so
    the PR that introduces the gate cannot fail its own gate:
    - `api`: 72.4% actual -> 65% floor. Weak package: `internal/orchestrator`
      (TLS/CA loading) at 27.1%; everything else in the module is 85%+.
    - `runner`: 80.6% actual -> 75% floor. Every package already above 65%;
      closest module to the 70% aspiration.
    - `orchestrator`: 16.0% actual (unit suite only, `go test ./...` with no
      build tags) -> 12% floor. This number understates the module: its
      largest package, `internal/server`, is exercised mainly by
      `make test-postgres-integration` (build tag `integration`, needs a real
      Postgres), which `coverage-go` does not run and whose coverage this
      profile does not merge in. `idgen`, `secrethash` and `textutil` are a
      genuine 0% and the real, actionable gap -- flagged as follow-up work
      rather than fixed in this PR, since fixing coverage gaps is a different
      task from building the reporting infrastructure that makes them
      visible.
    - `web`: 84.76%/77.22%/83.19%/87.89% (stmts/branches/funcs/lines) actual,
      already above the issue's 70% aspiration on every axis except branches;
      set floors at 70/65/70/75 for headroom against normal run-to-run
      variance.
    Raise a floor only in the same PR that raises the module's real coverage,
    per the comments left in `Makefile#coverage-go` and `vitest.config.ts`.
- Validation performed:
  - `cd api && go test -coverprofile=... ./... && go tool cover -func=...`
    -> total 72.4%
  - `cd runner && go test -coverprofile=... ./... && go tool cover -func=...`
    -> total 80.6%
  - `cd orchestrator && go test -coverprofile=... ./... && go tool cover -func=...`
    -> total 16.0%
  - `sh scripts/go-coverage.sh api 65` -> exit 0, prints report + total
  - `sh scripts/go-coverage.sh runner 75` -> exit 0
  - `sh scripts/go-coverage.sh orchestrator 12` -> exit 0
  - `sh scripts/go-coverage.sh orchestrator 99` -> exit 1 (deliberate
    failure-path check: confirms the floor actually gates)
  - `npm install` (web, adds `@vitest/coverage-v8`) -> 0 vulnerabilities
  - `npm run test -- --coverage` (web) -> 248 passed, coverage 84.76% stmts,
    exit 0, thresholds satisfied
  - `make coverage` (root) -> runs both `coverage-go` and `coverage-web` in
    sequence, exit 0
  - `python3 -c "import yaml; yaml.safe_load(open('.github/workflows/ci.yml'))"`
    -> parses clean
- Notes: `gen/go` (protobuf-generated Go module) was intentionally excluded
  from `coverage-go` -- it has no `_test.go` files, so there is nothing to
  measure. Files touched are scoped to this session's ownership (CI workflow
  files, `web/package.json` devDependencies, Go module test/CI config); no
  handler, `events.ts`, `console-data.tsx` or `auth.go` files (owned by
  issues #362 / #388) were touched.
