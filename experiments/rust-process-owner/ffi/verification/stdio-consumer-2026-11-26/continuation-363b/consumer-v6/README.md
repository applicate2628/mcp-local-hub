# Consumer v6: honest elapsed-sample validation

**PASS: four accepted Go–Rust–Rust–Go runs, 12 cases and 264 RPC.** All four original measurement commands, their original Jobs and eight output readers closed within their unchanged work/cleanup budgets. Seven raw RPC-time samples remain resolution-unresolved. The [v3–v5 record](../README.md) retains every earlier failure.

## Contents

- [Change and source](#change-and-source)
- [Original phase observations](#original-phase-observations)
- [Accepted observations and raw timings](#accepted-observations-and-raw-timings)
- [Parser check history](#parser-check-history)
- [Interpreting elapsed samples](#interpreting-elapsed-samples)
- [The two closure evidence paths](#the-two-closure-evidence-paths)
- [Acceptance boundary](#acceptance-boundary)

## Change and source

The [exact patch](timing-correction.patch) changes the controller's elapsed-value rule from positive integers to nonnegative integers. Missing fields, booleans, nonintegers and negative values remain rejected. The original producer receipt is retained unchanged. A separate controller object lists raw zero samples and classifies them as resolution-unresolved. It does not assert a clock quantum, zero latency or a speed ratio involving zero.

Production commit `3ebe335a2f666a5279d7fafc31a6f4dc3feedff4`, tree `67c13b8fc1317b4fd60e3135a3eb8a376fe01817`, is unchanged. The Go overlay remains 11,107 bytes, SHA-256 `c7218f274e9ef00906eff79975ca439f48974ef0fd9593f7198fc27a65a7b6c8`. The designated DLL remains SHA-256 `dfa87a10a39e9110cda735ae6a987bfa64b03446529072710723e9b6ea0dca56`. Full a217 applicability and the native efa73 delta are documented in the canonical [64-file static review](../full-a217-review/README.md).

Only two controller functions change: `receive_consumer` validates the sample range, and `execute` attaches separate elapsed metadata. The added `elapsed_timing_metadata` function performs a pure data projection. All other function/class definitions and original lifecycle mechanisms retain their exact semantics. Scope and dependent pins are freshly rebound; no previous compiled file or original receipt is relabeled as a v6 result.

## Original phase observations

**Table 1. Received terminal phase records.** Seconds are the original controller-parent elapsed observations, not consumer performance measurements. Complete original command and reader status and exact hashes are retained in [evidence.json](evidence.json).

| Phase | Status | Parent seconds | Original commands |
| --- | --- | ---: | ---: |
| guard | PASS | 2.525362 | 2 |
| prepare | PASS | 57.884944 | 1 |
| format | PASS | 4.115670 | 1 |
| compile | PASS | 153.473298 | 7 |
| measure | PASS | 6.943509 | 4 |

Current phase-state map: `{"guard":"PASS","prepare":"PASS","format":"PASS","compile":"PASS","measure":"PASS"}`. The original measure receipt is SHA-256 `26595afe69d4a034cec040afadf3b4abe6bd51648681deccc262d0b4b92ae245`; its plan is `fee8d396a39a4e9f235513f04d438ebe7336626d9ba53adbc1c69979156385cc`. The parent separately recorded its original controller reaped and captures closed; the outer launcher received and disposed the same original process object. No new observer repaired a missing closure fact.

## Accepted observations and raw timings

**Table 2. Four accepted invocations, in execution order.** The sum of case times adds the three recorded case intervals. It omits setup outside the cases; the original command duration also includes host startup and the test harness. The quantities have distinct boundaries and units.

| Order | Arm | Accepted cases | Accepted RPC | Sum of case times, ms | Original command, s | Unresolved zero samples |
| --- | --- | ---: | ---: | ---: | ---: | ---: |
| 1 | go | 3 | 66 | 421.7357 | 0.563915 | 2 |
| 2 | rust | 3 | 66 | 413.2553 | 0.726186 | 2 |
| 3 | rust | 3 | 66 | 416.4953 | 0.579462 | 2 |
| 4 | go | 3 | 66 | 536.4578 | 0.674595 | 1 |

**Table 3. All 12 accepted cases.** Values below are converted from the exact integer nanoseconds in [timings.json](timings.json) using 1 ms = 1,000,000 ns. A raw zero is displayed explicitly and never replaced by 1 ns. These are two observations per arm for each case, not an adoption justification or a stable performance estimate.

| Order / arm | Case | Start, ms | RPC loop, ms | Stop, ms | Entire case, ms |
| --- | --- | ---: | ---: | ---: | ---: |
| 1 / go | healthy-after-canceled-observers | 53.4563 | 4.6923 | 100.9488 | 159.0974 |
| 1 / go | final-reply | 39.6875 | 0 (unresolved) | 77.5526 | 117.2401 |
| 1 / go | lifetime-cancel | 44.6431 | 0 (unresolved) | 100.7551 | 145.3982 |
| 2 / rust | healthy-after-canceled-observers | 65.4206 | 3.6635 | 77.2375 | 146.3216 |
| 2 / rust | final-reply | 57.1534 | 0 (unresolved) | 79.9023 | 137.0557 |
| 2 / rust | lifetime-cancel | 52.6479 | 0 (unresolved) | 77.2301 | 129.8780 |
| 3 / rust | healthy-after-canceled-observers | 54.8790 | 3.1545 | 79.8017 | 137.8352 |
| 3 / rust | final-reply | 52.6020 | 0 (unresolved) | 104.5789 | 157.1809 |
| 3 / rust | lifetime-cancel | 36.1194 | 0 (unresolved) | 85.3598 | 121.4792 |
| 4 / go | healthy-after-canceled-observers | 47.9422 | 6.3097 | 100.7408 | 154.9927 |
| 4 / go | final-reply | 51.6260 | 0 (unresolved) | 77.6981 | 129.3241 |
| 4 / go | lifetime-cancel | 100.0418 | 0.3703 | 151.7289 | 252.1410 |

The 64-RPC loop recorded 4.6923 and 6.3097 ms for Go, and 3.6635 and 3.1545 ms for Rust. Other intervals vary, including the fourth run's startup/stop. No speed ratio or general Rust speedup is asserted. Seven single-RPC samples are zero at the observed clock resolution; their latency remains unresolved. Counts come from accepted original receipts, not from the planned workload.

## Parser check history

The original staged checker failed before invoking a parser: it expected `require` to be defined in the runner, while the runner imports that function from `focused_runner.py`. This failed attempt and its original checker hash remain in the [check history](validator-checks.json). No native phase ran during that failed check.

The [supplemental patch](checker-supplement.patch) loads only the actual `require` function's abstract syntax tree (AST) from the exact pinned lifecycle file. It executes no module initializer and synthesizes no alternate assertion behavior. The [supplemental checker](testdata/check_timing_validator_v2.py) passed 19 pure parser checks: the original v5 rejection, preserved zero values and distinct metadata, explicit synthetic range fixtures, invalid type/range/missing-field rejection, and the retained ownership/source/deadline/identity guards.

The synthetic positive and all-zero dictionaries exist only within those parser checks. They are not measured values. These 19 checks are not 19 native Go/Rust tests and do not change the failed v5 campaign result.

## Interpreting elapsed samples

The pinned Go producer surrounds its real RPC loop with `time.Now` and `time.Since(...).Nanoseconds()`. Equal clock readings can yield a raw zero. Such a value remains visible as an unresolved duration; it cannot mean the work was instantaneous. No minimum value is inserted and no ratio involving a zero sample is derived. The seven-file controller census found no latency reducer or statistical division downstream of receipt validation and retention.

The four metrics are elapsed nanoseconds: startup covers host start through the helper's ready barrier; RPC covers the actual request loop; stop covers the case's child-exit observation and terminal original-owner receiving; entire-case time runs from startup through the final closure checks. The single-RPC and 64-RPC cases have different workloads and are kept separate. A positive sample also remains subject to its clock's resolution.

## The two closure evidence paths

**Table 4. Actual consumer-level proof fields, common to each arm's six cases.** These fields are preserved separately from the outer command Job observations.

| Consumer proof field | Go cases | Rust cases |
| --- | --- | --- |
| `go_job_selected` | `true` | `false` |
| `original_job_exited` | `false` | `true` |
| `members_exited` | `true` | `false` |
| `accounting_known` | `true` | `false` |
| `final_active` raw field | `0`, with known accounting | `0`, without an accounting claim |

The receiving predicate accepts either original Job exit, or observed member exit with known accounting. Go used the latter proof; Rust used original Job exit. Rust's false `accounting_known` and `members_exited` fields remain false. Its raw `final_active = 0` is not reinterpreted as measured accounting. The outer measurement-command Jobs separately reported known active-zero closure for all four commands.

Normal final-reply root exit was 0 in both arms. Healthy explicit stop and lifetime cancellation recorded root exit 1 in Go and 0 in Rust. Those raw values remain in the evidence; the experiment does not assert identical API or exit-code semantics. Both arms met the tested complete-owner and primary-cancellation contracts, including six successful original file closes, eight received completion signals and the unchanged original stop deadline in every case.

## Acceptance boundary

The executed order was Go/Rust/Rust/Go, each with three accepted cases and 66 RPC. Each command retained 190 seconds outer work, 180 seconds Go test timeout and 10 seconds cleanup; original host deadlines and identity/owner/test-lifecycle checks were unchanged. No competing native build was launched by this workflow during the released measurement lane. This is not a measurement of every background activity on the machine.

The four accepted records establish these consumer scenarios on the exact private Source and designated DLL. Two observations per arm do not justify Rust adoption. A decision still needs demonstrated operational benefit or an independent product requirement, resolution of the review findings, and the relevant product/platform acceptance. The private Source does not qualify the separate composed e66 context or its successors. Installed Product, Reader #627's real metadata/close-failure combinations and general API equivalence remain outside this result.
