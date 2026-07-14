# Cooldown Protection for eGPU Power Cycling

## Context

Rapid power cycling damages eGPU hardware. After the system shuts down
(state → Off), a user or the gateway idle-watcher can immediately request
another startup — there is no minimum delay between a shutdown and the next
power-on, or between a startup and the next power-off. This story adds a
configurable cooldown period that blocks power transitions for a configurable
duration after each completed transition:

- **Post-shutdown cooldown**: after reaching Off, `POST /power/on` (and
  `POST /restart` from Off) are blocked for `cooldown`.
- **Post-startup cooldown**: after reaching Ready, `POST /power/off` (and
  `POST /restart` from Ready) are blocked for `cooldown`.

The cooldown defaults to 0s (disabled), preserving current behaviour. When
enabled, blocked requests return `429 Too Many Requests`. The `GET /status`
response gains a `cooldownRemaining` field (seconds remaining, 0 when inactive)
so the web UI and API clients can display a countdown.

When the OpenAI gateway is enabled, its `idleTimeout` must not be shorter than
`cooldown` — otherwise the idle watcher would attempt a shutdown during the
post-startup cooldown and fail repeatedly. If `idleTimeout > 0` and
`idleTimeout < cooldown`, DockMind logs a warning on startup and raises the
effective idle timeout to `cooldown`.

## Out of Scope

- **Persisting cooldown state across DockMind restarts.** The cooldown
  timestamps are in-memory. After a DockMind restart the state machine starts
  in Off with no active cooldown — consistent with the existing behaviour
  where the state machine always initialises to Off.
- **Separate cooldown durations for startup and shutdown.** A single
  `power.cooldown` value applies to both directions, per the user's request
  ("same after startup").
- **Cooldown on the Error state.** `POST /power/off` from Error is always
  allowed (no post-startup cooldown check) so error recovery is never blocked.
  `POST /power/on` from Error continues to return 409 (existing behaviour).
- **`Retry-After` header on 429 responses.** The `cooldownRemaining` field in
  `GET /status` already tells clients how long to wait. The 429 response body
  remains empty, consistent with the other POST responses.
- **Modifying the MVP specification document.**
  `docs/DockMind_MVP_Specification.md` is a historical artifact; no test
  enforces its content beyond the README link check.
- **New external dependencies.** `go.mod` remains
  `gopkg.in/yaml.v3 v3.0.1` only. No new imports are required in any file —
  all new code uses stdlib packages already imported by the respective files
  (`time`, `log/slog`).

## Implementation approach

### Config

Add a `PowerConfig` struct and a `Power` field to `Config`:

```go
type PowerConfig struct {
    Cooldown Duration `yaml:"cooldown"`
}
```

```go
type Config struct {
    // ... existing fields ...
    Power    PowerConfig    `yaml:"power"`
}
```

No default needed — the zero value `Duration(0)` means disabled. Add
validation in `validate`:

```go
if cfg.Power.Cooldown < 0 {
    return errors.New("power.cooldown must be >= 0")
}
```

Add a pure helper function to the config package (testable without a logger):

```go
// EffectiveIdleTimeout returns the idle timeout adjusted for cooldown.
// When idleTimeout > 0 and cooldown > idleTimeout, the effective idle
// timeout is raised to cooldown (the minimum sensible value). Returns
// the effective duration and whether it was adjusted.
func EffectiveIdleTimeout(idleTimeout, cooldown time.Duration) (time.Duration, bool) {
    if idleTimeout > 0 && cooldown > idleTimeout {
        return cooldown, true
    }
    return idleTimeout, false
}
```

### State machine (`internal/state/state.go`)

**New PowerResult constant:**

```go
const (
    ResultAccepted PowerResult = iota
    ResultAlreadyInState
    ResultConflict
    ResultCooldown
)
```

**New Machine fields** (guarded by `stateMu`):

```go
cooldown      time.Duration
lastReadyTime time.Time // set when state becomes Ready
lastOffTime   time.Time // set when state becomes Off
```

**Updated `New` signature** — add `cooldown time.Duration` as the last
parameter:

```go
func New(power PowerController, gpu GPUMonitor, docker ContainerController,
    health HealthChecker, logger *slog.Logger, pollInterval, startupTimeout,
    shutdownTimeout, cooldown time.Duration) *Machine
```

**Updated `setState`** — record timestamps when entering Ready or Off:

```go
func (m *Machine) setState(s State, err error) {
    m.stateMu.Lock()
    m.state = s
    m.lastError = err
    if s == Ready {
        m.lastReadyTime = time.Now()
    } else if s == Off {
        m.lastOffTime = time.Now()
    }
    close(m.changeCh)
    m.changeCh = make(chan struct{})
    m.stateMu.Unlock()
}
```

**Cooldown checks in PowerOn / PowerOff / Restart** — read state and the
relevant timestamp in a single `stateMu` critical section, then decide. The
check runs only when `transitionMu` is held (TryLock succeeded) and the state
is stable (Off or Ready). If `cooldown <= 0` or the timestamp is zero (never
transitioned to that state), the check is skipped.

- `PowerOn` (state Off): if `time.Since(lastOffTime) < cooldown` →
  `ResultCooldown` (unlock `transitionMu` and return).
- `PowerOff` (state Ready): if `time.Since(lastReadyTime) < cooldown` →
  `ResultCooldown`. The Error case is **not** checked — PowerOff from Error
  always proceeds (error recovery).
- `Restart` (state Off): same check as PowerOn (post-shutdown cooldown).
- `Restart` (state Ready): same check as PowerOff (post-startup cooldown).

Pattern for PowerOn (PowerOff and Restart follow the same structure):

```go
m.stateMu.Lock()
current := m.state
inCooldown := false
if current == Off && m.cooldown > 0 && !m.lastOffTime.IsZero() {
    inCooldown = time.Since(m.lastOffTime) < m.cooldown
}
m.stateMu.Unlock()

switch current {
case Off:
    if inCooldown {
        m.transitionMu.Unlock()
        return ResultCooldown
    }
    // ... existing goroutine launch ...
```

**StatusResponse — new field:**

```go
type StatusResponse struct {
    // ... existing fields ...
    CooldownRemaining float64 `json:"cooldownRemaining"`
}
```

In `Status()`, compute `cooldownRemaining` while holding `stateMu` (before
releasing the lock for dependency probing):

```go
var cooldownRemaining float64
if m.cooldown > 0 {
    var lastTime time.Time
    switch state {
    case Off:
        lastTime = m.lastOffTime
    case Ready:
        lastTime = m.lastReadyTime
    }
    if !lastTime.IsZero() {
        remaining := m.cooldown - time.Since(lastTime)
        if remaining > 0 {
            cooldownRemaining = remaining.Seconds()
        }
    }
}
```

`cooldownRemaining` is 0 for Starting / ShuttingDown / Error states, when
`cooldown` is 0, when the timestamp is zero (never transitioned), or when the
cooldown has expired.

**EnsureReady — handle ResultCooldown:**

Add a helper that returns the remaining post-shutdown cooldown as a
`time.Duration` (acquires `stateMu` internally):

```go
func (m *Machine) cooldownRemainingForOff() time.Duration {
    if m.cooldown <= 0 {
        return 0
    }
    m.stateMu.Lock()
    defer m.stateMu.Unlock()
    if m.lastOffTime.IsZero() {
        return 0
    }
    remaining := m.cooldown - time.Since(m.lastOffTime)
    if remaining < 0 {
        return 0
    }
    return remaining
}
```

In `EnsureReady`, the `case Off:` branch gains a `ResultCooldown` case that
waits for the cooldown to expire (or context cancellation), then loops back to
retry `PowerOn`:

```go
case ResultCooldown:
    remaining := m.cooldownRemainingForOff()
    if remaining > 0 {
        timer := time.NewTimer(remaining)
        select {
        case <-timer.C:
        case <-ctx.Done():
            timer.Stop()
            return ctx.Err()
        }
    }
    continue
```

This makes the gateway's auto-start transparently wait for the cooldown before
starting. If the context deadline (gateway `requestTimeout`) expires first,
`EnsureReady` returns `context.DeadlineExceeded` and the gateway returns 503
`startup_timeout` — the same behaviour as a startup that exceeds the request
deadline.

### API (`internal/api/api.go`)

Add `ResultCooldown` → `429` in `handlePowerResult`:

```go
case state.ResultCooldown:
    w.WriteHeader(http.StatusTooManyRequests)
```

No changes to the `StateMachine` interface — `Status()` already returns
`cooldownRemaining` in the `StatusResponse`.

### Gateway (`internal/gateway/gateway.go`)

Add an explicit `ResultCooldown` case in the idle watcher's `PowerOff` result
switch (currently handled by `default`). Same behaviour as `default` (reset
`pendingShutdown`) but with a debug log for observability:

```go
case state.ResultCooldown:
    g.activeMu.Lock()
    g.pendingShutdown = false
    g.activeMu.Unlock()
    g.logger.Debug("idle shutdown deferred due to cooldown")
```

With the idle-timeout adjustment in main.go, the idle watcher should never
encounter `ResultCooldown` in practice. This case is defensive.

### main.go (`cmd/dockmind/main.go`)

Pass `cfg.Power.Cooldown.Duration()` to `state.New`:

```go
machine := state.New(
    power, gpuMonitor, dockerClient, healthClient, logger,
    cfg.GPU.PollInterval.Duration(),
    cfg.Startup.Timeout.Duration(),
    cfg.Shutdown.Timeout.Duration(),
    cfg.Power.Cooldown.Duration(),
)
```

Inside the `if cfg.Gateway.Enabled` block, adjust idle timeout before creating
the gateway:

```go
effectiveIdleTimeout, adjusted := config.EffectiveIdleTimeout(
    cfg.Gateway.IdleTimeout.Duration(),
    cfg.Power.Cooldown.Duration(),
)
if adjusted {
    logger.Warn("gateway.idleTimeout is less than power.cooldown; increasing effective idleTimeout",
        "configuredIdleTimeout", cfg.Gateway.IdleTimeout.Duration(),
        "cooldown", cfg.Power.Cooldown.Duration(),
        "effectiveIdleTimeout", effectiveIdleTimeout,
    )
}
gw, err = gateway.NewGatewayWithPollInterval(
    cfg.LlamaSwap.BackendURL,
    effectiveIdleTimeout,
    cfg.Gateway.RequestTimeout.Duration(),
    cfg.GPU.PollInterval.Duration(),
    machine, logger,
)
```

### Web UI (`internal/api/index.html`)

Add a cooldown indicator in the state section (hidden by default):

```html
<p class="state__cooldown" id="cooldown" hidden>Cooldown active — <span id="cooldown-time">0</span>s remaining</p>
```

Add CSS for `.state__cooldown` (small text in the `--busy` colour).

In `render(data)`, after the existing button-enable logic, check
`data.cooldownRemaining > 0`:

- Show the cooldown element with `Math.ceil(data.cooldownRemaining)`.
- When state is Off: disable Power On and Restart buttons.
- When state is Ready: disable Power Off and Restart buttons.
- When `cooldownRemaining` is 0: hide the cooldown element and use the normal
  `ENABLED` map for button states.

In `doAction`, add 429 handling:

```js
else if (res.status === 429) showFeedback("Cooldown active — please wait");
```

Add `cooldown` and `cooldownTime` to the `els` object.

### OpenAPI spec (`internal/api/openapi.json`)

Add `cooldownRemaining` to `StatusResponse.properties`:

```json
"cooldownRemaining": {
    "type": "number",
    "description": "Remaining cooldown in seconds. 0 when no cooldown is active."
}
```

Add a `429` response to `/power/on`, `/power/off`, and `/restart`:

```json
"429": {
    "description": "Cooldown active — power transition blocked by recent power cycle"
}
```

### Polish

- **README** (`README.md`): add a `power:` section with `cooldown: 0s` to the
  first fenced yaml block. Add `cooldownRemaining` to the status example JSON.
  Add a note about the cooldown feature and the idleTimeout interaction.
  Document the 429 response for power endpoints.
- **`AGENTS.md`**: add `429` to the HTTP status mapping line in the "API /
  state-machine conventions" section (`429` = cooldown active).
- **`readme_test.go`**: add an assertion that README contains
  `cooldownRemaining`. Add an assertion that README contains `cooldown`.
- **`docs/product.md`**: add a Features entry for `011-cooldown-protection`.
- **`product_test.go`**: add an assertion that product.md contains
  `011-cooldown-protection`.
- **`configs/config.yaml`**: add `power: cooldown: 0s`.
- **`configs/config-with-gateway.yaml`**: add `power: cooldown: 60s`.

## Tasks

### Task 1 - Config: add cooldown field and EffectiveIdleTimeout helper

- `power.cooldown: 60s` in config YAML + required fields present + `config.Load`
  - → loads successfully
  - → `cfg.Power.Cooldown.Duration()` equals `60s`
- `power` section absent from config YAML + `config.Load`
  - → loads successfully
  - → `cfg.Power.Cooldown.Duration()` equals `0s` (zero value = disabled)
- `power.cooldown: 0s` in config YAML + `config.Load`
  - → loads successfully
  - → `cfg.Power.Cooldown.Duration()` equals `0s`
- `power.cooldown: -1s` in config YAML + `config.Load`
  - → returns an error containing `"power.cooldown"`
- `EffectiveIdleTimeout(30s, 60s)` (idleTimeout < cooldown, both > 0)
  - → returns `(60s, true)` (effective raised to cooldown)
- `EffectiveIdleTimeout(60s, 60s)` (idleTimeout == cooldown)
  - → returns `(60s, false)` (no adjustment needed)
- `EffectiveIdleTimeout(120s, 60s)` (idleTimeout > cooldown)
  - → returns `(120s, false)` (no adjustment needed)
- `EffectiveIdleTimeout(0, 60s)` (idleTimeout = 0 = disabled)
  - → returns `(0, false)` (no adjustment when idle shutdown is off)
- `EffectiveIdleTimeout(30s, 0)` (cooldown = 0 = disabled)
  - → returns `(30s, false)` (no adjustment when cooldown is off)
- existing `config_test.go` cases + `make test`
  - → all existing cases pass unchanged (full config, minimal config, gateway
    config, error cases) — the new `Power` field is the zero value
    `PowerConfig{Cooldown: Duration(0)}` in all existing `want` structs

### Task 2 - State machine: cooldown enforcement and status

- Machine with `cooldown=0` + state Off + `PowerOn()`
  - → returns `ResultAccepted` (no cooldown when disabled — existing behaviour preserved)
- Machine with `cooldown=0` + state Ready + `PowerOff()`
  - → returns `ResultAccepted` (no cooldown when disabled — existing behaviour preserved)
- Machine with `cooldown=50ms` + state Off + `lastOffTime=time.Now()` + `PowerOn()`
  - → returns `ResultCooldown` (post-shutdown cooldown blocks startup)
  - → state remains Off (no transition started)
- Machine with `cooldown=50ms` + state Off + `lastOffTime=time.Now().Add(-51ms)` (cooldown expired) + `PowerOn()`
  - → returns `ResultAccepted` (cooldown expired, startup allowed)
- Machine with `cooldown=50ms` + state Ready + `lastReadyTime=time.Now()` + `PowerOff()`
  - → returns `ResultCooldown` (post-startup cooldown blocks shutdown)
  - → state remains Ready (no transition started)
- Machine with `cooldown=50ms` + state Ready + `lastReadyTime=time.Now().Add(-51ms)` (cooldown expired) + `PowerOff()`
  - → returns `ResultAccepted` (cooldown expired, shutdown allowed)
- Machine with `cooldown=50ms` + state Off + `lastOffTime` zero (never shut down — fresh start) + `PowerOn()`
  - → returns `ResultAccepted` (first startup not blocked by cooldown)
- Machine with `cooldown=50ms` + state Ready + `lastReadyTime` zero (never started — manually set to Ready) + `PowerOff()`
  - → returns `ResultAccepted` (no cooldown when timestamp is zero)
- Machine with `cooldown=50ms` + state Error + `lastReadyTime=time.Now()` + `PowerOff()`
  - → returns `ResultAccepted` (Error recovery exempt from post-startup cooldown)
- Machine with `cooldown=50ms` + state Error + `PowerOn()`
  - → returns `ResultConflict` (existing behaviour — PowerOn from Error always 409)
- Machine with `cooldown=50ms` + state Off + `lastOffTime=time.Now()` + `Restart()`
  - → returns `ResultCooldown` (restart from Off blocked by post-shutdown cooldown)
- Machine with `cooldown=50ms` + state Ready + `lastReadyTime=time.Now()` + `Restart()`
  - → returns `ResultCooldown` (restart from Ready blocked by post-startup cooldown)
- Machine with `cooldown=50ms` + state Starting + `PowerOn()`
  - → returns `ResultConflict` (transition in progress takes precedence over cooldown)
- Machine with `cooldown=50ms` + state Starting + `PowerOff()`
  - → returns `ResultConflict` (transition in progress takes precedence)
- Machine with `cooldown=50ms` + state Off + `lastOffTime=time.Now()` + `PowerOff()`
  - → returns `ResultAlreadyInState` (already Off — cooldown does not affect idempotent no-op)
- Machine with `cooldown=50ms` + state Ready + `lastReadyTime=time.Now()` + `PowerOn()`
  - → returns `ResultAlreadyInState` (already Ready — cooldown does not affect idempotent no-op)
- Machine with `cooldown=50ms` + real PowerOn→Wait (reaches Ready, sets lastReadyTime) + immediate `PowerOff()`
  - → returns `ResultCooldown` (post-startup cooldown active after a real startup)
- Machine with `cooldown=50ms` + real PowerOn→Wait→PowerOff→Wait (reaches Off, sets lastOffTime) + immediate `PowerOn()`
  - → returns `ResultCooldown` (post-shutdown cooldown active after a real shutdown)
- Machine with `cooldown=50ms` + state Off + `lastOffTime=time.Now()` + `Status()`
  - → `StatusResponse.CooldownRemaining` is a positive float64 (approximately 0.05)
- Machine with `cooldown=50ms` + state Off + `lastOffTime=time.Now().Add(-51ms)` (expired) + `Status()`
  - → `StatusResponse.CooldownRemaining` equals `0` (cooldown expired)
- Machine with `cooldown=50ms` + state Ready + `lastReadyTime=time.Now()` + `Status()`
  - → `StatusResponse.CooldownRemaining` is a positive float64
- Machine with `cooldown=50ms` + state Starting + `Status()`
  - → `StatusResponse.CooldownRemaining` equals `0` (no cooldown display during transitions)
- Machine with `cooldown=50ms` + state Error + `Status()`
  - → `StatusResponse.CooldownRemaining` equals `0` (no cooldown display in Error)
- Machine with `cooldown=0` + any state + `Status()`
  - → `StatusResponse.CooldownRemaining` equals `0` (cooldown disabled)
- Machine with `cooldown=50ms` + state Off + `lastOffTime=time.Now()` + `EnsureReady(ctx)` with a context deadline of 200ms
  - → `EnsureReady` blocks until cooldown expires (~50ms), then starts the system
  - → returns `nil` (success — system reached Ready)
  - → final state is Ready
- Machine with `cooldown=50ms` + state Off + `lastOffTime=time.Now()` + `EnsureReady(ctx)` with a context deadline of 20ms (shorter than cooldown)
  - → returns `context.DeadlineExceeded` (context expires during cooldown wait)
- Machine with `cooldown=50ms` + state Off + `lastOffTime=time.Now()` + `EnsureReady(ctx)` with context cancelled immediately
  - → returns `context.Canceled`
- existing `state_test.go` tests + `make test`
  - → all existing tests pass unchanged (existing helpers pass `0` for cooldown)
- `go test -race ./internal/state/`
  - → no data race detected (cooldown fields guarded by `stateMu`)

### Task 3 - API: 429 mapping and OpenAPI spec

- `fakeStateMachine` with `powerOn = ResultCooldown` + `POST /power/on`
  - → HTTP response status `429`
  - → response body is empty
- `fakeStateMachine` with `powerOff = ResultCooldown` + `POST /power/off`
  - → HTTP response status `429`
- `fakeStateMachine` with `restart = ResultCooldown` + `POST /restart`
  - → HTTP response status `429`
- `fakeStateMachine` with `status` containing `CooldownRemaining: 45.5` + `GET /status`
  - → response body contains `"cooldownRemaining":45.5`
- `GET /openapi.json` + parse JSON
  - → `StatusResponse.properties` contains `cooldownRemaining`
- `GET /openapi.json` + parse JSON
  - → `/power/on` POST responses include a `429` entry
  - → `/power/off` POST responses include a `429` entry
  - → `/restart` POST responses include a `429` entry
- existing `api_test.go` tests + `make test`
  - → all existing route tests pass (the `state.StatusResponse` struct gains a
    `CooldownRemaining` field; the fake's existing `status` field includes it
    with zero value automatically — no struct change needed)

### Task 4 - Gateway: idle watcher cooldown handling

- `fakeController` with `powerOffResult = ResultCooldown` + idle watcher started with `idleTimeout=50ms, pollInterval=10ms` + `lastActivity` set 100ms in the past + wait 200ms
  - → no panic or deadlock
  - → `pendingShutdown` is false (reset by the ResultCooldown handler)
  - → a debug log containing "cooldown" is emitted (verify via `slog.NewTextHandler` writing to a `bytes.Buffer`)
- existing `gateway_test.go` tests + `make test`
  - → all existing gateway tests pass unchanged

### Task 5 - main.go wiring

- `make build` from repo root
  - → produces `./dockmind` binary without error
- `make test` from repo root
  - → all tests pass (config, state, gateway, api, readme, product, gateway_design)

### Task 6 - Web UI: cooldown display and 429 handling

- `GET /` response body contains the string `"cooldown"` (element id / JS variable)
- `GET /` response body contains the string `"Cooldown active"` (feedback message for 429)
- `GET /` response body contains the string `"429"` (status code check in doAction)
- `GET /` response body does NOT contain `http://` or `https://` (existing assertion preserved)
- existing `TestWebUIRoutes` assertions + `make test`
  - → all existing assertions pass (DockMind, /status, /power/on, /power/off, /restart, /docs, fetch, setInterval, llama-swap health, component__dot.is-danger, /favicon.svg, app__logo, rel="icon", no Health check label, no app__logo-link when unset)

### Task 7 - Polish: README, product.md, configs, test assertions

- README first fenced yaml block contains `cooldown` + `make test`
  - → `readme_test.go` yaml test passes (first block loads via `config.Load` with `power.cooldown: 0s`)
- README status example JSON contains `cooldownRemaining` + `make test`
  - → `readme_test.go` passes with new assertion for `cooldownRemaining`
- README contains the string `cooldown` + `make test`
  - → `readme_test.go` passes with new assertion for `cooldown`
- README existing assertions still satisfied + `make test`
  - → all existing `readme_test.go` cases pass (all routes, field names, commands, doc links, no `ResultAlreadyInState`/`ResultConflict`, no License section)
- `docs/product.md` Features list references `011-cooldown-protection` + `make test`
  - → `product_test.go` passes with `011-cooldown-protection` assertion
- `docs/product.md` existing assertions still satisfied + `make test`
  - → `product_test.go` existing assertions pass (`004-web-ui`, `006-add-favicon-logo`, `007-openai-gateway`, `008-openai-gateway`, `010-cache-models-json` still present; non-goal check still passes)
- `configs/config.yaml` contains `power` section with `cooldown`
  - → file loads via `config.Load` without error
- `configs/config-with-gateway.yaml` contains `power` section with `cooldown`
  - → file loads via `config.Load` without error
- `make lint` from repo root
  - → `gofmt -l .` prints no file paths
  - → `go vet ./...` reports no issues
- `make build` from repo root
  - → produces `./dockmind` binary without error

## Bootstrap

No new dependencies. The project uses Go 1.24.4 with `gopkg.in/yaml.v3 v3.0.1`
only. No new imports are required in any file — all new code uses stdlib
packages already imported by the respective files (`time`, `log/slog`).

```bash
make build      # go build -o dockmind ./cmd/dockmind
make test       # go test ./...
make lint       # gofmt -l . && go vet ./...
```

## Technical Context

- **Go 1.24.4** — `go.mod` toolchain. `time.Since`, `time.Now`,
  `time.NewTimer`, `time.Time.IsZero` are all stable stdlib APIs.
- **`time.Time.IsZero()`** — returns true for the zero value
  (`time.Time{}`). Used to distinguish "never transitioned to this state"
  (first startup) from "transitioned recently." `time.Since(time.Time{})`
  returns a huge duration (~175,000 hours), so without the `IsZero` guard the
  cooldown check would always pass for a fresh machine — but the guard makes
  the intent explicit and avoids relying on that arithmetic quirk.
- **`time.NewTimer` in EnsureReady** — stdlib timer that fires once after the
  specified duration. Must be stopped if the context fires first
  (`timer.Stop()` before returning `ctx.Err()`). The timer is not deferred
  because the `select` has already consumed the channel; `Stop` just prevents
  a lingering timer in the runtime. This matches the existing `poll` pattern
  which uses `time.NewTicker` with `defer ticker.Stop()`.
- **`float64` for `cooldownRemaining`** — JSON encodes as a number (e.g.
  `45.3` or `0`). The web UI uses `Math.ceil()` to display whole seconds.
  Using `float64` rather than integer seconds preserves sub-second precision
  for API clients and avoids rounding bias in the countdown.
- **`stateMu` vs `transitionMu`** — `stateMu` guards `state`, `lastError`,
  `lastReadyTime`, `lastOffTime`, and `changeCh` (brief critical sections).
  `transitionMu` is held for the entire async transition (TryLock + goroutine
  ownership). The cooldown check reads `lastOffTime`/`lastReadyTime` under
  `stateMu` while `transitionMu` is held — safe because no transition goroutine
  can be running (we hold `transitionMu`), so the timestamps cannot change
  between acquiring and releasing `stateMu`.
- **No new external dependencies** — `go.mod` remains
  `gopkg.in/yaml.v3 v3.0.1` only. `go.sum` unchanged.
- **Existing test conventions** — stdlib `testing`, hand-written fakes,
  table-driven tests, no testify/mocking libraries (per `AGENTS.md`). Cooldown
  tests use short durations (50ms) and manual `lastOffTime`/`lastReadyTime`
  assignment under `stateMu` for deterministic timing. The `newTestMachine`
  and `newTestMachineWithRecorder` helpers pass `0` for cooldown to preserve
  existing behaviour; a new `newTestMachineWithCooldown(cooldown)` helper is
  added for cooldown-specific tests.

## Notes

- **Single cooldown value for both directions.** The user requested "same
  after startup" — one `power.cooldown` value applies to both post-shutdown
  (blocks PowerOn) and post-startup (blocks PowerOff) directions.
- **Restart is subject to cooldown.** Restart from Off includes a startup, so
  it is blocked by the post-shutdown cooldown. Restart from Ready includes a
  shutdown, so it is blocked by the post-startup cooldown. This is consistent
  with the goal of preventing rapid power cycling.
- **Error recovery is never blocked.** `PowerOff` from the Error state does
  not check the post-startup cooldown. The Error state is reached when a
  transition fails; blocking recovery would leave the system stuck.
- **Gateway EnsureReady waits for cooldown.** When the gateway auto-starts the
  system during a post-shutdown cooldown, `EnsureReady` blocks until the
  cooldown expires, then triggers `PowerOn`. This makes the auto-start
  transparent to the client. If the gateway's `requestTimeout` is shorter than
  the remaining cooldown, the request times out with 503 `startup_timeout` —
  the same behaviour as any startup that exceeds the request deadline. Users
  who enable both gateway and cooldown should ensure `requestTimeout >
  cooldown`.
- **Effective idleTimeout = max(idleTimeout, cooldown).** When
  `idleTimeout > 0` and `idleTimeout < cooldown`, the effective value is raised
  to exactly `cooldown` (the minimum sensible value). The idle watcher's
  `lastActivity` is initialised when the system reaches Ready, and the
  post-startup cooldown also starts at that moment. With
  `effectiveIdleTimeout = cooldown`, the idle watcher attempts PowerOff
  exactly when the cooldown expires, so it succeeds. The adjustment and
  warning happen in main.go (inside the `if cfg.Gateway.Enabled` block) using
  the pure `config.EffectiveIdleTimeout` helper — the helper is unit-tested,
  the warning log is a trivial `logger.Warn` call.
- **Cooldown timestamps are in-memory only.** After a DockMind restart, the
  state machine starts in Off with zero timestamps, so no cooldown is active.
  This is consistent with the existing design where the state machine always
  initialises to Off regardless of the actual hardware state.
- **`cooldownRemaining` is 0 during transitions.** The field reflects the
  cooldown relevant to the current stable state (Off → post-shutdown, Ready →
  post-startup). During Starting / ShuttingDown / Error, it is 0 because the
  cooldown check only applies to stable states.
- **`New` signature change.** Adding `cooldown time.Duration` as the last
  parameter to `state.New` requires updating every call site. The two test
  helpers (`newTestMachine`, `newTestMachineWithRecorder`) pass `0`. main.go
  passes `cfg.Power.Cooldown.Duration()`. No other call sites exist.
- **`fakeStateMachine` in api_test.go.** The fake gains no new fields or
  interface methods (the `StateMachine` interface is unchanged). The
  `state.StatusResponse` struct gains a `CooldownRemaining` field; the fake's
  existing `status` field (of type `state.StatusResponse`) includes it with
  zero value automatically. Existing test cases continue to work unchanged.
- **OpenAPI `429` responses have no schema.** Consistent with the existing
  `409` responses which also have no schema (empty body). The
  `cooldownRemaining` field in `GET /status` is the machine-readable way to
  determine the remaining wait time.
- **`configs/config.yaml` uses `cooldown: 0s`.** The default config shows the
  option exists but is disabled. `configs/config-with-gateway.yaml` uses
  `cooldown: 60s` as an example non-default value; with `idleTimeout: 300s`
  (300s > 60s), no adjustment is triggered.
