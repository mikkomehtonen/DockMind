# Auto-Shutdown Countdown

## Context

When the OpenAI gateway is enabled with `gateway.idleTimeout > 0`, DockMind
automatically shuts the system down after a configurable idle period with no
inference requests (story 008). Today there is no visibility into *when* that
automatic shutdown will happen — a user watching the web UI or polling
`GET /status` cannot tell whether the system will stay up for another 5 minutes
or 5 seconds. This story surfaces the remaining time before an idle
auto-shutdown so users and API clients can see the countdown.

The countdown is reported as a new `idleRemaining` field (float64 seconds) in
`GET /status` and rendered in the web UI. The display resolution is 1 minute
while more than 1 minute remains, and switches to 1 second once less than 1
minute remains. The countdown is only shown when the gateway is enabled,
`idleTimeout > 0`, the state is `Ready`, and no inference request is in flight.

## Out of Scope

- **Changing the idle-shutdown trigger logic.** The two-phase grace-period
  shutdown, `lastActivity` reset rules, and `pendingShutdown` semantics
  (story 008) are unchanged. This story is read-only reporting on top of the
  existing idle tracking.
- **Resetting the idle timer from the web UI.** No new "keep alive" or "postpone
  shutdown" button is added. The timer is reset only by inference requests, as
  today.
- **Persisting idle state across DockMind restarts.** `lastActivity` is
  in-memory; after a restart the countdown reinitialises when the system next
  reaches `Ready` (same as today).
- **Background polling.** `GET /status` continues to query dependencies live on
  each call. `idleRemaining` is computed on demand from the gateway's in-memory
  `lastActivity` — no new goroutine.
- **New external dependencies.** `go.mod` remains
  `gopkg.in/yaml.v3 v3.0.1` only. All new code uses stdlib packages already
  imported by the respective files (`time`, `encoding/json`, `net/http`).
- **Modifying the MVP specification document.**
  `docs/DockMind_Gateway_Design.md` and `docs/DockMind_MVP_Specification.md` are
  not modified.

## Implementation approach

### StatusResponse field (`internal/state/state.go`)

Add an `IdleRemaining` field to `StatusResponse`:

```go
type StatusResponse struct {
    // ... existing fields ...
    CooldownRemaining float64 `json:"cooldownRemaining"`
    IdleRemaining     float64 `json:"idleRemaining"`
}
```

The state machine **never** populates `IdleRemaining` — it has no knowledge of
the gateway. `Machine.Status()` leaves it as the zero value (`0`), exactly as it
already leaves `CooldownRemaining` at `0` when cooldown is disabled. The API
layer overrides it when an idle reporter is wired (see below). This keeps the
state machine decoupled from the gateway, mirroring the existing
`cooldownRemaining` pattern where the machine owns cooldown but the gateway owns
idle.

### Gateway: `IdleRemaining()` method (`internal/gateway/gateway.go`)

Add a method on `*Gateway` that computes the remaining idle time on demand:

```go
// IdleRemaining returns the number of seconds before an idle auto-shutdown,
// or 0 when no shutdown is pending. It is safe to call concurrently with the
// idle watcher and request handlers.
func (g *Gateway) IdleRemaining() float64 {
    if g.idleTimeout <= 0 {
        return 0
    }
    if g.machine.State() != state.Ready {
        return 0
    }
    g.activeMu.Lock()
    active := g.active
    lastActivity := g.lastActivity
    g.activeMu.Unlock()
    if active > 0 {
        return 0
    }
    remaining := g.idleTimeout - time.Since(lastActivity)
    if remaining < 0 {
        return 0
    }
    return remaining.Seconds()
}
```

**Design rules (explicit):**

1. `idleTimeout <= 0` → `0` (auto-shutdown disabled).
2. `machine.State() != state.Ready` → `0`. Auto-shutdown only applies in the
   `Ready` state; during `Off`/`Starting`/`ShuttingDown`/`Error` there is no
   pending idle shutdown.
3. `active > 0` (an inference request is in flight) → `0`. The countdown is
   hidden while a request is active because `lastActivity` is only refreshed at
   request start/end, so a naive countdown would tick down misleadingly during a
   long streaming request. When the request finishes, the countdown reappears
   (at the full timeout, since inference requests reset `lastActivity`).
4. `remaining < 0` (idle time already exceeded, e.g. during the grace-period
   tick window or after `pendingShutdown` is set) → `0`.
5. Otherwise return `remaining.Seconds()` (a positive float64).

**Lock ordering:** `g.machine.State()` is called **before** acquiring
`activeMu`. The state machine never acquires the gateway's `activeMu`, so there
is no deadlock risk. `activeMu` is held only long enough to snapshot `active`
and `lastActivity`, then released before the arithmetic — consistent with the
existing `tick()` pattern which reads the same fields under `activeMu`.

**Pre-tick window (known minor behaviour):** `lastActivity` is initialised to
`time.Now()` when the system transitions to `Ready` by the idle watcher's first
tick (existing design, story 008). Between reaching `Ready` and that first tick
(at most `pollInterval`, typically 1s), `lastActivity` may still hold a stale
value, so `IdleRemaining()` returns `0` for up to one poll interval immediately
after the system becomes `Ready`. The web UI polls `/status` every 1s, so this
self-corrects within one poll. No guard is added because any guard that
distinguishes "pre-tick" from "genuinely idle past timeout" would reintroduce a
worse glitch at the timeout boundary (jumping from a small number back to the
full timeout).

### API: `IdleReporter` interface and wiring (`internal/api/api.go`)

Define a small interface in the `api` package so handler tests can use a fake,
decoupled from the real `*gateway.Gateway` (mirroring the existing
`StateMachine` interface pattern):

```go
// IdleReporter reports the remaining time before an idle auto-shutdown.
// Implementations return 0 when no shutdown is pending.
type IdleReporter interface {
    IdleRemaining() float64
}
```

Add an optional field to `Server` and a setter (does not break the existing
`NewServer` signature or tests):

```go
type Server struct {
    // ... existing fields ...
    idleReporter IdleReporter // nil = no idle reporter wired
}

func (s *Server) SetIdleReporter(r IdleReporter) {
    s.idleReporter = r
}
```

In `handleStatus`, merge the reporter's value into the status before encoding:

```go
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
    status := s.machine.Status()
    if s.idleReporter != nil {
        status.IdleRemaining = s.idleReporter.IdleRemaining()
    }
    w.Header().Set("Content-Type", "application/json")
    w.WriteHeader(http.StatusOK)
    if err := json.NewEncoder(w).Encode(status); err != nil {
        s.logger.Error("failed to encode status", "error", err)
    }
}
```

When no reporter is set (gateway disabled), `IdleRemaining` stays `0` — the
`StatusResponse` zero value. The existing `StateMachine` interface, `NewServer`
signature, and all existing routes are unchanged.

### main.go wiring (`cmd/dockmind/main.go`)

Inside the existing `if cfg.Gateway.Enabled` block, after creating the gateway,
wire it as the idle reporter:

```go
server.SetIdleReporter(gw)
```

`*gateway.Gateway` satisfies `api.IdleReporter` after the `IdleRemaining()`
method is added. When the gateway is disabled, `SetIdleReporter` is not called,
so `idleRemaining` is always `0` in `GET /status`.

### Web UI (`internal/api/index.html`)

Add a countdown element in the state section, after the existing cooldown
paragraph:

```html
<p class="state__idle" id="idle" hidden>Auto-shutdown in <span id="idle-time">0</span></p>
```

Add CSS for `.state__idle` (small text in the `--muted` colour, matching the
cooldown element's structure):

```css
.state__idle {
  margin: 0;
  font-size: var(--fs-sm);
  color: var(--muted);
  min-height: 1.25rem;
}

.state__idle[hidden] {
  display: none;
}
```

Add `idle` and `idleTime` to the `els` object:

```js
idle: document.getElementById("idle"),
idleTime: document.getElementById("idle-time"),
```

Add a formatting helper that implements the resolution rule (1-minute resolution
while ≥ 60s, 1-second resolution below 60s, rounded up):

```js
function formatIdleRemaining(seconds) {
  const s = Math.ceil(seconds);
  if (s < 60) return s + "s";
  return Math.ceil(s / 60) + "m";
}
```

In `render(data)`, after the cooldown block and before `showFeedback("")`, add:

```js
if (data.idleRemaining > 0) {
  els.idle.hidden = false;
  els.idleTime.textContent = formatIdleRemaining(data.idleRemaining);
} else {
  els.idle.hidden = true;
}
```

**Resolution rule (explicit):** given `remaining` seconds, let `s = ceil(remaining)`.
- `s < 60` → display `"<s>s"` (e.g. `45s`, `1s`).
- `s >= 60` → display `"<ceil(s/60)>m"` (e.g. `125s` → `3m`, `120s` → `2m`,
  `61s` → `2m`, `60s` → `1m`).

The countdown is hidden (`idleRemaining <= 0`) when the gateway is disabled,
`idleTimeout` is `0`, the state is not `Ready`, or a request is in flight.

### OpenAPI spec (`internal/api/openapi.json`)

Add `idleRemaining` to `StatusResponse.properties`:

```json
"idleRemaining": {
  "type": "number",
  "description": "Remaining seconds before an idle auto-shutdown. 0 when the gateway is disabled, idleTimeout is 0, the state is not Ready, or a request is in flight."
}
```

### Polish

- **README** (`README.md`): add `idleRemaining` to the `GET /status` example
  JSON. Add a short note in the Gateway Configuration section explaining that
  `GET /status` reports `idleRemaining` and the web UI shows an auto-shutdown
  countdown when the gateway is enabled with `idleTimeout > 0`.
- **`readme_test.go`**: add an assertion that README contains `idleRemaining`.
- **`docs/product.md`**: add a Features entry for `016-auto-shutdown-timer`.
- **`product_test.go`**: add an assertion that product.md contains
  `016-auto-shutdown-timer`.
- **`AGENTS.md`**: add a note in the "API / state-machine conventions" section
  that `GET /status` reports `idleRemaining` (seconds before idle auto-shutdown;
  0 when not applicable) alongside the existing `cooldownRemaining` note.
- **`configs/config.yaml` / `configs/config-with-gateway.yaml`**: no change
  required — `gateway.idleTimeout` is already present in
  `config-with-gateway.yaml` and the gateway section is absent from the default
  `config.yaml` (gateway disabled by default).

## Tasks

### Task 1 - Gateway: IdleRemaining() method

- `Gateway` with `idleTimeout=0` (disabled) + `IdleRemaining()` called
  - → returns `0`
- `Gateway` with `idleTimeout=50ms` + fake controller state `Off` + `IdleRemaining()` called
  - → returns `0` (only Ready state has a pending idle shutdown)
- `Gateway` with `idleTimeout=50ms` + fake controller state `Starting` + `IdleRemaining()` called
  - → returns `0`
- `Gateway` with `idleTimeout=50ms` + fake controller state `Ready` + `active=0` + `lastActivity=time.Now()` + `IdleRemaining()` called
  - → returns a positive float64 approximately equal to `0.05` (within tolerance)
- `Gateway` with `idleTimeout=50ms` + fake controller state `Ready` + `active=0` + `lastActivity=time.Now().Add(-25ms)` + `IdleRemaining()` called
  - → returns a positive float64 approximately equal to `0.025` (within tolerance)
- `Gateway` with `idleTimeout=50ms` + fake controller state `Ready` + `active=0` + `lastActivity=time.Now().Add(-100ms)` (idle exceeded) + `IdleRemaining()` called
  - → returns `0` (remaining is negative, clamped to 0)
- `Gateway` with `idleTimeout=50ms` + fake controller state `Ready` + `active=1` (request in flight) + `lastActivity=time.Now()` + `IdleRemaining()` called
  - → returns `0` (countdown hidden while a request is active)
- `Gateway` with `idleTimeout=50ms` + fake controller state `Ready` + `active=0` + `lastActivity=time.Now().Add(-100ms)` + `pendingShutdown=true` + `IdleRemaining()` called
  - → returns `0` (grace-period / shutdown-imminent → 0)
- `go test -race ./internal/gateway/`
  - → no data race detected (`activeMu` guards `active`/`lastActivity`; `State()` called outside `activeMu`)
- existing `gateway_test.go` tests + `make test`
  - → all existing tests pass unchanged

### Task 2 - State + API: StatusResponse field, IdleReporter interface, handleStatus merge

- `state.StatusResponse` struct has `IdleRemaining float64 json:"idleRemaining"` field
  - → JSON tag is exactly `idleRemaining`
- `Machine.Status()` (any state, cooldown 0) + JSON marshal
  - → `IdleRemaining` is `0` (machine never populates it)
- `Server` with no idle reporter set + `GET /status` with fake returning `State: "Ready"`
  - → response body contains `"idleRemaining":0`
- `Server` with a fake `IdleReporter` returning `45.5` set via `SetIdleReporter` + `GET /status`
  - → response body contains `"idleRemaining":45.5`
- `Server` with a fake `IdleReporter` returning `0` set via `SetIdleReporter` + `GET /status`
  - → response body contains `"idleRemaining":0`
- existing `api_test.go` tests + `make test`
  - → all existing route tests pass (the `state.StatusResponse` struct gains an
    `IdleRemaining` field; the fake's existing `status` field includes it with
    zero value automatically — no struct change needed in `fakeStateMachine`)
- existing `state_test.go` tests + `make test`
  - → all existing tests pass unchanged (new field is zero value in all existing assertions)

### Task 3 - main.go wiring

- `make build` from repo root
  - → produces `./dockmind` binary without error
- `make test` from repo root
  - → all tests pass

### Task 4 - Web UI: auto-shutdown countdown display

- `GET /` response body contains the string `"Auto-shutdown in"` (label text)
- `GET /` response body contains the string `"idle-time"` (element id)
- `GET /` response body contains the string `"formatIdleRemaining"` (JS helper)
- `GET /` response body contains the string `"idleRemaining"` (JS field access)
- `GET /` response body does NOT contain `http://` or `https://` (existing assertion preserved)
- existing `TestWebUIRoutes` assertions + `make test`
  - → all existing assertions pass (DockMind, /status, /power/on, /power/off, /restart, /docs, fetch, setInterval, llama-swap health, component__dot.is-danger, /favicon.svg, app__logo, rel="icon", cooldown, Cooldown active, 429, loadedModels, no app__logo-link when unset, no Health check label)

### Task 5 - OpenAPI spec

- `GET /openapi.json` + parse JSON
  - → `StatusResponse.properties` contains `idleRemaining`
- existing `api_test.go` OpenAPI assertions + `make test`
  - → all existing OpenAPI assertions pass (all 7 original field names plus `cooldownRemaining` plus `idleRemaining`; all routes; `429` responses; `OpenAIError` schema)

### Task 6 - Polish: README, product.md, AGENTS.md, test assertions

- README `GET /status` example JSON contains `idleRemaining` + `make test`
  - → `readme_test.go` passes with new assertion for `idleRemaining`
- README contains a note about the auto-shutdown countdown + `make test`
  - → `readme_test.go` passes (existing assertions still satisfied)
- README first fenced yaml block unchanged + `make test`
  - → `readme_test.go` yaml test still passes (first block loads via `config.Load` with required fields present)
- README existing assertions still satisfied + `make test`
  - → all existing `readme_test.go` cases pass (all routes, field names, commands, doc links, no `ResultAlreadyInState`/`ResultConflict`, no License section)
- `docs/product.md` Features list references `016-auto-shutdown-timer` + `make test`
  - → `product_test.go` passes with `016-auto-shutdown-timer` assertion
- `docs/product.md` existing assertions still satisfied + `make test`
  - → `product_test.go` existing assertions pass (`004-web-ui`, `006-add-favicon-logo`, `007-openai-gateway`, `008-openai-gateway`, `010-cache-models-json`, `011-cooldown-protection`, `012-egpu-unbind-shutdown`, `013-quiet-off-probe-warnings`, `014-llama-swap-running-endpoint`, `015-fix-loaded-models-empty` still present; non-goal check still passes)
- `AGENTS.md` mentions `idleRemaining` in the API / state-machine conventions section
  - → manual verification (no test enforces AGENTS.md content)
- `make lint` from repo root
  - → `gofmt -l .` prints no file paths
  - → `go vet ./...` reports no issues
- `make build` from repo root
  - → produces `./dockmind` binary without error
- `make test` from repo root
  - → all tests pass (config, state, gateway, api, readme, product, gateway_design)

## Bootstrap

No new dependencies. The project uses Go 1.24.4 with `gopkg.in/yaml.v3 v3.0.1`
only. No new imports are required in any file — all new code uses stdlib
packages already imported by the respective files (`time`, `encoding/json`,
`net/http`).

```bash
make build      # go build -o dockmind ./cmd/dockmind
make test       # go test ./...
make lint       # gofmt -l . && go vet ./...
```

## Technical Context

- **Go 1.24.4** — `go.mod` toolchain. `time.Since`, `time.Now`, `float64`
  arithmetic, and `sync.Mutex` are all stable stdlib APIs used elsewhere in the
  codebase.
- **`float64` for `idleRemaining`** — JSON encodes as a number (e.g. `125.3` or
  `0`). The web UI applies `Math.ceil()` and the resolution rule. Using
  `float64` rather than integer seconds preserves sub-second precision for API
  clients and is consistent with the existing `cooldownRemaining` field.
- **`activeMu` vs `stateMu`** — `activeMu` (in the gateway) guards `active`,
  `lastActivity`, and `pendingShutdown`. `stateMu` (in the state machine) guards
  `state`/`lastError`/timestamps. `IdleRemaining()` calls `machine.State()`
  (which acquires `stateMu`) **before** acquiring `activeMu`, so there is no
  lock-ordering conflict. The state machine never calls back into the gateway.
- **No new external dependencies** — `go.mod` remains
  `gopkg.in/yaml.v3 v3.0.1` only. `go.sum` unchanged.
- **Existing test conventions** — stdlib `testing`, hand-written fakes,
  table-driven tests, no testify/mocking libraries (per `AGENTS.md`). Gateway
  tests use the existing `fakeController` and short durations (50ms). API tests
  use a fake `IdleReporter` (a one-method struct) and `httptest.NewRecorder`.

## Notes

- **`idleRemaining` is gateway-owned, not state-machine-owned.** The state
  machine has no knowledge of the gateway's idle tracking. `Machine.Status()`
  always returns `IdleRemaining: 0`; the API layer overrides it from the
  `IdleReporter` when one is wired. This mirrors the architectural split where
  the state machine owns cooldown (`cooldownRemaining`) but the gateway owns idle
  shutdown.
- **Countdown hidden during active requests.** While `active > 0`, the countdown
  returns `0` and the web UI hides the element. This avoids a misleading
  tick-down during long streaming requests, since `lastActivity` is only
  refreshed at request start/end. When the request completes, the countdown
  reappears at the full `idleTimeout` (inference requests reset `lastActivity`).
- **Model-list requests do not reset the timer.** Per the existing design
  (story 008), `GET /v1/models` does not update `lastActivity` and does not count
  as activity. The countdown therefore continues to tick down during model-list
  polling. This is the intended behaviour and is unchanged by this story.
- **Pre-tick window.** For up to one `pollInterval` (typically 1s) after the
  system reaches `Ready`, `IdleRemaining()` may return `0` because the idle
  watcher has not yet initialised `lastActivity` for the new Ready period. The
  web UI polls every 1s, so this self-corrects within one poll. No guard is
  added (see Implementation approach → Pre-tick window for the rationale).
- **`pendingShutdown` grace period.** Once the idle watcher sets
  `pendingShutdown = true` (Phase 1 of the two-phase shutdown), the remaining
  time is ≤ 0, so `IdleRemaining()` returns `0` and the countdown disappears
  right before shutdown is confirmed on the next tick. This is the desired
  behaviour — the countdown vanishes as shutdown becomes imminent.
- **`SetIdleReporter` is a separate setter from `SetGatewayHandlers`.** Keeping
  them separate means handler tests that only need `SetGatewayHandlers` are
  unaffected, and the `IdleReporter` interface is a minimal one-method contract
  that is trivial to fake. `main.go` calls both setters inside the
  `if cfg.Gateway.Enabled` block.
- **No config changes.** `gateway.idleTimeout` already exists (story 008) and
  controls whether auto-shutdown is enabled. `idleRemaining` is `0` whenever
  `idleTimeout` is `0` or the gateway is disabled — no new config field is
  needed.
