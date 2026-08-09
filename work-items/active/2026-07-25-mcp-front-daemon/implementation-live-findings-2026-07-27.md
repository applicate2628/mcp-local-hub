# PR #588 Live Findings Backend Implementation

Date: 2026-07-27  
Role: backend-engineer and R2 integration owner  
Outcome: PASS for R2-A through R2-D; first R2-E mutation exposed and the backend corrected a helper-only test oracle; full independent R2-E restart remains open  
Revert group: `RG-PR588-V3-R2`

## Accepted inputs

| Artifact | SHA-256 |
| --- | --- |
| `work-items/decisions/2026-07-27-mcp-front-reconcile-v3-row-journal.md` | `42A3FBE24E4E87EA7EF1D5A2E59BEC895DF05C310C740222492E8A8AC3776B62` |
| `work-items/active/2026-07-25-mcp-front-daemon/design.md` | `92E590613B2712D5AB3E2B7BD9235E679BBC368D48D424584DE18D0B8D4E6067` |
| `work-items/active/2026-07-25-mcp-front-daemon/plan-live-findings-2026-07-27.md` | `A9A21F1DCC9E80D0D6CC5FAF67D68C64CC815350721883AB3DCD8C8A3DBD9DE8` |
| `work-items/active/2026-07-25-mcp-front-daemon/qa-live-findings-2026-07-27.md` | `071374571AC2049CBFFA85E798B77E28769DFD3D9890C8942975173291ECA5CC` |
| `.reports/2026-07/report(qa-engineer)-2026-07-27_pr588-live-findings-r2.md` | `4F9EA5C3735E18F6CCC6AC511DD3BB85A9AD6027755AC255D012D3330B91257A` |

## Finding classification

| Bot row | Finding | State | Closure evidence |
| ---: | --- | --- | --- |
| 1 | Preserve legacy LSP entries in rollback snapshot | REAL, fixed | Exact canonical and legacy rows are captured and restored as distinct `(surface, client, language, entry_name)` identities; version-3 group restore is at `internal/api/lsp_client_router_snapshot.go:152`. |
| 2 | Track latest port written by forward retries | REAL, fixed | Each row owns its applied receipt and port; same-call invoked observation alone promotes it in `internal/cli/install_reconcile_mcp_front.go:1372`. |
| 3 | Reject `--check` before dispatching reconcile mode | ALREADY FIXED by local commit `3872ee16` | Protected top-level gate remains in `internal/cli/install.go`; that file is diff-free in R2. |
| 4 | Refuse changed Serena entries during rollback | REAL, fixed | Present baselines use compare-and-swap byte restore and absent baselines use guarded remove in `internal/api/serena_client_reconcile.go:699`. |
| 5 | Serialize complete recovery transaction | ALREADY FIXED by local commit `3872ee16` | Existing operation lock remained unchanged. |
| 6 | Verify an LSP route before rewriting clients | ALREADY FIXED by local commit `3872ee16` | Existing total preflight remained unchanged. |
| 7 | Run session expiration inside route daemon | ALREADY FIXED by local commit `3872ee16` | Protected `internal/cli/route.go` is diff-free in R2. |
| 8 | Mark Serena rows applied only after rewrite succeeds | REAL, fixed | Serena forward uses the wrapper-owned conditional seam; the CLI creates a receipt only from `Invoked=true` plus exact same-call readback. |
| 9 | Reject `--check` before front reconciliation | ALREADY FIXED by local commit `3872ee16` | Same class and gate as row 3. |
| 10 | Refuse merging reports across different front ports | REAL, fixed | Immutable baselines remain row-owned while each successful retry updates only that row's applied receipt. |
| 11 | Serialize complete front-reconcile transaction | ALREADY FIXED by local commit `3872ee16` | Same class and operation lock as row 5. |
| 12 | Keep absent LSP rows pending while client is unreachable | REAL, fixed | Applied or uncertain unreachable ownership remains pending; `TestSnapshotRestore_AppliedOrUncertainAbsentBaselineUnreachableIsPending` is green. |
| 13 | Capture legacy LSP entries removed by forward pass | REAL, fixed | Same class and exact row capture as row 1. |
| 14 | Prevent newly appearing clients from bypassing snapshot | REAL, fixed | Plan application uses only the frozen private client population in `internal/api/lsp_client_router.go:270`. |

## R2 correction outcome

| Gap | Owner-level correction | Guard |
| --- | --- | --- |
| F1 split authorization | `ConditionalEntryMutator`, implemented only by `lockingClient`, now owns exact read, compare, optional backup, durable prepare, one typed mutation, and readback under one client lock at `internal/clients/config_lock.go:231-248`. Serena forward and the version-3 LSP forward/rollback paths call this seam. | `TestMCPFrontV3_ConditionalMutationRejectsInterveningEdit` at `internal/api/lsp_client_router_plan_test.go:80` tables Serena add, LSP canonical add, LSP legacy remove, and LSP rollback add/remove. |
| F2 equality without causation | Durable `prepared` and `precondition-conflict` states never become applied on re-entry from value equality. Only same-call `Invoked=true` can produce a receipt. | `TestMCPFrontV3_NoInvocationStateEqualityNeverCreatesReceipt`. |
| F3 absent Serena inverse | Baseline-present restores pinned bytes through rollback CAS; baseline-absent removes only the still-owned applied entry. | `TestMCPFrontV3_SerenaAbsentBaselineUsesOwnedRemove` at `internal/api/serena_client_reconcile_test.go:1190`. |
| F4 incomplete LSP retry group | Every call rebuilds `(client, language)` from all rows. Legacy terminal/restored/baseline-only state is live-rechecked. Missing, disabled, or non-routable state blocks canonical; unreadable/unreachable state stays retryable pending. | `TestMCPFrontV3_LSPDependencyBarrierSurvivesRetry` at `internal/api/lsp_client_router_snapshot_review_test.go:317` runs two calls and tables missing, unreachable, disabled, non-routable, live-ready, and restored states. |
| A-01 split schema authority | Version 3 persists only `Rows` plus the frozen active plan. Serena pins live only on Serena rows and all row pins are validated before the first inverse at `internal/cli/install_reconcile_mcp_front.go:1082`. Strict decoding rejects compatibility projections and trailing JSON at `internal/cli/install_reconcile_mcp_front.go:1193`. | `TestMCPFrontV3_RowsExclusivelyOwnSerenaPins` at `internal/cli/install_reconcile_mcp_front_v3_test.go:295`. |
| A-02 helper-only durability | Real Serena and LSP mutation owners publish prepared state inside the conditional seam before invocation. Retirement is computed only from the durable re-read at `internal/cli/install_reconcile_mcp_front.go:816-820`. After QA proved the original helper-only retirement test mutation-insensitive, the replacement drives `runRollbackMCPFront`, forces the final durable result to remain pending while the caller's local copy is terminal, observes zero retirement calls, and preserves the active path. | `TestMCPFrontV3_RealMutationSeamsRequireDurablePrepare` at `internal/api/lsp_client_router_plan_test.go:241`; caller-level `TestMCPFrontV3_RollbackCallerRereadsDurableStateBeforeRetirement` at `internal/cli/install_reconcile_mcp_front_v3_test.go:329`. |
| A-03 stale owners | Removed the superseded projection, merge, settlement, and snapshot-key helpers. One row transition owner and one LSP dependency-group owner remain. | Repository search returned `NO_STALE_HELPER_MATCHES` for all six superseded names. |

## Defect-class completeness sweep

| Class participant | Disposition | Evidence |
| --- | --- | --- |
| Serena forward add | Conditional wrapper; durable prepare before invocation; exact readback. | F1 and A-02 real-seam tables. |
| Serena post-add legacy cleanup | Removed as an impossible second write. Every supported adapter has one same-named `serena` entry, so the conditional add is the replacement; the only effective old cleanup window was a concurrent edit. `LegacyPort` and `RemoveLegacy` remain source-compatible no-op fields at `internal/api/serena_client_reconcile.go:291`. | `TestSerenaClientReconcile_LegacyEndpointRemovedOnlyAfterRewriteSuccess` now asserts zero secondary removes for both failed and successful clients. |
| LSP forward canonical add | Conditional wrapper, frozen plan pre-state, durable prepare. | F1/A-02 canonical-add rows. |
| LSP forward legacy remove | Conditional wrapper, ordered after canonical readiness. | F1/A-02 legacy-remove rows. |
| LSP rollback canonical remove/add | Conditional wrapper with effective applied snapshot matcher. | F1/A-02 rollback-remove and rollback-add rows. |
| LSP rollback legacy add | Conditional wrapper, exact raw baseline, then route-readiness verification. | F1/A-02 rollback-add plus F4 group table. |
| Serena present rollback | Existing nine-adapter rollback CAS allowlist; no admission expansion. | Final `TestCAS*` matrix enumerates exactly nine admitted adapters and excludes Windsurf. |
| Serena absent rollback | Existing nine-adapter guarded remove. | F3 positive and changed-replacement controls. |
| Attempt provenance | `Invoked=false` conflicts and surviving prepared rows create no ownership; same-call invoked readback owns promotion. | F2 and existing prepared-settlement guards. |
| Pin authority | One row-owned path/checksum; missing, extra, duplicate, escaped, mismatched, and projection-only pins fail before writes. | A-01 table. |
| Dependency retry | All terminal, restored, baseline-only, pending, failed, and unreachable rows participate on every call. | F4 two-call and baseline-only table. |
| Retirement | Durable re-read only; nonterminal rows keep active record; terminal conflicts may retire but return non-success naming every skipped row identity. The private re-read seam at `internal/cli/install_reconcile_mcp_front.go:1127-1132` exists only to falsify the caller-level gate and resets after the test. | A-02 caller-level retirement test and `conflictMCPFrontRowLabels` at `internal/cli/install_reconcile_mcp_front.go:653`. |
| Protected C3/C5/C6/C7 | No production changes. | `internal/cli/install.go` and `internal/cli/route.go` have zero diff; named exact CLI guards are green. |

The ordinary `AddEntry`/`RemoveEntry` calls still visible in
`applyLSPRouterOps` belong to setup/demigration and the old snapshot helper, not
to the declared `--reconcile-mcp-front` blast radius. The reconcile command's
forward path uses `ApplyLSPRouterClientPlan`; its rollback path uses
`RestoreLSPRouterRecoveryRows`.

## Changed production files

| File | SHA-256 |
| --- | --- |
| `internal/clients/config_lock.go` | `8A7DCB67A8750CCF60E7C2DB1A6ED1ACF0ACFA8E9FDCE949F4A56F2642FF349A` |
| `internal/clients/cas_mutator.go` | `8098CE4A12463F59DE8B3A7D7BDC0DE68AAF04E41E870E7769D6B9874DA19F1D` |
| `internal/api/serena_client_reconcile.go` | `7401E723FB4EF87F68B3E20C589E4B97F261BCC33ECD1197E65E3104409A3D0C` |
| `internal/api/lsp_client_router.go` | `9359AED28C77C690692177D5C9C2837294755C8945C170FD9651FF837CE07746` |
| `internal/api/lsp_client_router_snapshot.go` | `73CC03A737F44AF9AB8B63C2A90430756543E0D82A43BFD6AA8787B905654182` |
| `internal/cli/install_reconcile_mcp_front.go` | `1F44FA1DA99184C299BDC2095EB0A4D4C8990FA17496862237A60A0E4FE59AB1` |

## Changed test files

| File | SHA-256 |
| --- | --- |
| `internal/clients/config_lock_wrapped_test.go` | `2CA328B90D3125925C4B6BEC396559EE083D1CFD6F1B86CB403283CA5C9BECCD` |
| `internal/clients/cas_mutator_test.go` | `92F4921301CC4F92E2ABD2AA193564FC875B6AC6625EC3CB079C5DDB75B8C5C0` |
| `internal/api/serena_client_reconcile_test.go` | `5CAFED3A0C0E300FC9CCF497F8CFFE9D4AE4486CEA5824995ED70BCD109F38AB` |
| `internal/api/lsp_client_router_plan_test.go` | `96C34D47862D2354AA3D762645B929B20480F85980D677FD2242B05AEA918585` |
| `internal/api/lsp_client_router_snapshot_review_test.go` | `9314CFDEF790FEC0650DF00B6007FEEF1FE668105BD633DF80347B7617EE8D80` |
| `internal/cli/install_reconcile_mcp_front_v3_test.go` | `604691857E03DD31217DE981FB3CD52FB63B194BD50E071ABEEB9958D014460E` |
| `internal/cli/install_reconcile_mcp_front_review_test.go` | `4EB4D38D56AD46F908303BC46764333E5FD640CF27A014BAC7FCFC580D8AA8E2` |
| `internal/cli/install_reconcile_mcp_front_pr588_test.go` | `79949720EF802A7B812853AA0398209D182BBD5743AFD744D1139975C3036511` |
| `internal/cli/install_reconcile_mcp_front_pr588_r2_test.go` | `8C2C4D17650C051234F65DC210AA68D7A58246E365D0128241BF65BEA38A0D48` |

## Final scoped verification

Every Go command carried `-tags=test_state_path_env`, set
`MCPHUB_STATE_DIR_OVERRIDE` to its own fresh direct child of `/.scratch/`, and
used an exact narrow regular expression.

| Gate | Final captured output |
| --- | --- |
| R2-A named API owner gate | Both named top-level tests and all five mutation subcases enumerated; `PASS`; `ok mcp-local-hub/internal/api 0.110s`. |
| R2-D full API gate | F1, F3, F4, C9, legacy capture/restore, snapshot, and Serena guards enumerated; `PASS`; `ok mcp-local-hub/internal/api 0.123s`. |
| R2-D exact CLI gate | Version-3, C3/C5/C6/C7, pending, retirement, and route-session guards enumerated; `PASS`; `ok mcp-local-hub/internal/cli 2.291s`. |
| R2-D exact clients gate | Conditional factory matrix and all CAS guards enumerated; `PASS`; `ok mcp-local-hub/internal/clients 0.044s`. |
| R2-E caller-level retirement guard | Restored source: `PASS`; `ok mcp-local-hub/internal/cli 0.032s`. Exact regex: `^TestMCPFrontV3_RollbackCallerRereadsDurableStateBeforeRetirement$`. |
| R2-E exact stale-memory mutation | Changed only `canRetireMCPFrontReconcileReport(durable)` to `canRetireMCPFrontReconcileReport(&persisted)`. The named test failed, exit 1: `retirement attempts=1, want zero while durable report is pending`. The source was restored and the named test returned to `PASS`. |
| Diff hygiene | `git diff --check` exited 0. |
| Protected production files | `git diff --stat -- internal/cli/install.go internal/cli/route.go` produced no output. |
| Stale owners | Search for `newMCPFrontReconcileJournal`, `recordSerenaBackup`, `mergeMCPFrontReconcileReport`, `lspSnapshotKey`, `verifyMCPFrontSerenaNotEdited`, and `currentMCPFrontRowState` returned no matches. |
| Scratch cleanup | Fourteen verified-empty `r2-*` direct children plus five verified-empty `r2e-caller-*` direct children were removed without recursive deletion; no caller-gate scratch directory remains. |

R2-D intentionally did not rerun broad build or vet; those belong to independent
R2-E under the accepted amended plan. The first R2-E controlled mutation
returned `REVISE` because the old retirement test called only the helper
predicate. This correction used only the exact named CLI regex and did not run
the whole CLI package.

## C1 object axis

The preservation object is one exact recovery row identified by
`(surface, client, language, entry_name)`. No test, report, or implementation
claim collapses multiple legacy entries into a client/language projection.

## Safety incident retained from R1

One earlier backend R1 command violated the user's whole-CLI package-scope
prohibition:

`MCPHUB_STATE_DIR_OVERRIDE=.scratch/backend-cli-full-ad392ee8ecdc4299a16ae28c14c75b77; go test -tags=test_state_path_env -count=1 -timeout 10m ./internal/cli/`

It carried the required tag and fresh isolated state path, but it ran the whole
CLI package and launched test-only GUI listeners. It exited 1 after 45.6
seconds. No image-name process kill was used, and its scratch directory was
removed. R2 used only the accepted exact regular expressions.

## Residual risk and next gate

- External editors that ignore the client lock can still race the filesystem
  itself; the conditional seam prevents stale hub authorization but is not an
  operating-system transaction with non-cooperating editors.
- Version-1 and version-2 recovery records remain read-only refused because
  they cannot prove per-row mutation causation.
- Broad build, vet, the complete seven-mutation restart, and independent
  architecture review remain open in R2-E.
- No commit or push was performed.

Next gate: independent `$qa-engineer` restarts all R2-E checks and all seven
controlled mutations from the exact hashes above.

## Terms and Abbreviations

- CAS: compare-and-swap.
- CLI: command-line interface.
- LSP: Language Server Protocol.
- QA: quality assurance.
- R2: second implementation correction round.
- Serena: the Serena MCP route and client entry managed by this reconcile.
