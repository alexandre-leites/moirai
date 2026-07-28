# Moirai Platform

Moirai is a self-hosted control plane for durable, autonomous software-engineering workflows. See [`PROJECT.md`](PROJECT.md) for the architecture and product scope.

## Quick Start

Prerequisites: Docker Engine with the Compose plugin and a GitHub token that can access the repository you intend to automate.

```bash
cp .env.example .env
mkdir -p secrets
printf '%s\n' 'choose-a-strong-postgres-password' > secrets/postgres_password
printf '%s\n' 'postgresql+asyncpg://loop:choose-a-strong-postgres-password@postgres:5432/loop' > secrets/database_url
printf '%s\n' 'choose-a-strong-admin-password' > secrets/initial_admin_password
printf '%s\n' 'github-token-with-repo-and-workflow-scopes' > secrets/github_token
openssl rand -hex 32 > secrets/runner_registration_token
chmod 600 secrets/*
docker compose up --build -d
docker compose ps
curl --fail http://localhost:3000/
curl --fail http://localhost:8080/ready
```

`docker compose ps` reports every service as healthy once startup completes. The API port is bound to loopback for local diagnostics; use the web endpoint on port 3000 for normal browser access. Stop the stack with `docker compose down`. Add `-v` only when you intentionally want to remove local database and runner data.

Compose reads passwords, tokens, and registration credentials only from the files in `secrets/`. Do not put their values in `.env` or shell environment variables. The `.env` file contains non-secret configuration only; its `${...}` values are the variables Compose reads.

## Project Structure

- `api/` — Public REST API (Go)
- `orchestrator/` — Control plane (Python + LangGraph)
- `runner/` — Execution environment (Go)
- `web/` — Management dashboard (React + TypeScript)
- `proto/` — Shared gRPC service definitions

## Development

Use `make` for validation, testing, and protocol generation.
