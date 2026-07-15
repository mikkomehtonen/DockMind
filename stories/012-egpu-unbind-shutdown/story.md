# eGPU Driver Unbind Before Power-Off

## Context

Cutting Shelly power immediately after `llama-swap` stops leaves the NVIDIA
driver bound to a GPU that has just lost power. This sends the driver into an
infinite `dmesg` error loop (`nvidia-modeset: ERROR: GPU:0: Error while waiting
for GPU progress`) and can hang the host on reboot/shutdown.

The fix is to unbind the NVIDIA driver from the eGPU **before** switching off
the Shelly plug. A systemd oneshot service — `dockmind-egpu-unbind.service`
(already committed to the repo at `dockmind-egpu-unbind.service` +
`dockmind-egpu-unbind` helper script) — performs the unbind safely. DockMind
must invoke it via `sudo -n /usr/bin/systemctl start
dockmind-egpu-unbind.service` after `llama-swap` has stopped but before
`SetPower(false)`. If the unbind command fails (non-zero exit), the shutdown
must be aborted — the machine enters the Error state and Shelly power is **not**
cut. Sudoers access is pre-configured on the target host; a failure means the
access is missing or the service is broken, and cutting power anyway would
reproduce the driver hang.

## Out of Scope

- **Configurability of the unbind step.** The unbind is always-on and the
  service name is hardcoded. There is no config flag to disable it. Dev/test
  hosts without the service will see shutdown fail to Error — acceptable because
  the state-machine tests use fakes that never shell out.
- **Rebinding the driver on startup.** The NVIDIA driver rebinds automatically
  when the GPU reappears on the PCI bus after power-on. No explicit rebind
  command is needed.
- **Modifying the MVP specification document.**
  `docs/DockMind_MVP_Specification.md` is a historical artifact; no test
  enforces its content beyond the README link check.
- **New external dependencies.** `go.mod` remains
  `gopkg.in/yaml.v3 v3.0.1` only. The new `internal/unbind` package uses only
  stdlib (`context`, `os/exec`).
- **Changes to the `dockmind-egpu-unbind` helper script or service file.**
  Those are pre-existing infrastructure (commit `0204e6e`) and are not modified
  by this story.

## Implementation approach

### New interface in `internal/state`

Add an `Unbinder` interface alongside the existing four
(`PowerController`, `GPUMonitor`, `ContainerController`, `HealthChecker`):

```go
type Unbinder interface {
    Unbind(ctx context.Context) error
}
```

Add `unbinder Unbinder` as a field on `Machine`. Add `unbinder Unbinder` as a
new parameter to `New`, inserted after `health HealthChecker` and before
`logger *slog.Logger` (grouping all interface dependencies together):

```go
func New(power PowerController, gpu GPUMonitor, docker ContainerController,
    health HealthChecker, unbinder Unbinder, logger *slog.Logger,
    pollInterval, startupTimeout, shutdownTimeout, cooldown time.Duration) *Machine
```

### New package `internal/unbind`

Follows the exact `execFunc` injection pattern used by `internal/docker` and
`internal/gpu` so tests never shell out:

```go
package unbind

import (
    "context"
    "os/exec"
)

type execFunc func(ctx context.Context, name string, args ...string) ([]byte, error)

type Client struct {
    exec execFunc
}

func New() *Client {
    return &Client{
        exec: func(ctx context.Context, name string, args ...string) ([]byte, error) {
            cmd := exec.CommandContext(ctx, name, args...)
            return cmd.Output()
        },
    }
}

func (c *Client) Unbind(ctx context.Context) error {
    _, err := c.exec(ctx, "sudo", "-n", "/usr/bin/systemctl", "start", "dockmind-egpu-unbind.service")
    return err
}
```

The command and service name are hardcoded — no config. `sudo -n` runs
non-interactively (no password prompt); sudoers is pre-configured on the target
host. A non-zero exit (service missing, sudoers not configured, unbind script
failure) surfaces as an `*exec.ExitError` which `Unbind` returns as-is.

### Shutdown sequence change (`internal/state/state.go`)

Insert the unbind step into `shutdown()` **after** the container-exit poll
succeeds and **before** `SetPower(false)`. On failure, set Error and return
immediately — Shelly power is never cut:

```go
// After the "Waiting for llama-swap to stop" poll block succeeds:

m.logger.Info("Unbinding eGPU drivers")
if err := m.unbinder.Unbind(ctx); err != nil {
    m.setState(Error, fmt.Errorf("egpu unbind failed: %w", err))
    m.logger.Error("eGPU unbind failed", "error", err)
    return
}

m.logger.Info("Shelly power OFF")
// ... existing SetPower(false) code unchanged ...
```

The full new shutdown sequence is:

1. `setState(ShuttingDown, nil)`
2. `docker.Stop(ctx)` — on failure → Error, return
3. Poll `docker.IsRunning` until false — on timeout → Error, return
4. **`unbinder.Unbind(ctx)` — on failure → Error, return** (NEW)
5. `power.SetPower(ctx, false)` — on failure → Error, return
6. Poll `gpu.Status` until GPU disappears — on timeout → Error, return
7. `setState(Off, nil)`

Because `restart()` calls `shutdown()` then `startup()`, restart automatically
includes the unbind step. Because `PowerOff()` from the Error state calls
`shutdown()`, error-recovery shutdown also unbinds — correct, since the GPU may
still be bound after a failed startup.

### main.go wiring (`cmd/dockmind/main.go`)

Construct the unbind client and pass it to `state.New`:

```go
unbindClient := unbind.New()

machine := state.New(
    power, gpuMonitor, dockerClient, healthClient, unbindClient, logger,
    cfg.GPU.PollInterval.Duration(),
    cfg.Startup.Timeout.Duration(),
    cfg.Shutdown.Timeout.Duration(),
    cfg.Power.Cooldown.Duration(),
)
```

Add `"github.com/dockmind/dockmind/internal/unbind"` to the import block.

### Test helpers (`internal/state/state_test.go`)

Add a `fakeUnbinder` type:

```go
type fakeUnbinder struct {
    mu        sync.Mutex
    unbindErr error
    calls     int
}

func (f *fakeUnbinder) Unbind(ctx context.Context) error {
    f.mu.Lock()
    defer f.mu.Unlock()
    f.calls++
    return f.unbindErr
}
```

All three test helpers (`newTestMachine`, `newTestMachineWithCooldown`,
`newTestMachineWithRecorder`) create a `fakeUnbinder` (default: no error) and
pass it to `New`. Append `*fakeUnbinder` as the last return value of each
helper. Update every call site — tests that do not configure the unbinder use
`_` for the last return value.

### Polish

- **`README.md`**: add a brief note under the `/power/off` row or in the
  Configuration section explaining that DockMind unbinds the NVIDIA driver via
  `dockmind-egpu-unbind.service` before cutting Shelly power, and that a failed
  unbind aborts the shutdown. Do not add a new fenced yaml block (the
  `readme_test.go` yaml test uses the first fenced block).
- **`AGENTS.md`**: add a note in the "API / state-machine conventions" section
  that the shutdown sequence unbinds the eGPU driver before Shelly power-off,
  and that unbind failure aborts the shutdown to Error.
- **`docs/product.md`**: add a Features entry for `012-egpu-unbind-shutdown`.
- **`product_test.go`**: add an assertion that `docs/product.md` contains
  `012-egpu-unbind-shutdown`.

## Tasks

### Task 1 - unbind package: Client with execFunc injection

- `Client` with default `execFunc` + `Unbind(ctx)` called
  - → executes `sudo -n /usr/bin/systemctl start dockmind-egpu-unbind.service`
    (verify via injected `execFunc` that `gotArgs` equals
    `[]string{"sudo", "-n", "/usr/bin/systemctl", "start", "dockmind-egpu-unbind.service"}`)
- `Client` with injected `execFunc` returning `nil` error + `Unbind(ctx)`
  - → returns `nil`
- `Client` with injected `execFunc` returning a generic `errors.New(...)` + `Unbind(ctx)`
  - → returns a non-nil error
- `Client` with injected `execFunc` returning `*exec.ExitError` (non-zero exit) + `Unbind(ctx)`
  - → returns a non-nil error (the `*exec.ExitError` itself)
- `New()` constructor
  - → returns a non-nil `*Client`
  - → `Client.exec` is non-nil (default execFunc is set)
- `go test -race ./internal/unbind/`
  - → no data race detected

### Task 2 - State machine: unbind step in shutdown

- Machine in Ready + `PowerOff()` + `fakeUnbinder` succeeds (default)
  - → returns `ResultAccepted`
  - → after `Wait()`, state is `Off`
  - → `fakeUnbinder.calls` equals 1 (unbind called exactly once)
  - → `power.on` is false (Shelly was turned off after unbind succeeded)
- Machine in Ready + `PowerOff()` + `fakeUnbinder.unbindErr = errors.New("sudo failed")`
  - → returns `ResultAccepted`
  - → after `Wait()`, state is `Error`
  - → `lastError` contains `"unbind"` (case-insensitive)
  - → `power.on` is true (Shelly power was NOT cut — shutdown aborted)
  - → `fakeUnbinder.calls` equals 1 (unbind was called, then failed)
- Machine in Ready + `docker.stopErr = errors.New("stop failed")` + `PowerOff()`
  - → after `Wait()`, state is `Error`
  - → `fakeUnbinder.calls` equals 0 (unbind NOT called — docker stop failed before reaching the unbind step)
- Machine in Ready + `Restart()` + `fakeUnbinder` succeeds
  - → after `Wait()`, state is `Ready`
  - → `fakeUnbinder.calls` equals 1 (unbind called during the shutdown phase of restart)
- Machine in Error + `power.on = true` + `PowerOff()` + `fakeUnbinder` succeeds
  - → after `Wait()`, state is `Off`
  - → `fakeUnbinder.calls` equals 1 (unbind called during error-recovery shutdown)
- existing `state_test.go` tests + `make test`
  - → all existing tests pass unchanged (helpers now pass a succeeding `fakeUnbinder`; existing shutdown/restart tests reach the same final states because unbind succeeds by default)
- `go test -race ./internal/state/`
  - → no data race detected

### Task 3 - main.go wiring and full build

- `make build` from repo root
  - → produces `./dockmind` binary without error
- `make test` from repo root
  - → all tests pass (unbind, state, config, gateway, api, readme, product, gateway_design)
- `make lint` from repo root
  - → `gofmt -l .` prints no file paths
  - → `go vet ./...` reports no issues

### Task 4 - Polish: README, AGENTS.md, product.md, test assertions

- `docs/product.md` Features list references `012-egpu-unbind-shutdown` + `make test`
  - → `product_test.go` passes with `012-egpu-unbind-shutdown` assertion
- `docs/product.md` existing assertions still satisfied + `make test`
  - → `product_test.go` existing assertions pass (`004-web-ui`, `006-add-favicon-logo`, `007-openai-gateway`, `008-openai-gateway`, `010-cache-models-json`, `011-cooldown-protection` still present; non-goal check still passes)
- README contains the string `unbind` + `make test`
  - → `readme_test.go` passes (no existing assertion checks for absence of `unbind`; the first fenced yaml block is unchanged so the yaml-load test still passes)
- README existing assertions still satisfied + `make test`
  - → all existing `readme_test.go` cases pass (all routes, field names, commands, doc links, no `ResultAlreadyInState`/`ResultConflict`, no License section, first yaml block loads via `config.Load`)
- `make lint` from repo root
  - → `gofmt -l .` prints no file paths
  - → `go vet ./...` reports no issues
- `make build` from repo root
  - → produces `./dockmind` binary without error

## Bootstrap

No new dependencies. The project uses Go 1.24.4 with `gopkg.in/yaml.v3 v3.0.1`
only. The new `internal/unbind` package uses only stdlib (`context`, `os/exec`).

```bash
make build      # go build -o dockmind ./cmd/dockmind
make test       # go test ./...
make lint       # gofmt -l . && go vet ./...
```

## Technical Context

- **Go 1.24.4** — `go.mod` toolchain. `exec.CommandContext` and
  `cmd.Output()` are stable stdlib APIs used identically by `internal/docker`
  and `internal/gpu`.
- **`execFunc` injection pattern** — `internal/docker` and `internal/gpu` both
  define an unexported `execFunc` type and inject it via a struct field set in
  `New()`. Tests construct the struct literal directly with a fake `execFunc`.
  The new `internal/unbind` package follows this exact pattern so `sudo` and
  `systemctl` are never invoked during tests.
- **`sudo -n` (non-interactive)** — the `-n` flag prevents sudo from prompting
  for a password; if sudoers is not configured, sudo exits non-zero immediately
  instead of hanging. This is critical because `shutdown()` runs in a goroutine
  with a `shutdownTimeout` context — a password prompt would block until the
  context deadline, then abort.
- **`/usr/bin/systemctl`** — fully qualified path per the user's specification.
  Using the absolute path avoids `PATH` lookup issues in the daemon's
  environment.
- **`dockmind-egpu-unbind.service`** — a systemd `Type=oneshot` service
  (already in the repo at `dockmind-egpu-unbind.service`). `systemctl start`
  blocks until the oneshot completes, then returns the service's exit code.
  Exit 0 means the GPU is safely unbound and power can be cut.
- **`New` signature change** — adding `unbinder Unbinder` as the 5th parameter
  (after `health`, before `logger`) requires updating the single production call
  site (`cmd/dockmind/main.go`) and the three test helpers
  (`newTestMachine`, `newTestMachineWithCooldown`, `newTestMachineWithRecorder`).
  No other call sites exist.
- **No new external dependencies** — `go.mod` remains
  `gopkg.in/yaml.v3 v3.0.1` only. `go.sum` unchanged.
- **Existing test conventions** — stdlib `testing`, hand-written fakes,
  table-driven tests, no testify/mocking libraries (per `AGENTS.md`). The
  `fakeUnbinder` follows the same mutex-guarded pattern as `fakeDocker` and
  `fakeHealth`.

## Notes

- **Unbind is always-on and hardcoded.** The user explicitly chose no config
  option and a hardcoded service name. Every shutdown (including restart's
  shutdown phase and Error-recovery shutdown) calls the unbind service. Dev/test
  hosts do not have the service, but all tests use fakes — no real `sudo` or
  `systemctl` invocation occurs during `make test`.
- **Unbind failure aborts shutdown.** If `systemctl start
  dockmind-egpu-unbind.service` returns non-zero, the machine enters Error and
  Shelly power stays on. This prevents the NVIDIA driver hang that occurs when
  power is cut while the driver is still bound. The user can then investigate
  (check sudoers, service status) and retry via `POST /power/off` from Error.
- **Unbind ordering is after container exit, before Shelly off.** This ensures
  `llama-swap` is no longer using the GPU when the driver is unbound, and the
  driver is unbound before power is cut. The ordering is verified by two
  complementary tests: (1) docker-stop failure → unbind not called (proves
  unbind is after docker stop); (2) unbind failure → Shelly stays on (proves
  unbind is before Shelly off).
- **No rebinding step on startup.** The NVIDIA driver automatically rebinds to
  the GPU when it reappears on the PCI bus after Shelly power-on. The existing
  startup sequence (Shelly ON → wait for GPU via `nvidia-smi`) already handles
  this — `nvidia-smi` only succeeds once the driver has rebound.
- **Pre-existing infrastructure.** The `dockmind-egpu-unbind` helper script and
  `dockmind-egpu-unbind.service` unit file were committed in `0204e6e` and are
  installed via `install-unbind-service.sh`. This story only adds the DockMind
  daemon code that invokes the service; it does not modify the service files.
