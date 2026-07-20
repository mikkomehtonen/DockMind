# GPU Utilization Display

## Context

The web UI's GPU processes banner already shows VRAM usage (text + pressure-colored bar, stories 019/020) sourced from `nvidia-smi`. Users want a second metric — current GPU utilization — rendered the same way: a `Utilization: X %` text line with a horizontal bar beneath it, placed directly under the VRAM bar. Utilization is available from `nvidia-smi` via `--query-gpu=utilization.gpu`; to avoid spawning one `nvidia-smi` process per metric, it must be fetched in the **same** `nvidia-smi` invocation that already fetches memory. Additionally, the GPU processes banner is currently hidden when no compute processes are running, which means VRAM/utilization info disappears exactly when the user is idle-curious. The banner should instead always be visible whenever the GPU is present, showing VRAM and utilization (both near 0 when idle) even with an empty process list.

## Out of Scope

- Changing the "GPU processes" banner label or removing the process list when empty (the label and empty list remain).
- Adding utilization to the `Status()` probe path for the `Off` state (GPU absent → banner hidden, no probe).
- Color-coding utilization by pressure — utilization is not a pressure metric, so the bar uses a single neutral color.
- Showing a percentage number on the bar.
- Background polling or caching — utilization is queried live on each `/status` poll, same as memory.
- Renaming the `GPUMemory` struct or `Memory` method (see Notes for the semantic rationale).

## Implementation approach

### Backend: combine utilization into the memory nvidia-smi call

`internal/gpu/gpu.go`'s `Memory` currently runs:
```
nvidia-smi --query-gpu=memory.total,memory.used,memory.free --format=csv,noheader
```
producing `16311 MiB, 12742 MiB, 3108 MiB\n`. Append `utilization.gpu` to the query and keep `--format=csv,noheader` (do **not** add `nounits`, so memory values retain their ` MiB` suffix and the existing VRAM text display is unchanged):
```
nvidia-smi --query-gpu=memory.total,memory.used,memory.free,utilization.gpu --format=csv,noheader
```
producing `16311 MiB, 12742 MiB, 3108 MiB, 24 %\n`.

Parsing rule: change `strings.SplitN(trimmed, ", ", 3)` to `strings.SplitN(trimmed, ", ", 4)`. Require `len(fields) >= 4`; otherwise return an error `fmt.Errorf("unexpected nvidia-smi memory output: %q", trimmed)` (same shape as the existing 3-field check). Map `fields[3]` (e.g. `24 %`) into the new `Utilization` field. The first non-empty line wins (same multi-GPU first-line convention as today).

Add a `Utilization string` field to `state.GPUMemory` with JSON tag `utilization`:
```go
type GPUMemory struct {
	Total       string `json:"total"`
	Used        string `json:"used"`
	Free        string `json:"free"`
	Utilization string `json:"utilization"`
}
```
The raw string (e.g. `24 %`, `0 %`) is stored as-is, matching the raw-string philosophy of the existing memory fields. No new `GPUMonitor` interface method and no new `StatusResponse` top-level field — utilization rides inside `gpuMemory`.

### Backend: probe memory whenever the GPU is present

`internal/state/state.go`'s `Status()` currently probes memory only inside `if gpuProcesses != nil && len(gpuProcesses) > 0`. Change the condition so memory (now carrying utilization) is probed whenever `gpuPresent` is true:
```go
gpuPresent, gpuName := m.probeGPU(probeFailureExpected(state))
var gpuProcesses []GPUProcess
var gpuMemory GPUMemory
if gpuPresent {
	gpuProcesses = m.probeGPUProcesses()
	gpuMemory = m.probeGPUMemory()
}
if gpuProcesses == nil {
	gpuProcesses = []GPUProcess{}
}
```
`probeGPUMemory` is unchanged (10s timeout, `DEBUG` log on failure, returns zero-value `GPUMemory{}` on error). When the GPU is absent, `gpuMemory` stays zero-value and `gpuProcesses` is `[]GPUProcess{}` — same as today.

### UI: always-visible banner + utilization text and bar

In `internal/api/index.html`:

1. **Banner visibility.** The `#gpu-procs-banner` is currently shown only when `data.gpuProcesses.length > 0`. Change the gate to `data.gpuPresent`: when the GPU is present the banner is shown (process list may be empty); when absent the banner is hidden and all sub-elements cleared. The "GPU processes" label and `#gpu-procs-list` remain; the list simply has no `<li>` children when there are no processes.

2. **Utilization text line.** Add a `<p class="gpu-procs__util" id="gpu-procs-util"></p>` immediately after the VRAM bar (`#gpu-procs-vram-bar`), inside the `.gpu-procs` section. When `data.gpuMemory.utilization` is non-empty, set its text to `Utilization: ${data.gpuMemory.utilization}` (e.g. `Utilization: 24 %`); otherwise clear it.

3. **Utilization bar.** Add a track `<div class="gpu-procs__util-bar" id="gpu-procs-util-bar" hidden>` containing `<div class="gpu-procs__util-fill" id="gpu-procs-util-fill"></div>`, immediately after the utilization text. The fill width is `parseInt(data.gpuMemory.utilization, 10)` clamped to `[0, 100]`, set via `style.width`. The fill always uses a single neutral color — class `gpu-procs__util-fill` with `background: var(--primary)` (green). No `--warn`/`--crit` modifier classes. When `parseInt` yields `NaN` or the utilization string is empty, the bar is hidden (`hidden` attribute on the track). The bar is also hidden when the banner is hidden (GPU absent).

4. **CSS.** Add `.gpu-procs__util` (same styling as `.gpu-procs__memory`: `margin: 0.25rem 0 0; font-size: var(--fs-sm); color: var(--muted);`), `.gpu-procs__util-bar` (identical to `.gpu-procs__vram-bar`: `width: 100%; height: 0.5rem; background: var(--surface-2); border-radius: 999px; overflow: hidden;`), and `.gpu-procs__util-fill` (`height: 100%; border-radius: 999px; background: var(--primary); transition: width 200ms ease;`). Extend the existing `prefers-reduced-motion` media query to disable the util fill's `transition`.

5. **Rendering location.** All utilization rendering happens in the existing `render(data)` function, in the same block that manages the banner. The VRAM text/bar logic stays as-is (still parsed from `gpuMemory.used`/`gpuMemory.total`); the utilization text/bar is parsed from `gpuMemory.utilization`.

### Docs

- `internal/api/openapi.json`: add `utilization` (type `string`, description "GPU utilization as reported by nvidia-smi (e.g. '24 %'). Empty string when the GPU is absent or the probe fails.") to the `gpuMemory.properties` object. Update the `gpuMemory` description from "Empty strings when the GPU is absent or no compute processes are running." to "Overall GPU memory usage and utilization. Empty strings when the GPU is absent or the probe fails."
- `README.md`: add `"utilization": "24 %"` to the `gpuMemory` object in the `/status` JSON example.
- `docs/product.md`: add a Features list entry referencing `023-gpu-utilization-display`.

## Tasks

### Task 1 - Combine utilization into the nvidia-smi memory query

- `Memory` called with a fake `execFunc` returning `16311 MiB, 12742 MiB, 3108 MiB, 24 %\n`
  - → returns `GPUMemory{Total: "16311 MiB", Used: "12742 MiB", Free: "3108 MiB", Utilization: "24 %"}`, no error
- `Memory` called with output `16311 MiB, 12742 MiB, 3108 MiB, 0 %\n` (idle GPU)
  - → returns `GPUMemory{..., Utilization: "0 %"}`, no error
- `Memory` called with multi-GPU output where the first line is `16311 MiB, 12742 MiB, 3108 MiB, 24 %\n` and the second line is another GPU
  - → returns the first line's values, `Utilization: "24 %"`
- `Memory` called with output missing the 4th field (`16311 MiB, 12742 MiB, 3108 MiB\n`)
  - → returns an error (non-nil)
- `Memory` called with empty stdout
  - → returns an error (non-nil)
- `Memory` called with an `execErr`
  - → returns `GPUMemory{}` (zero value, empty `Utilization`) and the error
- the `nvidia-smi` argument list passed to `execFunc` contains `utilization.gpu` exactly once and `memory.total,memory.used,memory.free,utilization.gpu` as a single `--query-gpu` argument (verifiable by capturing the args in the fake and asserting the joined query string)

### Task 2 - State machine probes memory whenever the GPU is present

- machine in `Ready`, GPU present, no compute processes (`gpuProcesses = []`)
  - → `Status().GPUMemory` is non-empty (reflects the fake's `memory`/`Utilization` values), not the zero value
  - → `Status().GPUProcesses` is `[]GPUProcess{}` (non-nil empty slice)
- machine in `Ready`, GPU present, two compute processes
  - → `Status().GPUMemory` reflects the fake's values including `Utilization`
  - → `Status().GPUProcesses` has length 2
- machine in `Off`, GPU absent
  - → `Status().GPUMemory` is the zero value (`Total`, `Used`, `Free`, `Utilization` all `""`)
  - → `Status().GPUProcesses` is `[]GPUProcess{}`
- machine in `Ready`, GPU present, memory probe returns an error
  - → `Status().GPUMemory` is the zero value (all fields `""`, including `Utilization`)
  - → a `DEBUG`-level log record with message `"GPU memory probe failed"` is emitted
  - → `Status().GPUProcesses` still reflects the probed processes (memory failure does not suppress the process list)

### Task 3 - Web UI banner visibility and utilization rendering

The rendering logic lives in the embedded `<script>` of `index.html`. Automated verification follows the established repo convention (substring ACs in `internal/api/api_test.go`'s `GET /` body test). The **unique** anchors (strings not present in the body before this story) are what enforce the new logic; anchors already present from the VRAM bar (`parseInt`, `style.width`, `NaN`, `hidden`) are listed as supporting pattern references and need not be re-added if already present. Each AC lists the behavior plus its anchors, marking unique ones with **(unique)**.

- utilization text line element and class present
  - → body contains `gpu-procs__util` **(unique)** and `id="gpu-procs-util"` **(unique)**
- utilization bar track and fill elements present
  - → body contains `gpu-procs__util-bar` **(unique)** and `gpu-procs__util-fill` **(unique)**
- utilization text uses the `gpuMemory.utilization` payload field and the literal label
  - → body contains `gpuMemory.utilization` **(unique)** and `Utilization:` **(unique)**
- utilization bar width set from parsed integer
  - → body contains `parseInt` and `.style.width` set to a `%` (supporting anchors — already present from VRAM; the unique enforcement is `gpuMemory.utilization` being fed into the parse)
- utilization bar uses a single neutral color (no pressure modifiers)
  - → body contains `gpu-procs__util-fill` **(unique)** with `background: var(--primary)` in CSS, and the test asserts the body does **not** contain `gpu-procs__util-fill--warn` or `gpu-procs__util-fill--crit`
- bar hidden when utilization is unparseable / unavailable
  - → body contains `NaN` and `hidden` (supporting anchors — already present from VRAM); the unique enforcement is that the util bar track `gpu-procs__util-bar` is referenced in the hiding logic
- banner and all sub-elements cleared when GPU is absent
  - → the `else` branch (GPU absent) sets `gpuProcsBanner.hidden = true` and clears the utilization text/bar (anchor: `gpuProcsBanner.hidden = true` — already present; the unique enforcement is that the util elements are cleared in this branch)

The banner-visibility gating change (from `data.gpuProcesses.length > 0` to `data.gpuPresent`) is a control-flow edit to existing code that a substring test cannot uniquely enforce, because `gpuPresent` already appears in the body for the GPU component dot. It is verified indirectly by Task 2 (the backend now always sends `gpuMemory` when the GPU is present, even with no processes) and by code review.

### Task 4 - OpenAPI spec, README, and product doc

- `GET /openapi.json` response: `components.schemas.StatusResponse.properties.gpuMemory.properties` contains `utilization` (type `string`)
- `README.md` `/status` JSON example's `gpuMemory` object contains a `"utilization"` key — add `{"status example includes utilization field", "utilization", true}` to the `readme_test.go` substring table so it is enforced
- `docs/product.md` Features list contains an entry referencing `023-gpu-utilization-display` — enforced by `product_test.go` (check already added during planning)

## Bootstrap

No new dependencies. Existing commands still apply:

```bash
make build
make test
make lint
```

## Technical Context

- Go 1.24.4, stdlib `testing` only, single external dependency `gopkg.in/yaml.v3`.
- `nvidia-smi` is not installed on the dev host; every `gpu` test injects a fake `execFunc`. The combined query string is verified by capturing args in the fake.
- The UI is a single embedded `internal/api/index.html` served by `internal/api`; it polls `/status` once per second.
- `gpuMemory` (`total`, `used`, `free`) is already part of `state.StatusResponse` and the `/status` JSON; this story adds `utilization` to the same object and changes when it is populated (whenever the GPU is present, not just when processes exist).
- The existing `GET /` substring test in `internal/api/api_test.go` must be extended with the new anchors (`gpu-procs__util`, `gpu-procs-util`, `gpu-procs__util-bar`, `gpu-procs__util-fill`, `gpuMemory.utilization`, `Utilization:`) and must assert the absence of `gpu-procs__util-fill--warn` / `gpu-procs__util-fill--crit`.
- The existing OpenAPI test in `internal/api/api_test.go` asserts `gpuMemory.properties` contains `total`, `used`, `free`; add `utilization` to that list.

## Notes

- The `GPUMemory` struct name becomes a slight misnomer once it carries utilization. Renaming it (and the `Memory` method, the `GPUMonitor` interface, and all fakes) would be larger churn for no functional gain and would break the existing `gpuMemory` JSON key. This story keeps the name and adds the field; a future rename can happen independently.
- `--format=csv,noheader` (without `nounits`) is kept so memory values retain their ` MiB` suffix. The utilization value therefore arrives as `24 %` (with the `%` suffix) rather than the bare `24` that `nounits` would produce. The UI displays `Utilization: 24 %` and parses the integer with `parseInt` for the bar width — identical to how VRAM strings are handled.
- The utilization bar is green at all levels (0–100 %) because high utilization is normal operation, not a warning state. This differs deliberately from the VRAM bar's pressure coloring.
- When the GPU is present but idle (no processes), `nvidia-smi` typically reports `0 %` utilization and `0 MiB` used memory; the banner shows `Utilization: 0 %` with a zero-width bar and `VRAM: 0 MiB / <total> MiB (<total> MiB free)` with its bar.
- The "GPU processes" label and empty list remain visible when there are no processes; only the banner's *visibility* gate changes (from "has processes" to "GPU present").
