# Scheduler generation refusal and exact error origins

## Contents

- [1. Scope and behavior](#1-scope-and-behavior)
- [2. Error-origin proof](#2-error-origin-proof)
- [3. Exact source and executed checks](#3-exact-source-and-executed-checks)
- [4. Distinguishing failures](#4-distinguishing-failures)
- [5. Own review and remaining delete owners](#5-own-review-and-remaining-delete-owners)
- [6. Dependencies and readiness](#6-dependencies-and-readiness)
- [7. Evidence](#7-evidence)

## 1. Scope and behavior

The Scheduler interface can delete or replace by task name. An exported XML definition identifies content, not a task generation, and a local transaction mutex does not exclude external writers. The weekly refresh transaction and `SchedulerUpgrade` therefore refuse destructive replacement of an observed existing task through `ErrSchedulerReplacementUnowned`. This change chooses the refusal contract; it does not claim to have acquired an exclusive Scheduler namespace.

The refusal is issued before the desired-definition callback or native task mutations. A protected snapshot of the observed definition is retained with write/readback checks; a snapshot is recovery evidence, not authority to delete a later occupant. Even an existing task whose definition already equals the desired XML is refused. When the task was absent, only a fresh `Create` is attempted. The Windows implementation's fresh create has no overwrite flag.

If fresh creation returns an error with uncertain effect, the code records the observed state without deleting, importing, restoring or retrying. A matching XML definition does not turn that error into success. External occupants are preserved even when their XML is identical to the original generation. GUI (graphical user interface) tests retain exact settings rollback and release-error classification, without exposing private snapshot paths in public errors.

## 2. Error-origin proof

The production lock-order checker previously stopped at dynamic `errnoIs` → `Unwrap`. Its error-origin resolver now follows exact local definitions and assignments, indexed producer returns, contextual error parameters, concrete type assertions, wrapper methods, and the reviewed self-`Unwrap` loop. It computes a closed set of possible concrete error types and nil, follows the relevant method bodies, and preserves any reachable settings-lock acquisition.

Unknown external producers, unsupported fields or captures, mutable origins, unindexed methods, unsupported generic instantiations and exhausted analysis budgets still refuse proof. The existing context limit of 16, concrete-type limit of 16 and expression-depth limit of 48 are unchanged. No global terminal exemption or larger architecture allowance is introduced.

For the exact installed Windows SDK (software development kit), four native producer contracts retain either `syscall.Errno`, `windows.NTStatus`, or nil. A narrow `fmt.Errorf` summary permits exactly one `%w`, plain `%q`/`%s` string operands and escaped `%%`. Unknown formatting, custom `Error`/`Format`/`String` behavior, extra operands and other verbs remain rejected. Native standard-chain links retain their closed leaf origins; they are not erased by an imported-function wildcard.

This production summary is bound to Windows Go 1.27.4, unreplaced `golang.org/x/sys v0.47.0`, exact package/type/function identities, signatures and ten full-file SHA-256 fingerprints. SHA-256 is the Secure Hash Algorithm with a 256-bit digest. The installed-source captures include applicable licenses. Own review checked the actual `fmt.Errorf` wrapper and formatter dispatch, Windows string conversion and native status producers, `Errno.Error`, `NTStatus.Error`, message-formatting wrappers and DLL (dynamic-link library) helpers. Those bodies cannot call an application settings-lock callback through the admitted operands. A different SDK remains unqualified; hashes alone are not substituted for this body review.

Once this boundary was resolved, the checker reached the private reader callback. Three exact context-forwarding edges now preserve its argument: `readStateFileBeneathRootNoFollow` parameter 4, `readStateFileBeneathRootNoFollowWithPolicy` parameter 4, and `invokeStateReadBeneathRootStep` parameter 0 (zero-based indices). The public production call passes nil. Fixtures distinguish that nil context, a known callback containing a hidden lock, and a separate unknown caller which must still fail. Both platform signatures were read; only Windows execution is established here.

## 3. Exact source and executed checks

The standalone candidate is based on PR #660 commit `026928c31977788444f6b3e3504324ae21106446`, tree `42069a9870ac9585e425711a84f950d92b353d87`, plus the 17 files recorded in [audit.json](audit.json). The whole base archive was verified against all 3266 Git blobs before materialization. No donor branch was merged or rebased.

The resulting tested inventory contains 3268 files / 57,851,782 bytes, inventory SHA-256 `54f0bf808de010ba3d0bc7123764e6bebab3068e27c04695220bfc797ac49655`. Before/after inventories agree. Documentation added for publication is outside that tested inventory. The publication's 17 Go-file contents must match the manifest exactly.

**Table 1. Standalone checks on the PR's own base.** Test commands use `test_state_path_env`, the race detector, `-count=1` and a three-minute Go test timeout. Counts are terminal events, including subtests, parent tests and package events.

| Check | PASS | FAIL | SKIP | Result |
| --- | ---: | ---: | ---: | --- |
| Error, callback and context-origin fixtures | 127 | 0 | 0 | exit 0 |
| Production weekly lock-order inventory | 2 | 0 | 0 | exit 0 |
| Scheduler and weekly generation contracts | 92 | 0 | 0 | exit 0 |
| Atomic operations and lock-owner inventories | 49 | 0 | 0 | exit 0 |
| GUI handlers | 9 | 0 | 0 | exit 0 |
| Registration callers | 4 | 0 | 0 | exit 0 |
| Standard `go build ./...` | — | — | — | exit 0 |
| Standard `go vet ./...` | — | — | — | exit 0 |

The same production inventory and origin fixtures previously passed on the broader continuation composition (3312 files; inventory `e59a96f4da3ec901af9c497f1af1b52737f9bc95969c41e282fc917fd52bc36f`), with a full standard build and vet. Earlier Scheduler contract checks and their distinguishing control use their own pinned composition and plans. Those receipts are retained separately; the successful standalone checks in Table 1 remove the need to infer branch applicability from a larger composition.

All execution used the existing Windows/amd64 Go 1.27.4 installation and cached dependencies. No installation, native Scheduler mutation or manually dispatched workflow was performed. The source archive transfer is recorded separately from dependency acquisition.

## 4. Distinguishing failures

The initial error-origin run had 124 PASS fixture events but failed `TestWeeklyRefreshLockOrderInventory` at the private reader's unresolved callback. Its two FAIL terminal events (test and package) are retained. The three added callback/context fixtures and successful production run resolve that observed cause without weakening the unknown-caller rule.

The old-generation overlay is an expected negative control. The original code deletes an externally substituted task with identical XML in both weekly and upgrade cases. It produces four FAIL terminal events: the two leaves, their parent and the package. The declared oracle expects this refusal failure. This is evidence that an XML-only comparison did not protect generation identity; it is not a flaky rerun converted to success.

No unexplained FAIL or SKIP events remain in these targeted bundles. Full suites and other platform execution are still outside this claim.

## 5. Own review and remaining delete owners

Own review covered the complete 17-file diff, source inventories, native-error bodies, error and release paths, storage failure, uncertain create outcome, GUI rollback, test oracles and exact raw events. No identified defect remains in the changed weekly/upgrade scope. This is not external approval.

**The repository-wide generation-deletion contract remains open.** The following existing owners were observed outside this patch:

- `install_task_rollback.go`: `verifyInstallTaskDefinitionBeforeDelete` treats XML equality as authorization before a later name-based delete; rollback can delete a different generation with the same XML.
- `install.go`: replacement/pruning and uninstall paths still issue name-based deletes. Their lifecycle and authority must be analyzed separately before claiming the general A2 requirement.
- `liveness_task.go`, `lsp_auto_register.go`, `register.go`, `register_supervisor.go`, and the legacy enable path in `scheduler_mgmt.go` still contain name-based task removals. Each needs an owner-specific authority decision; their mere presence is not proof that every invocation is incorrect.

The concrete remaining defect is an automatic delete guarded only by definition equality: another writer can replace the generation after observation or retain identical XML. Explicit user-directed uninstall has a different authorization question and must not be conflated with rollback cleanup.

## 6. Dependencies and readiness

Keep #660 a draft. External review must target its new exact commit. Full standard and tagged Go suites, native installation/update/cancellation/recovery, POSIX reader execution and Unix/ARM64 execution are not established by Table 1.

PR #669 independently changes `secretsInitRecoveryOwner.locked` to use the existing lock ledger. Its standalone base has no raw-lock census row for that owner. When composing #669 with this branch, remove this branch's now-obsolete raw acquisition row from `supervisor_intent_lock_ownership_test.go`; preserve all other owner counts. This dependency is a composition edit, not a reason to copy either entire source tree or raise an inventory bound.

The previously issued Source `4c3eb47d002ea729fe80370a75506a1d0c9650ac`, its Product and Serena publisher do not contain this new Scheduler/origin patch, #663's architecture changes, or #669. Product admission from that older exact Source cannot be transferred to this branch or the complete integration composition.

The qualification runner retains direct-child identity but explicitly does not claim a whole-process-tree deadline. The earlier Git descendant that continued after timeout remains open. No donor PR is closed by this update.

## 7. Evidence

- [Earlier composition, negative controls and installed SDK captures](scheduler-error-origins-20261123-57604a3a.zip): 1,315,359 bytes; SHA-256 `26501be50a4ba36c136963fd4e68f42984d1df12af49233b079fdee6540b9102`.
- [Standalone PR-base qualification](scheduler-standalone-20261123-57604a3a.zip): 371249 bytes; SHA-256 `52c8ac806a5153927bd7f5c0f583cc7cb91b5ad1942a36097254b6bc398b2de3`. Contains full plans, exact base tree inventory, materialization and execution receipts, raw streams and runner source.
- [Member manifests, file hashes and independent event audit](audit.json).

Raw historical `source_head` fields in older composition plans identify their starting base, not a Git commit containing the tested composition. Their complete physical source tables and inventory hashes determine applicability. This qualification also precedes its publishing commit; readback of the changed Go blobs binds the published code to the tested candidate.
