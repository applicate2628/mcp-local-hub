# Phase 0–5 Core Implementation Record — CST Saved-Field Point Sampler

## Receiving-Side Acceptance

| Item | Accepted value |
|---|---|
| Execution role | `$backend-engineer`, implementation and integration owner through Phase 5 |
| Accepted design | `design.md`; SHA-256 `613675976529CDC69843BFB9286FEC4F09C52868340F94F19246131D3DBF8E72` |
| Accepted architecture review | `architecture-review.md`; SHA-256 `E19A8EAC52BEBD4BA9959767A5B4EBE2C6AEB0F83CBBBA4C330BC3BB8C0E0262` |
| Accepted plan | `plan.md`; SHA-256 `7E12A018FE8DE9A282EAD5B8A27DAFF92D175F8C02D5A463690A50620A73BFAB` |
| Baseline | `2658ac85a0e1ee88b01f920af94c2664201e7a1c` |
| Candidate C | `14a9b6b4cb9fc1e7248bd3b782b9e00d499181df` |
| Scope | Phases 0–5 only; local pure/fake integration; no proprietary or live operation |

The Change-Surface Contract was accepted unchanged. Production changes are limited to additive registration/composition in `cst.py`, the new application core in `cst_saved_field.py`, the new injected vendor adapter in `cst_saved_field_vendor.py`, and one additive trusted-workspace helper in `safety.py`. Test changes are limited to the three sampler test modules and exact CST inventory updates in `test_servers.py` and `test_stdio.py`. Existing tool bodies, `_runner`, jobs, exporters, HFSS, dependencies, manifests, locks, retained bundles, and live processes remain protected.

Authorization remains at the existing MCP transport boundary: the additive tool is visible to a connected MCP client, and this phase adds no separate endpoint, credential, or authorization mechanism. The proprietary boundary is synchronous and injected; it has no retry, timeout, background worker, or post-entry cancellation promise.

## Exact Change Inventory

| Status | Path | Ownership |
|---|---|---|
| M | `servers/electromagnetics-mcp/src/mcphub_em_mcp/cst.py` | Additive tool registration, dependency composition, stable error translation, actual `CallToolResult` publisher |
| A | `servers/electromagnetics-mcp/src/mcphub_em_mcp/cst_saved_field.py` | Closed schemas, resolver, units, source/hash application owner, settlement, response assembly |
| A | `servers/electromagnetics-mcp/src/mcphub_em_mcp/cst_saved_field_vendor.py` | Injected transactional owned-session and activation/sampling adapter |
| M | `servers/electromagnetics-mcp/src/mcphub_em_mcp/safety.py` | Additive trusted restrictive ephemeral workspace helper |
| A | `servers/electromagnetics-mcp/tests/test_cst_saved_field_contract.py` | Contract, resolver, hashes, settlement, validation-channel, publisher tests |
| A | `servers/electromagnetics-mcp/tests/test_cst_saved_field_integration.py` | Full fake, entered failures, restart, cancellation, actual MCP success tests |
| A | `servers/electromagnetics-mcp/tests/test_cst_saved_field_vendor.py` | Vendor order, ownership, rollback, foreign-process and invalid-result tests |
| M | `servers/electromagnetics-mcp/tests/test_servers.py` | Exact CST inventory 3 → 4 |
| M | `servers/electromagnetics-mcp/tests/test_stdio.py` | Exact stdio CST inventory 3 → 4 |

Candidate C contains exactly these nine paths: 2,422 insertions and 3 deletions. This implementation record is the sole Phase 0–5 artifact and is intentionally written after C so it can record C's immutable identity; it is not part of C's implementation/test tree.

## RED → GREEN Receipts

| Phase | Witnessed RED | Minimal GREEN evidence |
|---|---|---|
| 0 | Exact inventory tests failed only because `cst_sample_saved_field` was absent; focused sampler collection first failed on the missing contract module. | Pre-existing baseline command was green 12/12 before the inventory edit; final compatibility command is green 12/12. |
| 1 | Closed schema first rejected the required JSON `xyz` array representation incorrectly; strict `allow_solve=false` then admitted numeric zero. | Array normalization and a strict literal-false validator made the Phase 1 selector green 8/8. Resolver, unit, six-component, exact-zero and hash tests are literal and independent. |
| 2 | Focused tests failed on missing trusted workspace and then missing `SamplerSession`. | Trusted contained workspace, source copy/hash owner and all-attempt settlement made the exact Phase 2 selectors green; `test_safety.py` plus `test_provenance.py` are green 9/9. |
| 3 | Vendor tests failed on the missing adapter; transactional boundary cases then drove exact rollback/absence evidence. | Exact Phase 3 selector is green 7/7; foreign identities remain untouched and pre-transfer boundaries close only the exact returned owned resource. |
| 4 | Actual FastMCP test first failed on missing publisher, then unknown tool registration. | Actual `mcp.call_tool` tests prove pre-entry framework rejection, post-entry stable errors, success, one text item, no structured duplicate, and exact byte-cap behavior. |
| 5 | Full fake integration first failed on missing runtime composition; later REDs exposed omitted frequency-tolerance forwarding, frozen exception traceback incompatibility on Python 3.13, missing-source error leakage, hardcoded `project.cst`, and boolean vendor components admitted as numbers. | Each boundary received the narrow owning fix. Final sampler-focused suite is green 42/42; full package is green 85/85. |
|  | Deterministic blocked-vendor test cancelled only the observer and proved no cleanup while blocked. | After release the trace is exactly `is_live, activate, clear, close, absent`, one settlement is emitted, and a later call is accepted. |
|  | Exact-path publication scan rejected the synthetic fixture text `opaque-owned-token`. | The non-secret fixture marker was renamed to neutral `owned`; affected tests stayed green and all nine candidate paths scanned clean. |

No raw test transcript is committed.

## Wire-Level Before and After

| Surface | Before | Candidate C |
|---|---|---|
| Tool inventory | 3 HFSS + 3 CST | 3 HFSS + 4 CST; only `cst_sample_saved_field` is additive |
| Pre-entry invalid request | FastMCP framework error | Unchanged ownership: unknown nested keys and invalid `allow_solve` values fail before runtime entry and emit no sampler event |
| Entered invalid/failure | No sampler channel | Explicit `CallToolResult(isError=true)` with one safe UTF-8 `TextContent`; stable `cst_saved_field.*` ID; diagnostic followed by exactly one settlement event |
| Success | No sampler response | Explicit `CallToolResult(isError=false)` with one canonical finite JSON `TextContent`, schema `mcphub.cst.saved_field_sample.v1`, and no `structuredContent` |
| Ordering | Not applicable | Input point order retained; components exactly `ReX, ReY, ReZ, ImX, ImY, ImZ`; fixed vendor activation and cleanup order |
| Publisher budget | Not applicable | Final `TextContent.text` alone owns the UTF-8 cap: 1,048,576 bytes admitted; 1,048,577 bytes atomically becomes only `cst_saved_field.response_too_large` |
| Cancellation | Not applicable | Synchronous and non-preemptible after entry; observer cancellation adds no token/checkpoint and cannot start cleanup while the injected call is blocked |

## Phase and Acceptance-Criteria Matrix

| Criterion | Verdict | Evidence |
|---|---|---|
| P0-AC1 | PASS | Baseline compatibility command green 12/12 at the accepted baseline before inventory RED. |
| P0-AC2 | PASS | Named contract/vendor/integration nodes collected; first focused failures were missing sampler symbols/registration, not fixture, syntax, environment, or CST imports. |
| P0-AC3 | PASS | Frozen hashes cover exactly six prior schemas and `_runner`, `cst_solve`, `cst_export_mesh`, `cst_export_results`. |
| P0-AC4 | PASS | Phase 0 was test-only; production was added only after accepted RED. |
| P1-AC1 | PASS | Closed Pydantic request/result/point models enforce strict enums, finite bounds, no extras, exact false, and 1..256 points. |
| P1-AC2 | PASS | Permutation, zero/multiple candidates, exact selector, tolerance boundary, expected hashes and adaptive pass are table-tested. |
| P1-AC3 | PASS | Literal component order and signed exact-zero ambiguity tests pass; no physical-zero or finite-element coefficient claim is emitted. |
| P1-AC4 | PASS | Exact `m`/`mm` identity and conversion factors pass; unsupported units fail `metadata_unavailable`; caller `xyz` is retained. |
| P1-AC5 | PASS | Literal lowercase SHA-256 identities plus project, mesh, selected-field and post-source mismatch falsifiers suppress success. |
| P1-AC6 | PASS | Solver/job/Line10/VFEM/background-mechanism source scans are clean. |
| P2-AC1 | PASS | Two workspaces are unique, restrictive, resolved under the trusted root, and removed on entered paths. |
| P2-AC2 | PASS | Write-capable fakes receive disposable destinations/owned handles only; monitored retained bytes remain unchanged in success/failure tests. |
| P2-AC3 | PASS | Independent project-copy, mesh-copy, field-copy and source-post-hash falsifiers return `source_changed` and no success. |
| P2-AC4 | PASS | Every tested entered stage emits one final settlement after all cleanup/post-hash attempts. |
| P3-AC1 | PASS | Literal fake trace proves open/frequency/metadata/generated-header/register/select/recheck/ordered sample, with no solver/save/remesh/fallback. |
| P3-AC2 | PASS | Handle, identity, liveness, token and before-transfer faults each close the exact local handle once and report no transfer. |
| P3-AC3 | PASS | Foreign identity sets remain unchanged at transactional boundaries; no process-set-difference authority exists. |
| P3-AC4 | PASS | Create-before-handle without zero-creation/direct-rollback proof yields `session_settle_failed`. |
| P3-AC5 | PASS | Only a complete token normal return commits transfer once into an initially empty application slot. |
| P3-AC6 | PASS | Unknown status, wrong arity, nonfinite/boolean components, frequency mismatch and missing/unverified metadata fail atomically with stable IDs. |
| P4-AC1 | PASS | In-process and real stdio inventories are exactly 3 HFSS + 4 CST; resources/prompts are unchanged. |
| P4-AC2 | PASS | Actual FastMCP calls reject unknown nested input and `allow_solve` true/null/0/string before entry with zero events. |
| P4-AC3 | PASS | Actual calls with duplicate IDs and point count above caller `max_points` enter and return `invalid_request` plus settlement. |
| P4-AC4 | PASS | Actual fake-composed success is one canonical JSON text item, no structured duplicate, no absolute path, PID, or self-byte-count. |
| P4-AC5 | PASS | Actual publisher admits 1,048,576 UTF-8 bytes and replaces 1,048,577 bytes with only `response_too_large`. |
| P4-AC6 | PASS | Frozen six-schema and four CST-body identity tests remain green. |
| P5-AC1 | PASS | Two newly constructed runtime/composition instances over identical fake input produce semantically identical frames/results without mutable module state. |
| P5-AC2 | PASS | Success and seven injected entered failure stages settle owner-appropriate resources; transactional acquisition boundaries and cleanup-failure override are also green. |
| P5-AC3 | PASS | Deterministically widened blocked window proves no early cleanup; release yields one settlement before a subsequent accepted call. |
| P5-AC4 | PASS | Full pytest, Ruff lint/format, dependency/lock diff, protected diff and static guards are fresh and green. |
| P5-AC5 | PASS | Candidate C is recorded above and contains exactly the nine admitted implementation/test paths. |

## Fresh Verification

| Gate | Result |
|---|---|
| Phase 0 compatibility command | PASS, 12 tests |
| Phase 0/final sampler suite | PASS, 42 tests |
| Phase 1 exact selector | PASS, 8 tests |
| Phase 2 exact selector | PASS, 2 tests |
| Phase 2 safety/provenance | PASS, 9 tests |
| Phase 3 exact selector | PASS, 7 tests |
| Phase 4 actual boundary selector | PASS, 2 tests |
| Phase 4 integration/inventory/stdio | PASS, 16 tests |
| Full package pytest | PASS, 85 tests |
| Ruff lint | PASS, all `src` and `tests` |
| Ruff format check | PASS, 30 files |
| Dependency and lock diff | PASS, empty |
| Protected jobs/results/HFSS diff and status | PASS, empty |
| `git diff --cached --check` | PASS |
| Forbidden solver/job/source-specific/background scans | PASS, no matches |
| Exact candidate path publication scan | PASS, all nine files individually clean |

The test run reports one pre-existing `pydantic-settings` unresolved-forward-reference warning; it is not a test failure and no dependency or unrelated configuration change is admitted here.

The plan's documented global PowerShell scanner wrapper was directly probed and is absent. The installed canonical scanner is `check-publication-safety.py`; its verified `--path` interface was used separately on every candidate path. This is an environment/tool-wrapper deviation, not a design or vendor-contract conflict.

## Evidence Anchors

| Claim owner | Falsifying probe anchor |
|---|---|
| Closed request and pure resolver | `servers/electromagnetics-mcp/src/mcphub_em_mcp/cst_saved_field.py:79`; `servers/electromagnetics-mcp/src/mcphub_em_mcp/cst_saved_field.py:131` |
| Source and settlement ownership | `servers/electromagnetics-mcp/src/mcphub_em_mcp/cst_saved_field.py:287`; `servers/electromagnetics-mcp/src/mcphub_em_mcp/cst_saved_field.py:377`; `servers/electromagnetics-mcp/src/mcphub_em_mcp/cst_saved_field.py:695` |
| Transactional vendor boundary | `servers/electromagnetics-mcp/src/mcphub_em_mcp/cst_saved_field_vendor.py:157`; rollback falsifier `servers/electromagnetics-mcp/tests/test_cst_saved_field_vendor.py:117` |
| Vendor result strictness | `servers/electromagnetics-mcp/src/mcphub_em_mcp/cst_saved_field_vendor.py:246`; falsifier table `servers/electromagnetics-mcp/tests/test_cst_saved_field_vendor.py:202` |
| Actual MCP publisher/registration | `servers/electromagnetics-mcp/src/mcphub_em_mcp/cst.py:271`; `servers/electromagnetics-mcp/src/mcphub_em_mcp/cst.py:291` |
| Validation-channel and byte-cap boundary | `servers/electromagnetics-mcp/tests/test_cst_saved_field_contract.py:426`; `servers/electromagnetics-mcp/tests/test_cst_saved_field_contract.py:493` |
| Entered failure settlement and cancellation | `servers/electromagnetics-mcp/tests/test_cst_saved_field_integration.py:211`; `servers/electromagnetics-mcp/tests/test_cst_saved_field_integration.py:288` |
| Trusted workspace | `servers/electromagnetics-mcp/src/mcphub_em_mcp/safety.py:93` |

## Assumptions and Residual Risks

| Item | Status and resolving probe |
|---|---|
| Actual target CST exposes the complete injected saved-field port semantics used by composition | ASSUMPTION (UNVERIFIED). Phase 7 must verify the exact installed CST version, owned handle, metadata, status policy, call trace and cleanup before any target acceptance. |
| Independent native evidence and Line10 exact-value agreement | Out of Phase 0–5 and unverified by design. Phases 8–9 own the fail-closed empirical gates. |
| Cancellation after vendor entry | No cancellation is promised. Target/operator observation must treat the synchronous call as non-preemptible until settlement. |
| Publication/deployment | Not authorized. Phases 6–12, independent review, target evidence and human publication/deployment gates remain mandatory. |

## Source and Fleet Safety Statement

No CST or HFSS solver/tool was called. No installed proprietary library was imported. No retained project was opened or mutated. No live hub, daemon, client binding, fleet process, manifest, pin, install, restart, publication, or push operation occurred. All vendor behavior used injected fakes and disposable `tmp_path` data. No PID-difference or foreign-process ownership mechanism was introduced.

## Gate

**PASS → Phase 6 independent architecture, security, then quality-assurance review of exact candidate `14a9b6b4cb9fc1e7248bd3b782b9e00d499181df`.** Any reviewer correction creates a new candidate and invalidates downstream evidence. Target CST and later phases remain closed.

## Terms and Abbreviations

- CST — CST Studio Suite.
- FastMCP — the Model Context Protocol server framework used by this package.
- HFSS — High Frequency Structure Simulator.
- MCP — Model Context Protocol.
- PID — process identifier.
- QA — quality assurance.
- RED/GREEN — a test-driven-development cycle in which a correct failing test precedes the minimal passing implementation.
- SHA-256 — Secure Hash Algorithm 256-bit digest.
- UTF-8 — Unicode Transformation Format, 8-bit.
- VFEM — vector finite-element method; intentionally absent from production sampler logic.
