# Auto-Ready on Startup

## Context

When DockMind starts, `state.New` hard-codes the initial state to `Off` and `main.go` never reconciles with the live system. If the daemon is restarted (e.g. after a host reboot that left the Shelly plug on, the eGPU enumerated, and `llama-swap` already running and healthy), DockMind reports `Off` and a subsequent `POST /power/off` is a no-op while `POST /power/on` would redundantly re-run the full startup sequence (Shelly ON, wait for GPU, docker start, wait for health). The system is already up; the state machine should reflect that. This story adds a one-shot startup reconcile so DockMind enters `Ready` directly when all four dependencies are already up.

## Out of Scope

- Auto-restart on partial conditions (e.g. Shelly on + GPU present but llama-swap unhealthy). If only some conditions are met, DockMind stays in `Off` and the user must drive a normal `POST /power/on`.
- Background re-reconciliation after startup. Reconcile runs exactly once, synchronously, before the HTTP server accepts requests.
- A config flag to disable the behavior. Reconcile is always-on.
- Changes to the `api.StateMachine` interface or HTTP routes. Reconcile is a startup-only method on `*state.Machine`, called directly from `main.go`.

## Implementation approach

Add a `Reconcile() bool` method to `*state.Machine` in `internal/state/state.go` and call it once from `cmd/dockmind/main.go` after the machine is fully constructed (after `SetAuxContainers`) and before `api.NewServer` begins serving.

### Reconcile method

```go
func (m *Machine) Reconcile() bool
```

- Attempts to acquire `transitionMu` with `TryLock()`. If it fails (another transition holds the lock), returns `false` immediately — a transition is already in progress and Reconcile should not interfere. This matches the `TryLock` pattern used by `PowerOn`/`PowerOff`/`Restart` and avoids deadlocking if Reconcile is ever called while a transition is running. On success, defers the unlock.
- Reads the current state under `stateMu`; if it is not `Off`, returns `false` immediately (no-op, releases the lock via defer). This guards against being called after a transition has already changed the state.
- Probes the four dependencies using the existing helpers so timeouts (10s each) and log-level rules are identical to `Status()`:
  - `m.probeBool("Shelly", false, ...)` → `power.IsOn` (Shelly failures stay WARN, per story 013).
  - `m.probeGPU(probeFailureExpected(Off))` → `gpu.Status` (GPU failures DEBUG in Off, per story 013).
  - `m.probeBool("Docker", false, ...)` → `docker.IsRunning` (Docker failures stay WARN).
  - `m.probeHealth(probeFailureExpected(Off))` → `health.Check` (health failures DEBUG in Off).
- If **all four** are true (`shellyOn && gpuPresent && running && healthy`): sets state to `Ready` under `stateMu`, clears `lastError`, closes and recreates `changeCh` (so any `EnsureReady` waiter sees the change), but **does not stamp `lastReadyTime`**. This is the critical difference from `setState(Ready, nil)`: because no power cycle occurred, the post-startup cooldown (`power.cooldown`) must not activate. Logs `Info("State -> Ready (startup reconcile)")`. Returns `true`.
- Otherwise: leaves state as `Off`, logs `Debug("startup reconcile: system not fully up, staying Off", ...)` with the four probe booleans, returns `false`. Debug (not Info) because the system being off at startup is the common case and should not be noisy.

### Why not reuse `setState`

`setState(Ready, nil)` stamps `lastReadyTime = time.Now()`, which would activate the post-startup cooldown and block an immediate `POST /power/off` with 429. Reconcile must set `Ready` without that stamp, so it inlines the state mutation (set `state`, clear `lastError`, cycle `changeCh`) under `stateMu` instead of calling `setState`.

### main.go wiring

In `cmd/dockmind/main.go`, insert `machine.Reconcile()` immediately after `machine.SetAuxContainers(auxManager)` (line 61) and before `server := api.NewServer(machine, logger)` (line 63). No return-value handling is required — the method logs internally — but a one-line `logger.Info` guard is acceptable. The call is synchronous: the HTTP server does not start until Reconcile returns, guaranteeing `/status` reports the correct state from the first request.

### Edge cases

- **Probe error on any dependency**: the probe helpers return safe defaults (`false`/empty) on error, so any unreachable dependency causes Reconcile to stay in `Off`. This is the existing `Status()` behavior and is reused unchanged.
- **Reconcile called when state is not `Off`**: returns `false` without probing or mutating state.
- **Cooldown configured**: because `lastReadyTime` is not stamped, `cooldownActiveLocked(Ready)` returns `false` (the `lastTime.IsZero()` branch), so `POST /power/off` after a reconcile is accepted, not 429.
- **Gateway enabled**: `EnsureReady` (used by the gateway) checks state in a loop; if Reconcile already set `Ready`, `EnsureReady` returns `nil` immediately on its first iteration. No gateway code changes needed.

## Tasks

### Task 1 - Reconcile method on the state machine

- machine in `Off` state + Shelly on, GPU present, llama-swap running, llama-swap healthy + `Reconcile()` called
  - → returns `true`
  - → `m.State()` is `Ready`
  - → `m.lastError` is `nil`
- machine in `Off` state + Shelly off (all other conditions irrelevant) + `Reconcile()` called
  - → returns `false`
  - → `m.State()` is `Off`
- machine in `Off` state + Shelly on, GPU not present + `Reconcile()` called
  - → returns `false`
  - → `m.State()` is `Off`
- machine in `Off` state + Shelly on, GPU present, llama-swap not running + `Reconcile()` called
  - → returns `false`
  - → `m.State()` is `Off`
- machine in `Off` state + Shelly on, GPU present, llama-swap running, llama-swap not healthy + `Reconcile()` called
  - → returns `false`
  - → `m.State()` is `Off`
- machine in `Off` state + Shelly `IsOn` returns error + `Reconcile()` called
  - → returns `false`
  - → `m.State()` is `Off`
- machine in `Off` state + GPU `Status` returns error + `Reconcile()` called
  - → returns `false`
  - → `m.State()` is `Off`
- machine already in `Ready` + `Reconcile()` called
  - → returns `false`
  - → `m.State()` stays `Ready`
- machine already in `Starting` (a transition holds `transitionMu`) + `Reconcile()` called
  - → returns `false`
  - → `m.State()` stays `Starting`

### Task 2 - Reconcile does not activate the post-startup cooldown

- machine with `cooldown > 0`, in `Off` + all four dependencies up + `Reconcile()` called, then `PowerOff()` called immediately
  - → `Reconcile()` returns `true`, state is `Ready`
  - → `PowerOff()` returns `ResultAccepted` (not `ResultCooldown`)
  - → after `m.Wait()`, state is `Off`
- machine with `cooldown > 0`, in `Off` + all four dependencies up + `Reconcile()` called, then `PowerOn()` called
  - → `PowerOn()` returns `ResultAlreadyInState` (already Ready)

### Task 3 - Reconcile is wired into main.go startup

- This task is verified by the Task 1 and Task 2 unit tests plus a `go build` of `cmd/dockmind`. The wiring is a single `machine.Reconcile()` call after `SetAuxContainers` and before `api.NewServer`; no automated integration test covers `main.go` (consistent with the rest of the codebase, which has no `main_test.go`). Correctness of the call site is confirmed by `make build` and `make lint`.

## Technical Context

- Go 1.24.4, module `github.com/dockmind/dockmind`. No new dependencies.
- The existing probe helpers (`probeGPU`, `probeBool`, `probeHealth`) each create a `context.WithTimeout(context.Background(), 10*time.Second)` and log at WARN or DEBUG per `probeFailureExpected(state)`. Reconcile reuses them with `probeFailureExpected(Off)` so GPU/health failures are DEBUG and Shelly/Docker failures are WARN — identical to a `Status()` call in the `Off` state.
- `setState` is defined at `internal/state/state.go:513` and stamps `lastReadyTime`/`lastOffTime`. Reconcile must NOT use it for the `Ready` transition to avoid activating cooldown.
- `cooldownActiveLocked` (state.go:561) returns `false` when `lastTime.IsZero()`, so leaving `lastReadyTime` unset keeps the cooldown inactive.
- `changeCh` is closed and recreated on every state mutation so `EnsureReady` waiters wake; Reconcile must do the same to keep that contract.

## Notes

- Reconcile is synchronous and runs before the HTTP server starts. On a trusted local network the four probes complete in well under a second; this does not meaningfully delay server readiness. If a dependency is unreachable, the 10s per-probe timeout bounds the worst case to ~40s, after which DockMind starts in `Off` (safe default).
- Reconcile is not part of the `api.StateMachine` interface and is not exposed over HTTP. It is a lifecycle method called only from `main.go`.
- No README change is required: Reconcile adds no route, no `StatusResponse` field, and no config key. The `readme_test.go` contract is unaffected.
- No `docs/DockMind_MVP_Specification.md` change is required for this story; the spec does not prescribe an initial state. A follow-up doc update is optional and not in scope.
