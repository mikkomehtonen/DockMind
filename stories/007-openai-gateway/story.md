# DockMind as an Intelligent OpenAI Gateway — Design Document

## Context

`docs/DockMind_as_Gateway_Proposal.md` (commit `f4b9286`) requests a detailed
design for evolving DockMind from a manual control daemon into an intelligent
OpenAI-compatible gateway. The proposal explicitly states: "produce a detailed
design… before any production code is written." This story delivers that design
as a single Markdown document under `docs/`, with no production code changes.

The design covers all 12 areas the proposal lists (high-level architecture,
package structure, state machine, startup sequence, reverse proxy, idle
shutdown, synchronization, error handling, configuration, logging, testing
strategy, incremental implementation plan) plus a final critical review. It is
detailed enough that each milestone in the incremental plan can be implemented
as a separate follow-up story without further design decisions.

Key design decisions, grounded in user requirements and the existing codebase:

- **Opt-in**: the gateway is disabled by default and enabled via
  `gateway.enabled: true` in `config.yaml`. Existing manual-control deployments
  are unaffected until the flag is toggled.
- **Separate backend URL**: a new `llamaSwap.backendUrl` config field (e.g.
  `http://localhost:1234`) specifies the proxy target, distinct from the
  existing `llamaSwap.healthUrl`.
- **Existing 5 states preserved**: no new states (Busy/Idle) are added. The
  gateway adds an active-request counter and idle timer on top of the existing
  Off/Starting/Ready/ShuttingDown/Error machine, plus a new `EnsureReady`
  method.
- **Manual control coexists**: `POST /power/on`, `/power/off`, `/restart`
  continue to work when the gateway is enabled, so the user can shut the system
  down immediately when they know they are done.
- **All `/v1/*` paths proxied**: a single catch-all route forwards every
  OpenAI-compatible request to the backend.
- **30-minute idle timeout**: configurable via `gateway.idleTimeout`, defaulting
  to `30m`; setting `0` disables auto-shutdown.

## Out of Scope

- **Any production code.** No changes to `internal/state`, `internal/api`,
  `internal/config`, `cmd/dockmind/main.go`, or any client package. The
  deliverable is a design document only.
- **Implementation of the gateway.** Each milestone in the design's incremental
  plan is a separate follow-up story. This story does not implement any
  milestone.
- **Changes to the existing state machine, API contract, OpenAPI spec, or
  config schema.** The design *proposes* changes for future stories but does
  not apply them.
- **A new design for the existing MVP features.** The design focuses exclusively
  on the gateway; the existing manual-control architecture is treated as a
  stable baseline.
- **Web UI changes for the gateway.** The design may mention UI implications but
  does not specify UI changes (separate future story).

## Implementation approach

The deliverable is a single file: `docs/DockMind_Gateway_Design.md`. The
document is structured with one `##`-level section per proposal area, plus a
final review section. The content below is the blueprint the implementer follows
to write the document — every section's key decisions are specified here so no
guesswork is needed.

### Document structure and content per section

The document uses these exact `##` headings (the automated test checks for
them):

1. `## High-Level Architecture`
2. `## Go Package Structure`
3. `## State Machine`
4. `## Startup Sequence`
5. `## Reverse Proxy`
6. `## Idle Shutdown`
7. `## Synchronization`
8. `## Error Handling`
9. `## Configuration`
10. `## Logging`
11. `## Testing Strategy`
12. `## Incremental Implementation Plan`
13. `## Final Review`

#### 1. High-Level Architecture

Include an ASCII architecture diagram (fenced code block, same style as the
diagram in `docs/DockMind_MVP_Specification.md`). The diagram shows:

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

Describe the major components and their responsibilities:

- **Gateway** (new `internal/gateway` package): receives `/v1/*` requests,
  ensures the backend is Ready before proxying, tracks active requests, and runs
  an idle watcher goroutine.
- **State Machine** (existing `internal/state`): unchanged transitions; gains an
  `EnsureReady(ctx)` method and state-change signaling.
- **Existing clients** (shelly, gpu, docker, health): unchanged.

Interaction flow: client request → Gateway → `EnsureReady` (may trigger
startup) → proxy to llama-swap → response back to client. Idle watcher monitors
activity and calls `PowerOff` when idle.

#### 2. Go Package Structure

Recommend a new `internal/gateway` package. Describe each package's
responsibility:

- `internal/gateway` (new): `Gateway` struct holding `*httputil.ReverseProxy`,
  a `StateController` interface, logger, idle timeout, and active-request
  tracking. Exposes `Handler()` returning the `/v1/{rest...}` `http.Handler`,
  and `StartIdleWatcher(ctx)` / `StopIdleWatcher()`.
- `internal/state` (modified): adds `EnsureReady(ctx context.Context) error` and
  a state-change channel. No new states.
- `internal/config` (modified): adds `GatewayConfig` struct with `Enabled`,
  `IdleTimeout`, `RequestTimeout` fields; adds `BackendURL` to
  `LlamaSwapConfig`.
- `internal/api` (modified): conditionally registers the gateway route when
  `gateway.enabled` is true.
- `cmd/dockmind/main.go` (modified): wires the gateway when enabled, starts the
  idle watcher, and shuts it down on SIGINT/SIGTERM.

The `StateController` interface in `internal/gateway` (for testability, matching
the pattern of `internal/api`'s `StateMachine` interface):

```go
type StateController interface {
    EnsureReady(ctx context.Context) error
    PowerOff() state.PowerResult
}
```

#### 3. State Machine

Keep the existing 5 states: `Off`, `Starting`, `Ready`, `ShuttingDown`, `Error`.
Do **not** add `Busy` or `Idle` states — the gateway tracks activity externally.

Add two things to `internal/state.Machine`:

1. **`EnsureReady(ctx context.Context) error`** — the core method the gateway
   calls before proxying. Logic:
   - `Ready` → return `nil` immediately.
   - `Error` → return the last error (wrapped).
   - `Off` → call `PowerOn()` (which may return `ResultAccepted` or
     `ResultConflict` if another goroutine started first), then wait for state
     change.
   - `Starting` → wait for state change.
   - `ShuttingDown` → wait for state change (shutdown completes → `Off`), then
     loop back to trigger `PowerOn`.
   - The wait uses a state-change channel with `ctx.Done()` select, so the
     request is cancellable and responsive (no polling delay).
   - On `ctx.Done()` return `ctx.Err()`.

2. **State-change signaling** — a `chan struct{}` field (`changeCh`) guarded by
   `stateMu`. In `setState`, close the current channel and create a new one,
   then broadcast. `EnsureReady` reads the current channel under `stateMu`,
   releases the lock, and `select`s on the channel vs. `ctx.Done()`.

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

   `EnsureReady` must release `stateMu` before calling `PowerOn()` (which
   acquires `transitionMu` and `stateMu` internally) to avoid deadlock.

Discuss valid transitions (unchanged from MVP), error states (unchanged), and
recovery paths (unchanged: `POST /power/off` from Error → Off). The gateway does
not alter any transition rules.

#### 4. Startup Sequence

Step-by-step flow for a request arriving when the backend is down:

1. Client sends request to `/v1/chat/completions` (or any `/v1/*` path).
2. Gateway handler calls `machine.EnsureReady(r.Context())`.
3. If state is `Off`: `EnsureReady` calls `PowerOn()`. The existing async
   startup runs (Shelly ON → wait GPU → Docker start → wait health → Ready).
   `EnsureReady` waits on the state-change channel.
4. If state is `Starting` (another request triggered startup first):
   `EnsureReady` waits on the state-change channel — no second startup is
   started (`transitionMu` TryLock ensures one transition at a time).
5. If state is `ShuttingDown` (idle shutdown in progress): `EnsureReady` waits
   for shutdown to complete (→ `Off`), then calls `PowerOn()` to restart.
6. If state is `Ready`: `EnsureReady` returns `nil` immediately.
7. If state is `Error`: `EnsureReady` returns the error; the gateway responds
   503.
8. Once `EnsureReady` returns `nil`, the gateway increments the active-request
   counter, proxies the request, and decrements the counter on completion
   (including on error/panic via `defer`).

**Concurrent requests**: N requests arrive while state is `Off`. The first
request's `EnsureReady` calls `PowerOn()` → `ResultAccepted`. Requests 2..N's
`EnsureReady` calls `PowerOn()` → `ResultConflict` (transition in progress).
All N requests wait on the same state-change channel. When state becomes
`Ready`, all proceed. Exactly one startup runs — the existing `transitionMu`
guarantees this.

#### 5. Reverse Proxy

Use `net/http/httputil.ReverseProxy` from the standard library (no third-party
dependencies). Specify:

- **Director**: rewrite `req.URL.Scheme` and `req.URL.Host` to the parsed
  `backendUrl`; preserve `req.URL.Path` and `req.URL.RawQuery` (the client's
  OpenAI path is forwarded as-is). Set `req.Host` to the backend's host so the
  backend sees a direct request.
- **FlushInterval: `-1`**: flush immediately after every write. This is
  essential for SSE streaming (`text/event-stream`) — without it, the proxy
  buffers the response and streaming tokens arrive in batches. With `-1`, the
  proxy flushes each chunk as it arrives, giving real-time token streaming.
- **ErrorHandler**: log the error at `ERROR` level; write a 502 Bad Gateway
  response in OpenAI error JSON format (see Error Handling). This covers
  connection refused, DNS failure, and backend crash mid-request.
- **Timeouts**: the `http.Server` already has no per-request timeout (requests
  can be long-running streaming completions). The gateway's
  `gateway.requestTimeout` wraps the `EnsureReady` wait phase only (the time
  spent waiting for startup), not the proxy phase. The proxy phase inherits the
  client's request context (which the client or `http.Server` may cancel).
- **Headers**: pass through all client headers (Authorization, Content-Type,
  Accept). Add `X-Forwarded-For` with the client's remote address. Do not add
  `X-Forwarded-Host` or `X-Forwarded-Proto` (the backend is on localhost; not
  needed for a LAN tool).

Specify that the route is registered as `/v1/{rest...}` (Go 1.22+ ServeMux
wildcard, matches all methods and all paths under `/v1/`). The codebase already
uses Go 1.24.4 ServeMux method-based patterns (`GET /status`, etc.); the
wildcard pattern is compatible and does not conflict with existing routes.

#### 6. Idle Shutdown

The gateway runs a background goroutine (`idleWatcher`) only when
`gateway.enabled` is true and `gateway.idleTimeout > 0`.

**Activity tracking** (in the `Gateway` struct, guarded by `activeMu`):
- `active int` — count of in-flight proxied requests.
- `lastActivity time.Time` — updated on every request start and request end.

**Idle watcher loop**:
1. Tick at a reasonable interval (e.g. `gpu.pollInterval`, reused as in the
   existing codebase convention).
2. On each tick: acquire `activeMu`; if `active == 0` and
   `time.Since(lastActivity) >= idleTimeout`, mark `shouldShutdown`; release
   `activeMu`.
3. If `shouldShutdown`: call `machine.PowerOff()`. Check the result:
   - `ResultAccepted` — shutdown initiated; log at INFO.
   - `ResultAlreadyInState` — already Off; no action.
   - `ResultConflict` — a transition is in progress (e.g. a request just
     triggered startup); skip this tick.

**Race between new request and shutdown**: The critical race is a request
arriving at the exact moment the idle watcher calls `PowerOff()`.

- **Layer 1 (prevent)**: the idle watcher checks `active == 0` under `activeMu`
  before calling `PowerOff()`. The request handler increments `active` under
  `activeMu` *before* calling `EnsureReady`. So if a request is in-flight, the
  idle watcher sees `active > 0` and skips.
- **Layer 2 (handle)**: if a request arrives in the narrow window between the
  idle watcher's `activeMu.Unlock()` and `PowerOff()` succeeding, the state
  becomes `ShuttingDown`. The request's `EnsureReady` sees `ShuttingDown`, waits
  for it to complete (→ `Off`), then triggers `PowerOn()` (→ `Starting` →
  `Ready`). The request is proxied after a full shutdown+restart cycle. This is
  correct behavior — the client experiences latency (bounded by
  `shutdown.timeout + startup.timeout`) but no data loss or errors.

Discuss why this two-layer approach is simpler than cancelling an in-progress
shutdown (which would require modifying the state machine's transition logic).

#### 7. Synchronization

List each primitive and justify it:

- **`stateMu` (sync.Mutex, existing)**: guards `state`, `lastError`, and the new
  `changeCh`. Held briefly in `setState` and `EnsureReady`'s state checks. Never
  held across a `PowerOn()` call (deadlock prevention).
- **`changeCh` (chan struct{}, new)**: state-change broadcast. Closed and
  recreated in `setState`. `EnsureReady` selects on it vs. `ctx.Done()`. Chosen
  over `sync.Cond` because `Cond` does not support `context.Context`
  cancellation. Chosen over polling because it is responsive (no poll-interval
  delay) and context-aware.
- **`transitionMu` (sync.Mutex with TryLock, existing)**: ensures only one
  state transition runs at a time. Ownership passed to the async goroutine.
  Unchanged.
- **`activeMu` (sync.Mutex, new in gateway)**: guards `active` and
  `lastActivity`. Held briefly in the request handler (increment/decrement) and
  in the idle watcher (check). Never held during proxying or `PowerOff()`.
- **`sync.WaitGroup` (existing)**: backs `Machine.Wait()`. The gateway's idle
  watcher uses its own `context.Context` for lifecycle, not the WaitGroup.
- **`context.Context`**: propagated from the HTTP request through `EnsureReady`
  and the proxy. The idle watcher uses a separate context (cancelled on
  SIGINT/SIGTERM). Consistent with the existing codebase convention
  (see `docs/learnings.md`: "context propagation, timeouts, and error logging
  by default").
- **Goroutines**: one per in-flight request (managed by `net/http.Server`), one
  idle watcher (long-lived, gateway lifecycle). No goroutine pool needed.

#### 8. Error Handling

Describe behavior for each failure scenario:

| Scenario | State after | Client sees | Retry? |
|---|---|---|---|
| Shelly unreachable | `Error` | 503 `service_unavailable` | No (manual `POST /power/off` to reset) |
| GPU never appears (timeout) | `Error` | 503 `service_unavailable` | No |
| llama-swap fails to start | `Error` | 503 `service_unavailable` | No |
| Backend health check fails (timeout) | `Error` | 503 `service_unavailable` | No |
| Proxy connection fails | (unchanged) | 502 `bad_gateway` | No |
| Backend crashes mid-request | (unchanged) | 502 `bad_gateway` | No |
| `EnsureReady` context cancelled | (unchanged) | 503 `service_unavailable` | Client may retry |

**OpenAI error response format** (for all gateway errors):

```json
{
  "error": {
    "message": "backend is starting up, please retry",
    "type": "service_unavailable",
    "code": "backend_starting"
  }
}
```

Error types: `service_unavailable` (503, startup failed or in progress),
`bad_gateway` (502, proxy connection failed).

**No automatic retry**: consistent with the existing manual-control design
where `Error` requires `POST /power/off` to reset. The client (e.g. Hermes,
OpenWebUI) is expected to retry with backoff — this is standard OpenAI client
behavior.

**Manual recovery**: `POST /power/off` always works (even from `Error`), so the
user can reset the system and the next gateway request triggers a fresh
startup.

#### 9. Configuration

New and modified config fields:

| Field | Type | Default | Required | Description |
|---|---|---|---|---|
| `gateway.enabled` | bool | `false` | no | Opt-in flag. When false, no gateway route is registered. |
| `gateway.idleTimeout` | Duration | `30m` | no | Idle period before auto-shutdown. `0` disables. |
| `gateway.requestTimeout` | Duration | `120s` | no | Timeout for the `EnsureReady` wait phase (startup wait). |
| `llamaSwap.backendUrl` | string | (none) | yes (when `gateway.enabled`) | Proxy target URL, e.g. `http://localhost:1234`. |

Validation rules (added to `config.validate`):
- When `gateway.enabled` is true, `llamaSwap.backendUrl` must be non-empty and a
  valid URL (parseable by `net/url.Parse`).
- `gateway.idleTimeout` must be `>= 0` (`0` is valid = disabled).
- `gateway.requestTimeout` must be `> 0`.

The existing `Duration` type with `UnmarshalYAML` handles YAML parsing for the
new duration fields — no new parsing code needed.

Example config with gateway enabled:

```yaml
gateway:
  enabled: true
  idleTimeout: 30m
  requestTimeout: 120s
llamaSwap:
  healthUrl: http://localhost:1234/v1/models
  backendUrl: http://localhost:1234
```

#### 10. Logging

Use the existing `slog.Logger` (text handler to stderr, as in `main.go`).

| Level | When |
|---|---|
| INFO | Gateway enabled at startup; startup triggered by gateway request; shutdown triggered by idle watcher; state transitions (existing). |
| WARN | Idle timeout approaching (e.g. at 80% of idleTimeout); probe failures (existing). |
| ERROR | Startup/shutdown failures (existing); proxy connection errors; `EnsureReady` returned error. |
| DEBUG | Active request count changes; idle timer resets; `EnsureReady` state checks. |

Avoid excessive noise: active-count changes are DEBUG (not INFO). Idle
approaching is WARN only once per idle cycle (not on every tick). State
transitions remain INFO (existing behavior).

#### 11. Testing Strategy

Recommend test categories, all using stdlib `testing` + `net/http/httptest`
(consistent with existing conventions in `AGENTS.md`):

- **Proxy unit tests** (`internal/gateway/gateway_test.go`):
  - Request forwarded to backend with correct path/method/headers.
  - Response body forwarded correctly (non-streaming).
  - SSE streaming: response flushed incrementally (verify chunks arrive
    separately, not buffered).
  - 503 when `EnsureReady` returns error (Error state).
  - 502 when backend connection fails (backend server closed mid-test).
  - Active counter incremented/decremented correctly.

- **EnsureReady unit tests** (`internal/state/state_test.go`, extend existing):
  - Off → PowerOn triggered → waits → Ready → returns nil.
  - Starting → waits → Ready → returns nil.
  - Ready → returns nil immediately.
  - Error → returns error immediately.
  - ShuttingDown → waits → Off → PowerOn → Ready → returns nil.
  - Context cancelled → returns `ctx.Err()`.
  - Concurrent requests → single startup (one `PowerOn` accepted, rest
    `ResultConflict`, all wait on same channel).

- **Idle shutdown unit tests** (`internal/gateway/gateway_test.go`):
  - Idle timeout elapses with no active requests → `PowerOff` called.
  - Active request prevents shutdown.
  - Request arrives during shutdown → full restart cycle (correctness, not
    latency).
  - `idleTimeout: 0` → idle watcher does not run.

- **Integration test** (httptest backend + fake state machine):
  - Full flow: request → auto-start → proxy → response → idle → auto-shutdown.

Identify the most critical scenarios: concurrent startup (single transition
guarantee), idle race (no data loss), SSE streaming (no buffering).

#### 12. Incremental Implementation Plan

Four milestones, each leaving DockMind in a working state:

**Milestone 1 — Reverse Proxy**
- Objective: proxy `/v1/*` to backend when Ready; 503 when not Ready.
- Dependencies: `gateway.enabled` + `llamaSwap.backendUrl` config fields.
- Risks: route conflicts with existing ServeMux patterns (low — `/v1/` is a new
  prefix).
- Expected outcome: manual control unchanged; proxy works when backend is
  already up. No auto-start, no idle shutdown.

**Milestone 2 — Auto-Startup on First Request**
- Objective: `EnsureReady` + state-change signaling; request triggers startup
  if backend is down.
- Dependencies: Milestone 1; `internal/state` changes.
- Risks: deadlock in `EnsureReady` (stateMu held across PowerOn); channel
  lifecycle (closing a nil or already-closed channel).
- Expected outcome: first request auto-starts the backend; concurrent requests
  wait for a single startup. No idle shutdown yet.

**Milestone 3 — Idle Auto-Shutdown**
- Objective: idle watcher goroutine; active-request tracking; auto-shutdown
  after idle timeout.
- Dependencies: Milestone 2; `gateway.idleTimeout` config field.
- Risks: race between request and shutdown; idle watcher goroutine leak on
  shutdown.
- Expected outcome: full gateway — auto-start on request, auto-shutdown on
  idle.

**Milestone 4 — Polish**
- Objective: OpenAI error format; config validation; OpenAPI spec update;
  README + product.md updates; logging refinement.
- Dependencies: Milestones 1–3.
- Risks: README test enforcement (existing `readme_test.go` constraints).
- Expected outcome: production-ready gateway with documentation.

#### Final Review

Critically review the design:

- **Unnecessary complexity**: none identified. The design reuses the existing
  state machine, adds one method + one channel, and uses stdlib
  `ReverseProxy`. No new states, no goroutine pools, no third-party deps.
- **Possible simplifications**: the state-change channel could be replaced with
  polling (simpler code, but adds up to `pollInterval` latency per request).
  The channel approach is preferred for responsiveness; polling is documented
  as a fallback.
- **Architectural risks**: the shutdown+restart race (Layer 2 in Idle Shutdown)
  adds latency in a rare case. This is acceptable for a homelab tool and
  simpler than adding shutdown cancellation to the state machine.
- **Alternative approaches considered**:
  - Adding `Busy`/`Idle` states: rejected — adds transition complexity and
    breaks existing tests for no functional benefit.
  - `sync.Cond` instead of channel: rejected — no `context.Context` support.
  - Third-party reverse proxy (e.g. `chi`, `gin`): rejected — violates the
    "standard library whenever possible" constraint.
  - Request queuing during startup: rejected — listed as a non-goal in the MVP
    spec; concurrent requests wait on the channel instead.

Confirm: this is the simplest architecture that fully satisfies the proposal's
requirements.

### Test approach

Create `gateway_design_test.go` at the repo root (`package dockmind_test`),
following the `readme_test.go` / `product_test.go` pattern. The test reads
`docs/DockMind_Gateway_Design.md` and asserts (case-insensitive via
`strings.Contains` on lowercased body) that the document contains:

- All 13 section headings (High-Level Architecture, Go Package Structure, State
  Machine, Startup Sequence, Reverse Proxy, Idle Shutdown, Synchronization,
  Error Handling, Configuration, Logging, Testing Strategy, Incremental
  Implementation Plan, Final Review).
- Key design decisions: `opt-in`, `gateway.enabled`, `backendUrl`,
  `idleTimeout`, `30m`, `EnsureReady`, `/v1/`, `power/off`, `ReverseProxy`,
  `Milestone`, `SSE`.

The test uses a table-driven `cases` slice (same pattern as `readme_test.go`),
with `t.Run` per case. Case-insensitive matching avoids false failures from
heading capitalization.

### product.md update

Add a Features entry after the Favicon & Logo line, matching the existing
format:

```
- **OpenAI Gateway (design)** — design document for an opt-in OpenAI-compatible reverse proxy with automatic startup on first request and idle auto-shutdown ([007-openai-gateway](../stories/007-openai-gateway/story.md))
```

Remove two Non-Goals that are now designed (no longer permanent non-goals):
- "Proxy OpenAI-compatible inference requests."
- "Automatically power down after idle."

Add a Known Limitation:

```
- The OpenAI gateway is designed but not yet implemented. See [DockMind_Gateway_Design.md](DockMind_Gateway_Design.md) for the design and incremental implementation plan.
```

Update the opening paragraph: change "it does not proxy inference requests" to
reflect the planned gateway (e.g. "An opt-in OpenAI-compatible gateway mode is
designed but not yet implemented; see the gateway design document.").

### product_test.go update

Add one assertion to `TestProductDoc` (after the `006-add-favicon-logo` check):

```go
if !strings.Contains(body, "007-openai-gateway") {
    t.Error("docs/product.md Features list does not reference the 007-openai-gateway story")
}
```

### README update

Add one line to the "Further Reading" section (after the product overview link):

```
- [Gateway Design](docs/DockMind_Gateway_Design.md) — design for the opt-in OpenAI-compatible gateway with auto-startup and idle shutdown.
```

No other README sections change. The existing `readme_test.go` ACs remain
satisfied — the addition only adds content and does not touch the fenced yaml
block or any checked substring.

## Tasks

### Task 1 - Gateway design document with automated validation

- `docs/DockMind_Gateway_Design.md` created + `gateway_design_test.go` created at repo root + `make test`
  - → `gateway_design_test.go` passes: file `docs/DockMind_Gateway_Design.md` exists and is non-empty
  - → document contains the substring "High-Level Architecture" (case-insensitive)
  - → document contains the substring "Go Package Structure" (case-insensitive)
  - → document contains the substring "State Machine" (case-insensitive)
  - → document contains the substring "Startup Sequence" (case-insensitive)
  - → document contains the substring "Reverse Proxy" (case-insensitive)
  - → document contains the substring "Idle Shutdown" (case-insensitive)
  - → document contains the substring "Synchronization" (case-insensitive)
  - → document contains the substring "Error Handling" (case-insensitive)
  - → document contains the substring "Configuration" (case-insensitive)
  - → document contains the substring "Logging" (case-insensitive)
  - → document contains the substring "Testing Strategy" (case-insensitive)
  - → document contains the substring "Incremental Implementation Plan" (case-insensitive)
  - → document contains the substring "Final Review" (case-insensitive)
  - → document contains the substring "opt-in" (case-insensitive)
  - → document contains the substring "gateway.enabled" (case-insensitive)
  - → document contains the substring "backendUrl" (case-insensitive)
  - → document contains the substring "idleTimeout" (case-insensitive)
  - → document contains the substring "30m" (case-insensitive)
  - → document contains the substring "EnsureReady" (case-insensitive)
  - → document contains the substring "/v1/" (case-insensitive)
  - → document contains the substring "power/off" (case-insensitive)
  - → document contains the substring "ReverseProxy" (case-insensitive)
  - → document contains the substring "Milestone" (case-insensitive)
  - → document contains the substring "SSE" (case-insensitive)
- `make lint` from repo root
  - → `gofmt -l .` prints no file paths and `go vet ./...` reports no issues

### Task 2 - product.md, product_test.go, and README updates with automated validation

- `docs/product.md` Features list includes `007-openai-gateway` entry + `product_test.go` updated with `007-openai-gateway` assertion + `make test`
  - → `product_test.go` passes (product.md contains `007-openai-gateway`)
- `docs/product.md` Non-Goals no longer lists "Proxy OpenAI-compatible inference requests" or "Automatically power down after idle" + `make test`
  - → existing `product_test.go` assertions still pass (product.md still contains `004-web-ui` and `006-add-favicon-logo`; product.md still does NOT contain "Web UI, Prometheus metrics, or request queuing during startup")
- README "Further Reading" section includes a link to `docs/DockMind_Gateway_Design.md` + `make test`
  - → existing `readme_test.go` assertions still pass (all 25 substring cases, no License section, first fenced yaml block loads via `config.Load`)

## Technical Context

- **Go 1.24.4** — the module's toolchain (`go.mod`). All features referenced in
  the design (`httputil.ReverseProxy`, ServeMux `{rest...}` wildcards,
  `sync.Mutex`, channels, `context.Context`, `log/slog`) are stable stdlib.
- **`net/http/httputil.ReverseProxy`** — stdlib reverse proxy. `FlushInterval:
  -1` flushes after every write, enabling SSE streaming without buffering. The
  `Director` function rewrites the request URL; `ErrorHandler` handles upstream
  connection failures. No third-party proxy library is needed.
- **Go 1.22+ ServeMux wildcards** — `{rest...}` matches the remaining path
  including slashes. The codebase already uses Go 1.22+ method-based patterns
  (`GET /status`, `GET /{$}`). A method-agnostic `/v1/{rest...}` pattern
  coexists with method-specific patterns (more specific patterns take
  precedence).
- **Channel-based state-change signaling** — `chan struct{}` closed and
  recreated under `stateMu`. Preferred over `sync.Cond` (which lacks
  `context.Context` support) and over polling (which adds latency). This is a
  new pattern for the codebase but uses only stdlib primitives.
- **Existing `Duration` type** — `internal/config/config.go` defines a
  `Duration` type with `UnmarshalYAML` that parses YAML duration strings (e.g.
  `30m`, `120s`). The new `gateway.idleTimeout` and `gateway.requestTimeout`
  fields reuse this type — no new parsing code.
- **No new external dependencies** — `go.mod` remains
  `gopkg.in/yaml.v3 v3.0.1` only. The design uses exclusively stdlib packages.
- **Existing test conventions** — stdlib `testing` + `net/http/httptest`,
  hand-written fakes, table-driven tests, no testify/mocking libraries (per
  `AGENTS.md`). The design's testing strategy follows these conventions.

## Notes

- **Design-only deliverable.** This story produces a Markdown document and its
  validation test. No production code is written. Each milestone in the
  design's incremental plan is a separate follow-up story.
- **The design document must be detailed enough to implement without further
  design decisions.** Each of the 12 areas specifies concrete types, method
  signatures, config fields, error responses, and test scenarios — not just
  prose descriptions.
- **The design document should reference the existing codebase.** Cite specific
  files (`internal/state/state.go`, `internal/api/api.go`, etc.) and existing
  patterns (TryLock transitionMu, WaitGroup-backed Wait, poll loop) so the
  implementer of each milestone can find the code to modify.
- **The proposal document (`docs/DockMind_as_Gateway_Proposal.md`) is the
  input; the design document is the output.** The design answers all 12 areas
  the proposal lists and includes the final critical review the proposal
  requests.
- **product.md Non-Goals adjustment is intentional.** Removing "Proxy
  OpenAI-compatible inference requests" and "Automatically power down after
  idle" from Non-Goals reflects that these are now designed (planned), not
  permanently excluded. The Known Limitations section notes the gateway is
  designed but not yet implemented, so the product doc remains honest. This
  follows the precedent of story 004 (Web UI non-goal removed in the plan
  commit before implementation).
- **The `gateway_design_test.go` test file is in `package dockmind_test`** at
  the repo root, matching `readme_test.go` and `product_test.go`. It does not
  import `internal/config` (unlike `readme_test.go`) — it only reads and
  string-matches the design document.
