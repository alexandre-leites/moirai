# Moirai Orchestrator

Durable orchestration control plane for Moirai.

## Setup

- Ensure `PYTHONPATH=src` is set.
- Install dependencies: `pip install -e .`

## Operations

Logs are JSON and retain structured fields passed with Python logging `extra`. Metrics are served at `LOOP_METRICS_BIND` (default `0.0.0.0:9090`) on `/metrics`.

The gRPC listener stays insecure by default for local development. Set `LOOP_GRPC_TLS_CERT_FILE` and `LOOP_GRPC_TLS_KEY_FILE` to enable TLS. Set `LOOP_GRPC_TLS_CLIENT_CA_FILE` too to require runner mTLS certificates.

## Testing

Run tests using: `PYTHONPATH=src python3 -m unittest discover -s tests -v`
