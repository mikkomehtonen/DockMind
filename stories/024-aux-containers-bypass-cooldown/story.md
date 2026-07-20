# Allow Aux Container Start/Stop During Power Cooldown

## Context

The power cooldown (`power.cooldown`, story 011) prevents rapid eGPU
power cycling: after a startup it blocks `POST /power/off` for the cooldown
duration, and after a shutdown it blocks `POST /power/on` similarly. This
protects the eGPU hardware from being powered on/off too quickly.

The web UI (`internal/api/index.html`, `render` function) currently applies
the cooldown gate to **aux container** buttons as well:

```js
const auxStartEnabled = (data.state === "Ready") && data.cooldownRemaining <= 0;
const auxStopEnabled = (data.state === "Off" || data.state === "Ready") && data.cooldownRemaining <= 0;
```

So after a startup completes (state `Ready`) with a post-startup cooldown
active, the user cannot click Start on an aux container (e.g. Kokoro TTS,
Whisper STT) until the cooldown elapses — even though the GPU is already on
and the container is ready to use. The user wants to start using aux
containers immediately after startup.

This coupling is unnecessary. Aux containers depend on the **GPU being
ready** (the `Ready` state), not on the power-cycling cadence. The state
machine backend already reflects this: `doAuxOperation` (state.go:388) checks
only the state (`Ready` for start, `Off`/`Ready` for stop) and never consults
the cooldown. The cooldown is purely a power-transition guard. The only
blocker is the web UI disabling the buttons.

After a power-off the cooldown blocks power-on, but aux start is already
rejected in the `Off` state by the GPU-ready gate (story 021), so removing
the cooldown gate from aux buttons has no effect there — the state gate is
the relevant one. Aux stop in `Off` during a post-shutdown cooldown remains
safe (stopping is always safe and is part of the shutdown sequence itself).

## Out of Scope

- Changing when aux containers can be started or stopped. Start remains
  `Ready`-only (story 021); stop remains `Off` + `Ready`. The state gate is
  the sole authority.
- Modifying the backend cooldown logic for power transitions
  (`PowerOn`/`PowerOff`/`Restart`). The 429 behavior and `cooldownRemaining`
  reporting are unchanged.
- Auto-starting aux containers during power-on (still out of scope per 018).
- Any new config field, route, `AuxResult` value, or HTTP status code.
- The cooldown banner in the web UI (it still shows when
  `cooldownRemaining > 0`, since it describes the power-transition cooldown,
  which is unaffected).

## Implementation approach

### Web UI (`internal/api/index.html`)

Remove the `&& data.cooldownRemaining <= 0` conjunct from both aux
enablement expressions in the `render(data)` aux block (around line 801).
The state gate stays; only the cooldown conjunct is dropped.

Before:

```js
const auxStartEnabled = (data.state === "Ready") && data.cooldownRemaining <= 0;
const auxStopEnabled = (data.state === "Off" || data.state === "Ready") && data.cooldownRemaining <= 0;
```

After:

```js
const auxStartEnabled = (data.state === "Ready");
const auxStopEnabled = (data.state === "Off" || data.state === "Ready");
```

The button `disabled` expressions (`${running || !auxStartEnabled ?
"disabled" : ""}` and `${!running || !auxStopEnabled ? "disabled" : ""}`)
are unchanged — they already derive solely from the two enablement flags.

No other part of `render` changes. The cooldown banner block
(`if (data.cooldownRemaining > 0) { ... }`, around line 834) and the power
button enablement it controls are untouched — the cooldown still disables
the power buttons as before.

### State machine (`internal/state/state.go`)

No code change. `doAuxOperation` already permits aux operations during an
active cooldown because it never reads `cooldownActiveLocked` or
`cooldownRemainingLocked`. This story adds a regression test (see Task 2)
to lock that behavior in so a future change cannot silently re-introduce
the coupling.

### API (`internal/api/api.go`) and OpenAPI spec (`internal/api/openapi.json`)

No change. `handleAuxResult` already maps `AuxResultOK` → 200,
`AuxResultConflict` → 409, etc. The aux container 409 descriptions mention
"power transition in progress or state does not allow aux operations" —
cooldown is not referenced there (cooldown 429s are only on the power
endpoints), so no description update is needed.

### Documentation (`README.md`)

Add one sentence to the "Optional Aux Containers" section (around line 117)
clarifying that aux start/stop is not blocked by the power cooldown — only
by the system state. This pre-empts user confusion now that the UI no
longer disables the buttons during cooldown. `readme_test.go` only asserts
presence of `auxContainers`, the route paths, and a loadable yaml block, so
a prose addition is safe.

### Product doc (`docs/product.md`) and `product_test.go`

Add a new Features entry for `024-aux-containers-bypass-cooldown` matching
the existing entry format. Add a `strings.Contains(body, "024-aux-
containers-bypass-cooldown")` assertion to `product_test.go` following the
existing pattern.

## Tasks

### Task 1 - Web UI Drops Cooldown Gate From Aux Buttons

- `GET /` response body (the embedded `index.html`)
  - → contains `const auxStartEnabled = (data.state === "Ready");` (no
    `cooldownRemaining` conjunct)
  - → contains `const auxStopEnabled = (data.state === "Off" || data.state === "Ready");` (no
    `cooldownRemaining` conjunct)
  - → still contains `${running || !auxStartEnabled ? "disabled" : ""}` and
    `${!running || !auxStopEnabled ? "disabled" : ""}` (button disabled
    expressions unchanged)
  - → still contains the cooldown banner text `Cooldown active` and
    `cooldownRemaining` (power-transition cooldown UI unchanged)
- `render({state: "Ready", cooldownRemaining: 5, auxContainers: [{name: "kokoro", running: false}]})`
  - → Start button is enabled (state Ready; cooldown no longer disables it)
- `render({state: "Ready", cooldownRemaining: 5, auxContainers: [{name: "kokoro", running: true}]})`
  - → Stop button is enabled (container running, state Ready; cooldown no
    longer disables it)
- `render({state: "Off", cooldownRemaining: 5, auxContainers: [{name: "kokoro", running: false}]})`
  - → Start button is disabled (state Off fails the Ready gate, not cooldown)
  - → Stop button is enabled (Off + Ready allowed; cooldown ignored)
- `render({state: "Starting", cooldownRemaining: 0, auxContainers: [{name: "kokoro", running: false}]})`
  - → both Start and Stop buttons are disabled (state gate, unchanged)

### Task 2 - Backend Aux Operations Unaffected by Cooldown (Regression)

- machine in `Ready` with an active post-startup cooldown (`lastReadyTime`
  set to now, `cooldown` > 0) + `StartAuxContainer("kokoro")`
  - → returns `AuxResultOK`
  - → aux controller's `Start` called with name "kokoro"
- machine in `Ready` with an active post-startup cooldown + `StopAuxContainer("kokoro")` (container running)
  - → returns `AuxResultOK`
  - → aux controller's `Stop` called with name "kokoro"
- machine in `Off` with an active post-shutdown cooldown (`lastOffTime` set
  to now, `cooldown` > 0) + `StopAuxContainer("kokoro")`
  - → returns `AuxResultOK`
  - → aux controller's `Stop` called with name "kokoro"
- machine in `Off` with an active post-shutdown cooldown + `StartAuxContainer("kokoro")`
  - → returns `AuxResultConflict` (state gate, not cooldown — confirms the
    rejection reason is the `Ready` requirement, not cooldown)
  - → aux controller's `Start` NOT called
- machine in `Ready` with cooldown disabled (`cooldown` = 0) + `StartAuxContainer("kokoro")`
  - → returns `AuxResultOK` (baseline, unchanged)

### Task 3 - Documentation and Build

- `README.md` "Optional Aux Containers" section states that aux start/stop
  is not blocked by the power cooldown (only by the system state)
  - → the section contains the word "cooldown" in a sentence clarifying aux
    operations are exempt from it
- `docs/product.md` Features list includes the
  `024-aux-containers-bypass-cooldown` story
  - → `product_test.go` passes (asserts presence of
    `024-aux-containers-bypass-cooldown`)
- `make build && make test && make lint` all pass

## Technical Context

- Go 1.24.4, module `github.com/dockmind/dockmind`. No new dependencies.
- The web UI tests in `api_test.go` are string-containment checks against
  the served `index.html` body (there is no JS execution harness). The ACs
  in Task 1 are therefore verified by asserting the exact
  `auxStartEnabled` / `auxStopEnabled` source lines and the unchanged
  button `disabled` expressions appear in `GET /`'s response body. This
  matches the established pattern in `TestWebUIAuxStartGatedOnReady`
  (api_test.go:602), which must be updated to the new expected strings.
- `doAuxOperation` (state.go:388) performs the name lookup, then acquires
  `transitionMu` with `TryLock()`, reads `m.state` under `stateMu`, and
  applies the state gate. It never calls `cooldownActiveLocked`. The
  regression test in Task 2 sets `m.state` and `m.lastReadyTime` /
  `m.lastOffTime` directly under `stateMu` (the same pattern used in
  `TestCooldown`, state_test.go:1506) to simulate an active cooldown
  without running a real async transition.
- The cooldown banner in `render` (index.html:834) reads
  `data.cooldownRemaining` and disables only the power buttons
  (`btnOn`/`btnOff`/`btnRestart`); it does not touch the aux buttons. This
  story does not modify that block.
- `readme_test.go` asserts presence of `auxContainers`, the two
  `/containers/` route paths, and that the first yaml block loads via
  `config.Load`. A prose sentence added to the Optional Aux Containers
  section does not affect any of these.
- `product_test.go` currently asserts `021-aux-containers-gpu-gate` and
  `023-gpu-utilization-display` are present; the new assertion for
  `024-aux-containers-bypass-cooldown` follows the same
  `strings.Contains` pattern.

## Notes

- The fix is UI-only for the user-facing behavior; the backend already
  permits aux operations during cooldown. The regression test (Task 2)
  guards against a future refactor accidentally adding a cooldown check to
  `doAuxOperation`.
- The state gate (story 021) remains the sole authority for when aux
  containers may be started (`Ready`) or stopped (`Off` + `Ready`). During
  the `Starting` state — the "startup procedure" the user explicitly
  excludes — aux start is still rejected with `AuxResultConflict` because
  the state is not yet `Ready`. This is unchanged and correct: the GPU is
  not confirmed available until `Ready` is entered.
- After a power-off, aux start is rejected in `Off` by the state gate, so
  dropping the cooldown conjunct from `auxStartEnabled` has no observable
  effect in that state — confirming the user's point that "when GPU is off
  it is not anyhow possible to use Aux Containers."
