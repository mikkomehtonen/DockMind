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

## Story-provided test snippets may conflict with the implementation they prescribe
**Date**: 2026-07-08
**Area**: testing / stories
**What happened**: Story 006 instructed adding a CSS rule `.app__logo-link` and then asserted the response body should not contain the substring `app__logo-link` when the env var was unset. The CSS selector made that assertion impossible, so the test had to check for the HTML attribute `class="app__logo-link"` instead.
**Takeaway**: Treat story test snippets as intent, not gospel. Run them against the real implementation; when a literal substring check collides with static markup, tighten the assertion to the actual HTML contract and keep the AC's intent.
