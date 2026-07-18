# AGENTS.md

Guidance for OpenCode agents working in this repository.

## Commands

```bash
make build      # go build -o dockmind ./cmd/dockmind  (produces ./dockmind)
make test       # go test ./...
make lint       # gofmt -l . && go vet ./...   (BOTH must pass — not just go vet)
```

Run a single test or package:

```bash
go test ./internal/state/ -run TestPowerOnFromOff -v
go test ./internal/state/
```

Toolchain: Go 1.24.4, module `github.com/dockmind/dockmind`. One external dependency (`gopkg.in/yaml.v3`, config parsing only). No web framework, no test libraries — stdlib `net/http`, `log/slog`, `testing`, `net/http/httptest` throughout.

## README is test-enforced

`readme_test.go` (repo root, `package dockmind_test`) validates `README.md` content. Any README edit must keep `make test` green:

- Must contain: `make build`, `make test`, `make lint`, `--config`, `./config.yaml`, all five routes (`/status`, `/power/on`, `/power/off`, `/restart`, `/health`), all seven `StatusResponse` field names, and links to `docs/DockMind_MVP_Specification.md` + `docs/product.md`.
- Must NOT contain the internal enum names `ResultAlreadyInState` or `ResultConflict`, and must have no License section.
- The first fenced ```yaml block must load successfully via `config.Load` (required fields present).

The root-level test package imports `internal/config` — allowed because the repo root is the parent of `internal/`.

## Architecture

`cmd/dockmind/main.go` wires real clients into the state machine and HTTP server.

`internal/state` is the core. It defines four interfaces (`PowerController`, `GPUMonitor`, `ContainerController`, `HealthChecker`) that `internal/{shelly,gpu,docker,health}` satisfy. `internal/api` defines its own `StateMachine` interface so handler tests use a fake, decoupled from the real state machine.

State machine concurrency: `transitionMu` is acquired with `TryLock()` and held for the entire async transition — ownership is passed to the goroutine, which `defer`s the unlock. Never release it in the synchronous path. `stateMu` guards only the `state`/`lastError` fields briefly. A `sync.WaitGroup` backs `Machine.Wait()`; tests call it to block deterministically until an async transition finishes.

`gpu` and `docker` wrap command execution behind an injectable `execFunc` so tests never shell out. `nvidia-smi` and `docker` are not installed on the dev host — every test uses fakes.

## Testing conventions

- Stdlib only. No testify or mocking libraries — fakes are hand-written in each `_test.go`.
- Table-driven tests are the norm.
- `shelly`/`health`: test via `httptest.NewServer`.
- `gpu`/`docker`: test by injecting a fake `execFunc`.
- `state`: inject fakes for all four interfaces; call `m.Wait()` before asserting final state.
- `api`: `httptest.NewRecorder` with a fake `StateMachine`.

## API / state-machine conventions (non-default)

- HTTP status mapping: `202` = transition initiated (async), `200` = already in target state (idempotent no-op), `409` = conflict (transition in progress or not allowed), `429` = cooldown active (power transition blocked by recent power cycle). POST endpoints return an empty body.
- `GET /health` always returns 200 — daemon is up, not GPU readiness.
- From the `Error` state, only `POST /power/off` is accepted; `/power/on` and `/restart` return 409.
- `POST /restart` is a single atomic async operation (shutdown then startup) holding `transitionMu` for the full duration — not two sequential calls.
- No background polling: `GET /status` queries all four dependencies live on every call. Unreachable dependencies default to safe values (`false`/empty); `state` always reflects the state machine. `GET /status` also reports `idleRemaining` (seconds before an idle auto-shutdown; 0 when not applicable) alongside `cooldownRemaining`.
- `gpu.pollInterval` is reused as the polling interval for all transition wait loops, not just `/status`.
- The shutdown sequence unbinds the eGPU driver via `dockmind-egpu-unbind.service` before cutting Shelly power. If the unbind fails, the machine enters the Error state and Shelly power is not cut.

## Config

Loaded via `--config` (default `./config.yaml`). Required: `shelly.address`, `docker.container`, `llamaSwap.healthUrl`. Optional fields get defaults (`server.address=:8080`, `gpu.pollInterval=1s`, `startup.timeout=60s`, `shutdown.timeout=30s`). Durations use a custom `Duration` type with `UnmarshalYAML` — yaml.v3 does not parse `time.Duration` natively.

## Workflow (peck / story-driven)

- Features are planned as stories under `stories/NNN-name/story.md` via the `peck` CLI. Default branch is `master`.
- Reviewer reports (`peck code-review`, `peck acceptance-review`) are committed to the branch as commits — expect them in `git log`.
- `.opencode/` is workspace config and is gitignored — never commit it.
- Acceptance tests must assert the exact contract in the story's AC matrix, not the implementation's natural behavior.
- Code review expects `context.Context` propagation, bounded HTTP client timeouts, and error logging (not swallowing) by default.

## Key docs

- `docs/DockMind_MVP_Specification.md` — full request-response matrix, state-machine transitions, config schema.
- `docs/learnings.md` — hard-earned dev journal (concurrency pitfalls, review expectations).
