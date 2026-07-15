# Quiet expected probe warnings in non-Ready states

## Context

When the state machine is in the `Off` state, the web UI polls `GET /status`
once per second. Each call probes all four dependencies live. The GPU probe
(`nvidia-smi`) and the Health probe (llama-swap `/v1/models`) fail every time
because the eGPU is powered off and llama-swap is stopped — yet both failures
are logged at `WARN` level, producing repetitive warning spam in the journal:

```
WARN GPU status probe failed error="exit status 255"
WARN Health status probe failed error="... connection refused"
```

These failures are **expected** when the system is off (or transitioning), not
a cause for concern. The existing startup/shutdown poll loops already log
expected `nvidia-smi` failures at `DEBUG` (state.go:447, 519). This story
applies the same principle to the `Status()` probe path: downgrade GPU and
Health probe failures to `DEBUG` in states where the dependency is supposed to
be unavailable, and keep `WARN` only where a failure indicates a real problem.

## Out of Scope

- Changing the `StatusResponse` JSON fields or values — probes still return
  `false`/empty on error; only the log level changes.
- Changing probe behavior in the startup/shutdown `poll()` loops — those
  already log at `DEBUG` and are unaffected.
- Changing Shelly or Docker probe log levels — their backends (Shelly plug,
  Docker daemon) should always be reachable regardless of state, so failures
  remain `WARN` in all states.
- Updating `docs/learnings.md` or `docs/DockMind_Gateway_Design.md` — those
  are dev journals / design docs, not test-enforced artifacts.

## Implementation approach

### Rule

`Status()` captures the current state under `stateMu` into a local variable
before probing (state.go:232-237). Pass that captured state to the probe
helpers so they can choose the log level. Define a single predicate:

```
probeFailureExpected(state) == (state == Off || state == Starting || state == ShuttingDown)
```

| Probe   | `Off` | `Starting` | `ShuttingDown` | `Ready` | `Error` |
|---------|-------|------------|----------------|---------|---------|
| GPU     | DEBUG | DEBUG      | DEBUG          | WARN    | WARN    |
| Health  | DEBUG | DEBUG      | DEBUG          | WARN    | WARN    |
| Shelly  | WARN  | WARN       | WARN           | WARN    | WARN    |
| Docker  | WARN  | WARN       | WARN           | WARN    | WARN    |

Rationale:
- **Off / Starting / ShuttingDown** — the eGPU and llama-swap are supposed to
  be down or in flux; a probe failure is expected, not a warning.
- **Ready** — the GPU and llama-swap should be up; a probe failure signals a
  real problem.
- **Error** — the operator needs full diagnostics about what is failing;
  keeping `WARN` surfaces every failing dependency.
- **Shelly / Docker** — the Shelly plug and Docker daemon are host-level
  services that should always be reachable; a failure is never expected.

### Code changes

1. **`internal/state/state.go`** — `probeGPU()` and `probeBool()`:
   - `probeGPU` currently logs `WARN("GPU status probe failed", "error", err)`
     unconditionally. Add a `State` parameter; when
     `probeFailureExpected(state)` is true, log at `DEBUG` instead of `WARN`.
     Keep the same message string and `"error"` attr — only the level changes.
   - `probeBool` currently logs `WARN(name+" status probe failed", "error", err)`
     unconditionally. Add a `bool` parameter (e.g. `quietOnFailure`) that, when
     true, logs at `DEBUG` instead of `WARN`. In `Status()`, pass
     `quietOnFailure=false` for Shelly and Docker, and
     `quietOnFailure=probeFailureExpected(state)` for Health.
   - Add the unexported helper `func probeFailureExpected(s State) bool`.
   - Update the four call sites in `Status()` to pass the captured `state`
     (for `probeGPU`) and the `quietOnFailure` flag (for `probeBool`).

2. **`internal/state/state_test.go`** — test fakes and tests:
   - Add an `err error` field to `fakeHealth`; return it from `Check` when
     non-nil (mirrors `fakeGPU.err`). Default `nil` preserves existing tests.
   - Add an `isRunningErr error` field to `fakeDocker`; return it from
     `IsRunning` when non-nil (mirrors `fakePower.isOnErr`). Default `nil`
     preserves existing tests.
   - Update `TestStatusGPUProbeError`: the machine starts in `Off`, so the GPU
     probe failure must now assert `DEBUG` (not `WARN`). Rename or split it to
     also cover the `Ready` case where `WARN` is still expected.
   - Add table-driven tests covering the full matrix above (see ACs).

## Tasks

### Task 1 - State-conditional log levels for Status probes

All ACs use `newTestMachineWithRecorder()` (which provides a `recordingHandler`
that captures every level), set the fake's error field, set `m.state` directly
under `m.stateMu`, call `m.Status()`, then assert log levels via
`handler.hasRecord(level, msgSubstr)`.

**GPU probe — expected states (DEBUG):**

- state=Off + `gpu.err` set + `Status()` called
  - → `handler.hasRecord(slog.LevelDebug, "GPU status probe failed")` is true
  - → `handler.hasRecord(slog.LevelWarn, "GPU status probe failed")` is false
  - → `status.GPUPresent == false`, `status.GPUName == ""`

- state=Starting + `gpu.err` set + `Status()` called
  - → DEBUG log present, WARN log absent

- state=ShuttingDown + `gpu.err` set + `Status()` called
  - → DEBUG log present, WARN log absent

**GPU probe — unexpected states (WARN):**

- state=Ready + `gpu.err` set + `Status()` called
  - → `handler.hasRecord(slog.LevelWarn, "GPU status probe failed")` is true
  - → `handler.hasRecord(slog.LevelDebug, "GPU status probe failed")` is false

- state=Error + `gpu.err` set + `Status()` called
  - → WARN log present, DEBUG log absent

**Health probe — expected states (DEBUG):**

- state=Off + `health.err` set + `Status()` called
  - → `handler.hasRecord(slog.LevelDebug, "Health status probe failed")` is true
  - → `handler.hasRecord(slog.LevelWarn, "Health status probe failed")` is false
  - → `status.LlamaSwapHealthy == false`

- state=Starting + `health.err` set + `Status()` called
  - → DEBUG log present, WARN log absent

- state=ShuttingDown + `health.err` set + `Status()` called
  - → DEBUG log present, WARN log absent

**Health probe — unexpected states (WARN):**

- state=Ready + `health.err` set + `Status()` called
  - → `handler.hasRecord(slog.LevelWarn, "Health status probe failed")` is true
  - → `handler.hasRecord(slog.LevelDebug, "Health status probe failed")` is false

- state=Error + `health.err` set + `Status()` called
  - → WARN log present, DEBUG log absent

**Shelly probe — always WARN:**

- state=Off + `power.isOnErr` set + `Status()` called
  - → `handler.hasRecord(slog.LevelWarn, "Shelly status probe failed")` is true
  - → `handler.hasRecord(slog.LevelDebug, "Shelly status probe failed")` is false

- state=Ready + `power.isOnErr` set + `Status()` called
  - → WARN log present, DEBUG log absent

**Docker probe — always WARN:**

- state=Off + `docker.isRunningErr` set + `Status()` called
  - → `handler.hasRecord(slog.LevelWarn, "Docker status probe failed")` is true
  - → `handler.hasRecord(slog.LevelDebug, "Docker status probe failed")` is false

- state=Ready + `docker.isRunningErr` set + `Status()` called
  - → WARN log present, DEBUG log absent

**Existing test regression:**

- `TestStatusGPUProbeError` updated + `make test` + `make lint`
  - → both pass (test now asserts DEBUG in Off, WARN in Ready)

## Technical Context

- No new dependencies. The change uses only stdlib `log/slog` already imported
  in `internal/state/state.go`.
- `recordingHandler` (state_test.go:70-97) captures all levels because
  `Enabled` returns `true` unconditionally, and `hasRecord(level, msgSubstr)`
  checks both the level and a message substring — sufficient for asserting
  "DEBUG present" and "WARN absent" in the same test.
- `fakeGPU` already has an `err` field (state_test.go:58) and `fakePower`
  already has `isOnErr` (state_test.go:18); only `fakeHealth` and `fakeDocker`
  need new error fields.
- The `Status()` method captures `state` under `stateMu` at the top
  (state.go:232-237) and releases the lock before probing. The probe helpers
  must use this captured state value, not re-read `m.state` — consistent with
  the existing snapshot-then-probe design.

## Notes

- The message strings stay identical across DEBUG and WARN (e.g.
  `"GPU status probe failed"`); only the `slog.Level` changes. This lets tests
  distinguish by level via `hasRecord` and keeps log grepping stable.
- The `Error` state intentionally keeps `WARN` for GPU/Health: the failing
  dependency is variable/unknown (could be Shelly, Docker, GPU, or Health that
  caused the error), so the operator needs every failure surfaced.
- This refines the `docs/learnings.md` guidance ("log errors at Warn level
  rather than swallowing them"): unexpected probe failures remain `WARN`, but
  expected ones (dependency supposed to be down) are `DEBUG`. The learnings doc
  itself is not updated — it is a historical dev journal.
