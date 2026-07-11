# Implement OpenAI-Compatible Gateway

## Context

`docs/DockMind_Gateway_Design.md` (delivered by story 007) is a complete design
for evolving DockMind from a manual control daemon into an intelligent
OpenAI-compatible gateway. The design specifies four incremental milestones:
(1) reverse proxy with model-list caching, (2) auto-startup on first request,
(3) idle auto-shutdown, and (4) polish. This story implements all four
milestones, turning the design into production code.

The gateway is opt-in (`gateway.enabled: false` by default). When enabled, it
registers `GET /v1/models` and `/v1/{rest...}` routes that proxy
OpenAI-compatible requests to the llama-swap backend. The first inference
request auto-starts the backend (eGPU + Docker container); after a configurable
idle period with no inference requests, the gateway auto-shuts the backend down.
Manual control endpoints (`/power/on`, `/power/off`, `/restart`) continue to
work when the gateway is enabled.

The design document is the authoritative reference for pseudocode, type
definitions, and error responses. This story specifies the acceptance criteria
and task breakdown; the implementer follows the design document for
implementation details.

## Out of Scope

- **Web UI changes for the gateway.** The web UI continues to show the existing
  manual control panel. Gateway status visibility in the UI is a separate
  future story.
- **Disk persistence of the model-list cache.** The cache is in-memory only
  (`atomic.Pointer[modelCache]`). Persistence across DockMind restarts is
  deferred.
- **Shutdown cancellation.** Explicitly rejected in the design — the two-phase
  grace period reduces the race window without requiring shutdown cancellation.
- **New state machine states (Busy/Idle).** The gateway tracks activity
  externally; the state machine's five states are unchanged.
- **Third-party dependencies.** The gateway uses only Go stdlib
  (`net/http/httputil`, `sync`, `sync/atomic`, `log/slog`, `context`,
  `encoding/json`, `bytes`, `errors`, `fmt`, `net/url`, `time`). `go.mod`
  remains `gopkg.in/yaml.v3 v3.0.1` only.
- **Authentication.** The gateway is intended for a trusted local network, same
  as the existing API.
- **Background polling of dependencies.** `GET /status` continues to query
  dependencies live on each call. The gateway's idle watcher is the only
  additional background goroutine.

## Implementation approach

The design document (`docs/DockMind_Gateway_Design.md`) contains detailed
pseudocode, type definitions, and error response formats for every component.
The implementer should read the relevant design sections before implementing
each task. Key decisions and deviations are summarized here.

### Config (design §Configuration)

Add a `GatewayConfig` struct and a `BackendURL` field to `LlamaSwapConfig`:

```go
type GatewayConfig struct {
    Enabled        bool     `yaml:"enabled"`
    IdleTimeout    Duration `yaml:"idleTimeout"`
    RequestTimeout Duration `yaml:"requestTimeout"`
}

type LlamaSwapConfig struct {
    HealthURL  string `yaml:"healthUrl"`
    BackendURL string `yaml:"backendUrl"`
}
```

Add `Gateway GatewayConfig` to the `Config` struct.

**Defaults** (in `applyDefaults`): when `gateway.enabled` is true and
`requestTimeout` is zero, set it to `120s`. The `idleTimeout` field has **no
code default** — its zero value (`0`) means "auto-shutdown disabled." Users must
set `idleTimeout` explicitly (e.g. `30m`) to enable auto-shutdown. This is a
deliberate deviation from the design's stated default of `30m`: defaulting to
disabled is safer (no surprise shutdowns) and avoids the ambiguity that YAML's
zero value for `Duration` is indistinguishable from an explicit `0s`.

**Validation** (in `validate`, only when `gateway.enabled` is true):
- `llamaSwap.backendUrl` must be non-empty and parseable by `net/url.Parse`.
- `gateway.idleTimeout` must be `>= 0` (`0` is valid = disabled).
- `gateway.requestTimeout` must be `> 0` (always satisfied after default
  application, but validated explicitly for defense-in-depth).

### State machine (design §State Machine)

Add four things to `internal/state.Machine`:

1. **`changeCh chan struct{}`** — initialized in `New()` as
   `make(chan struct{})`, closed-and-recreated in `setState` under `stateMu`.
   See design §State Machine → "state-change signaling" for pseudocode. The
   close-and-recreate pattern ensures every waiter receives exactly one signal
   per state change; new waiters get a fresh channel.

2. **`State() State`** — returns the current state under `stateMu`. Used by the
   gateway's idle watcher to detect transitions to `Ready` and to confirm state
   before shutdown.

3. **`EnsureReady(ctx context.Context) error`** — blocks until the state is
   `Ready` or an error/context cancellation occurs. See design §State Machine →
   "EnsureReady" for full pseudocode and the state-by-state logic table. The
   method reads `state`, `lastError`, and `changeCh` under `stateMu`, releases
   the lock, then acts (calls `PowerOn()` if `Off`) and selects on `changeCh`
   vs. `ctx.Done()`.

4. **`ErrBackendError`** — sentinel error:
   `var ErrBackendError = errors.New("backend in error state")`. Returned as
   `fmt.Errorf("%w: %w", ErrBackendError, lastErr)` from the `Error` state case.
   The gateway handler checks `errors.Is(err, ErrBackendError)` **before**
   `context.DeadlineExceeded` or `context.Canceled` to avoid misclassifying
   startup failures as client timeouts (design §State Machine → "Distinguishing
   error types", `docs/learnings.md`).

**Critical**: `EnsureReady` must release `stateMu` before calling `PowerOn()` to
avoid deadlock with `setState` (which also acquires `stateMu`). `PowerOn()`
internally acquires `transitionMu` (via `TryLock`) and then `stateMu` — holding
`stateMu` across the `PowerOn()` call would deadlock (design §State Machine →
"Critical", §Synchronization).

### Gateway package (design §Go Package Structure, §Reverse Proxy, §Idle Shutdown)

Create `internal/gateway/gateway.go` with:

- **`StateController` interface** (design §Go Package Structure):
  ```go
  type StateController interface {
      EnsureReady(ctx context.Context) error
      PowerOff() state.PowerResult
      State() state.State
  }
  ```

- **`Gateway` struct** holding `*httputil.ReverseProxy`, `StateController`,
  `*slog.Logger`, `idleTimeout time.Duration`, `requestTimeout time.Duration`,
  `pollInterval time.Duration`, active-request tracking (`active int`,
  `lastActivity time.Time`, `pendingShutdown bool` under `activeMu sync.Mutex`),
  and `cachedModels atomic.Pointer[modelCache]`. The idle watcher's internal
  context and cancel function are also stored for `StopIdleWatcher()`.

- **`NewGateway(controller StateController, backendURL string, logger *slog.Logger, idleTimeout, requestTimeout, pollInterval time.Duration) *Gateway`**
  — parses `backendURL` via `net/url.Parse` (panics or returns error on invalid
  URL; config validation ensures it is valid before this point). Configures the
  `ReverseProxy` with `Rewrite` (using `pr.SetURL(backendUrl)` +
  `pr.SetXForwarded()`), `FlushInterval: -1`, and a shared `ErrorHandler`.

- **`Handler() http.Handler`** — the inference handler. Marks request active
  (increment `active`, set `lastActivity = time.Now()`, clear `pendingShutdown`)
  under `activeMu` **before** calling `EnsureReady`. Defers decrement (and
  `lastActivity` update). Calls `EnsureReady` with a context derived from
  `r.Context()` with a `requestTimeout` deadline. Maps errors to HTTP responses
  (see Error Handling below). Proxies via `responseTracker` wrapper. See design
  §Startup Sequence for the step-by-step flow.

- **`ModelsHandler() http.Handler`** — the model-list handler. Checks
  `State()` first: `Error` → 503 `backend_error`; non-Ready with cache → 200 +
  `X-DockMind-Cached: true`; non-Ready without cache → 503
  `model_cache_unavailable`; Ready → buffer-first proxy via `bufferingWriter`,
  cache on success. Does **not** call `EnsureReady`, does **not** reset idle
  timer, does **not** count as activity (except incrementing `active` for race
  safety when proxying to a Ready backend — see design §Idle Shutdown → "Model
  list endpoint" → behavior table). See design §Idle Shutdown → "Model list
  endpoint" for full pseudocode.

- **`StartIdleWatcher(ctx context.Context)`** / **`StopIdleWatcher()`** —
  starts/stops the idle watcher goroutine. `StartIdleWatcher` creates an
  internal cancellable context from `ctx` and launches the goroutine, returning
  immediately. `StopIdleWatcher` cancels the internal context. The watcher ticks
  at `pollInterval`, tracks `prevReady` across ticks, and implements the
  two-phase grace period shutdown (design §Idle Shutdown → "Idle watcher loop
  (revised)"). When `idleTimeout == 0`, the watcher does not run (or exits
  immediately).

- **`responseTracker`** — wraps `http.ResponseWriter`, tracks `headerWritten
  bool`, implements `Unwrap() http.ResponseWriter` for
  `http.ResponseController.Flush()` support. Without `Unwrap`, `FlushInterval:
  -1` silently fails to flush SSE streams (design §Reverse Proxy → "Streaming
  error handling", `docs/learnings.md`).

- **`bufferingWriter`** — captures full response (status code, headers, body)
  without sending to client. Has `failed bool` flag set by `ErrorHandler` on
  proxy failure. Does **not** implement `Unwrap()` — flush errors are silently
  discarded for small non-streaming responses (design §Idle Shutdown → "Model
  list endpoint" → "buffer-first approach", `docs/learnings.md`).

- **`modelCache`** — holds `body []byte`, `contentType string`,
  `refreshedAt time.Time`. Stored as `atomic.Pointer[modelCache]`. Readers call
  `Load()` for an immutable snapshot; the writer calls `Store()` after a
  successful 200 OK response. The `body` slice is never mutated after
  construction.

- **OpenAI error types** — `openAIError` and `openAIErrorBody` structs for JSON
  error responses:
  ```go
  type openAIErrorBody struct {
      Message string `json:"message"`
      Type    string `json:"type"`
      Code    string `json:"code"`
  }
  type openAIError struct {
      Error openAIErrorBody `json:"error"`
  }
  ```

**Error handling** (design §Error Handling):

| `EnsureReady` returns | HTTP status | `type` | `code` |
|---|---|---|---|
| `errors.Is(err, ErrBackendError)` (checked first) | 503 | `service_unavailable` | `backend_error` |
| `errors.Is(err, context.DeadlineExceeded)` | 503 | `service_unavailable` | `startup_timeout` |
| `errors.Is(err, context.Canceled)` | (no response — client gone) | — | — |
| Proxy connection fails (before headers) | 502 | `bad_gateway` | `proxy_error` |

**`ErrorHandler`** (design §Reverse Proxy → "Streaming error handling"): logs
at ERROR, checks for `*bufferingWriter` (sets `failed = true`, returns), checks
for `*responseTracker` with `headerWritten` (returns without writing — headers
already sent), otherwise writes 502 JSON with `code: "proxy_error"`.

### API integration (design §Go Package Structure → "Modified packages")

The `Server` struct in `internal/api` gains two optional unexported fields:
```go
gatewayHandler  http.Handler // nil = gateway disabled
modelsHandler   http.Handler // nil = gateway disabled
```

Add a setter method (does not break existing `NewServer` signature or tests):
```go
func (s *Server) SetGatewayHandlers(inference, models http.Handler) {
    s.gatewayHandler = inference
    s.modelsHandler = models
}
```

In `Handler()`, after the existing route registrations, conditionally register:
```go
if s.modelsHandler != nil {
    mux.Handle("GET /v1/models", s.modelsHandler)
}
if s.gatewayHandler != nil {
    mux.Handle("/v1/{rest...}", s.gatewayHandler)
}
```

`GET /v1/models` is registered before `/v1/{rest...}` so the more specific
pattern takes precedence (Go 1.22+ ServeMux). The existing `StateMachine`
interface, `NewServer` signature, and all existing routes are unchanged.

### main.go wiring (design §Go Package Structure → "cmd/dockmind/main.go")

After creating the `Machine` and `Server`, if `cfg.Gateway.Enabled`:
1. Create the `Gateway` via `gateway.NewGateway(machine,
   cfg.LlamaSwap.BackendURL, logger,
   cfg.Gateway.IdleTimeout.Duration(),
   cfg.Gateway.RequestTimeout.Duration(),
   cfg.GPU.PollInterval.Duration())`.
2. Call `server.SetGatewayHandlers(gw.Handler(), gw.ModelsHandler())`.
3. Create a cancellable context before the signal wait:
   `gwCtx, gwCancel := context.WithCancel(context.Background())`.
4. Start the idle watcher: `gw.StartIdleWatcher(gwCtx)`.
5. On signal: call `gwCancel()` (stops idle watcher), then
   `httpServer.Shutdown(ctx)`, then `machine.Wait()`.

The `Machine` satisfies `StateController` after the state machine changes (it
has `EnsureReady`, `PowerOff`, and `State()`).

### Polish (design §Incremental Implementation Plan → Milestone 4)

- **OpenAPI spec** (`internal/api/openapi.json`): add `/v1/models` (GET) and
  `/v1/chat/completions` (POST) paths with the OpenAI error response schema.
  These are representative gateway endpoints; the catch-all `/v1/{rest...}` is
  noted in the path description. Add an `OpenAIError` schema to
  `components.schemas`.
- **README** (`README.md`): add gateway routes to the API Endpoints table, add
  a "Gateway Configuration" section with a second fenced yaml block (the first
  block is test-enforced and must remain the existing manual-control config),
  add `readme_test.go` assertions for the new content.
- **product.md**: add an "OpenAI Gateway" Features entry referencing
  `008-openai-gateway` (keep the existing `007-openai-gateway` design entry).
  Update the Known Limitations to reflect that the gateway is now implemented
  but opt-in. Update the opening paragraph.
- **product_test.go**: add `008-openai-gateway` assertion.
- **configs/config.yaml**: add `gateway:` section with `enabled: false` and add
  `backendUrl` to `llamaSwap`.

## Tasks

### Task 1 - Config: GatewayConfig, BackendURL, validation, and defaults

- `gateway.enabled: true` + `llamaSwap.backendUrl: http://localhost:1234` in config YAML + `config.Load`
  - → loads successfully
  - → `cfg.Gateway.Enabled` is `true`
  - → `cfg.LlamaSwap.BackendURL` is `"http://localhost:1234"`
- `gateway.enabled: true` + `llamaSwap.backendUrl` empty + `config.Load`
  - → returns error containing `"backendUrl"`
- `gateway.enabled: true` + `llamaSwap.backendUrl: "not a url"` + `config.Load`
  - → returns error (invalid URL)
- `gateway.enabled: false` (or `gateway` section absent) + `llamaSwap.backendUrl` empty + `config.Load`
  - → loads successfully (backendUrl not validated when gateway disabled)
- `gateway.enabled: true` + `gateway.idleTimeout: 0s` + `config.Load`
  - → loads successfully (`0` is valid = disabled)
- `gateway.enabled: true` + `gateway.idleTimeout: -1s` + `config.Load`
  - → returns error (must be `>= 0`)
- `gateway.enabled: true` + `gateway.requestTimeout` absent + `config.Load`
  - → loads successfully with `cfg.Gateway.RequestTimeout` equal to `120s`
- `gateway.enabled: true` + `gateway.requestTimeout: 0s` + `config.Load`
  - → loads successfully with `cfg.Gateway.RequestTimeout` equal to `120s` (zero overridden by default)
- existing `config_test.go` cases + `make test`
  - → all existing cases pass unchanged (minimal config, full config, error cases)

### Task 2 - State machine: EnsureReady, State(), changeCh, ErrBackendError

- `Machine` in `Ready` state + `EnsureReady(ctx)` called
  - → returns `nil` immediately (no blocking)
- `Machine` in `Error` state with `lastError` set + `EnsureReady(ctx)` called
  - → returns error wrapping `lastError`
  - → `errors.Is(err, state.ErrBackendError)` returns `true`
- `Machine` in `Off` state + `EnsureReady(ctx)` called with fakes that succeed
  - → `PowerOn()` is called (state transitions to `Starting`)
  - → blocks until state becomes `Ready`
  - → returns `nil`
- `Machine` in `Starting` state + `EnsureReady(ctx)` called
  - → blocks until state becomes `Ready`
  - → returns `nil` (no second `PowerOn` called)
- `Machine` in `ShuttingDown` state + `EnsureReady(ctx)` called with fakes that succeed
  - → blocks until state becomes `Off`
  - → then triggers `PowerOn()`
  - → blocks until state becomes `Ready`
  - → returns `nil`
- `Machine` in `Off` state + `EnsureReady(ctx)` called with already-cancelled context
  - → returns `context.Canceled`
  - → state machine is not in `Error` state (startup may continue in background)
- `Machine` in `Off` state + `EnsureReady(ctx)` called with context deadline shorter than startup
  - → returns `context.DeadlineExceeded`
  - → state machine continues startup in background (state is `Starting` or `Ready`, not `Error`)
- N concurrent `EnsureReady(ctx)` calls from `Off` state with fakes that succeed
  - → exactly one `PowerOn()` returns `ResultAccepted`
  - → all N calls eventually return `nil` when state becomes `Ready`
- `Machine.State()` called from any state
  - → returns the current `State` value
- existing `state_test.go` tests + `make test`
  - → all existing tests pass unchanged (PowerOn, PowerOff, Restart, startup/shutdown errors, concurrent transitions, status)

### Task 3 - Gateway package: reverse proxy, model list, auto-start, idle shutdown

- `StateController.EnsureReady` returns `nil` (Ready) + `POST /v1/chat/completions` to gateway `Handler()`
  - → request forwarded to backend with original path `/v1/chat/completions`, original method, original headers
  - → response body forwarded correctly to client
- `StateController.EnsureReady` returns `nil` + backend returns SSE `text/event-stream` response with multiple chunks
  - → response chunks arrive separately (not buffered into one write)
  - → `FlushInterval: -1` enables immediate flushing (verify via `responseTracker.Unwrap()` reaching the real `ResponseWriter`)
- `StateController.EnsureReady` returns `fmt.Errorf("%w: %w", state.ErrBackendError, errors.New("gpu timeout"))` + request to gateway
  - → HTTP 503
  - → `Content-Type: application/json`
  - → response body contains `"code":"backend_error"`
- `StateController.EnsureReady` returns `context.DeadlineExceeded` + request to gateway
  - → HTTP 503
  - → response body contains `"code":"startup_timeout"`
- `StateController.EnsureReady` returns `context.Canceled` + request to gateway
  - → no response body written (response recorder body is empty)
  - → no `WriteHeader` call (response recorder default code 200, meaning nothing was explicitly written)
- Backend server closed before request + `StateController.EnsureReady` returns `nil`
  - → HTTP 502
  - → response body contains `"code":"proxy_error"`
- Backend sends headers then closes connection mid-stream + `StateController.EnsureReady` returns `nil`
  - → `responseTracker` records `headerWritten = true`
  - → `ErrorHandler` does not write a JSON error (headers already sent)
  - → client sees truncated stream (no JSON error appended after partial response)
- Active counter after a completed inference request (success or `EnsureReady` failure)
  - → `active` is `0` (incremented before `EnsureReady`, decremented by defer)
- Backend Ready + `GET /v1/models` via `ModelsHandler()` + backend returns 200 with body and `Content-Type: application/json`
  - → response forwarded to client with correct body and content-type
  - → cache populated (`cachedModels.Load()` returns non-nil with matching body, content-type, and recent timestamp)
- Backend Ready + `GET /v1/models` + backend returns 500
  - → HTTP 502 response
  - → cache NOT updated (existing cache unchanged if one existed)
- Backend Ready + `GET /v1/models` + backend connection drops mid-response
  - → `bufferingWriter.failed` is `true`
  - → HTTP 502 response
  - → cache NOT updated (truncation detected via `failed` flag)
- Backend `State() == Off` + cache exists + `GET /v1/models`
  - → HTTP 200 with cached body
  - → `X-DockMind-Cached: true` response header
  - → `EnsureReady` NOT called (verify via fake that `EnsureReady` call count is 0)
  - → `active` NOT incremented (verify `active` is 0 after request)
  - → `lastActivity` NOT updated (verify timestamp unchanged)
- Backend `State() == Off` + no cache + `GET /v1/models`
  - → HTTP 503
  - → response body contains `"code":"model_cache_unavailable"`
- Backend `State() == Error` + cache exists + `GET /v1/models`
  - → HTTP 503
  - → response body contains `"code":"backend_error"` (cache NOT served)
- Backend `State() == Starting` + cache exists + `GET /v1/models`
  - → HTTP 200 with cached body, `X-DockMind-Cached: true`
- Concurrent `GET /v1/models` reads + cache refresh + `go test -race ./internal/gateway/`
  - → no data race detected
- `idleTimeout > 0` + state `Ready` + no active requests + idle timeout elapses (use short timeout in test)
  - → `PowerOff()` called after two-tick grace period (verify `pendingShutdown` was set on first tick, `PowerOff` called on second tick)
- `idleTimeout > 0` + state `Ready` + active request in flight
  - → `PowerOff()` NOT called
- `idleTimeout > 0` + state `Ready` + request arrives during grace period (after `pendingShutdown` set, before confirm tick)
  - → `pendingShutdown` cleared
  - → `PowerOff()` NOT called
- Manual power-on (state transitions to `Ready` with no gateway request) + `idleTimeout > 0`
  - → idle timer initialized (`lastActivity` set to ~now via `prevReady` transition detection)
  - → no immediate `PowerOff()` call
- `GET /v1/models` polling while state is `Ready` with cache + `idleTimeout > 0`
  - → idle timer NOT reset by model-list requests
  - → `PowerOff()` called after idle timeout elapses
- `idleTimeout: 0` + `StartIdleWatcher` called
  - → idle watcher goroutine does not run (or exits immediately)
  - → `PowerOff()` never called
- `StartIdleWatcher(ctx)` + context cancelled
  - → goroutine stops cleanly (no goroutine leak, no panic)
- `StopIdleWatcher()` called after `StartIdleWatcher`
  - → goroutine stops cleanly

### Task 4 - API integration and main.go wiring

- `Server` with no gateway handlers set + `GET /v1/models` request
  - → HTTP 404 (no route registered)
- `Server` with no gateway handlers set + `POST /v1/chat/completions` request
  - → HTTP 404
- `Server` with gateway handlers set (via `SetGatewayHandlers`) + `GET /v1/models` request
  - → gateway models handler is invoked (verify via test handler that records the call)
- `Server` with gateway handlers set + `POST /v1/chat/completions` request
  - → gateway inference handler is invoked
- `Server` with gateway handlers set + existing routes (`GET /status`, `POST /power/on`, `POST /power/off`, `POST /restart`, `GET /health`, `GET /docs`, `GET /`)
  - → all existing routes return same status codes as before (regression)
- `main.go` with `gateway.enabled: false` in config + `make build`
  - → builds successfully
  - → no gateway code executed at runtime (gateway handlers not set)
- `main.go` with `gateway.enabled: true` in config + `make build`
  - → builds successfully
  - → gateway created, handlers wired, idle watcher started (verify via integration test with httptest backend + fake state machine: full flow request → auto-start → proxy → response → idle → auto-shutdown)
- existing `api_test.go` tests + `make test`
  - → all existing tests pass unchanged

### Task 5 - Polish: OpenAPI spec, README, product.md, configs

- `internal/api/openapi.json` contains `/v1/models` path with GET method + `make test`
  - → `api_test.go` OpenAPI assertions pass (spec parses as JSON, contains `/v1/models`)
- `internal/api/openapi.json` contains `/v1/chat/completions` path with POST method + `make test`
  - → spec contains `/v1/chat/completions`
- `internal/api/openapi.json` contains an `OpenAIError` schema in `components.schemas` + `make test`
  - → spec contains `OpenAIError` schema with `error` property containing `message`, `type`, `code`
- README API Endpoints table includes gateway routes (`/v1/models`, `/v1/chat/completions`) + `make test`
  - → `readme_test.go` passes with new assertions for `/v1/chat/completions` and `gateway`
- README first fenced yaml block unchanged + `make test`
  - → `readme_test.go` yaml test still passes (first block loads via `config.Load` with required fields present)
- README existing assertions still satisfied + `make test`
  - → all existing `readme_test.go` cases pass (all 5 original routes, `/docs`, `/`, all 7 field names, `make build`, `make test`, `make lint`, `--config`, `./config.yaml`, links to both docs)
  - → README still does NOT contain `ResultAlreadyInState` or `ResultConflict`
  - → README still has no License section
- `docs/product.md` Features list references `008-openai-gateway` + `make test`
  - → `product_test.go` passes with `008-openai-gateway` assertion
- `docs/product.md` existing assertions still satisfied + `make test`
  - → `product_test.go` existing assertions pass (`004-web-ui`, `006-add-favicon-logo`, `007-openai-gateway` still present; non-goal check still passes)
- `configs/config.yaml` contains `gateway:` section with `enabled: false` and `llamaSwap.backendUrl`
  - → `config.Load` on `configs/config.yaml` succeeds
- `make lint` from repo root
  - → `gofmt -l .` prints no file paths
  - → `go vet ./...` reports no issues
- `make test` from repo root
  - → all tests pass (config, state, gateway, api, readme, product, gateway_design)
- `make build` from repo root
  - → produces `./dockmind` binary without error

## Bootstrap

No new dependencies. The project uses Go 1.24.4 with `gopkg.in/yaml.v3 v3.0.1`
only. The gateway uses exclusively Go stdlib packages.

```bash
make build      # go build -o dockmind ./cmd/dockmind
make test       # go test ./...
make lint       # gofmt -l . && go vet ./...
```

## Technical Context

- **Go 1.24.4** — `go.mod` toolchain. All gateway features
  (`httputil.ReverseProxy` with `Rewrite` API, `FlushInterval: -1`,
  `atomic.Pointer`, ServeMux `{rest...}` wildcards, `http.ResponseController`)
  are stable in Go 1.22+ and fully supported in 1.24.4.
- **`net/http/httputil.ReverseProxy`** — stdlib reverse proxy. The `Rewrite`
  field (Go 1.22+) receives a `*httputil.ProxyRequest` with both inbound (`pr.In`)
  and outbound (`pr.Out`) requests. `pr.SetURL(backendUrl)` sets scheme/host on
  the outbound URL and clears `Out.Host` (sets it to `""`). `pr.SetXForwarded()`
  appends the client IP to `X-Forwarded-For` and sets `X-Forwarded-Host` /
  `X-Forwarded-Proto`. `FlushInterval: -1` flushes after every write for SSE
  streaming. When `Rewrite` is set, `ReverseProxy` does not automatically add
  `X-Forwarded-For` — `SetXForwarded()` is required.
- **`http.ResponseController`** — Go 1.20+ API for flushing. `ReverseProxy` uses
  it internally with `FlushInterval: -1`. The `responseTracker` wrapper must
  implement `Unwrap() http.ResponseWriter` so `ResponseController` can reach the
  concrete `ResponseWriter`'s `Flush` method. Without `Unwrap`, flush returns
  `errNotSupported` and SSE streaming silently breaks (see `docs/learnings.md`:
  "Code reviewer validates Go stdlib internals in design pseudocode").
- **`atomic.Pointer[modelCache]`** — Go 1.19+ generic atomic pointer. `Load()`
  returns an immutable `*modelCache` snapshot (lock-free read). `Store()`
  atomically swaps the pointer. The `body []byte` in each `modelCache` is never
  mutated after construction, so concurrent `w.Write(cached.body)` calls are
  safe.
- **`errors.Is` with sentinel errors** — `ErrBackendError` is a sentinel
  (`var ErrBackendError = errors.New(...)`). `EnsureReady` returns
  `fmt.Errorf("%w: %w", ErrBackendError, lastErr)` from the `Error` state. The
  handler checks `errors.Is(err, ErrBackendError)` **before**
  `errors.Is(err, context.DeadlineExceeded)` because `lastError` may itself wrap
  `context.DeadlineExceeded` (e.g. "gpu detection timeout: context deadline
  exceeded" from the existing `poll` function). See `docs/learnings.md`.
- **ServeMux pattern precedence** — Go 1.22+ ServeMux: more specific patterns
  take precedence. `GET /v1/models` matches before `/v1/{rest...}`. `GET
  /status` still matches `/status` exactly. A `POST /v1/models` (non-standard
  OpenAI endpoint) falls through to `/v1/{rest...}` and is treated as an
  inference request.
- **`bufferingWriter` vs `responseTracker`** — `bufferingWriter` captures the
  full response before sending to the client (for model-list caching,
  non-streaming). It does NOT implement `Unwrap()` — flush errors are silently
  discarded for small responses. `responseTracker` wraps the ResponseWriter for
  streaming (inference), implements `Unwrap()` for flush support, and tracks
  `headerWritten` to prevent JSON corruption of mid-stream responses. See
  `docs/learnings.md`: "Buffer-first ResponseWriter wrappers are simpler than
  tee for non-streaming responses".
- **No new external dependencies** — `go.mod` remains
  `gopkg.in/yaml.v3 v3.0.1` only. `go.sum` unchanged.
- **Existing test conventions** — stdlib `testing` + `net/http/httptest`,
  hand-written fakes, table-driven tests, no testify/mocking libraries (per
  `AGENTS.md`). Gateway tests use a fake `StateController` and
  `httptest.NewServer` for the backend. State machine tests use the existing
  fakes (`fakePower`, `fakeGPU`, `fakeDocker`, `fakeHealth`).

## Notes

- **`idleTimeout` default deviation.** The design document states the default
  for `gateway.idleTimeout` is `30m`. The implementation defaults to `0`
  (disabled) — users must set `idleTimeout` explicitly to enable auto-shutdown.
  This is safer (no surprise shutdowns) and avoids the YAML zero-value
  ambiguity (`0s` is indistinguishable from "not specified" with the `Duration`
  value type). The `requestTimeout` default of `120s` IS applied when `enabled`
  is true and the field is zero, because `0` is invalid for `requestTimeout`
  (must be `> 0`).
- **Design document is the authoritative reference.** The implementer should
  read `docs/DockMind_Gateway_Design.md` sections relevant to each task before
  implementing. The design contains complete pseudocode for `EnsureReady`,
  `setState` modification, the idle watcher loop, the `ErrorHandler`,
  `responseTracker`, `bufferingWriter`, and the model-list handler. This story
  specifies the acceptance criteria and task breakdown; it does not repeat all
  pseudocode.
- **`Machine` satisfies `StateController`.** After adding `EnsureReady`,
  `State()`, and the existing `PowerOff()`, the `*state.Machine` type satisfies
  the `gateway.StateController` interface. `main.go` passes the machine directly
  to `gateway.NewGateway`.
- **`api.StateMachine` interface unchanged.** The gateway handlers are separate
  from the existing API handlers. The `Server` struct gains optional handler
  fields set via `SetGatewayHandlers`. The existing `StateMachine` interface,
  `NewServer` signature, and all existing routes are unchanged — existing tests
  pass without modification.
- **README test enforcement.** `readme_test.go` validates README content. The
  first fenced yaml block must still load via `config.Load`. Gateway config is
  added in a **second** yaml block (in a "Gateway Configuration" section), not
  in the first block. New `readme_test.go` assertions are added for gateway
  routes and config. The README must NOT contain `ResultAlreadyInState` or
  `ResultConflict`, and must have no License section.
- **`gateway_design_test.go` unchanged.** The design document is not modified
  in this story. The existing `gateway_design_test.go` assertions continue to
  pass.
- **OpenAPI spec is representative, not exhaustive.** The catch-all
  `/v1/{rest...}` route proxies all OpenAI-compatible paths. The OpenAPI spec
  documents `/v1/models` (GET) and `/v1/chat/completions` (POST) as
  representative endpoints — OpenAPI 3.0 does not have a clean way to represent
  catch-all routes.
- **`configs/config.yaml` is not test-enforced.** Unlike the README yaml block,
  `configs/config.yaml` is not validated by any test. It is updated for
  documentation purposes to show the gateway config options with `enabled:
  false`.
- **Idle watcher tick interval.** The idle watcher ticks at `gpu.pollInterval`
  (reused as in the existing codebase convention, design §Idle Shutdown). This
  is passed to `NewGateway` as the `pollInterval` parameter.
- **`pendingShutdown` is the grace-period flag.** The two-phase shutdown sets
  `pendingShutdown = true` on the first idle tick (Phase 1 — reserve), then
  calls `PowerOff()` on the next tick if `active == 0` and `pendingShutdown` is
  still true (Phase 2 — confirm). A request arriving during the grace period
  clears `pendingShutdown` and cancels the shutdown. See design §Idle Shutdown →
  "Shutdown reservation".
- **`changeCh` initialization.** The `changeCh` field must be initialized in
  `New()` as `make(chan struct{})` to avoid closing a nil channel in
  `setState`. The close-and-recreate pattern in `setState` closes the current
  channel and creates a new one under `stateMu`.
- **`EnsureReady` error classification order.** The handler must check
  `errors.Is(err, ErrBackendError)` first, then `errors.Is(err,
  context.DeadlineExceeded)`, then `errors.Is(err, context.Canceled)`. This
  order is critical because a startup failure's `lastError` may wrap
  `context.DeadlineExceeded` in its chain (e.g. "gpu detection timeout: context
  deadline exceeded"). The sentinel takes precedence.
