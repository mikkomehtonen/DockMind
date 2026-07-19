# Optional Docker Containers

## Context

DockMind currently manages a single Docker container (`llama-swap`) as the
inference backend. Users want to run additional containers alongside it — for
example a Kokoro text-to-speech container or a Whisper speech-to-text container.
These containers are optional, user-configured, and independently controlled: they
are not part of the power-on lifecycle (the user starts them on demand) but they
must be cleaned up during power-off so no containers are left running when the
eGPU loses power.

This feature adds configurable aux containers, surfaces their running state in
`GET /status` and the web UI, provides per-container start/stop API endpoints,
and stops all running aux containers during the shutdown sequence (Phase 1,
alongside llama-swap) before the GPU-process check and Shelly power-off.

## Out of Scope

- Auto-starting aux containers during power-on. They are user-managed and
  started on demand via the API/UI.
- Health checking for aux containers (only running/stopped status is reported).
- GPU dependency tracking for aux containers. A container that needs the GPU is
  the user's responsibility to start only when the system is Ready; the GPU
  process guard in Phase 2 will catch any lingering GPU users at shutdown.
- Restarting aux containers automatically after a system restart. The restart
  transition stops them (via shutdown) but does not start them again.
- Configurable timeout for on-demand aux start/stop (fixed 30s; see Notes).

## Implementation approach

### Config

Add a new top-level `auxContainers` list to `Config`. Each entry has a display
`name` (used as the API/UI identifier) and a `container` (the Docker container
name passed to `docker start`/`stop`/`inspect`).

```yaml
auxContainers:
  - name: kokoro
    container: kokoro-tts
  - name: whisper
    container: whisper-stt
```

Validation rules (in `config.validate`):
- The list is optional; empty/absent is valid.
- Each entry must have a non-empty `name` and non-empty `container`.
- Names must be unique (case-sensitive); duplicates are a config error.

### Docker Manager

Add a `Manager` type to `internal/docker` that wraps multiple `Client` instances
behind a single injectable `execFunc` (same test-injection pattern as the
existing `Client`). The manager holds an ordered list of `{name, container}`
specs (preserving config order for stable UI display) and a `name → *Client` map.

```go
type ContainerSpec struct {
    Name      string
    Container string
}

type Manager struct {
    specs []ContainerSpec
    ctrls map[string]*Client
    exec  execFunc
}

func NewManager(specs []ContainerSpec) *Manager
func (m *Manager) Names() []string
func (m *Manager) Start(ctx context.Context, name string) error
func (m *Manager) Stop(ctx context.Context, name string) error
func (m *Manager) IsRunning(ctx context.Context, name string) (bool, error)
func (m *Manager) StopAll(ctx context.Context) error
```

The `docker` package does NOT import `state` — it stays a leaf package. The
`Manager` satisfies the `state.AuxContainerController` interface structurally
(Go duck typing), same as the existing `Client` satisfies
`state.ContainerController`.

- `Names()` returns the configured display names in config order (immutable, no
  context needed).
- `Start`/`Stop`/`IsRunning` look up the name; unknown name returns
  `ErrUnknownContainer` (for Start/Stop) or `(false, ErrUnknownContainer)` (for
  IsRunning).
- `StopAll` calls `docker stop` on every configured container sequentially
  (fail-fast: returns the first error). Docker stop is idempotent, so
  already-stopped containers are a no-op.

`AuxContainerStatus` is defined in `internal/state` (not `docker`) since it
appears in `StatusResponse`:

```go
type AuxContainerStatus struct {
    Name    string `json:"name"`
    Running bool   `json:"running"`
}
```

The state machine builds `[]AuxContainerStatus` itself by iterating over
`Names()` and calling `IsRunning` for each — there is no `StatusAll` method on
the controller. This keeps `docker` from importing `state`.

### State Machine

New types in `internal/state`:

```go
type AuxResult int
const (
    AuxResultOK       AuxResult = iota // 200
    AuxResultNotFound                  // 404
    AuxResultConflict                  // 409
    AuxResultError                     // 500
)

type AuxContainerStatus struct {
    Name    string `json:"name"`
    Running bool   `json:"running"`
}
```

New interface (satisfied by `docker.Manager` via structural typing — `docker`
does not import `state`):

```go
type AuxContainerController interface {
    Names() []string
    Start(ctx context.Context, name string) error
    Stop(ctx context.Context, name string) error
    IsRunning(ctx context.Context, name string) (bool, error)
    StopAll(ctx context.Context) error
}
```

The `Machine` gets a new `aux AuxContainerController` field (nil when no aux
containers are configured). It is set via a setter, not a constructor parameter,
so the existing `New` signature and all existing test helpers remain unchanged:

```go
func (m *Machine) SetAuxContainers(ctrl AuxContainerController)
```

This setter is called once during initialization in `main.go` before the HTTP
server starts; it is never called concurrently with requests, so no mutex guards
the field.

**On-demand start/stop** (Q2=A — accepted in `Off` and `Ready`, rejected with
409 in all other states):

```go
func (m *Machine) StartAuxContainer(name string) AuxResult
func (m *Machine) StopAuxContainer(name string) AuxResult
```

Logic (identical for both, differing only in the docker call):
1. If `m.aux == nil` → `AuxResultNotFound`.
2. Check `name` exists in `m.aux.Names()`. If not → `AuxResultNotFound`.
3. `m.transitionMu.TryLock()`. If fail → `AuxResultConflict` (a power transition
   is in progress).
4. Under `stateMu`, read `m.state`. If state is not `Off` and not `Ready` →
   unlock and return `AuxResultConflict`.
5. Create a `context.WithTimeout(30s)`. Call `m.aux.Start(ctx, name)` (or
   `Stop`). On error, log at ERROR level and return `AuxResultError`.
6. Return `AuxResultOK`. Unlock `transitionMu` via `defer`.

These operations are **synchronous** (the HTTP request blocks until docker
completes). This is safe because `docker start`/`stop` complete in seconds and
the 30s timeout bounds the worst case. Holding `transitionMu` for the duration
ensures no power transition can start concurrently — which is the desired
behavior (Q2=A).

**Shutdown integration** (Q1=A — aux stop failure aborts to Error):

In `shutdown()`, after llama-swap stops and is confirmed stopped (end of existing
Phase 1), insert a new sub-phase before the GPU-process check:

```
Phase 1a (existing, ctx1 / shutdownTimeout):
  - Stop llama-swap
  - Poll until llama-swap IsRunning == false

Phase 1b (NEW, ctx1b / fresh shutdownTimeout):
  - If m.aux != nil: call m.aux.StopAll(ctx1b)
  - On error: setState(Error, ...) and return

Phase 2 (existing): AwaitingGPUFree / GPU process check
Phase 3 (existing): unbind + Shelly off
```

A fresh `shutdownTimeout` context is created for Phase 1b so aux stops get their
own full timeout window (not the leftover from Phase 1a). This mirrors the
existing pattern where Phase 3 creates a fresh `ctx2`.

**Status integration**:

`StatusResponse` gets a new field:

```go
AuxContainers []AuxContainerStatus `json:"auxContainers"`
```

In `Status()`, after the existing probes, if `m.aux != nil`:
- Iterate over `m.aux.Names()`. For each name, call `m.aux.IsRunning(ctx)` with
  a 10s timeout context. On error, log at WARN and treat as `running: false`.
- Build `[]AuxContainerStatus` preserving the `Names()` order.
- Assign to `AuxContainers`.
If `m.aux == nil`, set `AuxContainers` to `[]AuxContainerStatus{}` (never nil,
so the JSON encodes `[]` not `null`).

Aux container probe failures are logged at WARN level (same as the existing
Docker/llama-swap probe — not quieted in any state, because aux containers may
be intentionally running while the system is Off).

### API

New methods on the `api.StateMachine` interface:

```go
StartAuxContainer(name string) state.AuxResult
StopAuxContainer(name string) state.AuxResult
```

New routes (Go 1.22+ ServeMux path patterns):

| Method | Path | Description |
|--------|------|-------------|
| POST | `/containers/{name}/start` | Start an aux container by name |
| POST | `/containers/{name}/stop` | Stop an aux container by name |

Handlers map `AuxResult` to HTTP status:
- `AuxResultOK` → 200 (empty body)
- `AuxResultNotFound` → 404 (empty body)
- `AuxResultConflict` → 409 (empty body)
- `AuxResultError` → 500 (empty body)

All POST endpoints return an empty body, consistent with the existing power
endpoints.

### OpenAPI spec

Add the two new paths to `openapi.json` and add `auxContainers` to the
`StatusResponse` schema:

```json
"auxContainers": {
  "type": "array",
  "items": {
    "type": "object",
    "properties": {
      "name": { "type": "string" },
      "running": { "type": "boolean" }
    }
  },
  "description": "Status of optional aux containers. Empty array when none are configured."
}
```

### Web UI

Add a new card in `index.html` (between the Components card and the GPU-process
banner) that is hidden when `data.auxContainers` is empty. Each row shows the
container name, a running/stopped dot, the status text, and Start/Stop buttons.

The card is rendered dynamically in the `render(data)` function:
- If `data.auxContainers` is empty or length 0 → hide the card.
- For each aux container, render a row with:
  - A `component__dot` (is-on when running).
  - The container name.
  - Status text: "Running" or "Stopped".
  - A Start button (disabled when running or when the power state is not
    Off/Ready).
  - A Stop button (disabled when not running or when the power state is not
    Off/Ready).
- Button click handlers call `POST /containers/{name}/start` or
  `POST /containers/{name}/stop`. After the response, the next 1s status poll
  refreshes the UI. Feedback is shown via the existing `showFeedback` mechanism
  (e.g. "Starting kokoro…", "Stopping kokoro…", "Container not found",
  "Not allowed right now").

The power-state check for button enabling uses the same `ENABLED` concept: aux
buttons are enabled only when `data.state` is `Off` or `Ready` and no cooldown is
active. During `Starting`/`ShuttingDown`/`AwaitingGPUFree`/`Error`, all aux
buttons are disabled.

### Wiring (main.go)

After creating the `docker.Manager` from `cfg.AuxContainers`, call
`machine.SetAuxContainers(auxManager)` before starting the HTTP server. When
`cfg.AuxContainers` is empty, pass an empty `docker.NewManager(nil)` (non-nil,
so the state machine never has a nil aux controller in production).

### Config files

Add a commented-out example to `configs/config.yaml`:

```yaml
# auxContainers:
#   - name: kokoro
#     container: kokoro-tts
#   - name: whisper
#     container: whisper-stt
```

## Tasks

### Task 1 - Config and Docker Manager

- valid config with two aux containers + `config.Load`
  - → `cfg.AuxContainers` has 2 entries with correct name/container pairs
- config with no `auxContainers` key + `config.Load`
  - → `cfg.AuxContainers` is nil/empty, no error
- config with an entry missing `name` + `config.Load`
  - → error message mentions `auxContainers` and `name`
- config with an entry missing `container` + `config.Load`
  - → error message mentions `auxContainers` and `container`
- config with duplicate names + `config.Load`
  - → error message mentions duplicate
- `docker.Manager` with two specs + `Start(ctx, "kokoro")`
  - → exec called with `["docker", "start", "kokoro-tts"]`
  - → returns nil on success
- `docker.Manager` + `Start(ctx, "unknown")`
  - → returns `ErrUnknownContainer`
- `docker.Manager` + `Stop(ctx, "whisper")`
  - → exec called with `["docker", "stop", "whisper-stt"]`
- `docker.Manager` + `StopAll(ctx)` with two containers
  - → exec called twice: `docker stop <container1>` then `docker stop <container2>`
  - → returns nil when both succeed
- `docker.Manager` with two specs + `Names()`
  - → returns `["kokoro", "whisper"]` in config order
- `docker.Manager` with no specs + `Names()`
  - → returns empty non-nil slice
- `docker.Manager` + `StopAll(ctx)` where first stop fails
  - → returns the error from the first stop
  - → second container's stop is NOT called (fail-fast)
- `docker.Manager` with no specs + `StopAll(ctx)`
  - → returns nil (no-op)
- `docker.Manager` + `IsRunning(ctx, "unknown")`
  - → returns `(false, ErrUnknownContainer)`
- `docker.Manager` + `IsRunning` where exec returns "No such container" exit error
  - → returns `(false, nil)` (consistent with existing `Client.IsRunning`)

### Task 2 - State Machine Aux Operations

- machine with aux controller, state `Ready` + `StartAuxContainer("kokoro")`
  - → returns `AuxResultOK`
  - → aux controller's `Start` called with name "kokoro"
- machine with aux controller, state `Off` + `StopAuxContainer("whisper")`
  - → returns `AuxResultOK`
  - → aux controller's `Stop` called with name "whisper"
- machine with aux controller, state `Starting` + `StartAuxContainer("kokoro")`
  - → returns `AuxResultConflict`
  - → aux controller's `Start` NOT called
- machine with aux controller, state `ShuttingDown` + `StopAuxContainer("kokoro")`
  - → returns `AuxResultConflict`
- machine with aux controller, state `Error` + `StartAuxContainer("kokoro")`
  - → returns `AuxResultConflict`
- machine with aux controller, state `AwaitingGPUFree` + `StartAuxContainer("kokoro")`
  - → returns `AuxResultConflict`
- machine with aux controller + `StartAuxContainer("unknown")`
  - → returns `AuxResultNotFound`
- machine with aux controller, state `Ready`, aux `Start` returns error + `StartAuxContainer("kokoro")`
  - → returns `AuxResultError`
- machine with nil aux controller + `StartAuxContainer("kokoro")`
  - → returns `AuxResultNotFound`
- machine with aux controller, state `Ready`, power transition in progress (transitionMu locked) + `StartAuxContainer("kokoro")`
  - → returns `AuxResultConflict`
- machine with aux controller, state `Ready` + `StartAuxContainer("kokoro")` while another aux operation holds transitionMu
  - → returns `AuxResultConflict`

### Task 3 - State Machine Shutdown Stops Aux Containers

- machine in `Ready`, aux controller with two running containers + `PowerOff` + `Wait`
  - → state is `Off`
  - → aux controller's `StopAll` called exactly once
  - → llama-swap `Stop` called before aux `StopAll`
- machine in `Ready`, aux controller's `StopAll` returns error + `PowerOff` + `Wait`
  - → state is `Error`
  - → `lastError` mentions aux container stop failure
  - → unbind NOT called
  - → Shelly power NOT turned off
- machine in `Error`, aux controller with running containers + `PowerOff` + `Wait`
  - → state is `Off`
  - → aux controller's `StopAll` called
- machine in `Ready`, nil aux controller + `PowerOff` + `Wait`
  - → state is `Off` (no aux stop attempted, no error)
- machine in `Ready`, aux controller with containers + `Restart` + `Wait`
  - → state is `Ready`
  - → aux `StopAll` called during shutdown phase
  - → aux `Start` NOT called during startup phase (aux containers are not auto-started)

### Task 4 - Status Reports Aux Containers

- machine with aux controller, state `Ready`, two containers (one running, one stopped) + `Status()`
  - → `AuxContainers` is `[{Name: "kokoro", Running: true}, {Name: "whisper", Running: false}]`
- machine with nil aux controller + `Status()`
  - → `AuxContainers` is `[]` (empty non-nil slice, JSON encodes as `[]`)
- machine with aux controller, Docker unreachable (IsRunning returns error) + `Status()`
  - → `AuxContainers` entries all have `Running: false`
  - → probe failure logged at WARN level

### Task 5 - API Routes for Aux Containers

- `POST /containers/kokoro/start`, fake returns `AuxResultOK`
  - → HTTP 200, empty body
- `POST /containers/kokoro/stop`, fake returns `AuxResultOK`
  - → HTTP 200, empty body
- `POST /containers/unknown/start`, fake returns `AuxResultNotFound`
  - → HTTP 404, empty body
- `POST /containers/kokoro/start`, fake returns `AuxResultConflict`
  - → HTTP 409, empty body
- `POST /containers/kokoro/start`, fake returns `AuxResultError`
  - → HTTP 500, empty body
- `POST /containers/kokoro/start` passes name "kokoro" to `StartAuxContainer`
  - → fake receives "kokoro"
- `GET /containers/kokoro/start` (wrong method)
  - → HTTP 405 Method Not Allowed
- `GET /status` response includes `auxContainers` field
  - → JSON body contains `"auxContainers"` key

### Task 6 - OpenAPI Spec

- `GET /openapi.json` response
  - → `paths` contains `/containers/{name}/start` and `/containers/{name}/stop`
  - → `StatusResponse.properties` contains `auxContainers`
  - → both new paths have POST with 200/404/409/500 responses

### Task 7 - Web UI Aux Containers Card

- `GET /` response (index.html)
  - → contains an aux containers card section (e.g. `id="aux-card"`)
  - → card is hidden by default (`hidden` attribute)
- `render({state: "Ready", auxContainers: [{name: "kokoro", running: true}]})`
  - → aux card is visible (not hidden)
  - → card shows "kokoro" with a running indicator
  - → Start button is disabled, Stop button is enabled
- `render({state: "Ready", auxContainers: [{name: "whisper", running: false}]})`
  - → card shows "whisper" with a stopped indicator
  - → Start button is enabled, Stop button is disabled
- `render({state: "Starting", auxContainers: [{name: "kokoro", running: true}]})`
  - → both Start and Stop buttons are disabled
- `render({state: "Ready", auxContainers: []})`
  - → aux card is hidden
- clicking the Start button sends `POST /containers/{name}/start`
  - → request is made to the correct URL
- clicking the Stop button sends `POST /containers/{name}/stop`
  - → request is made to the correct URL

### Task 8 - Wiring and Documentation

- `configs/config.yaml` contains a commented-out `auxContainers` example
  - → `config.Load("configs/config.yaml")` succeeds with no aux containers
- `cmd/dockmind/main.go` creates a `docker.Manager` from `cfg.AuxContainers` and calls `machine.SetAuxContainers`
  - → `make build` succeeds
- `README.md` documents the new endpoints, config section, and `auxContainers` status field
  - → README contains `/containers/{name}/start` and `/containers/{name}/stop`
  - → README contains `auxContainers`
  - → README yaml config example still loads via `config.Load`
- `docs/product.md` Features list includes the 018-optional-containers story
  - → `product_test.go` passes
- `make build && make test && make lint` all pass

## Technical Context

- Go 1.24.4, module `github.com/dockmind/dockmind`. No new external dependencies.
- The existing `docker.Client` (single container) is reused internally by the new
  `docker.Manager` — each spec gets its own `Client` with a shared `execFunc`.
  The `docker` package remains a leaf package (does not import `state`); the
  `Manager` satisfies `state.AuxContainerController` via structural typing.
- Go 1.22+ `ServeMux` path patterns (`{name}` wildcard) are already used
  (`/v1/{rest...}`); `/containers/{name}/start` uses the same mechanism.
- The `state.New` constructor signature is unchanged; the aux controller is
  injected via `SetAuxContainers` to avoid disrupting existing test helpers.

## Notes

- On-demand aux start/stop uses a fixed 30s timeout. Docker start/stop typically
  completes in under 5s; 30s is a generous bound. If a configurable timeout is
  needed later, it can be added as `auxContainers.timeout`.
- Aux operations are synchronous (not async 202 like power transitions) because
  docker start/stop is fast and the result is known within the request. This
  means the HTTP handler blocks for the duration of the docker call.
- Holding `transitionMu` during a synchronous aux operation blocks power
  transitions for at most 30s. This is acceptable and matches Q2=A (reject aux
  operations during power transitions, and vice versa).
- The aux controller is set once before the server starts; no mutex protects the
  `m.aux` field. This is safe because `SetAuxContainers` is called in `main.go`
  before `httpServer.ListenAndServe`.
- `docker stop` is idempotent (stopping a stopped container returns success), so
  `StopAll` can call it on every configured container without checking
  `IsRunning` first.
- The `fakeStateMachine` in `api_test.go` must be updated to implement the two
  new `StateMachine` interface methods; the `fakeDocker` in `state_test.go` must
  be extended (or a new fake `AuxContainerController` added) for state machine
  tests.
- `readme_test.go` must be updated to check for `auxContainers` and the new
  endpoint paths. `product_test.go` must be updated to check for
  `018-optional-containers`. The OpenAPI test in `api_test.go` must be updated
  to check for the new paths and `auxContainers` property.
