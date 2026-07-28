# Architecture

Moirai separates public HTTP, control-plane state, and execution environments.

```text
Browser -> web nginx -> API -> orchestrator gRPC -> PostgreSQL
                                  ^
                                  |
                              runner gRPC
```

The API does not access PostgreSQL directly. The orchestrator owns migrations, authentication/session validation, project configuration, scheduling, workflow state, GitHub integration, and runner leases. Runners receive offers over an outbound gRPC stream and maintain local identity and event delivery state. The web service is a static UI with nginx proxying `/api/` to the API service in Compose.

Shared gRPC definitions live in `proto/`; generated Go code is in `gen/go/` and Python code is under `orchestrator/src/moirai/protocols/`. The HTTP surface is separately described by [`api/openapi.yaml`](../api/openapi.yaml).
