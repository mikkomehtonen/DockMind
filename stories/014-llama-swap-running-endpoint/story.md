# Use llama-swap /running endpoint for health and loaded-model display

## Context

DockMind currently checks whether llama-swap is available and healthy by
calling `GET /v1/models` (configured via `llamaSwap.healthUrl`) and testing
for an HTTP 200 response — the body is discarded. llama-swap also exposes a
`GET /running` endpoint that returns the currently loaded model(s) and their
state. By switching the health check to `/running`, DockMind can determine
health **and** surface the loaded model name(s) in the web UI and
`GET /status` response in a single probe — no extra HTTP call per status
request.

The `/running` response shape:

```json
{"running":[]}
```

when no model is loaded but the service is up, and:

```json
{"running":[{"model":"qwen3.5-9b","state":"ready","cmd":"...","proxy":"...","ttl":600,"name":"","description":""}]}
```

when a model is loaded (state may be `"starting"` or `"ready"`).

Separately, the gateway's `/v1/models` model-list cache (story 010) is only
refreshed on-demand when a client calls `GET /v1/models`. When the gateway is
enabled, a background goroutine should periodically (default once per minute)
fetch `/v1/models` from the backend while the system is `Ready` so the cache
stays warm and disk-persisted without requiring a client request.

## Out of Scope

- Changing the `llamaSwap.healthUrl` config **field name** — only its
  documented value changes from `/v1/models` to `/running`.
- Changing the gateway's on-demand `handleModels` caching behavior — it still
  caches on every `GET /v1/models` client request; the background refresher is
  additive.
- Adding `loadedModels` to the gateway's model-list cache — the cache stores
  the raw `/v1/models` response; `loadedModels` comes from `/running` only.
- Changing the startup/shutdown state-machine sequences — they still wait for
  the health check to return healthy (now via `/running`).
- WebSocket/SSE push for real-time model updates — status remains poll-based.
- Updating `docs/learnings.md` or `docs/DockMind_Gateway_Design.md` — those
  are historical dev journals / design docs, not test-enforced artifacts.

## Implementation approach

### 1. Health client — parse `/running` and return loaded models

**`internal/health/health.go`**

Change `Client.Check` signature from `(bool, error)` to
`(bool, []string, error)`:

```go
type runningResponse struct {
    Running []struct {
        Model string `json:"model"`
    } `json:"running"`
}

func (c *Client) Check(ctx context.Context) (bool, []string, error)
```

Behavior:
- GET the configured URL (now pointing at `/running`).
- Network error → return `(false, nil, err)`.
- Non-200 status → return `(false, nil, nil)` — unhealthy, no error (server
  responded but is not ready). Body is discarded.
- 200 + valid JSON → decode into `runningResponse`, extract each
  `running[i].model` where `model != ""`, return `(true, models, nil)`.
- 200 + invalid JSON → return `(false, nil, err)` — cannot determine health.

Model extraction rule: iterate `result.Running`, append `r.Model` to the
slice when `r.Model != ""`. Entries with an empty `model` field are skipped.
The entry's `state` field (`"starting"`, `"ready"`, etc.) is ignored — any
model present in the `running` array is reported.

When `result.Running` is empty or all models are empty strings, `models` is
`nil` (the caller initializes to `[]string{}` for JSON serialization).

### 2. State machine — propagate loaded models through StatusResponse

**`internal/state/state.go`**

Change the `HealthChecker` interface:

```go
type HealthChecker interface {
    Check(ctx context.Context) (healthy bool, models []string, err error)
}
```

Add `LoadedModels` to `StatusResponse`, positioned right after
`LlamaSwapHealthy`:

```go
type StatusResponse struct {
    State             string   `json:"state"`
    GPUPresent        bool     `json:"gpuPresent"`
    GPUName           string   `json:"gpuName"`
    ShellyOn          bool     `json:"shellyOn"`
    LlamaSwapRunning  bool     `json:"llamaSwapRunning"`
    LlamaSwapHealthy  bool     `json:"llamaSwapHealthy"`
    LoadedModels      []string `json:"loadedModels"`
    LastError         *string  `json:"lastError"`
    CooldownRemaining float64  `json:"cooldownRemaining"`
}
```

Replace the `probeBool` call for Health in `Status()` with a new
`probeHealth` helper that returns both values:

```go
func (m *Machine) probeHealth(quietOnFailure bool) (bool, []string) {
    ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
    defer cancel()
    healthy, models, err := m.health.Check(ctx)
    if err != nil {
        if quietOnFailure {
            m.logger.Debug("Health status probe failed", "error", err)
        } else {
            m.logger.Warn("Health status probe failed", "error", err)
        }
        return false, nil
    }
    return healthy, models
}
```

The log message `"Health status probe failed"` stays identical to the existing
`probeBool` message so `TestStatusProbeLogLevels` continues to match by
substring. The `quietOnFailure` flag uses the same `probeFailureExpected(state)`
predicate as story 013.

In `Status()`, call `probeHealth` and ensure `LoadedModels` is never nil:

```go
healthy, models := m.probeHealth(probeFailureExpected(state))
if models == nil {
    models = []string{}
}
```

Set `LoadedModels: models` in the returned `StatusResponse`.

Update the `startup()` poll loop to use the new 3-return `Check`:

```go
func(ctx context.Context) (bool, error) {
    healthy, _, err := m.health.Check(ctx)
    return healthy, err
}
```

The `probeBool` helper itself is unchanged — it is still used for Shelly and
Docker probes.

### 3. Web UI — display loaded models in the health row

**`internal/api/index.html`**

Augment the existing "llama-swap health" row's JS `render` logic. The label
text `"llama-swap health"` stays unchanged. When `llamaSwapRunning` is true and
`llamaSwapHealthy` is true, show the loaded model name(s) instead of
`"Healthy"`:

```javascript
if (!data.llamaSwapRunning) {
    els.healthValue.textContent = "Stopped";
    els.healthDot.className = "component__dot";
} else if (data.llamaSwapHealthy) {
    if (data.loadedModels && data.loadedModels.length > 0) {
        els.healthValue.textContent = data.loadedModels.join(", ");
    } else {
        els.healthValue.textContent = "Healthy";
    }
    els.healthDot.className = "component__dot is-on";
} else {
    els.healthValue.textContent = "Unhealthy";
    els.healthDot.className = "component__dot is-danger";
}
```

When healthy with no loaded models, the value shows `"Healthy"` (same as
before). When healthy with one or more models, the value shows the model
name(s) joined by `", "`. The `"Unhealthy"` and `"Stopped"` cases are
unchanged.

### 4. Gateway — periodic /v1/models cache refresh

**`internal/gateway/gateway.go`**

Add a `modelsRefreshInterval time.Duration` field to `Gateway` (default
`60 * time.Second` set in `NewGateway`). Add a setter called before
`StartModelsRefresher`:

```go
func (g *Gateway) SetModelsRefreshInterval(d time.Duration) {
    g.modelsRefreshInterval = d
}
```

This follows the existing `InitModelsCache` post-construction setter pattern
and avoids changing the `NewGateway` / `NewGatewayWithPollInterval`
constructor signatures (which have many existing test callers).

Add `StartModelsRefresher(ctx context.Context)` and `StopModelsRefresher()`,
mirroring the `StartIdleWatcher` / `StopIdleWatcher` lifecycle pattern:

```go
func (g *Gateway) StartModelsRefresher(ctx context.Context) {
    if g.modelsRefreshInterval <= 0 {
        return
    }
    g.modelsCtx, g.modelsCancel = context.WithCancel(ctx)
    go func() {
        ticker := time.NewTicker(g.modelsRefreshInterval)
        defer ticker.Stop()
        for {
            select {
            case <-g.modelsCtx.Done():
                return
            case <-ticker.C:
                g.refreshModelsCache()
            }
        }
    }()
}

func (g *Gateway) StopModelsRefresher() {
    if g.modelsCancel != nil {
        g.modelsCancel()
    }
}
```

Add `refreshModelsCache()` — a standalone fetch-and-cache that does **not**
go through `handleModels` or `bufferingWriter`:

```go
func (g *Gateway) refreshModelsCache() {
    if g.machine.State() != state.Ready {
        return
    }
    ctx, cancel := context.WithTimeout(context.Background(), g.requestTimeout)
    defer cancel()

    modelsURL := *g.backendURL
    modelsURL.Path = "/v1/models"
    req, err := http.NewRequestWithContext(ctx, http.MethodGet, modelsURL.String(), nil)
    if err != nil {
        g.logger.Debug("models refresh: request creation failed", "error", err)
        return
    }
    req.Host = g.backendURL.Host

    resp, err := g.client.Do(req)
    if err != nil {
        g.logger.Debug("models refresh: fetch failed", "error", err)
        return
    }
    defer resp.Body.Close()
    if resp.StatusCode != http.StatusOK {
        g.logger.Debug("models refresh: non-200", "status", resp.StatusCode)
        return
    }
    body, err := io.ReadAll(resp.Body)
    if err != nil {
        g.logger.Debug("models refresh: read body failed", "error", err)
        return
    }
    contentType := resp.Header.Get("Content-Type")
    if contentType == "" {
        contentType = "application/json"
    }
    cache := &modelCache{body: body, contentType: contentType}
    g.cachedModels.Store(cache)
    if g.cacheStore != nil {
        if err := g.cacheStore.save(cache); err != nil {
            g.logger.Error("models refresh: persist failed", "error", err)
        }
    }
}
```

The refresher does **not** increment the `active` counter or reset
`lastActivity` — it is a background maintenance task, not a user request, and
must not prevent idle shutdown. Fetch failures are logged at `DEBUG` (expected
when the backend is going down); persist failures are logged at `ERROR`
(same level as `handleModels`).

**`internal/config/config.go`**

Add `ModelsRefreshInterval Duration` to `GatewayConfig`:

```go
type GatewayConfig struct {
    Enabled               bool     `yaml:"enabled"`
    IdleTimeout           Duration `yaml:"idleTimeout"`
    RequestTimeout        Duration `yaml:"requestTimeout"`
    ModelsCacheDir        string   `yaml:"modelsCacheDir"`
    ModelsRefreshInterval Duration `yaml:"modelsRefreshInterval"`
}
```

Default and validation follow the existing `RequestTimeout` pattern exactly:
- `applyDefaults`: when `gateway.enabled` is true and
  `ModelsRefreshInterval == 0`, set it to `Duration(60 * time.Second)`.
- `validate`: when `gateway.enabled` is true, require
  `ModelsRefreshInterval > 0` (after defaults are applied, so the only way to
  fail is an explicit negative value).

**`cmd/dockmind/main.go`**

In the `if cfg.Gateway.Enabled` block, after `gw.InitModelsCache(...)` and
before `gw.StartIdleWatcher(...)`, add:

```go
gw.SetModelsRefreshInterval(cfg.Gateway.ModelsRefreshInterval.Duration())
gw.StartModelsRefresher(context.Background())
```

In the shutdown section, add `gw.StopModelsRefresher()` right after
`gw.StopIdleWatcher()`.

### 5. OpenAPI spec, docs, and config examples

**`internal/api/openapi.json`** — add `loadedModels` to the
`StatusResponse.properties` object:

```json
"loadedModels": {
    "type": "array",
    "items": { "type": "string" },
    "description": "Models currently loaded in llama-swap, extracted from the /running endpoint. Empty array when no models are loaded or the service is unhealthy."
}
```

**`internal/api/api_test.go`** — add `"loadedModels"` to the
`StatusResponse` properties field list in `TestSwaggerRoutes`. Add
`"loadedModels"` to the expected-strings list in `TestWebUIRoutes` (the JS
references `data.loadedModels`).

**`configs/config.yaml`** — change `llamaSwap.healthUrl` to
`http://localhost:1234/running`. Add `modelsRefreshInterval: 60s` under the
`gateway` section.

**`README.md`** — update the Health Monitoring feature bullet to mention
`/running` and loaded models. Add `"loadedModels": ["qwen3.5-9b"]` to the
status example JSON. Change the config example's `healthUrl` to
`http://localhost:1234/running`. Add `modelsRefreshInterval: 60s` to the
gateway config example. Add a sentence in the gateway section explaining the
periodic refresh.

**`readme_test.go`** — add
`{"status example includes loadedModels field", "loadedModels", true}` to the
test cases.

**`docs/product.md`** — add a Features entry for this story.

**`product_test.go`** — add assertion that `docs/product.md` contains
`"014-llama-swap-running-endpoint"`.

**`docs/DockMind_MVP_Specification.md`** — update the "llama-swap" health
endpoint section from `GET /v1/models` to `GET /running`, document the
response shape, and update the config example and status example.

## Tasks

### Task 1 - Health client: parse /running and return loaded models

All ACs use `httptest.NewServer` (same pattern as existing `health_test.go`).

- server returns 200 + `{"running":[]}` + `Check(ctx)` called
  - → healthy == true
  - → models == nil (or len 0)
  - → err == nil

- server returns 200 + one entry with `"state":"starting"` + `Check(ctx)` called
  - → healthy == true
  - → models == []string{"qwen3.5-9b"}
  - → err == nil

- server returns 200 + one entry with `"state":"ready"` + `Check(ctx)` called
  - → healthy == true
  - → models == []string{"qwen3.5-9b"}
  - → err == nil

- server returns 200 + two entries with different models + `Check(ctx)` called
  - → healthy == true
  - → models == []string{"model-a", "model-b"}
  - → err == nil

- server returns 200 + entry with `"model":""` + `Check(ctx)` called
  - → healthy == true
  - → models has len 0 (empty model filtered out)
  - → err == nil

- server returns 500 + `Check(ctx)` called
  - → healthy == false
  - → err == nil

- server returns 404 + `Check(ctx)` called
  - → healthy == false
  - → err == nil

- server returns 200 + malformed JSON + `Check(ctx)` called
  - → healthy == false
  - → err != nil

- unreachable URL + `Check(ctx)` called
  - → healthy == false
  - → err != nil

### Task 2 - State machine: propagate loaded models through StatusResponse

All ACs use `newTestMachine()` / `newTestMachineWithRecorder()` and the
existing `fakeHealth` (updated to return `(bool, []string, error)`).

- `fakeHealth` updated: `Check` returns `(bool, []string, error)` + `models` field added
  - → all existing state tests pass with `make test` (no behavioral regression)

- state=Ready + `health.healthy=true` + `health.models=[]string{"qwen3.5-9b"}` + `Status()` called
  - → `status.LlamaSwapHealthy == true`
  - → `status.LoadedModels` equals `[]string{"qwen3.5-9b"}`

- state=Ready + `health.healthy=true` + `health.models=nil` + `Status()` called
  - → `status.LlamaSwapHealthy == true`
  - → `status.LoadedModels` equals `[]string{}` (not nil)

- state=Ready + `health.healthy=false` + `Status()` called
  - → `status.LlamaSwapHealthy == false`
  - → `status.LoadedModels` equals `[]string{}`

- state=Off + `health.err` set + `Status()` called
  - → `status.LlamaSwapHealthy == false`
  - → `status.LoadedModels` equals `[]string{}`
  - → DEBUG log "Health status probe failed" present (quiet per story 013)
  - → WARN log "Health status probe failed" absent

- state=Ready + `health.err` set + `Status()` called
  - → WARN log "Health status probe failed" present (not quiet per story 013)

- `StatusResponse` JSON marshaled with no models loaded
  - → JSON contains `"loadedModels":[]` (not `"loadedModels":null`)

- startup poll loop uses new 3-return `Check` + `PowerOn()` from Off with healthy health
  - → machine reaches Ready (existing `TestPowerOnFromOff` passes)

- `TestStatusProbeLogLevels` Health cases (all 5 states) + `make test`
  - → all pass (log levels unchanged, `assertOK` still matches)

### Task 3 - Web UI: display loaded models in the health row

- `GET /` response body contains `"loadedModels"` (JS references the field)
  - → `TestWebUIRoutes` passes with `"loadedModels"` added to expected strings

- `GET /` response body still contains `"llama-swap health"` (label unchanged)
  - → existing `TestWebUIRoutes` assertion passes

- `GET /` response body contains no `http://` or `https://` URLs
  - → existing `TestWebUIRoutes` assertion passes

### Task 4 - Gateway: periodic /v1/models cache refresh

All ACs use `httptest.NewServer` for the backend and `newFakeController()` for
the state machine (same patterns as existing `gateway_test.go`).

- gateway Ready + backend serves `/v1/models` + refresher tick elapses
  - → `g.cachedModels.Load()` is non-nil and contains the backend response body

- gateway Ready + `modelsCacheDir` configured + refresher tick elapses
  - → cache file written to disk at `<dir>/models.json`

- gateway Off + refresher tick elapses
  - → `g.cachedModels.Load()` unchanged (refresh skipped)

- gateway Ready + backend returns 500 + refresher tick elapses
  - → `g.cachedModels.Load()` unchanged (non-200 skips cache update)

- gateway Ready + backend unreachable + refresher tick elapses
  - → `g.cachedModels.Load()` unchanged (fetch failure skips cache update)

- `StartModelsRefresher` called + `StopModelsRefresher` called + wait
  - → no further refresh calls occur after stop

- `modelsRefreshInterval == 0` + `StartModelsRefresher` called
  - → goroutine not started (no-op, same as `StartIdleWatcher` with `idleTimeout == 0`)

- refresher does not increment `g.active` or reset `g.lastActivity`
  - → idle shutdown still triggers despite refresher ticks (refresher does not block shutdown)

- config: `gateway.enabled: true` + `modelsRefreshInterval` absent
  - → `cfg.Gateway.ModelsRefreshInterval` defaults to `Duration(60 * time.Second)`

- config: `gateway.enabled: true` + `modelsRefreshInterval: 30s`
  - → `cfg.Gateway.ModelsRefreshInterval` equals `Duration(30 * time.Second)`

- config: `gateway.enabled: true` + `modelsRefreshInterval: -1s`
  - → `config.Load` returns error containing `"modelsRefreshInterval"`

- config: `gateway.enabled: false` + `modelsRefreshInterval` absent
  - → `cfg.Gateway.ModelsRefreshInterval` equals `Duration(0)` (no default applied)

### Task 5 - OpenAPI, docs, and config examples

- `GET /openapi.json` response + `StatusResponse` properties parsed
  - → properties contain `"loadedModels"`

- `GET /status` response with `LoadedModels` set + body parsed as JSON
  - → body contains `"loadedModels"`

- README.md contains `"loadedModels"` in the status example
  - → `TestREADME` passes with the new field assertion

- README.md first yaml block has `healthUrl: http://localhost:1234/running`
  - → `config.Load` succeeds (field is non-empty, validation passes)

- README.md still contains `/v1/models` (gateway route documentation)
  - → existing `TestREADME` assertion for `/v1/models` passes

- `docs/product.md` contains `"014-llama-swap-running-endpoint"`
  - → `TestProductDoc` passes with the new assertion

- `configs/config.yaml` has `healthUrl: http://localhost:1234/running`
  - → `config.Load` succeeds

- `make build && make test && make lint`
  - → all three pass

## Technical Context

- No new dependencies. All changes use stdlib (`net/http`, `encoding/json`,
  `log/slog`, `context`, `time`, `io`) already imported across the affected
  packages.
- `encoding/json` is newly imported in `internal/health/health.go` (currently
  only imports `context`, `io`, `net/http`, `time`).
- The `HealthChecker` interface change from `(bool, error)` to
  `(bool, []string, error)` is the single breaking signature change. It
  propagates to: `state.Machine.Status()`, `state.Machine.startup()` poll
  loop, `state.fakeHealth` in `state_test.go`, and all `health_test.go` test
  cases. The `api.StateMachine` interface and `api.fakeStateMachine` are
  unaffected — they use `Status() state.StatusResponse`, not `HealthChecker`
  directly.
- The `config_test.go` `TestLoad` cases use `*cfg != tc.want` for full struct
  comparison. Adding `ModelsRefreshInterval` to `GatewayConfig` does not break
  existing cases because gateway is disabled (zero value `Duration(0)` on both
  sides). The `TestGatewayConfig` table needs new cases for the
  `modelsRefreshInterval` default, explicit value, and negative-value
  rejection — following the existing `requestTimeout` case pattern.
- The gateway refresher's `refreshModelsCache` builds the backend URL by
  copying `*g.backendURL` and setting `.Path = "/v1/models"`. This overwrites
  any existing path on the backend URL, consistent with how
  `newBackendRequest` preserves the request path (`/v1/models`) and only
  rewrites scheme/host.
- The `NewGateway` constructor sets `modelsRefreshInterval: 60 * time.Second`
  as the default. `SetModelsRefreshInterval` overrides it from config. In
  tests, callers use `SetModelsRefreshInterval` with a short duration (e.g.
  `10 * time.Millisecond`) to test the refresher quickly, mirroring the
  `NewGatewayWithPollInterval` pattern for the idle watcher.

## Notes

- `LoadedModels` must serialize as `[]` (not `null`) in JSON when empty. This
  is enforced by initializing `models` to `[]string{}` in `Status()` when the
  `probeHealth` return is nil. Go's `json.Marshal` produces `null` for a nil
  slice and `[]` for an empty non-nil slice.
- The `llamaSwap.healthUrl` config value changes from `/v1/models` to
  `/running`. Users must update their `config.yaml`. The field name and
  validation (non-empty) are unchanged.
- The periodic `/v1/models` refresh only runs when `gateway.enabled: true`.
  When the gateway is disabled, there is no model cache to keep warm and no
  refresher goroutine.
- The refresher does not increment the `active` request counter or reset
  `lastActivity`. This is intentional: it is a background maintenance task,
  not a user request, and must not prevent idle shutdown. If the backend goes
  down mid-refresh, the fetch fails gracefully (cache unchanged, DEBUG log).
- Model names are extracted from the `running` array regardless of the entry's
  `state` (`"starting"`, `"ready"`, etc.). A model in `"starting"` state is
  still reported — the user wants to see what is being loaded.
- The `TestStatus` table in `state_test.go` uses struct literals for expected
  values. Adding `LoadedModels` assertions requires either adding a
  `wantLoadedModels []string` field (initialized to `[]string{}` in every
  existing case) or asserting separately. The implementer should add
  `wantLoadedModels []string` to the struct and set it explicitly in each case
  to `[]string{}` (default zero value `nil` would fail a `reflect.DeepEqual`
  against `[]string{}`).
