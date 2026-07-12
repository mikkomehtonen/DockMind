# Persist Gateway Model Cache to Disk

## Context

The OpenAI gateway (story 008) caches the `GET /v1/models` response in memory
(`atomic.Pointer[modelCache]`) so it can serve the model list while the backend
is off. However, this cache is lost when DockMind restarts — after a restart,
`GET /v1/models` returns `503 model_cache_unavailable` until the backend is
started and queried again. This is problematic because OpenWebUI and other
clients poll `/v1/models` to display available models; an empty response clears
the user's model selection.

This story adds disk persistence: the cached model list is written to a
configurable directory and loaded on startup, so the cache survives DockMind
restarts. Since models cannot change while llama-swap is running, storing the
cache once after the backend starts is sufficient — the cached content will not
go stale during a run.

## Out of Scope

- **Proactive model fetching.** The cache is still populated lazily on the
  first `GET /v1/models` request when the backend is Ready. The gateway does
  not fetch the model list on the Ready state transition.
- **Cache TTL or staleness logic.** The cache is never expired by age. It is
  replaced by a new successful `200 OK` response and overwritten on disk when
  the backend is next queried while Ready.
- **JSON validation of cached content.** The file stores raw response bytes;
  no parsing or validation is performed on load.
- **Atomic file writes (write-to-temp-then-rename).** The cache file is small
  (a few KB); `os.WriteFile` is sufficient. A crash mid-write is unlikely for
  a single small write.
- **Persisting the Content-Type header.** The file stores only the response
  body. On load, the content type defaults to `application/json` (the standard
  OpenAI `/v1/models` content type).
- **Modifying the gateway design document.**
  `docs/DockMind_Gateway_Design.md` is a historical design artifact;
  `gateway_design_test.go` assertions remain unchanged.
- **New external dependencies.** `go.mod` remains
  `gopkg.in/yaml.v3 v3.0.1` only. The persistence uses `os`, `path/filepath`,
  `sync`, and `hash/fnv` from the stdlib.

## Implementation approach

### Config

Add a `ModelsCacheDir string` field to `GatewayConfig`:

```go
type GatewayConfig struct {
    Enabled        bool     `yaml:"enabled"`
    IdleTimeout    Duration `yaml:"idleTimeout"`
    RequestTimeout Duration `yaml:"requestTimeout"`
    ModelsCacheDir string   `yaml:"modelsCacheDir"`
}
```

No default, no validation. Empty string = persistence disabled. The field is
only meaningful when `gateway.enabled` is true.

### Gateway: model cache store

Add a `modelCacheStore` type to `internal/gateway/gateway.go` that handles
file I/O for the model cache:

```go
type modelCacheStore struct {
    path            string
    mu              sync.Mutex
    lastWrittenHash uint64 // FNV-1a hash of the last content written to disk
}
```

- `newModelCacheStore(dir string) *modelCacheStore` —
  sets `path` to `filepath.Join(dir, "models.json")`. `lastWrittenHash`
  starts at 0 (nothing written yet).
- `fnv64(data []byte) uint64` — package-level helper using `hash/fnv`:
  ```go
  h := fnv.New64a()
  h.Write(data)
  return h.Sum64()
  ```
- `load() (*modelCache, error)` — reads `models.json` via `os.ReadFile`.
  Returns `(nil, nil)` when the file does not exist
  (`errors.Is(err, os.ErrNotExist)`) or is empty (0 bytes); in both cases
  `lastWrittenHash` remains 0. On success, sets
  `s.lastWrittenHash = fnv64(data)` so that a subsequent `save` with the same
  content is recognized as redundant and skipped. Returns
  `&modelCache{body: data, contentType: "application/json"}`. Returns the
  error for other read failures (e.g. permission denied); `lastWrittenHash`
  remains 0 in that case.
- `save(cache *modelCache) error` — under `s.mu.Lock()`: computes
  `h := fnv64(cache.body)`. If `h == s.lastWrittenHash`, the content is
  identical to what is already on disk — skip the write and return `nil`.
  Otherwise: creates the directory via `os.MkdirAll(filepath.Dir(s.path),
  0o755)`, writes `cache.body` via `os.WriteFile(s.path, cache.body, 0o644)`,
  and on success sets `s.lastWrittenHash = h`. The mutex serializes concurrent
  saves to prevent file corruption and protects `lastWrittenHash`. The store
  does no logging — callers log errors (consistent with the existing gateway
  pattern where `handleModels` logs proxy errors, not the proxy itself).

### Gateway: InitModelsCache method

Add an `InitModelsCache(dir string)` method on `*Gateway`:

```go
func (g *Gateway) InitModelsCache(dir string) {
    if dir == "" {
        g.logger.Warn("gateway modelsCacheDir is not configured; cached model list will not persist across restarts")
        return
    }
    g.cacheStore = newModelCacheStore(dir)
    cache, err := g.cacheStore.load()
    if err != nil {
        g.logger.Warn("failed to load cached models from disk", "error", err, "dir", dir)
        return
    }
    if cache != nil {
        g.cachedModels.Store(cache)
        g.logger.Info("loaded cached models from disk")
    }
}
```

When `dir` is empty: logs a warning, leaves `cacheStore` nil (persistence
disabled), does not load anything. When `dir` is non-empty: creates the store,
loads the cache from disk, and populates `cachedModels` if a valid file exists.
Load failures (non-`IsNotExist` errors) are logged as warnings; the gateway
continues with an empty cache.

Add a `cacheStore *modelCacheStore` field to the `Gateway` struct (nil =
persistence disabled).

### Gateway: persist on cache update

In `handleModels`, after the existing `g.cachedModels.Store(cache)` line (the
successful 200 response path), add:

```go
if g.cacheStore != nil {
    if err := g.cacheStore.save(cache); err != nil {
        g.logger.Error("failed to persist cached models to disk", "error", err)
    }
}
```

The save is synchronous (model lists are small, a few KB). When the content
matches what is already on disk (same FNV-1a hash), the write is skipped
entirely — no `os.WriteFile` call, no disk I/O. Save failures are logged but
do not fail the request — the in-memory cache is still updated and the
response is still sent to the client. The existing cache-update behavior
(on 200 only, not on 500 or truncated responses) is unchanged.

### main.go wiring

After creating the gateway and before `SetGatewayHandlers` /
`StartIdleWatcher`, call:

```go
gw.InitModelsCache(cfg.Gateway.ModelsCacheDir)
```

This is called only when `cfg.Gateway.Enabled` is true (the gateway is only
created in that branch). The warning for an empty `modelsCacheDir` is logged
inside `InitModelsCache`, so main.go needs no separate warning logic.

### Polish

- **README** (`README.md`): add `modelsCacheDir` to the Gateway Configuration
  yaml block and explain it in the surrounding text. Add a `readme_test.go`
  assertion for `modelsCacheDir`.
- **product.md**: add a Features entry for `010-cache-models-json`. Update the
  Known Limitations to reflect that persistence is now configurable. Add a
  `product_test.go` assertion for `010-cache-models-json`.
- **configs/config-with-gateway.yaml**: add `modelsCacheDir: /var/lib/dockmind`
  to the gateway section.

## Tasks

### Task 1 - Config: add modelsCacheDir field

- `gateway.enabled: true` + `gateway.modelsCacheDir: /var/lib/dockmind` + `llamaSwap.backendUrl: http://localhost:1234` in config YAML + `config.Load`
  - → loads successfully
  - → `cfg.Gateway.ModelsCacheDir` is `"/var/lib/dockmind"`
- `gateway.enabled: true` + `gateway.modelsCacheDir` absent + `llamaSwap.backendUrl: http://localhost:1234` + `config.Load`
  - → loads successfully
  - → `cfg.Gateway.ModelsCacheDir` is `""` (empty = disabled)
- `gateway.enabled: false` + `gateway.modelsCacheDir: /var/lib/dockmind` + `config.Load`
  - → loads successfully (modelsCacheDir not validated when gateway disabled)
- existing `config_test.go` cases + `make test`
  - → all existing cases pass unchanged (minimal config, full config, gateway config, error cases)

### Task 2 - Gateway: model cache disk persistence

- `InitModelsCache("")` called on a Gateway whose logger writes to a `bytes.Buffer` via `slog.NewTextHandler`
  - → buffer contains a warning message mentioning `modelsCacheDir`
  - → `cacheStore` field is nil (persistence disabled)
  - → `cachedModels.Load()` returns nil (no cache loaded)
- `InitModelsCache(dir)` called where `dir/models.json` exists with body `{"data":[{"id":"model-1","object":"model"}]}`
  - → `cachedModels.Load()` returns non-nil
  - → `cachedModels.Load().body` equals the file content
  - → `cachedModels.Load().contentType` is `"application/json"`
- `InitModelsCache(dir)` called where `dir/models.json` does not exist
  - → `cachedModels.Load()` returns nil (no error, no cache)
  - → no warning logged (missing file is normal on first start)
- `InitModelsCache(dir)` called where `dir/models.json` exists but is empty (0 bytes)
  - → `cachedModels.Load()` returns nil (empty file = no cache)
- `InitModelsCache(dir)` called where `dir` path has a regular file as a parent component (causes read error, not file-not-found)
  - → warning logged about failed load
  - → `cachedModels.Load()` returns nil
- Gateway with `cacheStore` set (via `InitModelsCache`) + state `Ready` + `GET /v1/models` + backend returns 200 with body `{"data":[{"id":"model-1"}]}`
  - → `dir/models.json` file created with the response body as content
  - → in-memory cache updated (existing behavior preserved)
  - → HTTP 200 with body forwarded to client (existing behavior preserved)
- Gateway with `cacheStore` set + state `Ready` + `GET /v1/models` + backend returns 500
  - → `models.json` file NOT written or overwritten
  - → existing in-memory cache unchanged (existing behavior preserved)
- Gateway with `cacheStore` nil (persistence disabled via `InitModelsCache("")`) + state `Ready` + `GET /v1/models` + backend returns 200
  - → no file written to disk
  - → in-memory cache updated (existing behavior preserved)
- Gateway with `cacheStore` set + save fails (cache dir path has a regular file as parent component) + state `Ready` + `GET /v1/models` + backend returns 200
  - → error logged about save failure
  - → in-memory cache still updated (`cachedModels.Load()` returns the new body)
  - → HTTP 200 with body sent to client (request not failed by save error)
- Gateway with cache loaded from disk (via `InitModelsCache`) + state `Off` + `GET /v1/models`
  - → HTTP 200 with cached body (loaded from disk)
  - → `X-DockMind-Cached: true` response header
- Gateway with cache loaded from disk + state `Starting` + `GET /v1/models`
  - → HTTP 200 with cached body
  - → `X-DockMind-Cached: true` response header
- Gateway with `cacheStore` set (via `InitModelsCache(dir)`) where `dir` does not exist + state `Ready` + `GET /v1/models` + backend returns 200
  - → `dir/models.json` created (directory created by save via `os.MkdirAll`)
  - → file contains the response body
- `modelCacheStore.save(cacheA)` + then `save(cacheA)` again with identical body
  - → second call returns `nil` without writing to disk (verify `lastWrittenHash` unchanged after second call)
  - → file content still equals `cacheA.body` (not corrupted or truncated)
- `modelCacheStore.save(cacheA)` + then `save(cacheB)` with different body
  - → second call writes `cacheB.body` to disk
  - → `lastWrittenHash` updated to `fnv64(cacheB.body)`
  - → file content equals `cacheB.body`
- `modelCacheStore.load()` from a file with body A + then `save(cacheA)` with the same body A
  - → `save` returns `nil` without writing to disk (hash initialized by `load` matches)
  - → file content still equals A (not rewritten)
  - → `lastWrittenHash` equals `fnv64(A)` after `load` (set as side effect)
- `modelCacheStore.load()` from a non-existent file + then `save(cacheA)`
  - → `save` writes to disk (no prior hash to match; `lastWrittenHash` was 0)
  - → `lastWrittenHash` updated to `fnv64(cacheA.body)`
- Concurrent `GET /v1/models` requests (20 goroutines) with `cacheStore` set + `go test -race ./internal/gateway/`
  - → no data race detected
  - → `models.json` file written and readable
- existing `gateway_test.go` tests + `make test`
  - → all existing tests pass unchanged (TestModelsHandler_CacheAndServe, TestModelsHandler_CacheServedWhenOff, TestModelsHandler_NoCacheWhenError, TestModelsHandler_CacheUpdatedOnSuccess, TestFullGatewayFlow, etc.)

### Task 3 - main.go wiring

- `make build` from repo root
  - → produces `./dockmind` binary without error
- `make test` from repo root
  - → all tests pass (config, state, gateway, api, readme, product, gateway_design)

### Task 4 - Polish: README, product.md, product_test.go, configs

- README Gateway Configuration section includes `modelsCacheDir` in the yaml block + `make test`
  - → `readme_test.go` passes with new assertion for `modelsCacheDir`
- README first fenced yaml block unchanged + `make test`
  - → `readme_test.go` yaml test still passes (first block loads via `config.Load`)
- README existing assertions still satisfied + `make test`
  - → all existing `readme_test.go` cases pass (all routes, field names, commands, doc links, no `ResultAlreadyInState`/`ResultConflict`, no License section)
- `docs/product.md` Features list references `010-cache-models-json` + `make test`
  - → `product_test.go` passes with `010-cache-models-json` assertion
- `docs/product.md` existing assertions still satisfied + `make test`
  - → `product_test.go` existing assertions pass (`004-web-ui`, `006-add-favicon-logo`, `007-openai-gateway`, `008-openai-gateway` still present; non-goal check still passes)
- `configs/config-with-gateway.yaml` contains `modelsCacheDir` in the gateway section
  - → file is valid YAML (loads via `config.Load` without error)
- `make lint` from repo root
  - → `gofmt -l .` prints no file paths
  - → `go vet ./...` reports no issues
- `make build` from repo root
  - → produces `./dockmind` binary without error

## Bootstrap

No new dependencies. The project uses Go 1.24.4 with `gopkg.in/yaml.v3 v3.0.1`
only. The persistence uses exclusively Go stdlib packages (`os`,
`path/filepath`, `sync`, `hash/fnv`, `log/slog`).

```bash
make build      # go build -o dockmind ./cmd/dockmind
make test       # go test ./...
make lint       # gofmt -l . && go vet ./...
```

## Technical Context

- **Go 1.24.4** — `go.mod` toolchain. `os.WriteFile`, `os.ReadFile`,
  `os.MkdirAll`, `filepath.Join`, `filepath.Dir`, `errors.Is`,
  `os.ErrNotExist` are all stable stdlib APIs.
- **`os.ReadFile` / `os.WriteFile`** — stdlib file I/O. `os.ReadFile` reads
  the entire file into memory in one call (suitable for small cache files).
  `os.WriteFile` opens with `O_WRONLY|O_CREATE|O_TRUNC`, writes, and closes.
  For a small file (a few KB), this is a single `write` syscall — partial
  writes are not a concern in practice.
- **`os.ErrNotExist`** — sentinel error matched via `errors.Is(err,
  os.ErrNotExist)`. `os.ReadFile` returns an error wrapping `os.ErrNotExist`
  when the file does not exist. This distinguishes "first start, no cache"
  from actual read errors (permission denied, not-a-directory, etc.).
- **`os.MkdirAll`** — creates the directory and all parent directories if
  they don't exist. Called in `save` before `os.WriteFile`. If the directory
  already exists, `MkdirAll` is a no-op (returns nil).
- **`sync.Mutex` in `modelCacheStore`** — serializes concurrent `save` calls
  and protects `lastWrittenHash`. Multiple `handleModels` goroutines can
  update the cache concurrently (each stores a new `*modelCache` via
  `atomic.Pointer.Store`). Without the mutex, concurrent `os.WriteFile` calls
  could interleave and corrupt the file, and the hash comparison could race
  with a concurrent write. The mutex is held during `save` (hash check + file
  write + hash update). `load()` is called once at startup before any
  requests, so it does not need the lock — but it does set `lastWrittenHash`
  as a side effect, which is safe because no concurrent `save` can occur yet.
- **`hash/fnv` (FNV-1a 64-bit)** — stdlib non-cryptographic hash. Used to
  detect whether the new cache content is identical to what was last written
  to disk, avoiding redundant writes. `fnv.New64a()` creates a hasher;
  `h.Write(data)` feeds bytes; `h.Sum64()` returns the 64-bit hash. FNV-1a is
  fast (single pass, no allocation beyond the hasher) and sufficient for
  equality checking. The collision probability for two distinct small JSON
  files is 1/2^64 — astronomically unlikely. A collision would cause one
  skipped write of genuinely different content, leaving stale data on disk
  until the next restart; this is an acceptable trade-off for avoiding
  redundant disk I/O on every `GET /v1/models` poll.
- **No new external dependencies** — `go.mod` remains
  `gopkg.in/yaml.v3 v3.0.1` only. `go.sum` unchanged.
- **Existing test conventions** — stdlib `testing` + `net/http/httptest` +
  `t.TempDir()`, hand-written fakes, table-driven tests, no testify/mocking
  libraries (per `AGENTS.md`). The `modelCacheStore` is tested with
  `t.TempDir()` for file I/O. The `InitModelsCache` warning is tested by
  capturing log output via `slog.NewTextHandler` writing to a `bytes.Buffer`.

## Notes

- **"Store once" interpretation.** The user noted that models cannot change
  while llama-swap is running, so storing once is sufficient. The
  implementation persists on every successful cache update (every
  `GET /v1/models` 200 response while Ready), but skips the disk write when
  the content is identical to what is already on disk (detected via FNV-1a
  hash comparison). This means the file is written once after the first
  successful fetch, and subsequent polls with the same model list perform zero
  disk I/O. After a restart, `load()` initializes the hash from the existing
  file, so the first poll with unchanged models also skips the write. A
  "write-once-per-Ready-session" flag is not needed — the hash comparison
  achieves the same result with simpler logic.
- **Content-Type on load.** The disk file stores only the response body. On
  load, `contentType` is set to `"application/json"` (the standard OpenAI
  `/v1/models` content type). If the backend returns a different content type
  (e.g. `application/json; charset=utf-8`), the in-memory cache from a live
  fetch will have that type, but a disk-loaded cache will have
  `application/json`. This minor inconsistency is acceptable — the body is
  still JSON and clients parse it correctly.
- **Warning is logged by the gateway, not main.go.** `InitModelsCache("")`
  logs the warning. This makes it testable through the gateway package
  (capture logger output in a buffer) rather than requiring main.go
  integration tests. main.go calls `gw.InitModelsCache(cfg.Gateway.ModelsCacheDir)`
  unconditionally when the gateway is enabled — the method handles the
  empty-dir case.
- **Save failure does not fail the request.** If `os.WriteFile` fails (e.g.
  disk full, permission denied), the error is logged at ERROR level but the
  HTTP response is still sent. The in-memory cache is still updated, so
  subsequent requests while the backend is off will serve from the in-memory
  cache. The disk cache is retried on the next successful `GET /v1/models`.
- **Load failure does not fail startup.** If `os.ReadFile` returns a
  non-`IsNotExist` error (e.g. permission denied), the warning is logged and
  the gateway starts with an empty cache. The backend can still be started
  and the cache populated from a live fetch.
- **`InitModelsCache` is called before `StartIdleWatcher`.** The cache is
  loaded at startup before any request can arrive (the HTTP server starts in
  a goroutine after all setup). This ensures the disk cache is available
  immediately after a restart.
- **`configs/config.yaml` is not updated.** The gateway is disabled
  (`enabled: false`) in `configs/config.yaml`, so `modelsCacheDir` is not
  relevant. It is added only to `configs/config-with-gateway.yaml` where the
  gateway is enabled.
- **README test enforcement.** `readme_test.go` validates README content. The
  first fenced yaml block must still load via `config.Load`. The
  `modelsCacheDir` field is added to the second yaml block (Gateway
  Configuration section), not the first. A new `readme_test.go` assertion is
  added for `modelsCacheDir`.
- **`gateway_design_test.go` unchanged.** The design document is not modified.
  Existing assertions continue to pass.
- **`modelCacheStore` is unexported.** It is an implementation detail of the
  gateway package, accessible from `gateway_test.go` (same package). No
  interface is needed — the concrete struct is tested directly with
  `t.TempDir()`.
