# Fix stale /v1/models healthUrl in config-with-gateway.yaml

## Context

Story 014 switched the llama-swap health check from `GET /v1/models` to
`GET /running` so that DockMind can both determine health and surface the
currently loaded model name(s) via `loadedModels` in `GET /status`. The
`configs/config.yaml` example and README were updated, but
`configs/config-with-gateway.yaml` was missed — its `llamaSwap.healthUrl`
still points at `http://localhost:1234/v1/models`.

When a user copies `configs/config-with-gateway.yaml` as their runtime
config, the health client GETs `/v1/models` instead of `/running`. The
`/v1/models` response (`{"object":"list","data":[{"id":"qwen3.5-9b",...}]}`)
decodes successfully into the `runningResponse` struct (the `running` field
is simply absent), so `Check` returns `(true, nil, nil)` — healthy but no
models. The result is `llamaSwapHealthy: true` with `loadedModels: []` even
though a model is loaded, exactly the symptom reported.

## Out of Scope

- Changing any Go source code — the health client, state machine, and API
  layer already correctly parse `/running` and propagate `loadedModels`
  (verified by the existing test suite in story 014).
- Updating `docs/DockMind_Gateway_Design.md`, which also contains a stale
  `healthUrl: http://localhost:1234/v1/models` reference at line 1117.
  Story 014 explicitly excluded that historical design-proposal document
  from its scope, and this bug fix is scoped to the config example only.
- Changing the `llamaSwap.healthUrl` config field name or validation rules.

## Implementation approach

### 1. Fix the config example

**`configs/config-with-gateway.yaml`** — change line 9 from:

```yaml
  healthUrl: http://localhost:1234/v1/models
```

to:

```yaml
  healthUrl: http://localhost:1234/running
```

No other lines in the file change. The `backendUrl` stays
`http://localhost:1234` (the gateway's `/v1/models` cache refresher
constructs its own URL by copying `backendURL` and setting `.Path =
"/v1/models"`, so `backendUrl` is unaffected by the health URL change).

### 2. Add a regression test

**`configs_test.go`** (repo root, `package dockmind_test`) — add a test
that loads `configs/config-with-gateway.yaml` via `config.Load` and asserts
`cfg.LlamaSwap.HealthURL` ends with `/running`. This prevents the same
regression in the future. The test follows the same pattern as the
README yaml-block test in `readme_test.go`: pass the file path to
`config.Load` and assert on the parsed `HealthURL` field.

A companion assertion verifies the file still loads successfully (required
fields present, gateway validation passes) so the test fails if the file
is accidentally broken.

### 3. Enforce the product.md entry

**`product_test.go`** (repo root, `package dockmind_test`) — add an
assertion that `docs/product.md` contains `"015-fix-loaded-models-empty"`,
following the exact pattern of the existing `"014-llama-swap-running-endpoint"`
assertion (line 40-42). This keeps the new Features-list entry
test-enforced, matching the convention every prior story follows.

## Tasks

### Task 1 - Fix healthUrl in config-with-gateway.yaml

- `configs/config-with-gateway.yaml` loaded via `config.Load`
  - → `cfg.LlamaSwap.HealthURL` equals `http://localhost:1234/running`
  - → `config.Load` returns nil error (file is valid: shelly.address,
    docker.container, llamaSwap.healthUrl, llamaSwap.backendUrl all
    present; gateway.enabled=true validation passes)

### Task 2 - Regression test for config-with-gateway.yaml healthUrl

- `configs/config-with-gateway.yaml` + `config.Load` called + HealthURL inspected
  - → HealthURL ends with `/running` (not `/v1/models`)
- `configs/config-with-gateway.yaml` + `config.Load` called
  - → no error returned (all required fields present, gateway config valid)

### Task 3 - Product doc entry is test-enforced

- `docs/product.md` contains `"015-fix-loaded-models-empty"`
  - → `TestProductDoc` passes with the new assertion added

### Task 4 - Full build/test/lint

- `make build && make test && make lint`
  - → all three pass

## Technical Context

- No new dependencies. The test uses `config.Load` (already imported in
  `readme_test.go` in the same package `dockmind_test`) and stdlib
  `strings.HasSuffix` / `testing`.
- `configs/config-with-gateway.yaml` is not currently referenced by any Go
  test or production code; it is a reference example file. The new test is
  the first to load it directly.
- The `config.Load` validation for `gateway.enabled: true` requires
  `llamaSwap.backendUrl` to be a valid URL with scheme and host — the file
  already satisfies this (`http://localhost:1234`).

## Notes

- `docs/DockMind_Gateway_Design.md` line 1117 also contains
  `healthUrl: http://localhost:1234/v1/models`. This is a historical
  design-proposal document that story 014 explicitly excluded from its
  scope. It is not test-enforced and not a runtime config. It is left
  unchanged here per the user's explicit scoping ("That is only thing that
  needs to be fixed!"). A future doc-cleanup story could address it.
- The root `config.yaml` (the default runtime config path) is gitignored,
  so each deployment's actual config is outside version control. The
  `configs/` directory holds reference examples that users copy from —
  keeping them correct is the only way to prevent this class of bug.
