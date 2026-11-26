# Source52 Parole: failure-only stack diagnostic

**The diagnostic run failed and closed normally. Its stack locates the original event loop inside `FlushFileBuffers`, before fake spawn and HealthOK.** This finding applies to that diagnostic run. The exact blocking instruction in the earlier 14.05 s failure remains undetermined.

## Contents

1. [Exact experiment and result](#1-exact-experiment-and-result)
2. [Observed instruction and test boundary](#2-observed-instruction-and-test-boundary)
3. [Prepared follow-up](#3-prepared-follow-up)
4. [Preserved history and limits](#4-preserved-history-and-limits)
5. [Sources](#5-sources)

## 1. Exact experiment and result

The source remains commit `52a2d9ef57b7c372b9fa149adf062ad7e503b22d`, tree `51258ebd9ad3a654418b6ecb9163947734ef0314`. A Go overlay added three failure-only diagnostic lines before the existing fatal assertion in `lostChildWaitForSMState`. The original two-second wait, expected state and original-loop cleanup join were unchanged. The overlay used a one-mebibyte buffer for `runtime.Stack(..., true)`; the captured stack was not truncated. [S1, S2]

**Table 1 — Exact diagnostic result.**

| Item | Observed |
|---|---|
| Test | `TestSupervisorController_ParoleQueuedStop_PostSelfHonorsStop` |
| Mode | Default, exact test filter, `-count=1`, existing `-timeout 5m` |
| Test result / time | FAIL / 3.16 s |
| Package result / time | FAIL / 4.639 s |
| Original test command | Exit 1 / 25.568673 s / CLOSED |
| Original commands / readers | 2 / 4, all CLOSED |
| Original controller | Exit 1, reaped; captures closed; 26.314 s reported |
| Original receipt SHA-256 | `99124603fad77b49a254559dda80025e1ef7ba5b74f91a40b5990caec92d888f` |

The receipt hash and rounded controller interval come from the original receiving owner. The local review inspected parsed derivative receipt and event JSON, whose separate hashes are recorded in [the evidence](parole-diagnostic-20261126.json). It does not substitute those derivative bytes for the original receipt. [S1]

**Table 2 — Overlay byte binding.**

| Input | SHA-256 |
|---|---|
| Original Source52 test file | `8bd40a8b1d41becc6d21192044883f994516e666951d781a61cc1eda71e65be4` |
| Failure-only diagnostic postimage | `3ac226efd68d34c6109aa79b2e36b91d9f4cb16add8bff57416fe026748b3d66` |
| Three-line diagnostic patch | `1c645109a1665b3baeadff2a3bf35cf34c1f74b9c1fd1223306b4f489902f858` |

## 2. Observed instruction and test boundary

At the assertion point the state was `spawning`, while the test expected `exiting`. The original loop's stack contains the following selected chain; intermediate secure-writer wrappers are omitted from this table but the retained private diagnostic includes them. [S1]

**Table 3 — Selected repository-relative frames, outer caller to observed native instruction.**

| Frame | Source location |
|---|---|
| `EventLoop.Run` | `internal/api/supervisor_event_loop.go:271` |
| `supervisorController.handleLoopEvent` | `internal/cli/supervisor_controller.go:3427` |
| `persistDaemonRuntimeTracker` | `internal/cli/supervisor_runtime_tracker.go:1385` |
| `DaemonRuntimeTracker.PersistTo` | `internal/cli/supervisor_runtime_tracker.go:1320` |
| `MutateSupervisorStateIfChangedContext` | `internal/api/supervisor_state.go:455` |
| `WriteStateFileBytesLockHeld` | `internal/api/state_file_helper.go:115` |
| `secureWriteClientConfigToResolvedParentObserved` | `internal/api/secure_write_windows.go:330` |
| `windows.FlushFileBuffers` | Native flush call observed in that original loop |

Source publishes the new state at controller line 3342 and then performs persistence at line 3427. Fake spawn is later at line 3698 and the HealthOK priority self-post is at line 3755. The diagnostic therefore failed before the test reached the rest of the intended restart/stop sequence. A published state is not a completion acknowledgement for its handler. [S1, S3]

The snapshot identifies an instruction at one point in time. It does not measure that syscall's duration, identify the external cause of its latency, or demonstrate an ordering defect in `PostSelf`. Those claims are not made.

## 3. Prepared follow-up

A one-file test candidate reuses the existing five-second `paroleBarrierSync` and immediately asserts a present `StExiting` state afterward. The test's prefilled event sequence, stopped-intent sentinel, production persistence and original-loop cleanup join remain unchanged. The common two-second state helper remains unchanged for every other test. This waits for the original dispatch boundary that the ordering test needs; a dispatch that exceeds the existing barrier bound still fails. The candidate is **PREPARED / NOT_RUN**. Its test postimage SHA-256 is `ab655745fd82f65864dc82d35face33f367201e8db12a9996868628d8025f13a`. [S1, S3]

The isolated negative plan changes only the parole restart enqueue from `PostSelf` to `TryPost`, retaining `require_running_intent=true`. On current Source the delayed restart is refused after stopped intent is applied, so the task remains quarantined and the exact exiting assertion must fail. The old comment predicting a running task is obsolete because the loop-side stopped-intent guard is already present. The negative experiment is also **NOT_RUN** and is excluded from any production source change.

## 4. Preserved history and limits

The [initial Source52 result](README.md) remains FAIL/CLOSED with receipt `5ce22474571013d116634bf2b08043372487b594e577122e341f08eb8ca7b430`. Its build/vet PASS and 47 top-level controller PASS / 49 PASS including subtests, plus one Parole failure, are unchanged. The initial 14.05 s test failure has no corresponding instruction snapshot; its exact blocking instruction remains **UNDETERMINED**. This later diagnostic does not reclassify that history.

This single default-mode diagnostic adds no full-suite or tagged qualification. Its bytes differ from immutable Source52 only through the stated diagnostic overlay. It changes no production durability, timer, generation refusal or scheduler authority. Previous Source6d failure history and the separate historical held negative batch retain their prior classifications and scopes.

## 5. Sources

- **S1.** [Machine-readable diagnostic evidence](parole-diagnostic-20261126.json): original receiving hash, derivative input hashes, exact overlay, selected frames and claim limits.
- **S2.** [Source52 reconstruction capsule](../composed-functional-source-20261126/source52-363b/README.md): immutable commit/tree and predecessor dependency. The diagnostic overlay is a separate input.
- **S3.** Exact Source52 code: `supervise_lostchild_f6_f2_test.go` fixture/queue test and `paroleBarrierSync`; `supervisor_controller.go` state publication, persistence, stopped-intent guard and barrier aliases; `supervisor_event_loop.go` priority drain. The controller blob is `7c25903c38eff08844e957310ff678b4e0e33ffe`; its full transferred bytes were independently checked against the original immutable Git capture.
- **S4.** [Initial Source52 campaign](README.md): earlier build, vet and focused result preserved as a separate campaign.
