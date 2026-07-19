# GPU Process Guard Before Power-Off

## Context

When `llama-swap` is shut down, other processes may still be using the eGPU
(e.g. a stray `whisper-server`, a Python script with an open CUDA context).
If Shelly power is cut while any process holds GPU resources, the Linux kernel
panics the next time the eGPU is powered on — the NVIDIA driver encounters a
device that was yanked out from under live CUDA contexts.

DockMind must therefore check, **after `llama-swap` has stopped but before
unbinding the driver and cutting Shelly power**, that no processes are using the
GPU. If processes are found, DockMind enters a new `AwaitingGPUFree` state and
polls periodically (default every 5 minutes) until the GPU is clear, then
proceeds with the shutdown. If a request arrives while waiting (via the gateway
or a manual `POST /power/on` / `POST /restart`), DockMind cancels the wait and
runs startup, returning to the `Ready` state.

## Out of Scope

- **Killing GPU processes.** DockMind never terminates processes; it waits for
  them to exit naturally or for the user to kill them. The process list is
  surfaced in `GET /status` and the web UI so the user can identify and kill
  them manually.
- **Timeout for the `AwaitingGPUFree` wait.** The wait is indefinite — there is
  no max timeout. The user explicitly chose simplicity over a timeout.
- **Modifying the MVP specification document.**
  `docs/DockMind_MVP_Specification.md` is a historical artifact; no test
  enforces its content beyond the README link check.
- **New external dependencies.** `go.mod` remains
  `gopkg.in/yaml.v3 v3.0.1` only. All new code uses stdlib.

## Implementation approach

### New state: `AwaitingGPUFree`

Add a new `State` constant after `ShuttingDown` and before `Error`:

```go
const (
    Off State = iota
    Starting
    Ready
    ShuttingDown
    AwaitingGPUFree // NEW
    Error
)
```

Add `case AwaitingGPUFree: return "AwaitingGPUFree"` to `State.String()`.

### New domain type: `GPUProcess`

Define in `internal/state` alongside `StatusResponse`:

```go
type GPUProcess struct {
    PID  int    `json:"pid"`
    Name string `json:"name"`
}
```

Add `GPUProcesses []GPUProcess` to `StatusResponse`:

```go
type StatusResponse struct {
    // ... existing fields ...
    GPUProcesses []GPUProcess `json:"gpuProcesses"`
}
```

### Extend `GPUMonitor` interface

Add a `Processes` method to the existing `GPUMonitor` interface in
`internal/state`:

```go
type GPUMonitor interface {
    Status(ctx context.Context) (present bool, name string, err error)
    Processes(ctx context.Context) ([]GPUProcess, error)
}
```

`GPUProcess` is defined in `state`, so `internal/gpu` must import
`internal/state` to implement this method. This is the same dependency direction
as `internal/gateway` → `internal/state` (leaf → core); no circular dependency
is created (`state` does not import `gpu`).

### `internal/gpu`: `Processes` method

Add a `Processes` method to `Monitor` that runs:

```
nvidia-smi --query-compute-apps=pid,process_name --format=csv,noheader
```

`--query-compute-apps` lists only compute (CUDA) processes — the ones that hold
GPU contexts and would cause the kernel panic. When no processes are running,
`nvidia-smi` outputs empty stdout (zero lines). The CSV `noheader` format uses
`, ` as the field separator:

```
11213, /app/.venv/bin/python3
16131, whisper-server
82497, llama-server
```

Parse each non-empty line by splitting on `, ` into at most 2 fields
(`pid`, `process_name`) using `strings.SplitN(line, ", ", 2)`. `SplitN` with a
limit of 2 ensures process names containing `, ` are preserved intact. Lines
that do not split into ≥2 fields or whose PID is non-numeric are silently
skipped (defensive parsing). Return `[]GPUProcess` (empty slice, not nil, when
no processes). On `execFunc` error, return `(nil, err)`.

### Config: `shutdown.gpuFreeCheckInterval`

Add `GPUFreeCheckInterval Duration` to `ShutdownConfig`:

```go
type ShutdownConfig struct {
    Timeout              Duration `yaml:"timeout"`
    GPUFreeCheckInterval Duration `yaml:"gpuFreeCheckInterval"`
}
```

Default in `applyDefaults`: `5m` when zero. Validation in `validate`: must be
`> 0` (same rule as `gpu.pollInterval`). This field is **not** gated on
`gateway.enabled` — it applies to all shutdowns.

### `Machine` changes

**New field**: `gpuFreeCheckInterval time.Duration` on `Machine`.

**New fields for resume signaling**:

```go
resumeStartup bool          // guarded by stateMu
resumeCh      chan struct{} // created when entering AwaitingGPUFree, buffered size 1
```

**`New` signature** — add `gpuFreeCheckInterval` after `shutdownTimeout` and
before `cooldown`:

```go
func New(power PowerController, gpu GPUMonitor, docker ContainerController,
    health HealthChecker, unbinder Unbinder, logger *slog.Logger,
    pollInterval, startupTimeout, shutdownTimeout, gpuFreeCheckInterval,
    cooldown time.Duration) *Machine
```

### Shutdown sequence change

The current `shutdown()` uses a single `shutdownTimeout` context for the entire
sequence. The `AwaitingGPUFree` wait is indefinite, so the sequence must be
split into three phases with separate contexts:

**Phase 1** (with `shutdownTimeout` context): stop llama-swap.
1. `setState(ShuttingDown, nil)`
2. `docker.Stop(ctx1)` — on failure → Error, return
3. Poll `docker.IsRunning` until false — on timeout → Error, return

**Phase 2** (no timeout — indefinite wait): wait for GPU to be free.
4. Create `resumeCh` (buffered, size 1) and set `resumeStartup = false` under
   `stateMu`, **before** calling `setState`.
5. `setState(AwaitingGPUFree, nil)`
6. Call `awaitGpuFree()` — returns `(free bool, err error)`:
   - On nvidia-smi error → `setState(Error, ...)`, return
   - On `free == false` (startup requested) → call `m.startup()`, return
   - On `free == true` → proceed to Phase 3

**Phase 3** (with fresh `shutdownTimeout` context): unbind + power off.
7. `setState(ShuttingDown, nil)` — back to ShuttingDown for remaining steps
8. `unbinder.Unbind(ctx2)` — on failure → Error, return
9. `power.SetPower(ctx2, false)` — on failure → Error, return
10. Poll `gpu.Status` until GPU disappears — on timeout → Error, return
11. `setState(Off, nil)`

The two `defer cancel()` calls (one per context) are at different points in the
function, so early returns before Phase 3 only defer-cancel `ctx1`.

### `awaitGpuFree` method

```go
func (m *Machine) awaitGpuFree() (bool, error)
```

- Checks GPU processes immediately (first check). If `len(procs) == 0`, returns
  `(true, nil)` immediately — no waiting needed.
- If processes are found, logs `"GPU processes detected, waiting for them to
  clear"` at INFO with the count, then enters a select loop:
  - `<-ticker.C` (interval = `gpuFreeCheckInterval`): re-check processes. If
    free, do a **non-blocking** check of `resumeCh` (in case a startup request
    arrived between the process check and this point). If a resume signal is
    pending and `resumeStartup` is true, return `(false, nil)`. Otherwise return
    `(true, nil)`.
  - `<-m.resumeCh`: read `resumeStartup` under `stateMu`, reset it to false. If
    true, return `(false, nil)`. If false (spurious wake), continue the loop.
- On any `gpu.Processes` error, return `(false, err)` → caller sets Error state.
- Each `gpu.Processes` call uses a `context.WithTimeout(context.Background(),
  10*time.Second)` — same 10s probe timeout used by `probeGPU`.

### `requestResume` helper

```go
func (m *Machine) requestResume() {
    m.stateMu.Lock()
    m.resumeStartup = true
    ch := m.resumeCh
    m.stateMu.Unlock()
    if ch != nil {
        select {
        case ch <- struct{}{}:
        default:
        }
    }
}
```

Non-blocking send ensures the caller never blocks, even if the channel is full
or the goroutine is between select iterations.

### `PowerOn` / `PowerOff` / `Restart` from `AwaitingGPUFree`

When the shutdown goroutine is waiting in `AwaitingGPUFree`, it holds
`transitionMu`. The three public methods must detect this state **before**
calling `TryLock` and handle it specially:

**`PowerOn`** — add at the top, before `TryLock`:

```go
m.stateMu.Lock()
current := m.state
m.stateMu.Unlock()
if current == AwaitingGPUFree {
    m.requestResume()
    return ResultAccepted
}
```

The waiting goroutine wakes, calls `m.startup()`, and the system transitions
`Starting → Ready`. The caller gets `202 Accepted`.

**`Restart`** — identical special-case: `requestResume()` + `ResultAccepted`.
From `AwaitingGPUFree`, restart and power-on are semantically identical (both
mean "start back up").

**`PowerOff`** — add at the top, before `TryLock`:

```go
m.stateMu.Lock()
current := m.state
m.stateMu.Unlock()
if current == AwaitingGPUFree {
    return ResultAlreadyInState
}
```

The system is already in the process of shutting down — `200 OK`.

### `restart()` change

`restart()` calls `shutdown()` then `startup()`. When `shutdown()` handles a
resume request by calling `startup()` itself, the state is `Ready` when
`shutdown()` returns. `restart()` must detect this and skip its own
`startup()`:

```go
func (m *Machine) restart() {
    // ... existing: if Ready, call shutdown() ...
    
    m.stateMu.Lock()
    current = m.state
    m.stateMu.Unlock()
    if current == Error {
        return
    }
    if current == Ready {
        // shutdown() already ran startup() (resume from AwaitingGPUFree)
        return
    }
    m.startup()
}
```

### `EnsureReady` change (`internal/state`)

Add an `AwaitingGPUFree` case alongside `Off`:

```go
case AwaitingGPUFree:
    result := m.PowerOn()
    switch result {
    case ResultAccepted:
        select {
        case <-ch:
        case <-ctx.Done():
            return ctx.Err()
        }
    case ResultConflict, ResultAlreadyInState, ResultCooldown:
        continue
    }
```

`PowerOn` from `AwaitingGPUFree` always returns `ResultAccepted`, but the other
cases are handled defensively. After the state change (to `Starting`), the loop
re-evaluates and follows the existing `Starting` → `Ready` path.

### `Status()` change

After `probeGPU` returns `gpuPresent == true`, also probe processes:

```go
var gpuProcesses []GPUProcess
if gpuPresent {
    gpuProcesses = m.probeGPUProcesses()
}
if gpuProcesses == nil {
    gpuProcesses = []GPUProcess{}
}
```

Add a `probeGPUProcesses` helper (same 10s timeout pattern as `probeGPU`):

```go
func (m *Machine) probeGPUProcesses() []GPUProcess {
    ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
    defer cancel()
    procs, err := m.gpu.Processes(ctx)
    if err != nil {
        m.logger.Debug("GPU processes probe failed", "error", err)
        return nil
    }
    return procs
}
```

When GPU is absent, `gpuProcesses` is `[]` (empty array) — no extra
`nvidia-smi` call is made. Process probe failures are logged at `DEBUG` (not
`WARN`) because they are non-fatal in the `/status` context — the authoritative
process check happens in the shutdown goroutine.

Set `GPUProcesses: gpuProcesses` in the returned `StatusResponse`.

### `probeFailureExpected` change

Add `AwaitingGPUFree` to the quiet-probe set:

```go
func probeFailureExpected(s State) bool {
    return s == Off || s == Starting || s == ShuttingDown || s == AwaitingGPUFree
}
```

In `AwaitingGPUFree`, llama-swap is intentionally stopped, so health-probe
failures are expected (quieted to `DEBUG`). GPU-probe failures are also
quieted — the GPU is present and expected to work, but if `nvidia-smi`
transiently fails during a `/status` poll, `DEBUG` is appropriate since the
authoritative check is in the shutdown goroutine.

### `internal/gateway`: `EnsureReady` and fake controller

The gateway's `StateController` interface is unchanged (`State()`,
`PowerOff()`, `EnsureReady()`). The real `Machine.EnsureReady` handles
`AwaitingGPUFree` (see above). The gateway's `fakeController` in
`gateway_test.go` must add `AwaitingGPUFree` to the `EnsureReady` switch,
alongside `Off`:

```go
case state.Off, state.Starting, state.ShuttingDown, state.AwaitingGPUFree:
    if f.autoStart {
        f.setState(state.Ready, nil)
        return nil
    }
```

### `cmd/dockmind/main.go` wiring

Pass `cfg.Shutdown.GPUFreeCheckInterval.Duration()` to `state.New` as the new
parameter:

```go
machine := state.New(
    power, gpuMonitor, dockerClient, healthClient, unbindClient, logger,
    cfg.GPU.PollInterval.Duration(),
    cfg.Startup.Timeout.Duration(),
    cfg.Shutdown.Timeout.Duration(),
    cfg.Shutdown.GPUFreeCheckInterval.Duration(),  // NEW
    cfg.Power.Cooldown.Duration(),
)
```

### Test helpers (`internal/state/state_test.go`)

**`fakeGPU`** — add `Processes` method and fields:

```go
type fakeGPU struct {
    mu           sync.Mutex
    present      bool
    name         string
    err          error
    processes    []GPUProcess
    processesErr error
}

func (f *fakeGPU) Processes(ctx context.Context) ([]GPUProcess, error) {
    f.mu.Lock()
    defer f.mu.Unlock()
    if f.processesErr != nil {
        return nil, f.processesErr
    }
    return f.processes, nil
}
```

**All three test helpers** (`newTestMachine`, `newTestMachineWithCooldown`,
`newTestMachineWithRecorder`) — add `gpuFreeCheckInterval` to the `New` call.
Use a short interval (e.g. `10*time.Millisecond`) to keep tests fast. The
helpers' return signatures are unchanged (the `fakeGPU` they return now has the
`processes` field available for tests to configure).

### Web UI (`internal/api/index.html`)

1. Add `AwaitingGPUFree: "busy"` to `STATE_COLORS`.
2. Add `AwaitingGPUFree: { on: true, off: false, restart: true }` to `ENABLED`.
3. Add a GPU processes banner (styled like the error banner, placed after the
   components section and before the error banner):

```html
<section class="gpu-procs" id="gpu-procs-banner" hidden>
  <p class="gpu-procs__label">GPU processes (preventing power off)</p>
  <ul class="gpu-procs__list" id="gpu-procs-list"></ul>
</section>
```

CSS: reuse the `.error` style pattern (surface background, warning-colored
border, uppercase label). Use `--primary` for the border/label color (not
`--danger`) — this is an informational state, not an error.

In `render()`: if `data.gpuProcesses` is non-empty, show the banner and
populate the list with `<li>` entries formatted as
`{name} (PID {pid})`. Otherwise hide it.

### OpenAPI (`internal/api/openapi.json`)

1. Add `"AwaitingGPUFree"` to the `state` enum array.
2. Add `gpuProcesses` to `StatusResponse.properties`:

```json
"gpuProcesses": {
  "type": "array",
  "items": {
    "type": "object",
    "properties": {
      "pid": { "type": "integer" },
      "name": { "type": "string" }
    }
  },
  "description": "Compute processes currently using the GPU. Empty array when the GPU is absent or no processes are running."
}
```

### Polish

- **`README.md`**: update the `/power/off` row to mention the GPU process
  guard. Add `gpuProcesses` to the status JSON example. Add
  `shutdown.gpuFreeCheckInterval` to the yaml config example (under `shutdown:`)
  — must not break the `readme_test.go` yaml-load test (it's an optional field
  with a default). Add `AwaitingGPUFree` to the state list in the "Deterministic
  State Machine" feature bullet.
- **`AGENTS.md`**: add a note in "API / state-machine conventions" that the
  shutdown sequence checks for GPU processes after llama-swap stops and before
  unbind, entering `AwaitingGPUFree` if processes are found. Document the
  `AwaitingGPUFree` transition rules (power-on/restart → resume startup,
  power-off → 200 already-in-state).
- **`docs/product.md`**: add a Features entry for `017-gpu-process-guard`.
- **`product_test.go`**: add an assertion that `docs/product.md` contains
  `017-gpu-process-guard`.
- **`readme_test.go`**: add `{"status example includes gpuProcesses field",
  "gpuProcesses", true}` to the test cases.
- **`internal/api/api_test.go`**: add `"gpuProcesses"` to the list of
  StatusResponse properties checked in `TestOpenAPI`.
- **`configs/config.yaml`**: add `gpuFreeCheckInterval: 5m` under `shutdown:`
  for documentation.

## Tasks

### Task 1 - gpu package: `Processes` method with execFunc injection

- `Monitor` with injected `execFunc` returning valid CSV output with 3
  processes + `Processes(ctx)` called
  - → returns `[]GPUProcess` with 3 entries
  - → first entry `PID=11213, Name="/app/.venv/bin/python3"`
  - → second entry `PID=16131, Name="whisper-server"`
  - → third entry `PID=82497, Name="llama-server"`
- `Monitor` with injected `execFunc` returning empty stdout + `Processes(ctx)`
  - → returns empty (non-nil) `[]GPUProcess`
  - → returns `nil` error
- `Monitor` with injected `execFunc` returning a single valid line + `Processes(ctx)`
  - → returns `[]GPUProcess` with 1 entry
  - → entry has correct PID and Name parsed from the line
- `Monitor` with injected `execFunc` returning a line whose process name
  contains `, ` (e.g. `11213, my, process`) + `Processes(ctx)`
  - → returns `[]GPUProcess` with 1 entry
  - → entry `PID=11213, Name="my, process"` (name preserved intact via
    `SplitN` limit 2)
- `Monitor` with injected `execFunc` returning a line with only 1 field
  (missing name, e.g. `11213`) + `Processes(ctx)`
  - → returns empty `[]GPUProcess` (malformed line silently skipped)
  - → returns `nil` error
- `Monitor` with injected `execFunc` returning a line with non-numeric PID +
  `Processes(ctx)`
  - → returns empty `[]GPUProcess` (line skipped)
  - → returns `nil` error
- `Monitor` with injected `execFunc` returning a generic error + `Processes(ctx)`
  - → returns `nil` slice
  - → returns a non-nil error
- `Monitor` with injected `execFunc` returning `*exec.ExitError` + `Processes(ctx)`
  - → returns `nil` slice
  - → returns a non-nil error
- `New()` constructor
  - → returns a non-nil `*Monitor`
  - → `Monitor.exec` is non-nil
- `go test -race ./internal/gpu/`
  - → no data race detected

### Task 2 - Config: `shutdown.gpuFreeCheckInterval`

- Config with `shutdown.gpuFreeCheckInterval` absent + `Load`
  - → `cfg.Shutdown.GPUFreeCheckInterval` equals `Duration(5 * time.Minute)`
- Config with `shutdown.gpuFreeCheckInterval: 2m` + `Load`
  - → `cfg.Shutdown.GPUFreeCheckInterval` equals `Duration(2 * time.Minute)`
- Config with `shutdown.gpuFreeCheckInterval: 0s` + `Load`
  - → `cfg.Shutdown.GPUFreeCheckInterval` equals `Duration(5 * time.Minute)`
    (zero overridden by default)
- Config with `shutdown.gpuFreeCheckInterval: -1s` + `Load`
  - → returns an error containing `"gpuFreeCheckInterval"`
- All existing `config_test.go` table cases + `Load`
  - → pass with updated expected `ShutdownConfig` values that include
    `GPUFreeCheckInterval: Duration(5 * time.Minute)` (the default applied by
    `applyDefaults`)
- `go test -race ./internal/config/`
  - → no data race detected

### Task 3 - State machine: `AwaitingGPUFree` state and transitions

- Machine in Ready + `PowerOff()` + `fakeGPU.processes` is empty (default) +
  `fakeGPU.present = true`
  - → returns `ResultAccepted`
  - → after `Wait()`, state is `Off`
  - → `power.on` is false (Shelly was turned off)
  - → `fakeUnbinder.calls` equals 1 (unbind called after GPU check passed)
- Machine in Ready + `PowerOff()` + `fakeGPU.processes` has 1 entry +
  `fakeGPU.present = true` + then `fakeGPU.processes` cleared to empty after
  short delay
  - → returns `ResultAccepted`
  - → state transitions to `AwaitingGPUFree` (observable via `State()` before
    `Wait()` returns)
  - → after `Wait()`, state is `Off` (processes cleared, shutdown proceeded)
  - → `power.on` is false
  - → `fakeUnbinder.calls` equals 1
- Machine in Ready + `PowerOff()` + `fakeGPU.processes` has 1 entry +
  `fakeGPU.present = true` + `PowerOn()` called while in `AwaitingGPUFree`
  - → `PowerOn()` returns `ResultAccepted`
  - → after `Wait()`, state is `Ready` (startup ran from within shutdown
    goroutine)
  - → `power.on` is true (Shelly stayed on / was turned on by startup)
  - → `fakeUnbinder.calls` equals 0 (unbind NOT called — shutdown was cancelled)
- Machine in Ready + `PowerOff()` + `fakeGPU.processes` has 1 entry +
  `fakeGPU.present = true` + `Restart()` called while in `AwaitingGPUFree`
  - → `Restart()` returns `ResultAccepted`
  - → after `Wait()`, state is `Ready`
  - → `fakeUnbinder.calls` equals 0
- Machine in Ready + `PowerOff()` + `fakeGPU.processes` has 1 entry +
  `fakeGPU.present = true` + `PowerOff()` called while in `AwaitingGPUFree`
  - → `PowerOff()` returns `ResultAlreadyInState`
  - → state remains `AwaitingGPUFree` (the second PowerOff did not start a new
    transition)
- Machine in Ready + `PowerOff()` + `fakeGPU.processesErr = errors.New(...)`
  + `fakeGPU.present = true`
  - → returns `ResultAccepted`
  - → after `Wait()`, state is `Error`
  - → `lastError` contains `"gpu process"` (case-insensitive)
  - → `power.on` is true (Shelly NOT cut — nvidia-smi failed, safe default)
  - → `fakeUnbinder.calls` equals 0 (unbind NOT reached)
- Machine in Ready + `Restart()` + `fakeGPU.processes` has 1 entry +
  `fakeGPU.present = true` + then `fakeGPU.processes` cleared to empty
  - → returns `ResultAccepted`
  - → after `Wait()`, state is `Ready` (restart completed: shutdown waited for
    GPU, then unbound, powered off, then started back up)
  - → `fakeUnbinder.calls` equals 1 (unbind called during shutdown phase)
- Machine in Ready + `Restart()` + `fakeGPU.processes` has 1 entry +
  `fakeGPU.present = true` + `PowerOn()` called while in `AwaitingGPUFree`
  - → `PowerOn()` returns `ResultAccepted`
  - → after `Wait()`, state is `Ready` (shutdown's resume path ran startup;
    restart() detected Ready and did not double-startup)
  - → `fakeUnbinder.calls` equals 0
- Machine in Ready + `docker.stopErr = errors.New("stop failed")` + `PowerOff()`
  - → after `Wait()`, state is `Error`
  - → `fakeGPU` Processes was never called (GPU check is after docker stop)
- existing `state_test.go` tests + `make test`
  - → all existing tests pass (helpers now pass a short `gpuFreeCheckInterval`;
    existing shutdown/restart tests have empty `fakeGPU.processes` by default,
    so `awaitGpuFree` returns immediately)
- `go test -race ./internal/state/`
  - → no data race detected

### Task 4 - State machine: `Status` includes `gpuProcesses`

- Machine in Ready + `fakeGPU.present = true` + `fakeGPU.processes` has 2
  entries + `Status()` called
  - → `GPUProcesses` has 2 entries matching the fake's processes
- Machine in Off + `fakeGPU.present = false` + `Status()` called
  - → `GPUProcesses` is a non-nil empty slice (`[]GPUProcess{}`)
  - → JSON-serializes to `[]` (not `null`)
- Machine in AwaitingGPUFree + `fakeGPU.present = true` + `fakeGPU.processes`
  has 1 entry + `Status()` called
  - → `GPUProcesses` has 1 entry
  - → `State` is `"AwaitingGPUFree"`
- `go test -race ./internal/state/`
  - → no data race detected

### Task 5 - Gateway: `EnsureReady` handles `AwaitingGPUFree`

- `fakeController` with `currentState = AwaitingGPUFree` + `autoStart = true` +
  `EnsureReady` called
  - → returns `nil` (transitions to Ready via autoStart)
  - → `ensureReadyCalls` equals 1
- `fakeController` with `currentState = AwaitingGPUFree` + `autoStart = false` +
  `changeCh` closed externally + `EnsureReady` called
  - → does not hang; re-evaluates after channel signal
- existing `gateway_test.go` tests + `make test`
  - → all existing gateway tests pass unchanged
- `go test -race ./internal/gateway/`
  - → no data race detected

### Task 6 - main.go wiring and full build

- `make build` from repo root
  - → produces `./dockmind` binary without error
- `make test` from repo root
  - → all tests pass (gpu, state, config, gateway, api, readme, product,
    configs, gateway_design)
- `make lint` from repo root
  - → `gofmt -l .` prints no file paths
  - → `go vet ./...` reports no issues

### Task 7 - Polish: README, AGENTS.md, product.md, OpenAPI, web UI, test assertions

- `docs/product.md` Features list references `017-gpu-process-guard` + `make test`
  - → `product_test.go` passes with `017-gpu-process-guard` assertion
- `docs/product.md` existing assertions still satisfied + `make test`
  - → `product_test.go` existing assertions pass (all prior story references
    still present; non-goal check still passes)
- README contains the string `gpuProcesses` + `make test`
  - → `readme_test.go` passes with the new `gpuProcesses` assertion
- README existing assertions still satisfied + `make test`
  - → all existing `readme_test.go` cases pass (all routes, field names,
    commands, doc links, no `ResultAlreadyInState`/`ResultConflict`, no License
    section, first yaml block loads via `config.Load`)
- `internal/api/openapi.json` contains `gpuProcesses` in StatusResponse
  properties + `AwaitingGPUFree` in state enum + `make test`
  - → `api_test.go` `TestOpenAPI` passes with `gpuProcesses` in the checked
    properties list
- Web UI `index.html` contains `AwaitingGPUFree` in `STATE_COLORS` and
  `ENABLED` + `make test`
  - → `make test` passes (no specific web UI test, but build and lint must pass)
- `make lint` from repo root
  - → `gofmt -l .` prints no file paths
  - → `go vet ./...` reports no issues
- `make build` from repo root
  - → produces `./dockmind` binary without error

## Bootstrap

No new dependencies. The project uses Go 1.24.4 with `gopkg.in/yaml.v3 v3.0.1`
only. All new code uses stdlib (`context`, `os/exec`, `strconv`, `strings`,
`time`).

```bash
make build      # go build -o dockmind ./cmd/dockmind
make test       # go test ./...
make lint       # gofmt -l . && go vet ./...
```

## Technical Context

- **Go 1.24.4** — `go.mod` toolchain. All APIs used (`exec.CommandContext`,
  `strings.Split`, `strconv.Atoi`, `time.NewTicker`, `select`) are stable
  stdlib.
- **`nvidia-smi --query-compute-apps`** — queries only compute (CUDA) processes,
  which are the ones that hold GPU contexts and would cause the kernel panic.
  The `--format=csv,noheader` flag produces comma-separated output with no
  header row. Empty stdout means no processes — GPU is safe to power off.
  Only `pid` and `process_name` are queried; memory usage is not needed to
  identify and kill blocking processes.
- **`execFunc` injection pattern** — `internal/gpu` already defines an
  unexported `execFunc` type and injects it via a struct field. The new
  `Processes` method uses the same `m.exec` field, so tests inject a fake
  `execFunc` and `nvidia-smi` is never invoked during `make test`.
- **`internal/gpu` → `internal/state` import** — `Processes` returns
  `[]state.GPUProcess`, so `gpu` must import `state`. This is the same
  leaf → core dependency direction as `gateway` → `state`. No circular
  dependency: `state` does not import `gpu`.
- **`New` signature change** — adding `gpuFreeCheckInterval` as the 9th
  parameter (after `shutdownTimeout`, before `cooldown`) requires updating the
  single production call site (`cmd/dockmind/main.go`) and the three test
  helpers (`newTestMachine`, `newTestMachineWithCooldown`,
  `newTestMachineWithRecorder`). No other call sites exist.
- **`config_test.go` expected values** — all existing table-driven cases
  construct `ShutdownConfig{Timeout: ...}` without `GPUFreeCheckInterval`.
  After the change, `applyDefaults` sets `GPUFreeCheckInterval` to `5m`, so
  every expected `Config` value must be updated to include
  `GPUFreeCheckInterval: Duration(5 * time.Minute)`. There are 5 such cases
  in `TestLoad` (the ones with `want: Config{...}`). `TestGatewayConfig` uses
  per-field `assert` functions and does not compare the full `Config`, so it
  needs no changes.
- **Concurrency model** — the shutdown goroutine holds `transitionMu` for the
  entire transition, including the `AwaitingGPUFree` wait. `PowerOn`/`Restart`
  detect `AwaitingGPUFree` **before** `TryLock` and signal via `resumeCh`
  (buffered, non-blocking send). The goroutine's `select` wakes on
  `<-resumeCh`, checks `resumeStartup`, and calls `startup()` directly (it
  already holds `transitionMu`). This preserves the invariant that
  `transitionMu` is never released in the synchronous path and is always
  unlocked by the goroutine's `defer`.
- **Two-context shutdown** — the `AwaitingGPUFree` wait is indefinite, so the
  single `shutdownTimeout` context is split: `ctx1` covers Phase 1
  (stop llama-swap), `ctx2` covers Phase 3 (unbind + power off). Phase 2 uses
  `context.Background()` with per-check 10s timeouts. Both `defer cancel()`
  calls are at different points; early returns only cancel the contexts created
  so far.
- **No new external dependencies** — `go.mod` remains
  `gopkg.in/yaml.v3 v3.0.1` only. `go.sum` unchanged.
- **Existing test conventions** — stdlib `testing`, hand-written fakes,
  table-driven tests, no testify/mocking libraries (per `AGENTS.md`). The
  `fakeGPU.Processes` method follows the same mutex-guarded pattern as
  `fakeGPU.Status`.

## Notes

- **No timeout for `AwaitingGPUFree`.** The user explicitly chose indefinite
  waiting for simplicity. DockMind waits until the GPU is clear or a startup is
  requested. If processes never exit, the system stays in `AwaitingGPUFree`
  indefinitely — the user can see the process list in `/status` or the web UI
  and kill them manually, or trigger `POST /power/on` to resume.
- **nvidia-smi failure → Error state.** If `nvidia-smi` itself fails during the
  process check (binary error, GPU disappeared, etc.), DockMind enters the
  Error state and does NOT cut Shelly power. This is the safe default — never
  cut power when the GPU state is unverified. The user can investigate and
  retry via `POST /power/off` from Error.
- **Process check is after llama-swap stop, before unbind.** The ordering
  ensures llama-swap is no longer using the GPU when the check runs, and the
  unbind (which detaches the NVIDIA driver) only happens after the GPU is
  confirmed free. This is verified by the test: docker-stop failure →
  `Processes` never called.
- **`gpuProcesses` in `/status` doubles nvidia-smi calls when GPU is present.**
  This is consistent with the "live on every call" design (no background
  polling). The second call (`--query-compute-apps`) is fast (sub-second on a
  working GPU). When GPU is absent, no second call is made.
- **`AwaitingGPUFree` in `probeFailureExpected`.** llama-swap is intentionally
  stopped in this state, so health-probe failures are quieted to `DEBUG`.
  GPU-probe failures are also quieted — the authoritative GPU check is in the
  shutdown goroutine, not in `/status` probes.
- **Web UI process list.** The banner shows process name and PID so the user
  can identify what's preventing power off and kill it manually. The banner
  uses `--primary` color (informational, not an error).
- **`POST /power/off` from `AwaitingGPUFree` returns `200 OK`.** The system is
  already shutting down — this is an idempotent no-op, same as `POST /power/off`
  from `Off`. The off button is disabled in the web UI for this state.
- **Resume race window.** If a `PowerOn`/`Restart` arrives at the exact moment
  the GPU becomes free, `awaitGpuFree` does a non-blocking `resumeCh` check
  before returning `free=true`. If the signal arrived after that check, the
  system proceeds to `Off` and the next `EnsureReady` call triggers
  `PowerOn` from `Off` — a one-cycle delay, not a correctness issue.
