# Implementation Progress

## Issue #363 — orchestrator tests silently skip when LOOP_TEST_DATABASE_URL unset

- Agent/session identifier: issue-363 worktree agent, 2026-08-03
- Status: Done

### Problem

`orchestrator/internal/server` has 8 files tagged `//go:build integration`
(~115 tests) that require a real Postgres via `LOOP_TEST_DATABASE_URL`. Two
guard points (`newHarness` in `integration_test.go`, `scratchDatabaseURL` in
`tasksource_integration_test.go`) called `t.Skip` when the var was unset.
Combined with `go test`'s default non-verbose behavior (no skip count, no
per-test output on a passing run), a developer who explicitly opted into the
suite via `-tags integration` but forgot the database saw a plain `ok` with
zero indication the state-machine suite never ran.

### Investigation

- Confirmed `go test ./...` (no `-tags integration`) never compiles these 8
  files at all — the build tag excludes them entirely, so that path was
  already unaffected and is not the footgun (this is standard, discoverable
  Go convention, documented at the top of `integration_test.go` and in the
  Makefile).
- Confirmed the real gap: `go test -tags integration ./internal/server/...`
  without the env var printed a bare `ok` in ~0.08s (verified with
  `go clean -testcache` between runs).
- Tried a `TestMain` that prints a banner to stderr when the var is unset.
  Verified experimentally that `go test` **discards a passing test binary's
  direct stdout/stderr unless `-v` is passed** — the banner appeared under
  `-v` but was completely invisible under plain `go test` (redirected to a
  file, confirmed empty). This means a "print a notice" fix cannot actually
  satisfy the issue's goal for the exact command in the bug report; only a
  failing test guarantees `go test` prints output. Discarded this approach.
- Checked whether Postgres is an established "always running" local-dev
  assumption (relevant per the task brief): `compose.yaml`'s `postgres`
  service uses `MOIRAI_POSTGRES_PASSWORD`/db `loop`, while the integration
  suite's `LOOP_TEST_DATABASE_URL` documented default is a *different*
  database (`loop_test`, user `loop`/`loop-test-password`) — not the same
  instance docker-compose brings up for running the app. So plain
  `go test ./...` for a developer who has never touched Postgres must stay
  green, but a developer who deliberately passes `-tags integration` has
  already opted in and fail-loud is the right (and only truly visible)
  behavior for them.
- The `Makefile`'s `test-postgres-integration` target already had a guard
  (`test -n "$(LOOP_TEST_DATABASE_URL)"`) that fails the `make` invocation
  when unset, but with no explanatory message (bare `Error 1`), and it only
  protects the `make` entrypoint — running `go test -tags integration`
  directly (as the file header itself documents) bypassed it entirely.

### Fix

- `orchestrator/internal/server/integration_test.go`: `newHarness` now
  `t.Fatal`s with a new `missingTestDatabaseMessage` constant (explains what
  the suite needs and how to run it) instead of `t.Skip`. Added a paragraph
  to the file's top-of-file comment explaining the rationale and citing
  #363.
- `orchestrator/internal/server/tasksource_integration_test.go`:
  `scratchDatabaseURL` now `t.Fatal`s with the same message instead of
  `t.Skip`, and its doc comment was updated to match.
- `Makefile`: `test-postgres-integration`'s guard now prints a one-line
  explanation (with an example invocation) to stderr before exiting
  nonzero, instead of failing silently with just `Error 1`.
- Deliberately did **not** touch the `//go:build integration` tag itself —
  removing it (making these tests always compile into `go test ./...`)
  would force every plain `go test ./...` run to have Postgres, which is not
  an established assumption in this repo (see investigation above) and would
  be a much larger, riskier change than the issue asks for.

### Acceptance criteria verification

1. "Print which tests were skipped and why" / "fail loudly on missing DB" /
   "document in a prominent output line" — implemented the fail-loud option,
   justified above (only option `go test` actually surfaces by default).
2. Skip is now visible to a developer, not silent — verified: see commands
   and real output below.
3. CI (`LOOP_TEST_DATABASE_URL` always set there) is unaffected — verified:
   full suite passes against a real Postgres.
4. Developers without Postgres who are not trying to run the DB suite are
   unaffected — verified: plain `go test ./...` unchanged, still green,
   0 reliance on Postgres.

### Validation performed (exact commands and real results)

WITHOUT DB, before the fix (repro of the bug as filed), reproduced by
temporarily checking out the pre-fix file:
```
$ cd orchestrator && go test -tags integration ./internal/server/... 2>&1 | cat
ok  	github.com/loop-engineering/orchestrator/internal/server	0.089s
```
(No indication ~115 tests were skipped — the bug as described.)

WITHOUT DB, after the fix:
```
$ cd orchestrator && go test -tags integration ./internal/server/... 2>&1 | head -20
--- FAIL: TestStreamEventsColdConnectSkipsHistory (0.00s)
    events_test.go:99: LOOP_TEST_DATABASE_URL is not set.

        This package's Postgres-backed integration suite (`-tags integration`) needs a
        real database -- ...
        Run it with:
            make test-postgres-integration
        or directly:
            LOOP_TEST_DATABASE_URL=postgresql://loop:loop-test-password@localhost:5432/loop_test \
                go test -tags integration -race -count=1 ./internal/server/
FAIL
```
Every one of the ~115 database-backed tests now fails loudly and
individually with an explanation, instead of a silent pass.

Plain `go test ./...` (no `-tags integration`, the default developer
command) unaffected, still green:
```
$ cd orchestrator && go test ./...
ok  	github.com/loop-engineering/orchestrator	0.001s
ok  	github.com/loop-engineering/orchestrator/cmd/orchestrator	0.003s
ok  	github.com/loop-engineering/orchestrator/internal/config	0.003s
ok  	github.com/loop-engineering/orchestrator/internal/metrics	0.006s
ok  	github.com/loop-engineering/orchestrator/internal/migrate	0.001s
ok  	github.com/loop-engineering/orchestrator/internal/server	0.085s
```

WITH DB (spun up a namespaced, throwaway container so as not to collide with
any other agent's local Postgres — `moirai363-pg`, host port `55432`,
removed with `docker rm -f moirai363-pg` after use):
```
$ docker run -d --name moirai363-pg -e POSTGRES_USER=loop \
    -e POSTGRES_PASSWORD=loop-test-password -e POSTGRES_DB=loop_test \
    -p 55432:5432 postgres:16-alpine
$ cd orchestrator && LOOP_TEST_DATABASE_URL="postgresql://loop:loop-test-password@localhost:55432/loop_test" \
    go test -tags integration -race -count=1 ./internal/server/...
ok  	github.com/loop-engineering/orchestrator/internal/server	19.749s
```

`make test-postgres-integration`, without and with the var:
```
$ make test-postgres-integration
test-postgres-integration: LOOP_TEST_DATABASE_URL is not set; e.g. LOOP_TEST_DATABASE_URL=postgresql://loop:loop-test-password@localhost:5432/loop_test make test-postgres-integration
make: *** [Makefile:24: test-postgres-integration] Error 1

$ LOOP_TEST_DATABASE_URL="postgresql://loop:loop-test-password@localhost:55432/loop_test" make test-postgres-integration
cd orchestrator && go test -tags integration -race -count=1 ./internal/server/
ok  	github.com/loop-engineering/orchestrator/internal/server	20.008s
```

Format and full unit suite:
```
$ gofmt -l $(git ls-files --cached --others --exclude-standard -- '*.go')
(no output — clean)
$ cd orchestrator && go vet ./...
(no output — clean)
$ cd orchestrator && go build -tags integration ./...
(builds clean)
$ cd orchestrator && go test -race ./...
ok all packages
```

### Notes

- No `.proto`/shared-contract files touched; change is confined to
  `orchestrator/internal/server/*_test.go` and the top-level `Makefile`.
- Docker container `moirai363-pg` was removed after validation; no leftover
  containers, volumes, or non-default ports left running.
