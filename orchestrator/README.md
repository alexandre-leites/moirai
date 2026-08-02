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

The process applies SQL migrations from `migrations/`, serves gRPC on port `50051`, and runs the scheduler, the check observer, the recovery sweep, and the issue sync loop. `gh` is required for issue sync, pull requests, checks, and merges.

Migrations are read from `migrations/` relative to the working directory, which is why the commands above are run from `orchestrator/`; the image copies them next to the binary and sets `WORKDIR /app`.

## Configuration

Every secret below can be supplied either directly or as `<NAME>_FILE` pointing at a file to read it from. Setting both forms of the same secret is refused rather than resolved, so a half-migrated deployment fails loudly instead of using the one you did not mean.

| Variable | Required | Meaning |
|---|---|---|
| `LOOP_DATABASE_URL` | yes | PostgreSQL connection URL. A `postgresql+asyncpg://` scheme is accepted and normalised, so connection strings written for the previous Python orchestrator keep working. |
| `LOOP_GRPC_BIND` | no | `host:port` to serve gRPC on. Defaults to `0.0.0.0:50051`. The container healthcheck derives its port from this. |
| `LOOP_SECRET_KEY` | for credentials | 32-byte key, base64 or hex, used to encrypt project credentials at rest. Storing a credential without it is refused. |
| `LOOP_INITIAL_ADMIN_USERNAME` / `LOOP_INITIAL_ADMIN_PASSWORD` | first boot | Seeds the first admin account. |
| `RUNNER_REGISTRATION_TOKEN` | no | Seeds a single-use runner registration token valid for 15 minutes. Its expiry is re-armed on every start, so a restart does not leave an expired token behind. |
| `LOOP_SEED_TOKEN_LABELS` | no | Comma-separated labels the seeded token may register. Defaults to `linux`. |
| `LOOP_GITHUB_TOKEN` | no | Token handed to `gh` for issue sync, pull requests, checks and merges. |
| `LOOP_ISSUE_SYNC_INTERVAL` | no | How often issues are re-read from the tracker, as a Go duration. Defaults to `2m`. |
| `MOIRAI_BUILD_VERSION` | no | Build identifier reported by `GetSystemVersion` and shown in the console. |

### TLS

The gRPC endpoint is plaintext unless a certificate is configured. `LOOP_GRPC_TLS_CERT_FILE` and `LOOP_GRPC_TLS_KEY_FILE` must be set together — setting one alone is refused rather than silently downgraded, because an operator who configured half of it asked for an encrypted endpoint. `LOOP_GRPC_TLS_CLIENT_CA_FILE` additionally requires client certificates, and is meaningless without server TLS.

Runner secret delivery (`ResolveJobSecret`, `StoreJobSecret`) is refused over a plaintext peer, so a deployment that uses project credentials needs TLS configured here and on the runner. `compose.tls.yaml` wires both ends together.
