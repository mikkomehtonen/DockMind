# DockMind MVP — Core Daemon

## Context

DockMind is a greenfield daemon that manages the lifecycle of an AI inference
server running on an external GPU (eGPU). It exposes a simple HTTP API for
powering the eGPU on/off (via a Shelly Plug Gen3), starting/stopping the
`llama-swap` Docker container, and reporting system state through a
deterministic state machine. The MVP is intentionally small: it orchestrates
hardware and containers, it does not proxy inference requests. This story
builds the entire MVP from scratch — there is no existing code.

## Out of Scope

- Proxying OpenAI-compatible inference requests.
- Model management or automatic model loading.
- User authentication or authorization.
- GPU scheduling or automatic power-down after idle.
- Multiple GPUs, multiple inference servers, or multiple Shelly plugs.
- Background polling / push updates (WebSocket, SSE). Status is checked live
  only when `GET /status` is called.
- Web UI, Prometheus metrics, request queuing during startup.
- Integration with OpenWebUI / Hermes.

## Implementation approach

### Language, module, and dependencies

- **Go 1.24** (1.24.4 installed). Module path: `github.com/dockmind/dockmind`.
- **stdlib only** plus one external dependency: `gopkg.in/yaml.v3 v3.0.1` for
  config parsing (latest stable, confirmed via `proxy.golang.org`).
- HTTP server: stdlib `net/http` with `http.ServeMux` — no web framework, no
  external router.
- Logging: stdlib `log/slog` with a `TextHandler` (structured, human-readable).
- Testing: stdlib `testing` + `net/http/httptest`. No external test libraries.

### Package structure (per MVP spec)

```
cmd/dockmind/        # main entry point
internal/api/        # HTTP handlers
internal/config/     # config.yaml loading
internal/docker/     # Docker CLI wrapper
internal/gpu/        # nvidia-smi wrapper
internal/health/     # llama-swap health checker
internal/shelly/     # Shelly Plug Gen3 RPC client
internal/state/      # state machine
configs/             # sample config.yaml
```

### Interfaces (testability)

The `state` package defines interfaces that the real `shelly`, `gpu`, `docker`,
and `health` packages satisfy. Tests inject fakes — no real hardware, Docker,
or network needed.

```go
// state package
type PowerController interface {
    SetPower(on bool) error
    IsOn() (bool, error)
}
type GPUMonitor interface {
    Status() (present bool, name string, err error)
}
type ContainerController interface {
    Start() error
    Stop() error
    IsRunning() (bool, error)
}
type HealthChecker interface {
    Check() (healthy bool, err error)
}
```

The `api` package defines an interface for the state machine so API handler
tests use a fake, decoupled from the real state machine:

```go
// api package
type StateMachine interface {
    Status() state.StatusResponse
    PowerOn() state.PowerResult
    PowerOff() state.PowerResult
    Restart() state.PowerResult
}
```

The concrete `state.StateMachine` additionally exposes a `Wait()` method
(blocks until the current async transition completes) for use by state-machine
tests. `Wait()` is not part of the `api.StateMachine` interface.

### External command abstraction

The `gpu` and `docker` packages wrap command execution behind an injectable
function so tests do not shell out:

```go
type execFunc func(ctx context.Context, name string, args ...string) ([]byte, error)
```

The default implementation calls `exec.CommandContext(...).Output()`. Tests
inject a fake returning canned stdout/stderr/exit behavior. Docker commands
use `exec.Command("docker", "start", name)` — args are passed directly, never
through a shell, so the configured container name cannot cause command
injection.

### State machine concurrency model

Two mutexes:

- `transitionMu sync.Mutex` — held for the entire duration of any transition
  (startup, shutdown, or restart's combined off+on). Acquired with
  `TryLock()`. If `TryLock()` fails, a transition is already running → every
  power/restart request returns `ResultConflict` (HTTP 409).
- `stateMu sync.Mutex` — protects the `state` and `lastError` fields for
  reads/writes. Held only briefly (never during polling or network calls).

A `sync.WaitGroup` tracks the current transition goroutine. `Wait()` calls
`wg.Wait()` so tests can block deterministically until a transition finishes.
`wg.Add(1)` is called in the synchronous part of `PowerOn`/`PowerOff`/
`Restart` before the goroutine launches; the goroutine calls `wg.Done()` on
completion.

### PowerResult enum (HTTP-agnostic)

The state machine returns a result enum; the API layer maps it to HTTP codes.
This keeps HTTP logic out of the state machine.

```go
type PowerResult int
const (
    ResultAccepted       PowerResult = iota // transition initiated (async)
    ResultAlreadyInState                    // no-op, already in target state
    ResultConflict                          // transition in progress or not allowed
)
```

API mapping: `ResultAccepted → 202`, `ResultAlreadyInState → 200`,
`ResultConflict → 409`.

### Request response matrix

**POST /power/on**

| Current state | Result       | HTTP | Action              |
|---------------|--------------|------|---------------------|
| Off           | Accepted     | 202  | async startup       |
| Starting      | Conflict     | 409  | none                |
| Ready         | AlreadyInState | 200 | none                |
| ShuttingDown  | Conflict     | 409  | none                |
| Error         | Conflict     | 409  | none (must off first) |

**POST /power/off**

| Current state | Result       | HTTP | Action              |
|---------------|--------------|------|---------------------|
| Off           | AlreadyInState | 200 | none                |
| Starting      | Conflict     | 409  | none                |
| Ready         | Accepted     | 202  | async shutdown      |
| ShuttingDown  | Conflict     | 409  | none                |
| Error         | Accepted     | 202  | async shutdown (clean power off) |

**POST /restart** (single atomic async operation: shutdown then startup)

| Current state | Result   | HTTP | Action                              |
|---------------|----------|------|-------------------------------------|
| Off           | Accepted | 202  | async startup (off is a no-op)      |
| Starting      | Conflict | 409  | none                                |
| Ready         | Accepted | 202  | async shutdown then startup         |
| ShuttingDown  | Conflict | 409  | none                                |
| Error         | Conflict | 409  | none (must off first)               |

### Startup sequence (async, from Off)

Each step runs under a `context.Context` with deadline = `startup.timeout`
from the moment the sequence starts. `gpu.pollInterval` is the polling
interval for all wait loops.

1. `Shelly.SetPower(true)` — on error → Error (lastError set).
2. Poll `GPU.Status()` at `gpu.pollInterval` until `present == true` or
   deadline exceeded. On timeout → Error (lastError mentions GPU/timeout).
3. `Docker.Start()` — on error → Error (lastError set).
4. Poll `Health.Check()` at `gpu.pollInterval` until `healthy == true` or
   deadline exceeded. On timeout → Error (lastError mentions health/timeout).
5. Set state = Ready, lastError = nil.

### Shutdown sequence (async, from Ready or Error)

`context.Context` deadline = `shutdown.timeout`.

1. `Docker.Stop()` — if the error indicates "No such container" (container
   already absent), treat as success. Any other error → Error (lastError set).
2. Poll `Docker.IsRunning()` at `gpu.pollInterval` until `false` or deadline
   exceeded. On timeout → Error.
3. `Shelly.SetPower(false)` — on error → Error (lastError set). The system
   stays in Error and cannot exit until Shelly is reachable and the power-off
   succeeds (per user requirement).
4. Poll `GPU.Status()` at `gpu.pollInterval` until `present == false` or
   deadline exceeded. On timeout → Error.
5. Set state = Off, lastError = nil.

### Error state exit rule

From Error, only `POST /power/off` is accepted (runs the shutdown sequence).
`POST /power/on` and `POST /restart` return `ResultConflict` (409). If the
shutdown sequence fails (e.g. Shelly unreachable at step 3), the system
remains in Error with an updated `lastError`. The user must retry
`/power/off` until Shelly responds and the sequence completes → Off.

### GET /status (live, no background polling)

`Status()` reads `state` + `lastError` under `stateMu` (quick), then queries
all four dependencies live (no caching, no background goroutine):

- GPU: `nvidia-smi --query-gpu=name --format=csv,noheader`. Exit 0 +
  non-empty stdout → `gpuPresent=true`, `gpuName` = `strings.TrimSpace` of
  the first stdout line. Any failure (non-zero exit, empty stdout, binary not
  found) → `gpuPresent=false`, `gpuName=""`. Stderr is discarded.
- Shelly: `Switch.GetStatus` → parse `output` bool. On any error →
  `shellyOn=false`.
- Docker: `docker inspect --format '{{.State.Running}}' <container>` → parse
  `true`/`false`. On any error → `llamaSwapRunning=false`.
- Health: `GET <healthUrl>` → HTTP 200 = `llamaSwapHealthy=true`. Non-200 or
  network error → `llamaSwapHealthy=false`.

```go
type StatusResponse struct {
    State            string  `json:"state"`
    GPUPresent       bool    `json:"gpuPresent"`
    GPUName          string  `json:"gpuName"`
    ShellyOn         bool    `json:"shellyOn"`
    LlamaSwapRunning bool    `json:"llamaSwapRunning"`
    LlamaSwapHealthy bool    `json:"llamaSwapHealthy"`
    LastError        *string `json:"lastError"` // nil → JSON null
}
```

`LastError` is `*string`: `nil` (→ `null`) when not in Error; `&msg` when in
Error. Cleared to `nil` on successful transition to Off or Ready.

### Configuration

`config.yaml` loaded via a `--config` flag (default `./config.yaml`). A
custom `Duration` type wraps `time.Duration` and implements
`yaml.Unmarshaler` (parses values like `1s`, `60s` via `time.ParseDuration`).

Required fields (error if empty/missing after unmarshal): `shelly.address`,
`docker.container`, `llamaSwap.healthUrl`.

Optional fields with defaults applied after unmarshal when zero-valued:

| Field               | Default   |
|---------------------|-----------|
| `server.address`    | `:8080`   |
| `shelly.channel`    | `0`       |
| `gpu.pollInterval`  | `1s`      |
| `startup.timeout`   | `60s`     |
| `shutdown.timeout`  | `30s`     |

Sample `configs/config.yaml`:

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

### HTTP API routes

| Method | Path         | Handler behavior                                      |
|--------|--------------|-------------------------------------------------------|
| GET    | /status      | 200 + StatusResponse JSON                             |
| POST   | /power/on    | maps PowerResult → 202/200/409, empty body            |
| POST   | /power/off   | maps PowerResult → 202/200/409, empty body            |
| POST   | /restart     | maps PowerResult → 202/200/409, empty body            |
| GET    | /health      | always 200, empty body (DockMind operational)         |
| *      | any other    | 404 for unknown path; 405 for wrong method on a known path |

Use Go 1.22+ `ServeMux` method-prefixed patterns (e.g.
`mux.HandleFunc("POST /power/on", h)`) so the mux natively returns 405 for a
wrong method on a registered path and 404 for an unregistered path. Verified
on Go 1.24.

### Main entry point (cmd/dockmind)

1. Parse `--config` flag (default `./config.yaml`).
2. Load config.
3. Construct real `shelly`, `gpu`, `docker`, `health` clients from config.
4. Construct `state.StateMachine` with clients + config timeouts + `slog` logger.
5. Construct `api.Server` with the state machine.
6. Start `http.Server` on `config.server.address`.
7. Wait for SIGINT/SIGTERM, then gracefully shut down the HTTP server
   (`Shutdown(ctx)`). Do **not** power off the eGPU or stop llama-swap.

## Tasks

### Task 1 - Project scaffold & config package

1. valid full config.yaml (all fields) + Load
   - → Config struct populated: server.address=":8080", shelly.address="192.168.1.50", shelly.channel=0, docker.container="llama-swap", llamaSwap.healthUrl="http://localhost:1234/v1/models", gpu.pollInterval=1s, startup.timeout=60s, shutdown.timeout=30s
2. valid minimal config.yaml (only shelly.address, docker.container, llamaSwap.healthUrl) + Load
   - → defaults applied: server.address=":8080", shelly.channel=0, gpu.pollInterval=1s, startup.timeout=60s, shutdown.timeout=30s
3. config file does not exist + Load
   - → returns error
4. malformed YAML + Load
   - → returns error
5. custom path via --config flag + Load
   - → loads from the specified path, not the default
6. invalid duration value (gpu.pollInterval: "abc") + Load
   - → returns error
7. missing required field (shelly.address omitted) + Load
   - → returns error

### Task 2 - Shelly Plug Gen3 client

1. SetPower(true) + Shelly responds HTTP 200
   - → sends GET to /rpc/Switch.Set?id=0&on=true
   - → returns nil error
2. SetPower(false) + Shelly responds HTTP 200
   - → sends GET to /rpc/Switch.Set?id=0&on=false
   - → returns nil error
3. IsOn() + Shelly responds {"output": true}
   - → returns (true, nil)
4. IsOn() + Shelly responds {"output": false}
   - → returns (false, nil)
5. SetPower(true) + Shelly unreachable (connection refused)
   - → returns error
6. IsOn() + Shelly responds non-200
   - → returns (false, error)
7. channel=1 configured + SetPower(true)
   - → request URL path/query contains id=1

### Task 3 - GPU monitor (nvidia-smi wrapper)

1. nvidia-smi exits 0, stdout = "NVIDIA GeForce RTX 5060 Ti"
   - → Status() returns (true, "NVIDIA GeForce RTX 5060 Ti", nil)
2. nvidia-smi exits 0, stdout = "NVIDIA GeForce RTX 5060 Ti\nNVIDIA GeForce RTX 4090"
   - → Status() returns (true, "NVIDIA GeForce RTX 5060 Ti", nil) (first line only)
3. nvidia-smi exits 0, stdout = "" (empty)
   - → Status() returns (false, "", nil)
4. nvidia-smi binary not found (exec error)
   - → Status() returns (false, "", nil)
5. nvidia-smi exits non-zero
   - → Status() returns (false, "", nil)

### Task 4 - Docker CLI client

1. Start() + docker start exits 0
   - → returns nil error
   - → command executed is "docker start <container>"
2. Stop() + docker stop exits 0
   - → returns nil error
   - → command executed is "docker stop <container>"
3. Stop() + docker exits non-zero with "No such container" in stderr
   - → returns nil error (container already in desired stopped state)
4. IsRunning() + docker inspect stdout = "true"
   - → returns (true, nil)
5. IsRunning() + docker inspect stdout = "false"
   - → returns (false, nil)
6. IsRunning() + docker inspect exits non-zero (container not found)
   - → returns (false, nil)
7. Start() + docker start exits non-zero (error other than "No such container")
   - → returns error
8. custom container name "my-runner" configured
   - → Start/Stop/IsRunning commands use "my-runner" as the container argument

### Task 5 - llama-swap health checker

1. Check() + health endpoint returns HTTP 200
   - → returns (true, nil)
2. Check() + health endpoint returns HTTP 500
   - → returns (false, nil)
3. Check() + health endpoint returns HTTP 404
   - → returns (false, nil)
4. Check() + health endpoint unreachable (connection refused)
   - → returns (false, error)

### Task 6 - State machine

1. state=Off + PowerOn() (all deps succeed)
   - → returns ResultAccepted
   - → after Wait(), state=Ready and lastError=nil
2. state=Ready + PowerOn()
   - → returns ResultAlreadyInState
   - → state stays Ready
3. state=Error + PowerOn()
   - → returns ResultConflict
4. state=Off + PowerOff()
   - → returns ResultAlreadyInState
   - → state stays Off
5. state=Ready + PowerOff() (all deps succeed)
   - → returns ResultAccepted
   - → after Wait(), state=Off and lastError=nil
6. state=Error + PowerOff() (all deps succeed)
   - → returns ResultAccepted
   - → after Wait(), state=Off and lastError=nil (clean power off)
7. state=Off + Restart() (all deps succeed)
   - → returns ResultAccepted
   - → after Wait(), state=Ready
8. state=Ready + Restart() (all deps succeed)
   - → returns ResultAccepted
   - → after Wait(), state=Ready (went Ready→ShuttingDown→Off→Starting→Ready)
9. state=Error + Restart()
   - → returns ResultConflict
10. startup + Shelly.SetPower(true) returns error
    - → after Wait(), state=Error and lastError contains the error reason
11. startup + GPU.Status() always returns present=false (timeout)
    - → after Wait(), state=Error and lastError mentions timeout or GPU
12. startup + Docker.Start() returns error
    - → after Wait(), state=Error and lastError contains the error reason
13. startup + Health.Check() always returns healthy=false (timeout)
    - → after Wait(), state=Error and lastError mentions timeout or health
14. shutdown + Docker.Stop() returns error (not "No such container")
    - → after Wait(), state=Error and lastError contains the error reason
15. shutdown + Shelly.SetPower(false) returns error (Shelly unreachable)
    - → after Wait(), state=Error and lastError contains the error reason
16. shutdown + GPU.Status() always returns present=true (timeout)
    - → after Wait(), state=Error and lastError mentions timeout or GPU
17. startup transition running + concurrent PowerOn(), PowerOff(), Restart()
    - → all three return ResultConflict
18. shutdown transition running + concurrent PowerOn(), PowerOff(), Restart()
    - → all three return ResultConflict
19. state=Ready + Status() (all deps report healthy/running/present)
    - → returns State="Ready", gpuPresent=true, gpuName non-empty, shellyOn=true, llamaSwapRunning=true, llamaSwapHealthy=true, lastError=null
20. state=Error + Status()
    - → returns State="Error" and lastError is a non-nil string
21. state=Off + Status() (all deps report off/unavailable)
    - → returns State="Off", gpuPresent=false, gpuName="", shellyOn=false, llamaSwapRunning=false, llamaSwapHealthy=false, lastError=null

### Task 7 - HTTP API & main entry point

1. GET /status + state machine returns Ready status
   - → HTTP 200
   - → JSON body contains "state":"Ready"
2. GET /status + state machine returns Off status
   - → HTTP 200
   - → JSON body contains "state":"Off"
3. POST /power/on + state machine returns ResultAccepted
   - → HTTP 202
4. POST /power/on + state machine returns ResultAlreadyInState
   - → HTTP 200
5. POST /power/on + state machine returns ResultConflict
   - → HTTP 409
6. POST /power/off + state machine returns ResultAccepted
   - → HTTP 202
7. POST /power/off + state machine returns ResultAlreadyInState
   - → HTTP 200
8. POST /restart + state machine returns ResultAccepted
   - → HTTP 202
9. POST /restart + state machine returns ResultConflict
   - → HTTP 409
10. GET /health (any state)
    - → HTTP 200
    - → response body is empty
11. GET /foo (unknown path)
    - → HTTP 404
12. GET /power/on (wrong method on a known path)
    - → HTTP 405
13. GET /power/off (wrong method on a known path)
    - → HTTP 405

## Bootstrap

Run from the repository root (`/app`):

```bash
# 1. Initialize Go module
go mod init github.com/dockmind/dockmind

# 2. Add the single external dependency
go get gopkg.in/yaml.v3@v3.0.1

# 3. Create the directory structure from the MVP spec
mkdir -p cmd/dockmind internal/api internal/config internal/docker internal/gpu internal/health internal/shelly internal/state configs
```

Create a `Makefile` in the repository root (the acceptance reviewer discovers
lint/test commands from it):

```makefile
.PHONY: lint test build

lint:
	go vet ./...

test:
	go test ./...

build:
	go build ./...
```

Create `configs/config.yaml` with the content shown in the **Configuration**
section above.

After writing all source files, verify:

```bash
go build ./...
go vet ./...
go test ./...
```

## Technical Context

- **Go 1.24.4** — installed and confirmed. `log/slog` is stdlib (since Go
  1.21), no external logging library needed.
- **gopkg.in/yaml.v3 v3.0.1** — latest stable release (confirmed via
  `proxy.golang.org` `/@latest` endpoint; no newer version exists). Used only
  for config parsing. yaml.v3 does not natively parse `time.Duration`, so a
  custom `Duration` type with `UnmarshalYAML` is required.
- **Shelly Plug Gen3 RPC API** (Gen2+ platform, confirmed from official docs):
  - HTTP GET form: `http://<address>/rpc/<Method>?<param>=<value>`.
  - `Switch.Set`: `GET /rpc/Switch.Set?id=0&on=true` → HTTP 200
    `{"was_on": false}`. Params: `id` (int, required), `on` (bool, required).
  - `Switch.GetStatus`: `GET /rpc/Switch.GetStatus?id=0` → HTTP 200
    `{"id":0,"output":false,...}`. Parse the `output` boolean field.
  - No authentication required on the local LAN (cloud not required).
  - Shelly Plug Gen3 has a single switch channel (`id=0`).
- **nvidia-smi**: `nvidia-smi --query-gpu=name --format=csv,noheader`. Only
  the **exit code** and **stdout** are evaluated; stderr is discarded (the
  no-GPU error message goes to stderr, not stdout). Rules:
  - Exit 0 + non-empty stdout → GPU present; `gpuName` = first stdout line
    with `strings.TrimSpace` applied (drops the trailing `\n`).
  - Any other outcome (non-zero exit, empty stdout, binary not found) → GPU
    absent (`gpuPresent=false`, `gpuName=""`).
  - Real output confirmed by the user:
    - GPU present: stdout = `NVIDIA GeForce GTX 1080\n`, exit 0.
    - No GPU: stdout empty, exit non-zero, stderr =
      `Failed to initialize NVML: No supported GPUs were found\nUnable to
      determine the number of GPUs` (discarded — not on stdout).
  - Not installed on this dev host (exit 127) — expected; tests use an
    injectable fake.
- **Docker CLI**: `docker start <name>`, `docker stop <name>`,
  `docker inspect --format '{{.State.Running}}' <name>`. `docker stop` on an
  already-stopped container exits 0; on a non-existent container exits non-zero
  with "No such container" in stderr (treated as success). `docker inspect` on
  a non-existent container exits non-zero (treated as not running).
- **llama-swap health**: `GET <healthUrl>` (e.g.
  `http://localhost:1234/v1/models`). HTTP 200 = healthy.
- **No web framework** — stdlib `net/http` + `http.ServeMux` only.
- **Testing** — stdlib `testing` + `net/http/httptest`. Shelly and health
  clients tested via `httptest.NewServer`. GPU and Docker clients tested via
  an injectable `execFunc`. State machine tested via fake interface
  implementations. API handlers tested via `httptest.NewRecorder` with a fake
  `StateMachine`.

## Notes

- **Signal handling**: SIGINT/SIGTERM gracefully shut down only the DockMind
  HTTP server (`http.Server.Shutdown`). They do **not** power off the eGPU or
  stop llama-swap. Non-automatable — verified manually.
- **No background polling**: `GET /status` queries all dependencies live on
  each call. When a dependency is unreachable, the corresponding field
  defaults to a safe value (`gpuPresent=false`, `gpuName=""`,
  `shellyOn=false`, `llamaSwapRunning=false`, `llamaSwapHealthy=false`). The
  `state` field always reflects the state machine regardless of live-check
  failures.
- **`gpu.pollInterval`** is reused as the polling interval for all transition
  wait loops (GPU appear/disappear and llama-swap health checks during
  startup/shutdown). It does not govern `/status` (which is on-demand).
- **Error state exit**: only `POST /power/off` is accepted from Error. If
  Shelly is unreachable during the power-off attempt, the system stays in
  Error. The user must retry `/power/off` until it succeeds.
- **`POST /restart`** runs shutdown-then-startup as a single atomic async
  operation holding `transitionMu` for the full duration — it is not two
  sequential HTTP calls.
- **`GET /health`** always returns 200 OK regardless of system state. It
  indicates DockMind is operational, not GPU readiness.
- **HTTP status convention**: 200 = already in target state (idempotent
  no-op), 202 = transition initiated (async), 409 = conflict (transition in
  progress or operation not allowed from current state). POST endpoints
  return an empty body with just the status code.
- **Logging**: use `log/slog` with `TextHandler`. Log each state transition
  and each step of the startup/shutdown sequences (e.g. "Shelly power ON",
  "GPU detected", "Starting llama-swap", "State -> Ready"). Error logs must
  include the error reason. Logging format is non-automatable.
- **Table-driven tests**: use Go table-driven test patterns for similar
  scenarios (e.g. the request-response matrix, config defaults, Docker
  command construction) to avoid repetitive test functions.
- **`.gitignore`**: add the compiled binary (`/dockmind` or
  `/cmd/dockmind/dockmind`) to `.gitignore`.
