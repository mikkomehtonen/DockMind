# Learnings

## Atomic resume-check + state transition under stateMu
**Date**: 2026-07-19
**Area**: concurrency / state machine
**What happened**: In story 017, `awaitGpuFree()` returned "GPU free" and then `shutdown()` called `setState(ShuttingDown)`. A `PowerOn`/`Restart` arriving in that window saw `AwaitingGPUFree`, set `resumeStartup=true`, and returned `202 Accepted`, but the flag was not re-checked before the state transition, so the machine powered off instead of resuming.
**Takeaway**: When a state-machine decision and the following state transition are separately guarded, a signal that should override the decision can arrive between them. Hold the state lock across both the override check and the transition (or use a dedicated `setStateOrResume` helper) so the override is either observed or atomically prevented.

## Code reviewer expects context propagation, timeouts, and error logging by default
**Date**: 2026-07-08
**Area**: code review / interfaces
**What happened**: Multiple review rounds were needed because HTTP clients had no timeout, subprocess calls used `context.Background()`, `Status()` swallowed probe errors, and `poll()` discarded check errors. These were flagged as blocking correctness/reliability issues even when the acceptance tests passed.
**Takeaway**: Design internal interfaces with `context.Context` from the start, set bounded timeouts on HTTP clients, log errors at `Warn` level rather than swallowing them, and wrap timeout messages with the last underlying error for diagnostics.

## Acceptance reviewer requires exact AC behavior, not just happy-path coverage
**Date**: 2026-07-08
**Area**: testing / acceptance criteria
**What happened**: A test that validated the implementation's behavior (`docker.IsRunning` returning an error on missing container) caused a Fail because the AC explicitly required `(false, nil)`. Tests must assert the exact contract in the story, not the implementation's natural behavior.
**Takeaway**: Read the AC matrix literally when writing tests. If the implementation diverges from the AC, fix the implementation or escalate; do not write tests that codify the divergence.

## Race-detector requirements can expose pre-existing test races
**Date**: 2026-07-18
**Area**: testing / concurrency
**What happened**: Story 016 required `go test -race ./internal/gateway/` to pass. The gateway refresher tests had used a plain `int` counter (`callCount`) that the `httptest` handler goroutine incremented while the test goroutine read it, causing multiple data-race failures unrelated to the new idle-countdown code.
**Takeaway**: When a story mandates `-race`, audit existing tests in the same package for unsynchronized shared variables (counters, flags, timestamps) across handler and test goroutines. Use `sync/atomic` or a mutex, and remember that `atomic.Int32` must be read with `.Load()` — copying the value itself triggers a `go vet` warning.

## Adding slice/map fields to Config breaks struct-equality tests
**Date**: 2026-07-19
**Area**: testing / config
**What happened**: Story 018 added `AuxContainers []AuxContainerConfig` to the `Config` struct. The existing `config_test.go` compared loaded configs with `*cfg != tc.want`, which is invalid for structs containing non-comparable fields (slices/maps). The test file had to be updated to use `reflect.DeepEqual`.
**Takeaway**: When extending `Config` with slice or map fields, update `config_test.go` to compare with `reflect.DeepEqual` instead of `!=`. Check other places that compare `Config` by value for the same breakage.

## Extend existing table-driven tests instead of adding parallel hand-written cases
**Date**: 2026-07-08
**Area**: testing / code review
**What happened**: Adding Swagger UI routes produced a second `TestSwaggerRoutes` that duplicated `GET /status`, `GET /health`, `GET /foo`, and `POST /docs`/`POST /openapi.json` 405 checks already covered by the existing `TestRoutes` table. The code reviewer flagged this as a blocking simplicity issue because it introduced two patterns for the same concern and the new tests asserted less than the existing ones.
**Takeaway**: Before writing a new test for an existing route concern, check whether the current table-driven test can absorb the case. Keep one pattern per concern; add separate test functions only for assertions that genuinely do not fit the existing table (e.g., content-type, JSON schema shape, embedded asset parsing).

## Plan commits may already include doc or test changes
**Date**: 2026-07-08
**Area**: workflow / git
**What happened**: The `plan(004-web-ui)` commit already updated `docs/product.md` (removed the Web UI non-goal and added the Features entry) before implementation began. Re-applying those changes would have created unnecessary diff noise.
**Takeaway**: After `peck story load`, inspect `git log --oneline` and `git show <plan-commit>` to see what the planning step already changed. Treat plan commits as part of the branch baseline and only implement what remains.

## Plan commits can be partial — verify every file the story requires
**Date**: 2026-07-08
**Area**: workflow / git
**What happened**: For story 006, the plan commit added the `006-add-favicon-logo` line to `docs/product.md` but did not add the matching assertion to `product_test.go`. The acceptance reviewer flagged the missing test even though the implementation was correct and the existing test passed.
**Takeaway**: Do not assume a plan commit fully updated all docs/tests mentioned in the story. Compare the story's file checklist against the actual branch state and fill in any gaps left by the planner.

## Code reviewer checks pseudocode in design documents for concurrency correctness
**Date**: 2026-07-10
**Area**: code review / design documents
**What happened**: Story 007 is a design-only deliverable (Markdown document, no production code). The code reviewer still flagged a blocking correctness issue: the `EnsureReady` pseudocode read `m.lastError` outside the `stateMu` lock, contradicting the document's own Synchronization section which states `stateMu` guards `lastError`. This would be a data race if implemented verbatim.
**Takeaway**: Design-document pseudocode is reviewed as near-final implementation, not as illustrative prose. Before running reviewers on a design-only story, verify that pseudocode is internally consistent with its own stated synchronization rules — capture shared state under the lock that guards it, and ensure the pseudocode reflects every case described in the surrounding prose.

## Story-provided test snippets may conflict with the implementation they prescribe
**Date**: 2026-07-08
**Area**: testing / stories
**What happened**: Story 006 instructed adding a CSS rule `.app__logo-link` and then asserted the response body should not contain the substring `app__logo-link` when the env var was unset. The CSS selector made that assertion impossible, so the test had to check for the HTML attribute `class="app__logo-link"` instead.
**Takeaway**: Treat story test snippets as intent, not gospel. Run them against the real implementation; when a literal substring check collides with static markup, tighten the assertion to the actual HTML contract and keep the AC's intent.

## Background goroutines must reap goroutines and propagate context to in-flight requests
**Date**: 2026-07-16
**Area**: concurrency / gateway
**What happened**: The first models-cache refresher used a bare `time.Ticker` and `context.Background()` for its backend request. Stopping the refresher raced with pending ticks (extra refresh after stop), and a refresh in flight during SIGTERM could stall shutdown for up to `requestTimeout` because cancellation did not reach the HTTP request.
**Takeaway**: For background goroutines, close a `done` channel when the goroutine exits and wait on it in `Stop*()` so callers know the goroutine is fully reaped. Derive in-flight request contexts from the goroutine's lifecycle context (`context.WithTimeout(g.modelsCtx, ...)`) so `Stop*()` cancels outstanding work promptly.

## Code reviewer validates Go stdlib internals in design pseudocode
**Date**: 2026-07-10
**Area**: code review / design documents
**What happened**: The code reviewer found two blocking issues in story 007's design pseudocode by checking against actual Go 1.24 stdlib behavior: (1) `responseTracker` embedding `http.ResponseWriter` does not promote the concrete type's `Flush` method — `http.ResponseController.Flush()` needs an `Unwrap() http.ResponseWriter` method to traverse the wrapper, or SSE streaming silently breaks; (2) `errors.Is(err, context.DeadlineExceeded)` returns true for startup failures because the existing `poll` function wraps `context.DeadlineExceeded` in `lastError` — a sentinel error (`ErrBackendError`) is needed to distinguish startup failure from client timeout.
**Takeaway**: When writing design pseudocode that wraps stdlib types or uses `errors.Is`, verify the behavior against the actual Go version's source. Interface embedding does not promote concrete-type methods; `errors.Is` traverses the entire wrapped chain. Use sentinel errors to disambiguate error categories when underlying errors may share wrapped types.

## Buffer-first ResponseWriter wrappers are simpler than tee for non-streaming responses
**Date**: 2026-07-10
**Area**: architecture / design documents
**What happened**: The model-list cache design initially used a `capturingWriter` that teed the response body to a buffer while streaming to the client (same pattern as `responseTracker`). The code reviewer flagged two blocking issues: (1) the tee approach cannot detect truncation — a partial 200 response with bytes in the buffer passes the `buf.Len() > 0` check and overwrites a valid cache; (2) the shared `ErrorHandler` does `w.(*responseTracker)` which fails for `*capturingWriter`. Replacing the tee with a buffer-first `bufferingWriter` (captures the full response before sending anything to the client) fixed both: truncation is detected via a `failed` flag set by the `ErrorHandler`, and no `Unwrap()` method is needed because flush errors are silently discarded for small non-streaming responses.
**Takeaway**: For non-streaming HTTP responses (e.g. model lists, metadata endpoints), use a buffer-first `ResponseWriter` wrapper that captures the full response before sending to the client. This detects truncation for free (the `ErrorHandler` sets a `failed` flag), avoids `Unwrap()` complexity, and simplifies the `ErrorHandler` (it can type-assert on the wrapper and set a flag instead of writing an error). Reserve the tee/streaming pattern (`responseTracker` with `Unwrap()`) for SSE streaming endpoints where buffering the full response is impractical.

## EnsureReady must handle PowerOn ResultConflict to avoid waiting on a stale changeCh
**Date**: 2026-07-10
**Area**: concurrency / state machine
**What happened**: The first implementation of `EnsureReady` captured `changeCh` under `stateMu`, called `PowerOn()` from the `Off` case, and then unconditionally waited on that channel. If `PowerOn()` returned `ResultConflict` because a just-completed shutdown goroutine still held `transitionMu` after `setState(Off)`, the captured channel would never close again and `EnsureReady` hung until its context expired. A later fix used `continue` after every `PowerOn()`, which avoided the hang but introduced a busy-wait spin because the startup goroutine had not yet called `setState(Starting)`.
**Takeaway**: In `EnsureReady`, capture `changeCh` before `PowerOn()`, then branch on the result: if `ResultAccepted`, wait on the captured channel (the startup goroutine will close it via `setState`); if `ResultConflict` or `ResultAlreadyInState`, re-evaluate state and capture a fresh channel. This preserves prompt `ctx.Done()` handling while avoiding the stale-channel hang.

## Dead code in wrapper types is blocking even when tests exercise it
**Date**: 2026-07-10
**Area**: code review / testing
**What happened**: `bufferingWriter` initially implemented the full `http.ResponseWriter` interface (`Header`, `Write`, `WriteHeader`) and had a dedicated test for it. The code reviewer flagged this as blocking dead code because the production models path wrote directly to the struct's exported fields and never invoked the interface methods. Removing the methods and the test that exercised only the dead path was required to pass.
**Takeaway**: Do not keep unused interface methods just because a wrapper *could* be used as an `http.ResponseWriter`. If production code accesses the wrapper through concrete fields, the interface methods are dead code and will be flagged. Remove them and any tests that only exercise the unused interface contract.

## fakePower.SetPower mutates fakeGPU.present in state tests
**Date**: 2026-07-11
**Area**: testing / state machine
**What happened**: When writing tests that drive GPU state manually (e.g., simulating nvidia-smi errors during startup or shutdown), the GPU appeared or disappeared instantly because `fakePower.SetPower` sets `fakeGPU.present = on` whenever `power.gpu` is non-nil. This caused tests to complete without ever hitting the intended error path.
**Takeaway**: For state tests that need explicit control over `fakeGPU.present` or `fakeGPU.err`, set `power.gpu = nil` to disable the automatic coupling. Drive the GPU fields directly from the test (under `fakeGPU.mu` if accessed concurrently).

## Check story Out of Scope / Notes before acting on reviewer trade-off feedback
**Date**: 2026-07-12
**Area**: stories / code review
**What happened**: The code reviewer flagged non-atomic file writes, content-type inconsistency for disk-loaded caches, and unsynchronized `load()` as real issues. All three were explicitly called out in the story's "Out of Scope" or "Notes" sections as accepted trade-offs (no atomic rename, raw bytes with default `application/json`, `load()` called once at startup before any requests).
**Takeaway**: When a reviewer points out a design concern, first check the story's "Out of Scope" and "Notes" sections. If the story has already accepted the trade-off, treat it as a requirement, not a defect. Only escalate or fix if the concern is not covered by the story text.

## Table-driven state-machine tests must wait for async transitions before asserting state
**Date**: 2026-07-14
**Area**: testing / state machine
**What happened**: In `TestCooldown`, subtests that expected `ResultAccepted` launched real startup/shutdown goroutines and then immediately asserted the pre-transition state (`Off`/`Ready`). This violated the repo convention to call `m.Wait()` before asserting final state, created a scheduling-order race under `GOMAXPROCS>1`, and caused subtests to run for 500ms each when the fakes were not configured as healthy.
**Takeaway**: In table-driven state tests, branch on the result: for `ResultAccepted`, call `m.Wait()` and assert the final state (`Ready` after `PowerOn`/`Restart`, `Off` after `PowerOff`); for `ResultAlreadyInState`/`ResultConflict`/`ResultCooldown`, assert the unchanged state immediately. Also configure the fakes (`gpu.present`, `health.healthy`) so async transitions complete quickly when the test does wait.

## `time.Now()` in test-case literals is evaluated at function start, not subtest run
**Date**: 2026-07-14
**Area**: testing / timing
**What happened**: Cooldown tests used `offTime: time.Now()` inside a table-driven test slice literal. When earlier subtests unexpectedly took 500ms because fakes were not healthy, the stored timestamps were stale by the time the cooldown-blocking subtests ran, causing them to see an expired cooldown and fail.
**Takeaway**: For timing-sensitive table-driven tests, either capture `time.Now()` inside the subtest closure or use durations large enough to survive the full table execution. If async transitions are involved, ensure the fakes are configured to complete promptly so the table does not outlast the cooldown window.
