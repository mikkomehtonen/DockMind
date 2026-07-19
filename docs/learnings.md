# Learnings

## Reusing a task_id can return a stale reviewer report
**Date**: 2026-07-19
**Area**: workflow / reviewers
**What happened**: After committing fixes for an acceptance-review Fail, the second run reused the same task_id and produced a report that ignored the new tests (same line numbers, same gaps, no mention of the added assertions). Starting a fresh acceptance-reviewer task with no task_id correctly analyzed the current branch and passed.
**Takeaway**: When reviewer feedback has been addressed with new commits, prefer a fresh task invocation over reusing the previous task_id. If reusing a task_id, verify the report references the current HEAD and the new/changed files; if it looks stale, restart with a fresh task.

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

## UI behavior tests use source-code string checks, not JS execution
**Date**: 2026-07-19
**Area**: testing / web UI
**What happened**: Story 021 required testing web UI button enablement and feedback messages for different state inputs. The project has no JS test runner and only Go stdlib tests, so the new tests (`TestWebUIAuxStartGatedOnReady`, `TestWebUIAuxStartFeedbackMessage`) verify the exact JS source strings and conditional patterns in the served HTML rather than executing `render()` or `doAuxAction()`.
**Takeaway**: For web UI stories, test dynamic behavior by asserting the presence of the expected JS logic in the served HTML. Do not add a JS test runner or external browser dependency; keep UI tests as Go string-presence checks that pin the exact conditional expressions and message strings.


