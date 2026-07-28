# Moirai Platform

Moirai is a self-hosted control plane for durable software-engineering workflows. The orchestrator schedules eligible issues, runners execute isolated work, and the API and web dashboard expose administration and workflow state.

## Architecture

- `orchestrator/` is the Python gRPC control plane. It owns PostgreSQL state, scheduling, issue synchronization, and workflow recovery.
- `runner/` is the Go execution agent. It registers with the control plane and performs one or more eligible executions according to its configured capacity.
- `api/` is the Go HTTP gateway. It authenticates browser sessions and proxies management operations to the orchestrator over gRPC.
- `web/` is a React dashboard served by nginx. In Compose, nginx proxies `/api/` to the API service.
- `proto/` contains the gRPC contracts; `api/openapi.yaml` is the maintained HTTP contract.

## Local stack

```bash
cp .env.example .env
mkdir -p secrets
printf '%s' 'database-password' > secrets/postgres_password
printf '%s' 'postgresql://loop:database-password@postgres/loop' > secrets/database_url
printf '%s' 'admin-password' > secrets/initial_admin_password
printf '%s' 'github-token' > secrets/github_token
# Set RUNNER_REGISTRATION_TOKEN in .env to a generated secret.
docker compose up --build
```

The Compose configuration mounts secrets as files. The orchestrator reads the database URL, initial admin password, and GitHub token through their `_FILE` settings. A configured GitHub token is verified with `gh auth status` when the orchestrator starts.

The API is published at `http://localhost:8080`; the dashboard is published at `http://localhost:3000`. Compose disables the API secure-cookie setting only because this development topology terminates no TLS.

## Development

Run `make help` for available targets. `make test` runs orchestrator, runner, API, and web validation. Run `make dev-install` before Python-only targets on a clean checkout.

Each service README lists its executable configuration surface:

- [orchestrator configuration](orchestrator/README.md#configuration)
- [API configuration and HTTP contract](api/README.md#configuration)
- [runner configuration](runner/README.md#configuration)
- [web development and proxying](web/README.md#development)
