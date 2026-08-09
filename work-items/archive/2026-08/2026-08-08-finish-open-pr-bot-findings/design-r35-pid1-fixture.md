# R35 F1 design — Linux PID-1 fixture construction seam

## Decision scope and evidence

- Scope: architecture finding F1 only. F2 is already `inline-sufficient` and is not redesigned here.
- Candidate: `.scratch/pr591-bot-r9` at published HEAD `c363e5845ebda802932d57b23757d68a98cddcf8`, plus the inspected local R35 delta (`review-r35-consolidated-architecture.md:5`).
- Confirmed defect: the PID-1 fixture replaces the production child factory with a direct `posixContainedChild` literal (`internal/process/contained_stream_linux_test.go:315-323`), while production construction owns classifier selection and deferred-wait initialization (`internal/process/contained_stream_other.go:35-49`). Deferred waiting requires initialized `waitObserved` and `reapDone` channels (`internal/process/contained_stream_wait_deferred_posix.go:10-20,31-42,54-75`). The accepted analyst, QA, and architecture artifacts classify this as a real regression-guard contract failure (`review-r35-consolidated-analyst.md:38`; `review-r35-consolidated-qa.md:37,44`; `review-r35-consolidated-architecture.md:39,44-53`).
- Exact-candidate spot-check: HEAD matched `c363e5845ebda802932d57b23757d68a98cddcf8`; the cited constructor, channels, direct literal, and handshake/oracles remain at the cited lines. No test or build was run in this design lane.

## Chosen approach

Use a local decorator around the production factory already installed in `defaultContainedStreamDependencies`:

1. Build `deps` with `defaultContainedStreamDependencies` (`internal/process/contained_stream.go:128-141`).
2. Capture its original `newChild` function before replacing the dependency field.
3. In the fixture's replacement `newChild`, call the captured production factory first and propagate its error.
4. Use a checked assertion that the returned Linux child is `*posixContainedChild`; return a distinct test-fixture construction error if this Linux-only invariant is violated.
5. Override only `child.classifier` with the existing test classifier callback, then return the production-constructed child.
6. Keep `linuxClassifierInvocation` and `linuxClassifierTestFactory("worker", &invocation)` inside the classifier callback (`internal/process/contained_stream_linux_test.go:318-320`). Each classifier invocation therefore receives a fresh factory/tracking object; no invocation-local state is lifted to fixture or package scope.

This is a decorator of the exact production dependency consumed by `runContainedStreamWithDependencies` (`internal/process/contained_stream.go:252-280`), not a second constructor. Any later initialization added by the production factory is inherited automatically.

## Alternatives

| Alternative | Tradeoff and disposition |
|---|---|
| Call `newPlatformContainedChild` directly, then checked-assert and override only `classifier` | Correct today and inherits the current initializer (`internal/process/contained_stream_other.go:46-49`). Rejected in favor of the captured dependency because the fixture already receives the production factory through `defaultContainedStreamDependencies`; calling the symbol directly couples the test to a lower seam and could diverge if the default dependency later wraps or replaces it. |
| Keep the literal and call `initializePlatformContainedWait` manually | Smallest textual patch, but rejected. It duplicates constructor-order knowledge and only repairs today's two channels; a future production constructor invariant could again be omitted silently. This violates the finding's production-owner requirement. |
| Add a production option, setter, exported constructor, or shared test-support constructor | Rejected as over-abstraction for one Linux-only fixture. It enlarges production/API surface and introduces a second configuration path when the existing private dependency factory is already the approved seam. |

## Change-Surface Contract

| Field | Contract |
|---|---|
| Intended change surface | `.scratch/pr591-bot-r9/internal/process/contained_stream_linux_test.go` only, bounded to `TestRunContainedStreamPOSIX_ZombieOnlyGroupDoesNotFalseTimeout` and its local fixture-construction/early-failure reporting logic. No new file or reusable helper is required. |
| Approved extension seam(s) | The test-only `containedStreamDependencies.newChild` override, implemented as a decorator around the captured original factory. The only post-construction mutation is the existing `posixContainedChild.classifier` test seam. |
| Protected / must-not-touch surfaces | All production files, especially `internal/process/contained_stream.go`, `contained_stream_other.go`, `contained_stream_wait_deferred_posix.go`, `contained_stream_wait_linux.go`, `contained_stream_wait_kqueue.go`, and `contained_stream_wait_posix.go`; all non-F1 tests; README/catalogue F2 files; public APIs, manifests, and runtime flags. |
| Declared blast radius | One Linux-only PID-1 integration fixture. Production process containment, platform selection, wait/reap ordering, error contracts, and runtime behavior are unchanged. |

Dependency direction remains test fixture → private production dependency factory → platform child constructor. Production code has no dependency on test support.

Contract and persisted-state impact: **no external contract or persisted-state change**. No migration, compatibility window, rollout flag, or rollback of persisted data is needed. Rollback is removal of the single F1 test delta.

## Components and interaction

`defaultContainedStreamDependencies` remains the single composition owner for the production child factory (`internal/process/contained_stream.go:134-141`). The fixture decorates that factory only to inject its deterministic Linux classifier. The production factory still owns child creation, default classifier selection, and all platform wait-state initialization (`internal/process/contained_stream_other.go:46-49`). `runContainedStreamWithDependencies` invokes the decorated factory exactly once and otherwise follows the unchanged production lifecycle (`internal/process/contained_stream.go:252-280`).

No shared mutable state is introduced. Channel ownership remains on the production-constructed child: the deferred wait observer closes `waitObserved`, and the single reaper closes `reapDone` (`internal/process/contained_stream_wait_deferred_posix.go:15-20,31-42`). The fixture only supplies an invocation-local classifier callback.

## Failure modes and observable signals

| Failure mode | Observable discriminator |
|---|---|
| Fixture again bypasses production initialization | The exact PID-1 run fails before the `zombie-launched` handshake with `zombie fixture start: context deadline exceeded`, matching the confirmed QA failure (`review-r35-consolidated-qa.md:37,44`). Static review also finds a direct `&posixContainedChild{` or manual `initializePlatformContainedWait` in the PID-1 fixture. |
| Captured production factory returns an error or an unexpected concrete child | The decorator returns a distinct test-fixture construction error. The fixture's handshake wait must also observe early completion from `done`, so this is reported immediately as a containment/construction failure rather than being collapsed into a handshake timeout; production wrapping occurs at `internal/process/contained_stream.go:267-278`. |
| Classifier factory is moved out of invocation scope or not installed | Static guard finds the factory/tracking object outside the classifier callback; the PID-1 run either reports a cleanup lifecycle error with group snapshot (`internal/process/contained_stream_linux_test.go:355-362`) or fails the no-timeout oracle. |
| Signal/reap settlement regresses after successful handshake | `runErr` is not `context.Canceled`, or contains `ErrCleanupTimeout`; the test prints lifecycle stage, cleanup stage, causes, and group snapshot (`internal/process/contained_stream_linux_test.go:354-362`). |
| Direct child or adopted zombie survives cleanup | Existing `/proc` disappearance oracle fails with `pid <n> remains after product cleanup` (`internal/process/contained_stream_linux_test.go:364-376`). This distinguishes leaked identities from lifecycle-return failure. |

## Test strategy

1. Static F1 diff gate: the implementation delta for F1 names exactly `internal/process/contained_stream_linux_test.go`; production files in the protected list have no F1 delta.
2. Construction-seam guard: inspect the repaired fixture and require one call through the captured original `deps.newChild`; require no direct `posixContainedChild` construction and no manual `initializePlatformContainedWait` call in that fixture.
3. Invocation-local guard: require `linuxClassifierInvocation` and `linuxClassifierTestFactory("worker", ...)` to remain inside the installed classifier callback.
4. Run the exact QA Linux PID-1 shape: `go test -v -exec "unshare --user --map-root-user --pid --fork --mount-proc" ./internal/process -run "^TestRunContainedStreamPOSIX_ZombieOnlyGroupDoesNotFalseTimeout$" -count=1 -timeout=35s`. PASS requires the test to run as PID 1 (not skip), reach `zombie-launched`, return `context.Canceled` without `ErrCleanupTimeout`, and observe both process identities disappear.
5. Run the focused Linux process package: `go test ./internal/process -count=1 -timeout=120s`. This is the adjacent regression gate for the constructor, deferred-wait unit guard, settlement logic, and helper classifier.
6. Retain Darwin/BSD runtime behavior as explicitly unverified; the Linux fixture repair does not claim target-runtime proof for `kqueue` platforms.

## Diff-invisible invariants

| Invariant | Named regression guard |
|---|---|
| The production factory remains the sole owner of every present and future platform-child construction invariant. | **Production-factory decorator guard:** F1 diff inspection shows the wrapper calls captured original `deps.newChild`, and shows neither a child literal nor manual initializer in the fixture. Expected: exact match. |
| Classifier helper state is fresh per classifier invocation and never becomes shared fixture/package state. | **Invocation-local classifier guard:** source probe over the repaired callback finds both `linuxClassifierInvocation` and `linuxClassifierTestFactory("worker", ...)` inside the callback body. Expected: one local construction path, no lifted variable/factory. |
| Process-group signal precedes the exact-one direct-child reap, and the final wait result still reaches the lifecycle owner. | **Deferred-wait ordering guards:** the exact PID-1 command above and `TestR35DeferredWaitObservesBeforeSingleReap`. Expected: terminal PASS; no cleanup timeout; exact-one reap unit oracle remains green. |
| Successful product cleanup removes both the direct child and PID-1-adopted zombie. | **PID disappearance guard:** exact PID-1 command above. Expected: both `/proc/<pid>` entries become absent before the existing five-second deadline. |
| F1 does not alter production runtime, public contracts, or platform behavior. | **Protected-surface diff guard:** F1 implementation range contains exactly the Linux test file and no production/manifest/documentation path. Expected: no protected-path entry. |

## Claims

1. `{ guarantee: The fixture inherits every current and future invariant applied by the production child factory before applying its classifier-only test override; single-owner: defaultContainedStreamDependencies.newChild; enforcement-probe: Production-factory decorator guard plus the exact Linux PID-1 test }`.
2. `{ guarantee: The classifier factory and its invocation tracker remain fresh for each classifier callback invocation; single-owner: the PID-1 fixture's classifier callback; enforcement-probe: Invocation-local classifier guard }`.
3. `{ guarantee: The repaired fixture exercises the production deferred-wait lifecycle through handshake, cancellation, signal-before-reap settlement, final wait propagation, and disappearance of both identities; single-owner: TestRunContainedStreamPOSIX_ZombieOnlyGroupDoesNotFalseTimeout; enforcement-probe: exact WSL/Linux PID-1 command with terminal PASS and no skip }`.
4. `{ guarantee: F1 changes no production source, public contract, persisted state, manifest, or F2 documentation; single-owner: the F1 change range; enforcement-probe: Protected-surface diff guard }`.

This is a local, single-work-item decision and creates no `work-items/decisions/` record.

## Security and observability

No production trust boundary, subprocess authority, environment input, or diagnostic contract changes. The test retains its deterministic local classifier factory. Existing lifecycle diagnostics remain the receiving-side oracle; early factory/type failures must be surfaced distinctly instead of degrading into a generic handshake timeout.

## Terms and Abbreviations

- F1/F2: findings one and two in the R35 architecture review.
- PID 1: the init process in the test's Linux PID namespace.
- POSIX: Portable Operating System Interface family behavior used here for Unix-like contained processes.
- R35: the thirty-fifth bot-review correction round for pull request 591.
- WSL: Windows Subsystem for Linux.

## Gate decision

**PASS** — decorate the captured production `newChild` factory and override only the invocation-local classifier test seam. The design closes F1 without duplicating constructor invariants or changing production runtime; implementation and the named PID-1/focused probes remain required before the upstream review gate can pass.
