# Independent Core QA — CST Saved-Field Point Sampler

## Scope and immutable subject

- Execution role: `$qa-engineer`.
- Candidate C: `14a9b6b4cb9fc1e7248bd3b782b9e00d499181df`.
- Scope: independently verify Phases 0–5 and `P6-AC3` without CST Studio Suite, Line10, a native provider, live fleet operations, deployment, publication, or source/test edits.
- Raw receipt root: `/.scratch/work-items/2026-08-11-cst-saved-field-sampler/qa-core/`.
- Gate values: `PASS`, `REVISE`, or `BLOCKED`.

## Pre-run criterion pressure test

This matrix was written before the first test, lint, format, or scan command. “What would this criterion let pass?” identifies the degenerate outcome that a relative or incomplete oracle could admit; the final column fixes the acceptance oracle to a literal property, the accepted baseline, or an independently falsifying test.

| Criterion | What would this criterion let pass? | Non-degenerate oracle used for QA |
|---|---|---|
| P0-AC1 | Running only candidate tests could hide a baseline failure or an inventory-only incompatibility. | Run the exact baseline compatibility command from the accepted baseline tree and require its literal pass count and exit `0`. |
| P0-AC2 | Collection alone could pass while named probes are empty, skipped, or unrelated to the absent feature. | Inventory exact collected nodes and preserve the pre-change RED showing absence of the sampler contract/registration rather than fixture, syntax, import, or environment failure. |
| P0-AC3 | A snapshot that covers fewer/more names or only compares sibling outputs could miss an altered old schema/body. | Require exactly six old tool names, three HFSS plus three CST, and literal frozen identities for `_runner`, `cst_solve`, `cst_export_mesh`, and `cst_export_results`. |
| P0-AC4 | A test-only claim could conceal production, dependency, documentation, or manifest edits in the Phase-0 witness. | Inspect the baseline-to-Phase-0 witness or accepted RED receipt; any non-test change is failure. |
| P1-AC1 | A permissive model could accept unknown keys, coercions, `allow_solve=0`, non-finite values, or 0/257 points. | Require closed strict models and explicit boundary falsifiers for every enum/bound plus exact point cardinality `1..256`. |
| P1-AC2 | Resolver agreement on one candidate/order could hide first-match behavior, tolerance errors, or ignored identity selectors. | Require permutation invariance and literal zero/one/many, exact-selector, tolerance-boundary, hash, and adaptive-pass outcomes with stable failure IDs. |
| P1-AC3 | ON≈OFF or numeric equality could pass reordered components, partial zeros, or invented physical meaning. | Require literal order `ReX, ReY, ReZ, ImX, ImY, ImZ`; exactly six numeric signed/unsigned zeros trigger ambiguity; forbid physical-zero/FEM claims. |
| P1-AC4 | Round-trip agreement could pass the same wrong unit factor or mutated input coordinates. | Assert exact factors `1`, `1000`, and `0.001`, finite transformed values, preserved caller `xyz`, and `metadata_unavailable` for unsupported units. |
| P1-AC5 | Hash format-only checks could pass a mutated source or uppercase/non-role-bound identity. | Independently mutate project, mesh, selected field, and post-source bytes; require lowercase SHA-256 and no success on each mismatch. |
| P1-AC6 | A narrow import grep could miss calls, strings, state, or another production module. | Scan both new production modules for job/solve/Line10/VFEM/worker/cache/timer/listener/state edges and require zero prohibited hits after review of literals. |
| P2-AC1 | A workspace test could pass a shared, permissive, escaped, or success-only-cleaned directory. | Require two unique contained restrictive workspaces and absence after every entered success/failure path. |
| P2-AC2 | “Source unchanged” could pass while a write-capable fake received a retained source path but happened not to mutate it. | Inspect fake call arguments and require disposable destinations/owned handles only; independently hash retained bytes before/after. |
| P2-AC3 | One aggregate mutation case could leave mesh/field/project/post-copy roles unguarded. | Falsify every monitored role separately and require `source_changed` with success suppressed. |
| P2-AC4 | A settlement count could pass an early event before cleanup/post-hash or duplicate events hidden by filtering. | Require exactly one unfiltered settlement event, ordered after all cleanup and post-hash attempts on every entered stage. |
| P3-AC1 | A plausible final value could pass solve/save/remesh/fallback calls or a hand-written header. | Require the complete literal vendor trace using the CST-generated header and explicitly assert absence of forbidden calls. |
| P3-AC2 | One rollback test could miss a pre-transfer boundary or close the wrong handle. | Inject handle, identity, liveness, token, and immediately-before-transfer faults; each must close the exact returned local handle once with transfer false. |
| P3-AC3 | No observed foreign close in one case could pass PID-difference authority elsewhere. | Interleave a foreign identity at every acquisition boundary and assert its complete operation set is unchanged; scan for process-set-difference ownership. |
| P3-AC4 | Any create exception mapped to unavailable could hide an unowned process leak. | Only explicit zero-creation or direct-rollback proof may yield `cst_unavailable`; missing proof must yield `session_settle_failed`. |
| P3-AC5 | A normal return could pass incomplete ownership or multiple transfers. | Require complete handle/identity/liveness/token, one transfer into an initially empty slot, and no transfer on all earlier exits. |
| P3-AC6 | Success-path sampling could hide permissive status, arity, boolean, non-finite, frequency, or metadata handling. | Falsify every named invalid result and require atomic stable-ID failure with no partial rows/success. |
| P4-AC1 | Seeing the new name could pass missing old tools or changed prompts/resources. | Require exact inventory: three unchanged HFSS plus four CST names, and literal unchanged prompt/resource inventories in-process and over real disposable stdio. |
| P4-AC2 | Direct model validation could pass while FastMCP enters runtime or emits sampler diagnostics. | Use actual `mcp.call_tool`; unknown nested keys and `true`, `null`, numeric zero, and string `allow_solve` must be framework-owned errors with zero entries/events. |
| P4-AC3 | Any error response could pass pre-entry rejection or missing settlement. | Duplicate IDs and point count above caller `max_points` must enter once, return stable `invalid_request`, and emit diagnostic plus exactly one settlement. |
| P4-AC4 | Parseable JSON or sibling agreement could pass duplicate content, non-finite values, reordered rows, path/PID leaks, or self-byte-count. | Require one canonical UTF-8 JSON `TextContent`, no `structuredContent`, exact schema/order/finite values, and literal absence of absolute path, PID, and self-byte-count. |
| P4-AC5 | Testing “near the limit” could pass off-by-one, truncation, partial rows, or spill. | Require exact final text sizes: `1,048,576` bytes succeeds; `1,048,577` bytes atomically returns only `response_too_large`, with no spill artifact. |
| P4-AC6 | Green old tests could pass silently updated snapshots or altered old bodies. | Compare accepted-baseline bytes/AST identities for all six schemas and four named CST bodies; candidate tests must use immutable literal hashes. |
| P5-AC1 | Calling the same singleton twice could pass retained mutable state or nondeterministic ordering. | Construct two fresh runtime/composition instances with controlled ambient inputs and require semantically identical frame/results and literal ordering. |
| P5-AC2 | Aggregate cleanup success could hide an unattempted resource or safely attributed remainder. | Inject each entered failure stage; require all owner-appropriate attempts and zero safely attributed owned sessions/processes/workspaces, including cleanup-failure override. |
| P5-AC3 | A naturally brief race could pass without ever observing the prohibited early-cleanup window. | Deterministically block vendor entry, cancel only the observer, assert zero cleanup while blocked, then release and require one settlement before the next accepted call. |
| P5-AC4 | A focused green sampler suite could hide package regressions, lint/format drift, dependency changes, or protected-file changes. | Run fresh sampler and full-package tests, Ruff lint/format, dependency/lock diff, protected diff, and publication scan; any timeout/unrun remains `UNVERIFIED`. |
| P5-AC5 | Testing current files could accidentally verify a different tree or include unadmitted paths. | Verify `HEAD` equals exact C and C contains exactly the nine admitted paths with `2,422` insertions and `3` deletions; hash/report the subject before and after QA. |
| P6-AC3 | A prose cross-reference to implementer evidence could pass without independent execution or leave target-independent criteria unmapped. | This QA run must map every P0–P5 ID to fresh independent evidence or an explicit gap; Claims 7 and 15 alone remain deferred target gates. |

Claims 7 and 15 are intentionally not local/fake acceptance criteria: this report must mark them deferred and must not infer their target truth from fake agreement.

## Execution evidence

Times below use a period as the decimal separator; the raw Windows PowerShell receipts use the host locale's comma. Every substantive test/scan receipt is preserved under `/.scratch/work-items/2026-08-11-cst-saved-field-sampler/qa-core/`.

| Receipt | Command or check | Result and exact counts | Wall seconds |
|---|---|---:|---:|
| `01-preflight.txt` | Candidate/protected preflight composite | PASS: 11 assertions, 4 executable probes; 9 admitted paths; 2,422 insertions, 3 deletions; 0 dependency/lock changes; 0 named protected changes | 0.891 |
| `02-baseline-compatibility.txt` | `uv run --frozen --directory <scratch-baseline>/servers/electromagnetics-mcp pytest -q tests/test_servers.py tests/test_stdio.py tests/test_cst_contract.py` | Harness failure before pytest: exit 2, missing extracted path; 0 tests | 0.369 |
| `03-baseline-compatibility-corrected.txt` | Same exact baseline pytest command after a file-based archive extraction | Harness failure at collection: exit 2, 2 import errors, 0 tests; bare `uv run` selected ambient global pytest because test extras were absent | 9.913 |
| `04-baseline-test-env-sync.txt` | `uv sync --frozen --extra test --directory <scratch-baseline>/servers/electromagnetics-mcp` | PASS: 7 locked test packages installed | 0.841 |
| `05-baseline-compatibility-after-env-fix.txt` | Exact baseline pytest command after the verified harness correction | PASS: 12 passed, 0 failed/skipped/xfail; 1 pre-existing warning | 6.043 |
| `06-prechange-red.txt` | `uv run --frozen --directory <scratch-RED>/servers/electromagnetics-mcp pytest -q --collect-only` over the three candidate sampler modules | Pytest PASS: 42 collected: 17 contract, 15 vendor, 10 integration; wrapper exit 1 because the prewritten expectation incorrectly required collection failure | 3.514 |
| `07-prechange-red-execution.txt` | `uv run --frozen --directory <scratch-RED>/servers/electromagnetics-mcp pytest -q ... --maxfail=1` | Expected RED: 1 passed, 1 failed on literal missing `cst_saved_field` contract module, 40 unrun after `--maxfail=1` | 2.277 |
| `08-candidate-sampler-suite.txt` | `uv run --frozen --directory servers/electromagnetics-mcp pytest -q tests/test_cst_saved_field_contract.py tests/test_cst_saved_field_vendor.py tests/test_cst_saved_field_integration.py` | PASS: 42 passed, 0 failed/skipped/xfail; 1 warning | 2.133 |
| `09-full-package-pytest.txt` | `uv run --frozen --directory servers/electromagnetics-mcp pytest -q -rA` | PASS: 85 passed, 0 failed/skipped/xfail; 1 warning | 5.738 |
| `10-ruff-check.txt` | `uv run --frozen --directory servers/electromagnetics-mcp ruff check src tests` | PASS: 0 diagnostics | 0.113 |
| `11-ruff-format.txt` | `uv run --frozen --directory servers/electromagnetics-mcp ruff format --check src tests` | PASS: 30 files already formatted | 0.112 |
| `12-oracle-and-static-inventory.txt` | Exact test-node/guard inventory plus production Line10/VFEM scan | PASS: 23 test functions expanding to 42 nodes; 0 production Line10/VFEM/native-evidence hits | 0.173 |
| `13-test-oracle-assertions.txt` | Line-numbered literal assertion inventory after CodeGraph omitted test bodies | PASS: assertion evidence captured; no execution | 0.066 |
| `14-critical-test-source-lines.txt` | Line-numbered read of frozen schema/body, wire, vendor, restart, settlement, cancellation, and budget oracles | PASS: four critical ranges captured; no execution | 0.064 |
| `15-source-hash-test-oracles.txt` | Line-numbered read of project/mesh/field SHA-256 and copy falsifiers | PASS: one range captured; exposed missing selected-field-copy falsifier | 0.012 |
| `16-source-flow-lines.txt` | Line-numbered read of `copy_field`, response assembly, source hashes, and settlement flow | PASS: live `copy_field` branch confirmed; no execution | 0.019 |
| `17-static-guard-hits-diagnostic.txt` | Broad whole-`cst.py` forbidden-token diagnostic | Diagnostic: 15 pre-existing solve/job-owner hits; not sampler additions | 0.051 |
| `18-static-guard-diff-aware.txt` | Diff-aware new-module/addition scan | Three scan assertions printed PASS; wrapper exit 1 from expected `rg` no-match status retained in `$LASTEXITCODE` | 0.949 |
| `19-static-guard-diff-aware-corrected.txt` | Same diff-aware scan with explicit aggregate exit | PASS: 3/3; 0 new-module forbidden edges, 0 forbidden added-line edges, 0 target-only concept hits | 0.379 |
| `20-publication-scanner-help.txt` | `python <global-lead>/scripts/check-publication-safety.py --help` | PASS: verified installed `--path` interface | 1.319 |
| `21-publication-scan-nine-paths.txt` | Nine serial `python <global-lead>/scripts/check-publication-safety.py --path <candidate-path>` invocations | PASS: 9 clean, 0 failed | 2.838 |
| `22-prompt-resource-tool-inventory.txt` | Same in-process `list_tools`/`list_prompts`/`list_resources` probe at candidate and baseline | PASS: candidate 4 CST tools/0 prompts/0 resources; baseline 3/0/0 | 10.840 |
| `23-qa-gap-proof.txt` | Independent source/test negative-coverage probe | PASS as a finding probe: 3 acceptance-oracle gaps confirmed | 0.111 |
| `24-final-integrity.txt` | Final exact-candidate blob/SHA-256/report/bug/gate integrity assertions | PASS: 9/9 worktree blobs equal candidate C; 0 dirty candidate paths; report, bug, and one terminal gate present | 3.056 |

The harness corrections in receipts 02–05, 06–07, and 18–19 are all retained. No failing candidate product test was rerun to obtain a green result.

## Acceptance-criterion matrix

| Criterion | Verdict | Fresh evidence and reason |
|---|---|---|
| P0-AC1 | PASS | Accepted baseline compatibility is 12/12 in receipt 05. Receipts 02–04 preserve the environment correction. |
| P0-AC2 | PASS | Receipt 06 collects all 42 sampler nodes; receipt 07 reproduces RED at the literal absent contract module, not an unrelated failure. |
| P0-AC3 | PASS | Frozen exact six-schema and four-CST-body oracle passes at `tests/test_cst_saved_field_contract.py:84`; receipts 08–09. |
| P0-AC4 | PASS | RED tree overlays only the three candidate test files on baseline production and asserts the production sampler module is absent; receipts 06–07. |
| P1-AC1 | PASS | Closed strict models, nested-extra rejection, literal-false handling, units, finite/bounded declarations, and `1..256` cardinality are exercised/inspected at `tests/test_cst_saved_field_contract.py:114` and `src/mcphub_em_mcp/cst_saved_field.py:55`; receipt 08. |
| P1-AC2 | PASS | Resolver table/permutation, selector, zero/many, tolerance, hash, and adaptive-pass outcomes pass in `test_saved_field_frame_resolution_table`; receipt 09 names the node. |
| P1-AC3 | PASS | Literal component order and signed-zero semantics pass; restart output repeats the exact order at `tests/test_cst_saved_field_integration.py:185`. |
| P1-AC4 | PASS | Exact `m`/`mm` factors produce four passing parameter nodes; unsupported-unit validation is included in the wire-schema test. |
| P1-AC5 | REVISE | Project, mesh, and post-source falsifiers pass, but the live selected-field `copy_field` mismatch branch at `cst_saved_field.py:720` has no independent corrupted-copy falsifier; receipt 23. |
| P1-AC6 | PASS | Diff-aware scan finds 0 prohibited job/solve/Line10/VFEM/background-state edges in new modules or added composition/helper lines; receipt 19. |
| P2-AC1 | PASS | Two unique contained workspaces and platform restriction checks pass at `tests/test_cst_saved_field_contract.py:295`; entered-stage cleanup removes workspaces. |
| P2-AC2 | PASS | Source project/mesh bytes remain literal `project-v1`/`mesh-v1` after both runs at `tests/test_cst_saved_field_integration.py:195`; owned/disposable paths are asserted in the fake trace. |
| P2-AC3 | REVISE | Project-copy, mesh-copy, and post-source mutations are falsified; selected-field-copy corruption is not. Receipt 23 reports 2 live source-branch hits and 0 test falsifier hits. |
| P2-AC4 | PASS | Seven entered failure stages each emit exactly one final settlement and remove the workspace at `tests/test_cst_saved_field_integration.py:211`; cleanup-failure ordering also passes. |
| P3-AC1 | PASS | Exact eight-step activation/sample trace, generated header paths, and row values pass at `tests/test_cst_saved_field_vendor.py:158`; prohibited-edge scan is clean. |
| P3-AC2 | PASS | Handle, identity, liveness, token, and before-transfer cases each close/absence-check the exact handle once with transfer false; receipt 09. |
| P3-AC3 | REVISE | One foreign-identity case passes, but the five-boundary acquisition matrix contains no interleaved foreign identity or preservation assertion; receipt 23. |
| P3-AC4 | PASS | Unproved create-before-handle failure returns `session_settle_failed` with zero handles; source classifies only explicit zero-creation/direct-rollback proof as safe. |
| P3-AC5 | PASS | All five incomplete-token exits keep transfer false; the complete-token cleanup-sequence test commits transfer true once; receipt 09. |
| P3-AC6 | PASS | Six invalid vendor-result cases pass: wrong arity, non-finite, boolean component, unknown status, frequency mismatch, and unverified time metadata. |
| P4-AC1 | PASS | Full tests include in-process and real stdio inventory. Independent baseline/candidate probe confirms only CST 3→4 tools and unchanged empty prompts/resources; receipt 22. |
| P4-AC2 | PASS | Actual `mcp.call_tool` rejects unknown nested input plus `true`, `null`, numeric zero, and string `allow_solve` before entry; entries remain exactly 0 at `tests/test_cst_saved_field_contract.py:426`. |
| P4-AC3 | PASS | Duplicate IDs and 2 points above caller limit 1 enter and return one `invalid_request`, followed by one settlement; lines 462–490. |
| P4-AC4 | PASS | Actual fake success is one text item, no structured duplicate, exact ordered/finite public fields, relative source identity, and no PID/absolute path/self-byte-count field; `tests/test_cst_saved_field_integration.py:323` plus response builder lines 581–676. |
| P4-AC5 | PASS | Literal `1,048,576`-byte text succeeds and the one-byte-over case returns only `response_too_large`, no original payload or structured duplicate; lines 493–509. Publisher is pure and has no spill path. |
| P4-AC6 | PASS | Exact six schema and four body hashes pass at candidate C; receipts 08–09. |
| P5-AC1 | PASS | Two fresh runtime/application constructions produce equal results, traces, events, order, source bytes, and zero owned remainder; lines 176–196. |
| P5-AC2 | PASS | Success, seven entered stages, five acquisition boundaries, create-before-handle, and cleanup failure all execute owner-appropriate settlement paths; receipts 08–09. |
| P5-AC3 | REVISE | The window is deterministically widened, but before release the test asserts only `running.done() is False`; there is no assertion that cleanup trace/events are still empty. Receipt 23 reports 0 pre-release cleanup assertions. |
| P5-AC4 | PASS | Sampler 42/42, package 85/85, Ruff 0 diagnostics, format 30/30, dependency/lock diff 0, protected diff 0, static scans clean, publication paths 9/9 clean. |
| P5-AC5 | PASS | Preflight verifies `HEAD` exact C, exactly 9 admitted paths, +2,422/−3, and byte-clean candidate paths; receipt 01. |
| P6-AC3 | REVISE | Independent mapping is complete, but four non-target AC rows remain unverified because three required falsifiers are absent. |

Target Claim 7 and Claim 15 are explicitly **DEFERRED**, not waived: no proprietary CST/native-provider/Line10 execution occurred.

## Failures and classification

| Finding | Classification | Impact / route |
|---|---|---|
| Missing selected-field-copy falsifier | Acceptance-oracle/test-coverage defect; not test rot, contract change, or observed product regression | Blocks P1-AC5 and P2-AC3. Add a persistent pre-fix RED for corrupt copied field, then preserve stable `source_changed`/`copy_field` behavior. |
| Foreign identity absent from five-boundary matrix | Acceptance-oracle/test-coverage defect | Blocks P3-AC3. Interleave and assert the same foreign identity at every boundary. |
| No pre-release cleanup/event assertion in blocked window | Degenerate race-window oracle | Blocks P5-AC3. Expose trace/events before release and assert both show zero cleanup/settlement. |
| Initial detached-baseline harness failures | QA harness/environment, corrected and preserved | Not a product failure. Locked `test` extras resolved ambient global pytest selection; receipts 02–05. |
| Expected-no-match wrapper exit retained as failure | QA wrapper aggregation, corrected and preserved | Not a product failure. Receipt 18 printed 3 PASS results; receipt 19 explicitly aggregates expected `rg` exit 1. |
| `IncompleteFieldDefinitionWarning` | Pre-existing dependency warning at both baseline and candidate | Non-blocking for this gate; one warning in both package runs. No dependency change is admitted here. |

The required open bug is `work-items/bugs/2026-08-11-cst-saved-field-core-qa-oracle-gaps.md`.

## Performance smoke

No hard latency budget is accepted for this fake/local phase. The basic bounded-completion smoke passed without timeout or hang: sampler 42 tests in 2.133 s, full package 85 tests in 5.738 s, Ruff lint in 0.113 s, and Ruff format check in 0.112 s. The engineered blocked test completed inside its explicit 10-second synchronization bounds. This is not a target CST performance claim.

## Ambient-input control

- Test commands fixed `PYTHONHASHSEED=0`, `TZ=UTC`, `LANG=C`, `LC_ALL=C`, and `NO_COLOR=1`.
- Tests ran single-process with no `xdist`; no parallel test scheduler or filesystem-order dependence was admitted.
- The concurrency oracle uses explicit `threading.Event` barriers and 10-second bounded waits, not the natural race-window duration.
- Resolver tests permute candidate order explicitly; point/result order is asserted literally.
- Temporary paths are pytest-owned unique directories or scratch trees; no retained source/live fleet path was used.
- `UV_LINK_MODE=copy` removes cross-filesystem hardlink behavior from the corrected detached-baseline harness. Dependencies came from the frozen lock; the 7-package test extra setup is recorded.
- Wall clock affects only reported duration, not correctness. Locale changes only decimal rendering in PowerShell receipts and is normalized here.

## Residual risk

- Claims 7 and 15 remain target-only, fail-closed empirical gates. Nothing here verifies installed CST activation/identity/status semantics or independent Line10 equality.
- Candidate production logic contains the selected-field-copy guard and sequential post-vendor settlement flow, but code inspection cannot substitute for the three missing regression falsifiers.
- No source/test correction was made because candidate C is immutable. A new candidate invalidates this gate and requires full same-angle re-verification.
- No CST/HFSS proprietary module was imported, no solver/job/Line10/native provider was invoked, and no live process, fleet, bundle, manifest, dependency, deployment, publication, or push was mutated.

## Gate

**REVISE — candidate `14a9b6b4cb9fc1e7248bd3b782b9e00d499181df` does not satisfy `P6-AC3`. Blocking rows: `P1-AC5`, `P2-AC3`, `P3-AC3`, and `P5-AC3`. Route the open bug to the implementation/test owner for a new immutable candidate, then repeat Phase-6 QA. Phase 7 must not start on C.**

## Terms and Abbreviations

- AC — acceptance criterion.
- CST — CST Studio Suite.
- FastMCP — the Model Context Protocol server framework.
- HFSS — High Frequency Structure Simulator.
- MCP — Model Context Protocol.
- PID — process identifier.
- QA — quality assurance.
- SHA-256 — Secure Hash Algorithm 256-bit.
- UTF-8 — Unicode Transformation Format, 8-bit.
- VFEM — vector finite-element method.
