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
