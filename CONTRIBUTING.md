# Contributing

Thanks for considering a contribution to Moirai. This project is small and deliberately pragmatic; the rules below exist to keep it that way.

## Development setup

The repository is a Go + React monorepo. You need Go 1.25, Node 26, and Docker (with the Compose plugin). Everything else is pinned and installed by the build, or baked into the `ci-runner` image (`infra/ci-runner/Dockerfile`).

```bash
make test          # orchestrator (no DB), runner, API, web checks
make validate      # the full local gate: test, lint, vet, compose, proto, sqlc
```

`make test` deliberately excludes the orchestrator's PostgreSQL integration suites — most of the workflow state machine — because they need a real database. To run them:

```bash
docker run -d --name moirai-test-postgres -p 5432:5432 \
  -e POSTGRES_DB=loop_test -e POSTGRES_USER=loop \
  -e POSTGRES_PASSWORD=loop-test-password postgres:16-alpine

LOOP_TEST_DATABASE_URL=postgresql://loop:loop-test-password@localhost:5432/loop_test \
  make test-postgres-integration
```

`make coverage` reports and gates statement coverage across the three Go modules and the web suite; the orchestrator leg uses the same `LOOP_TEST_DATABASE_URL`.

`make help` lists every target. Each service README lists its executable configuration surface:

- [orchestrator configuration](orchestrator/README.md#configuration)
- [API configuration and HTTP contract](api/README.md#configuration)
- [runner configuration](runner/README.md#configuration)
- [web pages, development and proxying](web/README.md#pages)

## The product

Read [`PROJECT.md`](PROJECT.md) before changing behavior. It is the executable specification: the acceptance criteria, the in-scope and out-of-scope lists, and the design principles the implementation is held to. `PROGRESS.md` tracks the next incomplete requirement.

## Engineering rules

The rules the CI gate and reviewers enforce are written down in [`AGENTS.md`](AGENTS.md) §12. The ones most likely to affect a PR:

- **Database access goes through sqlc.** New or changed SQL is a `.sql` file under `orchestrator/internal/db/queries/`, regenerated with `make sqlc-generate`. Hand-written SQL in Go is rejected.
- **Keep GitHub-specific code in GitHub adapters** and OpenCode-specific code in the OpenCode backend. `api/`, `orchestrator/`, `runner/`, and `web/` are separate services with separate concerns.
- **External side effects are idempotent** — labels, branches, PRs, merges, and closes are safe to retry.
- **Bound retries and repair loops.** Every retry loop has explicit limits.
- **Persist workflow state outside agent sessions.** An agent session can be replaced without losing the task.
- **Secrets never reach agent processes, and are redacted from logs.**

## Pull request process

1. Work on a branch named after the change (`fix/...`, `infra/...`, `docs/...`, `feat/...`). One logical change per PR.
2. Keep the diff surgical. No unrelated refactors, no speculative abstractions.
3. Add or update tests with the change, and run the targeted checks for what you touched before opening the PR.
4. Open the PR against `main`. CI runs the full gate; the PR must be green to merge.
5. A reviewer approves, then the PR is merged.

There is a pull request template at [`.github/PULL_REQUEST_TEMPLATE.md`](.github/PULL_REQUEST_TEMPLATE.md); fill in the test/verification checklist.

## Reporting bugs and security issues

- For a bug or feature request, use the issue templates in [`.github/ISSUE_TEMPLATE/`](.github/ISSUE_TEMPLATE/).
- For a security vulnerability, follow the disclosure process in [`SECURITY.md`](SECURITY.md) — do not open a public issue.

## Code of conduct

Behave per [`CODE_OF_CONDUCT.md`](CODE_OF_CONDUCT.md). Be honest, be specific, and assume good faith.
