# Hermes / Forge Public Snapshot

Hermes / Forge is a Go walking skeleton for reliable task-envelope transport:

- **Hermes** stores envelopes, exposes HTTP/MCP-facing APIs, and tracks delivery/status truth.
- **Forge** is a local execution gateway that accepts deliveries and binds them to executor sessions.

This public snapshot is sanitized. It intentionally omits private operational runbooks, deployment manifests, local secrets, generated artifacts, databases, logs, and built binaries.

## Repository layout

```text
hermes/       Go module: transport/status service
forge/        Go module: local execution gateway
test/e2e/     Go module: in-process end-to-end tests
examples/     Sanitized sample envelope payloads
```

## Quick start

```bash
make test
```

Or run modules individually:

```bash
cd hermes && go test ./...
cd forge && go test ./...
cd test/e2e && go test ./...
```

## Configuration

Copy `.env.example` and replace placeholder values for local experiments. Do not commit real credentials.

## License

MIT. See `LICENSE`.
