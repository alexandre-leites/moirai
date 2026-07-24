# Moirai Platform

Moirai is a self-hosted control plane for durable, autonomous software-engineering workflows.

## Getting Started

1. Copy `.env.example` to `.env`.
2. Ensure required secrets are present in `secrets/`.
3. Run `docker compose up --build`.

## Project Structure

- `api/`: Public REST API service.
- `orchestrator/`: Durable control plane (Python).
- `runner/`: Execution environment (Go).
- `web/`: Management dashboard (React).
- `proto/`: Shared gRPC service definitions.

## Development

Use `make` to run validation, testing, and protocol generation. See individual project READMEs for details.
