# DockMind

DockMind is a lightweight daemon that manages the lifecycle of an AI inference server running on an external GPU.

## Features

- **eGPU Power Control** — power the eGPU on/off via a Shelly Plug Gen3 on the local network.
- **Inference Backend Lifecycle** — start and stop the `llama-swap` Docker container through the Docker CLI.
- **Deterministic State Machine** — tracks Off / Starting / Ready / ShuttingDown / Error states with single-transition concurrency.
- **REST API** — simple HTTP endpoints for status, power control, restart, and daemon health.
- **GPU Monitoring** — detects GPU availability and name via `nvidia-smi`.
- **Health Monitoring** — checks `llama-swap` readiness through its `/v1/models` endpoint.

See [docs/product.md](docs/product.md) for the full feature list and non-goals.

## Quick Start

Prerequisites:

- Go 1.24
- Docker CLI
- `nvidia-smi`
- A Shelly Plug Gen3 on the LAN
- A `llama-swap` Docker container already set up on the host

Build and run:

```bash
make build                 # produces ./dockmind
cp configs/config.yaml ./config.yaml
# edit config.yaml with your Shelly address and container name
./dockmind --config config.yaml   # default path is ./config.yaml
```

Development commands:

```bash
make test
make lint
```

## API Endpoints

| Method | Path | Description |
|--------|------|-------------|
| GET | `/status` | Current system state and component health |
| POST | `/power/on` | Power on the eGPU and start `llama-swap` |
| POST | `/power/off` | Stop `llama-swap` and power off the eGPU |
| POST | `/restart` | Stop then start the complete system |
| GET | `/health` | DockMind daemon health (does not indicate GPU readiness) |
| GET | `/docs` | Interactive Swagger UI for exploring the API |
| GET | `/` | Responsive web UI for monitoring and controlling DockMind |

Full request-response details and state-machine transitions are in [docs/DockMind_MVP_Specification.md](docs/DockMind_MVP_Specification.md).

## Configuration

Optional fields have defaults; see the spec for the complete schema.

```yaml
server:
  address: ":8080"
shelly:
  address: 192.168.1.50
  channel: 0
docker:
  container: llama-swap
llamaSwap:
  healthUrl: http://localhost:1234/v1/models
gpu:
  pollInterval: 1s
startup:
  timeout: 60s
shutdown:
  timeout: 30s
```

## Status Example

`GET /status` returns:

```json
{
  "state": "Ready",
  "gpuPresent": true,
  "gpuName": "NVIDIA GeForce RTX 5060 Ti",
  "shellyOn": true,
  "llamaSwapRunning": true,
  "llamaSwapHealthy": true,
  "lastError": null
}
```

## Project Structure

```text
.
├── cmd/dockmind              # daemon entry point
├── internal/
│   ├── api                   # HTTP handlers
│   ├── config                # config loading and validation
│   ├── docker                # Docker CLI wrapper
│   ├── gpu                   # nvidia-smi monitoring
│   ├── health                # llama-swap health checks
│   ├── shelly                # Shelly Plug RPC client
│   └── state                 # state machine and transitions
├── configs/
│   └── config.yaml           # example configuration
├── docs/                     # full specification and product overview
└── Makefile                  # build, test, lint
```

## Further Reading

- [MVP Specification](docs/DockMind_MVP_Specification.md) — complete API semantics, state machine, and config schema.
- [Product Overview](docs/product.md) — features, non-goals, and known limitations.
