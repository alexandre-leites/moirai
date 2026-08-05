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
## Session: issue-388 (logout never revoked the server-side session)

- Completed: 2026-08-05
- Agent/session identifier: issue-388 worktree agent
- Relevant files:
  - `api/internal/http/handlers/auth.go`
  - `api/internal/http/handlers/auth_test.go`
  - `api/openapi.yaml`
- Bug: `POST /api/v1/auth/logout` was registered as a bare
  `http.HandlerFunc(h.logout)`, with no `auth.RequireSession` (or any)
  middleware in front of it. The handler reads
  `auth.SessionToken(r.Context())`, which is only ever populated by
  `RequireSession` copying the session cookie into the request context. With
  no middleware, that lookup always returned `ok = false`, so
  `h.client.Logout(ctx)` was never called. The response still cleared cookies
  and returned `204`, so logout looked successful while leaving the session
  valid server-side — a captured/leaked token kept working after "logout".
- Fix: registered the route as
  `requireMutation(h.mutationLimiter, h.logout)` (the same helper `PUT
  /api/v1/auth/account` already uses), instead of a bare `auth.RequireSession`
  wrap. Reasoning: logout is a mutation (it revokes a server-side session) and
  `api/openapi.yaml` already documented `sessionCookie` + `csrfToken` as
  required security schemes for this route, and the console
  (`web/src/api.ts` `logout()`) already sends the CSRF header on this call —
  so CSRF protection was already the intended contract, just never wired up
  server-side. `requireMutation` = `RequireSession` + `RequireCSRF` + the
  existing shared `mutationLimiter` (already applied to every other mutating
  route via `cmd/api/main.go`), so this brings logout in line with existing
  precedent rather than inventing a new pattern.
- Deliberate decision (unauthenticated logout status code): an unauthenticated
  `POST /auth/logout` now answers `401`, not `204`. `RequireSession` needs the
  session cookie to know which server-side session to revoke, so there is no
  session to no-op against if the cookie is absent; every other
  session-protected route (`/auth/me`, `/auth/account`) already answers `401`
  the same way, and there is nothing sensitive in telling an unauthenticated
  caller it isn't signed in. `api/openapi.yaml` already documented `401`/`403`
  responses for this route from an earlier PR, so no spec change was needed
  for the status codes — only the summary/description were tightened to state
  plainly that logout revokes the session server-side (previously it only
  mentioned clearing cookies). A pre-existing test,
  `TestLogoutIdempotentWithoutSession`, asserted the old (buggy) `204`
  behavior for a cookie-less call; it was replaced with
  `TestLogoutRequiresSession` (expects `401`, matching
  `TestAuthMeRequiresSession`).
- Tests added:
  - `TestLogoutRequiresSession` — no session cookie → `401`.
  - `TestLogoutRequiresCSRF` — session cookie but no CSRF token/cookie → `403`.
  - `TestLogoutRouteRevokesTheSessionServerSide` — the real regression test:
    drives the request through the actual `http.ServeMux` (not calling
    `h.logout` directly, which the pre-existing tests did and which is why
    they did not catch the bug), with a real session cookie + matching CSRF
    cookie/header, and asserts (a) the orchestrator stub's `Logout` was
    called exactly once and (b) the *exact* captured session token
    (`"captured-token"`) was propagated to the outgoing gRPC metadata
    (`x-loop-session`, via `orchestrator.WithSession`) that the stub observes
    via `metadata.FromOutgoingContext`. This proves the specific token that
    was captured before logout is the one sent for revocation, not just that
    some call happened.
  - Existing tests `TestLogoutRevokesTheSessionServerSide` and
    `TestLogoutClearsCookiesEvenWhenRevocationFails` (call `h.logout`
    directly with a pre-populated context, added in PR #387) were left as-is
    and still pass — they pin handler-level behavior; the new route-level
    test additionally proves the *wiring* (middleware -> handler) that was
    the actual bug.
  - Removed the stale `NOTE` comment on the `logoutRequest()` helper that
    documented the (now-fixed) gap.
- Validation performed (targeted, from `api/`):
  - `go build ./...` → clean.
  - `go test ./...` → all packages pass (`auth`, `config`, `http`,
    `http/handlers`, `orchestrator`).
  - `go vet ./...` → clean.
  - `gofmt -l` on the changed files → clean.
  - `golangci-lint run ./...` → `0 issues`.
  - `make test-api` (repo root) → passes.
- Commands executed:
  - `cd api && go build ./... && go test ./... && go vet ./...`
  - `gofmt -l api/internal/http/handlers/auth.go api/internal/http/handlers/auth_test.go`
  - `cd api && golangci-lint run ./...`
  - `make test-api` (from repo root)
- Notes: Only `api/internal/http/handlers/auth.go`,
  `api/internal/http/handlers/auth_test.go` and `api/openapi.yaml` were
  touched, per this session's file ownership. No `events.go`, `web/`, or CI
  files were touched (those belong to sibling issues #362 / #372).

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

## Session: issue-365 (drop dead LangGraph schema and asyncpg DSN compat)

- Completed: 2026-08-05
- Agent/session identifier: issue-365 worktree agent
- Root cause: #247 replaced the Python/LangGraph orchestrator with the Go
  state machine, but the schema/tables it wrote to (`langgraph.checkpoints`,
  `langgraph.checkpoint_writes`), the sqlc-generated Go structs for them
  (`LanggraphCheckpoint`, `LanggraphCheckpointWrite`), the SQLAlchemy-style
  `postgresql+asyncpg://` default DSNs in the three compose files, and the
  compat shim in `config.go` that stripped the `+asyncpg` suffix were never
  cleaned up. They cost every fresh deployment a dead schema plus a
  never-exercised code path, and the 2026-07-29 platform review still read
  as a live description of a codebase that no longer exists.
- Relevant files:
  - `orchestrator/migrations/033_drop_langgraph.sql` (new): drops
    `langgraph.checkpoint_writes`, `langgraph.checkpoints`, and the
    `langgraph` schema. The old 001/002 migrations were left untouched
    (migrations are historical, not editable in place).
  - `orchestrator/internal/db/models.go`: regenerated via `make
    sqlc-generate`; the only diff is the removal of the two `Langgraph*`
    structs, since sqlc derives its schema by replaying every migration in
    order and the new migration removes `langgraph` from that replayed
    state.
  - `orchestrator/internal/config/config.go`: removed `normalizeDatabaseURL`
    and the `net/url` import it required; `Config.DatabaseURL` now just
    carries the value from `LOOP_DATABASE_URL` unchanged.
  - `orchestrator/internal/config/config_test.go`: removed
    `TestLoadNormalizesPythonDatabaseURL`, which exercised the removed shim.
  - `orchestrator/README.md`: updated the `LOOP_DATABASE_URL` table row,
    which documented the now-removed `+asyncpg` normalization.
  - `compose.yaml`, `compose.tls-stack.yaml`, `compose.secrets.yaml`:
    default/example DSNs changed from `postgresql+asyncpg://` to
    `postgresql://`.
  - `docs/reviews/2026-07-29-platform-review.md`: added a "Historical"
    callout at the top; body left untouched.
- Behavior delivered: a fresh deployment no longer creates or ships a schema
  nothing reads from, and `LOOP_DATABASE_URL` is a plain PostgreSQL URL with
  no scheme-rewriting behind it. Confirmed no other `langgraph` references
  exist anywhere in the repo (migrations, generated code, docs) apart from a
  now-historical checkbox in `tasks/todo.md` that was left alone as an old
  changelog entry.
- Validation performed: `make sqlc-generate` (regenerated models.go with the
  expected diff only), `make lint` (gofmt), `make lint-go` (golangci-lint,
  three modules), `make typecheck` (go vet), `make test-orchestrator` (unit
  suite), a full PostgreSQL integration run against a disposable
  `moirai-test-postgres-issue-365` container (verifies migrations 001-033
  apply cleanly end to end and nothing depends on the dropped schema), a
  manual `\dn` against that database confirming only `app` and `public`
  schemas remain post-migration, and `make compose`.
- Commands executed (from the `issue-365` worktree root):
  - `make sqlc-generate` → regenerated `orchestrator/internal/db/models.go`;
    `git diff` shows only the two `Langgraph*` structs removed
  - `make lint` → clean
  - `make lint-go` → `0 issues.` x3 (orchestrator, runner, api)
  - `make typecheck` → clean
  - `make test-orchestrator` → all non-integration Go suites pass
  - `docker run -d --name moirai-test-postgres-issue-365 -p 5433:5432 -e
    POSTGRES_DB=loop_test -e POSTGRES_USER=loop -e
    POSTGRES_PASSWORD=loop-test-password postgres:16-alpine`
  - `LOOP_TEST_DATABASE_URL=postgresql://loop:loop-test-password@localhost:5433/loop_test
    make test-postgres-integration` → `ok` 105 tests, `internal/server`
    19.5s
  - `docker exec moirai-test-postgres-issue-365 psql -U loop -d loop_test -c
    '\dn'` → only `app` and `public` schemas listed
  - `docker rm -f moirai-test-postgres-issue-365` → test container cleaned up
  - `make compose` → clean
- Notes: `make compose-overlays` failed in this sandbox with `docker compose
  5.3.1 does not match pinned v2.38.2` — a pre-existing local tooling
  version mismatch unrelated to this change (it is a version-pin assertion
  in `scripts/render-tls-stack.sh`, not a rendering failure caused by the
  DSN edits); `compose-overlays` config for the affected files was otherwise
  inspected by hand and is well-formed. Did not touch `.github/workflows/ci.yml`,
  `Makefile`, or any top-level doc files (LICENSE/CONTRIBUTING.md/issue
  templates) — those belong to the concurrent #372/#374 work.
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
