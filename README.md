# Moirai Platform

Moirai is a self-hosted control plane for durable, autonomous software-engineering workflows.

## Getting Started

1. Copy `.env.example` to `.env`.
2. Ensure required secrets are present in `secrets/` (`postgres_password`, `database_url`).
3. In `.env`, set `RUNNER_REGISTRATION_TOKEN` to a freshly generated secret (e.g. `openssl
   rand -hex 32`) and `LOOP_INITIAL_ADMIN_PASSWORD` to a real password. Neither has a
   default — the orchestrator refuses to seed an admin user or a runner registration
   token until both are set.
4. Run `docker compose up --build`.

## Project Structure

- `api/`: Public REST API service.
- `orchestrator/`: Durable control plane (Python).
- `runner/`: Execution environment (Go).
- `web/`: Management dashboard (React).
- `proto/`: Shared gRPC service definitions.

## Development

Use `make` to run validation, testing, and protocol generation. See individual project READMEs for details.
