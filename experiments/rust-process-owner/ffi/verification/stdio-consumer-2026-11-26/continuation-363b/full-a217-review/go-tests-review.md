# a217: full static review of Go tests

Exact Source: `a217c7799a0214967bf86211542e508162c680b5`; tree `ca118ff1ec513cc830bb8b96b4d6782d23915a26`.

## Contents

- [Scope and result](#scope-and-result)
- [Findings](#findings)
- [Coverage by file](#coverage-by-file)
- [Evidence boundaries](#evidence-boundaries)
- [Sources](#sources)

## Scope and result

**Reviewer:** Go test reviewer (`measurements_inventory`). The classifications below describe source-defined fixture mechanisms; they are not execution results. Original review digests are retained in [provenance.json](provenance.json).

All **23 test files, 148,868 bytes, 4,812 lines** were read in full and independently matched to the SHA-256 and size in [source-inventory.json](source-inventory.json). The source contains **97 top-level test functions**; this is a source census, not an executed test count. Every Git blob and SHA-256 is recorded in [go-tests-review.json](go-tests-review.json). Production Go files belong to the root review; this slice read only the necessary loader branch for cross-reference.

The source defines separate synthetic producer controls and native Windows fixtures. The original-owner and temporary-stdio-handle fixtures are written to induce protected `CloseHandle` refusal; this review did not execute them. Identity and outcome error reports are principally substituted producer outputs. Loader test source checks successful unload/reference behavior after rejection, but does not contain a fixture that makes `FreeLibrary` itself fail. No additional native test was run and no old result was reclassified.

## Findings

### GT1. confirmed conditional test-oracle gap

**Location:** `experiments/rust-process-owner/ffi/go/bridge_windows_test.go`, lines 36–57, especially 45–55.

**Condition:** An ordinary test relies on own() teardown and Finish returns err=nil with Outcome.State=cleanupFailed; no earlier body assertion requires complete. Example: TestCheckedAdmissionNilPolicyDoesNotConsumeAdmission ends at admission_windows_test.go:158.

**Observed in source:** own() explicitly accepts both complete and cleanupFailed, releases the allocation, and does not t.Error the unexpected cleanup failure. Library.Close can then succeed although failed native handle close remains recorded in Outcome.

**Consequence:** A PASS from such a test does not establish successful native cleanup. The cleanupFailed state is intentionally expected in native fault tests, so removing it globally would break their deliberate contract.

**Next action:** If a change is later authorized, make ordinary teardown require complete and keep an explicit expected-cleanup-failure helper for fault cases. Preserve historical native outcomes. No new test or production edit performed.

### GT2. confirmed static coverage limit supporting root candidate

**Location:** `experiments/rust-process-owner/ffi/go/loader_windows_test.go`, lines 99–127, 140–214; bridge_windows.go:89-94.

**Condition:** bridge_open successfully loads a DLL, later fails required-export/version validation, then FreeLibrary fails.

**Observed in source:** The tests cover successful failed-open cleanup via real module map/reference checks but never force the unload call itself to fail. faults_windows_test.go only substitutes producer fields and does not cover this.

**Consequence:** These tests do not refute the root-reviewed candidate that bridge_open fail-label ignores FreeLibrary return and frees its carrier, losing conditional cleanup failure/custody. This review has not induced that condition or verified a native occurrence.

**Next action:** Keep candidate conditional and distinguish it from real protected CloseHandle qualification. Do not mark actual unload-error behavior covered.

### GT3. test watchdog limitation

**Location:** `experiments/rust-process-owner/ffi/go/stdio_windows_test.go`, lines 207–210 and 515–518.

**Condition:** The 3-second EOF timer fires and endpoint Close or the worker fails to finish.

**Observed in source:** Timeout branch executes endpoint Close then a bare receive from done with no remaining deadline.

**Consequence:** The per-test watchdog does not guarantee bounded cleanup; outer go test/process deadline remains the eventual guard. No hang was executed or observed in this review.

**Next action:** Do not count the 3-second local timer alone as proof of bounded reader completion. Any eventual repair should retain the same reader and endpoint, preserve timeout status and avoid a new unrelated completion observer.

## Coverage by file

**Table 1. Fully read source and qualification class.** Exact blobs, sizes and SHA-256 values accompany every row in the JSON.

| File | Lines / tests | Class | Covered behavior and limit |
|---|---:|---|---|
| `admission_windows_test.go` | 273 / 7 | native functional | Checked admission observes original image/PID before resume; Go-owned callback copies survive GC; denied/error/panic/nil policies, stopped/released owners, callback locking and cancellation. **Limit:** Concurrency case releases callback immediately after scheduling Finish; it does not force the competing call to execute before release. own() teardown caveat applies. |
| `bridge_windows_test.go` | 343 / 8 | native functional and shared fixtures | Real suspended child lifecycle, copied argv/UTF-16, rejected admission, native missing-image error, ABI/input rejection, release/idempotence, concurrent Finish, root-versus-descendant and explicit environment. **Limit:** own() accepts cleanupFailed as terminal without marking ordinary teardown failed; source-only review does not establish any executed PASS. |
| `environment_windows_test.go` | 368 / 9 | native functional and validation | Copies explicit environment and drive entries, checks independent child report and parent isolation, nil/empty/legacy semantics, invalid inputs, concurrent children, denial, real missing-image error and UTF-16 budget. **Limit:** Input validation and constructed strings are not native metadata/cleanup-fault qualification. |
| `faults_windows_test.go` | 217 / 9 | mutated producer protocol controls | Deliberately mutated owned DLLs exercise create/release metadata, malformed and repeated checked callbacks, unknown decision, missing required exports, environment and stdio custody after metadata rejection. **Limit:** Normal native creation/cleanup remains real; malformed output/decision is substituted. No FreeLibrary-failure inducement. |
| `finish_context_windows_test.go` | 293 / 8 | native functional with observer barriers | Pre-cancel, nil and deadline retain owner; cancellation cause preserved; busy-owner and policy-lock observation, two observers, live descendant and cached terminal results. **Limit:** Some Background observations rely on fixture finiteness and outer test timeout; observer cancellation is not proof of native stop or cleanup. |
| `identity_faults_windows_test.go` | 84 / 1 | mutated producer protocol controls | Identity token query+close and close-only diagnostics, malformed observation, late report after actual child resume, original admission consumed only on admission path. **Limit:** Error codes 5 and 6 are producer substitutions. This file does not induce real token metadata-query or token CloseHandle failures. |
| `identity_legacy_windows_test.go` | 39 / 1 | native compatibility fixture | Frozen older DLL reports identity unsupported without invoking callback; explicit checked-admission choice still works and settles original owner. **Limit:** Requires separately pinned older DLL; optional path is not covered by an ordinary untagged run. |
| `identity_query_faults_windows_test.go` | 70 / 1 | mutated producer protocol controls | Rejects malformed/missing/repeated query reports and query/close diagnostics while preserving later explicit admission and actual child effect. **Limit:** Query and close errors are synthetic transport responses, not spontaneous or induced Windows API failures. |
| `identity_query_legacy_windows_test.go` | 49 / 1 | native compatibility fixture | ABI1 or identity-admission-only DLL reports query unsupported and retains the corresponding explicit admission route. **Limit:** Fixture mode alone does not pin DLL bytes; provenance must come from external runner evidence. |
| `identity_query_windows_test.go` | 210 / 3 | native functional | Independent child birth/SID report, repeated Go-owned copies, unchanged suspended admission, running/denied/stopped/settled/released queries and concurrent admission/query. **Limit:** Binary SID contents deliberately excluded from diagnostics; review did not read or publish private runtime markers. |
| `identity_windows_test.go` | 265 / 5 | native functional | Original birth time/job/image/wait facts and independent child PID/birth/SID, owned copies after release/unload, rejection/panics/nil/stopped, explicit owner TryLock proof during callback. **Limit:** Fixture root preserves private identity markers; runtime claims require actual runner/receipt. |
| `io_lifecycle_windows_test.go` | 328 / 5 | native functional with real pipe I/O | Actual blocked Go pipe writer/readers, arrival separate from completion, observer deadline retains owner, explicit endpoint close interrupts only owned I/O, resumed drain, descendant-held writer and unread final bytes. **Limit:** A joined result is observed through a buffered completion channel, not a retained OS process handle. Cleanup reports failed join after 3 s; it does not prove native lifetime by itself. |
| `loader_windows_test.go` | 285 / 7 | native loader fixtures | Real missing-export/version/image/init/dependency refusal, non-owning module-map observations, separate owner reference counts, Unicode path and isolated dependency search. **Limit:** No path induces FreeLibrary failure. Successful unload/map checks do not exercise bridge_open fail-branch cleanup error propagation. |
| `members_faults_windows_test.go` | 170 / 1 | mutated producer protocol controls | Retains native generation on malformed reports/copies, valid partial prefix, present zero-code token cleanup, malformed first close report and replay, persistent synthetic close failure and contradictory snapshot refusal. **Limit:** Real reservation/resources and native close are retained; fault report/view fields are substituted. Synthetic FAILED(0) is explicitly distinct from kernel close failure. |
| `members_legacy_windows_test.go` | 51 / 1 | native compatibility fixture | All-or-none optional member protocol, ABI1 and each missing member export; no adoption of a generation, original admission retained. **Limit:** Separate tagged variants and exact DLL provenance needed; source census is not test execution. |
| `members_report_windows_test.go` | 68 / 2 | pure constructed decoder cases | Error presence independent of nonzero errno, high-bit conversion, dual query/cleanup causes and incoherent field rejection. **Limit:** All reports are constructed values; no native query or close operation is performed. |
| `members_windows_test.go` | 407 / 5 | native functional with shared fixtures | Root borrowed ownership versus owned descendants, stable generations, copied historical snapshots, revalidation, real descendant natural exit, empty partial generation and stale/busy/budget rejection. **Limit:** memberFixture cleanup checks CloseFinalized only, not CleanupComplete/error; explicit requireMemberClose calls are stronger and most ordinary bodies use them. |
| `native_cleanup_windows_test.go` | 305 / 3 | real induced native close failures | Qualification-only read-only slot accessor; protects exact original process/thread/job handles, obtains actual native close errno, 21 mode/mask cases, cached failure/release/no-retry and explicit authorized fixture recovery. **Limit:** Normal DLL lacks accessor. It qualifies original owner CloseHandle failures, not FreeLibrary, token query, reader metadata or reader cleanup. Recovery does not convert historical failure to success. |
| `outcome_faults_windows_test.go` | 146 / 2 | mutated producer protocol controls | Foreign producer changes serialized finish result after real native settlement; malformed states/layout/flags/status rejected with allocation retained; 7 valid cleanup-mask reports transported. **Limit:** Codes 5+i are explicitly fabricated. Native Release intentionally sees actual successful kernel cleanup; do not count this as real CloseHandle-failure evidence. |
| `outcome_windows_test.go` | 77 / 2 | pure constructed decoder cases | Defined pending/complete/cleanupFailed values and 7 cleanup masks; rejects incoherent fields. **Limit:** No native failure is caused by these struct literals. |
| `stdio_windows_test.go` | 521 / 11 | native functional plus real temporary close failures | Source handle ownership/flags and binary bytes, duplicates survive caller close, shared position, sync and Go pipe EOF, deny/missing image, unrelated capability exclusion, partial duplicate cleanup and 7 protected-close masks. **Limit:** Protected handles are temporary Go bridge duplicates, distinct from Rust original owner slots. EOF timeout branches close endpoint then use unbounded receive; process-wide handle count is supporting evidence, not exact resource ownership proof. |
| `wait_legacy_windows_test.go` | 42 / 1 | native compatibility fixture | Missing one/both wait-close exports preserves original pointer and explicit legacy lifecycle without implicit fallback. **Limit:** Separate tagged fixture with pinned missing-export DLL required. |
| `wait_windows_test.go` | 201 / 4 | native functional and constructed decoder cases | Wait retains original owner until explicit close; root exit 259 and descendants preserved; replay and direct close; malformed ExitObservation decoder matrix. **Limit:** Exit decoder cases 150–201 are constructed values. Native-failure code 5 case is not induced Windows failure. |

## Evidence boundaries

- Source-defined cases are not executed-case counts. Tagged variants require exact recorded DLL/source provenance.
- Metadata, identity token query/close and members transport faults are mostly separately built producer substitutions; do not promote them to real Windows API failures.
- The native_cleanup and stdio protected-close tests are written to induce actual CloseHandle refusal on exact protected original/temporary handles, with explicit fixture recovery. They do not cover FreeLibrary.
- memberFixture cleanup at members_windows_test.go:28-39 checks finalization but omits error/CleanupComplete; explicit requireMemberClose at lines 72–82 checks both and ordinary bodies mostly use it.
- Several tests use background observation or WaitGroup joins that rely on finite fixtures/outer watchdog. This review makes no new claim of bounded whole-process completion.
- No private binary SID/account markers were opened or printed.
- No new native test, repeat 21-run qualification, production edit, publication or PR change was performed for this static review.

## Sources

1. All file/line references refer to the [immutable experiment source](https://github.com/applicate2628/mcp-local-hub/tree/a217c7799a0214967bf86211542e508162c680b5/experiments/rust-process-owner) at the commit above. [source-inventory.json](source-inventory.json) is the 64-file source manifest; this review covers exactly the 23 Go test rows and does not mark any other file reviewed.

2. [Per-file Go test review](go-tests-review.json) and [consolidated coverage](coverage.json).
3. [Go implementation review](go-production-review.md), finding A217-GO-01; GT2 is supporting coverage evidence, not a second loader defect.
