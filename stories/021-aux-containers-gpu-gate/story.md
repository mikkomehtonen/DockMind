# Gate Aux Container Start on GPU Ready

## Context

Aux containers (story 018) are optional Docker containers that depend on the
eGPU — e.g. a Kokoro TTS or Whisper STT container that runs inference on the
GPU. Today `StartAuxContainer` is accepted in both the `Off` and `Ready`
states (state.go `doAuxOperation`, line 414: `if current != Off && current !=
Ready`). Starting such a container while the system is `Off` makes no sense:
the Shelly plug is off, the eGPU is not detected, and the container cannot
function. It only wastes a Docker start and leaves the user with a broken
container they have to clean up manually.

The `Ready` state is the system's source of truth for "GPU detected and
inference backend healthy" — it is entered solely after `m.gpu.Status()`
confirms the GPU is present during startup (state.go:693, 722). Therefore the
correct gate is the state machine's state: aux containers may be **started**
only when the state is `Ready`. **Stopping** an aux container remains allowed
in both `Off` and `Ready`, since stopping is always safe and is in fact part
of the shutdown sequence itself.

This is a behavior change to an existing feature, not a new endpoint or config
field. No new `AuxResult` code is introduced — the rejected start reuses
`AuxResultConflict` (HTTP 409), which already means "state does not allow aux
operations" and is already what the web UI surfaces as "Not allowed right now".

## Out of Scope

- Live GPU probing inside `StartAuxContainer`. The state machine's `Ready`
  state is the gate; a container started in `Ready` that later finds the GPU
  gone is the user's problem (same as today for `llama-swap`).
- Changing the stop path. `StopAuxContainer` keeps the `Off` + `Ready`
  acceptance so users can clean up a stray aux container while the system is
  off, and so the shutdown sequence's `StopAll` is unaffected.
- Auto-starting aux containers during power-on (still out of scope per 018).
- Any new config field, route, or `AuxResult` value.

## Implementation approach

### State machine (`internal/state/state.go`)

Split the state check in `doAuxOperation` by operation type instead of the
single `current != Off && current != Ready` guard:

- For **start** (`start == true`): require `current == Ready`. Any other state
  (including `Off`) → `AuxResultConflict`.
- For **stop** (`start == false`): keep `current == Off || current == Ready`.
  Any other state → `AuxResultConflict`.

The check stays after the `transitionMu.TryLock()` acquisition and the
`stateMu` read of `m.state`, in the same position as today (state.go:410-416).
No other part of `doAuxOperation` changes — the name lookup, timeout, docker
call, and error logging are untouched.

Concretely, replace:

```go
if current != Off && current != Ready {
    return AuxResultConflict
}
```

with:

```go
if start {
    if current != Ready {
        return AuxResultConflict
    }
} else {
    if current != Off && current != Ready {
        return AuxResultConflict
    }
}
```

### API (`internal/api/api.go`)

No code change. `handleStartAuxContainer` / `handleStopAuxContainer` already
forward the `state.AuxResult` to `handleAuxResult`, which already maps
`AuxResultConflict` → 409. The HTTP contract is unchanged.

### OpenAPI spec (`internal/api/openapi.json`)

Update only the **description** of the 409 response on
`/containers/{name}/start` to reflect the new gate. The stop path's 409
description stays as-is. No status codes are added or removed.

Before:
```json
"409": {
  "description": "Conflict — power transition in progress or state does not allow aux operations"
}
```

After (start path only):
```json
"409": {
  "description": "Conflict — power transition in progress, or system not in the Ready state (aux containers require the GPU to be detected and the inference backend healthy)"
}
```

### Web UI (`internal/api/index.html`)

Two changes in the `render(data)` aux block (around line 756):

1. **Start button enablement**: today `auxEnabled` is
   `(data.state === "Off" || data.state === "Ready") && data.cooldownRemaining <= 0`
   and is applied to both Start and Stop. Split it:
   - `auxStartEnabled = (data.state === "Ready") && data.cooldownRemaining <= 0`
   - `auxStopEnabled = (data.state === "Off" || data.state === "Ready") && data.cooldownRemaining <= 0`
   - Start button: `disabled` when `running || !auxStartEnabled`.
   - Stop button: `disabled` when `!running || !auxStopEnabled` (unchanged
     behavior, just renamed variable).

2. **Feedback message for a rejected start**: in `doAuxAction(name, action)`
   (around line 868), the 409 branch currently shows "Not allowed right now".
   Keep that for the stop action, but for the **start** action show a more
   specific message so the user understands why: "GPU not ready — power on
   first". The 409 status code is the same for both; branch on the `action`
   argument.

No new DOM elements, no new CSS, no new fetch logic.

### Documentation (`README.md`)

Update the "Optional Aux Containers" section (around line 110) to state that
aux containers can be **started only when the system is Ready** (GPU detected
and inference backend healthy) and **stopped in the Off or Ready state**. Keep
the existing config example and `auxContainers: []` status note unchanged —
`readme_test.go` only checks for the presence of `auxContainers`, the route
paths, and a loadable yaml block, so a prose edit in this section is safe.

### Product doc (`docs/product.md`)

Update the existing 018 Features entry to mention the GPU-ready gate on
start. Add a new Features entry for this story (021) referencing the behavior
change. `product_test.go` currently only asserts the presence of
`018-optional-containers`; add an assertion for `021-aux-containers-gpu-gate`.

## Tasks

### Task 1 - State Machine Gates Start on Ready

- machine with aux controller, state `Ready` + `StartAuxContainer("kokoro")`
  - → returns `AuxResultOK`
  - → aux controller's `Start` called with name "kokoro"
- machine with aux controller, state `Off` + `StartAuxContainer("kokoro")`
  - → returns `AuxResultConflict`
  - → aux controller's `Start` NOT called
- machine with aux controller, state `Starting` + `StartAuxContainer("kokoro")`
  - → returns `AuxResultConflict`
  - → aux controller's `Start` NOT called
- machine with aux controller, state `ShuttingDown` + `StartAuxContainer("kokoro")`
  - → returns `AuxResultConflict`
- machine with aux controller, state `Error` + `StartAuxContainer("kokoro")`
  - → returns `AuxResultConflict`
- machine with aux controller, state `AwaitingGPUFree` + `StartAuxContainer("kokoro")`
  - → returns `AuxResultConflict`
- machine with aux controller + `StartAuxContainer("unknown")` in state `Ready`
  - → returns `AuxResultNotFound` (name check happens before state check is
    reached only when name is valid; unknown name still returns NotFound
    regardless of state, matching existing order in `doAuxOperation`)
- machine with aux controller + `StartAuxContainer("unknown")` in state `Off`
  - → returns `AuxResultNotFound` (name lookup precedes the state gate)
- machine with aux controller, state `Ready`, aux `Start` returns error + `StartAuxContainer("kokoro")`
  - → returns `AuxResultError`
- machine with nil aux controller + `StartAuxContainer("kokoro")` in state `Ready`
  - → returns `AuxResultNotFound`
- machine with aux controller, state `Ready`, transitionMu locked + `StartAuxContainer("kokoro")`
  - → returns `AuxResultConflict`

### Task 2 - State Machine Stop Unchanged (Off + Ready)

- machine with aux controller, state `Ready` + `StopAuxContainer("whisper")`
  - → returns `AuxResultOK`
  - → aux controller's `Stop` called with name "whisper"
- machine with aux controller, state `Off` + `StopAuxContainer("kokoro")`
  - → returns `AuxResultOK`
  - → aux controller's `Stop` called with name "kokoro"
- machine with aux controller, state `Starting` + `StopAuxContainer("kokoro")`
  - → returns `AuxResultConflict`
  - → aux controller's `Stop` NOT called
- machine with aux controller, state `ShuttingDown` + `StopAuxContainer("kokoro")`
  - → returns `AuxResultConflict`
- machine with aux controller, state `Error` + `StopAuxContainer("kokoro")`
  - → returns `AuxResultConflict`
- machine with aux controller, state `AwaitingGPUFree` + `StopAuxContainer("kokoro")`
  - → returns `AuxResultConflict`
- machine with aux controller + `StopAuxContainer("unknown")` in state `Ready`
  - → returns `AuxResultNotFound`

### Task 3 - API Routes Unchanged (Regression)

- `POST /containers/kokoro/start`, fake returns `AuxResultOK`
  - → HTTP 200, empty body
- `POST /containers/kokoro/start`, fake returns `AuxResultConflict`
  - → HTTP 409, empty body
- `POST /containers/kokoro/stop`, fake returns `AuxResultConflict`
  - → HTTP 409, empty body
- `POST /containers/kokoro/start` passes name "kokoro" to `StartAuxContainer`
  - → fake receives "kokoro"

### Task 4 - OpenAPI Spec Description Update

- `GET /openapi.json` response
  - → `/containers/{name}/start` 409 response description mentions "Ready"
    (e.g. contains the word "Ready")
  - → `/containers/{name}/stop` 409 response description is unchanged (does
    NOT mention "Ready" as a requirement)
  - → both paths still list 200/404/409/500 responses

### Task 5 - Web UI Start Button Gated on Ready

- `render({state: "Ready", auxContainers: [{name: "kokoro", running: false}]})`
  - → Start button is enabled (not disabled)
  - → Stop button is disabled (container not running)
- `render({state: "Off", auxContainers: [{name: "kokoro", running: false}]})`
  - → Start button is disabled
  - → Stop button is enabled
- `render({state: "Off", auxContainers: [{name: "kokoro", running: true}]})`
  - → Start button is disabled
  - → Stop button is enabled (container running, stop allowed in Off)
- `render({state: "Starting", auxContainers: [{name: "kokoro", running: false}]})`
  - → both Start and Stop buttons are disabled
- `render({state: "Ready", cooldownRemaining: 5, auxContainers: [{name: "kokoro", running: false}]})`
  - → Start button is disabled (cooldown active)
- `render({state: "Ready", auxContainers: []})`
  - → aux card is hidden

### Task 6 - Web UI Feedback Message for Rejected Start

- `doAuxAction("kokoro", "start")` receiving a 409 response
  - → feedback text is "GPU not ready — power on first"
- `doAuxAction("kokoro", "stop")` receiving a 409 response
  - → feedback text is "Not allowed right now" (unchanged)
- `doAuxAction("kokoro", "start")` receiving a 404 response
  - → feedback text is "Container not found" (unchanged)
- `doAuxAction("kokoro", "start")` receiving a 200 response
  - → feedback text is "Starting kokoro done" (unchanged)

### Task 7 - Documentation and Build

- `README.md` "Optional Aux Containers" section states that aux containers can
  be started only when the system is Ready
  - → README contains the word "Ready" within the Optional Aux Containers
    section
- `docs/product.md` Features list includes the 021-aux-containers-gpu-gate
  story
  - → `product_test.go` passes (asserts presence of
    `021-aux-containers-gpu-gate`)
- `make build && make test && make lint` all pass

## Technical Context

- Go 1.24.4, module `github.com/dockmind/dockmind`. No new dependencies.
- The `Ready` state is the system's source of truth for "GPU detected": it is
  entered only after `m.gpu.Status()` returns present during `startup()`
  (state.go:693) and `setState(Ready, nil)` (state.go:722). No other path
  enters `Ready`.
- `doAuxOperation` performs the name lookup (returns `AuxResultNotFound` for
  unknown names) **before** acquiring `transitionMu` and reading state. This
  ordering is preserved: an unknown name returns `AuxResultNotFound` in any
  state, including `Off`. The new gate only affects known names.
- `AuxResultConflict` already maps to HTTP 409 in `handleAuxResult`
  (api.go:161) and is already surfaced in the web UI as "Not allowed right
  now" (index.html `doAuxAction`). No new status code or result enum value.
- The web UI's `auxEnabled` variable (index.html:756) is a local in `render`;
  splitting it into `auxStartEnabled` / `auxStopEnabled` is a pure local
  refactor with no DOM/CSS impact.
- `readme_test.go` asserts presence of `auxContainers`, the two route paths,
  and that the first yaml block loads via `config.Load`. A prose edit to the
  Optional Aux Containers section does not affect any of these.
- `product_test.go` currently asserts `018-optional-containers` is present;
  the new assertion for `021-aux-containers-gpu-gate` follows the same
  `strings.Contains` pattern.

## Notes

- The gate is state-based, not probe-based, to stay consistent with how aux
  operations already work (they check `m.state`, never probe dependencies).
  A live `m.gpu.Status()` probe would add latency and inconsistency for no
  real benefit, since `Ready` is only reachable after a confirmed GPU
  detection.
- Stopping remains allowed in `Off` so a user can clean up a container that
  was started externally (e.g. via `docker` CLI) while the system is off, and
  so the existing shutdown `StopAll` path is unaffected (it runs in the
  `ShuttingDown` state via a separate code path, not via `StopAuxContainer`).
- The existing `TestStartAuxContainer` table in `state_test.go` has a case
  `"Off start whisper"` expecting `AuxResultOK` — this case must be updated
  to expect `AuxResultConflict` and `wantStart: false`. The `"Off stop
  kokoro"` case in `TestStopAuxContainer` stays `AuxResultOK`.
- The OpenAPI 409 description change is description-only; no tooling that
  relies on the response code set is affected.
