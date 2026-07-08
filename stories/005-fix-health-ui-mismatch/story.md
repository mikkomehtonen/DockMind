# Fix Web UI health row label and stopped-state value

## Context

The Web UI "Components" section has a row labeled **"Health check"**
(`internal/api/index.html:366`) whose value is driven by `data.llamaSwapHealthy`
(`index.html:489`):

```js
els.healthValue.textContent = data.llamaSwapHealthy ? "Healthy" : "Unhealthy";
```

`llamaSwapHealthy` comes from `GET /status`, which live-probes **llama-swap's**
`/v1/models` endpoint (`internal/health/health.go` `Client.Check` → returns
`true` only on HTTP 200, surfaced via `state.Machine.Status` →
`probeBool("Health", ...)`). This is a **completely separate concept** from the
daemon's `GET /health` endpoint (`internal/api/api.go:90`), which always returns
200 and is documented — in the OpenAPI spec (`openapi.json:81` "DockMind daemon
health (not GPU readiness)"), the README (`README.md:50`), and the MVP spec
(`docs/DockMind_MVP_Specification.md:184-188`) — as daemon liveness, **not** GPU
or inference readiness.

When the system is **Off**, llama-swap is intentionally stopped, so
`llamaSwapHealthy=false` and the row shows **"Unhealthy"** — even though the
daemon is perfectly healthy (`curl /health` → 200, as the user observed). The
label "Health check" reads as if it reflects the `/health` endpoint, which is
the mismatch the user hit.

Root cause: (a) a label that conflates the llama-swap inference-backend health
probe with the daemon `/health` endpoint, and (b) a value rule that reports
"Unhealthy" for the intentional-stopped case (a non-fault), producing a false
alarm.

The API contract (`llamaSwapHealthy` field, `/health` endpoint, OpenAPI, README,
MVP spec) is correct as designed and is **not** changed. This is a UI-only fix
to `internal/api/index.html` plus automated substring-contract assertions in
`internal/api/api_test.go`.

## Out of Scope

- Any change to the `GET /health` endpoint, `GET /status` response, the
  `StatusResponse` struct (`internal/state/state.go:65-73`), the OpenAPI spec
  (`internal/api/openapi.json`), `README.md`, `docs/product.md`, or
  `docs/DockMind_MVP_Specification.md`. The API contract is correct as designed.
- Any change to the state machine, health client, or other Go backend logic.
- Any change to the DOM element IDs (`health-dot`, `health-value`) or the JS
  element-lookup object `els` — only the visible label text and the `render`
  value/dot logic change. Keeping IDs stable avoids churn in the element lookup.
- A headless-browser or JS test harness. The repo is stdlib-only with no JS test
  infrastructure (story 004 established this); browser runtime behavior is
  verified manually (see Notes), while Go-side tests verify the served HTML
  contract via substring assertions.

## Implementation approach

All changes are confined to two files: `internal/api/index.html` (the embedded
SPA) and `internal/api/api_test.go` (the Go-side contract test). No Go backend
files, no docs, no dependencies change. `go.mod` stays at
`gopkg.in/yaml.v3 v3.0.1` only.

### 1. Relabel the row (visible text only)

In `internal/api/index.html`, the health row label (line 366):

```html
<span class="component__label">Health check</span>
```

becomes:

```html
<span class="component__label">llama-swap health</span>
```

Lowercase `health` matches the existing "llama-swap" row label (line 361, also
lowercase) and the `llamaSwapHealthy` field name. The element IDs `health-dot`
and `health-value` (lines 365, 367) and the JS references `els.healthDot` /
`els.healthValue` (lines 417-418) are unchanged.

### 2. Add a red "danger" dot class for component dots

The component-dot CSS currently has only the green `is-on` state (lines 91-95):

```css
.conn__dot.is-live,
.state__dot.is-primary,
.component__dot.is-on {
  background: var(--primary);
}
```

Add a new rule for the danger (red) state on component dots, placed immediately
after the `.state__dot.is-danger` rule (line 102-104) so the danger variants
stay grouped:

```css
.component__dot.is-danger {
  background: var(--danger);
}
```

`--danger` (`oklch(0.62 0.18 25)`) is already defined on `:root` (line 17) and
already used by `.state__dot.is-danger` and the Power Off button. No new color
token. This class is used **only** by the health row to signal a real fault
(running but failing its health check).

### 3. Replace the value/dot logic with a three-state predicate

In `render(data)` (lines 489-490), replace:

```js
els.healthValue.textContent = data.llamaSwapHealthy ? "Healthy" : "Unhealthy";
setComponentDot(els.healthDot, data.llamaSwapHealthy);
```

with:

```js
if (!data.llamaSwapRunning) {
  els.healthValue.textContent = "Stopped";
  els.healthDot.className = "component__dot";
} else if (data.llamaSwapHealthy) {
  els.healthValue.textContent = "Healthy";
  els.healthDot.className = "component__dot is-on";
} else {
  els.healthValue.textContent = "Unhealthy";
  els.healthDot.className = "component__dot is-danger";
}
```

The `setComponentDot` helper (lines 440-442) is **not** removed — it is still
used by the GPU, Shelly, and llama-swap running rows (lines 481, 484, 487). Only
the health row uses inline `className` assignment because it has three states
(neutral / on / danger) instead of two.

**Value/dot matrix (predicate order matters — check `!llamaSwapRunning` first):**

| `llamaSwapRunning` | `llamaSwapHealthy` | Value text  | Dot class                       | Meaning                                  |
|--------------------|--------------------|-------------|---------------------------------|------------------------------------------|
| `false`            | (any)              | `Stopped`   | `component__dot` (gray/neutral) | Intentionally not running (Off, etc.)    |
| `true`             | `true`             | `Healthy`   | `component__dot is-on` (green)  | Running and `/v1/models` returns 200     |
| `true`             | `false`            | `Unhealthy` | `component__dot is-danger` (red)| Running but health check failing — fault |

Checking `!llamaSwapRunning` first means the contradictory
`!running && healthy` case (health probe true while the container is reportedly
stopped) renders as `Stopped` — the running state is the primary signal for
whether the backend is stopped, and a stale/contradictory healthy flag must not
override it. In practice `!running` implies the `/v1/models` probe is
unreachable so `llamaSwapHealthy` is `false` anyway, but the UI is defensive and
does not assume that coupling.

This keeps the existing convention that **gray (not red) = neutral/off** for
non-fault cases, while reserving **red = real fault** (running but unhealthy) —
matching the user's Q4 decision. The overall Error state is still conveyed by
the state badge and error banner, not by this dot alone.

### 4. Automated substring-contract assertions

Add to the `GET /` subtest of `TestWebUIRoutes` in `internal/api/api_test.go`
(lines 275-308). The existing `want` slice (lines 288-297) gains two entries:

```go
"llama-swap health",
"component__dot.is-danger",
```

And after the existing `https://` / `http://` negative checks (lines 302-307),
add a negative check that the old confusing label is gone:

```go
if strings.Contains(body, "Health check") {
    t.Fatalf("expected body to no longer contain the confusing label \"Health check\", got %q", body)
}
```

These three assertions guard the contract: the new label is present, the old
label is absent, and the red-dot CSS rule is embedded. The existing assertions
(`DockMind`, `/status`, `/power/on`, `/power/off`, `/restart`, `/docs`, `fetch`,
`setInterval`, no `https://`, no `http://`) all remain satisfied — the new label
contains no URL and no protocol scheme.

## Tasks

### Task 1 - Relabel health row, fix value/dot semantics, add contract tests

- `GET /` served body + substring check
  - → body contains the substring `llama-swap health` (new label present)
- `GET /` served body + substring check
  - → body does NOT contain the substring `Health check` (old confusing label removed)
- `GET /` served body + substring check
  - → body contains the substring `component__dot.is-danger` (red-dot CSS rule embedded)
- existing `TestWebUIRoutes` assertions unchanged + `make test` from repo root
  - → all tests pass (existing web UI substring assertions still hold: `DockMind`, `/status`, `/power/on`, `/power/off`, `/restart`, `/docs`, `fetch`, `setInterval`; body contains no `https://` and no `http://`; `POST /` → 405; `GET /foo` → 404; regression `GET /status` → 200 body contains `"state"`, `GET /health` → 200 empty body, `GET /docs` → 200 body contains `swagger-ui`)
- `make lint` from repo root
  - → `gofmt -l .` prints no file paths and `go vet ./...` reports no issues

## Technical Context

- **`llamaSwapHealthy` vs `/health` — the conceptual split.** `llamaSwapHealthy`
  (`state.StatusResponse`, `internal/state/state.go:71`) is a live probe of
  llama-swap's `/v1/models` endpoint via `internal/health/health.go`
  `Client.Check` (returns `true` only on HTTP 200), surfaced through
  `state.Machine.Status` → `probeBool("Health", m.health.Check)`. The daemon's
  `GET /health` (`internal/api/api.go:90`) unconditionally returns 200 and means
  "daemon is up" — explicitly "not GPU readiness" per `openapi.json:81`,
  `README.md:50`, and `docs/DockMind_MVP_Specification.md:184-188`. The UI row
  was labeled "Health check" (suggesting the `/health` endpoint) but wired to
  `llamaSwapHealthy` (the inference-backend probe) — the mismatch.
- **`llamaSwapRunning` is the right discriminator for the stopped case.**
  `llamaSwapRunning` (`state.go:70`) is a live probe of the Docker container
  (`docker.IsRunning`). When the system is Off, the container is intentionally
  stopped, so `llamaSwapRunning=false`. Using `!llamaSwapRunning` as the first
  predicate branch renders "Stopped" (neutral) instead of "Unhealthy" (fault),
  eliminating the false alarm the user saw.
- **No new dependencies.** `index.html` is embedded via `//go:embed`
  (`internal/api/api.go:18-19`); `embed` is stdlib and already imported. The CSS
  uses the existing `--danger` token. `go.mod` is unchanged.
- **No JS test harness (story 004 precedent).** The repo is stdlib-only with no
  headless-browser or JS test infrastructure. Go-side tests verify the served
  HTML contract via substring assertions on `GET /`; the runtime three-state
  render behavior is verified manually (see Notes).

## Notes

- **Manual verification (non-automatable, per story 004 convention).** Open
  `http://<host>:<port>/` in a browser and confirm the "llama-swap health" row
  across the three states:
  - System **Off** (llama-swap stopped): row shows **"Stopped"** with a **gray**
    dot — not "Unhealthy". This is the user's reported bug scenario.
  - System **Ready** (llama-swap running and `/v1/models` returning 200): row
    shows **"Healthy"** with a **green** dot.
  - llama-swap **running but unhealthy** (container up, `/v1/models` not 200):
    row shows **"Unhealthy"** with a **red** dot — a genuine fault, distinguishable
    from the intentional-stopped case.
- **Why the label "llama-swap health" and not "Health check".** "Health check"
  implied the daemon `/health` endpoint. "llama-swap health" ties the row
  unambiguously to the llama-swap inference backend (mirroring the existing
  "llama-swap" running-status row directly above it) and to the `llamaSwapHealthy`
  field that drives it.
- **Why "Stopped" (not "—" / "N/A") when not running.** "Stopped" matches the
  wording the existing "llama-swap" running row already uses for the same
  condition (`data.llamaSwapRunning ? "Running" : "Stopped"`, line 486), so the
  two rows stay consistent and the operator reads a familiar, non-alarming
  state.
- **Dot color convention preserved.** Gray = neutral/off (non-fault); green =
  healthy/running; red = real fault (running but unhealthy). The overall Error
  state is still conveyed by the state badge and the last-error banner, not by
  this single dot.
