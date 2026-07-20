# Reorder Components Card: Shelly Plug Above GPU

## Context

The web UI's "Components" card currently lists rows in this order: GPU, Shelly Plug, llama-swap, llama-swap health. This does not match the system's actual startup/shutdown ordering. Shelly power is applied first (and removed last) during a power-on sequence, then the GPU comes up, then `llama-swap` starts, then its health check passes. Listing Shelly above GPU makes the card read in startup order, which is more intuitive for operators watching a power-on cycle progress top-to-bottom.

## Out of Scope

- No changes to the `GET /status` JSON response field order.
- No changes to the JS that populates the rows (`gpuDot`/`shellyDot`/etc. element IDs and lookup logic stay identical — only the DOM order of the `<div class="component">` blocks changes).
- No changes to the GPU processes banner, aux containers card, or any other section.
- No changes to `docs.html` or the OpenAPI spec.

## Implementation approach

Edit a single file: `internal/api/index.html`. Inside the `<section class="components">` block (around lines 517–536), swap the two `<div class="component">` blocks so the Shelly Plug row appears above the GPU row. The resulting order must be:

1. Shelly Plug (`id="shelly-dot"` / `id="shelly-value"`)
2. GPU (`id="gpu-dot"` / `id="gpu-value"`)
3. llama-swap (`id="swap-dot"` / `id="swap-value"`)
4. llama-swap health (`id="health-dot"` / `id="health-value"`)

The block contents (labels, IDs, value spans) are unchanged — only the position of the two blocks is swapped. No CSS, JS, or element IDs change.

### Test approach

Add a table-driven test in `internal/api/api_test.go` that fetches `/` and asserts the byte index of the `shelly-dot` element appears before the `gpu-dot` element in the response body. This locks in the new order and prevents regressions. Use `strings.Index` on the rendered HTML body (the page is served inline from `index.html` via `go:embed`, so the served body contains the literal `id="shelly-dot"` and `id="gpu-dot"` substrings).

## Tasks

### Task 1 - Swap Shelly Plug above GPU in the Components card

- `GET /` served by the web UI
  - → the `id="shelly-dot"` element appears in the response body before the `id="gpu-dot"` element
  - → the Components card row order is: Shelly Plug, GPU, llama-swap, llama-swap health
- `GET /` served by the web UI
  - → the `id="gpu-dot"`, `id="shelly-dot"`, `id="swap-dot"`, and `id="health-dot"` elements are all still present exactly once each (no IDs lost or duplicated by the swap)

## Notes

- The existing `TestWebUIRoutes` test in `internal/api/api_test.go` checks for presence of substrings like `component__dot.is-danger` and `llama-swap health` but does not assert row order, so it will continue to pass unchanged.
- `make lint` (gofmt + go vet) and `make test` must both pass after the change.
