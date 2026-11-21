# R25 — Serena constraint pin: focused qualification candidate

## Contents

1. [Decision and source](#decision-and-source)
2. [Contract and test boundaries](#contract-and-test-boundaries)
3. [Native effects and resource settlement](#native-effects-and-resource-settlement)
4. [Qualification selector](#qualification-selector)
5. [Own review and residual work](#own-review-and-residual-work)
6. [References](#references)
7. [Terms and abbreviations](#terms-and-abbreviations)

## Decision and source

Add one test file to the existing **PR #648**. No production correction is justified by the inspected evidence. The specific uncovered contract is the receiver of the fixed build-constraint file's retained read handle: pending native/input-output outcomes must retain its write/delete exclusion; a known local preflight or a genuinely settled original no-child outcome may release that pin; a failed native close must remain visible through later closes and the owning tree.

**Current candidate: revision 3, root execution scope `pr648v3`.** Root's independent source review rejected revision 1 before runtime: its assertion incorrectly searched `runErr.Error()` for `empty command path`. At the exact target, `ContainedRunError.Error()` intentionally returns the fixed safe text `contained process start failed` (`internal/process/contained_stream.go:65–70`). The deferred receiver copies the original `Cause` into `settled.PrimaryError` (lines 312–318). Revision 2 corrected only that assertion with a non-nil `PrimaryError` guard. Revision-1 candidate, patch, selector and review bytes remain verbatim under `rejected-before-runtime-v1/`, with their original hashes and rejection record.

Root then attempted revision-2 Windows qualification. Compilation failed because the pinned `golang.org/x/sys/windows` v0.47.0 does not export `HANDLE_FLAG_PROTECT_FROM_CLOSE`. The retained root receipt records exit 1, package failure, no test PASS and no timeout; **no native test body executed**. The submitted local revision-2 artifacts are preserved verbatim under `compile-failed-before-tests-v2/`, with the original candidate/patch hashes and compile-receipt identity in `FAILURE.json`; root retains the formatted remote preimage and original output independently. Revision 3 adds only a test-local `uint32` constant with the Windows SDK value `0x00000002` and replaces four symbol references at three existing `SetHandleInformation` call sites. It changes no production code, dependency or API. Revision 3 is not yet compiled or executed by this lane.

The source heads were read back through the GitHub connector in this session. Both remain open, unmerged Draft PRs:

| Table 1. Responsibility | Exact head | Exact tree | Result of this lane |
|---|---|---|---|
| #648: constraint acquisition and pin lifetime | `fc4dc5c7145e4f4d709b91654ab4bc1fd7abe259` | `47a6d86501573cb96e45b469365d8f54c18ea22d` | One new test-only candidate. |
| #652: original-generation read ownership | `b079e4c9503f080d8ab09742270c89a3bf88d7d5` | `4d4e3a0d44b23b383d0db17e06101017e541934b` | No new concrete production defect established in the inspected reader/batch/leaf paths; no candidate. |

The reviewed #648 receiver is `internal/api/serena_prepared_build_constraint_windows.go:141–185`; native close caching and its tree receiver are `internal/api/serena_owned_tree_windows.go:28–38,591–610`. Existing #648 tests cover the receipt shape, policy projection, unissued-attempt refusal and empty-invocation preflight; none exercises the native pin's protected access or actual close failure. The PR's own remaining-verification paragraph explicitly leaves complete constraint-file/native settlement acceptance open [1].

Candidate: `overlay-pr648/internal/api/serena_prepared_build_constraint_pin_windows_test.go`, 200 added lines. Target-relative patch: `pr648-target-relative.patch`. Exact content and source hashes are in `qualification-selector.json`. The target path is absent at the exact #648 head. The new tests use existing production entrypoints and introduce no production abstraction, dependency or test hook.

Revision-3 test SHA-256: `d75864f7d0f8517249ae7a68317f94735cf4589327e1398a757ea120ef997f68`. Revision-3 patch SHA-256: `d527850e5370c952bbf8630587ec7e30ebc7fa0fedc57588c8cabde5a0efe38e`. Git blob: `0798bba831c055965be0b4326a44b3c4bf97346b`.

## Contract and test boundaries

**Table 2. Scenarios and qualifying observations.**

| Scenario | Real observation | Synthetic input and limit |
|---|---|---|
| Local preflight releases an unused pin | Actual `runPreparedSerenaInvocation` rejects an empty invocation before entering the contained runner. `finishPreparedSerenaBuildConstraintUse` closes a real file handle; fresh write/delete access probes then succeed. | The private tree association and in-use state are deliberately constructed in the test. No attempt acquisition is claimed. |
| Missing native settlement retains the pin | Fresh write/delete access requests receive the Windows sharing violation before and after finish/close. The read handle has not been close-attempted. | `containedAttempted=true` with no native receipt is a deliberate negative receiver input. No complete receipt is fabricated. |
| Incomplete input/output settlement retains the pin | The same native exclusion remains after both receiver calls. | The actual settled receipt is copied by value and only its stdout settlement is degraded to a started, incomplete worker. The original native receipt remains untouched. This is a negative input, not an observed real worker failure. |
| Original no-child settlement releases the pin | The actual contained runner creates an empty Windows Job and five stream handles, rejects an empty command before process creation, observes its own zero-child accounting and closes those resources. Its original native receipt reaches the pin receiver; fresh access probes succeed after release. | The pin owner is synthetic. The complete native receipt is **not** hand-built. No child process, executable qualification or installation is exercised. |
| Native close failure remains sticky | A real file handle is marked `HANDLE_FLAG_PROTECT_FROM_CLOSE`; `CloseHandle` actually refuses. After removing that flag, later pin close and tree close retain the original error and do not retry it. Fresh access probes still show that the handle is live. | Protecting this test-owned handle injects the failure condition. This is not a naturally occurring production close failure and is not a machine-policy change. |

All successful file probes request access only. They neither write bytes nor set deletion disposition. The first root also checks that the fixed file bytes remain unchanged. There are no new timer sleeps, parallel subtests, mutable global seams, external network calls or skipped branches.

The fixtures intentionally leave creation observations, namespace policy and publisher authority absent. They do **not** call `AcquireSerenaOwnedTree`, `acquirePreparedSerenaBuildConstraint` or `beginPreparedSerenaBuildConstraintUse`, and do not fabricate finalized resource observations. Their use of the private finish/close receiver is not full admission, full acquisition or complete installation acceptance. The tree close in the error case must remain non-complete; its other unissued-owner obligations are not suppressed.

## Native effects and resource settlement

The original-runner call uses `process.RunContainedStreamObserved(ctx, &exec.Cmd{}, ...)` with a ten-second context, a one-second cleanup allowance and an absolute cleanup deadline equal to that original context deadline plus one second. Both output byte limits are 1,024 bytes and stderr is `io.Discard`.

The source trace at the exact head is:

1. `internal/process/contained_stream.go:274–465` validates the finite context and original absolute deadline, creates the native child owner, acquires two anonymous pipes and null stdin, and records entry to start.
2. `internal/process/contained_stream_windows.go:48–55,80–92` creates the dedicated Windows Job and rejects empty `cmd.Path` before `CreateProcess`, command-environment preparation, process/thread creation or worker goroutines.
3. `internal/process/contained_stream.go:350–378,423–447` closes the five acquired stream slots and receives original native settlement through the original child closer.
4. `internal/process/contained_stream_windows.go:398–440` queries that original empty Job's accounting. `closeOriginalWithPermit` closes the Job. `internal/process/contained_stream_observed.go:50–86` requires source-owned native completeness and every acquired stream's close outcome.

The test explicitly requires a non-nil error, the start stage and original `PrimaryError` containing `empty command path`, `StartAttempted`, unknown root identity/wait, no stdout/stderr worker, actual acquisition and successful close of all five stream slots, original native completeness and `NoChildKnown`. It does not expect private cause text in the deliberately safe outer error string. A mere wrapper exit or a default zero-value input/output receipt cannot satisfy this oracle. The deadline is an existing API input contract; this review does not assert that every synchronous Windows call has an independently enforced wall-clock timeout.

Each fixture creates one ordinary private file under `t.TempDir` and immediately registers cleanup after opening its read handle. Cleanup is therefore active before any protected-handle mutation or fatal assertion. If the production receiver already closed it successfully, cleanup does not touch the numeric handle again. Otherwise cleanup clears `HANDLE_FLAG_PROTECT_FROM_CLOSE` and closes the still test-owned handle directly. These final test cleanup actions do not overwrite the receiver's original failed-close record. Any clear/close failure is reported as a test error. The test framework then disposes its own temporary directory through the existing repository test infrastructure.

There is one real empty Job, five original stream handles, and five file fixtures across the two roots. No source-qualified publisher image, Product, installed Hub, Supervisor, scheduler task, live listener, persistent setting or system-policy mutation is requested. The handle flag's documented scope and behavior are given by Microsoft [3].

## Qualification selector

Execute on **Windows**, on the exact #648 head with this single file overlaid, using the root's already admitted isolated test runner and existing dependency cache. The candidate does not require build tags.

```text
Package: ./internal/api
Selector: ^(TestPreparedSerenaBuildConstraintPinSettlementNative|TestPreparedSerenaBuildConstraintPinCloseFailureNative)$
Expected: 2 roots; 5 leaf cases; 6 test PASS events; 1 package PASS event.
```

```sh
go test -race -count=1 -timeout 90s -run "^(TestPreparedSerenaBuildConstraintPinSettlementNative|TestPreparedSerenaBuildConstraintPinCloseFailureNative)$" -json ./internal/api
```

Require both exact root names and all four subtests of the first root. Any FAIL, SKIP, incomplete root, missing root or package failure is not qualification. The race detector supplies evidence only for the executed schedule; this selection creates no child process and no contained stream workers.

Before the root's run:

- Read the whole new file and format it with the installed supported `gofmt`; refresh hashes if bytes change.
- Keep all temporary roots and writable Go caches under the authorized work area. Reuse the existing offline build/telemetry controls; do not download a toolchain or dependency.
- Clear `MCPHUB_TEST_CALLER_START_TIME_CHILD_STATEDIR` and `MCPHUB_TEST_CALLER_START_TIME_CHILD_DELAY_MS`. The API TestMain consumes the first sentinel before `m.Run`, so inherited values could bypass the intended roots entirely.
- Preserve the original TestMain sandbox and live-process/Supervisor/autostart seals. Do not select the broader Serena migration tests.
- Capture exact source-plus-overlay hashes before/after, nested JSON outcomes and cleanup result. A run on this source-plus-overlay does not automatically qualify a later different tree.

Optional falsification, only if the root needs an oracle check: in a separately owned temporary copy, remove only the pending guard at `serena_prepared_build_constraint_windows.go:149–151`. The two incomplete-settlement leaves must then fail because the pin is closed. Do not publish that counterfactual. This mutation is not needed to claim an actual pre-existing production defect; none is asserted here.

## Own review and residual work

The new file was read end to end, including each test exit, handle close, source invocation and receipt copy. The initial own review missed the safe outer-error contract; root's independent review caught it before runtime. The exact error producer, outer formatter, typed cause receiver and corrected nil-guarded assertion were then reread for revision 2. The unverified assumption that the pinned Windows package exported the SDK flag was subsequently contradicted by the real revision-2 compiler result. Revision 3 removes that package-symbol assumption with the documented native value and a test-local typed constant; its four replacements leave handle acquisition, flag clearing, close receipt and cleanup order unchanged. The inspected production/runner files are byte-identical between the local source cache and the exact target for the relevant trace; the manifest pins their blob and SHA-256 identities. Existing worktrees remain untouched and clean. Static `git apply --check --whitespace=error-all` passed again for the revision-3 new-file patch, and the new path's absence was independently checked at the exact target head.

**Current revision-3 execution status: NOT_RUN in this lane.** The container's `/usr/bin/go` does not accept `version`; it is not the supported Go toolchain. No `go`/`gofmt` executable was found in the probed standard runtime paths or the existing workspace artifacts. No Go compilation, Windows test, native close or native Job call was executed by this subagent. Root's separate revision-2 compilation failed before the test bodies, as preserved above. No remote execution, installation, dependency download or GitHub write was performed by this subagent. The main agent owns source review and any subsequent Windows run.

No production logic was modified. The candidate does not broaden the cleanup/authority scope or transplant #652 into #648. Retain the full production constraint acquisition, begin-use identity/hash/policy, successful child/installation, original namespace/publisher and full-suite gates as **not verified**. The #652 outer qualification and native intermediate-reparse/close-failure branches remain separate open work. Existing historical integrated receipts stay bound to their recorded source; the new tests cannot make those receipts apply to this head.

Skills applied: Architecture Review for receiver/owner boundaries; MCP Boundary for evidence scope and no false qualification; Go Testing and Verification for the native oracle; Go Concurrency Lifecycle for original settlement and resource ownership; Ponytail Code Simplification for keeping one owner, existing seams and a test-only delta.

Execution role: focused source reviewer and test author. Assigned internal role: Serena R25 contract lane. Actual execution path: current conversation tools and local file/static checks. Model/profile: unspecified by runtime. No external provider or quota-consuming reviewer was launched by this lane.

## References

1. [PR #648, current scope and retained verification limits](https://github.com/applicate2628/mcp-local-hub/pull/648); [exact receiver source](https://github.com/applicate2628/mcp-local-hub/blob/fc4dc5c7145e4f4d709b91654ab4bc1fd7abe259/internal/api/serena_prepared_build_constraint_windows.go).
2. [PR #652, current scope and retained reader limits](https://github.com/applicate2628/mcp-local-hub/pull/652); [exact original-generation reader](https://github.com/applicate2628/mcp-local-hub/blob/b079e4c9503f080d8ab09742270c89a3bf88d7d5/internal/api/serena_prepared_owned_file_windows.go).
3. [Microsoft: SetHandleInformation](https://learn.microsoft.com/en-us/windows/win32/api/handleapi/nf-handleapi-sethandleinformation), `HANDLE_FLAG_PROTECT_FROM_CLOSE` behavior; checked during this session.
4. [Canonical repository instructions](https://github.com/applicate2628/mcp-local-hub/blob/2d12d9b63e60387591d27515aef3487c0e3ea560/AGENTS.md) and [operational runbook](https://github.com/applicate2628/mcp-local-hub/blob/2d12d9b63e60387591d27515aef3487c0e3ea560/docs/agent-runbook.md), read at the published plan revision.

## Terms and abbreviations

- **PR:** pull request, the existing review unit; this lane neither opens nor merges one.
- **MCP:** Model Context Protocol; no complete MCP qualification is asserted by these tests.
- **I/O:** input/output, here the original process runner's stream handles and worker settlement.
- **Job:** the Windows kernel object used by the existing runner to own and account for child processes. This scenario owns an empty Job.
- **Pin:** a retained read-only file handle whose share mode excludes write and delete access.
- **Receipt:** the original runner's observed result, not a manually constructed grant of authority.
- **SHA-256:** Secure Hash Algorithm with a 256-bit result, used here only to identify source and candidate bytes.
