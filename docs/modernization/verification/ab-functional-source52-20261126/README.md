# A/B functional progress on Source52

**Build and vet passed. The focused default controller selection failed one test, and all four original commands and eight original readers closed. The batch is FAIL/CLOSED. Tagged controller tests were not run. This is not full qualification.**

## Contents

1. [Source and scope](#1-source-and-scope)
2. [Measured command results](#2-measured-command-results)
3. [Focused test results](#3-focused-test-results)
4. [What the failure establishes](#4-what-the-failure-establishes)
5. [Diagnostic follow-up — not run](#5-diagnostic-follow-up--not-run)
6. [Preserved history and remaining scope](#6-preserved-history-and-remaining-scope)
7. [Sources](#7-sources)

## 1. Source and scope

Table 1. Immutable Source identity.

| Item | Value |
| --- | --- |
| Source commit | `52a2d9ef57b7c372b9fa149adf062ad7e503b22d` |
| Tree | `51258ebd9ad3a654418b6ecb9163947734ef0314` |
| Parent | `6d4fa7b8f9b86079020af125b030a26350cb5bb0` |
| Source commit/clean readback receipt SHA-256 | `7310b496e3d248c41f2035ff0c4aa942c142330f4131fade9b4ba235a422c9a3` |
| Original focused batch | `ab-regressions-52a2-363b-v1` |
| Original batch receipt SHA-256 | `5ce22474571013d116634bf2b08043372487b594e577122e341f08eb8ca7b430` |

Source52 adds the [original lost-child loop cleanup join][2] to Source6d. Its exact identity, incremental bundle and raw commit are recorded in the separately prepared [Source52 capsule][1]. The capsule is prepared separately for the same publication; it does not convert test failure into acceptance.

This report records the completed Source52 attempt. It leaves the [Source6d progress report][3] and lost-child change document unchanged.

## 2. Measured command results

Table 2. Original native commands.

| Command | Result | Original exit code | Command elapsed time | Physical receiving |
| --- | --- | ---: | ---: | --- |
| `git rev-parse HEAD` | PASS; exact Source52 | 0 | 0.0733 s | CLOSED |
| `go build ./...` | PASS | 0 | 25.6907 s | CLOSED |
| `go vet ./...` | PASS | 0 | 36.9042 s | CLOSED |
| Focused default CLI controller tests, below | FAIL | 1 | 41.8566 s | CLOSED |

The exact logical test command was:

```sh
go test -json -count=1 -timeout 5m -run '^TestSupervisorController_(OrphanReap.*|StaleLivenessDisownDroppedByGeneration|QuarantineParole_.*|Parole.*)$' ./internal/cli/
```

The original receipt reports each direct process reaped, its native handles closed and no active process remaining in its owned group. Both original readers for every command completed, observed EOF and returned from pipe/capture closure. All original readers joined and command quiescence completed. The machine-readable [evidence](evidence.json) retains these predicates and capture hashes without local machine paths or process identifiers.

CLOSED describes completion of the original execution and resources. It does not change the failed test result.

## 3. Focused test results

Table 3. Default CLI controller result.

| Measure | Result |
| --- | --- |
| Top-level tests passed | 47 |
| Passing tests including subtests | 49 |
| Top-level tests failed | 1 |
| Failures including subtests | 1 |
| Package result | FAIL |
| Package elapsed time reported by Go | 24.367 s |
| Selected `TestSupervisorController_OrphanReap.*` cases | PASS |
| `TestSupervisorController_StaleLivenessDisownDroppedByGeneration` | PASS |
| Tagged controller selection | NOT_RUN |

Package time differs from the outer command's elapsed time; neither number is the duration of a specific handler operation.

The sole reported failure was:

- Test: `TestSupervisorController_ParoleQueuedStop_PostSelfHonorsStop`.
- Source location: `internal/cli/supervise_lostchild_f6_f2_test.go:651`.
- Observed state: `spawning`.
- Expected state: `exiting`.
- Test duration: 14.05 seconds.
- Existing helper observation window: two seconds.

The passing orphan and stale-generation cases are evidence for their selected cases in this command. They do not imply acceptance of all tests outside the filter.

## 4. What the failure establishes

The assertion establishes that the expected `exiting` state was not observed within the existing window. The original failed run has no stack snapshot identifying the blocking instruction; that instruction remains **UNDETERMINED**.

Bounded source review found a concrete intermediate state: `handleLoopEvent` stores `spawning` before synchronously persisting runtime state, then calls the fake spawn and posts `EvHealthOK`. The persistence call reaches a process-global mutation mutex and a background-context flock/read/secure-write path. Observing the state is therefore not a completion barrier for the original handler.

The reviewed queue implementation dispatches synchronously and drains handler self-events before the next external event. No PostSelf ordering defect was established from this failure log. The total test duration includes setup and cleanup and does not identify time spent in persistence or the new cleanup join. Causation by the Source52 change is also unproven. No production fix is claimed here.

## 5. Diagnostic follow-up — not run

A separate diagnostic overlay is prepared to add a bounded `runtime.Stack` snapshot immediately before the existing helper Fatal. It preserves the two-second wait, expected state, existing assertion and original-loop cleanup.

**Diagnostic execution status: NOT_RUN.** Its prepared test-file SHA-256 is `3ac226efd68d34c6109aa79b2e36b91d9f4cb16add8bff57416fe026748b3d66`; the original Source52 test file remains `8bd40a8b1d41becc6d21192044883f994516e666951d781a61cc1eda71e65be4`.

Any future diagnostic execution will be recorded separately with its actual overlay hashes, original execution/reader closure and observed result. It will execute modified diagnostic test bytes against Source52 production and will not be labeled an unchanged-Source52 qualification. The failed original result above remains preserved.

## 6. Preserved history and remaining scope

Table 4. Scope boundaries.

| Item | Status |
| --- | --- |
| Current Source52 build and vet | PASS |
| Current focused default controller command | FAIL/CLOSED |
| Current tagged controller selection | NOT_RUN |
| Current full default suite | NOT_RUN |
| Current full tagged API/CLI suite | NOT_RUN |
| Diagnostic overlay execution | NOT_RUN |
| Full A/B qualification | Not established |
| Installed lifecycle / release acceptance | Not established |

The earlier Source6d FAIL/CLOSED result retains its original scope. The older negative-control invocation's physical UNKNOWN remains unchanged; this independent Source52 batch does not settle, replay or reclassify that held invocation. Historical documents and evidence are retained.

## 7. Sources

1. [Exact Source52 capsule, incremental bundle and raw commit][1].
2. [Original lost-child loop cleanup change][2].
3. [Preserved Source6d attempt and prior history][3].
4. [Sanitized machine-readable Source52 execution evidence](evidence.json), derived from the original receipt identified in Table 1 and the root's parsed test ledger.
5. Source52 `internal/cli/supervisor_controller.go`:3342,3420–3431,3698,3755; `internal/api/supervisor_event_loop.go`:139–155,241–283; `internal/cli/supervisor_runtime_tracker.go`:1296–1320; `internal/cli/supervise_state_mutation.go`:19–26; `internal/api/supervisor_state.go`:419–455.

[1]: ../composed-functional-source-20261126/source52-363b/README.md
[2]: ../lostchild-loop-cleanup-20261126/README.md
[3]: ../ab-functional-source6d-20261126/README.md

