# Liveness task creation and calendar comparison — Source6d

**The liveness change is implemented and reviewed. The Source6d focused tagged command passed its API package and failed its CLI package during test-fixture cleanup; the complete focused batch remains `FAIL`, with its original resources closed.** This update advances the existing scheduler work in PR #660. It does not claim complete acceptance of the broader A scope.

## Contents

1. [Exact source and publication boundary](#exact-source-and-publication-boundary)
2. [Liveness behavior](#liveness-behavior)
3. [Existing tests adapted to the contract](#existing-tests-adapted-to-the-contract)
4. [Verification and remaining acceptance](#verification-and-remaining-acceptance)
5. [History and evidence](#history-and-evidence)
6. [References](#references)
7. [Terms and abbreviations](#terms-and-abbreviations)

## Exact source and publication boundary

**Table 1. Exact identities.**

| Item | Identity |
| --- | --- |
| Composed Source commit | `6d4fa7b8f9b86079020af125b030a26350cb5bb0` |
| Source tree | `277dad42f9e9080d66955fa7469fbc68bc8618e4` |
| Source parent | `5ed87ff759405efa739229b7307b5c2f9579c8fc` |
| Root receipt for the clean Source commit | `f75c328af2d5d072fc538619d41593d63a8b62e8480ba2660481ae8f1eaa99ea` |
| Public #660 preimage head | `25508f8bfe5950daca3df1b0faf338d77913640a` |

The root received the clean native Source commit and its exact tree. That is source-identity evidence; it is not successful test execution. [evidence.json](evidence.json) binds the three changed public files to their exact before/after bytes and records the verification boundary. The Source capsule is maintained by the existing composition work in PR #664 [3].

The changed files are [liveness_task.go](../../../../internal/api/liveness_task.go), [liveness_task_test.go](../../../../internal/api/liveness_task_test.go) and [setup_watchdog_test.go](../../../../internal/cli/setup_watchdog_test.go). The standalone public PR and the full composed Source remain different revisions; tests on either must name the revision they actually exercised.

## Liveness behavior

`ensureLivenessTaskResolved` first exports the existing task definition. Only `ErrTaskNotFound` accompanied by zero exported bytes permits an attempt through the existing `scheduler.XMLCreator.CreateXMLIfAbsent` capability. A backend without that capability returns the established unsupported-operation error; there is no forced-import fallback.

**Table 2. Observations and effects.**

| Observation or operation | Resulting behavior |
| --- | --- |
| Existing definition matches the typed comparator | Return `unchanged`; perform no creation or replacement. |
| Existing definition differs, is empty or is corrupt | Retain the observation and return the typed difference/refusal. |
| Task-not-found includes nonempty bytes | Refuse creation because absence was not established. |
| Create-if-absent reports conflict or another error | Preserve the create error; diagnostic observation cannot convert it into success. Do not retry creation, force import or remove an occupant. |
| Creation is acknowledged, then readback fails or differs | Keep the occupant and preserve the post-create error plus the rollback/refusal result. |
| A receipt is later restored | Confirm the prior absence where possible, or retain/refuse the current occupant. XML equality and a creation receipt do not supply authority to delete a task generation. |

The existing typed comparator now checks calendar `StartBoundary` and `Enabled`. A future start boundary or disabled calendar cannot pass merely because the task's other fields agree. Field-specific diagnostics remain available through the existing difference type. The existing interpretation of default-enabled trigger omissions is reused; no second XML parser or comparator abstraction was added.

Explicit uninstall remains its separate existing operation. The setup fake and assertions now model create-if-absent and preservation on refusal. The existing non-Windows unsupported-operation handling remains in place.

## Existing tests adapted to the contract

**Table 3. Source-level regression coverage; received execution results are scoped below.**

| Contract | Relevant test source |
| --- | --- |
| Calendar start/enabled and precise mismatch fields | `TestLivenessTaskDifferenceNativeDefaultsAndIdentityMatrix`, `TestInstallLivenessTask_ReadbackDifferenceReportsField` |
| Matching task remains unchanged | `TestInstallLivenessTask_Idempotent`, `TestEnsureLivenessTask_SemanticNormalizationIsIdempotent` |
| Drift, empty or corrupt successful exports are retained | `TestEnsureLivenessTask_ObservedDriftIsRetainedAndRefused` |
| Task-not-found plus bytes cannot authorize creation | `TestEnsureLivenessTask_AbsenceRequiresNoObservedBytes` |
| Unsupported creator has no import fallback | `TestEnsureLivenessTask_RequiresXMLCreator` |
| Create errors never retry, delete or become success | `TestEnsureLivenessTask_CreateFailureNeverRetriesOrRemoves` |
| Readback failure and receipt restoration preserve occupants | `TestEnsureLivenessTask_PostCreateReadbackAndRecoveryErrorsSurvive`, `TestEnsureLivenessTask_ReadbackFailureRetainsCreatedTask`, `TestLivenessTaskReceipt_ConfirmsAbsenceButNeverRemovesOccupant` |
| Setup and explicit uninstall retain their distinct behavior | Adapted setup tests and existing `TestUninstallLivenessTask_DeletesByName` / `TestUninstall_LastServer_RemovesLivenessTask` |

The table records assertions present in the source. The received focused command reports the selected API package as passed; this table does not infer execution or acceptance of every wider scenario from that package result.

## Verification and remaining acceptance

The public preimages were fetched at the head in Table 1 and matched the preserved Git blobs. The three postimages are the previously formatted and reviewed liveness candidates. Own-diff and independent review covered the changed contract and existing callers/fakes. Preparation checks establish exact patch applicability and artifact integrity; they do not execute the target behavior.

The Source6d focused default command passed. The focused tagged command passed `internal/api` and failed `internal/cli` at `TestSupervisorController_StaleLivenessDisownDroppedByGeneration`, whose retained diagnostic is a temporary-directory cleanup failure. The batch receipt, received by the root, is `fc4e69767c640a4584e914213fbea35731d992e67975ca05c5a8bcfd076a700a`; all three original commands and six original output readers were received closed. The test failure remains a failure and does not create a new HOLD. Package counts and closed-capture hashes are retained in [evidence.json](evidence.json); raw paths are omitted.

Dedicated orphan regression, `go build ./...`, `go vet ./...`, native happy qualification and both mandatory full suites are `NOT_RUN` on Source6d in this record. Native Task Scheduler normalization of calendar `StartBoundary` is still a qualification limit. Broader handling of unknown XML fields remains a separately recorded residual. Further cleanup corrections or their later source revisions are outside this immutable Source6d record.

The wider A scope remains subject to its own acceptance: legacy cleanup, port stopping, dry-run, setup/watchdog, strict/recovery autostart, `WeeklyRefreshSet`, explicit deletion, autorun OFF and the requested composition. Existing verified facts remain history bound to their original source; this liveness slice does not promote the whole scope to PASS.

## History and evidence

The earlier Sourcee66 history, Source5ed focused failures and later held negative-check batch remain unchanged. The batch's final read-only verification failed after duplicate step names selected existing capture destinations; its original qualification remains failed and separate physical receiving remains `UNKNOWN`. Its failed captures are not evidence for this change and were not read for this account. Detailed B history and the independent Source6d continuation are documented in PR #664 [3].

The retained HOLD applies to its original invocation and dependent resources. It does not turn independently pinned Source bytes into unknown source or forbid a separately admitted corrected Source. No owner release, replay, termination or historical result upgrade is claimed here.

## References

1. [Existing #660 scheduler work](https://github.com/applicate2628/mcp-local-hub/pull/660) and [remaining scheduler-owner scope at the exact public preimage](https://github.com/applicate2628/mcp-local-hub/blob/25508f8bfe5950daca3df1b0faf338d77913640a/docs/qualification/install-scheduler-generation-20261123/remaining-scheduler-owners.md).
2. [Exact earlier composed Source record](https://github.com/applicate2628/mcp-local-hub/blob/10a68908390128a7df9e61193c89bac106661506/docs/modernization/verification/composed-functional-source-20261126/README.md).
3. [Existing #664 composition and verification work](https://github.com/applicate2628/mcp-local-hub/pull/664), including `docs/modernization/verification/composed-functional-source-20261126/source6d-363b/README.md` and the additive A/B Source6d progress record.
4. [Machine-readable evidence](evidence.json) and the three changed source files linked above.

## Terms and abbreviations

- **Source:** exact committed program source revision.
- **PR:** pull request.
- **XML:** the structured task definition exported by Task Scheduler.
- **Create-if-absent:** creation that refuses an occupied task name.
- **HOLD:** retained owner state whose required completion evidence has not been accepted.
