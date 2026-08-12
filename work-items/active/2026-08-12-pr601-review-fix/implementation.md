# Backend implementation — PR #601 retained-prior rollback admission

## Summary

Gate: **PASS for backend implementation; independent QA and publication remain open.**

The sole unresolved non-outdated PR thread (`PRRT_kwDOSJ-2SM6YYmn-`) was reproduced and fixed. A retained historical Windows console-subsystem binary can now pass the automatic rollback path after successor readiness failure. Ordinary setup, canonicalization, staging, and successor promotion remain GUI-subsystem-only.

## Diagnostic chain

| Claim | Evidence | Result |
| --- | --- | --- |
| The concrete rollback adapter rejected a valid retained CUI prior. | `internal/cli/install_migration_wiring_windows.go:571` at base `20e04f7c` called `AdmitWindowsGUI`. | Verified. |
| Removing only that adapter check would not fix rollback. | `internal/cli/setup.go:143` at base `20e04f7c` made `copyExe` repeat `AdmitWindowsGUI`. | Verified. |
| The PR's removal of all prior admission also admitted malformed priors. | RED `TestV5UpgradeDepsRejectsMalformedPriorBeforeSwap`: observed `error=<nil>`. | Verified. |
| The rollback lifecycle can clean the retained file safely after restored-prior readiness. | `rollbackInstallUpgrade` already waits for the restored prior's readiness before returning its rollback report. | Verified by source and the named regression. |

## Changed surfaces

| File | Change |
| --- | --- |
| `internal/binaryadmission/windows_pe.go` | Added the single `AdmitWindowsUpgradePrior` policy: well-formed PE32/PE32+ with subsystem GUI (2) or CUI (3); malformed and all other subsystems fail closed with existing stable failure IDs. Shared file parsing remains one owner. |
| `internal/binaryadmission/windows_pe_test.go` | Added host-neutral GUI/CUI/unsupported-subsystem coverage for the prior policy. |
| `internal/cli/setup.go` | Parameterized the existing atomic copy owner by a Windows admission function. `copyExe` still binds only `AdmitWindowsGUI`. |
| `internal/cli/install_migration_wiring_windows.go` | Bound prior forward admission and retained rollback copy to `AdmitWindowsUpgradePrior`; staged successor admission remains `AdmitWindowsGUI`. |
| `internal/cli/install_upgrade.go` | Removes the exact retained artifact only after the restored prior supervisor has passed readiness; cleanup failure is explicit while the ready prior remains running, and an alias guard prevents deletion of the canonical path. |
| `internal/cli/windows_pe_admission_test.go` | Added real-filesystem automatic rollback coverage, a malformed-prior no-mutation guard, and a degenerate-input regression proving cleanup cannot delete the canonical path when a dependency returns it as the retained path. |

## Contract statement

No HTTP, database, queue, cache, or remote procedure call contract changed. No endpoint or authorization surface changed.

| Internal surface | Before | After | Consumers |
| --- | --- | --- | --- |
| `copyExe` | Windows source must be GUI subsystem 2. | Unchanged: Windows source must be GUI subsystem 2. | Setup, canonicalize, Windows staging. |
| `AdmitWindowsUpgradePrior` | Absent. | `nil` for valid subsystem 2 or 3; `E_WINDOWS_PE_FORMAT` for malformed/non-regular inputs; `E_WINDOWS_PE_SUBSYSTEM` for other subsystem values. | Windows `RenameAsideBinary` and `RestoreRetainedBinary` only. |
| Automatic post-readiness-failure rollback | Retained CUI prior failed restoration; retained file survived successful GUI rollback. | Valid GUI/CUI prior restores exact bytes, the prior supervisor is restarted and proven ready, then the exact retained file is removed; cleanup failure remains explicit. | `RunInstallUpgrade` callers. |

Timeout/retry statement: no outbound call site was added or changed. Existing bounded successor/prior readiness waits are preserved; no retry was introduced.

## Strict TDD receipt

Named regression guard: `TestInstallUpgradeRestoresConsolePriorAfterSuccessorReadinessFailure`.

| Phase | Expected | Observed |
| --- | --- | --- |
| RED | A CUI prior reaches automatic rollback, restoration fails under the old GUI-only gate, and the test does not observe a successful rollback report. | Failed with `automatic rollback failed restoring retained prior ... E_WINDOWS_PE_SUBSYSTEM ... expected 2, actual 3`. |
| RED malformed guard | A malformed prior is rejected before the swap. | Failed with `error=<nil>, want E_WINDOWS_PE_FORMAT`, proving the PR head had removed too much admission. |
| GREEN | Successor readiness fails once; exact CUI prior bytes return to the canonical path; restored prior readiness succeeds; no `.old-*` or `.*.tmp` remains. | PASS. |
| RED cleanup-alias guard | A retained path that aliases the canonical binary must not be deleted. | Failed because rollback reported success and removed the canonical file. |
| GREEN cleanup-alias guard | Cleanup refuses the alias after prior readiness and the canonical bytes remain intact. | PASS. |

## Verification receipts

| Check | Result |
| --- | --- |
| Focused rollback/admission set, including GUI prior, CUI prior, malformed prior, and ordinary copy callers | PASS (`ok mcp-local-hub/internal/cli`). |
| `go test ./internal/binaryadmission -count=1` | PASS. |
| Adjacent `TestInstallUpgrade|TestV5UpgradeDeps|TestWindowsPEAdmission` set | PASS. |
| `go test -race ./internal/binaryadmission -count=1` | PASS. |
| Focused CLI rollback/admission race set | PASS. |
| `go vet ./internal/binaryadmission ./internal/cli` | PASS. |
| `git diff --check` | PASS. |
| Publication-safety review | PASS: every changed source, test, and work-item path scanned clean individually; the final local commit range produced a version-2 receipt covering 8 files, 1 commit, and complete commit messages. The whole-tree scan reported pre-existing repository fixtures and historical content unrelated to this diff. |
| `go test ./internal/cli -count=1` | NOT VERIFIED: exceeded the 300-second shell budget without output. The exact owned `go.exe test ./internal/cli` and child `cli.test.exe` were identified by command line and settled; no owned process remained. |

## Receiving-side echo

### Diff-invisible invariants

| Invariant | Result | Falsifying evidence |
| --- | --- | --- |
| Ordinary setup and canonicalization remain GUI-only. | Verified. | `TestWindowsPEAdmissionSetup` and `TestWindowsPEAdmissionCanonicalize` reject CUI without destination mutation or temp residue. |
| Windows migration/upgrade staging and the staged successor remain GUI-only. | Verified. | `TestWindowsPEAdmissionMigration`, `TestWindowsPEAdmissionUpgrade`, and the unchanged successor `AdmitWindowsGUI` binding. |
| Historical GUI priors still restore exact bytes. | Verified. | `TestV5UpgradeDepsRetainedRollbackExactBytes`. |
| Historical CUI priors are admitted only in the upgrade-prior role. | Verified. | Named automatic rollback regression passes; ordinary `copyExe` CUI rejection also passes. |
| Malformed priors fail before rename-aside mutation. | Verified. | `TestV5UpgradeDepsRejectsMalformedPriorBeforeSwap`. |
| Successful automatic rollback leaves neither retained nor copy-temp residue. | Verified. | Named regression asserts empty `target.old-*` and `target.*.tmp` globs after exact-byte restoration and prior readiness. |
| Retained cleanup cannot delete the canonical binary if a dependency returns an alias. | Verified. | `TestRollbackInstallUpgradeRefusesToDeleteCanonicalAlias` observes an explicit refusal and unchanged canonical bytes. |

### Defect-class inventory

| Participant | Classification | Evidence |
| --- | --- | --- |
| `copyExe` (`setup.go`) | Not affected; canonical GUI-only behavior preserved. | It binds `AdmitWindowsGUI`; setup admission test passes. |
| `canonicalizeBinaryToTarget` staged and target copies | Not affected; both use canonical `copyExe`. | Caller inventory plus canonicalize admission test. |
| `stageV5UpgradeBinary` copy | Not affected; staged successor remains GUI-only. | Caller inventory plus migration/upgrade admission tests. |
| `v5UpgradeDeps.RenameAsideBinary` prior side | Fixed. | It binds `AdmitWindowsUpgradePrior`; CUI passes and malformed prior fails before mutation. |
| `v5UpgradeDeps.RenameAsideBinary` successor side | Not affected. | It still calls `AdmitWindowsGUI(newSrc)`. |
| `v5UpgradeDeps.RestoreRetainedBinary` | Fixed. | It uses the shared copy owner with `AdmitWindowsUpgradePrior`. |
| `rollbackInstallUpgrade` restore caller | Fixed. | Named regression exercises successor readiness failure through exact restore, restart/readiness, and cleanup. |
| `rollbackInstallUpgrade` retained cleanup | Fixed and fail-safe. | Cleanup occurs only after prior readiness; canonical alias is refused; missing retained artifacts are idempotently accepted. |
| `fakeUpgradeDeps.RestoreRetainedBinary` in orchestrator unit tests | Not affected; test-only double. | No production bytes or admission policy pass through it; adjacent orchestrator tests pass. |
| `UpgradeDeps.RestoreRetainedBinary` interface declaration | Not affected; signature and ownership contract unchanged. | Source diff. |

## Risks and unknowns

- The broad `internal/cli` package suite did not complete inside 300 seconds; independent QA must decide whether to rerun it with a larger bounded budget.
- No live supervisor, installed binary, scheduler task, hub, GitHub thread, or fleet state was mutated.
- The working branch is local only; push, reply, resolution, bot rerun, and merge are explicitly outside this implementation lane.

## Recommended next role

`$qa-engineer` for an independent review of the named regression, adjacent admission invariants, and the broad-suite timeout.
