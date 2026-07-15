# DockMind

DockMind is a lightweight daemon that manages the lifecycle of an AI inference
server running on an external GPU (eGPU). It provides a simple HTTP API for
powering the eGPU on/off via a Shelly Plug Gen3, starting and stopping the
`llama-swap` Docker container, and reporting system state through a
deterministic state machine. It is designed for a trusted local network and
orchestrates hardware and containers. An opt-in OpenAI-compatible gateway mode
provides automatic startup on first request and idle auto-shutdown; see the
[Gateway Design](DockMind_Gateway_Design.md) document for architecture details.

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
- **Favicon & Logo** — SVG favicon served at `/favicon.svg` and displayed as a logo next to the page title in the web UI; the logo links to a configurable URL via the `LOGO_LINK_URL` environment variable ([006-add-favicon-logo](../stories/006-add-favicon-logo/story.md))
- **OpenAI Gateway (design)** — design document for an opt-in OpenAI-compatible reverse proxy with automatic startup on first request and idle auto-shutdown ([007-openai-gateway](../stories/007-openai-gateway/story.md))
- **OpenAI Gateway** — opt-in OpenAI-compatible reverse proxy with automatic startup on first request, model-list caching, and idle auto-shutdown ([008-openai-gateway](../stories/008-openai-gateway/story.md))
- **Model Cache Persistence** — persist the gateway's cached model list to disk so it survives DockMind restarts; configurable via `gateway.modelsCacheDir` ([010-cache-models-json](../stories/010-cache-models-json/story.md))
- **Cooldown Protection** — configurable cooldown period (`power.cooldown`, default 0s = disabled) that blocks rapid power cycling: after a shutdown, power-on is blocked for the cooldown duration, and after a startup, power-off is blocked similarly. Blocked requests return 429; `GET /status` reports remaining cooldown time via `cooldownRemaining` ([011-cooldown-protection](../stories/011-cooldown-protection/story.md))
- **eGPU Driver Unbind** — unbinds the NVIDIA driver from the eGPU via `dockmind-egpu-unbind.service` before cutting Shelly power during shutdown, preventing the NVIDIA driver error loop and host hang that occur when power is cut while the driver is still bound. A failed unbind aborts the shutdown to the Error state ([012-egpu-unbind-shutdown](../stories/012-egpu-unbind-shutdown/story.md))

## Non-Goals

- Manage or automatically load models.
- Authenticate users.
- Schedule GPU usage.
- Support multiple GPUs or multiple inference servers.
- Background polling or push updates (WebSocket/SSE). Status is checked live only on request.
- Prometheus metrics, or request queuing during startup.

## Known Limitations

- No background monitoring — `GET /status` queries all dependencies live on each call, so response latency scales with hardware/network round-trips.
- No authentication on the REST API; intended for a trusted local network only.
- Docker integration uses the CLI (subprocess), not the Docker Engine API.
- Error recovery requires manual intervention via `POST /power/off`; there is no automatic retry. If the Shelly plug is unreachable, the system cannot leave the Error state.
- Single GPU, single inference server, and single Shelly plug only.
- The OpenAI gateway is opt-in and disabled by default. Enable it via `gateway.enabled: true` in config.yaml. The model-list cache persists to disk only when `gateway.modelsCacheDir` is configured; otherwise it remains in-memory only and a warning is logged on startup. See [DockMind_Gateway_Design.md](DockMind_Gateway_Design.md) for the design and architecture details.
