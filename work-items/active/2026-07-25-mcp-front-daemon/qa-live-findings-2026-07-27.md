# PR #588 Live Findings QA Gate

Date: 2026-07-27  
Role: `$qa-engineer`  
R2 gate: **PASS — RETURN(lead)**  
R1 gate: **PASS — accepted historical evidence retained**  
Canonical R2 plan SHA-256:
`A9A21F1DCC9E80D0D6CC5FAF67D68C64CC815350721883AB3DCD8C8A3DBD9DE8`  
R2 implementation handoff SHA-256:
`18035D81C26E0BD1F33FF8BED5B2C5548E5F20A86A65734EB1773573B6D54770`

## R2 Summary

Independent QA restarted from the corrected handoff and proved all seven R2
defect classes with controlled production-only mutations. Every mutation
compiled, reached its exact named guard, and failed at an assertion describing
the reintroduced defect. Each inverse patch restored the production file to
its exact backend handoff SHA-256 before the same guard reran green.

All 15 production/test handoff hashes matched after the mutation sequence. The
exact restored CLI, API, and clients sets produced 64 top-level tests, 69
subtests, 133 PASS markers, zero FAIL markers, and zero skips. Tagged
fresh-state build and vet plus `git diff --check` exited 0. The corrected A-02
retirement guard now drives `runRollbackMCPFront`: the stale in-memory caller
mutation failed with one retirement attempt while the durable report remained
pending, then passed after exact restoration.

## R1 Accepted Evidence

Independent QA reproduced all six open defect classes with controlled
production-only mutations. Every mutation compiled, reached its exact named
guard, and failed on the intended invariant. Each inverse patch restored the
production file to its backend handoff hash before the guard was rerun green.

After all mutations, all 12 production/test handoff hashes matched exactly.
The accepted CLI, API, and clients test regexes passed with 55 top-level tests,
55 subtests, 110 PASS markers, and zero FAIL markers. Tagged fresh-state
`go build ./...`, tagged fresh-state `go vet ./...`, and `git diff --check`
exited 0. No whole-package CLI run and no unscoped `go test ./...` was run.

## Input and change-surface verification

| Input or surface | Expected | Observed | Verdict |
| --- | --- | --- | --- |
| Canonical plan | Dispatch hash ending `...ACB7B` | Exact SHA-256 match | PASS |
| Implementation handoff | Dispatch hash ending `...416` | Exact SHA-256 match | PASS |
| Backend handoff files | 5 production and 7 test hashes from the implementation artifact | All 12 matched; `hash_mismatch_count=0` before mutation and after all six restorations | PASS |
| Production paths | Exactly the five files in the Change-Surface Contract | 5 allowed production files; no other `internal/**` production path | PASS |
| Test paths | Exactly the seven files in the accepted plan | 7 allowed test files; no other `internal/**` test path | PASS |
| Unauthorized source paths | Zero | `unauthorized_source_count=0` before and after QA | PASS |

The source paths reconciled were:

| Kind | Path |
| --- | --- |
| Production | `internal/api/lsp_client_router.go` |
|  | `internal/api/lsp_client_router_snapshot.go` |
|  | `internal/api/serena_client_reconcile.go` |
|  | `internal/cli/install_reconcile_mcp_front.go` |
|  | `internal/clients/cas_mutator.go` |
| Test | `internal/api/lsp_client_router_plan_test.go` |
|  | `internal/api/lsp_client_router_snapshot_review_test.go` |
|  | `internal/api/serena_client_reconcile_test.go` |
|  | `internal/cli/install_reconcile_mcp_front_v3_test.go` |
|  | `internal/cli/install_reconcile_mcp_front_pr588_r2_test.go` |
|  | `internal/cli/install_reconcile_mcp_front_review_test.go` |
|  | `internal/clients/cas_mutator_test.go` |

## Acceptance-criterion preflight

| Criterion | What a weak criterion would let pass | QA criterion actually applied |
| --- | --- | --- |
| Mutation proof | A compile error, timeout, missing test, no-op regex, or unrelated panic | The changed production compiled; the exact named guard ran; its named assertion failed on the mutated invariant |
| Restoration | A visually similar inverse or only one production hash | The inverse used `apply_patch`; all 12 handoff hashes were compared; every mutated production hash matched its pre-mutation/backend hash |
| Regression gate | Package exit 0 with an empty or overly broad test selection | The exact accepted regex ran with `-v`; test and subtest markers were counted; zero FAIL markers were required |
| Build and vet | An untagged run that could resolve production state | Both commands carried `-tags=test_state_path_env` and a fresh direct child under `.scratch` |
| Change surface | A correct file count containing the wrong file | Every changed `internal/**` path was compared by exact path to the accepted allowlist |
| Cleanup | Merely unsetting the environment variable | Every QA state path was resolved below `.scratch`, emptied without cross-shell deletion, removed, and the final direct-child count was zero |

The behavior oracle was the accepted version-3 design plus each test's
absolute assertion. No sibling branch or relative "new output resembles old
output" comparison was used as an oracle.

## Controlled production mutations

Every command below also set
`MCPHUB_STATE_DIR_OVERRIDE=(Resolve-Path <fresh-state-dir>).Path`; each state
directory was a new direct child of `.scratch` and was removed after the run.
The test files, expectations, helpers, build tags, and timeouts were never
mutated.

| Class | Production mutation and exact guard | Expected and observed failing assertion | Exit / wall | Restoration and green rerun | Raw output |
| --- | --- | --- | --- | --- | --- |
| C1 | Omitted `entryName` from `mcpFrontReconcileRowKey`; `go test -tags=test_state_path_env -count=1 -timeout 10m -run '^TestMCPFrontV3_LSPArtifactRoundTripPreservesCanonicalAndMultipleLegacyRows$' ./internal/cli/` | Expected an exact-identity collision. Observed `active generation plan has an empty or duplicate row reference "lsp\x00codex-cli\x00go"` at `install_reconcile_mcp_front_v3_test.go:63` | 1 / 3.434 s | `install_reconcile_mcp_front.go` restored to `C48D9CB2...3CC2B`; same guard exit 0, package 0.033 s, wall 1.040 s | `.scratch/qa-pr588-live-findings/c1-mutated.txt`; `c1-green.txt` |
| C2 | Applied the retry's report port to every prior row receipt in `newMCPFrontV3Journal`; `go test -tags=test_state_path_env -count=1 -timeout 10m -run '^TestMCPFrontV3_PartialCrossPortRetryKeepsPerRowAppliedPorts$' ./internal/cli/` | Expected the no-write row to lose its older per-row port. Observed `Port:9138 ... want preserved port 9137` at `install_reconcile_mcp_front_v3_test.go:172` | 1 / 3.785 s | `install_reconcile_mcp_front.go` restored to `C48D9CB2...3CC2B`; same guard exit 0, package 0.044 s, wall 0.939 s | `.scratch/qa-pr588-live-findings/c2-mutated.txt`; `c2-green.txt` |
| C4 | Replaced `CASRestoreEntryFromBytesForRollback` with ordinary `CASRestoreEntryFromBytes`; `go test -tags=test_state_path_env -count=1 -timeout 10m -run '^TestMCPFrontV3_SerenaCASRestoresLegacyHubBackupAndRefusesConcurrentEdit$' ./internal/api/` | Expected the ordinary polarity to reject the rollback backup. Observed `backup copy of entry is already in hub-managed shape` at `serena_client_reconcile_test.go:1123` | 1 / 5.252 s | `serena_client_reconcile.go` restored to `F4917AF0...2F674`; same guard exit 0, package 0.031 s, wall 0.946 s | `.scratch/qa-pr588-live-findings/c4-mutated.txt`; `c4-green.txt` |
| C8 | Promoted `row.Applied` in `prepareSerenaAttempt`, before the adapter result; `go test -tags=test_state_path_env -count=1 -timeout 10m -run '^TestMCPFrontV3_SerenaAddFailureDoesNotPromoteApplied$' ./internal/cli/` | Expected a failed add to carry false ownership. Observed `failed AddEntry promoted applied ownership` at `install_reconcile_mcp_front_v3_test.go:585` | 1 / 4.043 s | `install_reconcile_mcp_front.go` restored to `C48D9CB2...3CC2B`; same guard exit 0, package 0.040 s, wall 1.019 s | `.scratch/qa-pr588-live-findings/c8-mutated.txt`; `c8-green.txt` |
| C9 | Limited uncertain handling to present baselines and classified unreachable absent baselines as baseline-only; `go test -tags=test_state_path_env -count=1 -timeout 10m -run '^TestSnapshotRestore_AppliedOrUncertainAbsentBaselineUnreachableIsPending$' ./internal/api/` | Expected both owned absent cases to be dropped. Observed `applied-receipt` and `uncertain-write` both returned `baseline-only ... want one pending row` at `lsp_client_router_snapshot_review_test.go:226` | 1 / 5.187 s | `lsp_client_router_snapshot.go` restored to `59037943...A1AB8B`; same guard exit 0, package 0.022 s, wall 0.884 s | `.scratch/qa-pr588-live-findings/c9-mutated.txt`; `c9-green.txt` |
| C10 | Rebuilt the plan from live `plan.opts` inside the applicator, admitting the newly present client; `go test -tags=test_state_path_env -count=1 -timeout 10m -run '^TestMCPFrontV3_ClientAppearingBetweenCaptureAndApplyIsNotMutated$' ./internal/api/` | Expected a post-capture client mutation. Observed `client admitted after capture was mutated: add=1 remove=0` at `lsp_client_router_plan_test.go:33` | 1 / 4.779 s | `lsp_client_router.go` restored to `7F89D47A...C04E53`; same guard exit 0, package 0.025 s, wall 0.906 s | `.scratch/qa-pr588-live-findings/c10-mutated.txt`; `c10-green.txt` |

The C2 plan wording named the effective-receipt resolver, but that resolver has
no active plan/report port input (`internal/cli/install_reconcile_mcp_front.go:286-301`).
The evidence-equivalent mutation therefore reintroduced the same defect at the
row-journal owner that receives the retry port
(`internal/cli/install_reconcile_mcp_front.go:1248-1266`): it replaced each
prior per-row port with the active retry port. The exact C2 guard then failed on
the stale-global-port behavior, not on compilation or a proxy.

## Restored integration and broad gates

All Go commands in this section carried
`-tags=test_state_path_env`, used a distinct fresh
`MCPHUB_STATE_DIR_OVERRIDE` under `.scratch`, and cleaned that directory after
the command.

| Gate | Exact command | Result counts | Exit / wall | Preserved output |
| --- | --- | --- | --- | --- |
| CLI accepted Phase-D set | Exact CLI regex below | 21 top-level, 4 subtests; 25 PASS, 0 FAIL, 0 skipped, 0 xfail | 0 / 3.230 s; package 2.307 s | `.scratch/qa-pr588-live-findings/phaseD-cli.txt` |
| API accepted Phase-D set | Exact API regex below | 24 top-level, 10 subtests; 34 PASS, 0 FAIL, 0 skipped, 0 xfail | 0 / 1.154 s; package 0.141 s | `.scratch/qa-pr588-live-findings/phaseD-api.txt` |
| Clients accepted Phase-D set | Exact clients regex below | 10 top-level, 41 subtests; 51 PASS, 0 FAIL, 0 skipped, 0 xfail | 0 / 0.568 s; package 0.047 s | `.scratch/qa-pr588-live-findings/phaseD-clients.txt` |
| Broad build | `go build -tags=test_state_path_env ./...` | No diagnostics | 0 / 2.259 s | `.scratch/qa-pr588-live-findings/build.txt` |
| Broad vet | `go vet -tags=test_state_path_env ./...` | No diagnostics | 0 / 2.012 s | `.scratch/qa-pr588-live-findings/vet.txt` |
| Diff hygiene | `git diff --check` | No diagnostics | 0 / 0.037 s | `.scratch/qa-pr588-live-findings/diff-check.txt` |

```powershell
go test -v -tags=test_state_path_env -count=1 -timeout 10m -run '^(TestMCPFrontV3_.*|TestMCPFrontR2_(CheckWithReconcileMutatesNothing|SecondInvocationRefusesWhileTheTransactionLockIsHeld|ForwardRefusesWhenOnlyTheSerenaRouteIsLive)|TestMCPFrontReview_(ClientIsNotMutatedWhenItsRecoveryRowCannotBeDurable|RollbackKeepsTheRecordWhileAnyRowIsPending|RollbackFailsWhenTheRecordCannotBeRetired|RetirementClearsTheActiveNamespace)|TestRouteDaemon_Session(StoresAreReachableForExpiry|ExpiryActuallyReclaimsBoundSessions|ExpiryStopsWithContext))$' ./internal/cli/
go test -v -tags=test_state_path_env -count=1 -timeout 10m -run '^(TestMCPFrontV3_.*|TestSnapshotRestore_.*|TestSnapshotLSPRouterClientEntries_CapturesLegacyPerWorkspaceEntries|TestMCPFrontLegacyLSP_ForwardThenRollbackRestoresTheLegacyEntry|TestSerenaClientReconcile_.*|TestSerenaReconcile_PostAttemptHookRunsForSuccessAndFailure)$' ./internal/api/
go test -v -tags=test_state_path_env -count=1 -timeout 10m -run '^(TestCAS.*|TestAllClientsAreLockWrapped)$' ./internal/clients/
```

Basic performance acceptance is advisory because this is not a
performance-sensitive phase. The exact regression sets completed in 3.230 s,
1.154 s, and 0.568 s wall time; build and vet completed in 2.259 s and
2.012 s. No timeout, hang, crash, skip, xfail, or incomplete shard occurred.

The new tests use fixed inputs, injected client maps, and `t.TempDir`-owned
files. Random seed, locale, timezone, filesystem ordering, and uncontrolled
parallel scheduling do not drive their assertions; no ambient-input waiver is
needed.

## Receiving-side echo and class audit

| Class | Enumerated participants | Classification | Evidence |
| --- | --- | --- | --- |
| C1 exact LSP row identity | Canonical row; every legacy row; CLI map key; active-plan reference; compatibility projection; rollback row | fixed | C1 mutation fails the exact artifact guard; restored CLI set and legacy capture/round-trip guards pass |
| C2 per-row retry ownership | Initial receipt; later applied receipt; confirmed-no-write retry; partial retry; rollback expected post-state | fixed | C2 mutation fails on 9138 replacing the preserved 9137 receipt; restored CLI set passes |
| C4 Serena owned rollback | Expected live fingerprint; rollback-bypass polarity; legacy hub backup; concurrent edit; nine admitted CAS adapters; locking forwarder | fixed | C4 mutation fails; Serena owned-restore test and 51 client CAS/lock PASS markers pass |
| C8 Serena attempt lifecycle | Backup capture; durable prepare; successful adapter result; failed adapter result; total post-attempt readback; Applied promotion | fixed | C8 mutation fails; restored Serena failure and total callback guards pass |
| C9 absent/unreachable rollback | Applied absent row; uncertain absent row; baseline-only row; reachable exact state; changed state | fixed | C9 mutation fails both owned absent subcases; restored three-case table passes |
| C10 frozen population/pre-state | Captured present client; absent-then-appearing client; caller-replaced adapter; late pre-state change; canonical failure; legacy removals | fixed | C10 mutation fails on one post-capture add; restored frozen-plan, precondition, and route-preservation tests pass |
| C3 read-only dispatch | `--check` with front reconcile and rollback | not-affected; preserved | `TestMCPFrontR2_CheckWithReconcileMutatesNothing` passes in the exact CLI set |
| C5 operation serialization | Forward/forward and forward/rollback operation lock | not-affected; preserved | `TestMCPFrontR2_SecondInvocationRefusesWhileTheTransactionLockIsHeld` passes |
| C6 total readiness | Serena route plus configured LSP lifecycle route before writes | not-affected; preserved | `TestMCPFrontR2_ForwardRefusesWhenOnlyTheSerenaRouteIsLive` passes |
| C7 route cleanup | Serena stores, LSP store, and context cancellation | not-affected; preserved | All three `TestRouteDaemon_Session*` guards pass |

### Diff-invisible invariants

| Invariant | Verification |
| --- | --- |
| No client mutation occurs before durable prepare | verified by `TestMCPFrontV3_EveryMutationRequiresDurablePrepared` in the restored CLI set |
| A prepared row settles before plan replacement or inverse | verified by both crash-window subtests and the uncertain-plan replacement guard |
| Confirmed no-write preserves the older receipt | verified by C2 mutation plus restored cross-port and confirmed-no-write guards |
| Forward canonical success gates legacy removal | verified by `TestMCPFrontV3_CanonicalFailurePreservesAllLegacyRoutes` |
| Rollback legacy restore gates canonical inverse | verified by `TestMCPFrontV3_LegacyRestoreFailureKeepsCanonicalRoute` |
| Planned population and pre-state remain frozen | verified by C10 mutation, appearing-client guard, and pre-state conflict guard |
| Owned absent rows remain pending while unreachable | verified by C9 mutation and the three-case restored table |
| Version 1 and 2 authorize zero writes | verified by both subtests of `TestMCPFrontV3_V1AndV2ArtifactsRefuseBeforeAnyWrite` |
| Retirement requires durable terminal state | verified by pending/failure retention and retirement guards in the restored CLI set |
| Existing C3/C5/C6/C7 behavior remains intact | verified by their exact named protected guards |

No diff-invisible invariant remains `ASSUMPTION (UNVERIFIED)` within this QA
scope.

### Object-axis record

| Lens | Primary object examined | Adjacent object classes re-aimed at | Decision facts proved | Result and evidence |
| --- | --- | --- | --- | --- |
| Single owner of exact recovery-row identity | Version-3 row-map key | Active-plan row references; compatibility LSP projection; canonical/legacy rollback inputs | `entry_name` must co-vary with client/language on every representation that identifies one mutation/restore row | fixed; C1 mutation fails and restored round-trip/legacy tests pass |
| Single owner of rollback authority | Per-row applied receipt | Forward attempt settlement; confirmed no-write retry; rollback expected state | The latest proven post-state is row-specific; diagnostic plan/report port is not ownership evidence | fixed; C2 mutation fails and restored retry/settlement tests pass |

## Safety and cleanup

- QA did not run an unscoped `go test ./...`.
- QA did not run the whole `./internal/cli/` package without the accepted
  exact `-run` regex.
- Every API/CLI command carried `-tags=test_state_path_env` and a fresh
  `MCPHUB_STATE_DIR_OVERRIDE`.
- QA did not launch the GUI, tray, supervisor, scheduler, or a production
  daemon; no process was killed.
- QA did not use checkout, reset, stash, a second worktree, push, or commit.
- Each QA state path was resolved below `.scratch` before cleanup. Final
  direct-child `.scratch/qa-state-*` count: `0`.
- `.scratch/qa-pr588-live-findings/` is intentionally retained as the
  required raw-evidence directory; it is not a state override directory.

The backend safety incident remains part of this handoff: before QA, the
implementation owner ran the whole tagged CLI package with a fresh redirected
state directory. It did not reach the production state path, but it violated
the user's package-scope rule and launched test GUI listeners at
`127.0.0.1:34675` and `127.0.0.1:34684`; that command exited 1 after
45.6 seconds. QA did not repeat that command or that behavior
(`implementation-live-findings-2026-07-27.md:114-127`).

## Residual risk and gate

- The accepted design retains advisory, point-in-time LSP comparisons for
  non-lock-honoring external editors. The deterministic in-repository drift
  classes are covered; kernel-enforced cross-process compare-and-set is not in
  scope.
- Client config and recovery journal writes remain separate files. Durable
  prepare plus settlement makes their interruption window recoverable and
  fail-closed, but not cross-file atomic.
- Version-1 and version-2 recovery records are intentionally refused
  read-only and require manual recovery.
- Independent architecture/review gates and the lead's final 14-row
  reconciliation remain downstream obligations; QA does not approve a push.

**R1 gate: PASS — accepted historical evidence retained.**

## R2 Independent QA — Final Gate

### Accepted handoff verification

| Input or surface | Expected SHA-256 / state | Observed | Verdict |
| --- | --- | --- | --- |
| R2 plan | `A9A21F1D...B9DE8` | Exact SHA-256 match | PASS |
| Corrected R2 implementation | `18035D81...54770` | Exact SHA-256 match | PASS |
| Version-3 row-journal decision | `42A3FBE2...76B62` | Exact SHA-256 match | PASS |
| Corrected backend R2 report | `670D09F7...AEE1C` | Exact SHA-256 match | PASS |
| Backend source and test files | 6 production and 9 test hashes from the corrected implementation handoff | All 15 matched before mutation and after all restorations; `hash_mismatch_count=0` | PASS |
| Changed `internal/**` paths | Exact R2 allowlist | 6 production and 9 test paths; `unauthorized_count=0`; no missing allowlist path | PASS |
| Protected surfaces | No change in `internal/cli/install.go` or `internal/cli/route.go` | `git diff --stat --` produced no output, exit 0 | PASS |

### R2 acceptance-criterion preflight

| Criterion | What a weak criterion would let pass | QA criterion applied |
| --- | --- | --- |
| R2E-AC1 mutation proof | Compile failure, missing test, empty regex, timeout, unrelated panic, or a helper-only proxy | The production mutation compiled and the exact real-seam guard failed at an assertion naming the reintroduced behavior |
| R2E-AC2 restoration | Similar-looking inverse or restoration of only the last file | Immediate inverse `apply_patch`, exact mutated-file hash equality, then the same exact guard green |
| R2E-AC3 regression scope | Package exit 0 with uncounted or overbroad selection | Exact accepted regex, verbose enumeration, explicit top/subtest/PASS/FAIL/skip counts, plus all six historical R1 guards |
| R2E-AC4 build and vet | Untagged broad command that could resolve production state | Both broad commands carried `-tags=test_state_path_env` and distinct fresh state overrides |
| R2E-AC5 closure evidence | A prose-only PASS without source reconciliation | All 15 hashes, exact allowlist, protected files, stale-owner search with positive control, leak scan, diff check, and state cleanup recorded |

### Seven controlled production mutations

Every Go command set `MCPHUB_STATE_DIR_OVERRIDE` to a distinct fresh direct
child of `.scratch` and carried `-tags=test_state_path_env`. Test source,
expected values, regexes, tags, timeouts, and runners were unchanged.

| Class | Controlled production defect and verbatim command | Named failure evidence | Exact restoration and green rerun | Raw output |
| --- | --- | --- | --- | --- |
| F1 | Precomputed LSP authorization with a split `GetEntry` before the conditional seam; `go test -v -tags=test_state_path_env -count=1 -timeout 10m -run '^TestMCPFrontV3_ConditionalMutationRejectsInterveningEdit$' ./internal/api/` | Exit 1; canonical add failed at `lsp_client_router_plan_test.go:135`, legacy remove at `:171`; 1 top + 5 subtests, 3 PASS and 3 FAIL markers; wall 4.638 s | `lsp_client_router.go` restored to `9359AED2...07746`; same guard exit 0, 6 PASS/0 FAIL; wall 0.855 s | `.scratch/qa-pr588-r2-restart/f1-mutated.txt`; `f1-restored.txt` |
| F2 | Equality-promoted a no-invocation precondition conflict; `go test -v -tags=test_state_path_env -count=1 -timeout 10m -run '^TestMCPFrontV3_NoInvocationStateEqualityNeverCreatesReceipt$' ./internal/cli/` | Exit 1 at `install_reconcile_mcp_front_v3_test.go:163`: `no-invocation equality created authority`; 1 FAIL; wall 3.399 s | `install_reconcile_mcp_front.go` restored to `1F44FA1D...59AB1`; same guard exit 0, 1 PASS; wall 0.906 s | `.scratch/qa-pr588-r2-restart/f2-mutated.txt`; `f2-restored.txt` |
| F3 | Replaced absent-baseline owned remove with ordinary `RemoveEntry`; `go test -v -tags=test_state_path_env -count=1 -timeout 10m -run '^TestMCPFrontV3_SerenaAbsentBaselineUsesOwnedRemove$' ./internal/api/` | Exit 1 at `serena_client_reconcile_test.go:1225`: operator replacement reported `restored` instead of conflict; wall 4.739 s | `serena_client_reconcile.go` restored to `7401E723...A3D0C`; same guard exit 0, 1 PASS; wall 0.883 s | `.scratch/qa-pr588-r2-restart/f3-mutated.txt`; `f3-restored.txt` |
| F4 | Filtered terminal legacy rows before dependency-group reconstruction; `go test -v -tags=test_state_path_env -count=1 -timeout 10m -run '^TestMCPFrontV3_LSPDependencyBarrierSurvivesRetry$' ./internal/api/` | Exit 1 at `lsp_client_router_snapshot_review_test.go:380` plus restored-row assertions; 1 top + 8 subtests, 5 PASS and 4 FAIL; wall 5.124 s | `lsp_client_router_snapshot.go` restored to `73CC03A7...54182`; same guard exit 0, 9 PASS; wall 0.924 s | `.scratch/qa-pr588-r2-restart/f4-mutated.txt`; `f4-restored.txt` |
| A-01 | Added and required a top-level Serena pin projection instead of row authority; `go test -v -tags=test_state_path_env -count=1 -timeout 10m -run '^TestMCPFrontV3_RowsExclusivelyOwnSerenaPins$' ./internal/cli/` | Exit 1 at `install_reconcile_mcp_front_v3_test.go:325`: valid row-owned pin rejected for missing top-level projection; wall 3.510 s | `install_reconcile_mcp_front.go` restored to `1F44FA1D...59AB1`; same guard exit 0, 1 PASS; wall 0.968 s | `.scratch/qa-pr588-r2-restart/a01-mutated.txt`; `a01-restored.txt` |
| A-02 mutation order | Invoked the real Serena `AddEntry` inside `BeforeMutation` before durable prepare succeeded; `go test -v -tags=test_state_path_env -count=1 -timeout 10m -run '^TestMCPFrontV3_RealMutationSeamsRequireDurablePrepare$' ./internal/api/` | Exit 1 at `lsp_client_router_plan_test.go:260`: `Serena addCalls=1, want 0`; 1 top + 5 subtests, 4 PASS and 2 FAIL; wall 5.073 s | `serena_client_reconcile.go` restored to `7401E723...A3D0C`; same guard exit 0, 6 PASS; wall 0.923 s | `.scratch/qa-pr588-r2-restart/a02-order-mutated.txt`; `a02-order-restored.txt` |
| A-02 retirement | Replaced the caller's durable retirement input with stale `&persisted`; `go test -v -tags=test_state_path_env -count=1 -timeout 10m -run '^TestMCPFrontV3_RollbackCallerRereadsDurableStateBeforeRetirement$' ./internal/cli/` | Exit 1 at `install_reconcile_mcp_front_v3_test.go:364`: `retirement attempts=1, want zero while durable report is pending`; wall 1.002 s | `install_reconcile_mcp_front.go` restored to `1F44FA1D...59AB1`; same guard exit 0, 1 PASS; wall 1.020 s | `.scratch/qa-pr588-r2-restart/a02-retirement-mutated.txt`; `a02-retirement-restored.txt` |

### Restored integration, build, and static gates

| Gate | Verbatim command | Result counts | Exit / wall | Raw output |
| --- | --- | --- | --- | --- |
| CLI | `go test -v -tags=test_state_path_env -count=1 -timeout 10m -run '^(TestMCPFrontV3_.*\|TestMCPFrontR2_(CheckWithReconcileMutatesNothing\|SecondInvocationRefusesWhileTheTransactionLockIsHeld\|ForwardRefusesWhenOnlyTheSerenaRouteIsLive)\|TestMCPFrontReview_(ClientIsNotMutatedWhenItsRecoveryRowCannotBeDurable\|RollbackKeepsTheRecordWhileAnyRowIsPending\|RollbackFailsWhenTheRecordCannotBeRetired\|RetirementClearsTheActiveNamespace)\|TestRouteDaemon_Session(StoresAreReachableForExpiry\|ExpiryActuallyReclaimsBoundSessions\|ExpiryStopsWithContext))$' ./internal/cli/` | 24 top, 0 sub; 24 PASS, 0 FAIL, 0 skip | 0 / 3.335 s; package 2.295 s | `.scratch/qa-pr588-r2-restart/restored-cli.txt` |
| API | `go test -v -tags=test_state_path_env -count=1 -timeout 10m -run '^(TestMCPFrontV3_.*\|TestSnapshotRestore_.*\|TestSnapshotLSPRouterClientEntries_CapturesLegacyPerWorkspaceEntries\|TestMCPFrontLegacyLSP_ForwardThenRollbackRestoresTheLegacyEntry\|TestSerenaClientReconcile_.*\|TestSerenaReconcile_.*)$' ./internal/api/` | 28 top, 28 sub; 56 PASS, 0 FAIL, 0 skip | 0 / 0.995 s; package 0.133 s | `.scratch/qa-pr588-r2-restart/restored-api.txt` |
| Clients | `go test -v -tags=test_state_path_env -count=1 -timeout 10m -run '^(Test(CAS.*\|AllClientsAreLockWrapped\|MCPFrontV3_Conditional.*))$' ./internal/clients/` | 12 top, 41 sub; 53 PASS, 0 FAIL, 0 skip | 0 / 0.549 s; package 0.047 s | `.scratch/qa-pr588-r2-restart/restored-clients.txt` |
| Build | `go build -tags=test_state_path_env ./...` | No diagnostics | 0 / 2.911 s | `.scratch/qa-pr588-r2-restart/build.txt` |
| Vet | `go vet -tags=test_state_path_env ./...` | No diagnostics | 0 / 2.981 s | `.scratch/qa-pr588-r2-restart/vet.txt` |
| Diff hygiene | `git diff --check` | No diagnostics | 0 | `.scratch/qa-pr588-r2-restart/surface-and-diff.txt` |

Basic performance acceptance is advisory because R2-E is not a
performance-sensitive phase. No timeout, hang, crash, skip, xfail, or
incomplete shard occurred. The three exact restored suites completed in
3.335 s, 0.995 s, and 0.549 s wall time.

### R2 receiving-side echo and class sweep

| Defect-class participant | Classification | Evidence |
| --- | --- | --- |
| Serena forward add | fixed | F1 Serena race remains green; A-02 order mutation fails before durable prepare |
| LSP forward canonical add | fixed | F1 stale split fails the canonical assertion; restored conditional table passes |
| LSP forward legacy remove | fixed | F1 stale split fails the legacy assertion; restored conditional table passes |
| LSP rollback legacy add | fixed | F1/A-02 tables preserve conditional and prepare ordering |
| LSP rollback canonical add/remove | fixed | F1/A-02 rollback rows pass; dependency barrier preserves canonical route |
| Serena present rollback | fixed | Restored `TestMCPFrontV3_SerenaCASRestoresLegacyHubBackupAndRefusesConcurrentEdit` passes |
| Serena absent rollback | fixed | F3 mutation removes the operator replacement and fails; restored owned remove passes |
| Attempt provenance | fixed | F2 equality mutation creates authority and fails; restored no-invocation and crash-window guards pass |
| Pin authority | fixed | A-01 top-level projection mutation fails; row-owned pin verifier and artifact-shape guard pass |
| LSP dependency retry | fixed | F4 mutation fails the second call and restored states; all eight restored subcases pass |
| Durable retirement re-read | fixed | Corrected caller-level A-02 mutation reaches one forbidden retirement attempt; restored caller records zero |
| Active-namespace retirement | fixed | Corrected A-02 caller test preserves the active path; existing retirement-clear/failure guards pass |
| Protected C3/C5/C6/C7 behavior | not-affected | `install.go` and `route.go` diff-free; all named exact CLI guards pass |
| Historical R1 C1/C2/C4/C8/C9/C10 guards | not-affected | All six are included and pass in the exact restored CLI/API sets |
| Superseded helper owners | not-affected; absent | Six-name `rg` search returns exit 1; positive control for `mcpFrontReadReportForRetirementFn` returns six matches and exit 0 |

Receiving-side echo: QA authenticated the corrected backend claim that
`TestMCPFrontV3_RollbackCallerRereadsDurableStateBeforeRetirement` now drives
the rollback caller. The exact stale-memory mutation failed at the retirement
call counter and the active report remained present; the former helper-only
coverage gap is closed.

### Safety, leak scan, and cleanup

- No unscoped `go test ./...` and no whole-package CLI test was run.
- Every API/CLI Go command carried `-tags=test_state_path_env` and a distinct
  fresh `MCPHUB_STATE_DIR_OVERRIDE`; build and vet used the same isolation.
- No GUI, tray, supervisor, scheduler, daemon, or process-kill action was
  used. No checkout, reset, stash, worktree creation, commit, or push was
  performed.
- All 19 R2 restart state directories were verified empty direct children of
  `.scratch`, removed individually, and re-counted. Final
  `.scratch/qa-r2-restart-state-*` count: `0`.
- The official publication-safety scanner ran in explicit path mode against
  each of the 15 changed source/test files: 15 examined, 15 clean, 0 failed
  scans. This is leak evidence for the admitted changed-file set, not staged
  publication approval.
- Changed-source allowlist reconciliation found 15 exact paths, zero
  unauthorized paths, and zero missing paths. Protected-file output,
  `git diff --check`, and final 15-file hash reconciliation were clean.
- `.scratch/qa-pr588-r2-restart/` is intentionally retained as the required
  raw-evidence directory. The earlier `.scratch/qa-pr588-r2/` preserves the
  initial helper-only-test REVISE evidence.

**R2 gate: PASS — RETURN(lead).** Independent architecture gates, decision
acceptance, final reconciliation, leak-checked local commit, and human review
remain downstream. QA performed no commit or push.

## Terms and Abbreviations

- A-02: the atomic journaling and retirement-order architecture claim.
- API: Application Programming Interface.
- CAS: compare-and-set under the repository-owned client-config lock.
- CLI: Command-Line Interface.
- LSP: Language Server Protocol.
- QA: Quality Assurance.
- PASS: the scoped verification gate completed with no open defect.
- R2: the second implementation and verification correction round.
