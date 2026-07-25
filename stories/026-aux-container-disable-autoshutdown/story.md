# Aux Container Disable Auto-Shutdown

## Context

DockMind's idle auto-shutdown (gateway idle watcher) tracks only OpenAI
gateway inference requests to decide whether the system is busy. Aux
containers like ComfyUI access the GPU directly — not through the
gateway/llama-swap proxy — so their activity is invisible to the idle
watcher. The result: while a user is actively using ComfyUI, the idle
countdown keeps running and eventually powers the system off, killing
the user's session.

This feature adds an opt-in per-container config flag,
`disableIdleShutdown`, that tells DockMind: while this aux container is
running, treat the system as busy and suppress the idle auto-shutdown
countdown. The gateway's idle watcher probes the configured blocking
containers on every tick; if any is running, it resets the idle timer,
cancels any pending shutdown, and reports `idleShutdownBlocked: true` in
`GET /status` so the web UI can show an explicit "Auto-shutdown paused —
<name> running" indicator.

## Out of Scope

- Detecting GPU activity generically (e.g. polling `nvidia-smi`
  utilization). The fix is opt-in per container, not heuristic.
- Blocking idle shutdown for the primary `llama-swap` container — it is
  already covered by gateway request tracking.
- Auto-starting blocking containers on power-on. They remain
  user-managed, started on demand.
- A configurable probe interval for the blocking-container check. It
  reuses the existing `gpu.pollInterval` (the idle watcher tick
  interval), consistent with all other transition wait loops.
- Health checking for aux containers. Only running/stopped status is
  probed, same as the existing aux container behavior.

## Implementation approach

### Config

Add a `DisableIdleShutdown` boolean field to `AuxContainerConfig`
(yaml: `disableIdleShutdown`, defaults `false`):

```yaml
auxContainers:
  - name: comfyui
    container: comfyui
    disableIdleShutdown: true
  - name: kokoro
    container: kokoro-tts
```

No new validation is required — the field is a plain boolean that
defaults to `false` when absent.

### Docker Manager

Add `DisableIdleShutdown bool` to `docker.ContainerSpec` and a new
`IdleBlockingNames() []string` method on `docker.Manager` that returns
the display names of specs where `DisableIdleShutdown` is true, in
config order. The `docker` package remains a leaf package (does not
import `state`); `Manager` satisfies the extended
`state.AuxContainerController` via structural typing.

### State Machine

Extend the `AuxContainerController` interface with one method:

```go
type AuxContainerController interface {
    Names() []string
    Start(ctx context.Context, name string) error
    Stop(ctx context.Context, name string) error
    IsRunning(ctx context.Context, name string) (bool, error)
    StopAll(ctx context.Context) error
    IdleBlockingNames() []string
}
```

Add a new method on `Machine`:

```go
func (m *Machine) IdleShutdownBlocked() bool
```

Logic:
1. If `m.aux == nil` → `false`.
2. Get `m.aux.IdleBlockingNames()`. If empty → `false`.
3. For each blocking name, call `m.aux.IsRunning` with a 10s timeout
   context. On error, log at `DEBUG` and treat as not running (safe
   default — a failed probe does not block shutdown).
4. If any blocking container is running → `true`; otherwise `false`.

This method is added to the `gateway.StateController` interface so the
gateway can call it on every idle-watcher tick.

`AuxContainerStatus` gains a `DisableIdleShutdown bool` field
(json: `disableIdleShutdown`). In `probeAuxContainers`, build a set of
blocking names from `m.aux.IdleBlockingNames()` and mark each status
entry whose name is in that set. This is config metadata (no extra
probe) and is always populated regardless of gateway state.

`StatusResponse` gains a top-level `IdleShutdownBlocked bool` field
(json: `idleShutdownBlocked`). It is **not** set by `Machine.Status()`;
it defaults to `false` and is populated by the API handler from the idle
reporter (see API section). When the gateway is disabled there is no
idle reporter, so the field stays `false` — correct, since there is no
auto-shutdown to block.

### Gateway

Add an `idleBlocked bool` field to `Gateway`, guarded by `activeMu` and
updated on every tick. Three changes:

**`tick()`** — after the Ready-transition initialization block and only
when `current == state.Ready`, probe `g.machine.IdleShutdownBlocked()`:

- If blocked: under `activeMu`, set `g.lastActivity = time.Now()`,
  `g.pendingShutdown = false`, `g.idleBlocked = true`; return early.
  This resets the idle timer so that when the blocking container later
  stops, the countdown restarts from that moment. It also cancels any
  pending/grace-period shutdown (Q4).
- If not blocked: under `activeMu`, set `g.idleBlocked = false`. Proceed
  with the existing idle-timeout / pending-shutdown logic unchanged.

The probe runs once per tick (every `gpu.pollInterval`, default 1s). It
calls `docker inspect` for each blocking container (typically one,
e.g. ComfyUI) — the same cost as an aux status probe.

**`IdleRemaining()`** — add `blocked` to the values read under
`activeMu`. If `blocked` is true, return `0` (same as when a request is
in flight or pendingShutdown is set). The existing `idleTimeout <= 0`
and `state != Ready` guards remain first.

**`IdleShutdownBlocked()`** — new method:

```go
func (g *Gateway) IdleShutdownBlocked() bool {
    if g.idleTimeout <= 0 {
        return false
    }
    if g.machine.State() != state.Ready {
        return false
    }
    g.activeMu.Lock()
    blocked := g.idleBlocked
    g.activeMu.Unlock()
    return blocked
}
```

This returns the cached value from the last tick (≤ 1s stale, matching
the countdown's 1s resolution) and avoids a redundant docker probe on
every `GET /status` call. The `idleTimeout <= 0` and `state != Ready`
guards mirror `IdleRemaining()` so the field is `false` when
auto-shutdown is not active.

### API

Extend the `IdleReporter` interface with one method:

```go
type IdleReporter interface {
    IdleRemaining() float64
    IdleShutdownBlocked() bool
}
```

In `handleStatus`, after `status := s.machine.Status()` and the existing
`IdleRemaining` assignment, add:

```go
if s.idleReporter != nil {
    status.IdleRemaining = s.idleReporter.IdleRemaining()
    status.IdleShutdownBlocked = s.idleReporter.IdleShutdownBlocked()
}
```

### OpenAPI spec

Add `idleShutdownBlocked` (boolean) to the `StatusResponse` schema, and
`disableIdleShutdown` (boolean) to the `auxContainers` items schema.

### Web UI

In `index.html`, the `render(data)` function:

1. **Paused indicator (Q1=A):** Before the existing
   `if (data.idleRemaining > 0)` block, check
   `data.idleShutdownBlocked`. If true, show the `#idle` element with
   text "Auto-shutdown paused — {name} running" (using the first aux
   container where `c.disableIdleShutdown && c.running`), and hide the
   countdown number. If `idleShutdownBlocked` is false, fall through to
   the existing countdown logic.

2. **Per-container badge (Q2=C):** In the aux container row template,
   when `c.disableIdleShutdown` is true, render a small label (e.g.
   a `<span class="aux__badge">pauses auto-shutdown</span>`) next to the
   container name. This is static config metadata, shown whether the
   container is running or stopped.

### Wiring (main.go)

When building `auxSpecs` from `cfg.AuxContainers`, copy the
`DisableIdleShutdown` flag:

```go
auxSpecs[i] = docker.ContainerSpec{
    Name:                aux.Name,
    Container:           aux.Container,
    DisableIdleShutdown: aux.DisableIdleShutdown,
}
```

No other wiring changes — the gateway already has the `machine`
reference via `StateController`, and `IdleShutdownBlocked()` is called
through that interface.

### Config files

Update the commented-out example in `configs/config.yaml` to show the
new flag on one entry:

```yaml
# auxContainers:
#   - name: comfyui
#     container: comfyui
#     disableIdleShutdown: true
#   - name: kokoro
#     container: kokoro-tts
```

### Documentation

- `README.md`: document the `disableIdleShutdown` aux container option,
  the `idleShutdownBlocked` status field, and the per-container
  `disableIdleShutdown` status field. Add `idleShutdownBlocked` to the
  status example JSON.
- `readme_test.go`: add a check that README contains
  `idleShutdownBlocked` and `disableIdleShutdown`.
- `product_test.go`: add a check that `docs/product.md` references
  `026-aux-container-disable-autoshutdown`.

## Tasks

### Task 1 - Config and Docker Manager

- config with `disableIdleShutdown: true` on one aux entry + `config.Load`
  - → `cfg.AuxContainers[0].DisableIdleShutdown` is `true`
  - → `cfg.AuxContainers[1].DisableIdleShutdown` is `false` (absent = default)
- config with no `disableIdleShutdown` key on any entry + `config.Load`
  - → all entries have `DisableIdleShutdown == false`, no error
- `docker.Manager` with two specs (one with `DisableIdleShutdown: true`) + `IdleBlockingNames()`
  - → returns `["comfyui"]` (only the flagged spec, in config order)
- `docker.Manager` with no specs having the flag + `IdleBlockingNames()`
  - → returns empty non-nil slice
- `docker.Manager` with no specs at all + `IdleBlockingNames()`
  - → returns empty non-nil slice

### Task 2 - State Machine IdleShutdownBlocked

- machine with aux controller, one blocking container running + `IdleShutdownBlocked()`
  - → returns `true`
- machine with aux controller, blocking container stopped + `IdleShutdownBlocked()`
  - → returns `false`
- machine with aux controller, two blocking containers (one running, one stopped) + `IdleShutdownBlocked()`
  - → returns `true`
- machine with aux controller, no blocking containers configured (`IdleBlockingNames` returns empty) + `IdleShutdownBlocked()`
  - → returns `false`
- machine with nil aux controller + `IdleShutdownBlocked()`
  - → returns `false`
- machine with aux controller, blocking container `IsRunning` returns error + `IdleShutdownBlocked()`
  - → returns `false` (failed probe treated as not running)
  - → probe failure logged at `DEBUG` level

### Task 3 - Status Reports disableIdleShutdown Per Container

- machine with aux controller, two containers (comfyui flagged, kokoro not) + `Status()`
  - → `AuxContainers[0].DisableIdleShutdown` is `true`
  - → `AuxContainers[1].DisableIdleShutdown` is `false`
- machine with nil aux controller + `Status()`
  - → `AuxContainers` is `[]` (empty non-nil slice)
  - → `IdleShutdownBlocked` is `false` (default, not set by Status)

### Task 4 - Gateway Idle Watcher Suppresses Shutdown When Blocked

- gateway with idleTimeout, machine Ready, blocking container running, lastActivity far in past + `StartIdleWatcher` + wait
  - → `PowerOff` NOT called
  - → `lastActivity` reset to ~now
  - → `pendingShutdown` is `false`
- gateway with idleTimeout, machine Ready, blocking container running, `pendingShutdown` pre-set to true + `StartIdleWatcher` + wait one tick
  - → `pendingShutdown` cleared to `false`
  - → `PowerOff` NOT called
- gateway with idleTimeout, machine Ready, blocking container running + `IdleRemaining()`
  - → returns `0`
- gateway with idleTimeout, machine Ready, blocking container stopped + `IdleRemaining()` with recent lastActivity
  - → returns a positive value (normal countdown resumes)
- gateway with idleTimeout, machine Ready, blocking container running + `IdleShutdownBlocked()`
  - → returns `true`
- gateway with idleTimeout, machine Off, blocking container running + `IdleShutdownBlocked()`
  - → returns `false` (not Ready)
- gateway with idleTimeout=0, machine Ready, blocking container running + `IdleShutdownBlocked()`
  - → returns `false` (idle shutdown disabled)
- gateway with idleTimeout, machine Ready, blocking container stops mid-run + wait
  - → after the container stops, idle countdown resumes and `PowerOff` is eventually called
  - → `IdleShutdownBlocked()` returns `false` after the next tick

### Task 5 - API Status Includes idleShutdownBlocked

- `GET /status` with `fakeIdleReporter{blocked: true}` wired via `SetIdleReporter`
  - → JSON body contains `"idleShutdownBlocked": true`
- `GET /status` with `fakeIdleReporter{blocked: false}` wired
  - → JSON body contains `"idleShutdownBlocked": false`
- `GET /status` with no idle reporter (gateway disabled)
  - → JSON body contains `"idleShutdownBlocked": false` (default)

### Task 6 - OpenAPI Spec

- `GET /openapi.json` response
  - → `StatusResponse.properties` contains `idleShutdownBlocked` (boolean)
  - → `auxContainers.items.properties` contains `disableIdleShutdown` (boolean)

### Task 7 - Web UI Paused Indicator and Per-Container Badge

The web UI is tested by string-matching the served `index.html` source
(same convention as the existing `TestWebUIAuxCard` tests — the repo has
no JS runtime; it asserts the HTML/JS source contains the implementing
code).

- `GET /` response body
  - → contains `data.idleShutdownBlocked` (the render branch that shows the paused indicator)
  - → contains the string `paused` (the paused indicator text)
  - → contains `disableIdleShutdown` (the per-container badge condition in the row template)
  - → contains `pauses auto-shutdown` (the badge label text)
  - → contains `aux__badge` (the badge CSS class)
- The paused-indicator branch precedes the existing `data.idleRemaining > 0` countdown branch
  - → body contains `idleShutdownBlocked` before `idleRemaining` in the render function source
- The aux row template renders the badge conditionally on `c.disableIdleShutdown`
  - → body contains a template expression referencing `disableIdleShutdown` inside the `auxContainers.map` block (e.g. `${c.disableIdleShutdown ? '<span class="aux__badge">pauses auto-shutdown</span>' : ''}`)

### Task 8 - Wiring and Documentation

- `configs/config.yaml` commented-out example includes `disableIdleShutdown: true`
  - → `config.Load("configs/config.yaml")` succeeds (commented out, no effect)
- `cmd/dockmind/main.go` copies `DisableIdleShutdown` from config into `docker.ContainerSpec`
  - → `make build` succeeds
- `README.md` documents `disableIdleShutdown` config option and `idleShutdownBlocked` status field
  - → README contains `disableIdleShutdown`
  - → README contains `idleShutdownBlocked`
  - → README status example JSON includes `"idleShutdownBlocked"`
  - → README yaml config example still loads via `config.Load`
- `docs/product.md` Features list includes the 026 story
  - → `product_test.go` passes
- `make build && make test && make lint` all pass

## Technical Context

- Go 1.24.4, module `github.com/dockmind/dockmind`. No new external
  dependencies.
- The `docker` package remains a leaf package (does not import `state`).
  `docker.Manager` satisfies the extended `state.AuxContainerController`
  via structural typing, same as the existing pattern.
- The `gateway.StateController` interface gains `IdleShutdownBlocked()
  bool`. The `fakeController` in `gateway_test.go` must be updated to
  implement it (default `false`, with a configurable field for tests
  that need `true`).
- The `api.IdleReporter` interface gains `IdleShutdownBlocked() bool`.
  The `fakeIdleReporter` in `api_test.go` must be updated to implement
  it (add a `blocked bool` field).
- The `fakeAuxController` in `state_test.go` must be updated to
  implement `IdleBlockingNames() []string` (add a `blockingNames`
  field, default nil → empty slice).
- The `fakeStateMachine` in `api_test.go` does NOT need
  `IdleShutdownBlocked()` — the API handler reads it from the idle
  reporter, not the state machine. The `StateMachine` interface is
  unchanged.
- `IdleShutdownBlocked()` on the state machine performs live docker
  probes (one `docker inspect` per blocking container). The gateway
  calls it once per tick (1s default). `IdleRemaining()` and
  `IdleShutdownBlocked()` on the gateway use the cached tick result to
  avoid redundant probes on `GET /status`.

## Notes

- The blocking-container probe in the idle watcher tick is a live
  `docker inspect` call, same as the aux status probe in `Status()`.
  With one blocking container (the typical case) this adds one
  ~50ms subprocess call per second — negligible relative to the
  existing four dependency probes on every `/status` call.
- While a blocking container is running, `lastActivity` is reset to
  `time.Now()` on every tick. This means the idle timer effectively
  pauses: when the container stops, the full `idleTimeout` elapses
  before shutdown. This is the desired behavior — the user was active
  via ComfyUI, so the idle window should start fresh.
- A failed `IsRunning` probe for a blocking container is treated as
  "not running" (does not block shutdown). This is the safe default: if
  Docker is unreachable, the system should still be able to shut down.
  The failure is logged at `DEBUG` (not `WARN`) because it is probed
  every second and a transient failure would be noisy.
- The `idleShutdownBlocked` status field is only `true` when the
  gateway is enabled (`idleTimeout > 0`), the state is `Ready`, and a
  blocking container is running. When the gateway is disabled, the
  field is always `false` — there is no auto-shutdown to block.
- The per-container `disableIdleShutdown` flag in `auxContainers`
  status entries is config metadata, always populated regardless of
  gateway state, so the UI can badge containers even when the gateway
  is off.
