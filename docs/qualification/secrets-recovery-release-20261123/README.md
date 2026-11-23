# Secrets recovery: retain lock-release failures

## Contents

- [1. Defect and change](#1-defect-and-change)
- [2. Exact source](#2-exact-source)
- [3. Executed checks](#3-executed-checks)
- [4. Failure interpretation](#4-failure-interpretation)
- [5. Own review](#5-own-review)
- [6. Integration and remaining gates](#6-integration-and-remaining-gates)
- [7. Evidence](#7-evidence)

## 1. Defect and change

`secretsInitRecoveryOwner.locked` discarded `Unlock` errors in a deferred closure. A successful operation could therefore report success while the original lock remained held. An operation failure also concealed the additional release failure. Reporting that error alone is insufficient: the next request must not try to reacquire an unconfirmed lock.

The owner now acquires through the existing `lockLeafLedgered` owner and defers `ReleaseAndJoin` against its named result. An unsuccessful release retains the original lock in the existing ledger and makes the next acquisition fail before entering the operating-system lock. No new lock abstraction or dependency is introduced. A successful release preserves the exact original operation result. This generic owner does not assert that a durable mutation was applied.

`SecretsRecoveryPersistenceError.Unwrap() error` now joins `InitError` and `PersistenceCause` when both are relevant. Its method signature and public `Error()` text are unchanged. This preserves initialization, recovery and release causes through the application programming interface (API) envelope for `errors.Is` and `errors.As`; otherwise a non-nil `InitError` hid the release failure again.

## 2. Exact source

The standalone candidate is based on master commit `75dbc6406123e22468f168b861d5e0622c9d5769`, tree `cb3eec77c23f9400be962d91c9f23834b84f7643`. Only the following two Go files change. Documentation and this evidence archive were added after execution and were not part of the tested source inventory.

**Table 1. Tested source files.** SHA-256 is the Secure Hash Algorithm with a 256-bit digest.

| Path | Bytes | SHA-256 |
| --- | ---: | --- |
| `internal/api/secrets_init_recovery.go` | 22087 | `f40c60a79affd1e9c1cb9b61a93c870eea2edec0899cf4c7766169aa1dfe3caf` |
| `internal/api/secrets_init_recovery_lock_test.go` | 4426 | `db8569cfede6f1a265d1bd5935f62d2d6c010885780be5bc27b57b3d3ce1f4b1` |

The physical standalone inventory contains 3271 files / 50,473,077 bytes, inventory SHA-256 `284bd90dfb9f42dcc43a190f1115d0ae5e5b4bd714d3038e3045c6722869f491`. Both before/after checks agree. The tested source is an exact composition of the pinned master and Table 1, not an already published Git commit. The publication's Go-file bytes must be read back against Table 1.

Execution used the existing Windows/amd64 Go 1.27.4 installation, race detector, existing compiler and cached modules. Tool and plan digests, arguments, environment, process identifiers, timestamps and raw output are retained. No dependency download, installation, native Scheduler operation or workflow dispatch was performed.

## 3. Executed checks

**Table 2. Checks and applicability.** Event counts include subtests, parent tests and package terminal events; they are not counts of independent test functions.

| Source / check | Result | Evidence phase |
| --- | --- | --- |
| Standalone master + Table 1, recovery contract with race detector | 39 PASS, 0 FAIL, 0 SKIP; exit 0 | `b1-standalone-20261123-57604a3a`, `standard-recovery-contract` |
| Same standalone source with `test_state_path_env` build tag and race detector | 39 PASS, 0 FAIL, 0 SKIP; exit 0 | same phase, `tagged-recovery-contract` |
| Standalone `go vet ./internal/api` | exit 0 | same phase, `api-vet` |
| Standalone `go build ./internal/api` | exit 0 | same phase, `api-build` |
| Broader continuation composition, focused recovery and lock-owner checks | 55 PASS, 0 FAIL, 0 SKIP; exit 0 | `b1-contract-20261123-57604a3a` |
| Old lock owner overlaid onto that composition | expected exit 1; 3 release-failure leaves fail | `b1-controls-20261123-57604a3a`, `old-owner` |
| Current lock owner with old envelope `Unwrap` overlaid | expected exit 1; 3 release-failure leaves fail | same phase, `old-envelope` |

The six-case matrix crosses successful operation, original failure, and combined original/recovery failure with successful/failed release. It checks all cause identities, unchanged result on successful release, no false applied-mutation marker, and refusal of reentry after an unconfirmed release. Separate tests cover acquisition failure and releasing during panic unwinding. The test releases its retained fixture handle and removes only the exact fixture ledger entry during cleanup.

The broader composition has 3312 files / 50,921,399 bytes, inventory SHA-256 `e59a96f4da3ec901af9c497f1af1b52737f9bc95969c41e282fc917fd52bc36f`. Its lock census removes the obsolete raw-lock row for this owner. That composition-only census change is not part of the standalone two-file fix because current master has no such row.

## 4. Failure interpretation

The initial broader recovery selection (`b1-continuation-20261123-57604a3a`) produced 54 PASS and 10 FAIL terminal events, with zero SKIP. All 10 FAIL events belong to the three native namespace fixture tests and their parent/package events: `TestSecretsInit_RecoveryNamespaceBoundary` (six leaves), `TestSecretsInit_RecoveryNamespaceRetainedDirectory`, and `TestSecretsInit_RecoveryNamespaceFailurePaths`. Their reported cause is `private namespace fixture binding missing`. These failures remain in the archive; they are not converted into passes or attributed to the lock fix.

A separate subsequent native namespace launcher attempt failed before creating its fixture root with `recovery namespace fixture: Access is denied.` No native namespace tests executed in that attempt. That environment/admission issue remains open in the full-suite qualification owner; it is not covered by the successful focused checks in Table 2.

The two negative controls each produce five FAIL terminal events: three release-failure leaves, their parent, and the package. Those failures are the declared distinguishing oracle. The old owner loses the release cause; the old envelope hides it again behind `InitError`. Both overlays leave the physical candidate unchanged. There are no unexplained FAIL or SKIP events in this bundle.

## 5. Own review

Reviewed the production owner, existing ledger/release semantics, public recovery envelope, its construction sites in `secrets.go`, fixture cleanup, exact source diff and raw events. No defect remains identified in this two-file scope. This is the implementer's review, not external approval.

The runtime behavior intentionally becomes stricter: after a failed unlock, subsequent requests receive the existing unconfirmed-release error instead of risking a blocking reacquisition. Underlying error traversal becomes broader while the `Unwrap() error` signature and user-facing error text stay compatible. No package dependency, shutdown order, analysis budget or unrelated behavior changes.

## 6. Integration and remaining gates

Keep this pull request (PR) a draft. External review of its exact published commit and the required full standard/tagged suites remain pending. Targeted checks are not full API or command-line interface (CLI) qualification, platform execution, Product admission, installation, cancellation or recovery proof.

When composing with PR #660 or its retained Scheduler candidate, remove the obsolete `secretsInitRecoveryOwner.locked` raw-acquisition row from `supervisor_intent_lock_ownership_test.go`; that candidate's census still describes the pre-fix implementation. Preserve all other ownership rows and lifecycle ordering. Do not apply its entire census to current master or raise its limits. This PR has no production-code dependency on #660.

The outer qualification runner enforces a deadline on its original direct child only. It explicitly records `whole_process_tree_deadline_enforced: false`; this work does not close the outstanding descendant-Git timeout case.

## 7. Evidence

- [Raw evidence archive](b1-release-20261123-57604a3a.zip): 805866 bytes; SHA-256 `616c2e2e4920f32b23744349d2c9178e2adefc4ffa4230d509351090b04a9810`.
- [Archive member manifest and independent event audit](audit.json).

The archive retains exact source inputs, source tables, plans, before/after materialization receipts, the unchanged qualification owner, raw standard/tagged events, both failed controls, and the original namespace-fixture failures. Preserve these records when updating the owner; a later successful run does not erase earlier observed failures.
