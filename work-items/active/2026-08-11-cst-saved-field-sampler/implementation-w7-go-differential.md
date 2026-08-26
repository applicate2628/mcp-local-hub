# W7 Full-Go Differential

Execution role: `$qa-engineer` / `$analyst`

Input W7: `05C62691BC8210ECF5B35CB8EC2C56DC782BEB382D175F6FAA3F242C463AAB3A` (`REVISE`)

Input in-scope correction: `5F0E0540B68C04229B04DF0C5A8E49F30E820C0E5801E2E1EBD153BC6799C361` (`PASS`)

Candidate base: `43fee019d46c69522ebe79be952d5f139bd4854f`

## Receiving-side echo

- Diagnose only the nine failures in `/.scratch/w7-20260813/go-test-all.jsonl`.
- Prove whether each failure belongs to the CST candidate or persists without it.
- Preserve source, tests, Git index, live services, CST and policy state.
- Return `PASS` only when all nine failures are either green or proven unrelated baseline/registered defects.

## Falsification criteria

| Criterion | What would it incorrectly let pass? | Required evidence |
|---|---|---|
| Exact reproduction | A package rerun that omits one failing test, shares state between tests, or uses the cache. | Nine separate serial `go test`, exact anchored test name, `-count=1`, terminal exit, raw output. |
| Ownership | A failure called unrelated merely because its package name is outside CST. | CodeGraph MCP owner/call-path result plus exact owning source/test blob comparison. |
| Differential | An unchanged file claim that misses package initialization, generated files or another dirty participant. | Actual symptom reproduction from a clean `git archive HEAD` snapshot containing none of the dirty CST candidate. |
| Registry match | A broad bug record treated as proof of an exact known defect. | Exact test/error-class match in the current bug registry; otherwise record that no exact match exists. |

## MCP ownership evidence

CodeGraph MCP was queried before source inspection. A first broad query reported one briefly pending routing file; the two bounded follow-up queries returned without a staleness banner for every target below.

| Failure group | Test owner | Production owner / call path |
|---|---|---|
| LSP refresh ordering | `internal/api/lsp_routing/resolver_test.go:18` | `TestRefreshCapturesEntriesBeforeRegistryRelease` lexically inspects `WorkspaceResolver.refresh` at `internal/api/lsp_routing/resolver.go:343`; the live capture is `entries := r.reg.LSPEntries()` at line 399. |
| Serena refresh ordering | `internal/api/serena_routing/resolver_test.go:18` | Same lexical test shape against `WorkspaceResolver.refresh` at `internal/api/serena_routing/resolver.go:163`. |
| V5 unreadable intent | `internal/cli/install_migration_wiring_windows_test.go:90` | `TestRunV5UpgradeWindows_UnreadableIntentAbortsUpgrade` calls `runV5UpgradeWindows` at `internal/cli/install_migration_wiring_windows.go:52`; staging and PE admission occur before intent read. |
| MCP-front rollback, three tests | `internal/cli/install_reconcile_mcp_front_review_test.go:472,513,572` | All enter `runRollbackMCPFront` / `runRollbackMCPFrontWithOps` at `internal/cli/install_reconcile_mcp_front.go:1599-1617`; closed pin-binding validation rejects the fixtures before their expected terminal branches. |
| Cross-volume staging | `internal/cli/install_upgrade_cross_volume_windows_test.go:11` | `stageV5UpgradeBinary` at `internal/cli/install_migration_wiring_windows.go:130` calls `copyExe`, whose Windows path applies admission in `internal/cli/setup.go:145`; the ten-byte fixture is not a PE image. |
| Self-replace and dev-build messages | `internal/cli/install_upgrade_test.go:138,469` | Both call `runInstallUpgrade`; `runInstallUpgradePreflightGuards` at `internal/cli/install.go:859` returns current Windows `build.ps1`/PE-admission guidance before mutation. |

## Blob and W0 comparison

The twelve exact test/owner files listed above plus `internal/cli/install.go` and `internal/cli/setup.go` were hashed with `git hash-object` and compared with `git rev-parse HEAD:<path>`: **12/12 equal, 0 different**. Within the queried packages, only `internal/cli/daemon.go` is dirty; CodeGraph did not place it on any of these test call paths.

The W0 inventory `/.scratch/cst-w0-20260813/inventory.json` records the same base `43fee019d46c69522ebe79be952d5f139bd4854f` but inventories only paths already dirty/untracked at W0. None of these twelve clean owners is a W0 inventory row. Equality to the exact W0 base and the full clean-archive reproduction below therefore provide the comparison; absence from the dirty-file inventory is not treated as evidence by itself.

## Candidate reproductions

Every command ran separately and serially from the dirty candidate root. Each selected exactly one test and returned 0 pass, 1 fail, 0 skip.

| Command | Result / wall time | Raw output |
|---|---|---|
| `go test ./internal/api/serena_routing -run '^TestRefreshCapturesEntriesBeforeRegistryRelease$' -count=1 -timeout=10m -v` | FAIL; 0/1; 0.747 s; `capture=-1`, release anchor present | `/.scratch/w7-20260813/diff-serena-refresh.txt` |
| `go test ./internal/api/lsp_routing -run '^TestRefreshCapturesEntriesBeforeRegistryRelease$' -count=1 -timeout=10m -v` | FAIL; 0/1; 0.592 s; `capture=-1`, release anchor present | `/.scratch/w7-20260813/diff-lsp-refresh.txt` |
| `go test ./internal/cli -run '^TestRunV5UpgradeWindows_UnreadableIntentAbortsUpgrade$' -count=1 -timeout=10m -v` | FAIL; 0/1; 4.902 s; GUI-subsystem admission precedes intent read | `/.scratch/w7-20260813/diff-TestRunV5UpgradeWindows_UnreadableIntentAbortsUpgrade.txt` |
| `go test ./internal/cli -run '^TestMCPFrontReview_RollbackRefusesARecordWhosePinnedInputIsGone$' -count=1 -timeout=10m -v` | FAIL; 0/1; 1.332 s; malformed pin binding | `/.scratch/w7-20260813/diff-TestMCPFrontReview_RollbackRefusesARecordWhosePinnedInputIsGone.txt` |
| `go test ./internal/cli -run '^TestMCPFrontReview_RollbackKeepsTheRecordWhileAnyRowIsPending$' -count=1 -timeout=10m -v` | FAIL; 0/1; 1.402 s; unbound pin binding | `/.scratch/w7-20260813/diff-TestMCPFrontReview_RollbackKeepsTheRecordWhileAnyRowIsPending.txt` |
| `go test ./internal/cli -run '^TestMCPFrontReview_RollbackFailsWhenTheRecordCannotBeRetired$' -count=1 -timeout=10m -v` | FAIL; 0/1; 1.274 s; unbound pin binding | `/.scratch/w7-20260813/diff-TestMCPFrontReview_RollbackFailsWhenTheRecordCannotBeRetired.txt` |
| `go test ./internal/cli -run '^TestStageV5UpgradeBinary_StagesBesideCanonicalTarget$' -count=1 -timeout=10m -v` | FAIL; 0/1; 1.056 s; truncated DOS header | `/.scratch/w7-20260813/diff-TestStageV5UpgradeBinary_StagesBesideCanonicalTarget.txt` |
| `go test ./internal/cli -run '^TestRunInstallUpgrade_RefusesSelfReplace$' -count=1 -timeout=10m -v` | FAIL; 0/1; 1.258 s; test expects obsolete `go build` hint | `/.scratch/w7-20260813/diff-TestRunInstallUpgrade_RefusesSelfReplace.txt` |
| `go test ./internal/cli -run '^TestRunInstallUpgrade_RefusesDevBuild$' -count=1 -timeout=10m -v` | FAIL; 0/1; 1.488 s; test expects `build.sh` and `CONSOLE-subsystem` | `/.scratch/w7-20260813/diff-TestRunInstallUpgrade_RefusesDevBuild.txt` |

## Clean-HEAD differential

`git archive --format=tar HEAD` produced an isolated source snapshot with tar SHA-256 `112E695A85C91D1FCA5EC7EEF890C62B26C0CE2CE1C3FCF7FE6E2CF9E5DD5638`. It contains no uncommitted CST candidate, no untracked W0-W7 tests, and no changed `internal/daemon` or `internal/cli/daemon.go` path.

The same nine exact serial commands ran in `/.scratch/w7-20260813/head-43fee019-clean`. All nine failed with the same test line and same semantic error as the dirty candidate:

| Group | Clean HEAD result / wall time | Raw output |
|---|---|---|
| Serena routing | same `capture=-1`; 10.698 s | `/.scratch/w7-20260813/head-clean-__internal_api_serena_routing-TestRefreshCapturesEntriesBeforeRegistryRelease.txt` |
| LSP routing | same `capture=-1`; 0.784 s | `/.scratch/w7-20260813/head-clean-__internal_api_lsp_routing-TestRefreshCapturesEntriesBeforeRegistryRelease.txt` |
| V5 unreadable intent | same GUI-subsystem admission; 12.973 s | `/.scratch/w7-20260813/head-clean-__internal_cli-TestRunV5UpgradeWindows_UnreadableIntentAbortsUpgrade.txt` |
| Rollback missing pin | same malformed pin binding; 1.242 s | `/.scratch/w7-20260813/head-clean-__internal_cli-TestMCPFrontReview_RollbackRefusesARecordWhosePinnedInputIsGone.txt` |
| Rollback pending row | same unbound pin binding; 1.209 s | `/.scratch/w7-20260813/head-clean-__internal_cli-TestMCPFrontReview_RollbackKeepsTheRecordWhileAnyRowIsPending.txt` |
| Rollback record retirement | same unbound pin binding; 1.238 s | `/.scratch/w7-20260813/head-clean-__internal_cli-TestMCPFrontReview_RollbackFailsWhenTheRecordCannotBeRetired.txt` |
| Cross-volume staging | same truncated DOS header; 1.238 s | `/.scratch/w7-20260813/head-clean-__internal_cli-TestStageV5UpgradeBinary_StagesBesideCanonicalTarget.txt` |
| Self-replace hint | same obsolete-hint expectation; 1.410 s | `/.scratch/w7-20260813/head-clean-__internal_cli-TestRunInstallUpgrade_RefusesSelfReplace.txt` |
| Dev-build hints | same two obsolete-hint expectations; 1.267 s | `/.scratch/w7-20260813/head-clean-__internal_cli-TestRunInstallUpgrade_RefusesDevBuild.txt` |

This is the decisive differential: removing the entire dirty CST candidate does not remove or alter any of the nine symptoms.

## Classification and bug registry

| Tests | Classification | Registry |
|---|---|---|
| Two routing source-anchor tests | `test-rot`, proven unrelated baseline | Exact open matches: `2026-08-10-resolver-source-anchor-test-rot` and current-context duplicate `2026-08-12-routing-refresh-tests-use-stale-source-anchor`. |
| V5 unreadable-intent test | unrelated baseline; owner contract/fixture diagnosis remains with `internal/cli` | No exact standalone bug match found. The W7 umbrella bug records the failure set but is not treated as an owner-level disposition. |
| Three MCP-front rollback tests | unrelated baseline; owner contract/fixture diagnosis remains with `internal/cli` | No exact standalone bug match found. |
| Cross-volume staging test | unrelated baseline; invalid fixture versus Windows PE-admission contract | No exact standalone bug match found. |
| Self-replace and dev-build message tests | unrelated baseline; stale Windows operator-copy expectations | No exact standalone bug match found. |

`Unrelated baseline` here means only that these failures are not caused by the CST W0-W7 candidate. It does not approve the CLI production behavior or close the failing repository tests; their owners still must decide whether each is test-rot or a production contract defect.

## Gate

`PASS` for the **W7 nine-test Go differential**.

All nine failures are proven to persist on immutable clean `HEAD`; two are also exact registered test-rot. None is a CST candidate regression. No source, test, Git index, service, policy, CST or publication state was mutated.

## W7 rerun handoff

Rerun the complete W7 suite on the combined candidate. The rerun may classify these exact nine signatures as the established `HEAD` baseline only if package, test name and semantic error remain identical. Any changed signature, additional failure, missing test, timeout or survivor is new `UNVERIFIED`/`REVISE` evidence. Keep the CLI owner defects open; this differential does not make `go test ./...` green and does not by itself satisfy W07-AC01.

## Terms and Abbreviations

- CST: Computer Simulation Technology electromagnetic solver.
- MCP: Model Context Protocol.
- PE: Portable Executable.
- W0/W7: working-result baseline and full-regression phases in the accepted plan.
