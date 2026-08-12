# T13 Independent Full Regression, Static Quality, and Portability QA — Cycle 2

Status: terminal
Execution role: `$qa-engineer`
Accepted plan SHA-256: `3A0CB9AB98447A7A8ED63B2115F68007A9C78EDA77412E94B7A9F6FA90F1E8BD`
Accepted T12 artifact SHA-256: `87D0499ECBF8DC728F9E1E1AC708E9FFC922E3A4B9B13F2959FBF9D806DE3693`
Prior T13 REVISE SHA-256: `299B3916687AFDE17A9597F7CE626F356ED93EAF4896D6E100806516D272D02A`
Accepted correction SHA-256: `7B8220DCC05C5F6CB6E1D1712E43A6EDDF43BB7EAA91418093286CA7EB66C7A3`
Candidate HEAD before/after: `048a30fabc10fa3e6bfc64facc9fb6da6ebe49da`

## Scope and evidence policy

Cycle 2 independently rechecked the two T13 corrections and the full T13 gate without source, test, Git/index, live process, service, hub, fleet, or CST mutation. The allowed write surface was this artifact and `status.md` only on PASS. Raw command streams remain in the execution transcript because no `.scratch/` write was authorized; all decision-driving receipts are preserved below.

CodeGraph exact-impact exploration returned current corrected source with no stale/pending banner. It showed that the correction changes only `internal/api/supervisor_cst_identity_test.go` and `internal/cli/supervise_ensure_alive_test.go`; the production `AcquireSupervisorLock` remains unchanged and still binds `StartedAt` to kernel process creation time. Therefore the fresh cycle-1 Python full-suite result (574/574), saved-field matrix (531/531), and existing-six sample (8/8) remain valid: no Python or production correction surface can affect them. Ruff and format were rerun regardless.

## Fail-closed preflight outcome

| AC | Degenerate result rejected | Absolute cycle-2 oracle |
|---|---|---|
| T13-AC01 | Reusing Python after a production/Python edit | PASS: accepted correction plus current CodeGraph prove two Go `_test.go` fixture edits only; prior fresh Python 574/574, saved-field 531/531, and existing-six 8/8 retained; current Ruff/format green |
| T13-AC02 | Isolated green hiding package-age failure | PASS with exact baseline differential: corrected tests 20x native and WSL; full Go completed; corrected EnsureAlive passed within it; every remaining failure is separately registered pre-existing test rot/cleanup flake; vet green |
| T13-AC03 | Happy path touching live services | PASS: deterministic clock/auth guards and native/WSL matrix used synthetic state only; foreign identity set unchanged |
| T13-AC04 | Two wrong modes agreeing | PASS: correction is Go-test-only; accepted absolute catalogue/schema/error identities and prior fresh existing-six smoke remain unchanged |
| T13-AC05 | Scanning only the prior failing leaf | PASS: installed scanner covered every changed/untracked leaf and returned zero findings |

## Fresh command receipts

| Surface | Verbatim command / result | Exit | Count / wall time |
|---|---|---:|---:|
| Corrected native guards | `go test ./internal/api ./internal/cli -run '^(TestSupervisorStatusAuthorizationKernelBinding|TestEnsureAlive_HeadlessFleet_BootGraceSuppresses|TestEnsureAliveHeadlessFleetSupervisorAge_DomainMatrix)$' -count=20 -timeout 4m` | 0 | 20 repeats per selected test; 2.695 s |
| Corrected WSL guards | same command through Ubuntu WSL2 | 0 | 20 repeats per selected test; 50.496 s |
| Ruff | `uv run --frozen --python 3.13 ruff check .` | 0 | `All checks passed!`; 0.982 s |
| Format | `uv run --frozen --python 3.13 ruff format --check .` | 0 | 69 files already formatted; 0.981 s |
| Publication safety | installed `check-publication-safety.py --path <leaf>` for every changed/untracked leaf | 0 | 140 leaves, zero findings; 52.157 s |
| Full Go | `go test ./...` | 1 | terminal, not timed out; 407.188 s |
| Known cleanup-flake differential | `go test ./internal/cli -run '^TestSuperviseCommand_SkipsOldBinarySweepWhenExecutableUnavailable$' -count=1 -timeout 2m` | 0 | 1/1; 2.657 s |
| Vet | `go vet ./...` | 0 | 6.180 s |
| Diff integrity | `git diff --check` | 0 | no output |

## Full-Go exact differential

| Failure | Current evidence | Classification / gate effect |
|---|---|---|
| `internal/api/lsp_routing.TestRefreshCapturesEntriesBeforeRegistryRelease` | Same `capture=-1 release=12527` as prior T13 and accepted T00 baseline; correction has no package path | Pre-existing test rot, tracked by `work-items/bugs/2026-08-12-routing-refresh-tests-use-stale-source-anchor.md`; non-blocking under the explicit baseline-differential allowance |
| `internal/api/serena_routing.TestRefreshCapturesEntriesBeforeRegistryRelease` | Same `capture=-1 release=9947`; correction has no package path | Same pre-existing tracked test rot; non-blocking |
| `internal/cli.TestSuperviseCommand_SkipsOldBinarySweepWhenExecutableUnavailable` | Full suite failed only during `t.TempDir` cleanup with `directory is not empty`; exact isolated rerun passed 1/1 | Pre-existing Windows full-suite cleanup race explicitly naming this test in `work-items/bugs/2026-07-25-supervisecommand-tempdir-cleanup-race-lingering-subprocess-handle.md`; no owned survivor remained; correction is not affected; non-blocking exact unrelated-baseline differential |
| Corrected `TestEnsureAlive_HeadlessFleet_BootGraceSuppresses` | Passed 20x native, 20x WSL, and passed inside the 407 s full-suite process | Fixed. The previous package-age failure is absent from the full run; this directly falsifies the prior T13 blocker. |

No failure is unexplained, newly affected, or attributed merely from an isolated rerun. The full-run cleanup failure is tied to an existing exact bug record and the failure class (`TempDir RemoveAll`, directory not empty), while the corrected must-not-break guard passed in the same aged process.

## Reused cycle-1 evidence

| Surface | Accepted fresh receipt | Why reuse is sound |
|---|---:|---|
| Frozen Python 3.13 full | 574/574 passed; 25.956 s | Correction is limited to two Go test files; CodeGraph current-source impact confirms no Python/production path |
| Saved-field synthetic matrix | 531/531 passed; 12.667 s | Same reason; no sampler source/test changed in correction |
| Existing-six exact sample | 8/8 passed; 5.123 s | Same reason; tool catalogue, schemas, errors, and route code unchanged |
| T13 exact Go auth/enrollment/launch matrix | native and WSL all selected packages passed | Cycle 2 adds stronger 20-repeat native/WSL execution of both corrected surfaces; no related production code changed |

## State preservation

| State | Before | After | Result |
|---|---:|---:|---|
| HEAD | `048a30fabc10fa3e6bfc64facc9fb6da6ebe49da` | same | unchanged |
| Staged paths | 0 | 0 | unchanged |
| Relevant foreign CST/hub identities | 52 rows; SHA-256 `59A5FF20974D6E15005D8ECB42061F38D116796ABBC481F8490531840BACD2C5` | 52 rows; same SHA-256 | unchanged |
| Owned Go/test survivors | 0 | 0 | none; no cleanup required |

Pre-existing unrelated dirty paths and worktrees were not edited, staged, deleted, or normalized. No test timeout occurred.

## Claims and falsifiers

| Claim | Owner | Falsifying probe |
|---|---|---|
| The publication blocker is fixed | `$qa-engineer` | Re-run the installed scanner over the complete changed/untracked leaf inventory; any finding falsifies the claim |
| The boot-grace package-age blocker is fixed | `$qa-engineer` | Run the 20-repeat guard and the full suite for longer than 45 s; any corrected-test failure falsifies the claim |
| Remaining full-Go failures are unrelated registered baseline defects | Existing bug records; verified by `$qa-engineer` | A failure outside the exact two routing anchors and exact registered TempDir cleanup class, or a candidate-path impact edge, falsifies the classification |
| Candidate/live state was preserved | `$qa-engineer` | HEAD/index drift, foreign identity digest drift, or an owned survivor falsifies the claim |

## Residual risk

- The repository-wide Go command is not numerically green because of three precisely classified pre-existing test defects. T14 must preserve these exact differentials; any additional failure is a new blocker.
- CodeGraph proves the correction impact for indexed Go source but is not a runtime oracle; native/WSL/full-suite execution supplies the behavioral evidence.
- The two routing-anchor and Windows TempDir cleanup bugs remain open outside this CST change surface.

## Terms and Abbreviations

- **CST**: Computer Simulation Technology electromagnetic solver.
- **WSL**: Windows Subsystem for Linux.
- **QA**: quality assurance.
- **RED/GREEN**: failing evidence before correction and passing evidence after correction.
- **PASS**: all T13 criteria are supported directly or by an explicitly permitted, exact unrelated-baseline differential.

Gate: PASS
