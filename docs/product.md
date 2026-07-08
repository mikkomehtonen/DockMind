# DockMind

DockMind is a lightweight daemon that manages the lifecycle of an AI inference
server running on an external GPU (eGPU). It provides a simple HTTP API for
powering the eGPU on/off via a Shelly Plug Gen3, starting and stopping the
`llama-swap` Docker container, and reporting system state through a
deterministic state machine. It is designed for a trusted local network and
orchestrates hardware and containers — it does not proxy inference requests.

## Features

- **eGPU Power Control** — power the eGPU on/off via the Shelly Plug Gen3 local HTTP RPC API ([001-mvp-core](../stories/001-mvp-core/story.md))
- **Inference Backend Lifecycle** — start and stop the `llama-swap` Docker container via the Docker CLI ([001-mvp-core](../stories/001-mvp-core/story.md))
- **System State Machine** — deterministic Off / Starting / Ready / ShuttingDown / Error state machine with asynchronous transitions and single-transition concurrency ([001-mvp-core](../stories/001-mvp-core/story.md))
- **REST API** — HTTP endpoints for status, power on/off, restart, and health ([001-mvp-core](../stories/001-mvp-core/story.md))
- **GPU Monitoring** — detect GPU availability and name via `nvidia-smi` ([001-mvp-core](../stories/001-mvp-core/story.md))
- **Health Monitoring** — check llama-swap health through its `/v1/models` REST endpoint ([001-mvp-core](../stories/001-mvp-core/story.md))
- **README** — concise project overview with quick start, API summary, and links to detailed docs ([002-add-readme](../stories/002-add-readme/story.md))
- **Swagger UI** — explore the REST API interactively via Swagger UI served at `/docs`, backed by an embedded OpenAPI 3.0 spec at `/openapi.json` ([003-add-swagger-ui](../stories/003-add-swagger-ui/story.md))
- **Web UI** — responsive mobile-first control panel served at `/`, polling `/status` once per second, with power on/off and restart controls ([004-web-ui](../stories/004-web-ui/story.md))

## Non-Goals

- Proxy OpenAI-compatible inference requests.
- Manage or automatically load models.
- Authenticate users.
- Schedule GPU usage.
- Automatically power down after idle.
- Support multiple GPUs or multiple inference servers.
- Background polling or push updates (WebSocket/SSE). Status is checked live only on request.
- Prometheus metrics, or request queuing during startup.

## Known Limitations

- No background monitoring — `GET /status` queries all dependencies live on each call, so response latency scales with hardware/network round-trips.
- No authentication on the REST API; intended for a trusted local network only.
- Docker integration uses the CLI (subprocess), not the Docker Engine API.
- Error recovery requires manual intervention via `POST /power/off`; there is no automatic retry. If the Shelly plug is unreachable, the system cannot leave the Error state.
- Single GPU, single inference server, and single Shelly plug only.
