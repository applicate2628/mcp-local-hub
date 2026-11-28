# Independent review: scheduler cancellation identity

Reviewer: `/root/cancel_verification`, a separate nonauthoring agent. This is supplemental code and verification review, not a formal external-review verdict or publication authorization. No Go command, Windows process, GitHub mutation, or source edit was performed by this reviewer.

## Contents

- [1. Scope and identity](#1-scope-and-identity)
- [2. Code assessment](#2-code-assessment)
- [3. Regression-test assessment](#3-regression-test-assessment)
- [4. Qualification-runner assessment](#4-qualification-runner-assessment)
- [5. Evidence still required](#5-evidence-still-required)
- [6. Skills actually read](#6-skills-actually-read)
- [7. Separate CLI fixture correction for PR 658](#7-separate-cli-fixture-correction-for-pr-658)
- [Terms and abbreviations](#terms-and-abbreviations)

## 1. Scope and identity

Reviewed the complete base and candidate versions of `internal/scheduler/scheduler_com_windows.go` and `internal/scheduler/scheduler_com_windows_test.go`, the three patches, `hashes.json`, exact-base `go.mod`, and the supplied `scheduler_windows.go` excerpt covering the scheduler structure and `ListContext`. The declared base is `f5fb00370a833255eacb992a7cd32f2e68da7399`.

Independent Python calculations confirmed all four source SHA-256 values and Git blob identities against `hashes.json`. All supplied source-input contents match the local base bytes. These comparisons verify the supplied snapshot's consistency; the root agent owns comparison with the actual remote commit.

**Table 1. Reviewed artifact identities.**

| Artifact | SHA-256 |
| --- | --- |
| Candidate production file | `bddbb11722e1c1ac3a50edd85bb1acfef7278010ce4497aae87d22cb4a16442d` |
| Candidate test file | `5352797d1adb396ff006683b64ec63fe8490883e52c3967b30c243fa4accc2d6` |
| `candidate.patch` | `7d050c1cf2787b59b890b7e9f4865a798fd78ff1545461b9b6d71bd3c270d2dc` |
| `product.patch` | `0bc62272cc9bf969e4d0ced5f5c14924a62720c41c3dc2cf2ea2f08b20fa0706` |
| `negative-control.patch` | `695119ada26e2577602eea327f9dad8393784987cb9155cf02419b2b37e2cae6` |
| `check_cancellation.py`, initial reviewed revision | `818b4e940a2510fcd5516cd2fbe8992e9ed4b3f7a83883edd3ded1c176753f6e` |
| `check_cancellation.py`, revised positive-event checks | `bb41ce97ba8626ba77fede81eeb86b4108321fcd8959e3117969cd4948a99584` |

The candidate patch exactly equals independently reconstructed unified diffs of the two supplied source pairs. The product patch equals the production-file diff; the negative-control patch equals the test-file diff. The runner was parsed with Python's `ast` module without importing or executing it.

## 2. Code assessment

**No required production-code correction was found in the reviewed delta.** Runtime verification remains separate.

The defect has two layers. In the base production file at lines 132–133, the runner notices a stopped context but returns only `ErrUnavailable` plus the text `timeout`. Its caller at line 153 then formats any runner error with `%v`, retaining the text while discarding the runner cause from the error structure. Either loss prevents `errors.Is` from recognizing the original context error.

The candidate repairs both producing boundaries. At lines 132–133 it reads `ctx.Err()` once and wraps that nonnil error together with `ErrUnavailable`. At line 153 it wraps the runner error as a cause while retaining `ErrUnavailable`. Thus callers can test both the established availability classification and the specific cancellation/deadline classification. An unrelated runner cause is preserved too.

On an error, `schedulerCOM` still returns the zero response before checking `response.OK`. A runner reporting both an apparently successful partial response and an error cannot leak that partial result. In the exact-base public `ListContext`, lines 371–374 return the error directly and return a nil task slice. The new public tests therefore exercise the entire relevant error-propagation chain.

Nil context behavior is unchanged: `schedulerCOM` substitutes `context.Background()` at lines 146–147, and the public method does so at lines 368–369. The 15-second derived deadline and deferred cancellation are unchanged. The change does not introduce goroutines, channels, locks, background work, new process creation, dependencies, or additional Task Scheduler operations. Cancellation still takes precedence when `ctx.Err()` is nonnil after command completion; that ordering predates this change.

The production change is minimal: three changed source lines in two error paths, with no new helper or abstraction. The existing standard-library error mechanism is sufficient.

**Compatibility qualification:** `errors.Is(err, ErrUnavailable)` is preserved. Multiple `%w` operands produce an error with multiple causes; a caller using only `errors.Unwrap(err)` or an `Unwrap() error` type assertion will not see the same single-child structure as before. No such consumer occurs in the supplied scoped files. A whole-repository caller scan is owned by root and is required before claiming that there are no affected direct-unwrapping callers. The outer formatted error text stays the same; the inner stopped-context text intentionally changes from generic `timeout` to the context error text.

**Privacy assessment:** the outer wrapper already included the runner error text before this change. Wrapping it structurally does not newly print request data. The newly exposed context texts are standard cancellation/deadline messages. Test inputs use synthetic names; the delta adds no credentials, personal task names, machine-specific paths, or logging of request bodies.

## 3. Regression-test assessment

**Table 2. Test invariants and discriminating power.**

| Cases | Invariant | Defect they detect |
| --- | --- | --- |
| Runner `canceled`, `deadline`, `bridge` | Error matches both `ErrUnavailable` and the supplied wrapped cause; no partial response escapes | The outer `%v` loses the runner cause |
| Public `ListContext` `canceled`, `deadline` | Production runner and public method preserve both classifications and return nil tasks | Either production layer discards the stopped-context cause |

The bridge case strengthens the superseded generic availability-only test by preserving the exact sentinel through an extra wrapping layer. The partial-response fixture asserts useful externally visible failure behavior. These are five distinct regression cases, not five separate test functions.

The public fixtures are deterministic: one context is canceled synchronously, and the other has a deadline at `time.Unix(1, 0)`. Each fixture checks its own `ctx.Err()` before use. No sleeps or timing window are involved. The public method does not depend on `windowsScheduler` fields on this error path, so its zero-valued receiver is sufficient.

The new mock tests use the existing `withCOMRunner` helper, whose cleanup restores the previous package-global function. The reviewed file has no `t.Parallel` call. Whole-package parallelism and initialization checks remain with root; global replacement must not overlap any other reader/writer.

The negative control is appropriately test-only: old production source plus the new tests. All five cases are expected to fail specifically at the error-identity assertions. A negative-control exit code by itself is insufficient; the runner's exact failing-case and assertion-text checks address that ambiguity.

The tests do **not** demonstrate cancellation after PowerShell starts, descendant-process termination, elapsed-time bounds, freedom from leaks, or behavior of a real Task Scheduler service. They exercise the production runner with a context already stopped before command execution. The two existing PowerShell fixtures selected by the focused run replace the COM connection with a synthetic exception or construct a synthetic response before the original script body, respectively. Their inspected bodies do not enumerate, create, start, stop, or delete native scheduler tasks.

## 4. Qualification-runner assessment

The following observations refer to the runner revision in Table 1. They are verification-boundary qualifications, not defects in the production change.

1. **Source admission is appropriately narrow.** `admit`, lines 99–108, requires the intended HEAD, an empty index, the exact expected dirty path set, and matching bytes for both scoped source files. It also rejects untracked and ignored files reported by Git. The control phase clones the admitted existing repository with `--no-hardlinks` and uses a detached exact base. Neither merge, rebase, cherry-pick, nor installation is present.

2. **Normal-completion capture preserves both streams.** `run`, lines 70–91, sends standard output and standard error directly into exclusive-create files, waits for the direct child, records its exit status, and hashes both streams. The main receipt includes command rows and is written in `finally`. A JSON parsing error after process completion leaves the captured raw streams intact and fails the phase. Stream durability on abrupt termination or host loss has not been demonstrated.

3. **Outer resource ownership must remain explicit.** Line 78 waits without a timeout. The Go test timeout does not bound compilation, `go build`, `go vet`, or all descendants. The receipt accurately states `whole_process_tree_deadline_enforced: false`. Root must retain an admitted external owner and its supported bounded-wait/cancellation/settlement semantics, or describe the actual limitation. A false receipt flag is disclosure, not proof of resource settlement.

4. **A successful test exit does not itself prove the expected test inventory.** `require_pass`, lines 94–96, rejects nonzero exit status and failure events, but does not require the seven intended top-level test functions, their subtests, both `-count=2` rounds, or an empty skip list. The final evidence review must verify those events explicitly. The selected names presently match the reviewed file.

   **Resolved in the revised runner in Table 1:** independently inspected `confirm_positive` and its focused/published call sites. It compares a `Counter` of actual pass events with all seven parent functions and eight subtests twice, plus one package pass, and rejects skip events. The revision also adds two adjacent CLI timeout tests with their own exact count check. Their test bodies have not been supplied to this reviewer yet, so the no-native-operation assessment above remains confined to the seven scheduler test bodies.

5. **Published HEAD requires separate whole-delta evidence.** The `published` phase admits the supplied HEAD, clean source state, and the two candidate hashes. It does not itself prove that the whole commit differs from the base only in these two files, nor prove the commit's parent relationship. Root must retain and compare that publication delta and ancestry separately.

6. **The tool and environment choices are bounded in scope.** The runner uses existing Go, Git, and C compiler paths; disables module/toolchain retrieval; and routes declared runtime, temporary, profile, and cache writes to the allowed drive. It uses `go build ./...` and `go vet ./...`; these commands are broader compilation/analysis, not a full repository test run. Runtime isolation of all ambient configuration and package initializers is not established solely by the environment dictionary.

## 5. Evidence still required

No runtime result was available at initial review. Before completion claims, independently inspect the retained negative-control events, focused race-test events, build/vet outcomes, source identity before and after commands, captured stream hashes, and final published delta. Check archives against their manifests if evidence is archived. Root's own review remains required and is not replaced by this memo.

Root subsequently reported the expected five negative-control child failures and package-level scans for direct unwrapping, parallel tests, and `TestMain`. Those reports are useful handoff context; this reviewer has not yet inspected their underlying retained evidence and does not count them as independently verified results.

The directly inspected code supports the claimed error-propagation correction. It does not support a claim that post-start cancellation, process-tree lifecycle, real scheduler behavior, or every repository test was verified.

## 6. Skills actually read

- `c29/go-testing-and-verification`: invariant-first testing, negative-control oracles, and accurate limits of runtime evidence.
- `c30/go-concurrency-lifecycle`: context lifetime, error propagation, ownership, and the distinction between passing context and proving process termination.
- `c38/code-review-and-quality`: tests-first, independent review across correctness, simplicity, architecture, security, and performance.

The supplied shared-governance file was searched and portions inspected; this memo does not claim it was read in full. No skill guidance was used to extend the delegated mutation or execution scope.

## 7. Separate CLI fixture correction for PR 658

This addendum reviews a separate, test-only candidate based on `4f3513d886e301d7a358884a34896402f99e8002`. It does not integrate the PR 660 production fix. Root reported that the original 25-millisecond fixture failed twice both with the PR 660 candidate and on pristine `f5fb00370a833255eacb992a7cd32f2e68da7399`, while the adjacent default-timeout check passed. Those execution results remain reported context until their retained streams are independently inspected.

### Identity and minimality

Independently recalculated the single base/candidate file's byte count, SHA-256 and Git blob identity; all match `cli-fix/MANIFEST.json`. Independently reconstructed `cli-fix/candidate.patch`, which matches exactly. All bytes outside the changed test and its comment are identical, including the adjacent `TestReconcileIPC_HandlerTimeout`.

**Table 3. Separate CLI fixture candidate identities.**

| Artifact | Identity |
| --- | --- |
| Base test file SHA-256 | `09eb9dbc9b09a5cd3fec4a3d69510aa99d0df69c7c6012bb8ced3f48464e7d0b` |
| Base test Git blob | `623e9b74fd5b23c0ce53d2982e6621b0f4c3c0e7` |
| Candidate test SHA-256 | `e41e7500eb6e60b2abef2f590ad676810f46b90983bf7811cb4a248234e008d4` |
| Candidate test Git blob | `4fbe28e28679c5a48b4731057683ac6f19607137` |
| Candidate patch SHA-256 | `bb58699e2a0b8c2b463cc61b612e22a5735426fc4373689ec0dfe0c58ec86761` |
| `cli-fix/check_cli.py` reviewed SHA-256 | `06cb0f648dd8137919247fe5dd4c23e91c884a2ac73d799f073a9ad01f24fe55` |

The supplied full handler context has one additional terminal newline: its local Git blob is `0ac2547f6afe41c9887990b0956f47c9be1669d8`; removing exactly that final byte yields the reported source blob `65af6c787bbab9c2c48195eb98225ce7c0b8558b`. This reviewer did not change the context file. Root reports that the actual handler blob is identical at the two branch bases. The local extra newline has no effect on the reviewed control flow and is outside the candidate.

### Code conclusion and oracle

**No required correction was found in this test-only delta.** The scope and test name accurately state the new property: parent cancellation reaches an already-entered context-aware scheduler list. The adjacent test still checks the configured 25-second deadline; neither test now claims to wait for that deadline to expire while scheduler work is active.

At candidate test lines 1186–1188, the test derives a cancelable parent from the fixture's context, registers cancellation cleanup, and assigns the derived context before invoking the handler. The callback at lines 1192–1207 first checks the expected scheduler prefix and rejects an already-stopped context. Only then does it call `cancelParent`. A missing parent-to-list cancellation link fails after the one-second guard; a successful path sends the observed error into a one-slot buffered channel before returning it.

This removes the defective dependence on a 25-millisecond timer winning during file/registry setup. The one-second branch is a failure bound after explicit callback entry, not the trigger or success condition. The test introduces no production timeout increase, dependency, production seam, or new goroutine.

The failure oracle remains substantive. It requires a `RECONCILE_TIMEOUT` response and an observed `context.Canceled` from the entered scheduler context. If the list callback never runs, the final nonblocking receive fails. If legacy `List` is used, the existing fake at lines 1755–1756 returns an explicit failure. If the production bridge substitutes an unrelated context, the callback fails its cancellation guard. A premature stop fails the initial `ctx.Err()` check. These outcomes distinguish cancellation propagation from merely seeing a generic timeout response.

### Concurrency and cleanup

The `fx.ctrl.ctx` assignment does not race with a context read in the inspected scenario. The fixture's sole event-loop goroutine receives the original local `ctx` at lines 259–264; it does not repeatedly read the mutable controller field. The new assignment precedes the main goroutine's handler call. The handler reads the field through `baseReconcileContext`, lines 617–623, and derives its timeout at line 193. The context-aware scheduler branch at lines 64–69 invokes `ListContext` synchronously, so callback assertions execute on the test goroutine.

The request has `apply: false`, no target and no stop batch. The inspected handler reaches the scheduler snapshot without posting an apply event, then returns at lines 367–368 on cancellation, before any apply operation. The fixture has no independent event producer. Consequently, the registered event handlers do not concurrently read the replaced field in this test. This is a scoped control-flow assessment, not a claim about every controller use.

The constructor seam is restored by `defer uninstall`. Cleanup cancels the derived parent, then the fixture's original cancellation cleanup stops and joins the event loop, with its existing two-second failure bound. Canceling the replacement parent does not stop the original loop, but its original owner remains intact and performs that shutdown. The buffered observation send cannot block on the post-handler assertion. No added goroutine requires separate joining. The unchanged pre-list asynchronous file reader completes before the callback is reached in the successful test path.

### Qualification runner

Read the complete `check_cli.py` and parsed it without execution. Its exact-base/tree admission, one-file source hashes, test-only patch application, and exact two-functions/two-rounds positive event counter fit this isolated correction. Baseline success requires the exact two historical failure messages and the two adjacent passing events. Candidate and published phases require both replacement and unchanged tests to pass twice, with no skip events. Build and vet remain separate commands.

The same limits identified in section 4 remain: direct children are waited for, but no outer wall-time or complete process-tree deadline is enforced; published whole-commit ancestry/delta needs separate evidence. This runner hashes the prior baseline receipt before candidate admission but does not revalidate baseline stream hashes there, so archive verification must explicitly check the baseline streams. The declared lack of actual deadline-expiry qualification applies to the replacement test; it must not be used to deny timeout observations in the deliberately failing historical baseline.

The source and fixtures inspected here use synthetic tasks and redirected state/configuration roots. The selected cancellation callback replaces construction of the native scheduler. No new native-task operation or user-state access is introduced by the patch. Runtime race checks, exact-base reproduction, and final artifact/archive verification remain to be inspected independently.

## Terms and abbreviations

- **COM**: Component Object Model, the Windows component interface used by the scheduler bridge.
- **Negative control**: running the new regression tests against the unchanged production implementation to confirm they expose the intended defect.
- **SHA-256**: a cryptographic file-content digest used here to bind review and execution evidence to exact bytes.
- **Git blob identity**: Git's content-addressed identifier for a file's exact contents.
- **Race test**: execution with Go's data-race detector; evidence applies only to paths and schedules actually exercised.
- **HEAD**: the Git commit currently selected in the working checkout.
- **CLI**: command-line interface.
- **IPC**: inter-process communication; here, requests and responses between a client and the supervisor.
