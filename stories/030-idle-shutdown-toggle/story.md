# Idle Auto-Shutdown Toggle

## Context

DockMind's OpenAI gateway performs an idle auto-shutdown: when the system is `Ready`, the gateway is enabled, `gateway.idleTimeout > 0`, and no request has arrived for `idleTimeout`, the idle watcher (`internal/gateway/gateway.go` `tick()`) powers the whole stack down via `g.machine.PowerOff()`. This is always-on whenever a timeout is configured, and there is no runtime way to pause it short of editing config and restarting.

The user needs to temporarily suspend idle auto-shutdown — for example to hold the eGPU up across a long gap between requests without worrying the daemon will spin it down mid-session — while keeping manual power-off working exactly as before. The toggle must default to **enabled** so existing behavior is preserved, and must re-arm to enabled whenever the eGPU is started. The control lives in the existing web control panel and reflects live state.

## Out of Scope

- Persisting the toggle across DockMind process restarts or power cycles. It is in-memory only and defaults to enabled (see Implementation approach).
- Changing the idle timeout duration, the two-phase grace-period shutdown, or the per-container `disableIdleShutdown` (story 026) behavior. The global toggle composes with them; it does not replace them.
- Disabling gateway auto-start (`EnsureReady`) on first request. The toggle governs **shutdown only**; auto-start remains unaffected.
- A dedicated settings/prefs page, authentication, or any change to config-file parsing (`gateway.idleTimeout` stays the enable/threshold gate).
- Background polling or push updates; the UI keeps polling `/status` once per second.

## Implementation approach

The toggle is owned by the gateway, because idle auto-shutdown is a gateway concern. The state machine, `PowerOff`, `EnsureReady`, and the HTTP power endpoints are not touched.

**Gateway state (`internal/gateway/gateway.go`):**
- Add `idleShutdownEnabled atomic.Bool` to `Gateway`, initialized to `true` in both constructors (`NewGateway`, `NewGatewayWithPollInterval`) so the default is enabled at startup.
- Add methods:
  - `IdleShutdownEnabled() bool` — returns `idleShutdownEnabled.Load()`.
  - `IdleShutdownAvailable() bool` — returns `idleTimeout > 0` (the toggle is only meaningful when the gateway exists, which already implies `gateway.enabled: true`).
  - `SetIdleShutdownEnabled(enabled bool) bool` — if `idleTimeout <= 0` return `false` (not applicable, reject); otherwise `idleShutdownEnabled.Store(enabled)` and, when re-enabling (false → true), reset `lastActivity = time.Now()` under `activeMu` so the countdown restarts fresh, then return `true`.
- **Gate `tick()` on the toggle:** at the point where Phase 1 sets `pendingShutdown = true` when `idle >= idleTimeout`, additionally require `idleShutdownEnabled.Load()`. While disabled, reset `lastActivity = time.Now()` each tick (mirroring the existing aux-block handling that resets `lastActivity`) so idle time does not accrue and re-enabling restarts the countdown from zero. The gate is ONLY on the idle-triggered shutdown path; it must not touch the manual `PowerOff` path.
- **Re-arm on eGPU start:** in the existing `Ready`-transition edge block (the branch that resets `lastActivity` and clears `pendingShutdown`/`idleBlocked` on entering `Ready`), also `idleShutdownEnabled.Store(true)`. This satisfies "enabled by default when eGPU is started" — each Off→Ready startup (manual power-on or gateway `EnsureReady` auto-start) re-enables it. The constructor default covers a cold start that reconciles directly to `Ready`.
- `IdleRemaining()` must return `0` when `!idleShutdownEnabled.Load()` (add this alongside the existing `idleTimeout <= 0`, `idleBlocked`, state≠Ready, and `active > 0 || pendingShutdown` guards) so no countdown is reported while suspended.

**API seam (`internal/api/api.go`):**
- Add a narrow interface so handler tests keep using a fake and `state` stays decoupled:
  ```go
  type IdleShutdownController interface {
      IdleShutdownEnabled() bool
      IdleShutdownAvailable() bool
      SetIdleShutdownEnabled(enabled bool) bool
  }
  ```
- Add `idleShutdownCtl IdleShutdownController` field + `SetIdleShutdownController(c IdleShutdownController)` setter, wired to the gateway in `cmd/dockmind/main.go` inside the `if cfg.Gateway.Enabled` block, next to the existing `SetIdleReporter(gw)` call. `*gateway.Gateway` satisfies the interface structurally.
- `StatusResponse` (`internal/state/state.go`) gains two always-present boolean fields (matching existing top-level bools, no `omitempty`): `IdleShutdownEnabled bool json:"idleShutdownEnabled"` and `IdleShutdownAvailable bool json:"idleShutdownAvailable"`. In `handleStatus`, set both from `idleShutdownCtl` when it is non-nil; when nil (gateway disabled) they stay `false`, which the UI renders as "not applicable."
- Add two action-style idempotent routes in `Server.Handler()`, mirroring `/power/on`·`/power/off` and returning empty bodies:
  - `POST /idle-shutdown/enable` → `handleEnableIdleShutdown`
  - `POST /idle-shutdown/disable` → `handleDisableIdleShutdown`
  - Handler contract: `idleShutdownCtl == nil` → `409`; else call `SetIdleShutdownEnabled(true|false)`; result `false` (not available) → `409`; result `true` → `200` (idempotent — already in target state still returns 200).

**Web UI (`internal/api/index.html`, single embedded vanilla-JS file):**
- Add a toggle control (checkbox/switch) labeled "Automatic idle shutdown" in the status/idle area. It is **always rendered**.
- Checked state binds to `data.idleShutdownEnabled`; the control is `disabled` when `data.idleShutdownAvailable === false`. When disabled, show a short hint (e.g. "requires gateway with an idle timeout").
- Clicking POSTs to `/idle-shutdown/enable` or `/idle-shutdown/disable` for the **target** state, then force-refreshes via `setTimeout(fetchStatus, 1000)` exactly like `doAuxAction`. No optimistic DOM mutation; the next poll reflects server truth. Feedback message maps 200 → applied, 409 → "not available."
- The existing "Auto-shutdown in X" countdown already hides when `idleRemaining` is 0, so disabling the toggle (which forces `idleRemaining` to 0) automatically suppresses the countdown.

**Docs:** append a concise paragraph to the "Idle Shutdown" section of `docs/DockMind_Gateway_Design.md` documenting the toggle; the existing required substrings (`idle shutdown`, `idletimeout`, `pendingshutdown`, `grace period`, `idle timer initialization`, `30m`, etc.) must remain present so `gateway_design_test.go` stays green. Add a feature bullet to `docs/product.md`.

## Tasks

### Task 1 - Gateway idle-shutdown toggle state and watcher gating

- gateway constructed (any idleTimeout) + read default
  - → `IdleShutdownEnabled()` returns `true`
  - → `IdleShutdownAvailable()` returns `true` when `idleTimeout > 0`, `false` when `idleTimeout == 0`
- gateway with `idleTimeout > 0` + system `Ready`, request idled past `idleTimeout`, toggle left at default
  - → idle watcher calls `machine.PowerOff()` (existing behavior preserved)
- gateway with `idleTimeout > 0` + `SetIdleShutdownEnabled(false)` returns `true` + system idles past `idleTimeout`
  - → `machine.PowerOff()` is NOT called
  - → `IdleRemaining()` returns `0`
- `SetIdleShutdownEnabled(true)` while disabled and idling + no new request
  - → returns `true`
  - → `IdleRemaining()` restarts from near `idleTimeout` (countdown reset, not resumed from the old value)
  - → after a full `idleTimeout` of idleness, `machine.PowerOff()` is called
- gateway with `idleTimeout == 0` + `SetIdleShutdownEnabled(true)`
  - → returns `false` (not applicable)
- machine enters `Ready` after the toggle was disabled (Off→Ready edge)
  - → `IdleShutdownEnabled()` returns `true` (re-armed when eGPU is started)
- toggle disabled + gateway receives an inference request
  - → `machine.EnsureReady()` still invoked / auto-start unaffected (toggle governs shutdown only)

### Task 2 - HTTP status fields, enable/disable endpoints, and spec

- gateway present (controller wired) + `GET /status`
  - → response JSON contains `idleShutdownEnabled` (bool) and `idleShutdownAvailable` (bool) reflecting the controller
- no gateway (controller not wired) + `GET /status`
  - → `idleShutdownEnabled` is `false` and `idleShutdownAvailable` is `false`
- controller available + `POST /idle-shutdown/enable`
  - → `200` with empty body, and a subsequent `GET /status` reports `idleShutdownEnabled: true`
- controller available + `POST /idle-shutdown/disable`
  - → `200` with empty body, and a subsequent `GET /status` reports `idleShutdownEnabled: false`
- controller available + `POST /idle-shutdown/enable` when already enabled (and vice-versa for disable)
  - → `200` (idempotent no-op)
- controller not available (`idleTimeout == 0`, `SetIdleShutdownEnabled` returns `false`) + `POST /idle-shutdown/enable` or `/disable`
  - → `409` with empty body
- controller not wired (`idleShutdownCtl == nil`, gateway disabled) + `POST /idle-shutdown/enable` or `/disable`
  - → `409`
- manual `POST /power/off` while `idleShutdownEnabled` is `false`
  - → still returns `202` (manual shutdown unaffected by the toggle)
- `openapi.json` + the two list-based contract tests
  - → both `/idle-shutdown/enable` and `/idle-shutdown/disable` appear in `openapi.json` `paths` and are appended to the route-list assertion (`api_test.go:411`)
  - → `idleShutdownEnabled` and `idleShutdownAvailable` appear in `StatusResponse` `properties` and are appended to the field-list assertion (`api_test.go:433`)
- `IdleReporter`/`IdleShutdownController` fake in `internal/api/api_test.go`
  - → `fakeIdleReporter` (and any new controller fake) implements all interface methods so the package compiles

### Task 3 - Web UI control and main wiring

Add a substring contract test over the embedded `internal/api/index.html` (matching the existing `readme_test.go` / `gateway_design_test.go` file-substring convention) plus the `main.go` wiring:

- `index.html` served by the embedded handler
  - → contains a control bound to `idleShutdownEnabled` (checked state)
  - → contains a disable condition keyed on `idleShutdownAvailable`
  - → issues `fetch` calls to both `/idle-shutdown/enable` and `/idle-shutdown/disable`
- `cmd/dockmind/main.go` gateway-enabled path
  - → contains a `SetIdleShutdownController(gw)` wiring call (same gate as `SetIdleReporter`)
- `cmd/dockmind/main.go` gateway-disabled path
  - → no `SetIdleShutdownController` call executes, so `GET /status` reports `idleShutdownAvailable: false` (asserted in Task 2)

### Task 4 - Documentation

- `docs/DockMind_Gateway_Design.md` "Idle Shutdown" section updated to describe the runtime toggle
  - → `make test` green, including `gateway_design_test.go` (all previously required doc substrings still present)
- `docs/product.md` Features list gains an "Idle Auto-Shutdown Toggle" bullet
  - → `readme_test.go` still green (no forbidden strings; required strings intact)

## Technical Context

- Toolchain Go 1.24.4, module `github.com/dockmind/dockmind`. **No new dependencies** — stdlib only (`sync/atomic`, `net/http`, `time`), consistent with the project (single external dep is `gopkg.in/yaml.v3`).
- Commands: `make build`, `make test`, `make lint` (`gofmt -l . && go vet ./...` — both must pass).
- `idleRemaining` is sourced from the gateway via the `IdleReporter` seam (`internal/api/api.go`), merged in `handleStatus` (`api.go:109-112`); the new `IdleShutdownController` follows the same wiring pattern in `cmd/dockmind/main.go` (`main.go:103`).
- `gateway_design_test.go` and `readme_test.go` (repo root) validate documentation via case-insensitive substring checks; treat doc edits as test-enforced and preserve existing required substrings.
- Existing gateway test helpers to reuse: `fakeController` (`gateway_test.go:22`), `NewGatewayWithPollInterval`, and the `TestIdleShutdown_PreventedWhenBlocked` / `TestIdleShutdown_ResumeWhenUnblocked` structure (`gateway_test.go` ~2193-2274). Table-driven stdlib tests only; no mocking libraries.
- `state.Machine` and the `api.StateMachine` interface are unchanged; the machine does not know about the toggle.

## Notes

- **Default and lifecycle:** the toggle is enabled by default, has no config entry, and is never persisted to disk. It resets to enabled on every Off→Ready transition and at process startup. Disabling it is intentionally ephemeral — the next eGPU power-on re-arms it. This is a deliberate product decision (no persistence), not an oversight.
- **Composes with story 026:** the per-container `disableIdleShutdown` flag still pauses the countdown independently. Global toggle **off** means no idle shutdown at all regardless of aux containers; global toggle **on** does not bypass an active aux-container block (`idleShutdownBlocked` still `true`).
- **Manual shutdown is a hard requirement to preserve:** the gate lives only on the gateway's idle `tick()` path. `POST /power/off`, `POST /power/off` from `Error`, `POST /restart`, and gateway auto-start (`EnsureReady`) must behave exactly as today while the toggle is off.
- **UI verification:** the project has no browser/JS execution harness (vanilla embedded HTML, stdlib only). The automatable surface is the `index.html` substring contract (Task 3), the `GET /status` fields, and the enable/disable endpoint status codes (Task 2). The following behaviors are verified manually by hand: the toggle renders on every poll with its checked state reflecting `idleShutdownEnabled`, is greyed out with a hint when `idleShutdownAvailable` is false, and flipping it issues a single `POST` to the target endpoint followed by a forced `fetchStatus` (pattern parity with `doAuxAction`) — no optimistic DOM mutation; the next 1s poll reflects server truth.
- UI must never present the toggle as silently non-functional: when `idleShutdownAvailable` is false it is visibly disabled with a hint, per the product decision "always visible but disabled when not applicable."
