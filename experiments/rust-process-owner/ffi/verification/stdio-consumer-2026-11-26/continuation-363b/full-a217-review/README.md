# Complete static review of the a217 Rust process-owner experiment

## Contents

1. [Result and scope](#result-and-scope)
2. [Source identity and native applicability](#source-identity-and-native-applicability)
3. [Findings](#findings)
4. [Contract and test-evidence assessment](#contract-and-test-evidence-assessment)
5. [Coverage and reviewer provenance](#coverage-and-reviewer-provenance)
6. [Next bounded decisions](#next-bounded-decisions)
7. [Packet contents](#packet-contents)
8. [Sources](#sources)

## Result and scope

**All 64 inventoried source files were fully read and statically reviewed by their assigned reviewers.** The review has five unresolved findings and one related test-coverage limitation. Completion means source-review coverage, not a clean review, native PASS, production readiness or permission to merge.

The exact baseline is commit `a217c7799a0214967bf86211542e508162c680b5`, tree `ca118ff1ec513cc830bb8b96b4d6782d23915a26`, under `experiments/rust-process-owner/`. The 64 files contain **649,751 bytes and 18,440 lines**. The machine-readable [coverage.json](coverage.json) records each file's Git blob ID, SHA-256, size, line count, assigned reviewer, semantic review notes, limits and finding links. The three allocations are disjoint and cover every inventory path exactly once [sources 1–4].

No tests, builds, product processes or native probes were run for this static review. Reading a test fixture does not establish that it ran successfully. Earlier acquisition hashes and selected-function inspection were not credited as full-file review. The consolidation checks source identity and complete coverage; it does not claim a second independent read of all 64 files by one reviewer.

## Source identity and native applicability

**Table 1. Source and native-artifact identities.**

| Item | Exact pin or relation |
| --- | --- |
| Static-review baseline | `a217c7799a0214967bf86211542e508162c680b5` |
| Baseline tree | `ca118ff1ec513cc830bb8b96b4d6782d23915a26` |
| Source used for the native DLL | `efa73c0e23f7af16b8aa2ccb047b745a58ba4f71` |
| Native DLL receipt | 192,512 bytes; SHA-256 `dfa87a10a39e9110cda735ae6a987bfa64b03446529072710723e9b6ea0dca56` |
| Inventory relation | 63 byte-identical paths; one semantic source delta; full byte equivalence is **false** |
| Different file | `src/windows.rs` |
| a217 file SHA-256 | `4c031c1a3824b83c7cec4a63a61120c5d9597f893c0819f6dee8613cc14e90a9` |
| Native file SHA-256 | `110d7cec3dc450cf4049c8e8435fd54f50f734fb4c7d42bd126a75158459f499` |

The [native mapping](native-dll-source-map.json) is attributed to the native qualification reviewer. The Rust/ABI reviewer independently checked the two local window-source hashes, lengths, native Git blob ID and complete two-hunk diff. The other 63 native-file comparisons were not repeated by this consolidation. This distinction preserves the provenance of the mapping [source 5].

The delta imports `STARTF_USESHOWWINDOW` and, only when `foreground` is false, supplies `wShowWindow = SW_HIDE`. Existing stdio flags are combined with it. Foreground mode without a parent console still uses `CREATE_NO_WINDOW` but skips the new show-window flag. The changed code does not alter handle ownership or cleanup branches [source 5].

Microsoft documents that the startup show value controls a GUI child's first `ShowWindow`; `CREATE_NO_WINDOW` alone is ignored for non-console applications. Therefore the delta affects initial GUI visibility as well as its stated later-console purpose. That is a material policy difference from a217, not a formatting-only change [sources 6–7]. The exact visible result of a later `AllocConsole` is not established by this source review. The supplement distinguishes the supplied startup fields from any native console qualification [sources 5, 8].

All five findings below concern unchanged files or unchanged branches. They apply to the mapped native source with the same conditions. This mapping does not establish that the conditional failure occurred in a native run.

## Findings

**Table 2. Findings and evidence classification.** Paths are relative to `experiments/rust-process-owner/`; all baseline line numbers refer to a217.

| ID and reviewer | Location | Condition and consequence | Native-source applicability |
| --- | --- | --- | --- |
| **RUST-01**, Rust/ABI reviewer; proposed P2 | `ffi/src/identity.rs:82–93,105–119`; related token producer/header/Go decoder | A typed token-close failure with numeric code zero loses its independent presence. Combined with query error 5, it becomes indistinguishable from query-only failure. Query-zero and close-only-zero also violate the identity schema. Diagnostic/cleanup evidence is lost; false admission was not established. | All involved files byte-identical. |
| **RUST-02**, Rust/ABI reviewer; proposed P2 | `ffi/src/lib.rs:202–205,269–284`; related base header and Go outcome decoder | Native-zero or cleanup-zero errors produce a base outcome rejected by the declared schema and Go validator. The validated root/cleanup receipt is lost. Cleared release-slot custody remains separately honored. | Encoder/header/Go files identical; native close/termination branches unchanged, shifted seven lines. |
| **A217-GO-01**, lead Go reviewer; P2 | `ffi/go/bridge_windows.go:89–90` | After successful DLL loading and later rejection, a failed `FreeLibrary` return is ignored and the bridge allocation is freed. The secondary unload failure and remaining module custody are not returned. No real unload failure or leak was observed. | Entire Go file byte-identical. |
| **GT1**, Go test reviewer; test-oracle gap | `ffi/go/bridge_windows_test.go:36–57`, especially 45–55 | Ordinary tests using `own()` teardown can accept `cleanupFailed` without failing the test when the body has no stronger assertion. A test PASS in that condition cannot establish successful cleanup. Deliberate failure fixtures need an explicit expected-failure path. | Entire Go test file byte-identical. |
| **GT3**, Go test reviewer; local watchdog limitation | `ffi/go/stdio_windows_test.go:207–210,515–518` | After the three-second EOF timer fires, the test closes the endpoint and waits on the worker without another bound. The local timer alone does not bound completion if close or worker completion stalls. No hang was observed here. | Entire Go test file byte-identical. |

**GT2 is a supporting coverage limit, not a sixth independent defect.** Loader tests cover rejection followed by successful unload/reference checks, but do not force `FreeLibrary` itself to fail. This leaves A217-GO-01's condition unqualified. Native `CloseHandle` refusal and substituted producer metadata concern different operations and cannot establish loader-unload failure behavior [sources 3–4].

The Rust findings also require a clear distinction between **failure presence** and **numeric error code**. The underlying Rust types preserve the former even when the latter is zero. The older identity/base ABI does not represent that state consistently. The members ABI already uses an independent source/mask indicator and has constructed zero-code cases. No actual Win32 zero-last-error failure was induced by this review; the proven fact is the conditional conversion/schema behavior [source 2].

Each detailed report states the input condition, exact branch, consequence, existing test gap and bounded next acceptance. None of the conditional findings is presented as a measured native leak, false admission, resource closure or reproduced operating-system failure.

## Contract and test-evidence assessment

The source review follows ownership through suspended creation, admission, observation and settlement. The Rust owner keeps the original process, primary thread and Job handles; the at-create Job attribute and kill-on-close policy are established before release of the suspended thread. Admission is consumed before policy evaluation. Callback observations borrow or copy from the original held resources. Exit observation is separate from terminal cleanup, and consumed numeric handles are not automatically retried after a failed close [source 2].

Member snapshots reserve a generation before fallible acquisition, keep partial owned resources and revalidate retained handles. The FFI finalization scalar survives later reporting failure. These observations describe implemented control flow, not atomicity of every operating-system observation or proof of every creation-time interleaving. Drop remains best-effort fallback rather than settlement evidence [source 2].

The Go review follows loader symbol negotiation, explicit request buffers, transferred-owner preservation, callback copies, split wait/close/release and member-generation handling. Its release path distinguishes deallocation custody from a valid outcome receipt, which limits the consequence claimed in RUST-02. The experiment still has a trusted in-process ABI contract; validating serialized fields does not validate arbitrary foreign pointers [source 3].

The 23 Go test files contain **97 top-level test functions**. This is a source census, not an executed test count. The test reviewer distinguishes constructed decoder inputs, separately built producer substitutions, compatibility fixtures, normal native functional tests and source-defined protected-handle refusal fixtures. Substituted report fields are not evidence that the corresponding Windows query or close failed. Likewise, a local timeout or background observer does not by itself prove physical resource completion [source 4].

The protected-close fixtures are written to exercise original Rust owner handles or temporary Go stdio duplicates and preserve their original failure receipts during authorized fixture recovery. They do not cover token-query failure, loader unload failure or every reader/stream ownership path. Tagged compatibility/fault variants require their own exact DLL and source provenance. No execution status from those variants is inferred by this packet [source 4].

## Coverage and reviewer provenance

**Table 3. Full-file review allocation.** Reviewer IDs describe review roles; they are not a claim that one reviewer independently repeated another's entire slice.

| Assigned reviewer | Scope | Files | Bytes | Lines | Detailed evidence |
| --- | --- | ---: | ---: | ---: | --- |
| `functional_review` | Rust implementation, Rust tests, ABI headers, Cargo | 31 | 425,989 | 11,668 | [Rust/ABI report](RUST-ABI-REVIEW.md), [record](coverage-rust.json) |
| `root` | Nine Go implementation files and `go.mod` | 10 | 74,894 | 1,960 | [Go implementation report](go-production-review.md), [record](go-production-review.json) |
| `measurements_inventory` | Go tests | 23 | 148,868 | 4,812 | [Go test report](go-tests-review.md), [record](go-tests-review.json) |
| **Total, disjoint** | Complete inventory | **64** | **649,751** | **18,440** | [Full coverage](coverage.json) |

During consolidation, every review row matched the inventory path, Git blob, byte length and SHA-256. Every local source file matched those pins, and each line count matched its exact text. These checks establish the identity of the reviewed files; the semantic review notes and attributed full-file reads establish review coverage. A source hash alone was not used to claim a review [sources 1–4].

Original reports remain preserved outside the publication copies. [provenance.json](provenance.json) retains their SHA-256 digests and reviewer allocation. Public copies remove execution-environment locations, normalize formatting and resolve links. Source-defined test descriptions are explicitly separated from runtime evidence; finding IDs and substantive conditions are retained.

## Next bounded decisions

1. **Resolve the identity/base ABI representation issue deliberately.** Reuse the existing conversion tests for native-zero, close-only-zero and dual errors. Preserve raw numeric values and independent failure presence, callback/admission behavior and no-retry custody. Choose a compatibility treatment before changing the wire or merely relaxing the Go decoder.
2. **Account for loader rejection cleanup.** A217-GO-01 needs both primary and secondary error preservation and an explicit disposition for any remaining module reference. Keep synthetic coverage distinct from actual native unload failure evidence.
3. **Strengthen the relevant test conclusions.** Ordinary teardown should require complete cleanup while deliberate failure fixtures state their expected failure. A timeout outcome should remain a timeout while completion of the same original reader/endpoint is accounted for; a new observer cannot repair missing original completion evidence.
4. **Keep native window policy explicit.** Use the efa73 mapping and supplemental review when interpreting the existing native DLL. A217 alone does not describe its full startup-window behavior. Any runtime qualification must use its original pinned source/artifact receipts and actual mode/console conditions.

These are bounded follow-up decisions. This packet changes no code, expands no runtime deadline, creates no native environment and does not promote an old FAIL/UNKNOWN or physical-only result to runtime success. Product rollout, Unix parity, descendant closure and complete stream cancellation remain outside what this source review establishes.

## Packet contents

**Table 4. Publication files.**

| File | Purpose |
| --- | --- |
| [coverage.json](coverage.json) | Canonical full 64-file review coverage, exact source pins, finding records and native mapping |
| [source-inventory.json](source-inventory.json) | Exact source pins without stale acquisition-as-review status |
| [RUST-ABI-REVIEW.md](RUST-ABI-REVIEW.md), [coverage-rust.json](coverage-rust.json) | Attributed 31-file Rust/ABI/Cargo review |
| [go-production-review.md](go-production-review.md), [go-production-review.json](go-production-review.json) | Attributed 10-file Go implementation/module review |
| [go-tests-review.md](go-tests-review.md), [go-tests-review.json](go-tests-review.json) | Attributed 23-file Go test review and fixture-evidence classification |
| [NATIVE-DELTA-REVIEW.md](NATIVE-DELTA-REVIEW.md), [native-dll-source-map.json](native-dll-source-map.json), [diff](native-vs-a217-windows.patch) | Exact non-equivalence mapping and bounded efa73 window-policy review |
| [provenance.json](provenance.json) | Original review digests and editorial transformation record |
| [packet-manifest.json](packet-manifest.json) | Byte lengths and SHA-256 values of all other publication files |

## Sources

1. [Immutable a217 experiment source](https://github.com/applicate2628/mcp-local-hub/tree/a217c7799a0214967bf86211542e508162c680b5/experiments/rust-process-owner), [source inventory](source-inventory.json) and [full coverage](coverage.json).
2. [Rust/ABI report](RUST-ABI-REVIEW.md) and [31-file record](coverage-rust.json). Supporting READMEs are identified by exact a217 blobs in that report; selective consultation is not counted as additional full-file documentation review.
3. [Lead Go implementation report](go-production-review.md) and [original-attributed record](go-production-review.json).
4. [Go test report](go-tests-review.md) and [23-file record](go-tests-review.json).
5. [Native source mapping](native-dll-source-map.json), [exact delta](native-vs-a217-windows.patch) and [supplemental review](NATIVE-DELTA-REVIEW.md).
6. Microsoft Learn, [STARTUPINFOW](https://learn.microsoft.com/en-us/windows/win32/api/processthreadsapi/ns-processthreadsapi-startupinfow), `STARTF_USESHOWWINDOW`/`wShowWindow`, retrieved 2026-11-26.
7. Microsoft Learn, [Process Creation Flags](https://learn.microsoft.com/en-us/windows/win32/procthread/process-creation-flags), `CREATE_NO_WINDOW`, retrieved 2026-11-26.
8. Microsoft Learn, [Setting Window Properties Using STARTUPINFO](https://learn.microsoft.com/en-us/windows/win32/procthread/setting-window-properties-using-startupinfo), newly created consoles and later `AllocConsole`, retrieved 2026-11-26.
