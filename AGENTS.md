# AGENTS.md

Guidance for OpenCode agents working in this repository.

## Commands

```bash
make build      # go build -o dockmind ./cmd/dockmind  (produces ./dockmind)
make test       # go test ./...
make lint       # gofmt -l . && go vet ./...
```

`gofmt -l .` prints offending files but exits 0 — non-empty output is a lint failure even when `make lint` itself succeeds. Concurrency-sensitive changes must also pass `go test -race ./...`.

Run a single test or package:

```bash
go test ./internal/state/ -run TestPowerOnFromOff -v
go test ./internal/state/
```

Toolchain: Go 1.24.4, module `github.com/dockmind/dockmind`. One external dependency (`gopkg.in/yaml.v3`, config parsing only). No web framework, no test libraries — stdlib `net/http`, `log/slog`, `testing`, `net/http/httptest` throughout.

## Docs are test-enforced

Four root-level tests (`package dockmind_test`) validate prose files — any feature or doc edit must keep them green:

- `readme_test.go` — pins `README.md` content: routes, every `StatusResponse` JSON field name, config keys (`gateway`, `modelsCacheDir`, `auxContainers`, `disableIdleShutdown`, `unloadLlamaSwap`), `LOGO_LINK_URL`, feature phrases. The case list in the test is the source of truth — read it before editing the README. Must NOT contain `ResultAlreadyInState`/`ResultConflict` and must have no License section; the first fenced ```yaml block must load via `config.Load`.
- `product_test.go` — `docs/product.md` must reference each shipped feature by story slug; when a story ships a user-facing feature, add the feature to `docs/product.md` AND its slug assertion to `product_test.go`.
- `gateway_design_test.go` — pins required sections/decision keywords in `docs/DockMind_Gateway_Design.md`.
- `configs_test.go` — `configs/config-with-gateway.yaml` must load via `config.Load`.

## Architecture

`cmd/dockmind/main.go` wires real clients into the state machine, HTTP server, and (when `gateway.enabled`) the gateway.

`internal/state` is the core. It defines interfaces for every collaborator (`PowerController`, `GPUMonitor`, `ContainerController`, `HealthChecker`, `Unbinder`, `ModelUnloader`, `AuxContainerController`, `LactController`) that `internal/{shelly,gpu,docker,health,unbind,lact}` satisfy. `state` never imports concrete internal packages — new optional features must be wired via an interface defined in `state` plus a `Set*` setter (`SetAuxContainers`, `SetModelUnloader`, `SetLactClient`), not a concrete pointer. `internal/api` defines its own `StateMachine` interface so handler tests use a fake, decoupled from the real state machine.

`internal/gateway` is an opt-in OpenAI-compatible reverse proxy (auto-start via `EnsureReady`, idle auto-shutdown, disk-persisted `/v1/models` cache). It depends on its own `StateController` interface (`State`/`PowerOff`/`EnsureReady`/`IdleShutdownBlocked`) — same decoupling rule. It attaches to `api.Server` only through `SetGatewayHandlers`/`SetIdleReporter`/`SetIdleShutdownController`; all nil when the gateway is disabled.

Web UI: `internal/api/{index,docs}.html`, `openapi.json`, `favicon.svg` are `go:embed` assets served by `api`. The `LOGO_LINK_URL` env var wraps the logo in a link at server construction.

State machine concurrency: `transitionMu` is acquired with `TryLock()` and held for the entire async transition — ownership is passed to the goroutine, which `defer`s the unlock. Never release it in the synchronous path. `stateMu` guards only the `state`/`lastError` fields briefly. A `sync.WaitGroup` backs `Machine.Wait()`; tests call it to block deterministically until an async transition finishes. Gateway lock discipline (release mutex before slow probes, reset state under lock before flipping gating atomics) is documented in `docs/learnings.md`.

`gpu` and `docker` wrap command execution behind an injectable `execFunc` so tests never shell out. `nvidia-smi` and `docker` are not installed on the dev host — every test uses fakes.

`service/` and `configs/` hold deployment artifacts (systemd units, udev rule, sudoers), not Go code.

## Testing conventions

- Stdlib only. No testify or mocking libraries — fakes are hand-written in each `_test.go`.
- Table-driven tests are the norm. Extend an existing table-driven test for a new case rather than adding a parallel test function that re-covers the same concern — code review flags the duplication.
- `cmd/dockmind/main.go` wiring is verified behaviorally through the seams (real `Gateway`/clients against an `httptest` backend + interface fakes, asserting endpoint status and `/status` reflection) — code review rejects source-text-scan tests of `main.go`.
- `shelly`/`health`: test via `httptest.NewServer`.
- `gpu`/`docker`: test by injecting a fake `execFunc`.
- `state`: inject fakes for all interfaces; call `m.Wait()` before asserting final state.
- `gateway`: fake `StateController` + `httptest` backend.
- `api`: `httptest.NewRecorder` with a fake `StateMachine`.
- Web UI behavior: assert exact JS source strings in the served HTML — no JS test runner, no browser. Never execute the page logic.

## API / state-machine conventions (non-default)

- HTTP status mapping: `202` = transition initiated (async), `200` = already in target state (idempotent no-op), `409` = conflict (transition in progress or not allowed), `429` = cooldown active (power transition blocked by recent power cycle). POST endpoints return an empty body.
- Routes beyond the power set: `POST /containers/{name}/start|stop` (200/404/409/500), `POST /idle-shutdown/enable|disable` (409 when no gateway idle timeout exists), `GET /docs`, `/openapi.json`, `/favicon.svg`, `GET /{$}` (web UI). `GET /v1/models` and `/v1/{rest...}` are registered only when the gateway is enabled.
- `GET /health` always returns 200 — daemon is up, not GPU readiness.
- From the `Error` state, only `POST /power/off` is accepted; `/power/on` and `/restart` return 409.
- `POST /restart` is a single atomic async operation (shutdown then startup) holding `transitionMu` for the full duration — not two sequential calls.
- No background polling: `GET /status` queries dependencies live on every call. Unreachable dependencies default to safe values (`false`/empty); `state` always reflects the state machine. `/status` reports `cooldownRemaining`, `idleRemaining`, and the `idleShutdownEnabled`/`idleShutdownAvailable`/`idleShutdownBlocked` toggle fields.
- `gpu.pollInterval` is reused as the polling interval for all transition wait loops, not just `/status`.
- The shutdown sequence stops `llama-swap`, then checks `nvidia-smi` for compute processes still using the GPU. If any are found, the machine enters `AwaitingGPUFree` and polls every `shutdown.gpuFreeCheckInterval` (default 5m) until the GPU is clear, then unbinds the eGPU driver via `dockmind-egpu-unbind.service` and cuts Shelly power. If the unbind fails, the machine enters the Error state and Shelly power is not cut.
- From `AwaitingGPUFree`, `POST /power/on` and `POST /restart` resume startup (202 Accepted); `POST /power/off` is an idempotent no-op (200 OK) because the system is already shutting down.

## Config

Loaded via `--config` (default `./config.yaml`). Required: `shelly.address`, `docker.container`, `llamaSwap.healthUrl`. `llamaSwap.backendUrl` is required when `gateway.enabled` or when any aux container sets `unloadLlamaSwap: true`. Optional fields get defaults (`server.address=:8080`, `gpu.pollInterval=1s`, `startup.timeout=60s`, `shutdown.timeout=30s`, `shutdown.gpuFreeCheckInterval=5m`, `gateway.requestTimeout=120s`, `gateway.modelsRefreshInterval=60s`). `config.EffectiveIdleTimeout` silently raises `gateway.idleTimeout` to at least `power.cooldown`. Durations use a custom `Duration` type with `UnmarshalYAML` — yaml.v3 does not parse `time.Duration` natively. Adding slice/map fields to `Config` breaks struct-equality tests — see `docs/learnings.md`.

## Workflow (peck / story-driven)

- Features are planned as stories under `stories/NNN-name/story.md` via the `peck` CLI. Default branch is `master`.
- Reviewer reports (`peck code-review`, `peck acceptance-review`) are committed to the branch as commits — expect them in `git log`.
- `.opencode/` is workspace config and is gitignored — never commit it.
- Acceptance tests must assert the exact contract in the story's AC matrix, not the implementation's natural behavior.
- A story's "Tests to extend" list (Technical Context) is a mandatory checklist — verify every listed test location was actually extended before the first reviewer run. Editing one anchor list while a sibling one in the same file is listed separately is an easy miss (story 031).
- Code review expects `context.Context` propagation, bounded HTTP client timeouts, and error logging (not swallowing) by default.

## Key docs

- `docs/DockMind_MVP_Specification.md` — full request-response matrix, state-machine transitions, config schema.
- `docs/DockMind_Gateway_Design.md` — gateway design (its content is pinned by `gateway_design_test.go`).
- `docs/product.md` — product feature list (pinned by `product_test.go`).
- `docs/learnings.md` — hard-earned dev journal (concurrency pitfalls, review expectations). Read before touching state-machine or gateway concurrency.
