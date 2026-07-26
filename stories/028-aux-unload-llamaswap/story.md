# Aux Container Unload llama-swap Before Start

## Context

Aux containers like ComfyUI manage GPU VRAM themselves — they do not go
through llama-swap. When llama-swap has a model loaded, that model
occupies VRAM. If a user starts ComfyUI while a model is loaded, there
may not be enough free VRAM for ComfyUI to work. llama-swap exposes an
HTTP endpoint (`GET <backendUrl>/unload`) that unloads all currently
loaded models, freeing VRAM. Calling it when no models are loaded is
harmless — llama-swap replies instantly with `OK`.

This feature adds an opt-in per-container config flag,
`unloadLlamaSwap`, that tells DockMind: before starting this aux
container, call the llama-swap unload endpoint to free VRAM. If the
unload call fails (e.g. llama-swap is down), the container start
proceeds anyway — if llama-swap is down, VRAM is likely already free.

## Out of Scope

- Unloading specific models rather than all models. The endpoint
  unloads everything, which is the desired behavior.
- Calling unload before the primary llama-swap container starts — it
  is already stopped at that point.
- Calling unload during the shutdown sequence — llama-swap is already
  being stopped.
- A configurable unload endpoint path. It is hardcoded to
  `<backendUrl>/unload` (GET), matching the confirmed llama-swap API.
- A configurable timeout for the unload call. It reuses the existing
  30-second aux operation timeout.

## Implementation approach

### Config

Add an `UnloadLlamaSwap bool` field to `AuxContainerConfig`
(yaml: `unloadLlamaSwap`, defaults `false`):

```yaml
auxContainers:
  - name: comfyui
    container: comfyui
    unloadLlamaSwap: true
  - name: kokoro
    container: kokoro-tts
```

Add validation: if any aux container has `unloadLlamaSwap: true`, then
`llamaSwap.backendUrl` must be set. This extends the existing
`backendUrl` validation, which currently only requires it when
`gateway.enabled` is true. The error message is:
`llamaSwap.backendUrl is required when an aux container has unloadLlamaSwap: true`.

### llama-swap Unload Client

Add a new `UnloadClient` type to the `health` package (the package that
already interacts with the llama-swap HTTP API). It is constructed with
the backend URL and issues `GET <backendURL>/unload`:

```go
type UnloadClient struct {
    url    string
    client *http.Client
}

func NewUnloadClient(backendURL string) *UnloadClient {
    return &UnloadClient{
        url:    strings.TrimRight(backendURL, "/") + "/unload",
        client: &http.Client{Timeout: 30 * time.Second},
    }
}

func (c *UnloadClient) Unload(ctx context.Context) error {
    req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
    if err != nil {
        return err
    }
    resp, err := c.client.Do(req)
    if err != nil {
        return err
    }
    defer resp.Body.Close()
    io.Copy(io.Discard, resp.Body)
    if resp.StatusCode != http.StatusOK {
        return fmt.Errorf("llama-swap unload returned status %d", resp.StatusCode)
    }
    return nil
}
```

The HTTP method is `GET` — confirmed by the user's test:
`curl http://localhost:1235/unload` returns `OK`. The official
`POST /api/models/unload` endpoint returns "Method Not Allowed" on the
user's llama-swap version, so the plain `GET /unload` path is used.

The HTTP client timeout is 30s, matching the aux operation timeout.
The context passed by the state machine also has a 30s timeout.

### State Machine

Add a new interface and wire it into `Machine`:

```go
type ModelUnloader interface {
    Unload(ctx context.Context) error
}
```

Add `unloader ModelUnloader` field to `Machine` (nil = no unloader
configured) and a setter:

```go
func (m *Machine) SetModelUnloader(u ModelUnloader) {
    m.unloader = u
}
```

Extend `AuxContainerController` with one method:

```go
type AuxContainerController interface {
    Names() []string
    IdleBlockingNames() []string
    NeedsUnloadBeforeStart(name string) bool
    Start(ctx context.Context, name string) error
    Stop(ctx context.Context, name string) error
    IsRunning(ctx context.Context, name string) (bool, error)
    StopAll(ctx context.Context) error
}
```

`NeedsUnloadBeforeStart` returns `true` when the named container's spec
has `UnloadLlamaSwap` set. It returns `false` for unknown names.

In `doAuxOperation`, when `start` is true and the state is `Ready`,
before calling `m.aux.Start(ctx, name)`, check if unload is needed:

```go
if m.unloader != nil && m.aux.NeedsUnloadBeforeStart(name) {
    unloadCtx, unloadCancel := context.WithTimeout(context.Background(), 30*time.Second)
    if err := m.unloader.Unload(unloadCtx); err != nil {
        m.logger.Warn("llama-swap unload before aux start failed, continuing with start",
            "name", name, "error", err)
    }
    unloadCancel()
}
```

The unload gets its own 30s context so it does not eat into the
docker-start context. If the unload fails, a warning is logged and the
start proceeds — if llama-swap is down, VRAM is likely already free.
If `m.unloader` is nil (no `backendUrl` configured), the unload is
skipped silently; this cannot happen in practice because config
validation requires `backendUrl` when any container has
`unloadLlamaSwap: true`, but the nil guard prevents a panic if the
machine is used without wiring.

Add `UnloadLlamaSwap bool` to `AuxContainerStatus`
(json: `unloadLlamaSwap,omitempty`). In `probeAuxContainers`, populate
it by calling `m.aux.NeedsUnloadBeforeStart(name)` for each container.
This is config metadata (no extra probe) and is always populated
regardless of gateway state, matching the `DisableIdleShutdown` pattern.

### Docker Manager

Add `UnloadLlamaSwap bool` to `docker.ContainerSpec` and implement
`NeedsUnloadBeforeStart(name string) bool` on `Manager`:

```go
func (m *Manager) NeedsUnloadBeforeStart(name string) bool {
    for _, spec := range m.specs {
        if spec.Name == name {
            return spec.UnloadLlamaSwap
        }
    }
    return false
}
```

The `docker` package remains a leaf package (does not import `state`).
`Manager` satisfies the extended `state.AuxContainerController` via
structural typing, same as the existing pattern.

### Wiring (main.go)

When building `auxSpecs` from `cfg.AuxContainers`, copy the
`UnloadLlamaSwap` flag:

```go
auxSpecs[i] = docker.ContainerSpec{
    Name:                aux.Name,
    Container:           aux.Container,
    DisableIdleShutdown: aux.DisableIdleShutdown,
    UnloadLlamaSwap:     aux.UnloadLlamaSwap,
}
```

Create the unload client and wire it into the machine when
`backendUrl` is set (regardless of whether the gateway is enabled):

```go
if cfg.LlamaSwap.BackendURL != "" {
    machine.SetModelUnloader(health.NewUnloadClient(cfg.LlamaSwap.BackendURL))
}
```

### OpenAPI spec

Add `unloadLlamaSwap` (boolean) to the `auxContainers` items schema,
with `description`: "True when llama-swap models are unloaded before
starting this container. Omitted when false."

### Web UI

In `index.html`, the aux container row template currently builds a
badge from `c.disableIdleShutdown`. Extend the badge logic to also
show a badge for `c.unloadLlamaSwap`:

```javascript
const badges = [];
if (c.disableIdleShutdown) badges.push('<span class="aux__badge">pauses auto-shutdown</span>');
if (c.unloadLlamaSwap) badges.push('<span class="aux__badge">unloads llama-swap</span>');
const badgeHtml = badges.length > 0
  ? `<div class="aux__badge-row">${badges.join('')}</div>`
  : '';
```

Both badges can appear on the same badge row. The existing
`aux__badge` CSS class is reused — no new CSS is needed.

### Config files

Update the commented-out example in `configs/config.yaml` to show the
new flag on one entry:

```yaml
# auxContainers:
#   - name: comfyui
#     container: comfyui
#     unloadLlamaSwap: true
#   - name: kokoro
#     container: kokoro-tts
#     disableIdleShutdown: true
```

### Documentation

- `README.md`: document the `unloadLlamaSwap` aux container option and
  add `unloadLlamaSwap` to the status example JSON's auxContainers
  entries.
- `readme_test.go`: add a check that README contains `unloadLlamaSwap`.
- `product_test.go`: add a check that `docs/product.md` references
  `028-aux-unload-llamaswap`.

## Tasks

### Task 1 - Config and Validation

- config with `unloadLlamaSwap: true` on one aux entry and `llamaSwap.backendUrl` set + `config.Load`
  - → `cfg.AuxContainers[0].UnloadLlamaSwap` is `true`
  - → `cfg.AuxContainers[1].UnloadLlamaSwap` is `false` (absent = default)
  - → no error
- config with `unloadLlamaSwap: true` but no `llamaSwap.backendUrl` and gateway disabled + `config.Load`
  - → returns error containing "llamaSwap.backendUrl is required when an aux container has unloadLlamaSwap: true"
- config with `unloadLlamaSwap: true` and `llamaSwap.backendUrl` set and gateway disabled + `config.Load`
  - → no error (backendUrl is valid for unload without gateway)
- config with no `unloadLlamaSwap` key on any entry + `config.Load`
  - → all entries have `UnloadLlamaSwap == false`, no error

### Task 2 - Docker Manager NeedsUnloadBeforeStart

- `docker.Manager` with two specs (comfyui with `UnloadLlamaSwap: true`, kokoro without) + `NeedsUnloadBeforeStart("comfyui")`
  - → returns `true`
- same manager + `NeedsUnloadBeforeStart("kokoro")`
  - → returns `false`
- same manager + `NeedsUnloadBeforeStart("unknown")`
  - → returns `false`
- `docker.Manager` with no specs + `NeedsUnloadBeforeStart("anything")`
  - → returns `false`

### Task 3 - UnloadClient

- `httptest.NewServer` returning 200 + body "OK" + `UnloadClient.Unload(ctx)`
  - → returns nil
  - → the request method is GET
  - → the request path is `/unload`
- `httptest.NewServer` returning 500 + `UnloadClient.Unload(ctx)`
  - → returns error containing "status 500"
- `UnloadClient` constructed with `http://127.0.0.1:1` (unreachable) + `UnloadClient.Unload(ctx)`
  - → returns error
- `UnloadClient` constructed with trailing slash `"http://localhost:1234/"` + server at that address
  - → request path is `/unload` (not `//unload`)

### Task 4 - State Machine Unload Before Aux Start

- machine Ready, aux controller with `NeedsUnloadBeforeStart` returning true for "comfyui", unloader fake that records calls, docker start succeeds + `StartAuxContainer("comfyui")`
  - → returns `AuxResultOK`
  - → unloader was called exactly once before aux.Start
  - → aux.Start was called with "comfyui"
- machine Ready, aux controller with `NeedsUnloadBeforeStart` returning true, unloader fake that returns error, aux start succeeds + `StartAuxContainer("comfyui")`
  - → returns `AuxResultOK` (start proceeds despite unload failure)
  - → aux.Start was called (start not skipped)
- machine Ready, aux controller with `NeedsUnloadBeforeStart` returning false for "kokoro", unloader fake + `StartAuxContainer("kokoro")`
  - → returns `AuxResultOK`
  - → unloader was NOT called
- machine Ready, aux controller with `NeedsUnloadBeforeStart` returning true, unloader is nil (not wired) + `StartAuxContainer("comfyui")`
  - → returns `AuxResultOK` (no panic, unload skipped)
  - → aux.Start was called
- machine Off, aux controller with `NeedsUnloadBeforeStart` returning true, unloader fake + `StartAuxContainer("comfyui")`
  - → returns `AuxResultConflict` (state gate applies before unload)
  - → unloader was NOT called
- machine Starting, aux controller with `NeedsUnloadBeforeStart` returning true, unloader fake + `StartAuxContainer("comfyui")`
  - → returns `AuxResultConflict`
  - → unloader was NOT called

### Task 5 - Status Reports unloadLlamaSwap Per Container

- machine with aux controller, two containers (comfyui flagged with unload, kokoro not) + `Status()`
  - → `AuxContainers[0].UnloadLlamaSwap` is `true`
  - → `AuxContainers[1].UnloadLlamaSwap` is `false`
- machine with nil aux controller + `Status()`
  - → `AuxContainers` is `[]` (empty non-nil slice)

### Task 6 - OpenAPI Spec

- `GET /openapi.json` response
  - → `auxContainers.items.properties` contains `unloadLlamaSwap` (boolean)

### Task 7 - Web UI Badge

The web UI is tested by string-matching the served `index.html` source
(same convention as the existing `TestWebUIAuxCard` tests).

- `GET /` response body
  - → contains `unloadLlamaSwap` (the badge condition in the row template)
  - → contains `unloads llama-swap` (the badge label text)
- The badge logic supports both badges simultaneously
  - → body contains a template expression that pushes badges into an array and joins them (e.g. `badges.push` or equivalent), so both `pauses auto-shutdown` and `unloads llama-swap` can appear on the same row

### Task 8 - Wiring and Documentation

- `configs/config.yaml` commented-out example includes `unloadLlamaSwap: true`
  - → `config.Load("configs/config.yaml")` succeeds (commented out, no effect)
- `cmd/dockmind/main.go` copies `UnloadLlamaSwap` from config into `docker.ContainerSpec` and creates `UnloadClient` when `backendUrl` is set
  - → `make build` succeeds
- `README.md` documents `unloadLlamaSwap` config option
  - → README contains `unloadLlamaSwap`
  - → README status example JSON includes `"unloadLlamaSwap"` in an auxContainers entry
  - → README yaml config example still loads via `config.Load`
- `docs/product.md` Features list includes the 028 story
  - → `product_test.go` passes
- `make build && make test && make lint` all pass

## Technical Context

- Go 1.24.4, module `github.com/dockmind/dockmind`. No new external
  dependencies.
- The `docker` package remains a leaf package (does not import `state`).
  `docker.Manager` satisfies the extended `state.AuxContainerController`
  via structural typing, same as the existing pattern.
- The `health` package gains a new `UnloadClient` type alongside the
  existing `Client`. Both talk to llama-swap via HTTP but use different
  URLs: `Client` uses `healthUrl` (the `/running` endpoint),
  `UnloadClient` uses `backendUrl + "/unload"`.
- The `state.ModelUnloader` interface is a new single-method interface.
  The `fakeAuxController` in `state_test.go` does NOT need to implement
  it — it is a separate dependency injected via `SetModelUnloader`.
  Tests inject a fake `ModelUnloader` (e.g. a `fakeUnloader` struct
  with `called int` and `err error` fields).
- The `fakeAuxController` in `state_test.go` must be updated to
  implement `NeedsUnloadBeforeStart(name string) bool` (add an
  `unloadBeforeStart` map or slice field, default nil → returns false).
- The `fakeStateMachine` in `api_test.go` does NOT need changes — the
  `StateMachine` interface is unchanged. The unload happens inside the
  state machine, not at the API layer.
- The `api_test.go` OpenAPI test must be updated to check for
  `unloadLlamaSwap` in the auxContainers items properties (add it to
  the `for _, field := range` loop on the line that currently checks
  `{"name", "running", "disableIdleShutdown"}`).
- The `TestWebUIAuxBadgeRowLayout` test in `api_test.go` checks for the
  exact string literal `<div class="aux__badge-row"><span class="aux__badge">pauses auto-shutdown</span></div>`.
  When the badge logic changes to use a `badges` array with `push`/`join`,
  this exact string no longer appears as a single literal in the JS
  source. The test must be updated: instead of matching the full wrapper
  string, assert that the body contains `<span class="aux__badge">pauses auto-shutdown</span>`
  (the badge content, still a literal inside `badges.push(...)`) and that
  the body contains `aux__badge-row` (the wrapper class, still present in
  the template). The `${badgeHtml}` ordering assertion (badge after
  `aux__actions`) remains valid and unchanged.
- The unload HTTP call uses `GET` (not `POST`), confirmed by the user's
  test: `curl http://localhost:1235/unload` returns `OK`. The
  `POST /api/models/unload` endpoint returns "Method Not Allowed" on
  the user's llama-swap version.
- The `transitionMu` is held for the entire aux operation (existing
  behavior). With unload, the lock is held for up to 60s (30s unload +
  30s docker start). This is acceptable — aux operations are
  synchronous and user-initiated; no other transition can run
  concurrently.

## Notes

- The unload call is only made for on-demand aux container starts
  (`POST /containers/{name}/start`). It is not made during the
  startup sequence (aux containers are not auto-started during
  power-on) or the shutdown sequence (llama-swap is being stopped
  anyway).
- If the unload fails, the start proceeds with a `WARN` log. The
  rationale: if llama-swap is unreachable, it is probably down, and
  VRAM is likely free. The user is not blocked from starting ComfyUI.
- The `unloadLlamaSwap` status field is config metadata, always
  populated regardless of gateway state, so the UI can badge
  containers even when the gateway is off — matching the
  `disableIdleShutdown` pattern.
- The `backendUrl` config field was previously only required when
  `gateway.enabled` is true. With this feature, it is also required
  when any aux container has `unloadLlamaSwap: true`. When neither
  condition is true, `backendUrl` remains optional.
- The unload endpoint path is hardcoded to `/unload`. If a future
  llama-swap version changes the endpoint, the `UnloadClient` can be
  updated without touching the state machine or config.
