# Moirai Platform

Moirai is a self-hosted control plane for durable, autonomous software-engineering workflows. See [`PROJECT.md`](PROJECT.md) for the full architecture, current status, and known issues.

## Quick Start

```bash
cp .env.example .env
# Edit .env: set LOOP_INITIAL_ADMIN_PASSWORD and RUNNER_REGISTRATION_TOKEN
# Populate secrets/ (see .env.example for required files)
docker compose up --build
```

**Note:** Several [P0 bugs](https://github.com/alexandre-leites/moirai/issues?q=is%3Aopen+is%3Aissue+label%3AP0) prevent a clean startup. See GitHub issues for the current fix list.

## Project Structure

- `api/` — Public REST API (Go)
- `orchestrator/` — Control plane (Python + LangGraph)
- `runner/` — Execution environment (Go)
- `web/` — Management dashboard (React + TypeScript)
- `proto/` — Shared gRPC service definitions

## Development

Use `make` for validation, testing, and protocol generation.
