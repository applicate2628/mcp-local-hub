# Native cleanup across FFI: results and review

**Qualification-code commit:** `c3fca608507d2c5a655e583116f06950c6d8dd4c`.
**Starting head:** `a59a867d0d8a44d156ab783c78f91a02fe4d83e5`.
**PR:** [#631](https://github.com/applicate2628/mcp-local-hub/pull/631), based on unchanged native-owner [#625](https://github.com/applicate2628/mcp-local-hub/pull/625).

## Contents

1. [Result](#result)
2. [Verified cases](#verified-cases)
3. [Recorded runs](#recorded-runs)
4. [Self-review](#self-review)
5. [Reproduction and remaining work](#reproduction-and-remaining-work)

## Result

Closed the previously unqualified **selected real Windows cleanup-failure -> native Rust settlement -> DLL -> Go** path. This is no longer inferred solely by combining independent native tests with binary-encoding unit tests.

No runtime defect was discovered by this gate. The existing native owner and foreign result mapping behaved as intended. **No production implementation, ABI, dependency or installation changes were needed.** Four added test/instrumentation files construct and verify a disposable qualification artifact. Documentation/evidence is committed separately.

The runner appends a read-only original-slot accessor only to a copied native/FFI source tree. The actual CloseHandle calls, state transitions and ordinary result encoding remain the same in the positive run. Tests apply Windows close protection to exact resources they own, observe a genuine OS failure, and compare it with the error received through Go. No return value is mocked.

The normal debug/release DLLs do not export the qualification accessor. Their hashes and all original source hashes are checked before and after the isolated qualification. This is not a new public recovery function or an environment-triggered production fault switch.

## Verified cases

**Table 1. Failure matrix.**

| Mask | Protected original handles |
|---|---|
| 1 | Primary thread |
| 2 | Root process |
| 3 | Thread and process |
| 4 | Windows Job |
| 5 | Thread and Job |
| 6 | Process and Job |
| 7 | Thread, process and Job |

Each mask was exercised through both `Finish` and `FinishContext`: **14 subcases**. The finite child exited with code259. Go received `CleanupFailed`, known root exit259, the exact failure mask and the matching native error for every selected resource. The OS code was measured on the protected original rather than hardcoded; this host returned6 for the selected mechanism.

Repeated finish calls kept the original outcome. A terminal-but-unreleased owner still prevented library unload. Releasing the Rust allocation did not itself clear the protected native resources. The test then reclaimed those known resources under its exceptional fixture authority. A separate case verifies that recovery does not retroactively turn the historical failure into success.

The protection/recovery helper checks the actual native slots, not only the decoded Go outcome. When the owner still has a slot, it restores flags without taking ownership. When the slot has been detached, continuously verified protection establishes that the exact original handle remained live and permits test-only reclamation. It refuses ambiguous numeric-handle cleanup. No PID reopening, process census, stale-handle retry or global cleanup occurs.

**This recovery knowledge is specific to the fixture.** It does not justify retrying an arbitrary production close error. Cached outcome equality alone is also not a proof of every syscall schedule; the unchanged native terminal branch and explicit fixture observations define the tested scope.

## Recorded runs

All paths below are in Git. The final native and ordinary source manifests match the qualification-code bytes in `c3fca608`. Earlier runs retain their own earlier test/runner hashes (before diagnostic logging/header refinements) and are not claimed to match the final source. No run is presented as a clean checkout of the later evidence commit.

**Table 2. Runs and their meaning.**

| Run | Result |
|---|---|
| [Initial preparation](../runs/native-cleanup-initial/PREPARATION_FAILED.txt) | Harness stopped before any native test because an already-existing temporary directory was recreated without permitting reuse. The original runner hash and cause are preserved |
| [First positive qualification](../runs/native-cleanup-positive-1/steps.json) | Real native cases passed. This characterizes existing behavior, not a newly fixed runtime bug |
| [Wrong success-state control](../runs/native-cleanup-negative-failure/10-debug-native-close.log) | Deliberate `CleanupFailed -> Complete` reporting caused the intended Go assertions to fail |
| [Missing failure-mask control](../runs/native-cleanup-negative-mask/10-debug-native-close.log) | Deliberate mask loss was detected even though error codes/root result were still present |
| [Final positive qualification](../runs/native-cleanup-final/steps.json) | All15 expected steps matched; both DLL profiles passed, with exact observed result values in the logs |
| [Full ordinary suite](../runs/native-cleanup-ordinary-final/steps.json) | All22 expected steps matched on the unchanged ordinary implementation and the new opt-in test source |

The negative controls change only one encoding statement in their disposable source copies. They do not fake the native failure or alter the ordinary DLL. Their logs contain the failing semantic assertions, not merely a failed compilation. Fresh unmodified copies were used for the final positive run.

**Table 3. Fresh verification scope.**

| Layer | Result |
|---|---|
| Opt-in native cleanup tests | Two behavioral functions,14 matrix subcases, debug and release DLLs with `-race -count=3` |
| Normal-artifact control | Third opt-in function confirms missing-symbol127 for the accessor in each ordinary DLL |
| Instrumented Go/cgo checks | `GOEXPERIMENT=cgocheck2` and `go vet -tags ffi_native_faults` passed |
| Instrumented Rust static check | Clippy with `-D warnings` passed; no extra dependency/version introduced |
| Ordinary Go module | **47 top-level functions,31 nested cases,0 skips**; debug/release DLL race x3, cgocheck2, vet and formatting passed |
| Ordinary Rust ABI | **14 unit tests**, debug/release, Clippy and format checks passed |
| Unchanged parent native owner | **53 checks**, debug/release, Clippy and format checks passed |

The new three opt-in functions are not counted as skipped normal tests or added to the ordinary47. Debug/release profiles and repetitions are configurations, not additional independent test functions. A deliberate exit7 step validates exit capture in each runner and is not a product failure.

Environment: Wave, Windows x86_64, existing Rust1.99.1/MSVC and Go1.27.3/cgo with installed gcc, cache-only/offline/locked. All new writable files were restricted to `D:\dev\ChatGPT`. No installed Hub, external provider/model, actual user environment dump or paid run was used.

## Self-review

**Table 4. Eleven selected skills, read and applied.**

| Skill | Applied decision |
|---|---|
| Using Superpowers | Select the outstanding native-failure gate rather than repeating completed stdio/context work |
| Executing Plans | Recover from PR and committed ledger; keep source/evidence commits and unresolved gates explicit |
| Test-Driven Development | Define outcome assertions first; characterize existing code, then use two deliberate wrong behaviors to prove the oracle distinguishes them |
| Using Git Worktrees | Verify the existing private clone and isolate all instrumented source changes in disposable copies |
| API and Interface Design | Preserve normal ABI and distinguish call success, terminal cleanup failure, wrapper release and actual resource release |
| Architecture Review | One native owner; no new manager, recovery service or global registry; no transfer of Windows evidence to other OSes |
| Rust Unsafe Checker | Exclusive owner borrow, aligned pinned output, no retained Go pointer or adoption of a second native owner |
| Rust Resource Lifecycle | Test recovery distinguishes still-owned slots from detached continuously protected resources; no automatic retry |
| Systematic Debugging | Diagnose the runner's existing-temp-directory failure before changing only preparation behavior |
| Ponytail Review | Two tiny appended accessors instead of a production fault framework; standard OS flags and existing result path |
| Verification Before Completion | Fresh ordinary and instrumented suites, input/artifact hashes, separate negative results and exact publication checks |

Rust skills were retrieved read-only through omgskills, pinned to `actionbook/rust-skills@5c40d3ad785193231b7d0dbfb8e1eb447e5edd94`; no installation or agent run occurred. Microsoft handle APIs and Rust raw-handle ownership documentation were checked as primary references; see [the method and reference list](README.md#limits-and-sources).

Review of unsafe boundaries found no arbitrary pointer fabrication: the only owner passed to the accessor is the live allocation returned by the selected DLL, under its existing Go mutex. The output is a pinned integer array, with no Go pointers retained. The additional loader reference belongs to the test and is released before the normal library cleanup; it is not production lifetime policy.

No independent reviewer or external agent was used. Absence of a discovered defect is not proof of exhaustive safety. The ordinary API remains subject to its existing trusted-pointer and in-process panic/abort limitations.

## Reproduction and remaining work

[README.md](README.md) documents the complete setup, native fault mechanism, guard/recovery rules, exact negative mutations and all commands. [run.ps1](run.ps1) creates a new source copy and run directory and refuses to overwrite either. It does not install, push, delete files or modify ordinary source/DLLs. [The full runner](../run.ps1) reproduces ordinary tests from a checkout without an uncommitted environment script.

The initial preparation defect was corrected by treating the run/report directories as new but reusing the already permitted temporary directory. No native process had started at that failure. The empty aborted copy was confirmed empty and moved directly to the approved trash; the error record stayed outside it. No trash was emptied.

The added coverage is **protected CloseHandle failures for these three original Windows resources**. Remaining failures include other acquisition/query/loader/allocator/panic/cancellation mechanisms and races, complete product recovery/admission/stream policies, other descriptor profiles, native Linux/macOS backends, workload/performance comparisons, independent review and installed acceptance. No whole-repository, product deployment, speedup or complete migration claim is made.

Source code, failed/negative/successful logs, exact commands, identity manifests and limitations are retained in this PR. Caches, instrumented binaries and disposable source copies are rebuildable and are not the sole copy of any necessary result.

### Disposable-copy cleanup

After all observed test/build processes finished, four instrumented source copies
were compared with their complete recorded manifests and confirmed reproducible.
They and the now-unused instrumented target were moved directly to the approved
trash, not deleted. Movements are recorded in `disposable-copies.json` and were
checked at source and destination. Ordinary DLLs, active checkout, Cargo cache,
all evidence and sole required source copies remain outside trash. Trash was not
emptied; manual removal of its contents cannot erase a required result.
