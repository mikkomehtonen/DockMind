# Responsive Mobile-First Web UI for DockMind

## Context

DockMind exposes a stable 5-endpoint REST API (`/status`, `/power/on`,
`/power/off`, `/restart`, `/health`) and an interactive Swagger UI at `/docs`,
but there is no browser interface for an operator to see the system state at a
glance and toggle the eGPU inference server. Today a homelab operator must curl
the API or read raw JSON. This story adds a single-page, mobile-first control
panel served by the daemon itself: it polls `/status` once per second and
exposes Power On / Power Off / Restart actions whose availability mirrors the
state machine exactly. It is plain HTML + CSS + vanilla JavaScript with no
frontend framework and no external/CDN requests, so it works on an isolated
trusted LAN (the same constraint the Shelly integration already enforces — no
cloud connectivity required).

The UI is designed with the `impeccable` skill's **product register** (design
serves the product: a tool/dashboard where the interface disappears into the
task) and a **dark, Restrained** visual direction.

## Out of Scope

- Any frontend framework, build step, bundler, or npm dependency. The UI is one
  self-contained `index.html` with inline `<style>` and `<script>`.
- External/CDN assets. The page must load with zero network requests beyond the
  daemon itself (no web fonts, no icon CDN, no JS libraries). This is stricter
  than the Swagger UI page (`/docs`), which loads from `unpkg.com` — the control
  panel must work on an air-gapped LAN.
- Authentication. The UI has no login; it is served on the same trusted LAN as
  the existing unauthenticated REST API (consistent with the existing non-goal).
- WebSocket/SSE or server-side push. The daemon still does no background
  polling; the *client* polls `/status` once per second. Each poll is an
  ordinary request to which the daemon responds live, so the existing non-goal
  ("Background polling or push updates (WebSocket/SSE). Status is checked live
  only on request") still holds.
- Separate `app.css` / `app.js` asset files and routes. Everything is inline in
  one embedded HTML file served at `GET /`.
- Browser-side unit tests or a headless-browser harness. The repo is stdlib-only
  with no JS test infrastructure; browser runtime behavior is verified manually
  (see Notes). Go-side tests verify the served HTML contract.

## Implementation approach

### Files and placement

One new file lives in `internal/api/` (the package that owns the mux and already
embeds `docs.html` + `openapi.json`), embedded at compile time:

- `internal/api/index.html` — the self-contained SPA (inline CSS + inline JS,
  no external requests).

No new Go files. No new directories. No new dependencies — `go.mod` stays at
`gopkg.in/yaml.v3 v3.0.1` only; `embed` is stdlib and already imported in
`internal/api/api.go` (`import _ "embed"` on line 4).

### Embedding and route registration

Add one embedded variable in `internal/api/api.go`, immediately after the
existing `docsHTML` embed, following the exact same pattern as story 003:

```go
//go:embed index.html
var indexHTML []byte
```

`//go:embed` with `[]byte` requires `import _ "embed"`, which is already
present. Add one route to the existing `http.ServeMux` in `Server.Handler()`,
using the Go 1.22+ method-prefixed pattern with the `{$}` exact-root anchor:

```go
mux.HandleFunc("GET /{$}", s.handleIndex)
```

**Why `{$}` and not `"GET /"`:** a bare `"GET /"` pattern matches every path
that starts with `/` (i.e. everything) and would shadow `/status`, `/power/on`,
etc. The `{$}` wildcard restricts the pattern to the exact root path only.
Verified against Go 1.24.4: with `"GET /{$}"` registered, `GET /` → 200, `POST /`
→ 405 (wrong method, native mux behavior), `GET /foo` → 404 (unknown path),
`GET /status` → 200 (existing route unaffected), `GET /power/on` (wrong method)
→ 405. This preserves every existing `TestRoutes` / `TestSwaggerRoutes`
assertion with zero regression.

### Handler implementation

```go
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write(indexHTML)
}
```

Identical structure to `handleDocs` / `handleOpenAPI`: embedded bytes are
in-memory, so `w.Write` can only fail on client disconnect; the error is ignored
(consistent with the existing handlers, which only log `json.Encode` errors).

### Design system (impeccable, product register, dark + Restrained)

**Scene sentence (forces dark):** a homelab operator glances at their phone,
briefly, in varying ambient light — sometimes a dim room at night — to check
whether the eGPU inference server is up and to toggle it. Dark is correct
(server-dashboard convention: Grafana, Portainer, Proxmox; comfortable in low
light).

**Color strategy:** Restrained — tinted neutrals carry the surface; a single
green primary (the `impeccable` `palette.mjs` seed, hue 150°) is reserved for
the Ready state and the Power On action; semantic amber/red appear only as state
indicators and the Power Off danger action. No decorative color.

**Palette (OKLCH, defined as CSS custom properties on `:root`):**

| Token            | Value                          | Role                                              |
|------------------|--------------------------------|---------------------------------------------------|
| `--bg`           | `oklch(0.10 0 0)`              | Page background — near-black, zero chroma         |
| `--surface`      | `oklch(0.155 0.004 150)`       | Panels/cards, faint green tint toward brand       |
| `--surface-2`    | `oklch(0.205 0.005 150)`       | Hover/active lift on interactive surfaces         |
| `--ink`          | `oklch(0.93 0.006 150)`        | Body text — near-white, ≥7:1 vs bg                |
| `--muted`        | `oklch(0.62 0.012 150)`        | Secondary text/labels — ≥3.5:1 vs bg              |
| `--border`       | `oklch(0.24 0.005 150)`        | Hairline borders                                  |
| `--primary`      | `oklch(0.62 0.145 150)`        | Green — Ready state + Power On button bg          |
| `--primary-hover`| `oklch(0.66 0.155 150)`        | Power On hover                                    |
| `--danger`       | `oklch(0.62 0.18 25)`          | Red — Error state + Power Off button bg           |
| `--danger-hover` | `oklch(0.66 0.19 25)`          | Power Off hover                                   |
| `--busy`         | `oklch(0.78 0.15 70)`          | Amber — Starting / ShuttingDown state             |
| `--neutral`      | `oklch(0.62 0.012 150)`        | Off state (same family as muted)                  |
| `--radius`       | `10px`                         | Cards/panels/buttons (≤16px per product register) |

Button label text is white and bold (≥14px) so it meets the ≥3:1 large-text
threshold against the green/red button backgrounds. If the implementer finds
white-on-`--primary` short of 3:1, lower `--primary` L toward 0.58 — do not
raise it. Disabled buttons use `--surface` bg + `--muted` text + reduced opacity
+ `cursor: not-allowed` + the native `disabled` attribute (no saturated color on
inactive states — a product-register ban).

**Typography (product register: one family, fixed rem scale, 1.125–1.2 ratio):**
system font stack (`-apple-system, BlinkMacSystemFont, "Segoe UI", Roboto,
"Helvetica Neue", Arial, sans-serif`), no web fonts. Fixed scale: `--fs-sm`
0.8125rem (13px, labels), `--fs-base` 0.9375rem (15px, values/body),
`--fs-md` 1rem (16px, button labels, state label), `--fs-lg` 1.25rem (20px,
section headings), `--fs-state` 1.5rem (24px, the state name — prominent but not
a landing-page hero). No fluid `clamp()` on headings (product register: fixed
rem, not fluid). Body line-length cap does not apply (this is a compact control
panel, not prose).

**Layout (mobile-first, single column):** a centered container, `max-width:
30rem` (480px), horizontal padding `1rem`, stacked vertically with `1.25rem`
gaps. Flexbox column for the page; each section is a block. Buttons are
full-width, stacked. Component rows are full-width flex rows (label left, value
+ dot right). On wider viewports the column simply stays centered and capped —
no breakpoint needed for a 4-row component list. `box-sizing: border-box` on
everything.

### Page structure (DOM)

```html
<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>DockMind</title>
  <style> … inline CSS … </style>
</head>
<body>
  <main class="app">
    <header class="app__header">
      <h1 class="app__title">DockMind</h1>
      <p class="app__connection" id="connection">
        <span class="conn__dot" id="conn-dot"></span>
        <span id="conn-text">Connecting…</span>
      </p>
    </header>

    <section class="state" aria-live="polite">
      <span class="state__label">System state</span>
      <div class="state__badge" id="state-badge">
        <span class="state__dot" id="state-dot"></span>
        <span class="state__name" id="state-name">Loading…</span>
      </div>
      <p class="state__feedback" id="feedback" hidden></p>
    </section>

    <section class="actions">
      <button class="btn btn--primary" id="btn-on"      data-action="on"      disabled>Power On</button>
      <button class="btn btn--danger"  id="btn-off"     data-action="off"     disabled>Power Off</button>
      <button class="btn btn--ghost"   id="btn-restart" data-action="restart" disabled>Restart</button>
    </section>

    <section class="components">
      <h2 class="components__heading">Components</h2>
      <div class="component">
        <span class="component__dot" id="gpu-dot"></span>
        <span class="component__label">GPU</span>
        <span class="component__value" id="gpu-value">—</span>
      </div>
      <div class="component">
        <span class="component__dot" id="shelly-dot"></span>
        <span class="component__label">Shelly Plug</span>
        <span class="component__value" id="shelly-value">—</span>
      </div>
      <div class="component">
        <span class="component__dot" id="swap-dot"></span>
        <span class="component__label">llama-swap</span>
        <span class="component__value" id="swap-value">—</span>
      </div>
      <div class="component">
        <span class="component__dot" id="health-dot"></span>
        <span class="component__label">Health check</span>
        <span class="component__value" id="health-value">—</span>
      </div>
    </section>

    <section class="error" id="error-banner" hidden>
      <p class="error__label">Last error</p>
      <p class="error__message" id="error-message"></p>
    </section>

    <footer class="app__footer">
      <a href="/docs">API Docs</a>
    </footer>
  </main>
  <script> … inline JS … </script>
</body>
</html>
```

The component rows and error banner are static HTML with stable IDs; JS updates
their text and class names in place (no innerHTML reconstruction, preserving
accessibility tree and avoiding reflow thrash).

### JavaScript behavior

**Polling.** On load, call `fetchStatus()` once, then `setInterval(fetchStatus,
1000)`. Guard against request pile-up (the spec warns `/status` latency scales
with hardware round-trips): a module-level `pending` boolean; if a fetch is in
flight, the next tick is skipped. Each fetch uses `AbortController` with a 5s
timeout so a hung request cannot block the guard forever.

```js
const POLL_MS = 1000;
const FETCH_TIMEOUT_MS = 5000;
let pending = false;

async function fetchStatus() {
  if (pending) return;
  pending = true;
  const ctrl = new AbortController();
  const timer = setTimeout(() => ctrl.abort(), FETCH_TIMEOUT_MS);
  try {
    const res = await fetch("/status", { signal: ctrl.signal });
    if (!res.ok) throw new Error("status " + res.status);
    const data = await res.json();
    render(data);
    setConnected(true);
  } catch (e) {
    setConnected(false);
  } finally {
    clearTimeout(timer);
    pending = false;
  }
}
setInterval(fetchStatus, POLL_MS);
fetchStatus();
```

**Rendering (`render(data)`).** `data` is the `StatusResponse` JSON with fields
`state`, `gpuPresent`, `gpuName`, `shellyOn`, `llamaSwapRunning`,
`llamaSwapHealthy`, `lastError` (exact JSON tags from `state.StatusResponse`).

- State badge: set `#state-name` text to `data.state`; set `#state-badge` and
  `#state-dot` color class from a state→color map:
  `Off`→`neutral`, `Starting`→`busy`, `Ready`→`primary`, `ShuttingDown`→`busy`,
  `Error`→`danger`. Any unrecognized state string → `neutral` (defensive).
- GPU row: `#gpu-value` = `data.gpuName` if `data.gpuPresent && data.gpuName`;
  else `"Detected"` if `data.gpuPresent`; else `"Not detected"`. `#gpu-dot`
  green class if `data.gpuPresent`, else off.
- Shelly row: `#shelly-value` = `"On"` / `"Off"`. Dot green if `data.shellyOn`.
- llama-swap row: `#swap-value` = `"Running"` / `"Stopped"`. Dot green if
  `data.llamaSwapRunning`.
- Health row: `#health-value` = `"Healthy"` / `"Unhealthy"`. Dot green if
  `data.llamaSwapHealthy`.
- Error banner: if `data.lastError` is a non-empty string, show `#error-banner`
  (`hidden` removed) and set `#error-message` text to `data.lastError`;
  otherwise hide it. (`lastError` is `null` when not in Error state per
  `state.go`.)
- Buttons: enable/disable per the mapping table below.

Component dots use two classes only: `is-on` (green via `--primary`) and the
base (neutral/off). Green = the raw boolean is true; gray = false. Gray (not
red) for false booleans avoids false alarms — the overall Error state is
conveyed by the state badge and error banner, not by every component dot.

**State → button-enabled mapping (derived exactly from `state.go` transition
rules — enable only when the call returns 202 Accepted; disable for 200
already-in-state and 409 conflict):**

| State         | Power On | Power Off | Restart |
|---------------|----------|-----------|---------|
| `Off`         | enabled  | disabled  | enabled |
| `Starting`    | disabled | disabled  | disabled|
| `Ready`       | disabled | enabled   | enabled |
| `ShuttingDown`| disabled | disabled  | disabled|
| `Error`       | disabled | enabled   | disabled|

Encoded as a lookup object:

```js
const ENABLED = {
  Off:         { on: true,  off: false, restart: true  },
  Starting:    { on: false, off: false, restart: false },
  Ready:       { on: false, off: true,  restart: true  },
  ShuttingDown:{ on: false, off: false, restart: false },
  Error:       { on: false, off: true,  restart: false },
};
```

Rationale, traced to `state.go`: `PowerOn` returns `ResultAccepted` (202) only
from `Off`; `ResultAlreadyInState` (200) from `Ready`; `ResultConflict` (409)
from `Starting`/`ShuttingDown`/`Error`. `PowerOff` returns 202 only from
`Ready`/`Error`; 200 from `Off`; 409 from `Starting`/`ShuttingDown`. `Restart`
returns 202 only from `Off`/`Ready`; 409 from `Starting`/`ShuttingDown`/`Error`.
The UI enables a button only when it would trigger a real transition (202).

**Action click handler.** Each button has `data-action` ∈ `on`/`off`/`restart`,
mapping to endpoint `/power/on`, `/power/off`, `/restart` (POST, empty body).

```js
async function doAction(endpoint, label) {
  disableAllButtons();
  showFeedback("…"); // see below
  try {
    const res = await fetch(endpoint, { method: "POST" });
    if (res.status === 202)      showFeedback(label + "…");
    else if (res.status === 200) showFeedback("Already in target state");
    else if (res.status === 409) showFeedback("Not allowed right now");
    else                         showFeedback("Unexpected response");
  } catch (e) {
    showFeedback("Request failed");
  }
  // Do NOT re-enable here. The next successful status poll re-enables the
  // buttons appropriate to the new state. This avoids races and double-taps.
}
```

On click: immediately disable all three buttons (prevents double-tap during the
in-flight POST and the subsequent async transition). After the POST resolves,
show transient feedback in `#feedback` but leave buttons disabled until the next
successful `fetchStatus` poll re-enables them per the mapping. If the connection
is down, buttons stay disabled (you cannot act while disconnected); on reconnect
the poll re-enables them. The feedback element auto-clears after 2.5s via
`setTimeout` and is also cleared on each successful render.

**Connection indicator (`setConnected(bool)`).** `#conn-dot` green + `#conn-text`
`"Live"` when the last fetch succeeded; `#conn-dot` amber (`--busy`) +
`#conn-text` `"Disconnected — retrying"` when it failed. On a failed poll, the
last successfully-rendered status stays visible (do not blank the state or
components) — only the connection indicator changes. Before the first successful
poll, the state badge reads `"Loading…"` (neutral) and all buttons are disabled.

### Accessibility and motion

- All controls are native `<button>` elements with `disabled` attributes (product
  register: do not reinvent standard affordances). `:focus-visible` outlines for
  keyboard navigation.
- State never relies on color alone: every component shows a text value
  (`"On"`/`"Off"`, `"Healthy"`/`"Unhealthy"`, etc.) alongside the dot; the state
  badge shows the state name. Color is supplementary.
- `aria-live="polite"` on the state section so screen readers announce state
  changes without interrupting.
- `<meta name="viewport" content="width=device-width, initial-scale=1">` for
  responsive mobile rendering.
- Motion: 150–200ms transitions on state-badge color and button hover/active
  (product register: 150–250ms, motion conveys state not decoration). The
  `Starting`/`ShuttingDown` state dot gets a subtle pulse animation (opacity
  0.45↔1, 1.6s ease-in-out infinite) to convey "working." A
  `@media (prefers-reduced-motion: reduce)` block disables the pulse and reduces
  transitions to instant/color-only (reduced motion is not optional per the
  skill). No page-load orchestration; no decorative motion.

### README update

Add one row to the API Endpoints table in `README.md`, after the `/docs` row:

```
| GET | `/` | Responsive web UI for monitoring and controlling DockMind |
```

No other README sections change. The existing `readme_test.go` ACs all remain
satisfied — the new row only adds content. The first fenced ```yaml block is
untouched.

### `readme_test.go` update

Add one case to the `cases` slice in `TestREADME`:

```go
{"documents web UI route", "web UI", true},
```

The current README does not contain the substring `web UI` (verified: only
`Swagger UI` appears, in the `/docs` row), so this check is meaningful only
after the table row is added. The substring `web UI` (lowercase w) matches the
new row's description "Responsive web UI for monitoring and controlling
DockMind".

### `docs/product.md` update

`docs/product.md` currently lists "Web UI" as a Non-Goal (line 29: `- Web UI,
Prometheus metrics, or request queuing during startup.`). Shipping a web UI
contradicts that, so it must be removed:

- Non-Goals line becomes: `- Prometheus metrics, or request queuing during
  startup.`
- Add a Features entry after the Swagger UI line, matching the existing entry
  format (bold name, em-dash, description, story link):
  `- **Web UI** — responsive mobile-first control panel served at \`/\`, polling \`/status\` once per second, with power on/off and restart controls ([004-web-ui](../stories/004-web-ui/story.md))`

The other Non-Goals and Known Limitations still hold: no auth (the UI has no
login), no daemon-side background polling (the client polls, which is still
"on request"). `docs/DockMind_MVP_Specification.md` lists "Web UI" under Future
Extensions; it is a historical spec document and is left unchanged (story 003
also did not modify it).

### `product_test.go` (new, repo root)

Add a new root-level test file `product_test.go` (package `dockmind_test`,
analogous to `readme_test.go`) that enforces the product.md contract change so
it cannot silently regress:

```go
package dockmind_test

import (
	"os"
	"strings"
	"testing"
)

func TestProductDoc(t *testing.T) {
	data, err := os.ReadFile("docs/product.md")
	if err != nil {
		t.Fatalf("failed to read docs/product.md: %v", err)
	}
	body := string(data)

	if !strings.Contains(body, "004-web-ui") {
		t.Error("docs/product.md Features list does not reference the 004-web-ui story")
	}
	if strings.Contains(body, "Web UI, Prometheus metrics, or request queuing during startup") {
		t.Error("docs/product.md still lists Web UI as a non-goal")
	}
}
```

It uses only stdlib (`os`, `strings`, `testing`) — no import of `internal/`
needed, so it compiles cleanly in the root test package.

### Test approach (Go-side)

Add `TestWebUIRoutes` to `internal/api/api_test.go` using the existing
`httptest.NewRecorder` + `NewServer(fake, nil)` pattern. The `/` route does not
depend on the state machine, so the fake can be zero-valued. Assertions:

- `GET /` → 200, `Content-Type` contains `text/html`, body contains `DockMind`,
  `/status`, `/power/on`, `/power/off`, `/restart`, `/docs`, `fetch`,
  `setInterval` (confirms the SPA, its API targets, and the polling mechanism
  are embedded), and body does NOT contain `https://` or `http://` (confirms
  self-contained, no CDN/external requests — all URLs are relative).
- `POST /` → 405 (wrong method, native mux via `{$}`).
- `GET /foo` → 404 (unknown path, regression — `{$}` does not shadow unknown
  paths).
- Regression: `GET /status` → 200 body contains `"state"`; `GET /health` → 200
  empty body; `GET /docs` → 200 body contains `swagger-ui`.

## Tasks

### Task 1 - Web UI page, embed, route, and Go-side tests

- `GET /` request
  - → HTTP 200
  - → `Content-Type` response header contains `text/html`
  - → response body contains the substring `DockMind`
  - → response body contains the substring `/status`
  - → response body contains the substring `/power/on`
  - → response body contains the substring `/power/off`
  - → response body contains the substring `/restart`
  - → response body contains the substring `/docs`
  - → response body contains the substring `fetch`
  - → response body contains the substring `setInterval`
  - → response body does NOT contain the substring `https://`
  - → response body does NOT contain the substring `http://`
- `POST /` (wrong method on the root path)
  - → HTTP 405
- `GET /foo` (unknown path, regression check after adding the root route)
  - → HTTP 404
- existing routes regression + `GET /status`
  - → HTTP 200 and body contains `"state"`
- existing routes regression + `GET /health`
  - → HTTP 200 and body is empty
- existing routes regression + `GET /docs`
  - → HTTP 200 and body contains `swagger-ui`
- `make test` from repo root
  - → all tests pass (existing + new web UI route tests + product_test)
- `make lint` from repo root
  - → `gofmt -l .` and `go vet ./...` report no issues

### Task 2 - README and product.md updates with automated validation

- README API Endpoints table updated with the web UI row + `readme_test.go` updated with `web UI` presence check + `make test`
  - → `readme_test.go` passes (README contains the substring `web UI`)
- existing `readme_test.go` ACs still satisfied + `make test`
  - → README still contains all 5 original routes (`/status`, `/power/on`, `/power/off`, `/restart`, `/health`), `/docs`, all 7 field names (`state`, `gpuPresent`, `gpuName`, `shellyOn`, `llamaSwapRunning`, `llamaSwapHealthy`, `lastError`), `make build`, `make test`, `make lint`, `--config`, `./config.yaml`, and links to both `docs/DockMind_MVP_Specification.md` and `docs/product.md`
  - → README still does NOT contain `ResultAlreadyInState` or `ResultConflict`
  - → README still has no License section
  - → first fenced ```yaml block still loads successfully via `config.Load`
- `docs/product.md` Non-Goals updated (Web UI removed) + Features entry added referencing `004-web-ui` + new `product_test.go` + `make test`
  - → `product_test.go` passes (product.md contains `004-web-ui` and does NOT contain `Web UI, Prometheus metrics, or request queuing during startup`)

## Technical Context

- **Go 1.24.4** — `embed` is stdlib (stable since Go 1.16); `import _ "embed"`
  is already present in `internal/api/api.go`. No new external Go dependencies;
  `go.mod` remains `gopkg.in/yaml.v3 v3.0.1` only.
- **`http.ServeMux` `{$}` exact-root pattern** — Go 1.22+ method-prefixed
  patterns. `"GET /{$}"` matches only `GET /` without shadowing sub-paths.
  Verified against Go 1.24.4: preserves native 405 on wrong method and 404 on
  unknown paths, leaving all existing routes and their tests intact.
- **No frontend framework, no npm, no CDN** — plain HTML + CSS + vanilla JS in a
  single `index.html`, embedded via `//go:embed`. Zero external requests so the
  UI works on an isolated trusted LAN. This is intentionally stricter than the
  `/docs` Swagger UI page (which loads from `unpkg.com`).
- **impeccable skill, product register** — design serves the product (a
  tool/dashboard). Dark theme, Restrained color strategy, one system-font
  family, fixed rem scale, native affordances, 150–200ms state-conveying motion,
  `prefers-reduced-motion` support. The green primary is anchored to the
  `palette.mjs` seed (hue 150°); the full OKLCH palette is specified in the
  Implementation approach.
- **`StatusResponse` JSON contract** — fields `state` (string: `Off`/`Starting`/
  `Ready`/`ShuttingDown`/`Error`), `gpuPresent` (bool), `gpuName` (string),
  `shellyOn` (bool), `llamaSwapRunning` (bool), `llamaSwapHealthy` (bool),
  `lastError` (string or `null`). Exact JSON tags from `state.StatusResponse` in
  `internal/state/state.go`. POST endpoints return empty bodies with status 202
  / 200 / 409.
- **State → button mapping** — derived exactly from `state.go` `PowerOn` /
  `PowerOff` / `Restart` transition tables; see the mapping table in the
  Implementation approach.

## Notes

- **Browser runtime behavior is verified manually, not by Go tests.** The repo
  is stdlib-only with no headless-browser or JS test harness. Go-side tests
  verify the served HTML contract (correct endpoints, polling code, no external
  requests, route status codes). The following are non-automatable and must be
  checked by opening `http://<host>:8080/` in a mobile-width browser:
  - polls `/status` once per second and updates the state badge, component rows,
    and button states;
  - Power On / Power Off / Restart are enabled/disabled per the state→button
    mapping table;
  - clicking an enabled button POSTs and shows transient feedback (202/200/409);
  - disconnecting the daemon turns the connection indicator amber and preserves
    the last known status;
  - the `Error` state shows the `lastError` banner;
  - layout is usable at 360px viewport width;
  - `prefers-reduced-motion` disables the busy-state pulse.
- **Client polling vs. the daemon non-goal.** The product non-goal "Background
  polling or push updates (WebSocket/SSE)" refers to the *daemon* not polling or
  pushing. The web UI polls `/status` from the browser; each poll is an ordinary
  HTTP request answered live by the daemon. This does not violate the non-goal,
  and `docs/product.md`'s Non-Goals list is otherwise unchanged.
- **No auth (MVP).** The UI has no login, consistent with the existing
  "Authenticate users" non-goal and the "No authentication on the REST API"
  known limitation. It is intended for a trusted local network only.
- **Self-contained / offline.** The page must not reference any `https://` or
  `http://` URL (no web fonts, no icon CDN, no JS libraries). Icons, if any, are
  inline SVG or CSS-drawn; the Go test enforces the absence of both. This is the
  key difference from the `/docs` Swagger UI page and is required for air-gapped
  homelab use.
- **Recommended impeccable references during implementation:** `reference/product.md`
  (register — already loaded), `reference/layout.md` (spacing/rhythm), `reference/animate.md`
  (the busy-state pulse + reduced-motion), `reference/harden.md` (connection-error
  and edge states), `reference/adapt.md` (mobile responsive behavior).
- **`{$}` route ordering.** The new `GET /{$}` route may be registered in any
  position relative to the existing routes in `Server.Handler()`; Go 1.22+
  ServeMux resolves by specificity, and `{$}` is exact-root only, so it never
  conflicts with `/status`, `/power/on`, `/power/off`, `/restart`, `/health`,
  `/docs`, or `/openapi.json`.
