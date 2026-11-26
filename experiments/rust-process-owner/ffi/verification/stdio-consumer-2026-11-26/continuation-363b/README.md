# Stdio consumer continuation 363b

**The v6 Go–Rust–Rust–Go consumer sequence passed: four accepted runs, 12 cases and 264 RPC.** All original measurement processes, Jobs and readers closed. Seven raw RPC-time samples are resolution-unresolved. [Complete v6 report and raw timing tables](consumer-v6/README.md) · [Exact integer samples](consumer-v6/timings.json). Two observations per arm do not justify Rust adoption.

## Contents

- [Result boundaries](#result-boundaries)
- [Consolidated experiment status](#consolidated-experiment-status)
- [Source applicability](#source-applicability)
- [Oracle correction](#oracle-correction)
- [Preparation and measurement](#preparation-and-measurement)
- [Static review](#static-review)
- [Evidence and provenance](#evidence-and-provenance)

## Result boundaries

Table 1 distinguishes the public historical attempts from the later local continuation.

| Record | Actual outcome | Boundary |
| --- | --- | --- |
| Existing public report at `62c853e8a9e9b2817f5fcaf15018450b76798690` | Two closed preparation failures and one guard failure with late physical closure | Preserved byte-for-byte; its third attempt is distinct from consumer-v3 below |
| Consumer-v3 | FAIL in the first Go consumer, after preparation passed | A closed `os.File` returned Windows invalid-handle from `Stat`; the oracle accepted only `os.ErrClosed`. The original command and readers closed. No complete consumer receipt was accepted; no Rust consumer ran |
| Consumer-v4 | FAIL compiling the helper | The overlay referred to unsupported `syscall.ERROR_INVALID_HANDLE`; five original commands closed, including the failed compiler. Go/Rust arm builds and measurement did not run |
| Consumer-v5 | FAIL at the timing gate | Three Go cases passed and emitted 66 RPC; zero elapsed samples were rejected, so no receipt was accepted and Rust did not run |
| Consumer-v6 | **PASS** | Four accepted Go/Rust/Rust/Go runs, 12 cases and 264 RPC; seven raw zero samples retained as unresolved; [full result](consumer-v6/README.md) |

V5 preparation and all seven compile commands passed. Its first Go command then exited successfully and logged three passing cases, with one emitted report for 66 RPC. The controller rejected that report because the two single-RPC cases recorded `rpc_ns = 0`; the strict timing rule required all four elapsed fields to be positive. This is an emitted observation, not an accepted consumer receipt. The original command, Job, two readers, controller and outer launcher all closed. For v5, accepted consumer receipts remain **zero**. Both Rust invocations and the final Go invocation remain **NOT_RUN** in that attempt, and its ABBA comparison remains incomplete.

The [timing diagnostic](v5-timing-diagnostic.json) binds the exact producer, validator and installed clock-source excerpts. The producer unconditionally records `time.Since(rpcStarted).Nanoseconds()` around the actual loop; these zeros are observed clock differences, not unmeasured placeholders. They are retained as **resolution-unresolved** samples, not zero-latency work. The exact clock quantum was not measured.

**Table 1a. The sole emitted v5 Go observation, rejected by the original timing gate.** Elapsed values are nanoseconds (ns); one millisecond is 1,000,000 ns. No ratio or Rust comparison is derived.

| Case | Reported RPC | Start, ns | RPC loop, ns | Stop, ns | Entire case, ns |
| --- | ---: | ---: | ---: | ---: | ---: |
| healthy-after-canceled-observers | 64 | 55,390,200 | 7,360,400 | 101,235,400 | 163,986,000 |
| final-reply | 1 | 50,533,700 | **0, unresolved** | 83,337,900 | 133,871,600 |
| lifetime-cancel | 1 | 57,152,800 | **0, unresolved** | 100,724,000 | 157,876,800 |

These failures remain failures. Later physical closure is not promoted to closure before an earlier deadline. The successful v6 result leaves every predecessor outcome unchanged.

## Consolidated experiment status

**Table 2. Existing C evidence, kept in its original units and source scope.** These rows consolidate recorded outcomes; no #626, #629 or #627 run was repeated for this report. Full evidence commits, executed-source descriptions and count definitions are retained in [experiment-consolidation.json](experiment-consolidation.json).

| Experiment and pinned evidence | Actual recorded result | Applicability and open boundary |
| --- | --- | --- |
| #626 [Relay/CMake profiles, report commit `3f8f5530`](https://github.com/applicate2628/mcp-local-hub/blob/3f8f5530fe8751ff544d0044c19f80d2a25cfb6d/docs/modernization/verification/sdk-consumer-pairs-20261126/sdk-consumer-profile-readback.md) | 16 measured commands; 96 normal CMake replies and 2,304 Relay replies. Separately: 32 CMake and 52 Relay cancelled calls. | Matched A/B variants within each profile. CMake reused a previously produced real trace; no new CMake process ran. Synthetic Windows/amd64 profiles and later source changes are separate; no Rust benefit established. |
| #629 [POST resumption, report at `6808e99b`](https://github.com/applicate2628/mcp-local-hub/blob/6808e99bacc0961fef2a511b1ae1a088f12a1e12/docs/qualification/relay-post-resumption-20261124/README.md) | 281 PASS events / 0 FAIL / 0 SKIP across 74 selected root functions; the real SDK case recorded one effect, one POST, one GET continuation and one delivery. | Events include roots, subtests and package events. Executed Source was `4b6ec399` plus five pinned files; this is targeted request-local resumption evidence, with older failures retained. Full suites, installed Product and other platforms remain outside that run. |
| #627 [Reader partial-read/cleanup run, `1d927f5e`](https://github.com/applicate2628/mcp-local-hub/blob/1d927f5e38b353abe841f974f65a145e2cd50411/docs/modernization/verification/reader-read-cleanup-20261126/README.md) | 21 Rust library tests PASS / 0 FAIL / 0 ignored; three new partial-read plus cleanup-status combinations. | Genuine `ReadFile` refusal after 4,096 bytes; cleanup errors are substituted statuses after successful native closes. Real metadata/`CloseHandle` failure combinations and the Reader Go adapter remain open. |
| #631 [This process-owner consumer v6](consumer-v6/README.md) | Four accepted invocations, 12 cases and 264 RPC; seven unresolved zero RPC samples. | Exact private `3ebe` Source and designated `efa73` DLL. Go and Rust use different closure evidence paths and non-final exit codes. Two observations per arm provide no adoption justification or general API-equivalence claim. |

The report-commit links identify retained evidence, not a blanket assertion that each newer PR head was the compiled Source. The source map records the CMake variants `75dbc640` / `ee20f600`, Relay variants `4b6ec399` / `e33924ac` with the common `279887a4` profile, POST's exact composite inventory, and Reader's eight-file candidate. Replies, cancellations, test events, root functions, Rust test roots, consumer cases and RPC are not summed together.

## Source applicability

Table 3 identifies each source independently. Full SHA-256 file mappings are in the canonical [native source map](full-a217-review/native-dll-source-map.json).

| Role | Exact identity | Applicability |
| --- | --- | --- |
| Consumer production Source | Commit `3ebe335a2f666a5279d7fafc31a6f4dc3feedff4`, tree `67c13b8fc1317b4fd60e3135a3eb8a376fe01817` | 3,508 tracked files, 57,144,526 bytes, plus the separately pinned test overlay; unchanged production bytes |
| Composed A/B context | Commit `e66f5c6fab7da2a2eff16ff1f0891991ca1a839c`, tree `4df69e99dca3f808b26e6a5bf7721bdd0ba3a91c` | Not the consumer Source; C outcomes do not qualify it |
| Full static review baseline | Commit `a217c7799a0214967bf86211542e508162c680b5`, tree `ca118ff1ec513cc830bb8b96b4d6782d23915a26` | Review applicability must be mapped to actual native bytes |
| Native build | Commit `efa73c0e23f7af16b8aa2ccb047b745a58ba4f71` | Original 64-file manifest; 63 files byte-identical to a217, one separately reviewed semantic delta in `src/windows.rs` |
| Used DLL | 192,512 bytes, SHA-256 `dfa87a10a39e9110cda735ae6a987bfa64b03446529072710723e9b6ea0dca56` | Designated native artifact; original historical descendant-closure claim remains false |
| Unchanged v5/v6 overlay | 11,107 bytes, SHA-256 `c7218f274e9ef00906eff79975ca439f48974ef0fd9593f7198fc27a65a7b6c8` | Test-only closure oracle correction |

The native delta imports `STARTF_USESHOWWINDOW` and requests `SW_HIDE` when `!foreground`. The exact [patch](full-a217-review/native-vs-a217-windows.patch) and [supplemental review](full-a217-review/NATIVE-DELTA-REVIEW.md) are retained. That review finds no additional ownership, cleanup, ABI-layout or branch-composition defect in this delta, and identifies its broader initial GUI-window policy. The comment about later `AllocConsole` is intent, not proof of runtime behavior. All 23 Go tests are byte-identical between a217 and the native source manifest. The 63 equal files inherit byte-level review applicability; the changed Rust creation path is covered by the separate bounded delta review.

## Oracle correction

The private consumer owns six original `os.File` wrappers. Their first `Close` result is retained by the existing `sync.Once` owner. Original parent pipe copies close during launch; the retained wrappers are all checked at settlement. The relevant source locations are `internal/process/host_original_windows.go:91-103,136-137`, `internal/process/host_original.go:66-71,352-358`, and the overlay's original-file loop.

On the pinned Go 1.27.4 Windows implementation, `os.File.Close` closes the poll descriptor, waits for destruction, invalidates its native handle, and clears the finalizer. Windows `Stat` can then receive the native invalid-handle error. The corrected oracle requires a nil first close error, `Fd() == uintptr(syscall.InvalidHandle)`, and `Stat` matching either `os.ErrClosed` or `windows.ERROR_INVALID_HANDLE`.

This conjunction distinguishes a usable file, a native handle closed externally while its Go wrapper remains live, a failed first close, and the two supported closed-state errors. It does not establish an earlier deadline by itself; original owner, reader, and deadline records remain separate acceptance conditions.

The [v3-to-v4 patch](source/v3-to-v4-oracle.patch) records the oracle change and its unsupported constant. The [v4-to-v5 patch](source/v4-to-v5-supported-constant.patch) changes only the import and constant package. `golang.org/x/sys v0.47.0` already exists as a direct dependency, and its Windows constant is `syscall.Errno(6)`. Production source and dependency declarations are unchanged.

## Preparation and measurement

The existing controller requires original receipts and exact same-scope manifests at each phase. Both v5 and v6 therefore freshly read the unchanged pinned Source and permitted DLL/proxy donors, materialized the source, formatted the overlay, and built the helper and both arm binaries in their own scopes. Neither attempt reused previous compiled binaries or relabeled a previous same-scope receipt. Deadlines and original process/Job/reader ownership are unchanged.

Both attempts planned Go, Rust, Rust, Go (ABBA), with three cases and 66 remote procedure calls (RPC) per invocation: `healthy-after-canceled-observers`, `final-reply`, and `lifetime-cancel`. V5 emitted only its first Go observation and accepted none. V6 completed the planned sequence and accepted four original receipts containing 12 cases and 264 RPC. The completed v6 count does not fill any missing v5 result. The same unmarked helper and existing `NewStdioHost`/original RPC body are used; the host's linked Rust mark distinguishes the arms. The request path is an in-process HTTP handler with internal session setup, without an MCP initialize handshake or a network listener.

Each measurement command retains its 190-second outer work budget, 180-second Go test timeout and 10-second original cleanup budget. The v5 timing failure stopped that sequence after its first Go command; v6 later completed all four invocations under its separately pinned inputs. Preparation completion does not qualify the workload. Emitted observations and original controller acceptance are separate in the evidence; missing results in failed attempts are not filled from planned counts or borrowed from v6.

## Static review

The canonical [full a217 review](full-a217-review/README.md) covers all 64 inventoried files, 649,751 bytes and 18,440 lines through three disjoint, attributed reviewer slices. Completion means source-review coverage; five findings and one related coverage limit remain unresolved. No native PASS is inferred from those reads.

Its [Go test section](full-a217-review/go-tests-review.md) reads all 23 test files (148,868 bytes, 4,812 lines); 97 top-level test functions are a source census, not an execution count. The [per-file JSON](full-a217-review/go-tests-review.json) includes original hashes, Git blobs, coverage conditions, and limits.

The Go review identifies a conditional test-oracle gap: the ordinary `own()` teardown accepts a native cleanup-failed outcome, so a test relying only on that teardown may pass without surfacing unexpected cleanup failure. Fault-specific cases need deliberate allowances, but ordinary cases should not silently inherit them. Loader tests cover real missing exports/version checks and map/reference counting; they do not demonstrate an actual `FreeLibrary` failure. The fail-label's ignored unload return remains a separate conditional production finding, not a native-proven failure. The full packet also records Rust identity/base ABI conversion issues when a typed failure carries numeric code zero, and a test-local watchdog limitation. Each report preserves its exact conditions and runtime-evidence limits.

## Evidence and provenance

[evidence.json](evidence.json) is a normalized public projection of the original records. It retains source/plan/receipt/capture hashes, sizes, statuses, durations, original native closure fields, and reader completion. It omits machine paths, operating-system identifiers, command argument paths, and raw captures. It is not byte-identical to an original receipt. The originals stay in their original scopes.

This directory is additive. The existing historical `evidence.json`, `draft-plan.json`, test data, and earlier review are retained. The parent README links this continuation and explicitly labels its old narrative as historical; its prior body is preserved exactly. The v6 supplement adds actual terminal outcomes while v3, v4, v5, the failed parser harness and older public FAIL/UNKNOWN/V1 records retain their original classifications.
