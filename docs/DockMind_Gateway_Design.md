# DockMind as an Intelligent OpenAI Gateway — Design Document

This document is the detailed design for evolving DockMind from a manual control
daemon into an intelligent OpenAI-compatible gateway. It is a **design-only**
deliverable: no production code is written here. Each milestone in the
[Incremental Implementation Plan](#incremental-implementation-plan) is a
separate follow-up story that can be implemented without further design
decisions.

The design answers all 12 areas requested in
`docs/DockMind_as_Gateway_Proposal.md` and includes a final critical review.

## High-Level Architecture

```
Client (Hermes / OpenWebUI / curl)
  |
  |  OpenAI-compatible API  (/v1/*)
  v
+-----------------------------+
|         DockMind            |
|  +-----------------------+  |
|  |  Gateway (new)        |  |
|  |  httputil.ReverseProxy|  |
|  +-----+---------+------+  |
|        |         |         |
|   EnsureReady   PowerOff   |
|   (per request) (idle)     |
|        v         v         |
|  +-----------------------+ |
|  |  State Machine (ex.)  | |
|  +--+----+----+----+-----+ |
+----+----+----+----+--------+
     |    |    |    |
     v    |    v    v
  Shelly  |  Docker  Health
     |    |    |       |
     v    |    v       v
   eGPU   | llama-swap  |
     |    |    |       |
     v    |    v       |
 nvidia-smi  |       |
             v       |
          (proxied)  |
```

### Major components and responsibilities

- **Gateway** (new `internal/gateway` package): receives `/v1/*` requests,
  ensures the backend is Ready before proxying, tracks active requests, and runs
  an idle watcher goroutine. It is an opt-in feature controlled by
  `gateway.enabled` in `config.yaml`. When disabled, no gateway route is
  registered and DockMind behaves exactly as it does today.

- **State Machine** (existing `internal/state`): the existing Off / Starting /
  Ready / ShuttingDown / Error state machine is preserved with no new states.
  The gateway adds two things on top of it: an `EnsureReady(ctx)` method and a
  state-change signaling channel. All existing transition rules, concurrency
  guarantees (`transitionMu` TryLock), and the `WaitGroup`-backed `Wait()` are
  unchanged.

- **Existing clients** (shelly, gpu, docker, health): completely unchanged.
  The gateway never talks to Shelly, the GPU, Docker, or the health endpoint
  directly — it goes through the state machine, which orchestrates these
  clients exactly as it does for manual `POST /power/on` and `POST /power/off`.

### Interaction flow

1. A client sends an OpenAI-compatible request to `/v1/chat/completions` (or
   any `/v1/*` path).
2. The Gateway handler **immediately** marks the request as active:
   `activeMu.Lock(); active++; lastActivity = time.Now(); activeMu.Unlock()`.
   A `defer` decrements `active` and updates `lastActivity` when the request
   finishes (including on error/panic). This ensures that a request waiting for
   startup is still counted as active and prevents idle shutdown during the
   wait.
3. The handler calls `machine.EnsureReady(r.Context())`.
4. If the backend is not Ready, `EnsureReady` triggers startup (or waits for an
   in-progress startup) and blocks until the state becomes Ready or an error
   occurs.
5. Once `EnsureReady` returns `nil`, the handler proxies the request to
   llama-swap via `httputil.ReverseProxy`. The active-request counter remains
   incremented for the entire duration (wait + proxy) and is decremented by the
   `defer` when the handler returns.
6. The response is streamed back to the client.
7. In the background, an idle watcher goroutine monitors activity. When no
   requests have been active for `gateway.idleTimeout` (default 30m), it calls
   `machine.PowerOff()` to shut the system down.

Manual control (`POST /power/on`, `/power/off`, `/restart`) continues to work
when the gateway is enabled, so the user can shut the system down immediately
when they know they are done.

## Go Package Structure

### New package: `internal/gateway`

The `Gateway` struct holds:

- `*httputil.ReverseProxy` — the stdlib reverse proxy configured with the
  backend URL.
- `StateController` interface — decouples the gateway from the real state
  machine for testing (same pattern as `internal/api`'s `StateMachine`
  interface).
- `*slog.Logger` — structured logger.
- `idleTimeout time.Duration` — idle period before auto-shutdown.
- `requestTimeout time.Duration` — timeout for the `EnsureReady` wait phase.
- Active-request tracking: `active int`, `lastActivity time.Time`, and
  `pendingShutdown bool`, all guarded by `activeMu sync.Mutex`.
  `pendingShutdown` is the shutdown-reservation flag (see
  [Idle Shutdown](#idle-shutdown)).
- Model-list cache: `cachedModels atomic.Pointer[modelCache]`. Stores the
  last successful `GET /v1/models` response from the backend so it can be
  served while the backend is sleeping (see
  [Model List Endpoint](#model-list-endpoint)). The cache is in-memory
  only; persistence across DockMind restarts is not required. `atomic.Pointer`
  (Go 1.19+) provides lock-free reads and atomic pointer swaps — no mutex
  needed.

The `StateController` interface (for testability):

```go
type StateController interface {
    EnsureReady(ctx context.Context) error
    PowerOff() state.PowerResult
    State() state.State
}
```

`State()` returns the current state-machine state. The idle watcher uses it to
detect transitions to `Ready` (which reset the idle timer) and to confirm the
state is `Ready` before initiating shutdown. This prevents the idle watcher from
shutting down a system that was manually powered on while no gateway requests
have arrived yet (see [Idle Timer Initialization](#idle-timer-initialization)).

Public methods:

- `Handler() http.Handler` — returns the `/v1/{rest...}` inference handler that
  marks the request active, ensures readiness, and proxies to the backend.
- `ModelsHandler() http.Handler` — returns the `GET /v1/models` handler that
  caches the last successful backend response and serves it from cache when
  the backend is not Ready, without resetting the idle timer (see
  [Model List Endpoint](#model-list-endpoint)).
- `StartIdleWatcher(ctx context.Context)` — starts the idle watcher goroutine
  and returns immediately. The goroutine runs until `ctx` is cancelled or
  `StopIdleWatcher()` is called (used with a context tied to SIGINT/SIGTERM).
- `StopIdleWatcher()` — cancels the idle watcher's internal context, stopping
  the goroutine cleanly.

### Modified packages

- **`internal/state`** (modified): adds `EnsureReady(ctx context.Context) error`,
  a `State() State` query method, and a state-change channel (`changeCh`). No
  new states. See [State Machine](#state-machine).

- **`internal/config`** (modified): adds a `GatewayConfig` struct with `Enabled`
  (`bool`), `IdleTimeout` (`Duration`), and `RequestTimeout` (`Duration`)
  fields. Adds `BackendURL` (`string`) to `LlamaSwapConfig`. See
  [Configuration](#configuration).

- **`internal/api`** (modified): conditionally registers the gateway routes
  when `gateway.enabled` is true. Two patterns are registered: `GET /v1/models`
  (model list, lightweight) and `/v1/{rest...}` (inference, catch-all). The
  `Server` struct gains an optional `gatewayHandler http.Handler` and
  `modelsHandler http.Handler` field; when non-nil, both are registered on the
  ServeMux alongside the existing routes. See
  [Model List Endpoint](#model-list-endpoint) and
  [Route Registration](#route-registration).

- **`cmd/dockmind/main.go`** (modified): when `gateway.enabled` is true, creates
  the `Gateway`, registers both its inference handler and model-list handler
  with the API server, starts the idle watcher with a context derived from the
  SIGINT/SIGTERM signal handler, and ensures the watcher is stopped during
  shutdown.

## State Machine

### Existing states (unchanged)

The five existing states are preserved with no additions:

| State | Description |
|-------|-------------|
| `Off` | eGPU powered off, llama-swap container stopped. |
| `Starting` | Startup transition in progress (Shelly ON → wait GPU → Docker start → wait health). |
| `Ready` | eGPU on, llama-swap running and healthy. Requests can be proxied. |
| `ShuttingDown` | Shutdown transition in progress (Docker stop → wait stop → Shelly OFF → wait GPU off). |
| `Error` | A transition failed. Only `POST /power/off` is accepted from this state. |

No `Busy` or `Idle` states are added. The gateway tracks activity externally
via the active-request counter and idle timer. This avoids transition
complexity and keeps existing tests passing.

### New: `EnsureReady(ctx context.Context) error`

This is the core method the gateway calls before proxying. It is synchronous
and blocks until the state is Ready or an error/context cancellation occurs.

Logic by current state:

- **`Ready`** → return `nil` immediately.
- **`Error`** → return a sentinel `ErrBackendError` wrapping the last error.
  This represents a **failed transition** (startup or shutdown) — the state
  machine is in `Error` and requires manual `POST /power/off` to reset. The
  sentinel is necessary because `lastError` may itself wrap
  `context.DeadlineExceeded` (e.g. "GPU detection timeout" from the existing
  `poll` function), which would make `errors.Is(err, context.DeadlineExceeded)`
  return true and misclassify a startup failure as a client timeout.
- **`Off`** → call `PowerOn()` (which may return `ResultAccepted` or
  `ResultConflict` if another goroutine started first), then wait for a
  state change.
- **`Starting`** → wait for a state change (another request already triggered
  startup).
- **`ShuttingDown`** → wait for a state change (shutdown completes → `Off`),
  then loop back to trigger `PowerOn`.
- On `ctx.Done()` → return `ctx.Err()`. This represents either a **client
  timeout** (`context.DeadlineExceeded`) or a **client cancellation**
  (`context.Canceled`). The startup process may still be running in the
  background — the state machine is not affected.

The wait uses a state-change channel with `ctx.Done()` select, so the request
is cancellable and responsive (no polling delay).

**Distinguishing error types**: the gateway handler uses `errors.Is` to
classify the returned error. A sentinel error (`ErrBackendError`) is used for
the `Error` state case so that startup failures (which may internally wrap
`context.DeadlineExceeded` via the existing `poll` function) are not
misclassified as client timeouts:

```go
// Sentinel error — defined in internal/state.
var ErrBackendError = errors.New("backend in error state")
```

| `EnsureReady` returns | `errors.Is` check (in order) | Meaning |
|---|---|---|
| `fmt.Errorf("%w: %w", ErrBackendError, lastErr)` | `errors.Is(err, ErrBackendError)` **first** | Startup/shutdown failed; state is `Error` |
| `context.DeadlineExceeded` | `errors.Is(err, context.DeadlineExceeded)` | Client timeout; startup may still succeed |
| `context.Canceled` | `errors.Is(err, context.Canceled)` | Client disconnected; startup may still succeed |

The handler must check `ErrBackendError` **before** `DeadlineExceeded` or
`Canceled`, because a startup failure's `lastError` may wrap
`context.DeadlineExceeded` in its chain (e.g. "gpu detection timeout:
context deadline exceeded"). The sentinel takes precedence.

This distinction is critical: a timeout or cancellation does **not** mean the
startup failed. The state machine continues the transition independently. A
subsequent request may find the state `Ready` and succeed immediately. See
[Error Handling](#error-handling) for the corresponding HTTP responses.

Pseudocode:

```go
func (m *Machine) EnsureReady(ctx context.Context) error {
    for {
        m.stateMu.Lock()
        current := m.state
        lastErr := m.lastError // capture under lock — lastError is guarded by stateMu
        ch := m.changeCh
        m.stateMu.Unlock()

        switch current {
        case Ready:
            return nil
        case Error:
            return fmt.Errorf("%w: %w", ErrBackendError, lastErr)
        case Off:
            result := m.PowerOn()
            // ResultAccepted: we triggered startup, wait for change.
            // ResultConflict: another goroutine is transitioning, wait for change.
            // ResultAlreadyInState: should not happen from Off, but wait anyway.
            _ = result
        case Starting:
            // Another request already triggered startup; wait for state change.
        case ShuttingDown:
            // Wait for shutdown to complete (→ Off), then loop back to PowerOn.
        }

        select {
        case <-ch:
            // State changed; loop to re-evaluate.
        case <-ctx.Done():
            return ctx.Err()
        }
    }
}
```

**Critical**: `EnsureReady` must release `stateMu` before calling `PowerOn()`.
`PowerOn()` internally acquires `transitionMu` (via `TryLock`) and then
`stateMu`. If `EnsureReady` held `stateMu` across the `PowerOn()` call, it
would deadlock with `setState` (which also acquires `stateMu`).

### New: state-change signaling

A `chan struct{}` field (`changeCh`) is added to `Machine`, guarded by
`stateMu`. In `setState`, the current channel is closed and a new one is
created, then the lock is released. `EnsureReady` reads the current channel
under `stateMu`, releases the lock, and `select`s on the channel vs.
`ctx.Done()`.

```go
func (m *Machine) setState(s State, err error) {
    m.stateMu.Lock()
    m.state = s
    m.lastError = err
    close(m.changeCh)
    m.changeCh = make(chan struct{})
    m.stateMu.Unlock()
}
```

The `changeCh` must be initialized in `New()` (or lazily) to avoid closing a
nil channel. The close-and-recreate pattern ensures that every waiter receives
exactly one signal per state change, and new waiters get a fresh channel.

### Valid transitions (unchanged from MVP)

| From | To | Trigger |
|------|----|---------|
| Off | Starting | `PowerOn()` accepted |
| Starting | Ready | Startup completes successfully |
| Starting | Error | Startup fails (Shelly, GPU, Docker, or health) |
| Ready | ShuttingDown | `PowerOff()` accepted |
| Ready | ShuttingDown | `Restart()` accepted (shutdown phase) |
| Error | ShuttingDown | `PowerOff()` accepted |
| ShuttingDown | Off | Shutdown completes successfully |
| ShuttingDown | Error | Shutdown fails (Docker stop, Shelly, or GPU off) |
| Off | Starting | `Restart()` accepted (startup phase) |

The gateway does not alter any transition rules. `EnsureReady` calls
`PowerOn()` and `PowerOff()` — the same methods used by the manual control API.

### Error states and recovery (unchanged)

From the `Error` state, only `POST /power/off` is accepted; `/power/on` and
`/restart` return 409. The gateway's `EnsureReady` returns an error when the
state is `Error`, and the gateway responds with 503. Manual recovery via
`POST /power/off` resets the system to `Off`, after which the next gateway
request triggers a fresh startup.

## Startup Sequence

Step-by-step flow for a request arriving when the backend is down:

1. Client sends request to `/v1/chat/completions` (or any `/v1/*` path except
   `GET /v1/models`, which is handled separately — see
   [Model List Endpoint](#model-list-endpoint)).
2. Gateway handler **immediately** increments `active` and sets
   `lastActivity = time.Now()` under `activeMu`, then defers the decrement.
   This ensures the request counts as active throughout the entire wait +
   proxy phase, preventing idle shutdown.
3. Gateway handler calls `machine.EnsureReady(ctx)` where `ctx` is derived from
   `r.Context()` with a `gateway.requestTimeout` deadline.
4. If state is `Off`: `EnsureReady` calls `PowerOn()`. The existing async
   startup runs (Shelly ON → wait GPU → Docker start → wait health → Ready).
   `EnsureReady` waits on the state-change channel.
5. If state is `Starting` (another request triggered startup first):
   `EnsureReady` waits on the state-change channel — no second startup is
   started (`transitionMu` TryLock ensures one transition at a time).
6. If state is `ShuttingDown` (idle shutdown in progress): `EnsureReady` waits
   for shutdown to complete (→ `Off`), then calls `PowerOn()` to restart.
7. If state is `Ready`: `EnsureReady` returns `nil` immediately.
8. If state is `Error`: `EnsureReady` returns the wrapped `lastError`; the
   gateway responds 503 with `code: "backend_error"`.
9. Once `EnsureReady` returns `nil`, the handler proxies the request. The
   `defer` decrements `active` and updates `lastActivity` when the handler
   returns (including on error/panic).

### Concurrent requests

N requests arrive while state is `Off`. All N increment `active` under
`activeMu` before calling `EnsureReady`. The first request's `EnsureReady`
calls `PowerOn()` → `ResultAccepted`. Requests 2..N's `EnsureReady` calls
`PowerOn()` → `ResultConflict` (transition in progress). All N requests wait on
the same state-change channel. When state becomes `Ready`, all proceed. Exactly
one startup runs — the existing `transitionMu` guarantees this.

### Client timeout vs startup failure

The `gateway.requestTimeout` (default 120s) wraps the `EnsureReady` wait phase
only (the time spent waiting for startup), not the proxy phase:

```go
ctx, cancel := context.WithTimeout(r.Context(), g.requestTimeout)
defer cancel()
err := g.controller.EnsureReady(ctx)
```

If `requestTimeout` expires, `EnsureReady` returns
`context.DeadlineExceeded` — but the startup process **continues running in the
background**. The state machine is unaffected. The gateway responds 503 with
`code: "startup_timeout"` and the client may retry. A subsequent request may
find the state `Ready` and succeed immediately.

If the client disconnects (cancels `r.Context()`), `EnsureReady` returns
`context.Canceled`. The gateway logs this at DEBUG and does not write a
response (the client is gone). The startup process continues unaffected.

This is fundamentally different from a startup failure, where the state machine
enters `Error` and requires manual `POST /power/off` to reset.

## Reverse Proxy

The gateway uses `net/http/httputil.ReverseProxy` from the standard library —
no third-party dependencies.

### Rewrite (Go 1.22+ API)

The project targets Go 1.24.4, so the design uses the modern `Rewrite` field
instead of the legacy `Director` callback. `Rewrite` receives a
`*httputil.ProxyRequest` with both the inbound (`pr.In`) and outbound
(`pr.Out`) requests, making it clearer and less error-prone than `Director`
(which mutates the outbound request in place with no access to the original).

```go
proxy := &httputil.ReverseProxy{
    Rewrite: func(pr *httputil.ProxyRequest) {
        pr.SetURL(backendUrl)
        // pr.SetURL sets scheme, host, and path on pr.Out.URL,
        // and sets pr.Out.Host to the backend's host.
        // The client's OpenAI path (e.g. /v1/chat/completions) is
        // forwarded as-is because backendUrl has no path component.
    },
    FlushInterval: -1,
    ErrorHandler:  errorHandler,
}
```

`pr.SetURL(backendUrl)` sets `pr.Out.URL.Scheme` and `pr.Out.URL.Host` to the
backend's values and clears `pr.Out.Host` (sets it to `""`), causing the HTTP
client to use `pr.Out.URL.Host` as the `Host` header. The client's path and
query are preserved because `backendUrl` (e.g. `http://localhost:1234`) has no
path component — `SetURL` only overrides the path if the target URL has one.

### FlushInterval: `-1`

`FlushInterval: -1` flushes immediately after every write. This is essential
for SSE streaming (`text/event-stream`) — without it, the proxy buffers the
response and streaming tokens arrive in batches. With `-1`, the proxy flushes
each chunk as it arrives, giving real-time token streaming to the client.

### Streaming error handling

A JSON error response can only be sent before any response bytes are forwarded
to the client. Once `WriteHeader` has been called (headers sent), the
`ErrorHandler` cannot send a clean error — `WriteHeader` is a silent no-op and
a JSON body would be appended to the partial streamed response, corrupting it.

The gateway uses a lightweight `ResponseWriter` wrapper to track whether
headers have been written:

```go
type responseTracker struct {
    http.ResponseWriter
    headerWritten bool
}

func (rt *responseTracker) WriteHeader(code int) {
    rt.headerWritten = true
    rt.ResponseWriter.WriteHeader(code)
}

func (rt *responseTracker) Write(b []byte) (int, error) {
    // Write implicitly commits headers if WriteHeader hasn't been called.
    if !rt.headerWritten {
        rt.headerWritten = true
    }
    return rt.ResponseWriter.Write(b)
}

// Unwrap exposes the underlying ResponseWriter so that http.ResponseController
// can reach the concrete type's Flush method. Without this, ReverseProxy's
// ResponseController.Flush() returns errNotSupported and FlushInterval: -1
// silently fails to flush SSE streams — the flush error is discarded.
func (rt *responseTracker) Unwrap() http.ResponseWriter {
    return rt.ResponseWriter
}
```

The handler wraps the `ResponseWriter` before calling `proxy.ServeHTTP`:

```go
rt := &responseTracker{ResponseWriter: w}
proxy.ServeHTTP(rt, r)
```

The `ErrorHandler` checks `headerWritten` and handles the `bufferingWriter`
used by the model-list endpoint (see [Model List Endpoint](#model-list-endpoint)):

```go
errorHandler := func(w http.ResponseWriter, r *http.Request, err error) {
    logger.Error("proxy error", "error", err)
    // Model-list path: buffer-first, so nothing has been sent to the client.
    // Mark the writer as failed so the handler knows not to cache.
    if bw, ok := w.(*bufferingWriter); ok {
        bw.failed = true
        return
    }
    // Inference path: streaming, check if headers were already sent.
    if rt, ok := w.(*responseTracker); ok && rt.headerWritten {
        // Headers already sent (mid-stream crash). Cannot send a clean
        // error response. The client sees a truncated stream and retries.
        return
    }
    w.Header().Set("Content-Type", "application/json")
    w.WriteHeader(http.StatusBadGateway)
    json.NewEncoder(w).Encode(openAIError{
        Error: openAIErrorBody{
            Message: "backend connection failed",
            Type:    "bad_gateway",
            Code:    "proxy_error",
        },
    })
}
```

This covers connection refused, DNS failure, and backend crash before any
response bytes are forwarded (clean 502). Mid-stream backend crashes result in
a truncated response — the `ErrorHandler` logs the error and returns without
writing, so the client sees a broken stream and retries. This is an inherent
limitation of HTTP streaming and is acceptable for a homelab tool.

### Timeouts

The `http.Server` already has no per-request timeout (requests can be
long-running streaming completions). The gateway's `gateway.requestTimeout`
wraps the `EnsureReady` wait phase only (the time spent waiting for startup),
not the proxy phase. The proxy phase inherits the client's request context
(which the client or `http.Server` may cancel).

### Forwarded headers

When using the `Rewrite` API, `ReverseProxy` does **not** automatically add
`X-Forwarded-For` (this default behavior is suppressed when `Rewrite` is set).
The gateway calls `pr.SetXForwarded()` to handle forwarded headers correctly:

```go
Rewrite: func(pr *httputil.ProxyRequest) {
    pr.SetURL(backendUrl)
    pr.SetXForwarded()
},
```

`SetXForwarded` performs three actions:

1. **`X-Forwarded-For`**: appends the client's IP (with port stripped via
   `net.SplitHostPort`) to any existing `X-Forwarded-For` chain. If the client
   already sent `X-Forwarded-For` (e.g. through another proxy), the value is
   appended, not overwritten — no duplication.
2. **`X-Forwarded-Host`**: set to the original request's `Host`.
3. **`X-Forwarded-Proto`**: set to `http` or `https` based on the inbound
   request's TLS state.

`X-Forwarded-Host` and `X-Forwarded-Proto` are harmless for a localhost backend
and are included because `SetXForwarded` is the standard, single-call way to
handle forwarded headers with the `Rewrite` API. Manually managing
`X-Forwarded-For` to exclude the other two would be more error-prone (incorrect
port stripping, duplicate headers) for no practical benefit on a LAN tool.

### Route registration

Two route patterns are registered on the ServeMux:

- `GET /v1/models` → `ModelsHandler()` — lightweight model-list endpoint (see
  [Model List Endpoint](#model-list-endpoint)).
- `/v1/{rest...}` → `Handler()` — catch-all inference handler (all methods, all
  other `/v1/` paths).

Both use Go 1.22+ ServeMux patterns. The codebase already uses Go 1.24.4
method-based patterns (`GET /status`, `POST /power/on`, etc.); these patterns
are compatible and do not conflict with existing routes. More specific patterns
take precedence, so `GET /v1/models` matches before `/v1/{rest...}`, and
`GET /status` still matches `/status` exactly. A `POST /v1/models` (non-standard
OpenAI endpoint) falls through to `/v1/{rest...}` and is treated as an
inference request.

## Idle Shutdown

The gateway runs a background goroutine (`idleWatcher`) only when
`gateway.enabled` is true and `gateway.idleTimeout > 0`. Setting
`idleTimeout: 0` disables auto-shutdown.

### Activity tracking

In the `Gateway` struct, guarded by `activeMu sync.Mutex`:

- `active int` — count of in-flight requests (both waiting in `EnsureReady` and
  being proxied). Incremented **before** `EnsureReady` and decremented via
  `defer` when the handler returns.
- `lastActivity time.Time` — updated on every inference request start and
  request end. **Not** updated by `GET /v1/models` (see
  [Model List Endpoint](#model-list-endpoint)).
- `pendingShutdown bool` — shutdown-reservation flag for the two-phase idle
  shutdown (see [Shutdown Reservation](#shutdown-reservation)).

### Idle timer initialization

`lastActivity` must be initialized when the system becomes `Ready`, not just
when a gateway request arrives. Without this, a manual `POST /power/on` (via
the DockMind UI) would leave `lastActivity` at its zero value, causing the idle
watcher to shut the system down immediately on the next tick.

The idle watcher detects transitions to `Ready` via `StateController.State()`:

1. On each tick, the idle watcher reads `currentState := controller.State()`.
2. It tracks `prevReady bool` across ticks. If `prevReady` is false and
   `currentState == Ready`, the system just became ready — the watcher sets
   `lastActivity = time.Now()` under `activeMu`.
3. `prevReady` is updated to `currentState == Ready` at the end of each tick.

This handles all paths to `Ready`:

- **Gateway-triggered startup**: the first inference request triggers
  `EnsureReady` → `PowerOn` → `Starting` → `Ready`. The idle watcher detects
  the transition and resets `lastActivity`. (The request handler also sets
  `lastActivity` when it increments `active`, so the timer is already fresh,
  but the watcher's reset is harmless and covers the edge case where the
  request's `EnsureReady` timed out before `Ready` was reached.)
- **Manual `POST /power/on`**: the user powers on via the UI. No gateway
  request occurs. The idle watcher detects `Off → Starting → Ready` and resets
  `lastActivity`. The idle timer starts from the moment `Ready` is entered.
- **`Restart` from `Ready`**: the system shuts down and restarts. The idle
  watcher detects the `Ready → ShuttingDown → Off → Starting → Ready`
  transition and resets `lastActivity`.

### Model list endpoint

`GET /v1/models` is a lightweight metadata endpoint that OpenWebUI and other
clients may poll periodically. If it triggered startup, reset the idle timer,
or prevented auto-shutdown, the GPU could remain powered on forever even though
no inference requests are being made. Returning an empty model list while the
backend is sleeping is also undesirable — OpenWebUI would interpret it as
meaning no models are configured, potentially clearing the user's model
selection.

Instead, DockMind caches the last successful model-list response and serves it
from cache while the backend is off.

#### Cache structure

```go
// modelCache holds the last successful GET /v1/models response.
// It is stored as an atomic.Pointer[modelCache] on the Gateway struct.
// The writer builds a new modelCache value and atomically swaps the pointer
// via Store(). Readers call Load() and get an immutable snapshot — they
// never see a partially-written cache. This avoids sharing a mutable byte
// slice between readers and writers.
type modelCache struct {
    body        []byte    // complete response body
    contentType string    // Content-Type header from the backend response
    refreshedAt time.Time // timestamp of the successful refresh
}
```

The `refreshedAt` field is stored for future use (e.g. cache staleness logging
or TTL). The initial implementation does not expire or invalidate the cache
based on age — the cache is only replaced by a new successful `200 OK`
response.

#### Behavior

| State | `GET /v1/models` response | Starts backend? | Resets idle timer? | Counts as activity? |
|-------|---------------------------|-----------------|--------------------|--------------------|
| `Ready` | Proxied to backend; response cached on success | No (already up) | No | No (but increments `active` for race safety) |
| `Off`, `Starting`, `ShuttingDown` | Cached model list (200) with `X-DockMind-Cached: true` header, or 503 if no cache | No | No | No |
| `Error` | 503 `service_unavailable` `code: "backend_error"` (cache not served) | No | No | No |

#### When the backend is Ready — buffer-first approach

Model-list responses are typically small and non-streaming. Instead of teeing
the response body to a buffer while streaming to the client (which requires a
`capturingWriter` with `Unwrap()` and cannot easily detect truncation), the
handler uses a **buffer-first** approach:

1. Proxy the request into a `bufferingWriter` that captures the full response
   (status code, headers, body) without sending anything to the client.
2. After `proxy.ServeHTTP` returns, check whether the response was successful
   (`statusCode == 200`, `!failed`, `body.Len() > 0`).
3. If successful: write the buffered response to the client and cache it.
4. If failed: write a 502 error to the client (the buffered response is
   discarded).

This approach detects truncation for free: if the backend drops the connection
mid-body, `ReverseProxy` calls the `ErrorHandler`, which sets `bw.failed = true`
(see [Streaming Error Handling](#streaming-error-handling)). The handler then
checks `!bw.failed` before caching. A truncated, invalid, or failed backend
response never overwrites the existing cache.

```go
// bufferingWriter captures the full response without sending anything to
// the client. Used by the model-list handler for buffer-first proxying.
type bufferingWriter struct {
    header     http.Header
    statusCode int
    body       bytes.Buffer
    headerWritten bool
    failed     bool // set by ErrorHandler on proxy failure
}

func (bw *bufferingWriter) Header() http.Header {
    if bw.header == nil {
        bw.header = make(http.Header)
    }
    return bw.header
}

func (bw *bufferingWriter) WriteHeader(code int) {
    if !bw.headerWritten {
        bw.headerWritten = true
        bw.statusCode = code
    }
}

func (bw *bufferingWriter) Write(b []byte) (int, error) {
    if !bw.headerWritten {
        bw.headerWritten = true
        bw.statusCode = http.StatusOK // implicit 200
    }
    return bw.body.Write(b)
}
```

The `bufferingWriter` does **not** implement `Unwrap()` — it is not a streaming
wrapper. `ReverseProxy` with `FlushInterval: -1` will attempt to flush via
`ResponseController`, get `errNotSupported`, and silently discard the error.
This is harmless: the response is small and non-streaming, so all bytes are
captured in the buffer regardless of flush behavior.

The handler logic when the backend is `Ready`:

```go
// Backend is Ready: proxy to backend, buffer the response for caching.
g.activeMu.Lock()
g.active++
g.pendingShutdown = false
g.activeMu.Unlock()
defer func() {
    g.activeMu.Lock()
    g.active--
    g.activeMu.Unlock()
}()

bw := &bufferingWriter{}
g.proxy.ServeHTTP(bw, r)

// Cache only on a complete, successful 200 response.
// bw.failed is set by the ErrorHandler if the backend connection fails
// or drops mid-response (truncation).
if !bw.failed && bw.statusCode == http.StatusOK && bw.body.Len() > 0 {
    contentType := bw.Header().Get("Content-Type")
    if contentType == "" {
        contentType = "application/json"
    }
    g.cachedModels.Store(&modelCache{
        body:        bw.body.Bytes(),
        contentType: contentType,
        refreshedAt: time.Now(),
    })
    // Write the successful response to the client.
    for k, v := range bw.Header() {
        w.Header()[k] = v
    }
    w.WriteHeader(http.StatusOK)
    w.Write(bw.body.Bytes())
    return
}

// Failed or non-200: do not cache. Write a 502 error to the client.
w.Header().Set("Content-Type", "application/json")
w.WriteHeader(http.StatusBadGateway)
json.NewEncoder(w).Encode(openAIError{
    Error: openAIErrorBody{
        Message: "backend connection failed",
        Type:    "bad_gateway",
        Code:    "proxy_error",
    },
})
```

#### When the backend is Off, Starting, or ShuttingDown

The handler returns the cached model list without calling `EnsureReady` and
without starting the backend. The `Error` state is checked first to return
`backend_error` before the non-Ready cache-serving branch. A
`X-DockMind-Cached: true` response header makes the source visible during
debugging:

```go
currentState := g.controller.State()

// Error state: return backend_error, do not serve cache.
if currentState == state.Error {
    w.Header().Set("Content-Type", "application/json")
    w.WriteHeader(http.StatusServiceUnavailable)
    json.NewEncoder(w).Encode(openAIError{
        Error: openAIErrorBody{
            Message: "backend is in error state",
            Type:    "service_unavailable",
            Code:    "backend_error",
        },
    })
    return
}

// Off, Starting, or ShuttingDown: serve cached model list.
if currentState != state.Ready {
    cached := g.cachedModels.Load()
    if cached != nil {
        w.Header().Set("Content-Type", cached.contentType)
        w.Header().Set("X-DockMind-Cached", "true")
        w.WriteHeader(http.StatusOK)
        w.Write(cached.body)
        return
    }
    // No cached response exists yet.
    w.Header().Set("Content-Type", "application/json")
    w.WriteHeader(http.StatusServiceUnavailable)
    json.NewEncoder(w).Encode(openAIError{
        Error: openAIErrorBody{
            Message: "model list is not available while the backend is sleeping",
            Type:    "service_unavailable",
            Code:    "model_cache_unavailable",
        },
    })
    return
}

// Ready: buffer-first proxy (see above).
```

#### When no cached response exists

If DockMind has not yet received a successful model-list response (e.g. the
backend has never been started, or was started but the model-list request
failed), the handler returns a clear error instead of an empty list:

```json
{
  "error": {
    "message": "model list is not available while the backend is sleeping",
    "type": "service_unavailable",
    "code": "model_cache_unavailable"
  }
}
```

HTTP status: `503 Service Unavailable`. The client may retry; once an
inference request triggers startup and the backend becomes `Ready`, the next
`GET /v1/models` request will populate the cache.

#### Error state

If the state machine is in `Error`, the handler returns the existing
`backend_error` response (503) and does **not** serve the cached model list.
The error must remain visible to the user so they know the system requires
manual intervention via `POST /power/off`.

#### Cache scope and lifecycle

- **In-memory only**: the cache lives in the `Gateway` struct as an
  `atomic.Pointer[modelCache]`. Persistence across DockMind restarts is not
  required for the initial implementation. Disk persistence may be considered
  later if model visibility immediately after a DockMind restart becomes
  important.
- **Manual shutdown or restart does not clear the cache**: the cache is only
  replaced by a new successful `200 OK` response from the backend. Powering
  the system off and back on does not clear the cached model list — the user
  sees the same models before and after the sleep cycle.
- **Cache is per-backend**: if `llamaSwap.backendUrl` changes between
  restarts, the cache starts empty (new `Gateway` instance). This is
  acceptable since the cache is in-memory only.
- **No size bound**: the cache holds the full backend response body in memory
  indefinitely (until replaced). A misbehaving backend returning a very large
  `/v1/models` body would be cached forever. This is acceptable for a trusted
  homelab backend.

### Shutdown reservation

The original two-layer race handling (check `active == 0`, then call
`PowerOff()`) has a window between the check and `PowerOff()` where a new
request can arrive, leading to a full shutdown+restart cycle. To reduce this
window, the idle watcher uses a **two-phase shutdown with a grace period**:

**Phase 1 — Reserve**: On the tick where `active == 0` and
`time.Since(lastActivity) >= idleTimeout` and `state == Ready`, the watcher
sets `pendingShutdown = true` under `activeMu` and logs at DEBUG. It does
**not** call `PowerOff()` yet.

**Phase 2 — Confirm**: On the **next** tick, the watcher re-checks under
`activeMu`: if `active == 0` and `pendingShutdown` is still true, it clears
`pendingShutdown` and calls `PowerOff()`. If `active > 0` (a request arrived
during the grace period), it clears `pendingShutdown` and skips.

**Request handler cancels reservation**: when an inference request increments
`active` under `activeMu`, it also clears `pendingShutdown = false`. This
ensures that any request arriving during the grace period cancels the pending
shutdown.

```go
// Request handler (inference):
g.activeMu.Lock()
g.active++
g.lastActivity = time.Now()
g.pendingShutdown = false // cancel any pending shutdown
g.activeMu.Unlock()
defer func() {
    g.activeMu.Lock()
    g.active--
    g.lastActivity = time.Now()
    g.activeMu.Unlock()
}()
```

The grace period adds at most one `pollInterval` (default 1s) of delay before
shutdown — negligible compared to the 30-minute idle timeout. It eliminates the
race in the common case: a request arriving within 1 second of the idle timeout
cancels the reservation instead of triggering a shutdown+restart cycle.

### Idle watcher loop (revised)

1. Tick at `gpu.pollInterval` (reused as in the existing codebase convention).
2. Read `currentState := controller.State()`.
3. Acquire `activeMu`.
4. If `currentState == Ready` and `!prevReady`: set `lastActivity = time.Now()`
   (idle timer initialization — see [Idle Timer Initialization](#idle-timer-initialization)).
5. If `currentState != Ready`: clear `pendingShutdown`, release `activeMu`,
   set `prevReady = false`, continue to next tick. (No shutdown from non-Ready
   states — the system is already off, starting, shutting down, or in error.)
6. If `active > 0`: clear `pendingShutdown`, release `activeMu`,
   set `prevReady = true`, continue. (Requests in-flight.)
7. If `active == 0` and `time.Since(lastActivity) < idleTimeout`: clear
   `pendingShutdown`, release `activeMu`, set `prevReady = true`, continue.
   (Not idle yet.)
8. If `active == 0` and idle timeout elapsed:
   - If `!pendingShutdown`: set `pendingShutdown = true`, release `activeMu`,
     set `prevReady = true`, log at DEBUG ("shutdown reserved, grace period"),
     continue. (Phase 1 — reserve.)
   - If `pendingShutdown`: clear `pendingShutdown`, release `activeMu`,
     set `prevReady = true`, call `machine.PowerOff()`. (Phase 2 — confirm.)
     Check the result:
     - `ResultAccepted` — shutdown initiated; log at INFO.
     - `ResultAlreadyInState` — already Off; no action.
     - `ResultConflict` — a transition is in progress (e.g. a request just
       triggered startup); skip this tick.

### Remaining race (after grace period)

The grace period eliminates the race in the common case. A residual window
remains: a request arrives in the microseconds between Phase 2's `active == 0`
check and `PowerOff()` succeeding. In this case:

- `PowerOff()` succeeds → state becomes `ShuttingDown`.
- The request's `EnsureReady` sees `ShuttingDown`, waits for it to complete
  (→ `Off`), then triggers `PowerOn()` (→ `Starting` → `Ready`).
- The request is proxied after a full shutdown+restart cycle.

This is correct behavior — the client experiences latency (bounded by
`shutdown.timeout + startup.timeout`) but no data loss or errors. The
probability is vanishingly small (the request must arrive in the microsecond
window after the second tick's confirmation check), and the cost is a one-time
restart cycle. This is simpler than cancelling an in-progress shutdown (which
would require modifying the state machine's transition logic to interrupt a
running goroutine and roll back partial state changes).

## Synchronization

Each primitive and its justification:

- **`stateMu` (sync.Mutex, existing)**: guards `state`, `lastError`, and the
  new `changeCh`. Held briefly in `setState` and `EnsureReady`'s state checks.
  Never held across a `PowerOn()` call (deadlock prevention — `PowerOn`
  acquires `transitionMu` then `stateMu` internally).

- **`changeCh` (chan struct{}, new)**: state-change broadcast. Closed and
  recreated in `setState`. `EnsureReady` selects on it vs. `ctx.Done()`. Chosen
  over `sync.Cond` because `Cond` does not support `context.Context`
  cancellation. Chosen over polling because it is responsive (no poll-interval
  delay) and context-aware.

- **`transitionMu` (sync.Mutex with TryLock, existing)**: ensures only one
  state transition runs at a time. Ownership is passed to the async goroutine,
  which `defer`s the unlock. Unchanged.

- **`activeMu` (sync.Mutex, new in gateway)**: guards `active`,
  `lastActivity`, and `pendingShutdown`. Held briefly in the request handler
  (increment `active`, set `lastActivity`, clear `pendingShutdown`) and in the
  idle watcher (check state, update timer, manage reservation). Never held
  during proxying, `EnsureReady`, or `PowerOff()` — the handler releases it
  before calling `EnsureReady` and re-acquires it in the `defer`.

- **`atomic.Pointer[modelCache]` (new in gateway)**: stores the cached
  model-list response. Readers (model-list requests when backend is not Ready)
  call `Load()` and get an immutable `*modelCache` snapshot — lock-free, no
  blocking. The writer (model-list request when backend is Ready, after a
  successful `200 OK` response) calls `Store()` to atomically swap the
  pointer. Chosen over `sync.RWMutex` because `atomic.Pointer` (Go 1.19+) is
  the idiomatic stdlib solution for single-writer/multi-reader pointer swaps:
  no lock/unlock ceremony, truly lock-free reads, no risk of reader
  starvation. The `body []byte` in each `modelCache` is never mutated after
  construction, so concurrent `w.Write(cached.body)` calls are safe.

- **`sync.WaitGroup` (existing)**: backs `Machine.Wait()`. The gateway's idle
  watcher uses its own `context.Context` for lifecycle, not the WaitGroup.

- **`context.Context`**: propagated from the HTTP request through `EnsureReady`
  and the proxy. The idle watcher uses a separate context (cancelled on
  SIGINT/SIGTERM). Consistent with the existing codebase convention (see
  `docs/learnings.md`: "context propagation, timeouts, and error logging by
  default").

- **Goroutines**: one per in-flight request (managed by `net/http.Server`),
  one idle watcher (long-lived, gateway lifecycle). No goroutine pool needed.

## Error Handling

Behavior for each failure scenario:

| Scenario | State after | Client sees | Retry? |
|---|---|---|---|
| Shelly unreachable | `Error` | 503 `service_unavailable` `code: "backend_error"` | No (manual `POST /power/off` to reset) |
| GPU never appears (timeout) | `Error` | 503 `service_unavailable` `code: "backend_error"` | No |
| llama-swap fails to start | `Error` | 503 `service_unavailable` `code: "backend_error"` | No |
| Backend health check fails (timeout) | `Error` | 503 `service_unavailable` `code: "backend_error"` | No |
| Client timeout waiting for startup | (unchanged — startup continues) | 503 `service_unavailable` `code: "startup_timeout"` | Yes — client may retry; startup may succeed |
| Client disconnects while waiting | (unchanged — startup continues) | No response (client is gone) | N/A |
| Proxy connection fails | (unchanged) | 502 `bad_gateway` `code: "proxy_error"` | No |
| Backend crashes before headers sent | (unchanged) | 502 `bad_gateway` `code: "proxy_error"` | No |
| Backend crashes mid-stream | (unchanged) | Truncated stream (no JSON error) | Client detects broken stream and retries |

### OpenAI error response format

All gateway errors use the OpenAI error JSON format:

```json
{
  "error": {
    "message": "backend is starting up, please retry",
    "type": "service_unavailable",
    "code": "startup_timeout"
  }
}
```

Error types and codes:

| HTTP status | `type` | `code` | When |
|---|---|---|---|
| 503 | `service_unavailable` | `backend_error` | State machine is in `Error` (startup or shutdown failed). Requires manual `POST /power/off`. |
| 503 | `service_unavailable` | `startup_timeout` | `EnsureReady` timed out (`context.DeadlineExceeded`). Startup may still succeed in the background. Client may retry. |
| 503 | `service_unavailable` | `model_cache_unavailable` | `GET /v1/models` requested while backend is sleeping and no cached model list exists yet. Client may retry after an inference request triggers startup. |
| 502 | `bad_gateway` | `proxy_error` | Proxy connection failed (upstream unreachable or crashed before headers sent). |

**Client cancellation** (`context.Canceled`): the gateway logs at DEBUG and
does not write a response — the client has disconnected and the connection is
already closed. The startup process continues unaffected in the background.

### No automatic retry

Consistent with the existing manual-control design where `Error` requires
`POST /power/off` to reset. The client (e.g. Hermes, OpenWebUI) is expected to
retry with backoff — this is standard OpenAI client behavior. A `startup_timeout`
is explicitly retryable: the client should retry after a short delay, and the
next request may find the state `Ready` if startup completed in the meantime.

### Manual recovery

`POST /power/off` always works (even from `Error`), so the user can reset the
system and the next gateway request triggers a fresh startup. Manual control
endpoints (`/power/on`, `/power/off`, `/restart`) remain available when the
gateway is enabled.

## Configuration

### New and modified config fields

| Field | Type | Default | Required | Description |
|---|---|---|---|---|
| `gateway.enabled` | bool | `false` | no | Opt-in flag. When false, no gateway route is registered. |
| `gateway.idleTimeout` | Duration | `30m` | no | Idle period before auto-shutdown. `0` disables. |
| `gateway.requestTimeout` | Duration | `120s` | no | Timeout for the `EnsureReady` wait phase (startup wait). |
| `llamaSwap.backendUrl` | string | (none) | yes (when `gateway.enabled`) | Proxy target URL, e.g. `http://localhost:1234`. |

### Config structs

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

### Validation rules (added to `config.validate`)

- When `gateway.enabled` is true, `llamaSwap.backendUrl` must be non-empty and
  a valid URL (parseable by `net/url.Parse`).
- `gateway.idleTimeout` must be `>= 0` (`0` is valid = disabled).
- `gateway.requestTimeout` must be `> 0`.

The existing `Duration` type with `UnmarshalYAML` handles YAML parsing for the
new duration fields — no new parsing code needed.

### Example config with gateway enabled

```yaml
gateway:
  enabled: true
  idleTimeout: 30m
  requestTimeout: 120s
llamaSwap:
  healthUrl: http://localhost:1234/v1/models
  backendUrl: http://localhost:1234
```

## Logging

Use the existing `slog.Logger` (text handler to stderr, as in `main.go`).

| Level | When |
|---|---|
| INFO | Gateway enabled at startup; startup triggered by gateway request; shutdown triggered by idle watcher; state transitions (existing). |
| WARN | Idle timeout approaching (e.g. at 80% of idleTimeout); `EnsureReady` timed out (`startup_timeout` — startup may still succeed); probe failures (existing). |
| ERROR | Startup/shutdown failures (existing); proxy connection errors; `EnsureReady` returned `backend_error` (state machine in Error). |
| DEBUG | Active request count changes; idle timer resets; `EnsureReady` state checks; shutdown reservation set/cleared; client cancellation (`context.Canceled` — client disconnected). |

Avoid excessive noise: active-count changes are DEBUG (not INFO). Idle
approaching is WARN only once per idle cycle (not on every tick). State
transitions remain INFO (existing behavior). Client cancellations are DEBUG
(not WARN or ERROR) — the client simply gave up, which is normal behavior.

## Testing Strategy

All tests use stdlib `testing` + `net/http/httptest` (consistent with existing
conventions in `AGENTS.md`): no testify, no mocking libraries, hand-written
fakes, table-driven tests.

### Proxy unit tests (`internal/gateway/gateway_test.go`)

- Request forwarded to backend with correct path/method/headers.
- Response body forwarded correctly (non-streaming).
- SSE streaming: response flushed incrementally (verify chunks arrive
  separately, not buffered).
- 503 with `code: "backend_error"` when `EnsureReady` returns startup failure
  (Error state).
- 503 with `code: "startup_timeout"` when `EnsureReady` returns
  `context.DeadlineExceeded`.
- No response written when `EnsureReady` returns `context.Canceled` (client
  disconnected).
- 502 when backend connection fails (backend server closed mid-test).
- 502 before headers sent; truncated stream after headers sent (verify
  `responseTracker` prevents JSON corruption of mid-stream responses).
- Active counter incremented before `EnsureReady` and decremented after
  proxy (including when `EnsureReady` fails).

### Model list endpoint tests (`internal/gateway/gateway_test.go`)

- Successful backend `200 OK` response populates the cache (verify body,
  Content-Type, and timestamp are stored).
- Backend-off request returns the cached response with
  `X-DockMind-Cached: true` header.
- Cached response does not reset the idle timer (verify `lastActivity` is
  unchanged).
- Failed backend response (non-200) does not replace a valid cache.
- Truncated backend response (connection closed mid-body) does not replace a
  valid cache.
- Concurrent reads and cache refreshes are race-free (run with `-race`).
- Missing cache (no prior successful response) returns 503
  `model_cache_unavailable`.
- `Error` state returns `backend_error` (503) even when a cache exists.
- `GET /v1/models` when state is Off, Starting, or ShuttingDown with a valid
  cache → 200 with cached body, no `EnsureReady` call, no `active` increment.
- Periodic `GET /v1/models` polling does not prevent idle shutdown.

### EnsureReady unit tests (`internal/state/state_test.go`, extend existing)

- Off → PowerOn triggered → waits → Ready → returns nil.
- Starting → waits → Ready → returns nil.
- Ready → returns nil immediately.
- Error → returns wrapped `lastError` immediately (distinguishable from
  `context.DeadlineExceeded` via `errors.Is`).
- ShuttingDown → waits → Off → PowerOn → Ready → returns nil.
- Context deadline exceeded → returns `context.DeadlineExceeded` (startup
  continues in background; verify state is still `Starting`, not `Error`).
- Context cancelled → returns `context.Canceled` (startup continues).
- Concurrent requests → single startup (one `PowerOn` accepted, rest
  `ResultConflict`, all wait on same channel).

### Idle shutdown unit tests (`internal/gateway/gateway_test.go`)

- Idle timeout elapses with no active requests → `PowerOff` called (after
  two-tick grace period).
- Active request prevents shutdown (and clears `pendingShutdown`).
- Request arrives during grace period → `pendingShutdown` cleared, no
  `PowerOff` called.
- Manual power-on (state → Ready with no gateway request) → idle timer
  initialized, no immediate shutdown.
- `GET /v1/models` polling does not reset idle timer or prevent shutdown.
- Request arrives during shutdown (after grace period) → full restart cycle
  (correctness, not latency).
- `idleTimeout: 0` → idle watcher does not run.

### Integration test (httptest backend + fake state machine)

- Full flow: request → auto-start → proxy → response → idle → auto-shutdown.
- Full flow with model polling: `GET /v1/models` polling does not keep system
  alive; inference request triggers startup; idle after last inference request
  → shutdown.

### Most critical scenarios

1. **Concurrent startup** — single transition guarantee (multiple requests,
   one `PowerOn`).
2. **Idle race with grace period** — request arriving during grace period
   cancels shutdown, no restart cycle.
3. **SSE streaming** — no buffering; tokens arrive incrementally; mid-stream
   crash produces truncated response (not corrupted JSON).
4. **Startup timeout vs failure** — timeout returns retryable 503, failure
   returns non-retryable 503; state machine continues independently of client
   timeout.
5. **Model polling** — `GET /v1/models` does not start backend or prevent
   idle shutdown; cached response is served when backend is sleeping.

## Incremental Implementation Plan

Four milestones, each leaving DockMind in a working state:

### Milestone 1 — Reverse Proxy

- **Objective**: proxy `/v1/*` to backend when Ready; 503 when not Ready.
  Includes `GET /v1/models` special handling with model-list caching: proxy
  and cache on success when Ready, serve from cache when backend is sleeping,
  503 `model_cache_unavailable` when no cache exists, 503 `backend_error` in
  Error state.
- **Dependencies**: `gateway.enabled` + `llamaSwap.backendUrl` config fields.
- **Risks**: route conflicts with existing ServeMux patterns (low — `/v1/` is a
  new prefix); `Rewrite` API vs `Director` (use `Rewrite` — Go 1.22+ API,
  project targets 1.24.4); `bufferingWriter` correctness (must buffer full
  response before sending to client, must detect truncation via `failed` flag
  set by ErrorHandler).
- **Expected outcome**: manual control unchanged; proxy works when backend is
  already up. No auto-start, no idle shutdown. `GET /v1/models` returns cached
  model list when backend is down (or 503 if no cache exists yet).

### Milestone 2 — Auto-Startup on First Request

- **Objective**: `EnsureReady` + state-change signaling; request triggers
  startup if backend is down. Active-request tracking (increment before
  `EnsureReady`). `responseTracker` for streaming error handling. Distinct
  error codes for startup failure vs client timeout vs cancellation.
- **Dependencies**: Milestone 1; `internal/state` changes (`EnsureReady`,
  `changeCh`, `State()`).
- **Risks**: deadlock in `EnsureReady` (stateMu held across PowerOn); channel
  lifecycle (closing a nil or already-closed channel); `responseTracker`
  correctness (must track implicit `Write` header commit).
- **Expected outcome**: first request auto-starts the backend; concurrent
  requests wait for a single startup. No idle shutdown yet. Client timeout
  returns retryable 503; startup failure returns non-retryable 503.

### Milestone 3 — Idle Auto-Shutdown

- **Objective**: idle watcher goroutine; active-request tracking with
  `pendingShutdown` reservation; idle timer initialization on `Ready` entry;
  auto-shutdown after idle timeout with two-phase grace period.
- **Dependencies**: Milestone 2; `gateway.idleTimeout` config field;
  `StateController.State()` method.
- **Risks**: race between request and shutdown (mitigated by grace period);
  idle timer initialization (must detect manual power-on); idle watcher
  goroutine leak on shutdown.
- **Expected outcome**: full gateway — auto-start on request, auto-shutdown on
  idle. Manual power-on does not cause immediate shutdown. `GET /v1/models`
  polling does not keep system alive.

### Milestone 4 — Polish

- **Objective**: OpenAI error format; config validation; OpenAPI spec update;
  README + product.md updates; logging refinement.
- **Dependencies**: Milestones 1–3.
- **Risks**: README test enforcement (existing `readme_test.go` constraints).
- **Expected outcome**: production-ready gateway with documentation.

## Final Review

### Unnecessary complexity

None identified. The design reuses the existing state machine, adds one method
(`EnsureReady`) + one channel (`changeCh`) + one query method (`State()`), and
uses stdlib `httputil.ReverseProxy`. No new states, no goroutine pools, no
third-party dependencies. The `responseTracker` wrapper is ~18 lines of
straightforward code (including the `Unwrap` method required for
`ResponseController` flush support). The `pendingShutdown` flag is a single
bool.

### Possible simplifications

The state-change channel could be replaced with polling (simpler code, but
adds up to `pollInterval` latency per request). The channel approach is
preferred for responsiveness; polling is documented as a fallback.

The two-phase shutdown reservation adds one `pollInterval` of delay and a
`pendingShutdown` bool. A simpler approach (check `active == 0` and immediately
call `PowerOff()`) was rejected because it leaves a wider race window. The
grace period is the minimal coordination mechanism that significantly reduces
the race without requiring shutdown cancellation.

The `GET /v1/models` special handling adds a separate handler, route pattern,
and model-list cache. A simpler approach (treat all `/v1/*` equally) was
rejected because model polling would keep the GPU powered on indefinitely. The
cache uses `atomic.Pointer[modelCache]` for lock-free reads and atomic pointer
swaps — readers get an immutable snapshot, the writer builds a new value before
storing. The `bufferingWriter` captures the full response before sending
anything to the client; the cache is only updated on a complete, non-failed
`200 OK` response. Truncation is detected via the `failed` flag set by the
`ErrorHandler`.

### Architectural risks

The residual shutdown+restart race (after the grace period) adds latency in a
vanishingly rare case. This is acceptable for a homelab tool and simpler than
adding shutdown cancellation to the state machine (which would require
interrupting a running transition goroutine and rolling back partial state
changes).

The `StateController.State()` addition creates a minor coupling between the
gateway and the state machine's state enum. This is acceptable — the gateway
already depends on `state.PowerResult`, and `State()` is a read-only query
that does not affect transition logic.

### Alternative approaches considered

- **Adding `Busy`/`Idle` states**: rejected — adds transition complexity and
  breaks existing tests for no functional benefit. The gateway tracks activity
  externally, which is simpler and keeps the state machine focused on hardware
  lifecycle.
- **`sync.Cond` instead of channel**: rejected — `Cond` does not support
  `context.Context` cancellation, which is required for request-scoped
  timeouts.
- **Third-party reverse proxy (e.g. `chi`, `gin`)**: rejected — violates the
  "standard library whenever possible" constraint. `httputil.ReverseProxy`
  handles all requirements including SSE streaming.
- **Request queuing during startup**: rejected — listed as a non-goal in the
  MVP spec; concurrent requests wait on the channel instead, which is simpler
  and avoids queue management.
- **`Director` instead of `Rewrite`**: rejected — `Rewrite` is the modern API
  for Go 1.22+ and provides access to both inbound and outbound requests,
  making URL rewriting clearer and less error-prone.
- **Static empty model list instead of caching**: rejected — OpenWebUI
  interprets an empty model list as "no models configured," potentially
  clearing the user's model selection. Caching the last successful response
  preserves model visibility while the backend sleeps.
- **Disk-persisted model cache**: deferred — in-memory caching is sufficient
  for the initial implementation. Disk persistence may be added later if model
  visibility immediately after a DockMind restart becomes important.
- **Shutdown cancellation**: rejected — the user explicitly excluded this. The
  two-phase grace period reduces the race window without requiring
  cancellation.

### Conclusion

This is the simplest architecture that fully satisfies the proposal's
requirements. It is opt-in, preserves all existing behavior, reuses the
existing state machine without modification to its transition logic, and uses
only standard library primitives. The incremental implementation plan
minimizes risk by delivering a working proxy first, then layering on
auto-start and auto-shutdown in separate milestones. The design addresses all
seven review feedback items: consistent active-request tracking, idle timer
initialization, `/v1/models` special handling with model-list caching, distinct
error codes for timeout vs failure, grace-period shutdown reservation, modern
`Rewrite` API, and `responseTracker`-based streaming error handling.
