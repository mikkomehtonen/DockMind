# Learnings

## Hold transitionMu for the full async transition
**Date**: 2026-07-08
**Area**: concurrency / state machine
**What happened**: The first implementation acquired `transitionMu` in `PowerOn`/`PowerOff`/`Restart` and released it with `defer` before the async goroutine finished. Concurrent requests could then race past the in-flight transition and return wrong results, and `Wait()`/`WaitGroup` accounting became fragile.
**Takeaway**: When a method spawns an async goroutine, acquire the lock in the caller, pass ownership to the goroutine (which `defer`s the unlock), and never release it in the synchronous path. Add `wg.Add(1)` synchronously before `go`, and `defer wg.Done()` in the goroutine wrapper.

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

## Include gofmt in the lint target from day one
**Date**: 2026-07-08
**Area**: build / lint
**What happened**: `go vet` passed but `gofmt -l` reported misaligned struct fields in several files. The Makefile `lint` target initially only ran `go vet`.
**Takeaway**: Make `lint` run both `gofmt -l .` and `go vet ./...` so formatting issues are caught before code review.

## Reviewer reports are committed to the branch; .opencode/ stays untracked workspace config
**Date**: 2026-07-08
**Area**: workflow / git
**What happened**: Running the acceptance/code reviewers created committed review-report commits on the branch and left the `.opencode/` workspace configuration directory untracked. This makes `git status` show untracked files even though all story-related changes are committed.
**Takeaway**: Expect reviewer artifacts as branch commits. Do not commit `.opencode/` as part of a feature story; treat it as pre-existing workspace configuration.

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

## Reviewer agents running peck story load can switch branches in the shared repo
**Date**: 2026-07-10
**Area**: workflow / git
**What happened**: During story 007, reviewer agents ran `peck story load 007` which created a separate `007` branch (distinct from `007-openai-gateway`) and checked it out in the shared `.git` directory. Subsequent commits went to the `007` branch instead of `007-openai-gateway`, causing the design revision to diverge onto the wrong branch. The issue was detected only when `git log --graph --all` showed two divergent branch tips.
**Takeaway**: After reviewer agents run, verify the current branch with `git branch --show-current` before committing. If the branch has changed, either switch back with `git checkout <correct-branch>` or use `git checkout <correct-branch> -- <files>` to port changes across. Use `git log --graph --all` to diagnose divergent branches.

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
