# Add Swagger UI for API Exploration

## Context

DockMind has a stable 5-endpoint REST API (`/status`, `/power/on`, `/power/off`,
`/restart`, `/health`) documented in `docs/DockMind_MVP_Specification.md` and the
README, but there is no way to explore it interactively in a browser. Adding
Swagger UI lets a developer or operator point a browser at the daemon and
inspect every endpoint's parameters, response codes, and schema without reading
the spec document. The OpenAPI 3.0 spec is hand-written (the API surface is
small and stable) and served alongside the UI — no code-generation step, no new
Go dependencies, consistent with the project's stdlib-only philosophy.

## Out of Scope

- Auto-generating the OpenAPI spec from code annotations (e.g. `swaggo/swag`).
  The spec is hand-written and maintained manually.
- Embedding Swagger UI assets into the binary. The UI loads its JS/CSS from a
  CDN; the binary stays lean.
- Authentication on the Swagger UI or OpenAPI spec endpoints (trusted LAN, same
  as the rest of the API).
- Documenting `/openapi.json` in the README API table — it is an internal
  endpoint consumed by the UI; only `/docs` is user-facing.

## Implementation approach

### Files and placement

Two new files live in `internal/api/` (the package that owns the mux) and are
embedded at compile time via `//go:embed`:

- `internal/api/openapi.json` — the OpenAPI 3.0.3 spec (static JSON).
- `internal/api/docs.html` — the Swagger UI HTML page (loads UI from CDN).

### Embedding (stdlib only)

`internal/api/api.go` gains a blank import of `embed` and two embedded
variables. The `//go:embed` directive must be immediately above each variable
declaration with no blank line in between:

```go
import _ "embed"

//go:embed openapi.json
var openapiSpec []byte

//go:embed docs.html
var docsHTML []byte
```

`//go:embed` with `[]byte` requires `import _ "embed"` (stdlib, since Go 1.16).
No new external dependencies — `go.mod` stays at `gopkg.in/yaml.v3 v3.0.1` only.

### Route registration

Add two routes to the existing `http.ServeMux` in `Server.Handler()`, using the
same Go 1.22+ method-prefixed pattern as the existing five routes:

```go
mux.HandleFunc("GET /docs", s.handleDocs)
mux.HandleFunc("GET /openapi.json", s.handleOpenAPI)
```

### Handler implementations

```go
func (s *Server) handleDocs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write(docsHTML)
}

func (s *Server) handleOpenAPI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(openapiSpec)
}
```

The content is embedded at compile time, so there is no error path — a simple
`w.Write` suffices. `w.Write` errors are ignored (the client may have
disconnected; nothing useful to do), consistent with the existing handlers which
only log `json.Encode` errors.

### OpenAPI 3.0.3 spec structure (`openapi.json`)

The spec is a hand-written JSON document with this required structure:

- `openapi`: `"3.0.3"`
- `info.title`: `"DockMind API"`
- `info.version`: `"1.0.0"`
- `info.description`: a one-paragraph summary of DockMind (from
  `docs/product.md`'s opening description: a lightweight daemon that manages the
  lifecycle of an AI inference server running on an external GPU).
- `servers`: `[{"url": "/"}]` — relative URL so Swagger UI sends "Try it out"
  requests to the same host that served the page.
- `paths` — five entries:

| Path          | Method | Summary                                          | Responses                                                            |
|---------------|--------|--------------------------------------------------|----------------------------------------------------------------------|
| `/status`     | GET    | Get current system state and component health    | 200 → `StatusResponse` JSON                                          |
| `/power/on`   | POST   | Power on the eGPU and start llama-swap           | 202 (transition initiated), 200 (already in state), 409 (conflict)   |
| `/power/off`  | POST   | Stop llama-swap and power off the eGPU           | 202 (transition initiated), 200 (already in state), 409 (conflict)   |
| `/restart`    | POST   | Stop then start the complete system              | 202 (transition initiated), 200 (already in state), 409 (conflict)   |
| `/health`     | GET    | DockMind daemon health (not GPU readiness)       | 200 (empty body)                                                     |

POST endpoints have empty-body responses (no `content` key in the response
object); each response carries only a `description` string. The `/status` GET
200 response includes `content.application/json.schema` set to
`{"$ref": "#/components/schemas/StatusResponse"}`.

- `components.schemas.StatusResponse` — an object schema with these exact
  properties (field names and types match the JSON tags on
  `state.StatusResponse` in `internal/state/state.go`):

| Property            | Type    | Notes                                                          |
|---------------------|---------|----------------------------------------------------------------|
| `state`             | string  | `enum`: `["Off", "Starting", "Ready", "ShuttingDown", "Error"]` |
| `gpuPresent`        | boolean |                                                                |
| `gpuName`           | string  |                                                                |
| `shellyOn`          | boolean |                                                                |
| `llamaSwapRunning`  | boolean |                                                                |
| `llamaSwapHealthy`  | boolean |                                                                |
| `lastError`         | string  | `nullable: true` (null when not in Error state)                |

### Swagger UI HTML page (`docs.html`)

A minimal HTML page that loads Swagger UI from CDN and points it at the local
spec. CDN version pinned to `swagger-ui-dist@5.32.8` (latest stable, confirmed
via `npm view swagger-ui-dist version` on 2026-07-08):

```html
<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <title>DockMind API</title>
  <link rel="stylesheet" href="https://unpkg.com/swagger-ui-dist@5.32.8/swagger-ui.css">
</head>
<body>
  <div id="swagger-ui"></div>
  <script src="https://unpkg.com/swagger-ui-dist@5.32.8/swagger-ui-bundle.js"></script>
  <script>
    window.onload = function () {
      SwaggerUIBundle({
        url: "/openapi.json",
        dom_id: "#swagger-ui"
      });
    };
  </script>
</body>
</html>
```

The page requires internet access to load the UI JS/CSS from `unpkg.com`. The
OpenAPI spec itself is always served locally from the embedded bytes.

### README update

Add one row to the API Endpoints table in `README.md`, after the `/health` row:

```
| GET | `/docs` | Interactive Swagger UI for exploring the API |
```

No other README sections change. The existing `readme_test.go` ACs (presence of
the 5 original routes, 7 field names, absence of internal enum names, no License
section, yaml block loads) all remain satisfied — the new row only adds content.

### `readme_test.go` update

Add one case to the `cases` slice in `TestREADME`:

```go
{"documents /docs route", "/docs", true},
```

This enforces that the README documents the Swagger UI endpoint. The current
README does not contain the substring `/docs` (verified: `docs/product.md` and
`docs/DockMind_MVP_Specification.md` contain `docs/` but not the sequence
`/docs`), so this check is meaningful only after the table row is added.

### Test approach

Add a new test function `TestSwaggerRoutes` in `internal/api/api_test.go` using
the existing `httptest.NewRecorder` + `NewServer(fake, nil)` pattern. The new
routes do not depend on the state machine, so the fake can be zero-valued.
Tests verify:

- Status codes and Content-Type headers for `/docs` and `/openapi.json`.
- The `/openapi.json` body parses as valid JSON via `encoding/json.Unmarshal`
  into `map[string]any`, then assert structural properties (openapi version,
  path keys, schema property keys) by navigating the parsed map with type
  assertions.
- The `/docs` body contains `swagger-ui` and `/openapi.json` as substrings.
- Wrong-method (POST) on `/docs` and `/openapi.json` returns 405 (the mux
  handles this natively, same as existing routes).
- Existing routes still work (regression): `GET /status` → 200 with JSON body,
  `GET /health` → 200 empty body, `GET /foo` → 404.

## Tasks

### Task 1 - OpenAPI spec, Swagger UI page, and routes

- `GET /docs` request
  - → HTTP 200
  - → `Content-Type` response header contains `text/html`
  - → response body contains the substring `swagger-ui`
  - → response body contains the substring `/openapi.json`
- `GET /openapi.json` request
  - → HTTP 200
  - → `Content-Type` response header contains `application/json`
  - → response body parses as valid JSON (`json.Unmarshal` into `map[string]any` returns no error)
  - → parsed JSON `openapi` field equals `"3.0.3"`
  - → parsed JSON `paths` object contains the keys `/status`, `/power/on`, `/power/off`, `/restart`, `/health`
  - → parsed JSON `components.schemas.StatusResponse.properties` object contains the keys `state`, `gpuPresent`, `gpuName`, `shellyOn`, `llamaSwapRunning`, `llamaSwapHealthy`, `lastError`
- `POST /docs` (wrong method on a known path)
  - → HTTP 405
- `POST /openapi.json` (wrong method on a known path)
  - → HTTP 405
- existing routes after adding new routes + `GET /status`
  - → HTTP 200 and body contains `"state"`
- existing routes after adding new routes + `GET /health`
  - → HTTP 200 and body is empty
- existing routes after adding new routes + `GET /foo` (unknown path)
  - → HTTP 404
- `make test` from repo root
  - → all tests pass (existing + new swagger route tests)
- `make lint` from repo root
  - → `gofmt -l .` and `go vet ./...` report no issues

### Task 2 - README update with automated validation

- README API Endpoints table updated + `readme_test.go` updated with `/docs` presence check + `make test`
  - → `readme_test.go` passes (README contains the substring `/docs`)
- existing `readme_test.go` ACs still satisfied + `make test`
  - → README still contains all 5 original routes (`/status`, `/power/on`, `/power/off`, `/restart`, `/health`), all 7 field names (`state`, `gpuPresent`, `gpuName`, `shellyOn`, `llamaSwapRunning`, `llamaSwapHealthy`, `lastError`), `make build`, `make test`, `make lint`, `--config`, `./config.yaml`, and links to both `docs/DockMind_MVP_Specification.md` and `docs/product.md`
  - → README still does NOT contain `ResultAlreadyInState` or `ResultConflict`
  - → README still has no License section
  - → first fenced ```yaml block still loads successfully via `config.Load`

## Technical Context

- **Go 1.24.4** — `embed` is stdlib (stable since Go 1.16). `//go:embed` with
  `[]byte` requires `import _ "embed"`. No new external Go dependencies.
- **swagger-ui-dist 5.32.8** — latest stable version, confirmed via
  `npm view swagger-ui-dist version` (2026-07-08). Loaded from CDN
  (`https://unpkg.com/swagger-ui-dist@5.32.8/`), not embedded in the binary.
  The UI requires internet access in the browser; the OpenAPI spec is served
  locally from embedded bytes.
- **OpenAPI 3.0.3** — a stable, widely-supported spec version. Swagger UI 5.x
  renders it natively. No validation library is added; tests verify structural
  properties by parsing the JSON with stdlib `encoding/json`.
- **`http.ServeMux` method patterns** — Go 1.22+ patterns (`"GET /docs"`)
  natively return 405 for wrong methods and 404 for unknown paths, consistent
  with the existing five routes.
- **No new Go dependencies** — `go.mod` remains `gopkg.in/yaml.v3 v3.0.1` only.

## Notes

- **CDN dependency**: the Swagger UI page loads JS/CSS from `unpkg.com`. If the
  browser has no internet access, the UI will not render, but `/openapi.json`
  is still served locally and can be imported into any external Swagger UI
  instance or API client. This tradeoff keeps the binary lean (no size
  increase) and was chosen by the project owner.
- **Spec maintenance**: the OpenAPI spec is hand-written and must be updated
  manually when endpoints change. The API surface is small (5 endpoints) and
  stable, so this is low-overhead. The test suite verifies the spec contains
  all 5 paths and all 7 `StatusResponse` fields, catching drift if an endpoint
  or field is removed (but not if descriptions change).
- **`/openapi.json` not in README API table**: it is an internal endpoint
  consumed by the Swagger UI page. Only `/docs` is documented as user-facing.
- **Content-Type for `/docs`**: `text/html; charset=utf-8`. The test checks
  that the header *contains* `text/html`, so the charset suffix does not break
  the assertion.
- **`w.Write` error handling**: the embedded content is in-memory, so
  `w.Write` can only fail if the client disconnects. The existing handlers
  only log `json.Encode` errors and otherwise ignore write failures; the new
  handlers follow the same convention (ignore `w.Write` errors).
- **Relationship to the "Web UI" non-goal**: `docs/product.md` lists "Web UI"
  as a non-goal, referring to a management dashboard or control panel for the
  daemon. Swagger UI is an API documentation/exploration tool, not a
  management interface — it renders the OpenAPI spec and lets users send test
  requests, but it does not add new business logic or a custom UI. This
  distinction is why the feature does not contradict the non-goal.
