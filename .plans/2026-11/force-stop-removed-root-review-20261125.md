# WL-04: review of the retained removal continuation

## Contents

1. [Scope](#scope)
2. [Reviewed contract](#reviewed-contract)
3. [Evidence and limits](#evidence-and-limits)
4. [Native follow-up received by Root](#native-follow-up-received-by-root)
5. [Test ownership correction](#test-ownership-correction)
6. [Fixture and refusal follow-up](#fixture-and-refusal-follow-up)
7. [Remaining acceptance](#remaining-acceptance)

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

Source commit `7f0155b44ddfddb14067b1b867de0ea9a4d315c7` retained all fifteen frozen source digests. The positive API command observed **13 root PASS and 2 root FAIL**. Both failures were existing workspace callers returning `SERENA_PREPARATION_ASSEMBLY_UNAVAILABLE` before the intended mutation. At that checkpoint the prepared writes were unverified; neither failure was skipped or converted into a passing result. The test ownership correction below explains why the ordinary API constructor cannot supply that preparation and assigns the actual prepared checks to #637.

The separate CLI command passed **13 root tests**, with the one non-Windows-only platform test skipped on Windows. `TestForceStopAuthenticatedWindowsPipeOriginalCompletion` passed all three real-pipe scenarios in 5.97 s: explicit, removed-client-first and removed-reconcile-first. The whole CLI test package took 6.734 s; command compilation/startup are separately recorded, not included in that package duration. This is isolated source/native evidence, not installed-Product acceptance.

Each negative patch was then applied independently. Sending preparation through the old destructive opcode caused one action and the named preparation assertion to fail. Removing the reconcile-original guard caused the named forbidden PID-reacquisition assertion to fail. Each Go command exited 1 as required. Both patches were reversed and the positive source tree was verified clean afterward.

All four controllers and all 22 command owners were received with original root reaped, native Job active count zero, handles closed, capture EOF/closure and original readers joined. Exact receipt and export digests are in `force-stop-removed-native-20261125.json`; existing coordination PR #664 maintains sanitized receipt projections and exact digests in `integration-audit-20261125/native-continuation-evidence.json`. Raw command captures remain private.

The dependent ARA-01 caller in existing PR #671 subsequently passed its focused API, registered-route and owned-stdio fixtures on the composed Source. Two separate causal regressions demonstrated that ignoring original completion or discarding a pending receipt after public SID deletion is detected; exact positive source was restored and the affected roots passed. The full records are `ara01-reviewed-positive-2` and `ara01-causal-negative-1` in coordination evidence. Those results qualify the caller fixtures; real pinned-provider/Product migration remains a separate acceptance boundary.

## Test ownership correction

The historical 13 PASS / 2 FAIL result above is unchanged. Both failed tests constructed an ordinary `NewAPI()` while requesting a live Serena workspace install. That constructor deliberately lacks the preparation assembly, so those tests could not receive a successful prepared write. This continuation corrects the test ownership without changing production admission.

The two API tests now explicitly require `SERENA_PREPARATION_ASSEMBLY_UNAVAILABLE`, an empty committed path, byte-for-byte unchanged prior intent, and no removal capture or reconcile nudge. The generic `TestInstallPlanCore_RemovedTargetKillDependsOnNudgeOutcome` retains the positive cleanup contract: capture both original rows before mutation with the flock available, commit the dropped-row removal before nudge, complete only the dropped capture after successful nudge, skip completion on a hard nudge error, and close both captures exactly once.

Actual prepared Serena add/drop/retention and stop-map checks move to the existing `TestMigrateSerena_AlreadyMigrated_RegistryDrift_RefansOut` in [#637](https://github.com/applicate2628/mcp-local-hub/pull/637). That prepared root remains in the same 14-root private inventory. The reviewed continuation has 18 preparation operations across that inventory, including the separate audit-refusal operation; their native acceptance remains NOT_RUN. Its modeled reap/start does not prove live Supervisor removal and native completion in that same run.

Root and an independent reviewer examined the complete two-owner delta and the affected production ordering. The API file's public preimage at `678b29f4b0d2da95aeb8457262ae91a6a50b80ae` exactly matches the composition preimage. Reviewed resulting Git blob: `73d3a4ce670b4168c3c87abc8dedfe16fa2a0800`.

The focused `serena-api-test-owner-1` run passed all **3 top-level tests and 2 nested cases**, with no skips; package time was 0.350 s. Its source is historical `464fb1079517177fa8b5b3db590ccfe7d1093404` plus the recorded working-file overlays, not a standalone execution of this PR. The API file SHA-256 was `773a1dbd60335d13fe39d3a016886074c63874eb37ee11366ed28fb936aaba22`. Receipt SHA-256: `95a76d084fc25ed486d2f5fe1fe60efa1d594deff55765887c4fcd77bef90d3f`. Root, native Job, handles and original readers were received CLOSED. Exact inputs and outcomes remain in the existing #664 continuation evidence.

## Fixture and refusal follow-up

This continuation also updates the generic removed-row mapping fixture to use retained original captures. Both rows are captured before mutation while the intent flock is available; only the committed removal is completed after the nudge, and both captures close exactly once. Numerical PID/port destructive fallbacks are sealed by the existing fixture. This is DATA mapping coverage, not another native termination claim.

Runtime-image refusal now checks the public AdmissionError classification before any intent, intent-lock, event-log or Scheduler mutation. The wrong-root fixture uses the existing protected temporary directory helper: acquiring its own foreign lock must not create an audit event in the tested state root. Preconditions and postconditions both check absence. The released-token case preserves refusal even after a different owner reacquires the same path. The corrupt owner-sidecar fixture retains the original quiet lease and requires the original JSON syntax cause plus unchanged sidecar bytes.

Table 1 records the latest received integration runs; it does not qualify this owner branch by itself.

| Source and lane | Observed result | Original command time |
| --- | --- | --- |
| `a5b81b0ac3bfe807f09d22d75ddee5d9eb6b8f56`, default focused refusal pair | 2 roots / 2 terminal PASS; no FAIL or SKIP | 46.9153311 s |
| Same source, `test_state_path_env` affected API group | 26 roots / 127 test and subtest terminal PASS; no FAIL or SKIP | 132.6277431 s |

The composed Source tree is `94080d8ec65dec1e20d1327e0839b0ea363406d4`. The retained parent completed in 180.4540177 s with original command/Job/IO closure received. Receipt SHA-256: `bf726c42ebb624380e6e0ba02db035062c74b1c23592f2f9f1076de47c0835f3`; exact command configuration SHA-256: `7f9f1002b3002cfa63782e647af562054d7002ad295a108bee8c2b41c8ff63ae`. The earlier c894 wrong-root failure and 8c refusal failures remain historical FAIL results; they are not relabeled.

The owner projection applies the reviewed c894 residual fixture patch `746135ee8e92186e66c7a20aaf3633c6dfe987f3dadf89e1e55521b884fd0ec5`, then the protected foreign-lock correction `830cd7c84a989f29cf7349623f3c7294abda7af85dae44a3fd87a9fc9f6c9482`. The resulting parsed-install test file SHA-256 is `b3c830c4aacb0de6130c2c3630364a3c3a65f87c614de654d35b91d33244ad36`. Existing owner code and unrelated composition deltas are preserved.

## Remaining acceptance

Receive the expanded prepared drift root in #637 and both unchanged mandatory test gates on the final composition. The last full run remains FAIL on `cc6879be732631bead8365d5e8ce30383173f598` (tree `e14c9466ec83ebb7409fee21b517d176bc7c3915`): build and vet passed, while default and tagged tests failed with cumulative package timeouts and fixture/acceptance failures. Later focused results in Table 1 do not replace those gates or prove the eventual owner head.

A single run combining prepared Serena installation with a live Supervisor nudge and retained native removal completion remains unproven. The original native fixture and its modeled port inventory are not an installed Product or a different-user adversary. Linux/macOS functional acceptance and current-head architecture acceptance remain separate. No new Codex approval is claimed. Keep this existing PR Draft; do not merge.
