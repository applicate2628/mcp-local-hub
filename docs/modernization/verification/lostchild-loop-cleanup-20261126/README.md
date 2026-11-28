# Lostchild fixture cleanup — Source52a2

**The one-file cleanup change is independently reviewed, mapped to the exact public #664 preimage, and committed in Source52a2. The root received its clean Source identity; tests of Source52a2 remain `NOT_RUN` in this record.** This supplements the immutable [Source6d progress record](../ab-functional-source6d-20261126/README.md); it does not replace its failed result.

## Contents

1. [Exact public mapping](#exact-public-mapping)
2. [Observed failure and cleanup change](#observed-failure-and-cleanup-change)
3. [Review and remaining acceptance](#review-and-remaining-acceptance)
4. [Preserved history](#preserved-history)
5. [References and terms](#references-and-terms)

## Exact public mapping

Table 1. Exact source-file mapping.

| Item | Identity |
| --- | --- |
| Existing PR | #664, `fix/p0-test-entry-preserve-20261121` |
| Public head checked | `0e8bec723af3f4a52f158e29d2447836ae887374` |
| Changed file | `internal/cli/supervise_lostchild_f6_f2_test.go` |
| Public preimage Git blob | `607e3dd5ce78970f5af027200718d20c350b37a8` |
| Public preimage SHA-256 | `db977a528f7b8c85d8d5198ffe82f1f2caad5ce7da42ecc7fb78255c339d06d0` |
| Candidate postimage Git blob | `8d6414f0e64d6dd02fbacbca357a6edd692aa359` |
| Candidate postimage SHA-256 | `8bd40a8b1d41becc6d21192044883f994516e666951d781a61cc1eda71e65be4` |
| Candidate baseline Source | `6d4fa7b8f9b86079020af125b030a26350cb5bb0` |
| Successor Source | `52a2d9ef57b7c372b9fa149adf062ad7e503b22d` |
| Successor tree | `51258ebd9ad3a654418b6ecb9163947734ef0314` |
| Successor parent | `6d4fa7b8f9b86079020af125b030a26350cb5bb0` |
| Clean source-commit receipt SHA-256 | `7310b496e3d248c41f2035ff0c4aa942c142330f4131fade9b4ba235a422c9a3` |

The fetched public preimage is byte-for-byte identical to the baseline used for the reviewed candidate. Applying only the narrow patch to that public file produces the postimage in Table 1. The mapping imports no unrelated private-composition changes.

The root received the Source52a2 commit/tree and clean readback with seven original commands and fourteen readers closed. This receipt proves source identity and command settlement; it is not a test result. [evidence.json](evidence.json) records that distinction. The successor source capsule is assembled separately; its publication path is not assumed here.

## Observed failure and cleanup change

The Source6d tagged focused command failed at `TestSupervisorController_StaleLivenessDisownDroppedByGeneration` with a `TempDir RemoveAll` diagnostic reporting a nonempty directory. The test can return as soon as the state machine leaves `StRunning`, while its synchronous event handler still has work to complete. The fixture previously requested context cancellation without joining the original event loop before log and directory cleanup.

The new `startLostChildParoleLoop` helper owns a child context and a completion channel for that same loop. It assigns the child context to the controller before starting `Run`. Cleanup cancels that context and waits for the original goroutine to return. Because cleanup callbacks run in reverse registration order, this wait completes before the fixture's earlier event-log and temporary-directory cleanup callbacks.

The helper replaces ten existing loop-start statements. The controller and loop are the same objects returned by the existing fixture. Test assertions, fake spawn/terminate behavior, prequeued event order and all deadlines remain unchanged. No production source is changed.

This removes a demonstrated fixture lifetime gap. The failed run did not capture a stack proving which operation held the directory at that instant; a successful successor rerun is still required to confirm the observed failure is resolved.

## Review and remaining acceptance

Table 2. Evidence boundary.

| Check | Result |
| --- | --- |
| Public preimage fetched at the exact head and Git blob verified | Completed |
| Public/private baseline byte comparison | Identical |
| Narrow patch application to the public preimage | Passed as an artifact check |
| Own and independent review | Completed; no blocking finding in the proposed cleanup scope |
| Committed Source52a2 and tree with clean readback | Received; source-commit receipt in Table 1 |
| Tests on Source52a2 | `NOT_RUN` |
| Source52a2 build, vet, native happy and mandatory full commands | `NOT_RUN` |

The independent review read the whole candidate file and the baseline event-loop dispatch, spawn side effect and backoff timer context. Dispatch is synchronous, and the changed fixture cases use fake spawn callbacks. Joining `Run` therefore waits for the active loop handler and its synchronous state writes.

This helper joins the original loop; it does not separately join every controller timer. Backoff timers receive the cancelled child context. A handler that does not return on cancellation can keep cleanup waiting until the existing outer test timeout. No longer timeout or alternate observer is introduced.

The list of ten affected existing tests and exact file pins is retained in [evidence.json](evidence.json). Later results must name the actual committed successor Source and its original execution receipt; static review and patch applicability do not establish runtime acceptance.

## Preserved history

Source6d remains the immutable revision whose default focused command passed and tagged focused command failed. Its batch receipt remains `FAIL`, with all three original commands and six readers `CLOSED`; it created no new operational HOLD. The separately held older negative-check batch remains `FAIL/HOLD` with separate physical receiving `UNKNOWN`.

No held captures, original owner state or historical verdict were changed or used to infer a successful successor result. The wider A/B acceptance and the earlier unresolved orphan-handler timeout remain outside this one-file cleanup claim. The Source6d record, capsule and previous reports remain preserved.

## References and terms

1. [Exact public preimage](https://github.com/applicate2628/mcp-local-hub/blob/0e8bec723af3f4a52f158e29d2447836ae887374/internal/cli/supervise_lostchild_f6_f2_test.go) and the [proposed file](../../../../internal/cli/supervise_lostchild_f6_f2_test.go).
2. [Immutable Source6d progress](../ab-functional-source6d-20261126/README.md), including its failed focused batch and separate held history.
3. [Machine-readable evidence](evidence.json).

**Source** means the exact committed source revision. **Join** means waiting for the original goroutine to finish. **HOLD** means retained owner state whose required completion evidence has not been accepted. **NOT_RUN** records that no successor execution result has been received here.
