# GPU Temperature Display

## Context

The web UI's GPU banner already shows VRAM (text + pressure-colored bar, stories 019/020) and GPU utilization (text + single-color bar, story 023), both sourced from a single `nvidia-smi` call. Operators watching a loaded eGPU want the same at-a-glance read for **GPU temperature**. This adds `temperature.gpu` to the existing `nvidia-smi --query-gpu` invocation (not a new subprocess — one call already fetches memory + utilization), surfaces the value in `GET /status` (`gpuMemory.temperature`), and renders a `Temperature: X °C` text line plus a 0–100 °C bar directly beneath the utilization bar, color-coded by heat.

## Out of Scope

- No label/layout changes to existing VRAM or utilization rows — temperature is appended below them.
- No temperature probe when the GPU is absent (the banner is hidden then; already gated on `gpuPresent` from story 023).
- No new `GPUMonitor` method and no new top-level `StatusResponse` field — temperature rides inside the existing `gpuMemory` object, exactly as utilization does.
- No numeric/unit conversion in the backend (raw string passthrough, same as memory and utilization).
- No per-GPU or multi-GPU aggregation, no min/max/avg history, no logging/alerting on temperature thresholds.
- No change to the banner's visibility gate — it is already always-visible when the GPU is present (story 023).

## Implementation approach

### Backend: combine temperature into the existing memory nvidia-smi call

`internal/gpu/gpu.go` `Monitor.Memory` builds the query at `gpu.go:79`. Append `,temperature.gpu`:

```
nvidia-smi --query-gpu=memory.total,memory.used,memory.free,utilization.gpu,temperature.gpu --format=csv,noheader
```

Keep `noheader` and **do not** add `nounits` (memory/utilization keep their units). Note: unlike `memory.*` (`8192 MiB`) and `utilization.gpu` (`0 %`), the `temperature.gpu` column is emitted as a **bare integer with no unit** (e.g. `39`), confirmed against a real device. So the raw stored string is `"54"`, `"0"`, `"N/A"`, etc.; the UI appends the °C unit for display.

Parsing rule (`gpu.go:84-101`) — bump the split arity by one. `strings.SplitN(trimmed, ", ", 4)` → `5`, and the `len(fields) < 4` guard → `len(fields) < 5`, with the same error shape (`fmt.Errorf("unexpected nvidia-smi memory output: %q", trimmed)`). Because `temperature.gpu` is the **last** column, the greedy final `SplitN` field captures it cleanly (`fields[4] == "54"`). First non-empty line wins; empty stdout still returns `"no nvidia-smi memory output"`. Map `fields[4]` → the new `Temperature` field, verbatim (no `strconv`, no trimming).

### Backend: new field on `state.GPUMemory` — no interface or wiring change

`internal/state/state.go:64-69` `GPUMemory` gains `Temperature string \`json:"temperature"\``. `probeGPUMemory` (`state.go:620-629`) copies the whole struct from `m.gpu.Memory(ctx)` on success and returns `GPUMemory{}` on error — the new field flows through untouched. The probe is already gated on `gpuPresent` inside `Status()` (`state.go:401-404`) from story 023, so no gate change and **no change to `cmd/dockmind/main.go`** (the whole struct is copied wholesale; only whole clients need setters).

### UI: temperature text line + 0–100 °C color-coded bar

All in `internal/api/index.html` (single embedded file). Mirror the utilization bar exactly, with pressure color-coding like VRAM.

- **HTML** (`index.html:642-653`): after the utilization bar block, add `<p class="gpu-procs__temp" id="gpu-procs-temp"></p>` and `<div class="gpu-procs__temp-bar" id="gpu-procs-temp-bar" hidden><div class="gpu-procs__temp-fill" id="gpu-procs-temp-fill"></div></div>`.
- **Element handles** (`index.html:708-715`): add `gpuProcsTemp`, `gpuProcsTempBar`, `gpuProcsTempFill`.
- **CSS** (clone the `.gpu-procs__util-bar`/`__util-fill` rules at `index.html:453-466`): track `.gpu-procs__temp-bar` identical to the util track (`background: var(--surface-2)`). `.gpu-procs__temp-fill` base has no default color; color comes from modifiers reusing the existing theme tokens: `.gpu-procs__temp-fill--ok { background: var(--primary); }`, `--warn { background: var(--busy); }`, `--crit { background: var(--danger); }`. Add the temp fill to the `prefers-reduced-motion` opt-out list (`index.html:570-571`).
- **Thresholds** (JS constants next to `VRAM_WARN_PCT`/`VRAM_CRIT_PCT` at `index.html:687-688`): `const TEMP_WARN_C = 70; const TEMP_CRIT_C = 85;`.
- **Render** (inside existing `render(data)`, after the utilization block `index.html:855-867`): when `data.gpuMemory && data.gpuMemory.temperature` is truthy, set text `Temperature: ${data.gpuMemory.temperature} °C`; parse `const n = parseInt(data.gpuMemory.temperature, 10)`. If `isNaN(n)` → leave the bar `hidden` (text still shows the raw value with °C appended, matching utilization's raw-passthrough behavior). Else: `width = Math.min(100, Math.max(0, n)) + "%"` (the 0–100 °C scale means the °C value is already a percentage); show the bar; set fill class to `--crit` when `n >= TEMP_CRIT_C`, else `--warn` when `n >= TEMP_WARN_C`, else `--ok` (so `<70` green, `70–84` amber, `≥85` red). When the field is falsy → clear text and hide bar. In the `!data.gpuPresent` reset branch (`index.html:868-875`) also clear temp text and hide the temp bar.

### Docs

- `internal/api/openapi.json`: add a sibling `"temperature": { "type": "string", ... }` to the `gpuMemory` properties (`openapi.json:273-282`).
- `README.md`: add `temperature` to the `gpuMemory` object in the `/status` JSON example (`README.md:233-237`).
- `docs/product.md`: add a Features bullet for this story (see below).

## Tasks

### Task 1 - Fetch temperature in the gpu package (`internal/gpu`)

- GPU present + stdout `16311 MiB, 12742 MiB, 3108 MiB, 24 %, 54\n` + `Memory()` called
  - → returns `state.GPUMemory{Total:"16311 MiB", Used:"12742 MiB", Free:"3108 MiB", Utilization:"24 %", Temperature:"54"}`
- GPU present + multi-GPU stdout (`16311 MiB, 12742 MiB, 3108 MiB, 24 %, 54\n24576 MiB, 0 MiB, 24576 MiB, 0 %, 30\n`) + `Memory()`
  - → first line's values returned (`Temperature: "54"`)
- 4-field line with no temperature (`16311 MiB, 12742 MiB, 3108 MiB, 24 %\n`) + `Memory()`
  - → returns an error (`len(fields) < 5`) — extend the existing "missing fields" case
- empty stdout + `Memory()`
  - → returns an error, zero-value `GPUMemory{}`
- exec error + `Memory()`
  - → returns the error, zero-value `GPUMemory{}`
- `Memory()` invoked + args captured
  - → command name is `nvidia-smi`, args are exactly `["--query-gpu=memory.total,memory.used,memory.free,utilization.gpu,temperature.gpu", "--format=csv,noheader"]`
  - → `temperature.gpu` appears exactly once in the query argument (extend the args test; keep the existing `utilization.gpu`-once assertion)

### Task 2 - Surface temperature through the state machine (`internal/state`)

- `GPUMemory` literal with `Temperature: "54"` returned by the fake `GPUMonitor` + GPU present + `Status()` called
  - → `status.GPUMemory.Temperature == "54"` in the returned `StatusResponse`
- GPU absent + `Status()` called
  - → `status.GPUMemory == GPUMemory{}` (i.e. `Temperature == ""`)
- fake `Memory()` returns an error + GPU present + `Status()` called
  - → `status.GPUMemory` is the zero value (`Temperature == ""`) and a `DEBUG` log line `GPU memory probe failed` is emitted
- existing all-empty/zero-state assertions for `GPUMemory`
  - → updated to also assert `status.GPUMemory.Temperature == ""` in zero cases, and add a `!= ""` assertion alongside the existing `Utilization != ""` checks where memory is populated

### Task 3 - Render temperature text + bar in the web UI (`internal/api`)

- `GET /` served HTML + substring anchors present (extend the existing anchor list in `TestWebUIRoutes`)
  - → contains `id="gpu-procs-temp"`, `gpu-procs__temp-bar`, `gpu-procs__temp-fill`, `gpu-procs__temp-fill--ok`, `gpu-procs__temp-fill--warn`, `gpu-procs__temp-fill--crit`, `gpuMemory.temperature`, `Temperature:`, `TEMP_WARN_C`, `TEMP_CRIT_C`
- `GET /` served HTML + negative anchors (unchanged)
  - → the existing forbidden substrings (util fill `--warn`/`--crit`) remain absent; temp's own `--warn`/`--crit` classes are distinct names and do not collide

  The exact threshold boundary math, the `Math.min/max` clamp to 0–100, and the `isNaN`→hide rule inside `render(data)` are not substring-enforceable and are covered by code review against the rules above (mirrors story 023's division of labor).

### Task 4 - Schema and docs

- `GET /openapi.json` + `StatusResponse` schema assertion
  - → `gpuMemory.properties` includes `temperature` (extend the existing `{total,used,free,utilization}` anchor list in `TestSwaggerRoutes`)
- `README.md` `/status` example + `readme_test.go`
  - → README's `gpuMemory` example includes a `temperature` key; add a `readme_test.go` row asserting `temperature` is present and keep `make test` green
- `docs/product.md` + `product_test.go`
  - → a Features bullet referencing `031-add-gpu-temperature` exists; `product_test.go` gains a row asserting that slug is mentioned

## Technical Context

- No new dependencies — stdlib `os/exec`, `strings`, `net/http` only; toolchain unchanged (Go 1.24.4). `nvidia-smi` is not installed on the dev host, so all gpu-package tests use the injectable `execFunc` seam (`gpu.go:13-17`).
- `temperature.gpu` emits a **bare integer, no unit** (verified against real hardware: `..., 0 %, 39`), unlike the unit-bearing memory/utilization columns; the °C unit is appended only in the UI text template.
- Raw-string passthrough is deliberate throughout `GPUMemory`; keep it — do not `strconv`-convert or strip units in the backend.
- `SplitN(..., ", ", N)` arity is hardcoded in two coupled places: the call (`gpu.go:90`) and the `len(fields) < N` guard (`gpu.go:91`). Both must move from `4`→`5`; missing one lets the greedy last field swallow `"24 %, 54"`.
- Tests to extend: `internal/gpu/gpu_test.go:115-214` (table rows + args test), `internal/state/state_test.go:2192-2300` (`GPUMemory` fixtures + probe matrix + DEBUG log), `internal/api/api_test.go:500-512` (`gpuMemory.properties`) and `:908-994` (UI substring anchors), `readme_test.go:59-60` and `product_test.go:58-59` (add sibling rows for this story).
- Theme tokens already defined at `index.html:9-28`: `--primary` (green), `--busy` (amber), `--danger` (red), `--surface-2` (track) — reuse, do not introduce new colors.

## Notes

- Color thresholds chosen at 70/85 °C (amber 70–84, red ≥ 85) — near typical eGPU throttle points; the decision (color-coded, not neutral like utilization) was made explicitly because temperature is a heat/pressure metric.
- The degree symbol `°` is UTF-8 in the embedded HTML; anchor substring tests use `Temperature:` (not the symbol) to avoid encoding flakiness.
- When a GPU reports `temperature.gpu` as `N/A` (rare — the banner only renders when `gpuPresent` already required a successful `nvidia-smi` call), the text shows the raw value with ` °C` appended and the bar hides (`parseInt` → `NaN`), consistent with how the utilization bar tolerates unparseable raw values.
- `GET /status` keeps its "safe defaults for unreachable dependencies" contract (`AGENTS.md`): an unreachable GPU yields zero-value `GPUMemory` (`Temperature == ""`), so the UI temp row simply stays hidden.
