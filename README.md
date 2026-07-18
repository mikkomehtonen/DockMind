# DockMind

DockMind is a lightweight daemon that manages the lifecycle of an AI inference server running on an external GPU.

## Features

- **eGPU Power Control** — power the eGPU on/off via a Shelly Plug Gen3 on the local network.
- **Inference Backend Lifecycle** — start and stop the `llama-swap` Docker container through the Docker CLI.
- **Deterministic State Machine** — tracks Off / Starting / Ready / ShuttingDown / Error states with single-transition concurrency.
- **REST API** — simple HTTP endpoints for status, power control, restart, and daemon health.
- **GPU Monitoring** — detects GPU availability and name via `nvidia-smi`.
- **Health Monitoring** — checks `llama-swap` readiness through its `/running` endpoint and reports the currently loaded model name(s) in status and the web UI.
- **OpenAI-Compatible Gateway** — opt-in reverse proxy that forwards OpenAI SDK requests to the backend, with auto-startup on first request and idle shutdown to save power. Cached model list is served when the backend is off.

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

### Admin API

| Method | Path | Description |
|--------|------|-------------|
| GET | `/status` | Current system state and component health |
| POST | `/power/on` | Power on the eGPU and start `llama-swap` |
| POST | `/power/off` | Stop `llama-swap`, unbind the NVIDIA driver via `dockmind-egpu-unbind.service`, then cut Shelly power. A failed unbind aborts shutdown to Error. |
| POST | `/restart` | Stop then start the complete system |
| GET | `/health` | DockMind daemon health (does not indicate GPU readiness) |
| GET | `/docs` | Interactive Swagger UI for exploring the API |
| GET | `/` | Responsive web UI for monitoring and controlling DockMind |
| GET | `/favicon.svg` | SVG favicon for the web UI |

Power endpoints return `202 Accepted` when a transition starts, `200 OK` when
the system is already in the target state, `409 Conflict` when a transition is
already in progress or the requested transition is not allowed (e.g. power-on
from Error), and `429 Too Many Requests` when the request is blocked by an
active `power.cooldown`.

### Gateway API (when enabled)

| Method | Path | Description |
|--------|------|-------------|
| POST | `/v1/chat/completions` | OpenAI-compatible chat completions proxy to llama-swap |
| GET | `/v1/models` | List available models; serves cached list when backend is off |

Full request-response details and state-machine transitions are in [docs/DockMind_MVP_Specification.md](docs/DockMind_MVP_Specification.md).

## Configuration

Optional fields have defaults; see the spec for the complete schema.
The web UI logo can link to a custom URL by setting the `LOGO_LINK_URL`
environment variable (e.g. `LOGO_LINK_URL=https://dockmind.example.org`). When
unset, the logo is a plain image with no link.

```yaml
server:
  address: ":8080"
shelly:
  address: 192.168.1.50
  channel: 0
docker:
  container: llama-swap
llamaSwap:
  healthUrl: http://localhost:1234/running
gpu:
  pollInterval: 1s
startup:
  timeout: 60s
shutdown:
  timeout: 30s
power:
  cooldown: 0s
```

`power.cooldown` sets a minimum delay between power cycles. When set to a
positive duration, `POST /power/on` is blocked for that long after the system
reaches Off, and `POST /power/off` is blocked for that long after the system
reaches Ready. Blocked requests return `429 Too Many Requests`; `GET /status`
reports the remaining seconds in `cooldownRemaining`. The default is `0s`
(disabled).

### Gateway Configuration

Enable the OpenAI-compatible gateway by setting `gateway.enabled` to `true`
and providing the llama-swap backend URL in `llamaSwap.backendUrl`. The idle
timeout controls when the system powers off after no requests. Set it to `0`
to disable idle shutdown (default).

```yaml
gateway:
  enabled: true
  idleTimeout: 300s
  requestTimeout: 120s
  modelsCacheDir: /var/lib/dockmind
  modelsRefreshInterval: 60s
llamaSwap:
  healthUrl: http://localhost:1234/running
  backendUrl: http://localhost:1234
```

The gateway auto-starts the system on first request, resets the idle timer
on each new request and when streaming responses close. When the backend is
unavailable, it returns `503 Service Unavailable` with a `Retry-After` header.
Cached model lists are served from memory with an `X-DockMind-Cached: true`
header. Set `gateway.modelsCacheDir` to a writable directory to persist the
cached model list across DockMind restarts; leave it unset to keep the cache
in-memory only. When the gateway is enabled, a background goroutine fetches
`/v1/models` every `gateway.modelsRefreshInterval` (default `60s`) while the
system is `Ready` so the cache stays warm without requiring a client request.

When `power.cooldown` is enabled and the gateway is enabled, the effective idle
shutdown timeout is raised to at least `power.cooldown` so the idle watcher does
not try to shut down during the post-startup cooldown. A warning is logged when
this adjustment happens.

When the gateway is enabled with `idleTimeout > 0`, `GET /status` reports the
remaining seconds before an idle auto-shutdown in `idleRemaining`, and the web UI
shows an auto-shutdown countdown. The countdown is hidden when the state is not
`Ready` or while an inference request is in flight.

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
  "loadedModels": ["qwen3.5-9b"],
  "lastError": null,
  "cooldownRemaining": 0,
  "idleRemaining": 0
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
│   ├── gateway               # OpenAI-compatible reverse proxy with idle shutdown
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
- [Gateway Design](docs/DockMind_Gateway_Design.md) — design for the opt-in OpenAI-compatible gateway with auto-startup and idle shutdown.
