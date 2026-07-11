# Demote nvidia-smi failure logs from WARN to DEBUG during power transitions

## Context

When DockMind powers the eGPU on or off, it polls `nvidia-smi` repeatedly until
the GPU appears (startup) or disappears (shutdown). Until the GPU is ready
(startup) or after it is gone (shutdown), `nvidia-smi` exits non-zero — this is
expected, not an error. However, `gpu.Monitor.Status()` logs every `nvidia-smi`
failure at WARN level unconditionally, producing repeated warning spam during
every transition (4–5 WARN lines per power-on, 1+ per power-off in production
logs). These should be DEBUG-level messages during transitions, since they
represent normal polling behavior, not problems.

The root cause is that `gpu.Monitor.Status()` logs internally at WARN but has no
knowledge of the caller's context (startup, shutdown, or live status probe). The
fix moves the logging decision to the state machine, which knows the transition
context and can choose the appropriate level: DEBUG during transitions (expected
failures) and WARN during status probes (potentially unexpected failures).

## Out of Scope

- Demoting nvidia-smi failure logs during `GET /status` probes in the `Off`
  state (where the GPU is intentionally powered off). These remain at WARN via
  `probeGPU()`. This is the existing behavior and a separate concern.
- Adding a configurable `--log-level` flag or runtime log-level control.
- Changing the `GPUMonitor` interface signature (still
  `Status(ctx) (bool, string, error)`).

## Implementation approach

**Move logging from `gpu.Monitor.Status()` to the state machine callers.** The
`gpu.Monitor` currently swallows the `nvidia-smi` error (returns `nil`) and logs
at WARN internally. Instead:

1. **`gpu.Monitor.Status()`** returns the error (`false, "", err`) instead of
   swallowing it, and no longer logs. The `logger` field and `log/slog` import
   are removed from the `gpu` package entirely. `New()` takes no arguments.

2. **`state.Machine.startup()` poll check** (state.go ~line 343) logs
   `nvidia-smi` failures at `slog.LevelDebug` and passes the error through to
   `poll()` — which treats non-nil errors as "not ready yet" and continues
   polling (unchanged behavior). On timeout, `poll()` wraps `lastErr` into the
   error message, so the last `nvidia-smi` error remains visible in the timeout
   diagnostic.

3. **`state.Machine.shutdown()` poll check** (state.go ~line 405) treats a
   `nvidia-smi` error as "GPU gone" by returning `(true, nil)` to `poll()`, and
   logs at `slog.LevelDebug`. This preserves the current behavior where
   `nvidia-smi` failure during shutdown completes the transition to `Off`
   immediately.

4. **`state.Machine.probeGPU()`** (state.go ~line 236) already contains
   `m.logger.Warn("GPU status probe failed", "error", err)` for non-nil errors.
   This was previously dead code because `gpu.Status()` always returned `nil`.
   It now fires for real `nvidia-smi` failures during `GET /status` probes,
   keeping unexpected failures visible at WARN. No code change needed here —
   only the `gpu.Status()` return-value change activates it.

**Why not just change WARN to DEBUG in `gpu.Status()`?** A global demotion would
also silence unexpected `nvidia-smi` failures during status probes in the
`Ready` state (e.g., GPU disappears while serving requests). The contextual
approach keeps WARN for potentially unexpected failures and DEBUG for expected
transition polling.

**`poll()` behavior is preserved:** `poll()` returns nil only when
`err == nil && ok`. For startup, `nvidia-smi` failure returns `(false, err)` →
poll continues. For shutdown, the check closure returns `(true, nil)` on error →
poll returns nil immediately. On startup timeout, `lastErr` includes the last
`nvidia-smi` error (wrapped by `poll()`). On shutdown, if `nvidia-smi` fails the
check returns `(true, nil)` so no timeout occurs; if `nvidia-smi` succeeds but
GPU remains present, `lastErr` is nil and the timeout message is just
`ctx.Err()` — same as current behavior.

**Files changed:**

- `internal/gpu/gpu.go` — remove `logger` field, `log/slog` import; `New()` takes
  no args; `Status()` returns `false, "", err` on exec failure.
- `internal/gpu/gpu_test.go` — update `TestStatus` to expect non-nil errors for
  failure cases; replace `TestStatusSwallowsExecError` with a test asserting the
  error is returned.
- `internal/state/state.go` — add `Debug` logging in `startup()` and `shutdown()`
  poll check closures; shutdown closure returns `(true, nil)` on error.
- `internal/state/state_test.go` — add `err` field to `fakeGPU`; add recording
  `slog.Handler` for log-level assertions; add tests for Tasks 2–4.
- `cmd/dockmind/main.go` — change `gpu.New(logger)` to `gpu.New()`.

## Tasks

### Task 1 - gpu.Monitor.Status() returns nvidia-smi errors instead of swallowing them

- nvidia-smi exits non-zero + `Status()` called
  - → returns `(false, "", non-nil-error)`
  - → no log output from the `gpu` package (logger removed)
- nvidia-smi binary not found + `Status()` called
  - → returns `(false, "", non-nil-error)`
- nvidia-smi succeeds with GPU name in output + `Status()` called
  - → returns `(true, name, nil)`
- nvidia-smi succeeds with empty output + `Status()` called
  - → returns `(false, "", nil)`
- `gpu.New()` called with no arguments
  - → returns a `*Monitor` with a working default `execFunc`

### Task 2 - Startup transition logs nvidia-smi failures at DEBUG

- GPU absent with nvidia-smi error + PowerOn initiated + GPU becomes present
  - → state transitions to `Ready`
  - → nvidia-smi failures logged at `slog.LevelDebug` (message contains "nvidia-smi")
  - → no WARN-level records with "nvidia-smi" in the message
- GPU absent with nvidia-smi error + PowerOn initiated + GPU never appears (timeout)
  - → state transitions to `Error`
  - → `lastError` contains "gpu" and "timeout"
  - → nvidia-smi failures logged at `slog.LevelDebug`

### Task 3 - Shutdown transition treats nvidia-smi failure as GPU gone and logs at DEBUG

- GPU present + PowerOff initiated + nvidia-smi starts returning error (GPU disappears)
  - → state transitions to `Off`
  - → nvidia-smi failures logged at `slog.LevelDebug`
  - → no WARN-level records with "nvidia-smi" in the message

### Task 4 - Status probe logs nvidia-smi failures at WARN

- nvidia-smi returns error + `Machine.Status()` called
  - → `gpuPresent` is `false`
  - → `gpuName` is empty string
  - → failure logged at `slog.LevelWarn` (message contains "GPU status probe failed")

## Notes

- **`cmd/dockmind/main.go`** must be updated: `gpu.New(logger)` → `gpu.New()`.
  The `log/slog` import in `main.go` stays — it is still used to create the
  logger passed to `state.New` and `api.NewServer`.
- **`fakeGPU` in `state_test.go`** needs an `err error` field to simulate
  `nvidia-smi` failures. When `present` is `false` and `err` is non-nil,
  `Status()` returns `(false, "", err)`. When `present` is `true`, `Status()`
  returns `(true, name, nil)` regardless of `err`. Existing tests leave `err` as
  nil (zero value), preserving current behavior.
- **Log level testing** requires a recording `slog.Handler` that captures all
  records including DEBUG. There is no existing log-capture pattern in the
  codebase. Implement a minimal handler satisfying `slog.Handler`: `Enabled()`
  returns `true` for all levels; `Handle()` appends each `slog.Record` to a
  slice (guarded by a mutex); `WithAttrs`/`WithGroup` return the receiver. Tests
  assert on `record.Level` and `record.Message` via helper methods on the
  handler (e.g., `hasRecord(level, msgSubstr)`).
- **`probeGPU()` WARN is no longer dead code**: the existing
  `m.logger.Warn("GPU status probe failed", "error", err)` at state.go:241 now
  fires for real `nvidia-smi` failures during `GET /status`. This is the correct
  level for potentially unexpected failures during status probes. No code change
  is needed in `probeGPU()` — only the `gpu.Status()` return-value change
  activates it.
- **`gpu_test.go` constructor tests**: existing tests construct `&Monitor{exec:
  ...}` directly (not via `New()`), so removing the `logger` field does not
  break them. The `New()` constructor test (if any) must be updated to call
  `New()` with no args.
