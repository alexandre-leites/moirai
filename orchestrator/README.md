# Moirai Orchestrator

Go gRPC control plane for projects, runners, issue scheduling, GitHub delivery, and PostgreSQL state.

## Local checks

```bash
go test ./...
go vet ./...
```

## Run locally

```bash
LOOP_DATABASE_URL='postgresql://loop:password@localhost/loop' \
LOOP_INITIAL_ADMIN_PASSWORD='Moirai-Local-1' \
RUNNER_REGISTRATION_TOKEN='local-runner-token' \
go run ./cmd/orchestrator
```

The process applies SQL migrations from `migrations/`, serves gRPC on port `50051`, and runs scheduler/check observers. `gh` is required for issue sync, pull requests, checks, and merges.
