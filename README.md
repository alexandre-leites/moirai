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

## Workflow recovery guarantees

- Terminal runner events persist a diff hash and outcome fingerprint. Four identical terminal outcomes block the workflow instead of retrying indefinitely.
- Three consecutive blocked workflows with the same project failure reason open that project's circuit. After a five-minute cooldown, one durable half-open probe is allowed; its delivery closes the circuit and a blocked probe reopens it. Provider failures use the same durable circuit state.
- Task packets carry acceptance criteria, prior failures, revision and diff context. Reviewer prompts are independently generated and exclude developer plans or reasoning.
- Issue reconciliation revisits terminal workflows so `agent:blocked` and `agent:delivered` labels converge after retries or restarts.

## Project Structure

- `api/` — Public REST API (Go)
- `orchestrator/` — Control plane (Python + LangGraph)
- `runner/` — Execution environment (Go)
- `web/` — Management dashboard (React + TypeScript)
- `proto/` — Shared gRPC service definitions

## Development

Use `make` for validation, testing, and protocol generation.
