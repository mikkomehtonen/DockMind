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

## Adding slice/map fields to Config breaks struct-equality tests
**Date**: 2026-07-19
**Area**: testing / config
**What happened**: Story 018 added `AuxContainers []AuxContainerConfig` to the `Config` struct. The existing `config_test.go` compared loaded configs with `*cfg != tc.want`, which is invalid for structs containing non-comparable fields (slices/maps). The test file had to be updated to use `reflect.DeepEqual`.
**Takeaway**: When extending `Config` with slice or map fields, update `config_test.go` to compare with `reflect.DeepEqual` instead of `!=`. Check other places that compare `Config` by value for the same breakage.

## Plan commits can be pre-applied or partial — diff the story checklist against the branch
**Date**: 2026-07-08
**Area**: workflow / git
**What happened**: For story 004 the plan commit had already updated `docs/product.md` before implementation began (re-applying it created diff noise), while for story 006 the plan commit added the `product.md` line but missed the matching `product_test.go` assertion — the acceptance reviewer flagged the missing test even though the implementation was correct.
**Takeaway**: After `peck story load`, inspect `git log --oneline` and `git show <plan-commit>`: plan commits are part of the branch baseline (do not re-apply what is already there), but they can be incomplete — compare the story's file checklist against the actual branch state and fill in any gaps left by the planner.

## Code reviewer checks pseudocode in design documents for concurrency correctness
**Date**: 2026-07-10
**Area**: code review / design documents
**What happened**: Story 007 is a design-only deliverable (Markdown document, no production code). The code reviewer still flagged a blocking correctness issue: the `EnsureReady` pseudocode read `m.lastError` outside the `stateMu` lock, contradicting the document's own Synchronization section which states `stateMu` guards `lastError`. This would be a data race if implemented verbatim.
**Takeaway**: Design-document pseudocode is reviewed as near-final implementation, not as illustrative prose. Before running reviewers on a design-only story, verify that pseudocode is internally consistent with its own stated synchronization rules — capture shared state under the lock that guards it, and ensure the pseudocode reflects every case described in the surrounding prose. Reviewers also check pseudocode against actual stdlib behavior (interface embedding does not promote concrete-type methods, `errors.Is` traverses the whole wrapped chain) — verify wrapper/sentinel patterns against the current Go version's source.

## Background goroutines must reap goroutines and propagate context to in-flight requests
**Date**: 2026-07-16
**Area**: concurrency / gateway
**What happened**: The first models-cache refresher used a bare `time.Ticker` and `context.Background()` for its backend request. Stopping the refresher raced with pending ticks (extra refresh after stop), and a refresh in flight during SIGTERM could stall shutdown for up to `requestTimeout` because cancellation did not reach the HTTP request.
**Takeaway**: For background goroutines, close a `done` channel when the goroutine exits and wait on it in `Stop*()` so callers know the goroutine is fully reaped. Derive in-flight request contexts from the goroutine's lifecycle context (`context.WithTimeout(g.modelsCtx, ...)`) so `Stop*()` cancels outstanding work promptly.

## fakePower.SetPower mutates fakeGPU.present in state tests
**Date**: 2026-07-11
**Area**: testing / state machine
**What happened**: When writing tests that drive GPU state manually (e.g., simulating nvidia-smi errors during startup or shutdown), the GPU appeared or disappeared instantly because `fakePower.SetPower` sets `fakeGPU.present = on` whenever `power.gpu` is non-nil. This caused tests to complete without ever hitting the intended error path.
**Takeaway**: For state tests that need explicit control over `fakeGPU.present` or `fakeGPU.err`, set `power.gpu = nil` to disable the automatic coupling. Drive the GPU fields directly from the test (under `fakeGPU.mu` if accessed concurrently).

## Check story text before acting on reviewer feedback — contract lines beat reviewer preference
**Date**: 2026-07-12
**Area**: stories / code review
**What happened**: The code reviewer flagged non-atomic file writes, content-type inconsistency for disk-loaded caches, and unsynchronized `load()` as real issues; all three were explicitly accepted trade-offs in the story's "Out of Scope"/"Notes" sections. Later (story 030), a non-blocking reviewer *suggestion* (report `false` from `IdleShutdownEnabled()` when the toggle is not applicable) contradicted the story's explicit contract line requiring the raw `Load()`; the real gap the suggestion pointed at was a stale OpenAPI description, which was fixed instead.
**Takeaway**: When a reviewer points out a design concern or suggests an improvement, first check the story text ("Out of Scope", "Notes", AC/contract lines). If the story has already decided, treat it as a requirement, not a defect — follow the story and fix the real gap the suggestion surfaces (e.g. a doc/description mismatch). When re-reviewing after accepting a non-blocking advisory, tell the reviewer exactly which findings were accepted and cite the story line, so it confirms the acceptance rather than re-raising them.

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

## Release mutex before calling slow probe functions in tick loops
**Date**: 2026-07-25
**Area**: concurrency / gateway
**What happened**: In story 026, restructuring `Gateway.tick()` to gate the blocked-check behind the active-check caused a deadlock. The active-check acquired `activeMu` and then called `g.machine.IdleShutdownBlocked()` (which does live docker probes) while still holding the lock. Tests that tried to acquire `activeMu` to set `lastActivity` deadlocked because the tick held the lock across the slow probe. Additionally, the idle-check after the probe read `g.lastActivity` and `g.pendingShutdown` without re-acquiring the lock, then called `g.activeMu.Unlock()` which panicked.
**Takeaway**: When restructuring a tick loop that calls a potentially-slow probe (docker inspect, HTTP request, subprocess), release the mutex before the probe and re-acquire it before reading/writing the protected state. Trace every lock acquisition and release path after restructuring — a missing `Lock()` before a read or a stray `Unlock()` without a matching `Lock()` will deadlock or panic.

## Tests that claim to verify an invariant must actually verify it
**Date**: 2026-07-26
**Area**: testing / code review
**What happened**: In story 028, `TestStartAuxContainerUnloadBeforeStartOrdering` was named and commented to verify that the unloader is called before `aux.Start`, but the assertions only checked call counts (`unloader.calls == 1`, `len(aux.startCalls) == 1`). If a future refactor swapped the order, the test would still pass — false confidence on the feature's core invariant. The code reviewer flagged this as blocking.
**Takeaway**: When a test name or comment claims to verify a specific invariant (ordering, atomicity, exclusivity), the assertions must actually check that invariant. For ordering, use a shared sequence counter stamped by both fakes and assert the relative order. For atomicity, assert that no intermediate state is observable. Call-count assertions alone prove "both happened," not "in the right order."

---

## Story "implementation approach" is guidance — codebase conventions win
**Date**: 2026-08-02
**Area**: workflow / code review
**What happened**: Story 029's implementation approach said the Machine should hold a pointer to `*lact.Client`, but the `state` core is intentionally stdlib-only and wires every collaborator through an interface. The code reviewer flagged the concrete pointer as a blocking simplicity issue; switching to a `LactController` interface (matching `Unbinder`/`ModelUnloader`/`AuxContainerController`) resolved it without touching any AC.
**Takeaway**: When a story's implementation approach conflicts with an established codebase pattern, follow the codebase pattern — ACs are behavioral, the approach section is guidance. Interface-based wiring in `state` is the norm; concrete pointers get flagged.

---

## Reset mutex-guarded state before flipping the atomic flag that gates it
**Date**: 2026-09-23
**Area**: concurrency / gateway
**What happened**: Story 030's `SetIdleShutdownEnabled(true)` first did `idleShutdownEnabled.CompareAndSwap(false, true)` and only then reset `lastActivity` / cleared `pendingShutdown` under `activeMu`. The code reviewer flagged the sub-instruction window: a concurrent `tick()` could observe `enabled=true` from the CAS while still reading the stale reservation and fire the Phase-2 `PowerOff`. Reordering to reset-under-lock-first, then `Store(true)`, closed the window — a tick that observes the new flag value is guaranteed (happens-before) to acquire `activeMu` after the reset critical section.
**Takeaway**: When an atomic flag gates readers of mutex-guarded state, write the state first (under the lock) and flip the flag last — never flip-then-reset. If the pre-flip check (`Load()`) is only advisory, keep the reset idempotent so a benign double-reset is harmless. Verify with `go test -race` plus a regression test that primes the stale state directly (sub-tick scenarios cannot be hit with fixed-sleep watcher tests alone).


