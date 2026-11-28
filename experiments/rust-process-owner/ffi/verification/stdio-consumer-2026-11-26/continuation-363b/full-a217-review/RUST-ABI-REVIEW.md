# Static Rust and ABI review of a217

## Contents

1. [Outcome and evidence boundary](#outcome-and-evidence-boundary)
2. [Findings](#findings)
3. [Contract review](#contract-review)
4. [Full-file coverage](#full-file-coverage)
5. [Acceptance gaps and limits](#acceptance-gaps-and-limits)
6. [Sources](#sources)

## Outcome and evidence boundary

**All 31 assigned Rust implementation, Rust test, ABI header and Cargo files were fully read and reviewed by the Rust/ABI reviewer (`functional_review`).** This is static coverage, not a build or native qualification result. No code, test, build, native fixture, product runtime or publication was executed as part of this review. The review found two related conditional representation defects at the Rust-to-C ABI boundary. Both concern a failure whose numeric native code is zero; the underlying Rust types retain failure presence, but the older ABI contracts do not represent that presence consistently.

Exact reviewed source:

- Commit: `a217c7799a0214967bf86211542e508162c680b5`.
- Tree: `ca118ff1ec513cc830bb8b96b4d6782d23915a26`.
- Root: `experiments/rust-process-owner/`.
- Acquisition inventory: [source-inventory.json](source-inventory.json), 64 source files, 649,751 bytes.
- This reviewer: [coverage-rust.json](coverage-rust.json), 31 files, 425,989 bytes; per-file Git blob ID, SHA-256, byte count, line count and semantic review notes.
- The other 33 Go files belong to the lead Go implementation reviewer and the Go test reviewer. Selected Go decoder branches were cross-checked here to establish consequences; that is not additional full-file Go coverage.

The exact 31 local files were rebound to their inventory byte lengths, SHA-256 digests and Git blob IDs when this report was finalized. Earlier acquisition or a previous five-file selected-function report was not counted as full-file review. Source line references below refer to a217, with paths relative to the root above.

The actual native DLL was built from a different commit, `efa73c0e23f7af16b8aa2ccb047b745a58ba4f71`. Its single source difference is reviewed separately in [NATIVE-DELTA-REVIEW.md](NATIVE-DELTA-REVIEW.md). The two source sets are not byte-equivalent.

## Findings

### RUST-01 — identity encoding loses the independent token-close failure when its code is zero

**Classification:** concrete conditional data-representation defect, proposed priority P2. The conversion result is determinable directly from source. A real Win32 operation returning failure with last-error code zero was not provoked or otherwise established by this review.

**Locations:** `ffi/src/identity.rs:82–93`, `105–119`, functions `record_query_error` and `record_token_error`; `ffi/include/identity_v1.h:56–66`; producer `src/token_user.rs:82–105`. Consumer cross-check: `ffi/go/identity_windows.go:95–118` and `78–85`.

`owned_token_user_sid` represents a failed close through `TokenUserError::Cleanup` whenever `CloseHandle` returns failure. The numeric code is captured separately. Even when that code is zero, the enum still records that cleanup failed. `record_token_error` removes this independent discriminant and publishes only `token_close_error = failure.code`.

**Table 1. Distinct typed inputs collapse or violate the identity report contract.** Each report cell lists `(stage, query_error_kind, query_native_error, token_close_error)` after initialization.

| Typed input | Encoded report | Consequence in the current Go decoder |
| --- | --- | --- |
| Observation error with native code 5 | `(TOKEN, NATIVE, 5, 0)` | Accepted as a query-only identity error. |
| Cleanup failure code 0 plus observation error code 5 | `(TOKEN, NATIVE, 5, 0)` | The same report as the preceding row; the token-close failure is lost. |
| Cleanup failure code 0, observation succeeded | `(TOKEN, NONE, 0, 0)` | Rejected because a query stage omitted its failure. |
| Observation error with native code 0 | `(TOKEN, NATIVE, 0, 0)` | Rejected because native-kind presence disagrees with the numeric code. |

The first two rows demonstrate information loss independently of the Go decoder: two different source errors become identical ABI values. The last two rows contradict the declared identity-report invariants. The Go decoder correctly enforces those current invariants; simply weakening its check would not restore the missing cleanup-presence bit in the first pair.

**Consequence:** a caller can receive a query-only `IdentityError` where both observation and token cleanup failed, or receive an invalid-observation error instead of the typed failure receipt. This is a diagnostic and cleanup-accounting defect. It does not establish false admission: the native observation path still fails, does not invoke the identity policy callback, and does not publish a successful identity.

**Existing coverage:** the embedded tests `query_error_classification_preserves_stage_native_code_and_non_native_kind` and `token_query_and_close_failures_remain_independent`, `ffi/src/identity.rs:373–454`, cover nonzero native and close codes and non-native query kinds. They omit the zero-code cases in Table 1. In contrast, the members ABI preserves cleanup presence using `SOURCE_TOKEN_CLEANUP` independently of the number (`ffi/src/members.rs:158–170`) and includes zero-code cases in `ffi/src/members_tests.rs`.

**Bounded next acceptance:** agree an explicit compatibility treatment for the identity ABI, then reuse the existing conversion tests for native-query zero, close-only zero, query-5 plus close-zero, and both-zero. The receipt must preserve each failure's presence and raw number without inventing a replacement error code. Retain the existing no-callback, admission-consumption and read-only-query behavior. This report does not choose an ABI-breaking wire change or authorize implementation.

### RUST-02 — the base ABI encodes native-zero and cleanup-zero results that its own header and decoder reject

**Classification:** concrete conditional producer/schema mismatch, proposed priority P2. Native occurrence is not demonstrated; source-level encoding and rejection are explicit.

**Locations:** `ffi/src/lib.rs:202–205`, function `failure`, and `269–284`, function `encode_settlement`; `ffi/include/owner_v1.h:32–39`; native producers `src/windows.rs:705–708` and `803–806`. Consumer cross-check: `ffi/go/outcome_windows.go:26–27`, `45–47`; `ffi/go/bridge_windows.go:212–230`.

`failure` returns `MH_NATIVE` whenever `io::Error::raw_os_error()` is `Some(code)`, including `Some(0)`. The base header requires a nonzero native code for that status. Similarly, `encode_settlement` sets a cleanup-mask bit for each `NativeFailure`, even when its stored code is zero, whereas the header requires a mask bit if and only if its code is nonzero.

**Table 2. Base outcome mismatch.**

| Source value | Produced ABI value | Current consumer result |
| --- | --- | --- |
| `io::Error::from_raw_os_error(0)` | `MH_NATIVE`, `native_error = 0` | `ErrInvalidOutcome`: native failure without its code. |
| `CleanupFailed` containing `CloseThread` with code 0 | Cleanup-failed state, thread mask bit set, thread code 0 | `ErrInvalidOutcome`: cleanup slot and error code disagree. |
| Equivalent process/job failure with code 0 | The corresponding mask bit set and code 0 | The same schema rejection. |

The core stores failure presence separately when a native termination or close call fails. Its cached close result intentionally avoids retrying consumed numeric handles. The FFI encoder then publishes an outcome outside its own advertised schema. `result` returns a zero `Outcome` together with the validation error, so the original root result and failed-slot receipt are unavailable as a validated Go observation.

**Important limit:** this does not prove owner loss, an automatic retry or a leak in the Go release path. `Owner.Release`, `ffi/go/bridge_windows.go:488–506`, independently honors a cleared native owner slot and adjusts the library's live-owner count even when outcome validation fails. That custody safeguard remains present. The problem is the inconsistent representation of the receipt, not a demonstrated violation of that safeguard.

**Existing coverage:** the base synthetic settlement/error encoding tests in `ffi/src/tests.rs:103–161` use nonzero native and cleanup codes. The core's embedded history test explicitly retains `Err(0)` in `src/windows.rs:1055–1073`, showing that zero-valued native failure is representable in the core. Native protected-close tests cover their observed nonzero failure codes; they do not qualify the zero-last-error case.

**Bounded next acceptance:** reuse the base encoding tests for an explicit native-zero error and each close slot with zero, then exercise the selected schema decision in the Go decoder. Preserve raw failure presence, cached terminal result, cleared release-slot custody and the no-retry rule. An ABI compatibility decision is necessary; removing a decoder assertion alone is insufficient to explain the contract change. No broad fault-injection layer or unrelated refactor is required to demonstrate this conversion gap.

## Contract review

The following are observations about implementation and test design, not native PASS claims or proofs of every operating-system interleaving.

**Creation and custody.** `NativeOwner::create_suspended_impl` (`src/windows.rs:375`) configures kill-on-close on the original Job, supplies that Job through the at-create attribute list, and requests a suspended primary thread. Attribute backing arrays outlive deletion of the attribute list. After process creation succeeds, the original process/thread/Job handles remain in one owner, including when original creation-time capture failed. Borrowed stdio is validated and deduplicated for an explicit inheritance list; its input flags and ownership are not transferred. These details were cross-checked against the FFI single-publication path and the Rust lifecycle/stdio tests.

**Admission and observation.** `admit_and_resume` (`src/windows.rs:634`) consumes the admission attempt before calling policy. Denial or resume failure does not make it retryable. `process_view` and `with_original_process_handle` (`576`, `605`) expose the held process under a synchronous lifetime; query exports do not resume or consume admission. Copying a raw integer does not confer ownership or extend the callback contract. Active member-snapshot custody blocks lifecycle actions that would invalidate those handles.

**Exit and settlement.** `wait_for_exit`, `finish` and `close_exited` (`src/windows.rs:724`, `760`, `783`) keep exit observation distinct from resource settlement. Exit code 259 can be a known result. `close_exited` rechecks root and Job exit, consumes each owner handle once, caches cleanup failures, and does not retry potentially recycled numbers. `Drop` is a best-effort kill-on-Job-close fallback; it is not evidence of successful settlement. Failure-path and crash tests were read for whether cleanup could be mistaken for the measured outcome.

**Member snapshots.** `observe_job_members`, `revalidate_job_members` and `close_job_members` (`src/job_members.rs:136`, `177`, `238`) reserve a generation before acquisition, retain partial acquired resources, and revalidate held member handles rather than reopening by PID. A partial snapshot, even one containing no handles, has explicit close obligations. Acquisition compares the census around collection and gathers identity, Job membership and liveness observations separately; the implementation does not create an atomic global snapshot. Close caches its receipt and ends the active-generation guard. The FFI publishes irreversible finalization before fallible report projection/callback. Unlike the older identity/base ABI, this report explicitly preserves zero-code failure presence.

**Identity, launch context and arguments.** Token-user query storage is aligned and bounded; the SID pointer/header/subauthority extent is checked before copying. No copied SID is returned after token-close failure. `LaunchContext::prepare` validates absolute cwd and a complete, explicitly supplied environment, including Windows-ordinal order, case-insensitive uniqueness, drive-directory names, embedded NULs and raw UTF-16 behavior. Empty means empty, not parent inheritance. Version 2's first command-line token is separate from the executable path; later arguments retain their own quoting rule. Version 3's foreground path uses the observed parent-console state and does not silently fall back after an unknown query result.

**Tests and fixtures.** Full-file reads cover structural ABI validation, original-handle borrowing, independent child-side identity observations, admission/deny, bounded wait and explicit close, member epochs/partial ownership, crash barriers, launch environment/cwd, stdio bytes/EOF and argv0. The crash suite samples original process exit before fixture cleanup and includes an unrelated control. Native close-failure recovery is fixture-specific. These are meaningful assertions in source, but they were not executed here. The documentation requests serial Rust test execution for process-wide handle-count observations; absence of an extra test mutex is not reported as an independent defect.

**Cargo and scope.** The manifests consistently declare Rust 1.100 and Windows-only dependencies, and both lockfiles pin the same Windows crates. The FFI profiles use `panic = abort`; callback/foreign-input contracts explicitly exclude unwind/longjmp and invalid backing storage. The experiment does not itself establish production rollout, Unix parity, hostile in-process memory safety, native-call cancellation, or product stream-drain ownership. No new dependency, wrapper layer or architecture rewrite is proposed by this review.

## Full-file coverage

**Table 3. Assigned source files fully read and reviewed.** Exact immutable blob IDs and SHA-256 values are in [coverage-rust.json](coverage-rust.json). A finding entry identifies the relevant source or schema, not an assertion that every listed test file itself contains a defect.

| Path relative to `experiments/rust-process-owner/` | Lines | Bytes | Finding linkage |
| --- | ---: | ---: | --- |
| `Cargo.lock` | 25 | 633 | — |
| `Cargo.toml` | 12 | 460 | — |
| `ffi/Cargo.lock` | 32 | 746 | — |
| `ffi/Cargo.toml` | 20 | 325 | — |
| `ffi/include/identity_v1.h` | 107 | 6145 | RUST-01 |
| `ffi/include/members_v1.h` | 197 | 10860 | — |
| `ffi/include/owner_v1.h` | 159 | 10869 | RUST-02 |
| `ffi/src/argv0_tests.rs` | 195 | 6619 | — |
| `ffi/src/identity.rs` | 688 | 25005 | RUST-01 |
| `ffi/src/lib.rs` | 799 | 25843 | RUST-02 |
| `ffi/src/members.rs` | 704 | 23779 | — |
| `ffi/src/members_tests.rs` | 1033 | 33559 | — |
| `ffi/src/process_borrow.rs` | 44 | 1659 | — |
| `ffi/src/process_borrow_tests.rs` | 153 | 5545 | — |
| `ffi/src/tests.rs` | 455 | 13334 | RUST-02 |
| `src/argv0_tests.rs` | 60 | 1625 | — |
| `src/bin/owner_child.rs` | 343 | 13902 | — |
| `src/bin/owner_crash_fixture.rs` | 219 | 9044 | — |
| `src/foreground_console_tests.rs` | 43 | 1423 | — |
| `src/job_member_tests.rs` | 606 | 23506 | — |
| `src/job_members.rs` | 672 | 25516 | — |
| `src/launch_context.rs` | 296 | 11377 | — |
| `src/lib.rs` | 73 | 2108 | — |
| `src/native_failure_tests.rs` | 940 | 33708 | — |
| `src/process_borrow_tests.rs` | 103 | 3687 | — |
| `src/token_user.rs` | 323 | 13337 | RUST-01 |
| `src/windows.rs` | 1078 | 42672 | RUST-02 |
| `tests/launch_context.rs` | 394 | 13066 | — |
| `tests/lifecycle.rs` | 1067 | 37408 | — |
| `tests/owner_crash.rs` | 321 | 11077 | — |
| `tests/stdio.rs` | 507 | 17152 | — |

All rows have `read_status = full_file_read` and `review_status = reviewed`. The source bytes have not been changed. The coverage file includes a per-file description of the semantic checks, so a matching hash alone is not presented as a review.

## Acceptance gaps and limits

1. **RUST-01 and RUST-02 need conversion-level zero-code cases and an explicit wire-compatibility decision.** They should not be silently reclassified as native reproduced failures. Existing member-report handling provides a local model for representing presence separately from a number.
2. **The native DLL has one material source difference.** Apply the a217 review to the mapped identical text and the separate native delta report to its window policy. Do not state full source byte equivalence or infer console behavior from a217 tests.
3. **This review has no execution result.** No native fixture, Cargo/Go test, build, crash probe, product-process launch or cleanup attempt was performed by this reviewer for this task. Read-only Git object acquisition and local source-digest checks were performed. A source-read result cannot upgrade an earlier FAIL/UNKNOWN receipt, establish descendant closure or satisfy a runtime acceptance gate.
4. **Support documentation was used selectively.** The exact a217 core/FFI READMEs were acquired and consulted for the ABI, experiment scope and serial-test profile. They are outside the 64-source-file inventory and are not claimed as full-file documentation review here.

The consolidated [README](README.md) reconciles this slice with the 33 Go source/test reviews. Any decision to change code remains separate. This report does not publish a PR, merge code or authorize an operational action.

## Sources

1. [Immutable a217 experiment source](https://github.com/applicate2628/mcp-local-hub/tree/a217c7799a0214967bf86211542e508162c680b5/experiments/rust-process-owner). Line references and code findings in this report are bound to that commit through the per-file inventory and coverage digests.
2. [Source inventory](source-inventory.json) and [full Rust review coverage](coverage-rust.json). The public source inventory retains the exact acquired object pins; immutable objects were read with replacement objects, lazy fetch and optional Git locks disabled.
3. Exact support core README: blob `d46f0cee2cffe87a9a64057558ee0709b0b79481`, SHA-256 `45d730d5c7f62286fb41041c6c8fc911515e47e347346c3543714af23cc8bcd1`, [immutable source](https://github.com/applicate2628/mcp-local-hub/blob/a217c7799a0214967bf86211542e508162c680b5/experiments/rust-process-owner/README.md).
4. Exact support FFI README: blob `516865ccd9202fdedaf9333093c32196acacea06`, SHA-256 `b4b5a246a6c36e1a0b8ddbb21a2da212f9c8606f6a184f10f2243d61a8b5810d`, [immutable source](https://github.com/applicate2628/mcp-local-hub/blob/a217c7799a0214967bf86211542e508162c680b5/experiments/rust-process-owner/ffi/README.md).
5. [Native source mapping](native-dll-source-map.json) and [bounded supplemental native review](NATIVE-DELTA-REVIEW.md). Native artifact provenance belongs to the separate qualification record; this report does not replace or promote its execution evidence.
