# W10 Independent QA and Working-Result Acceptance

Date: 2026-08-13

Execution role: `$qa-engineer`

Accepted plan: `484883EDBAD02333162C61FAF78B99AA56C402FD64D5955F0E6B65BDDEC82E14`

## Receiving-side echo and immutable binding

W10 independently reran the exact working-result acceptance surface. Product
source, tests, Git index, live services, App Control, virtual disks, installed
CST, fleet, publication and deployment state were read-only. Raw evidence is
under `/.scratch/w10-acceptance-20260813/`.

| Bound input | Fresh result |
|---|---|
| Candidate commit | `bab886092ae0a4148c05f1e057eeedd73731eedf`, exact |
| Parent | `43fee019d46c69522ebe79be952d5f139bd4854f`, exact |
| Git tree | `1850fd616f6585b31727c725b37de236c09d527a`, exact |
| Changed paths | 66, exact |
| Candidate content SHA-256 | `D8DA50B229BF8120A581B91531264103C00D5B6919122C07B85251782050108A`, independently recomputed from path/blob rows |
| `git show --check` | exit 0 |
| Git index | 0 paths before and after QA |
| W7 QA | `1E099E74EDFCF098B042A4D4385EFFC962729FE566A2C5C87CFE835CCEF6C9E1`, exact |
| W8 architecture | `64A1192479246386A0E6CCC7506B15D9925C3DF63C4EDB2093F58686E85D5188`, exact |
| W9 security constraints | `153328F5DF914EA969B8D6E945A604E81CCE1B556B7112083B7D52CD5EBD2EF8`, exact |
| W9 independent security review | `BC20E62A5DF4923F95A97B7B91EC30D704D11F8FBF3EB637B46C858850B0710B`, exact |

CodeGraph MCP was used first. `codegraph status` reported `[OK] Index is up to
date`; one later query named shared-worktree pending files, so no gate claim was
bound to those live entries. All executable gate evidence ran from a clean
`git archive` of the exact candidate.

## Pre-run falsification oracles

Before execution, each criterion was checked for weak or degenerate passing:

| Criterion | Weak result rejected by the QA oracle |
|---|---|
| W10-AC01 | Partial suite, dirty-worktree run, timeout, or any Go failure beyond the exact nine accepted package/test/signature rows. |
| W10-AC02 | Test double, child without exact identity/revocation evidence, fail-closed exit with process/pipe/artifact residue, or synthetic receipt substituted for an observed one. |
| W10-AC03 | ON approximately equals OFF, no-op path that never reaches the boundary, fake/caller settlement, or both control and mutant failing. |
| W10-AC04 | Git-only no-diff while live SCM/App Control/VHDX/CiTool/CST/fleet state drifts; missing probes are gaps, not clean. |
| W10-AC05 | A broad “works” statement that implies target CST, enterprise controls, Line10, publication or deployment. |

Known-good oracles were the accepted contract, immutable clean-HEAD nine-failure
differential, independent native verifier, explicit local process/kernel facts,
and non-degenerate positive plus mutation assertions.

## Fresh command evidence

| Check and verbatim command | Result | Counts / wall time | Raw output |
|---|---|---|---|
| `go test ./... -count=1 -json` | FAIL | 7,745 test PASS, 11 FAIL, 37 SKIP; 40 package PASS, 4 FAIL, 11 SKIP; 363.931 s | `/.scratch/w10-acceptance-20260813/go-test-all.jsonl` |
| isolated flake probe: `go test ./internal/cli -run '^(TestSuperviseCommand_AcquiresLockAndExitsOnSignal|TestSuperviseCommand_SweepsOldBinariesOnStartup)$' -count=5 -v` | PASS but non-clearing | 10/10 PASS, 0 FAIL; wrapper 2.4 s | `/.scratch/w10-acceptance-20260813/go-supervise-repeat.txt` |
| `go vet ./...` | FAIL | compile failure at `internal/api/netsh_no_console_windows_test.go:12:9`; 2.832 s | `/.scratch/w10-acceptance-20260813/go-vet.txt` |
| `uv sync --frozen --extra test --python 3.13` | PASS prerequisite | test environment installed; 1.271 s | `/.scratch/w10-acceptance-20260813/uv-sync-test-extra.txt` |
| `uv run --frozen --python 3.13 pytest -q` | PASS | 635/635 PASS, 0 fail/skip/xfail; 13.931 s; one known Pydantic warning | `/.scratch/w10-acceptance-20260813/python-full-rerun.txt` |
| `uv run --frozen --python 3.13 ruff check .` | PASS | 0 findings; 0.783 s | `/.scratch/w10-acceptance-20260813/ruff-check.txt` |
| `uv run --frozen --python 3.13 ruff format --check .` | PASS | 78 files already formatted; 0.798 s | `/.scratch/w10-acceptance-20260813/ruff-format.txt` |
| `pwsh ./servers/electromagnetics-mcp/native/cst-runtime/verify.ps1 -Unsigned` | PASS | 1 verifier PASS; 0.485 s; image SHA-256 `38D87C50F716E334F89628D4F35604534C3A35BDCF35F378F1D939137BAB89E3` | `/.scratch/w10-acceptance-20260813/native-verify.txt` |
| `git diff --check` | PASS | 0 findings; 0.077 s | `/.scratch/w10-acceptance-20260813/git-diff-check.txt` |
| `python "$env:USERPROFILE\.codex\skills\lead\scripts\check-publication-safety.py"` | PASS, non-authorizing | 0 staged paths; 0.342 s | `/.scratch/w10-acceptance-20260813/publication-safety.txt` |

The first frozen Python attempt collected with no test extra and terminated with
nine import errors; it is recorded as `UNVERIFIED`, not PASS. The explicit
frozen test-extra sync corrected only the scratch environment, after which the
plan command completed 635/635.

### Full-Go failure classification

Nine failures exactly equal the accepted clean-HEAD differential: two routing
capture-before-release guards and seven Windows CLI upgrade/review/staging
guards. They remain unrelated repository defects and are not candidate regressions.

Two additional failures are not accepted:

| Test | Failure | Classification |
|---|---|---|
| `TestSuperviseCommand_AcquiresLockAndExitsOnSignal` | Go `t.TempDir` cleanup: `hardened-parent: The directory is not empty` | `flaky`; same registered harness defect class, must-not-break gate remains failed |
| `TestSuperviseCommand_SweepsOldBinariesOnStartup` | same | `flaky`; same disposition |

The isolated 10/10 green result proves asymmetric behavior but cannot clear a
must-not-break flaky failure. The deterministic `go vet` failure is a separate
exact-candidate closure defect: `newExcludedPortNetshCommand` exists only in the
uncommitted live-worktree `internal/api/port_alloc_excluded_windows.go`, not in
the candidate Git tree. The prior worktree-based W7 vet result did not prove the
immutable candidate.

## Actual local route, revocation and residue

| Probe | Result |
|---|---|
| `go test ./internal/daemon -run '^TestCstDirectFrontendCrossesGoCapabilityAndFixedLocalPipe$' -count=1 -v` | PASS: one real Go `StdioHost` -> exact native frontend -> unique fixed local pipe; 1/1, test 1.90 s, wrapper 2.543 s. The receiver observed the exact 32-byte capability and matched its Go-enrolled digest. |
| `uv run --frozen --python 3.13 pytest -q tests/test_cst_saved_field_w5_worker_pre_main.py::test_native_worker_emits_bound_pre_main_receipt_before_python` | PASS: 1/1 in 0.661 s; exact native worker five-handle pre-main receipt before Python. |
| Direct `mcphub-cst-runtime.exe --role=frontend` without provisioned handles | PASS default-off: exact image SHA above, exit 78, 82.1 ms, zero stdout/stderr. |
| Direct `mcphub-cst-runtime.exe --role=worker` without provisioned handles | PASS default-off: exact image SHA above, exit 78, 14.3 ms, zero stdout/stderr. |
| Post-process probe | PASS for process/pipe child residue: no `go`, test executable or `mcphub-cst-runtime*` survivor; no CST sampler pipe. |
| Post-artifact probe | FAIL: seven new `R:\TEMP\mcphub-cli-test-state-*` / `mcphub-gui-test-state-*` roots remained after the full Go suite. |

The native verifier binds required frontend four-handle and worker five-handle
pre-entry revocation markers. Real transport and pre-main probes passed; the
complete W10 zero-residue condition did not.

## Synthetic authorized sensitivity

The exact snapshot ran 113/113 focused cases across enrollment/frontend,
daemon, broker, worker, containment, native pre-main and MCP result boundaries.
These include a positive non-degenerate four-schema/three-endpoint synthetic
authorized route with exact trace/order and a mutation matrix over capability,
deadline, response, containment and cleanup facts.

An explicit no-fake/no-op selection passed 5/5: fake/default-on settlement
source rejection, no unavailable/literal broker success, worker-owned settlement,
and the MCP result boundary's non-empty exact-size/oversize controls. Control
paths return concrete content/receipts; broken modes return the named denial, so
both-modes-broken cannot satisfy the matrix.

Raw outputs:

- `/.scratch/w10-acceptance-20260813/sensitivity-boundaries-rerun.txt`
- `/.scratch/w10-acceptance-20260813/sensitivity-no-fake-noop.txt`

## Live-state and existing-six preservation

Read-only pre/post inventories recorded three pre-existing CST services, three
pre-existing CST processes, seven App Control policy files, `CiTool.exe`, one
installed CST entry, zero matching attached VHDX images and zero sampler pipes.
Pre/post canonical values were equal for manifest, services, CST processes,
App Control policy paths/hashes, CiTool path/hash, attached CST disk images,
installed CST inventory and pipes. `servers/cst/manifest.yaml` SHA-256 remained
`5B731F7FBD40D104F9F3F6F2FD0AD34B32AF5CAD2EBA40835491DC7182A8A423`.
The candidate changed zero manifest/policy/dependency-pin paths.

Existing-six focused W0 compatibility passed 43/43 under the exact snapshot;
the full 635-test Python suite also passed. No SCM, App Control, VHDX, CiTool,
installed CST, fleet, manifest pin, publication or deployment mutation occurred.

## W10 acceptance reconciliation

| Criterion | Verdict | Exact reason |
|---|---|---|
| W10-AC01 | `failed` | Full Go has two additional flaky failures beyond the accepted nine; exact-snapshot `go vet ./...` does not compile. All other W7 commands terminated as recorded. |
| W10-AC02 | `failed` | Exact real child/pipe/pre-main/default-off/process cleanup passes, but complete zero residue fails because seven test-state roots remain. |
| W10-AC03 | `verified` | 113/113 boundary cases plus 5/5 explicit no-fake/no-op sensitivity pass with non-degenerate controls. |
| W10-AC04 | `verified` | Manifest, SCM/CST services/processes, App Control, VHDX, CiTool and installed CST inventories are equal pre/post; existing-six is 43/43. |
| W10-AC05 | `not-accepted` | The bounded statement is below, but it cannot become an acceptance while AC01/AC02 fail. |

Bounded working-result statement: **code candidate is executable and default-off
locally; enterprise registration, target CST correctness, Line10, publication
and deployment remain open.** This is a scope statement, not a W10 PASS.

## Defects, owner handoff and residual risk

Canonical defect:
`work-items/bugs/2026-08-13-cst-w10-exact-candidate-regression-gate-fails.md`.
The two supervise failures also map to the existing open adjacent defect
`2026-07-25-supervisecommand-tempdir-cleanup-race-lingering-subprocess-handle`.

Correction owners:

- `$backend-engineer`: restore exact Git-tree ownership between
  `internal/api/netsh_no_console_windows_test.go` and the production owner of
  `newExcludedPortNetshCommand`; verify from a clean archive.
- `$qa-engineer` / shared test-harness owner: correct the real
  `TestSuperviseCommand_*` shutdown/temporary-state lifetime and prove zero
  `mcphub-*-test-state-*` residue. Do not hide it with cleanup retries alone.
- `$lead`: create a new immutable candidate after accepted corrections, then
  rerun W8 architecture, W9 security and W10 QA sequentially. The current W8-W10
  chain cannot be promoted.

Target-only Claims 7, 15 and security Claim 17 remain open exactly as before.
No enterprise X1-X6, publication, deployment or live acceptance claim is made.

## Gate

`REVISE`

## Terms and Abbreviations

- App Control: Windows application-control policy enforcement.
- CST: Computer Simulation Technology electromagnetic solver.
- MCP: Model Context Protocol.
- SCM: Windows Service Control Manager.
- VHDX: Hyper-V virtual hard disk format.
- W10: independent local working-result acceptance phase.
