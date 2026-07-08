# Add SVG Favicon and Configurable Logo Link to the Web UI

## Context

The DockMind web UI (`internal/api/index.html`, served at `GET /`) has no
favicon — browsers show a generic default icon in the tab/bookmark bar. The
page header shows the title "DockMind" as plain text with no brand mark. This
story adds an SVG favicon (served at `GET /favicon.svg`) and displays the same
icon as a logo to the left of the page title. The logo is optionally a
hyperlink: when the `LOGO_LINK_URL` environment variable is set to a non-empty
value, the logo image is wrapped in an `<a>` pointing to that URL; when it is
unset (or empty), the logo is a plain image with no link.

The SVG asset (`dockmind.svg`, ~54 KB, viewBox `0 0 1024 1024`, valid XML) was
placed at the project root by the user. It is moved into `internal/api/` (the
package that owns the mux and already embeds `index.html`, `docs.html`, and
`openapi.json`) and renamed `favicon.svg`, following the existing `//go:embed`
pattern.

## Out of Scope

- Any change to the REST API contract (`/status`, `/power/on`, `/power/off`,
  `/restart`, `/health`), the `StatusResponse` struct, the OpenAPI spec
  (`openapi.json`), or the state machine. The favicon is a static asset, not an
  API endpoint, and is not added to `openapi.json` — consistent with how `/`
  and `/docs` are already absent from the spec.
- Any frontend framework, build step, or npm dependency. The favicon is an
  embedded SVG served directly; the logo is a static `<img>` (or `<a><img>`)
  in the existing inline HTML. No external/CDN requests — the air-gapped LAN
  constraint from story 004 is preserved.
- Config-file (`config.yaml`) support for the logo link URL. The link target is
  controlled exclusively via the `LOGO_LINK_URL` environment variable, not the
  YAML schema. This keeps the config struct and loader unchanged for a
  deployment-time branding setting.
- A separate favicon asset or route for the Swagger UI page (`/docs`). The same
  `<link rel="icon">` tag is added to `docs.html` for consistency, but no
  second asset or route is created.
- Browser-side testing of favicon/logo rendering. The repo is stdlib-only with
  no headless-browser harness (story 004 precedent). Go-side tests verify the
  route, content type, and HTML contract; visual rendering is verified manually
  (see Notes).

## Implementation approach

### File placement and embedding

Move `dockmind.svg` from the project root to `internal/api/favicon.svg` and
delete the original at the root. Add one embedded variable in
`internal/api/api.go`, immediately after the existing `indexHTML` embed
(lines 18–19), following the same pattern:

```go
//go:embed favicon.svg
var faviconSVG []byte
```

`//go:embed` with `[]byte` requires `import _ "embed"`, already present (line
4). No new external dependencies; `go.mod` stays at `gopkg.in/yaml.v3 v3.0.1`
only. New stdlib imports in `api.go`: `bytes`, `html`, `os`.

### Favicon route

Add one route to the existing `http.ServeMux` in `Server.Handler()`, after the
`GET /openapi.json` route (line 51):

```go
mux.HandleFunc("GET /favicon.svg", s.handleFavicon)
```

Handler:

```go
func (s *Server) handleFavicon(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "image/svg+xml")
	w.WriteHeader(http.StatusOK)
	w.Write(faviconSVG)
}
```

`image/svg+xml` is the IANA-registered MIME type for SVG. The embedded bytes
are in-memory, so `w.Write` can only fail on client disconnect; the error is
ignored, consistent with `handleDocs` / `handleOpenAPI` / `handleIndex`.

### Logo link URL injection (environment variable)

Add an `indexHTMLRendered []byte` field to the `Server` struct. In
`NewServer`, read `LOGO_LINK_URL` from the environment once at startup and
pre-compute the HTML:

```go
func NewServer(machine StateMachine, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	s := &Server{
		machine: machine,
		logger:  logger,
	}
	s.indexHTMLRendered = indexHTML
	if logoURL := os.Getenv("LOGO_LINK_URL"); logoURL != "" {
		const logoImg = `<img src="/favicon.svg" alt="DockMind" class="app__logo" width="24" height="24">`
		escaped := html.EscapeString(logoURL)
		s.indexHTMLRendered = bytes.Replace(
			indexHTML,
			[]byte(logoImg),
			[]byte(`<a href="`+escaped+`" class="app__logo-link">`+logoImg+`</a>`),
			1,
		)
	}
	return s
}
```

**Why pre-compute at startup, not per-request:** the env var is a
deployment-time setting (like the config file), not a per-request value.
Reading it once in `NewServer` and doing the `bytes.Replace` once is efficient
and keeps `handleIndex` a single `w.Write`. `bytes.Replace` with `n=1` returns
a new slice when a replacement is made (no mutation of the package-level
`indexHTML`); when `LOGO_LINK_URL` is unset/empty, `indexHTMLRendered` is the
original `indexHTML` (no copy, no replacement).

**Why `html.EscapeString`:** the URL is injected into an `href` attribute.
Escaping `&`, `<`, `>`, `"`, `'` prevents attribute breakout if the URL
contains those characters (e.g. query-string `&` separators). This is
defense-in-depth even though the env var is operator-controlled on a trusted
LAN.

**Empty vs. unset:** `os.Getenv` returns `""` for both an unset variable and
one explicitly set to empty. Both produce the no-link HTML (the
`if logoURL != ""` guard handles this). An empty `LOGO_LINK_URL` means "no
link."

`handleIndex` changes from writing the package-level `indexHTML` to writing
the pre-computed `s.indexHTMLRendered`:

```go
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write(s.indexHTMLRendered)
}
```

### HTML changes (`internal/api/index.html`)

**1. Favicon link in `<head>`** — add after the `<title>` element (line 6):

```html
<link rel="icon" href="/favicon.svg" type="image/svg+xml">
```

**2. Logo in the header** — replace the existing header (lines 328–334):

```html
<header class="app__header">
  <div class="app__brand">
    <img src="/favicon.svg" alt="DockMind" class="app__logo" width="24" height="24">
    <h1 class="app__title">DockMind</h1>
  </div>
  <p class="app__connection" id="connection">
    <span class="conn__dot" id="conn-dot"></span>
    <span id="conn-text">Connecting…</span>
  </p>
</header>
```

The `<img>` tag string must match the `logoImg` constant in `api.go` exactly
(including attribute order and spacing) so `bytes.Replace` finds it. The
`width="24" height="24"` attributes reserve layout space to prevent CLS before
the SVG loads; CSS overrides the display size.

**3. CSS** — add after the `.app__title` rule (line 68):

```css
.app__brand {
  display: flex;
  align-items: center;
  gap: 0.5rem;
}

.app__logo {
  width: 1.5rem;
  height: 1.5rem;
  flex-shrink: 0;
}

.app__logo-link {
  display: inline-flex;
  line-height: 0;
  text-decoration: none;
}
```

`.app__brand` groups the logo and title as a single flex child of
`.app__header` (which uses `justify-content: space-between`), keeping the
connection indicator on the right. `.app__logo` sets the display size to
1.5rem (24px), matching the `width`/`height` attributes. `.app__logo-link`
eliminates extra inline space around the image and removes the default
underline when the logo is a link. No new color tokens; no transitions on the
logo (it is a static brand element, consistent with the product register).

**4. Favicon link in `docs.html`** — add the same `<link>` tag to the `<head>`
of `internal/api/docs.html` (after the `<title>` element, line 5) so the
Swagger UI page also shows the favicon:

```html
<link rel="icon" href="/favicon.svg" type="image/svg+xml">
```

### Test approach (Go-side, `internal/api/api_test.go`)

**Favicon route tests** — add to `TestWebUIRoutes`:

```go
t.Run("GET /favicon.svg", func(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/favicon.svg", nil)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, rec.Code)
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.Contains(ct, "image/svg+xml") {
		t.Fatalf("expected Content-Type to contain image/svg+xml, got %q", ct)
	}
	if !strings.Contains(rec.Body.String(), "<svg") {
		t.Fatalf("expected body to contain <svg")
	}
})

t.Run("POST /favicon.svg wrong method", func(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/favicon.svg", nil)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected status %d, got %d", http.StatusMethodNotAllowed, rec.Code)
	}
})
```

**Logo assertions in the existing `GET /` subtest** — add
`t.Setenv("LOGO_LINK_URL", "")` before the `server := NewServer(...)` call at
the top of `TestWebUIRoutes` (line 273) to guarantee the env var is unset
regardless of the host environment. Add to the `want` slice (lines 288–299):

```go
"/favicon.svg",
"app__logo",
`rel="icon"`,
```

Add after the existing `https://` / `http://` negative checks (lines 304–309):

```go
if strings.Contains(body, "app__logo-link") {
	t.Fatalf("expected body to NOT contain app__logo-link when LOGO_LINK_URL is unset")
}
```

**Logo link test (env var set)** — new test function:

```go
func TestLogoLink(t *testing.T) {
	t.Setenv("LOGO_LINK_URL", "https://example.com")
	server := NewServer(&fakeStateMachine{}, nil)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "app__logo-link") {
		t.Fatalf("expected body to contain app__logo-link when LOGO_LINK_URL is set")
	}
	if !strings.Contains(body, `href="https://example.com"`) {
		t.Fatalf("expected body to contain href=\"https://example.com\"")
	}
}
```

`t.Setenv` automatically restores the env var after the test, so it does not
leak into `TestWebUIRoutes` (which runs with the var explicitly unset). No
`t.Parallel()` is used in either test, so there is no concurrent-env-var race.

### README update (`README.md`)

Add one row to the API Endpoints table, after the `/` row (line 52):

```
| GET | `/favicon.svg` | SVG favicon for the web UI |
```

Add a brief environment-variable note at the end of the Configuration section
(after line 58, "Optional fields have defaults; see the spec for the complete
schema."):

```
The web UI logo can link to a custom URL by setting the `LOGO_LINK_URL`
environment variable (e.g. `LOGO_LINK_URL=https://dockmind.example.org`). When
unset, the logo is a plain image with no link.
```

No other README sections change. The first fenced ```yaml block is untouched.
The existing `readme_test.go` ACs all remain satisfied — the additions only
add content.

### `readme_test.go` update

Add two cases to the `cases` slice in `TestREADME`:

```go
{"documents favicon route", "/favicon.svg", true},
{"documents LOGO_LINK_URL env var", "LOGO_LINK_URL", true},
```

### `docs/product.md` update

Add a Features entry after the Web UI line (line 20), matching the existing
format:

```
- **Favicon & Logo** — SVG favicon served at `/favicon.svg` and displayed as a logo next to the page title in the web UI; the logo links to a configurable URL via the `LOGO_LINK_URL` environment variable ([006-add-favicon-logo](../stories/006-add-favicon-logo/story.md))
```

### `product_test.go` update

Add one assertion to the existing `TestProductDoc` function (after the
`004-web-ui` check, line 16):

```go
if !strings.Contains(body, "006-add-favicon-logo") {
	t.Error("docs/product.md Features list does not reference the 006-add-favicon-logo story")
}
```

## Tasks

### Task 1 - Favicon route, logo in UI, env var injection, and Go-side tests

- `GET /favicon.svg` request
  - → HTTP 200
  - → `Content-Type` response header contains `image/svg+xml`
  - → response body contains the substring `<svg`
- `POST /favicon.svg` (wrong method)
  - → HTTP 405
- `GET /` request with `LOGO_LINK_URL` unset
  - → response body contains the substring `/favicon.svg`
  - → response body contains the substring `app__logo`
  - → response body contains the substring `rel="icon"`
  - → response body does NOT contain the substring `app__logo-link`
  - → response body does NOT contain the substring `https://`
  - → response body does NOT contain the substring `http://`
- `GET /` request with `LOGO_LINK_URL` set to `https://example.com`
  - → response body contains the substring `app__logo-link`
  - → response body contains the substring `href="https://example.com"`
- `GET /` request with `LOGO_LINK_URL` set to empty string `""`
  - → response body does NOT contain the substring `app__logo-link`
- existing `TestWebUIRoutes` assertions unchanged + `make test` from repo root
  - → all tests pass (existing web UI substring assertions still hold: `DockMind`, `/status`, `/power/on`, `/power/off`, `/restart`, `/docs`, `fetch`, `setInterval`, `llama-swap health`, `component__dot.is-danger`; `POST /` → 405; `GET /foo` → 404; regression `GET /status` → 200 body contains `"state"`, `GET /health` → 200 empty body, `GET /docs` → 200 body contains `swagger-ui`)
- `make lint` from repo root
  - → `gofmt -l .` prints no file paths and `go vet ./...` reports no issues

### Task 2 - README and product.md updates with automated validation

- README API Endpoints table updated with the favicon row + Configuration section updated with `LOGO_LINK_URL` note + `readme_test.go` updated with `/favicon.svg` and `LOGO_LINK_URL` presence checks + `make test`
  - → `readme_test.go` passes (README contains the substrings `/favicon.svg` and `LOGO_LINK_URL`)
- existing `readme_test.go` ACs still satisfied + `make test`
  - → README still contains all 5 original routes (`/status`, `/power/on`, `/power/off`, `/restart`, `/health`), `/docs`, `/`, all 7 field names (`state`, `gpuPresent`, `gpuName`, `shellyOn`, `llamaSwapRunning`, `llamaSwapHealthy`, `lastError`), `make build`, `make test`, `make lint`, `--config`, `./config.yaml`, and links to both `docs/DockMind_MVP_Specification.md` and `docs/product.md`
  - → README still does NOT contain `ResultAlreadyInState` or `ResultConflict`
  - → README still has no License section
  - → first fenced ```yaml block still loads successfully via `config.Load`
- `docs/product.md` Features entry added referencing `006-add-favicon-logo` + `product_test.go` updated + `make test`
  - → `product_test.go` passes (product.md contains `006-add-favicon-logo`)

## Technical Context

- **Go 1.24.4** — `embed` is stdlib (stable since Go 1.16); `import _ "embed"`
  is already present in `internal/api/api.go`. New stdlib imports `bytes`,
  `html`, `os` are all standard library. No new external dependencies;
  `go.mod` remains `gopkg.in/yaml.v3 v3.0.1` only.
- **`image/svg+xml`** — the IANA-registered MIME type for SVG. Setting it
  explicitly ensures the browser renders the SVG as an image rather than
  treating it as a download or text.
- **`html.EscapeString`** — Go stdlib `html` package; escapes `&`→`&amp;`,
  `<`→`&lt;`, `>`→`&gt;`, `"`→`&#34;`, `'`→`&#39;`. Used to safely inject the
  `LOGO_LINK_URL` value into the `href` attribute, preventing attribute
  breakout if the URL contains quotes or ampersands.
- **`bytes.Replace(s, old, new, 1)`** — returns a new `[]byte` with the first
  occurrence of `old` replaced by `new`. When `old` is not found, returns the
  original slice unchanged. With `n=1`, only the single logo `<img>` tag is
  wrapped; the footer `<a href="/docs">` is unaffected (different string).
- **`os.Getenv` vs. config file** — `LOGO_LINK_URL` is read via `os.Getenv` in
  `NewServer`, not from `config.yaml`. This avoids changing the YAML schema and
  config-loading code for a deployment-time branding setting. The env var is
  read once at startup; changing it requires a daemon restart (consistent with
  how `--config` is a startup-time flag).
- **`t.Setenv`** — Go 1.17+ testing helper that sets an env var for the
  duration of a test and automatically restores the original value on cleanup.
  Safe in non-parallel tests (no `t.Parallel()` is used in `api_test.go`).
- **No `html/template`** — the index HTML contains CSS and JavaScript that
  could conflict with Go template delimiters (`{{` / `}}`). Using
  `bytes.Replace` on a specific `<img>` string avoids this risk entirely and
  requires no template parsing.

## Notes

- **Manual verification (non-automatable, per story 004 convention).** Open
  `http://<host>:<port>/` in a browser and confirm:
  - The browser tab shows the DockMind SVG favicon (not a generic default).
  - The page header shows the logo image to the left of the "DockMind" title,
    vertically centered, with ~0.5rem gap.
  - With `LOGO_LINK_URL` unset: the logo is a plain image; clicking it does
    nothing.
  - With `LOGO_LINK_URL=https://example.com`: the logo is a link; clicking it
    navigates to the URL; no underline appears on the logo.
  - The Swagger UI page at `/docs` also shows the favicon in the tab.
  - Layout is usable at 360px viewport width (the logo does not cause overflow
    or wrapping).
- **`alt="DockMind"` on the logo image.** The image has a non-empty `alt` so
  that when it is wrapped in a link, the link has an accessible name. When the
  logo is not a link, the `alt` text is slightly redundant with the adjacent
  `<h1>` title, but this is harmless and keeps the HTML static (the `alt` does
  not need to change based on link presence).
- **`LOGO_LINK_URL` with a relative URL.** A relative value like `/docs` is
  valid — the `href` would be `/docs` and the link would navigate within the
  daemon. The `https://` / `http://` negative check in `TestWebUIRoutes` only
  runs with the env var unset, so a relative URL in a deployed instance does
  not conflict with the test.
- **SVG file size.** The favicon SVG is ~54 KB. SVGs are text and compress
  well with gzip (if the HTTP server adds compression in the future). The
  browser fetches it once and caches it; subsequent page loads use the cache.
  54 KB is acceptable for a homelab tool on a LAN.
- **Link target.** The logo link navigates in the same tab (no
  `target="_blank"`). For a homelab tool this is the expected behavior; if
  a new-tab preference is desired it can be requested as a follow-up.
