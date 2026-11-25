# WL-04: review of the retained removal continuation

## Contents

1. [Scope](#scope)
2. [Reviewed contract](#reviewed-contract)
3. [Evidence and limits](#evidence-and-limits)
4. [Native follow-up received by Root](#native-follow-up-received-by-root)
5. [Remaining acceptance](#remaining-acceptance)

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

## Native follow-up received by Root

Source commit `7f0155b44ddfddb14067b1b867de0ea9a4d315c7` retained all fifteen frozen source digests. The positive API command observed **13 root PASS and 2 root FAIL**. Both failures were existing workspace callers returning `SERENA_PREPARATION_ASSEMBLY_UNAVAILABLE` before the intended mutation; they remain unverified until current preparation inputs are issued. They were not skipped or converted into passing fixtures.

The separate CLI command passed **13 root tests**, with the one non-Windows-only platform test skipped on Windows. `TestForceStopAuthenticatedWindowsPipeOriginalCompletion` passed all three real-pipe scenarios in 5.97 s: explicit, removed-client-first and removed-reconcile-first. The whole CLI test package took 6.734 s; command compilation/startup are separately recorded, not included in that package duration. This is isolated source/native evidence, not installed-Product acceptance.

Each negative patch was then applied independently. Sending preparation through the old destructive opcode caused one action and the named preparation assertion to fail. Removing the reconcile-original guard caused the named forbidden PID-reacquisition assertion to fail. Each Go command exited 1 as required. Both patches were reversed and the positive source tree was verified clean afterward.

All four controllers and all 22 command owners were received with original root reaped, native Job active count zero, handles closed, capture EOF/closure and original readers joined. Exact receipt and export digests are in `force-stop-removed-native-20261125.json`; complete redacted command streams are maintained by existing coordination PR #664 in `integration-audit-20261125/native-continuation-evidence.json`.

The dependent ARA-01 caller in existing PR #671 subsequently passed its focused API, registered-route and owned-stdio fixtures on the composed Source. Two separate causal regressions demonstrated that ignoring original completion or discarding a pending receipt after public SID deletion is detected; exact positive source was restored and the affected roots passed. The full records are `ara01-reviewed-positive-2` and `ara01-causal-negative-1` in coordination evidence. Those results qualify the caller fixtures; real pinned-provider/Product migration remains a separate acceptance boundary.

## Remaining acceptance

Retest the two workspace callers with actual current preparation inputs and run the mandatory whole-repository gates. The original native test process and its modeled port inventory are not an installed Product or a different-user adversary. Native results above do not establish full integration, deployment readiness or a new Codex approval on the eventual published head.
