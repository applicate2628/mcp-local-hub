# WL-04: review of the retained removal continuation

## Contents

1. [Scope](#scope)
2. [Reviewed contract](#reviewed-contract)
3. [Evidence and limits](#evidence-and-limits)
4. [Remaining acceptance](#remaining-acceptance)

## Scope

Root reviewed the fifteen Go files pinned by `force-stop-removed-frozen-20261125.json`, against the published PR #659 base `9043acae5a778b9b123b195d3c04cb213e5e568d`. Source manifest SHA-256: `49040a6a0e8023d58f6fa879d22de41c4427ced62cfc9996d1e424c2f91622c5`. This review does not transfer approval from the earlier explicit-force-stop change.

## Reviewed contract

Preparation uses one authenticated original Supervisor connection and selects an existing creator record before stop intent or descriptor mutation. It does not terminate. The distinct `force_stop_removed` verb makes an older server reject preparation before its old destructive dispatcher can act. The explicit `force_stop` handler rejects removal phases.

The authoritative removed set is recomputed under the existing intent flock. A descriptor fingerprint prevents a preliminary capture from being borrowed by a new or changed row. Workspace install now receives the original lock release before reconcile or completion; the tests check this ordering by actually trying the flock at the IPC seams.

Completion consumes the original connection, verifies descriptor absence in the controller snapshot and a fresh intent read, and joins the original creator's terminal cell. Reconcile can select this same creator first. The reference cannot be retargeted to a successor, reconstructed from a PID, or redialed after an ambiguous response. Server capture closure releases its reference under the original record lock.

Failure/no-op cleanup, unused captures, old-server refusal, lost response, changed/reappeared descriptor, original result after successor, and reconcile's retained failure are covered by explicit fixtures. A dedicated Windows test uses the actual named pipe, native peer validation, child, Job, terminal receipt and sole waiter. Port inventory and reap timing are explicitly controlled fixture inputs.

## Evidence and limits

Static review found no additional blocker in this scoped continuation. Existing Go 1.27.4 formatting and Windows cross-compilation passed in the implementation agent's run. Cross-compilation does not execute Windows behavior.

The existing server idle timeout remains sixty seconds. If a transaction outlives that connection, cleanup is incomplete and must not be reported as successful; this change does not add a heartbeat or reacquire authority. Ordinary uninstall continues to report incomplete cleanup as warnings under its existing contract. ARA-01 shared-to-private migration must use strict validation before removal and strict completion before publishing its new authority.

The two checked-in negative patches are test inputs, not active source. Their exact `.gitattributes` entries preserve unified-diff context bytes; normal Go whitespace checks remain enabled.

## Remaining acceptance

Run the frozen positive Windows commands and each causal negative patch separately; retain original controller, child, Job, reader and capture completion evidence. Then test the ARA-01 caller against the composed source. Native execution and installed-Product migration acceptance are not claimed by this static review.
