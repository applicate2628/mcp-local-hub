# Parole queue-order candidate: positive and negative overlay results

**The test-only dispatch-barrier candidate passed its one selected default-mode test. The same test rejected the deliberate `PostSelf` → `TryPost` mutation at the expected `quarantined`-versus-`exiting` assertion.** Both original commands and the complete five-command batch reached physical `CLOSED`. The execution left Source52 unchanged and did not establish full A/B qualification. [1][2]

This is an additive result for the 2026-11-26 candidate execution. The [initial Source52 controller failure](README.md), the [separate failure-stack diagnostic](parole-diagnostic-20261126.md), older immutable source capsules and any separate `FAIL`/`UNKNOWN` history keep their original outcomes. The earlier candidate manifest's `NOT_RUN` fields describe its preparation cutoff; they are not rewritten. [1][3][4]

## Contents

1. [Observed results](#1-observed-results)
2. [What the candidate tests](#2-what-the-candidate-tests)
3. [Exact source and overlay binding](#3-exact-source-and-overlay-binding)
4. [Original command and reader closure](#4-original-command-and-reader-closure)
5. [Applicability and preserved failures](#5-applicability-and-preserved-failures)
6. [Evidence pins and sources](#6-evidence-pins-and-sources)

## 1. Observed results

Both commands selected only `^TestSupervisorController_ParoleQueuedStop_PostSelfHonorsStop$` in `./internal/cli/`, with `go test -json -count=1`, no build tags, a five-minute Go test process timeout, and the existing 360-second command-owner budget. The positive overlay substituted the candidate test file. The negative overlay substituted that same file and the one-site production mutation. These were distinct original commands with distinct capture names. [1][2]

**Table 1. Measured results.** Test and package durations come from the terminal JSON events; command duration comes from the original owner. These quantities measure different boundaries.

| Variant | Go command exit | Test terminal action | Test duration, s | Package terminal-event duration, s | Original command duration, s | Interpretation |
| --- | ---: | --- | ---: | ---: | ---: | --- |
| Candidate test + original production `PostSelf` | 0 | PASS | 0.27 | 0.799 | 145.2838723 | Selected candidate test passed |
| Same candidate test + one-site `TryPost` negative mutation | 1 | FAIL | 0.06 | 0.584 | 53.0194957 | Expected negative assertion was observed |

The negative capture identifies `supervise_lostchild_f6_f2_test.go:651` and contains the exact diagnostic fragment `after queued dispatch = quarantined (present=true), want exiting`. Its failing condition is the intended state assertion, rather than a compiler error or a dispatch-barrier timeout. Exit code 1 alone was not used to admit this control. The package output text separately displays `0.582s`; the terminal package JSON event records `Elapsed: 0.584`. Table 1 uses the latter and [the evidence JSON](parole-barrier-controls-20261126.json) preserves both values. [1][2]

The base64 transport was decoded strictly and compared with the original measured capture metadata. The positive capture is **1,425 bytes**, SHA-256 `bd09e749df489d7b3ad5337cb3425b00eaf00026170e07f12c7b3b641f263857`; the negative is **1,806 bytes**, SHA-256 `a04fa3663d3e143db77975358cf50c5e4c97ac3a13fc5e9bd0afab6b116493d5`. Both stderr streams were empty and fully closed. This public report retains selected outcomes rather than reproducing the raw captures. [1][2]

## 2. What the candidate tests

The test exercises queue order when a quarantined daemon becomes eligible for a parole restart while a stop intent is already queued. Its original fixture preloads the events before starting the EventLoop, and an on-loop sentinel changes the cached intent from running to stopped between the tick and the intent event. This supplies a controlled ordering case for the production `PostSelf` dispatch. [2]

With `PostSelf`, the restart is drained through the priority self-post path before the queued stop is processed. The fake spawn and subsequent stop should leave the daemon in `StExiting`. With `TryPost`, the automatic restart goes to the ordinary channel tail: the stop is processed while the daemon remains quarantined, and the existing `require_running_intent` guard refuses the late restart. The final state is then `StQuarantined`. The negative mutation retains that guard, so the test distinguishes the current production contract rather than an obsolete unguarded restart. [2]

The candidate replaces the target test's polling call with the **existing five-second dispatch barrier**, followed by a direct `StExiting` assertion. The barrier waits for the original queued handlers and their priority self-posts to finish. This matters because the handler stores `StSpawning` before completing synchronous persistence and the fake spawn. An intermediate state alone does not mean dispatch has finished. The candidate also corrects comments that still described the negative result as `StRunning`. [2][3]

The candidate does not alter the two-second general state-wait helper or its other callers, the five-second barrier implementation, real persistence, the EventLoop's original cancellation/join cleanup, the stop assertion, or positive-run production code. The prior three-line failure-stack overlay is absent. The deliberate `TryPost` change belongs exclusively to the negative control and is not a production candidate. [2]

## 3. Exact source and overlay binding

The base is immutable Source52 commit `52a2d9ef57b7c372b9fa149adf062ad7e503b22d`, tree `51258ebd9ad3a654418b6ecb9163947734ef0314`. Its [source capsule](../composed-functional-source-20261126/source52-363b/README.md) preserves the earlier source history. Successful post-run `git diff --exit-code` against that commit and empty `git status --porcelain=v1` output confirm that the execution left the checkout unchanged. The recorded HEAD capture digest also matches this commit followed by a newline. [1][4]

**Table 2. Exact source images.** Git blob identifiers are recomputed from the file bytes using Git's blob framing; candidate and negative rows identify overlay content, not committed Source52 files.

| Role | Repository path | Bytes | SHA-256 | Git blob identifier |
| --- | --- | ---: | --- | --- |
| source52_test_preimage | `internal/cli/supervise_lostchild_f6_f2_test.go` | 41462 | `8bd40a8b1d41becc6d21192044883f994516e666951d781a61cc1eda71e65be4` | `8d6414f0e64d6dd02fbacbca357a6edd692aa359` |
| positive_candidate_test | `internal/cli/supervise_lostchild_f6_f2_test.go` | 41482 | `ab655745fd82f65864dc82d35face33f367201e8db12a9996868628d8025f13a` | `993a90c5eae0c40a47cd26f339c9dbd9e23876a2` |
| source52_production_controller | `internal/cli/supervisor_controller.go` | 246194 | `a3216efbca82df399c8546a0eaa851e6bb0a69b1f32c09bf2a0ff4c273cc5a06` | `7c25903c38eff08844e957310ff678b4e0e33ffe` |
| negative_only_controller | `internal/cli/supervisor_controller.go` | 246193 | `fc46f9402d72e505aa72ba8b1e71fb39ef770be43dd61761336b8482efff5258` | `fbb87fa7b8d0c5b975613216cf86243077928c98` |

The positive overlay changes only `internal/cli/supervise_lostchild_f6_f2_test.go`. The negative overlay adds only `internal/cli/supervisor_controller.go`, where the parole enqueue changes a single method token from `PostSelf` to `TryPost` while retaining the event kind, task name and `require_running_intent` body. Patch and overlay hashes appear in Table 4. The execution configuration bytes match the bytes embedded in the retained launcher snapshot. This binds the stated selectors and expected negative marker to the reviewed configuration without publishing machine-specific paths. [2]

## 4. Original command and reader closure

The batch's original parent exited 0 after **203.1781622 seconds**, was reaped, and closed its controller capture handles. Its original receipt digest is `f558bde54cb3f313859d856ddf11913fae93f4244a617164577769193aec47f5`. The locally retained receiving summary has a separate digest listed in Table 4; it is not presented as the original receipt bytes. [1]

**Table 3. Original closure observations.** All five commands report direct exit known/reaped, zero active job members, native handles closed, original readers joined, and complete capture measurement. No command required job termination.

| Original command | Exit | Physical closure | Original stream readers |
| --- | ---: | --- | ---: |
| HEAD check | 0 | CLOSED | 2 |
| Positive candidate test | 0 | CLOSED | 2 |
| Negative mutation test | 1, expected | CLOSED | 2 |
| Diff against Source52 | 0; empty output | CLOSED | 2 |
| Porcelain status | 0; empty output | CLOSED | 2 |

For each stdout/stderr pair, the original worker completed, reached end-of-file, returned from pipe close and capture close, and reported no read error or capture-limit overflow. The batch's `PASS` means that its positive expectation, explicit negative expectation and source-preservation checks were satisfied with this closure. It does not relabel the negative Go test as a passing test. [1]

## 5. Applicability and preserved failures

This result supports the narrow fixture correction: after the original dispatch drains, the candidate accepts the production queue behavior and rejects the specified queue-order mutation. It is a default-mode overlay result for one test. It is not a run of the unchanged Source52 test bytes, a tagged-mode result, a full API/CLI campaign, a Product runtime acceptance, or an installed application/Scheduler result. [1][2]

The initial Source52 controller run remains `FAIL/CLOSED`: the selected Parole test observed `spawning` instead of `exiting` after its original wait, and its 14.05-second test run has no proven exact blocking instruction. The later diagnostic overlay also remains `FAIL/CLOSED`; its captured original EventLoop stack places that diagnostic run inside `windows.FlushFileBuffers`, before fake spawn and `HealthOK`. These new passing/negative controls do not determine the external cause or duration of that native flush and do not retroactively prove the initial run's cause. [3]

The current checkout remained Source52 at this report's execution cutoff. Integrating the reviewed test candidate into a successor Source, qualifying tagged/full suites, and admitting any resulting test image require their own exact source and execution evidence. Older held negative scopes remain untouched; this independently closed batch neither releases them nor upgrades any historical `UNKNOWN`. [1][4]

## 6. Evidence pins and sources

**Table 4. Retained input and receiving artifacts.** Byte counts and hashes identify the original retained local files; machine locations and raw capture content are omitted. The frozen preparation manifest deliberately keeps its earlier `NOT_RUN` state.

| Artifact | Bytes | SHA-256 |
| --- | ---: | --- |
| `native-closed-receiving.json` | 11189 | `acf99329c0ee2e310146cb40a99fcde98780c9ec3120cd55192107342cb60cd4` |
| `native-captures.transport.json` | 4338 | `a6d2d226dfd5668e021f39acf5923238e1e7fa7b2858fc4c773809fccec564ed` |
| `native-execution-config.json` | 3106 | `536332d8ae04b9c5ca9a34d56e8b01f1305b314fee7d252b7452e89af5ac81e8` |
| `launch-parole-candidate-52a2-363b-v1.ps1` | 9157 | `20a5f6283765dcf9bcbf864a9af9f22a355aa408158c6be961e425c404a525d2` |
| `positive-overlay.json` | 235 | `df1ff9d8c7fd68ebd033704809245dd2265574df7795d2b8c2a69c4b22468d5c` |
| `negative-overlay.json` | 430 | `35d2e6d822a26880b439d5d78b9b513ae5ac46d3f1e4cc4a024a9a4d2d8478f3` |
| `queued-stop-dispatch-barrier.patch` | 2670 | `96d432f7fdc9ec7c61ca0fa372249029d975ba8cfcc597300b43710b1e901678` |
| `negative-parole-trypost.patch` | 567 | `6bccf7c5e909cd2b2261f969b9ca8001e8d45995518ac61e1e96c1c990cfc6ae` |
| `provenance/coordinator-v2.snapshot.txt` | 8463 | `74ddf8a3610dc6c8f1bfcda4dbc349833bf334baf0f695999196fe5bd340b5fe` |
| `manifest.json` | 3318 | `933fcfe0660345b29d17fbc7f2bcf99c693b26f83c0640b97abb8440830f4a53` |

1. **Original execution receiving and decoded test events.** Original receipt SHA-256 `f558bde54cb3f313859d856ddf11913fae93f4244a617164577769193aec47f5`; distinct receiving-summary and transport pins in Table 4. The [machine-readable companion](parole-barrier-controls-20261126.json) records sanitized closure, selected events, counts and durations.
2. **Exact candidate, negative patch, overlay mappings and execution configuration.** Tables 2 and 4 bind the source images and commands. Frozen preparation manifest SHA-256 `933fcfe0660345b29d17fbc7f2bcf99c693b26f83c0640b97abb8440830f4a53` remains unchanged.
3. **Preserved failed executions.** [Source52 initial report](README.md), receipt `5ce22474571013d116634bf2b08043372487b594e577122e341f08eb8ca7b430`; [Parole failure-stack diagnostic](parole-diagnostic-20261126.md), receipt `99124603fad77b49a254559dda80025e1ef7ba5b74f91a40b5990caec92d888f`.
4. **Immutable source and wider scope.** [Source52 capsule](../composed-functional-source-20261126/source52-363b/README.md), [Source6d predecessor capsule](../composed-functional-source-20261126/source6d-363b/README.md), and [the earlier A/B status snapshot](../ab-functional-latest-363b-20261126/README.md). This additive result does not rewrite any of those snapshots.
