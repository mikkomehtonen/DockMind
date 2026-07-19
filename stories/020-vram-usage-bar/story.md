# VRAM Usage Bar

## Context

The web UI's GPU processes banner already shows a textual VRAM summary (`VRAM: used / total (free)`) sourced from `nvidia-smi`'s raw memory strings. Users want a quick visual gauge of memory pressure alongside that text, rendered as a horizontal progress bar below the text line. The bar must fit the existing black/red/green/white oklch theme and only appear when memory data is available and the banner is visible.

## Out of Scope

- Changing when the GPU processes banner is shown (still gated on `gpuProcesses` being non-empty).
- Adding new backend fields or `nvidia-smi` queries — the bar is computed client-side from the existing `gpuMemory.used` / `gpuMemory.total` strings already in `GET /status`.
- Showing a percentage number on the bar.
- Showing the bar outside the GPU processes banner.
- Background polling or caching.

## Implementation approach

- Add a progress bar element inside the existing `.gpu-procs` banner, immediately after the `#gpu-procs-memory` `<p>` element. Structure: a track `<div class="gpu-procs__vram-bar">` containing a fill `<div class="gpu-procs__vram-fill">`.
- The fill width is set via inline `style="width: NN%"` from JavaScript, where `NN` is `round(used / total * 100)` clamped to `[0, 100]`.
- Parsing rule: extract the leading integer from `gpuMemory.used` and `gpuMemory.total` via `parseInt(str, 10)` (this handles `12742 MiB`, `16311 MiB`, etc.). If either parse result is `NaN`, or `total <= 0`, the bar is hidden (`hidden` attribute set on the track element) and only the text line remains. This covers `N/A` and any non-numeric output.
- Color thresholds (applied by toggling a modifier class on the fill element, using existing theme tokens). The thresholds are defined as named JS constants so they are identifiable in the embedded source: `const VRAM_WARN_PCT = 75;` and `const VRAM_CRIT_PCT = 90;`.
  - `pct < VRAM_WARN_PCT` → class `gpu-procs__vram-fill--ok` → `background: var(--primary)` (green).
  - `VRAM_WARN_PCT <= pct < VRAM_CRIT_PCT` → class `gpu-procs__vram-fill--warn` → `background: var(--busy)` (amber).
  - `pct >= VRAM_CRIT_PCT` → class `gpu-procs__vram-fill--crit` → `background: var(--danger)` (red).
- The bar is rendered/updated in the existing `render(data)` function, in the same `if (data.gpuProcesses && data.gpuProcesses.length > 0)` block that already manages `#gpu-procs-memory`. When the banner is hidden (no processes), the bar is also hidden.
- CSS: the track is `width: 100%; height: 0.5rem; background: var(--surface-2); border-radius: 999px; overflow: hidden;` and the fill is `height: 100%; border-radius: 999px; transition: width 200ms ease, background-color 200ms ease;`. The `prefers-reduced-motion` block is extended to disable the fill transition.
- No README, OpenAPI, or backend changes — `gpuMemory` is already documented and tested.

## Tasks

### Task 1 - VRAM bar markup and styles

- `GET /` response body contains a `gpu-procs__vram-bar` element inside the `.gpu-procs` section
- `GET /` response body contains a `gpu-procs__vram-fill` child element inside the bar track
- the CSS defines `.gpu-procs__vram-bar`, `.gpu-procs__vram-fill`, and the three modifier classes (`--ok`, `--warn`, `--crit`) referencing `var(--primary)`, `var(--busy)`, and `var(--danger)` respectively
- the `prefers-reduced-motion` media query disables the fill's `transition`

### Task 2 - VRAM bar rendering logic

The rendering logic lives in the embedded `<script>` of `index.html`. Automated verification follows the established repo convention (see story 019's `"VRAM:"`, `"gpuMemory"` substring ACs): the `GET /` body-substring test asserts that the supporting JS constructs are present and identifiable. Each AC below lists the behavior plus the substring anchor the test checks.

- percentage computation from `gpuMemory.used` / `gpuMemory.total`
  - → body contains `parseInt(` applied to `gpuMemory.used` and `gpuMemory.total` (anchor: `parseInt`)
  - → body defines the thresholds as named constants `VRAM_WARN_PCT` (value `75`) and `VRAM_CRIT_PCT` (value `90`) used to pick the modifier class (anchors: `VRAM_WARN_PCT`, `VRAM_CRIT_PCT`)
- color modifier class assignment
  - → body contains the three class-name strings `gpu-procs__vram-fill--ok`, `gpu-procs__vram-fill--warn`, `gpu-procs__vram-fill--crit` (anchors: those exact strings)
- fill width is set as an inline percentage
  - → body contains a `width:` assignment producing a `%` value on the fill element (anchor: `width:` and `%` already present in CSS; the JS anchor is `.style.width` — anchor: `style.width`)
- bar hidden when memory is unparseable / unavailable
  - → body contains logic that sets `hidden` on the bar track when `parseInt` yields `NaN` or `total <= 0` (anchors: `hidden`, `NaN`)
- bar hidden together with the banner when `gpuProcesses` is empty
  - → covered by the existing `gpuProcsBanner.hidden = true` path in the `else` branch; the bar track must be cleared/hidden in that same branch (anchor: `gpuProcsBanner.hidden = true` already present)

## Bootstrap

No new dependencies. Existing commands still apply:

```bash
make build
make test
make lint
```

## Technical Context

- Go 1.24.4, stdlib `testing` only, single external dependency `gopkg.in/yaml.v3`.
- The UI is a single embedded `internal/api/index.html` served by `internal/api`; it polls `/status` once per second.
- `gpuMemory` (`total`, `used`, `free`) is already part of `state.StatusResponse` and the `/status` JSON; no backend change is needed.
- The existing `GET /` test in `internal/api/api_test.go` asserts that the body contains a fixed list of substrings; the new bar-related class/element names and JS anchors (`gpu-procs__vram-bar`, `gpu-procs__vram-fill`, `gpu-procs__vram-fill--ok`, `gpu-procs__vram-fill--warn`, `gpu-procs__vram-fill--crit`, `parseInt`, `VRAM_WARN_PCT`, `VRAM_CRIT_PCT`, `NaN`) must be added to that list so the test stays green and enforces the markup/logic's presence. This matches the convention used for prior UI stories (e.g. story 019's `"VRAM:"`, `"gpuMemory"` anchors).

## Notes

- The bar is purely a client-side visualization computed from strings already in the payload — no extra `nvidia-smi` invocation per poll.
- `parseInt` is used (not `parseFloat`) because `nvidia-smi` reports integer MiB values; this keeps parsing simple and matches the raw-string philosophy from story 019.
- The bar sits below the `VRAM: used / total (free)` text line, exactly as requested.
- Worked examples (for the implementer, verified): `12742/16311 MiB` → 78% → amber (`--warn`); `1200/16311` → 7% → green (`--ok`); `15000/16311` → 92% → red (`--crit`); `16311/16311` → 100% → red (`--crit`). Rounding uses `Math.round`; the result is clamped to `[0, 100]`.
