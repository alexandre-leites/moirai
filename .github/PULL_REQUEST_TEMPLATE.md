## What and why

<!-- Describe the change and the problem it solves. Link the issue it closes, e.g. "Closes #123". -->

## Component(s) affected

<!-- orchestrator / api / runner / web / proto / docs / infra -->

## How this was tested

<!-- List the commands you ran, e.g.:
- `make test-orchestrator`
- `make test-web`
- `make validate`
-->

## Checklist

- [ ] Tests were added or updated for this change, and pass locally.
- [ ] `make lint` / `make lint-go` / `make typecheck` (as relevant to the changed service) pass locally.
- [ ] `make proto-check` / `make sqlc-check` pass, if `.proto` files or SQL queries changed.
- [ ] Relevant documentation was updated (`README.md`, `PROJECT.md`, service `README.md`, `AGENTS.md`, etc.), if behavior or configuration changed.
- [ ] No secrets, debug code, or local artifacts are included in this change.
