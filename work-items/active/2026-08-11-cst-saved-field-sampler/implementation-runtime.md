# P07-P09 Runtime Implementation Handoff

Date: 2026-08-12
Execution role: backend integration owner
Scope: Plan P07-P09 only
Accepted containment input: `implementation-containment.md` SHA-256
`CD6526BC3D11FFB4B25CC8B9E5A2558DD545D301A915763040DBBF823236E70F`
The backend lane performed no Git mutation. The Lead subsequently completed the
plan-authorized history step and bound this handoff to immutable local candidate
`C = c7654794c79ee4d8eed5378117e2ce84f7608040`. No push, live CST/HFSS,
daemon, hub, or fleet mutation was performed.

## Outcome

The default-off CST saved-field runtime is implemented through a neutral application
port, concrete vendor acquisition and Result3D adapter, helper-owned complete bundle
transfer, atomic admission and containment, and conditional FastMCP 1.29.0
composition. The helper has no unwired success path. The installed CST adapter
surface is deliberately not exercised here; target CST qualification remains P11.

The Lead created immutable candidate `C` only after verifying exact ownership,
an empty index, absence from every remote ref, all 38 unrelated-byte receipts,
the exact 24-path staging allowlist, a clean staged diff, and 24/24 clean
publication-safety scans. The amend collapsed superseded candidate `14a9b6b4...`
into one unpublished commit directly above `origin/master`; the superseded commit
is not an ancestor of `C`, and the index is empty.

## Changed Paths

| Path | Runtime-slice ownership |
|---|---|
| `servers/electromagnetics-mcp/src/mcphub_em_mcp/cst_saved_field_port.py` | Neutral candidates, capability, acquisition and application settlement contracts. |
| `servers/electromagnetics-mcp/src/mcphub_em_mcp/cst_saved_field.py` | Filesystem-free request, selection, units, batch validation, result construction, sole application settlement. |
| `servers/electromagnetics-mcp/src/mcphub_em_mcp/cst_saved_field_vendor.py` | Bounded raw-record validation, transactional owned-session acquisition, fixed Result3D activation. |
| `servers/electromagnetics-mcp/src/mcphub_em_mcp/cst_saved_field_helper.py` | Concrete helper composition, complete transfer, vendor port binding, postflight and settlement. |
| `servers/electromagnetics-mcp/src/mcphub_em_mcp/cst_saved_field_containment_windows.py` | Admission-before-descriptor entry and contained invocation handoff. |
| `servers/electromagnetics-mcp/src/mcphub_em_mcp/cst.py` | Restart-only policy composition, conditional tool registration, contained invocation and direct text publication. |
| `servers/electromagnetics-mcp/src/mcphub_em_mcp/strict_fastmcp.py` | Sampler-only fixed pre-entry validation failure mapping. |
| `servers/electromagnetics-mcp/README.md` | Default-off provisioning, restart/revocation, limits, containment and catalogue semantics. |
| `servers/electromagnetics-mcp/tests/test_cst_saved_field_vendor.py` | P07 raw/vendor/acquisition/activation/copy-corruption oracles. |
| `servers/electromagnetics-mcp/tests/test_cst_saved_field_integration.py` | Pure application re-entry, receipt propagation and source-role mutation oracles. |
| `servers/electromagnetics-mcp/tests/test_cst_saved_field_composition.py` | P08 FastMCP 1.29.0 registration, schema, validation, authority and byte-cap oracles. |
| `servers/electromagnetics-mcp/tests/test_cst_saved_field_helper.py` | Pure-core dependency direction updated for admitted P07 concrete adapter. |

Every other dirty or untracked repository path was preserved. In particular,
`internal/api/port_alloc_excluded_windows.go`, `work-items/README.md`, other
work-items, registries and bug records were not edited by this lane.

## RED to GREEN Receipts

| Slice | RED receipt | GREEN receipt |
|---|---|---|
| P07 vendor owner | `pytest -q tests/test_cst_saved_field_vendor.py` returned 26 failures, all at `P07 concrete vendor adapter is missing`; no collection error or process activity. | Same focused suite first returned 26 passes; after adding exact 4,096/8 MiB and copy-corruption coverage it returns 30 passes. |
| P07 application correction | Focused integration returned 1 pass and 4 failures: acquisition receipt was `None`; project/mesh/field drift returned `session_settle_failed`. This is a correction RED, not a claimed pre-production RED. | Core/vendor/contract group returned 39 passes; full integration returns 5 passes with exact receipt equality and role-specific `source_changed`. |
| P07 selected-field oracle | Adding the independent copy-corruption oracle returned 8 failures because the concrete adapter had no expected-copy identity contract. | Vendor suite returns 30 passes; corrupted copy fails at `copy_field`, before ResultTree registration and with no point content. |
| P08 composition | `pytest -q tests/test_cst_saved_field_composition.py` returned 8 failures on absent registration and publisher owners. | Composition plus stdio returned 10 passes on installed MCP 1.29.0. |
| P09 aggregate | Full package initially exposed one stale P05 expectation and later one synthetic-probe expectation; each failed at its exact assertion. | `uv run --frozen --python 3.13 --directory servers/electromagnetics-mcp pytest -q` returned 484 passes. |

Historical selected-copy, foreign-identity and blocked-observer findings are not
misrepresented as pre-production RED. The accepted containment handoff records the
disposable mutation-sensitivity receipts for the three historical oracle gaps; this
slice adds the direct selected-copy RED above and preserves the named foreign and
blocked-window tests unchanged.

## P07-P09 Acceptance Reconciliation

| Acceptance criterion | Result | Evidence or residual |
|---|---|---|
| P07-AC01 | PASS | Complete invalid raw-record matrix; exact 4,096 candidates and exact 8 MiB metadata pass, each one-over fails atomically without selection. |
| P07-AC02 | PASS | Fake trace proves Result3D open/frequency/metadata/generated header/clean payload/register/select/frequency/ordered samples; forbidden solve/save/remesh/fallback/handwritten-header counters remain zero. |
| P07-AC03 | PASS | Handle, identity, liveness, token and pre-transfer failures close the exact local resource once, preserve the same foreign identities and carry the complete acquisition receipt into the sole application event. |
| P07-AC04 | PASS | Raise-before-handle requires zero-creation or direct-rollback proof; missing proof is `session_settle_failed`. |
| P07-AC05 | PASS | Deterministic project, mesh and field post-snapshot cases return role-specific `source_changed` after cleanup. |
| P07-AC06 | PASS | Corrupted selected-field copy returns `source_changed` at `copy_field`; no registration, success or partial point result. |
| P08-AC01 | PASS | Missing/disabled/invalid restart policy leaves exactly the baseline three CST tools and creates no sampler runner. |
| P08-AC02 | PASS | Valid synthetic enabled snapshot registers only `cst_sample_saved_field`; top-level, result and point schemas are closed, producing four CST and seven total tools. |
| P08-AC03 | PASS | Unknown nested input and every non-literal-false `allow_solve` value return exactly `cst_saved_field.invalid_request` before entry; frozen existing-tool hashes pass. |
| P08-AC04 | PASS | Lexical denial calls authority once and invokes zero helper/vendor/runtime work; every registered route uses the same in-server composition. |
| P08-AC05 | PASS | Installed MCP 1.29.0 returns exactly one `TextContent`, `structuredContent=None`; 1,048,576 UTF-8 bytes pass and 1,048,577 fail atomically. |
| P08-AC06 | PASS | README describes policy-off/on catalogues, provisioning/restart/revocation, manifest-v2, 60+10, quarantine, no-solve/no-console and redaction without machine-local enablement. |
| P09-AC01 | PASS | Full package has 484 passing tests; no skip, expected-failure or empty replacement was introduced. |
| P09-AC02 | PASS | Selected-copy corruption, five acquisition boundaries and accepted blocked-window mutation receipts directly falsify the prior gaps. |
| P09-AC03 | PASS | Full package, Ruff check/format and frozen existing-six body/schema hashes pass. |
| P09-AC04 | PASS | The guard allocation below contains every distinct P00-P08 named guard once, with evidence and target-only status. |
| P09-AC05 | PASS | Lead-owned immutable candidate `C=c7654794c79ee4d8eed5378117e2ce84f7608040` is one unpublished commit directly above `origin/master`; its 21 effective changed paths are the byte-different subset of the exact 24-path admitted staging list, old candidate `14a9b6b4...` is not an ancestor, index is empty, and all 38 unrelated-byte receipts match. |
| P09-AC06 | PASS | All three SR/QA bug records remain open; implementer evidence does not close them. |

## Guard Allocation

| Named guard | Fresh result | Target-only remainder |
|---|---|---|
| Complete-manifest transfer guard | PASS: complete source/destination row and mismatch matrices plus helper-owned transfer. | Target filesystem qualification remains P11. |
| Namespace identity guard | PASS: synthetic Win32 alias, reserved name, stream, reparse, link and identity matrix. | Target policy/source volumes remain P11. |
| Foreign-process guard | PASS: five acquisition boundaries preserve the same foreign identities. | Real CST foreign-process trace remains P11. |
| Settlement-order guard | PASS: acquisition receipts, blocked-window containment and sole application settlement tests. | Real CST descendant settlement remains P11. |
| Existing-wire compatibility guard | PASS: frozen six schemas and CST body hashes plus policy-off catalogue. | Existing-six real smoke remains P14. |
| In-server authority guard | PASS: restart-only registration and one composition path; authorization occurs only after atomic lease seal for contained production calls. | Provisioned daemon routes remain P12/P14. |
| Trusted-root injection guard | PASS: only `cst.py` reads the policy environment input; lower layers receive typed values. | Operator ACL provisioning remains P12. |
| Canary-redaction guard | PASS: protocol/stderr/canary matrices and fixed public errors. | Real vendor diagnostics remain P11. |
| Source-capability guard | PASS: core receives one opaque committed capability and vendor paths derive only beneath its workspace. | Real CST write-target instrumentation remains P11. |
| Workspace-transaction guard | PASS: every factory/transfer failure removes only its owned child and preserves siblings. | Target cleanup trace remains P11. |
| Finite-budget guard | PASS: exact/one-over policy, manifest, vendor 4,096/8 MiB, protocol, point, process and response limits. | Target wall-time trace remains P11. |
| Protocol drift guard | PASS: one neutral schema, canonical frame, correlation/revision/entry and full receipt equality. | None. |
| MCP-boundary budget guard | PASS: installed MCP direct one-text result and exact UTF-8 limit. | None. |
| Neutral-port guard | PASS: AST dependency test proves core imports neither filesystem nor concrete vendor; adapter imports neutral port only. | None. |
| No-job-edge guard | PASS: core contains no jobs, solve, exporter, save or remesh edge; frozen existing bodies pass. | None. |
| Component-order and zero semantics guard | PASS: exact ReX/ReY/ReZ/ImX/ImY/ImZ order and signed all-zero ambiguity. | Numeric target comparison remains P13. |
| Atomic-containment guard | PASS: real Win32 synthetic first instruction is already in exact Job with three inherited stdio handles and no console. | CST descendant proof remains P11. |
| Contained-duration guard | PASS: injected launch/read/timeout/crash/residual paths use one 60+10 state machine. | Target CST duration remains P11. |
| Sole-Job-handle guard | PASS: Win32 handle and kill-on-close probes leave no helper process. | CST descendant breakaway proof remains P11. |
| Quarantine linearization guard | PASS: settlement failure latches before release and queued/future calls fail. | Provisioned daemon restart trace remains P11/P12. |
| Vendor-record guard | PASS: complete shape/type/string/finite/enum/path/hash/pairing/status and aggregate matrix. | Installed CST record shape remains P11. |
| Validation-channel guard | PASS: sampler validation maps pre-entry only; existing tools retain baseline framework behavior. | None. |
| Solve-path preservation guard | PASS: full package and frozen body hashes show no solve/runner/exporter change. | Existing-six real smoke remains P14. |
| Publication guard | PASS: every admitted production/docs path and all 24 exact staged paths scanned clean; no manifest, registry audit, fleet or proprietary data change. | Final publication range scan and push remain P16/P17. |

## Final Verification

| Check | Result |
|---|---|
| Full package | PASS, 484/484. |
| Ruff check | PASS. |
| Ruff format check | PASS. |
| `git diff --check` | PASS. |
| Win32 containment and path-identity focused suites | PASS, 290/290. |
| WSL cross-platform safety | PASS: Python 3 compiled all core/port/vendor/helper/composition modules. |
| Helper/process cleanup | PASS: no Python process with the helper module command line remained after probes. |
| Dependency direction | PASS: no new package; pure core has no filesystem/concrete-vendor import. |
| Protected surfaces | PASS for this lane: no hub, CST manifest, HFSS or Go source edit; the reported Go diff is pre-existing unrelated work. |
| Publication scan | PASS on every admitted production/docs path. A whole-server scan is not a valid candidate scan because it traverses `.venv` and synthetic security fixtures and therefore reported expected markers. |

## Residuals

- P10 independent architecture, security and QA review has not run on this runtime
  delta.
- Proprietary target Claim 7, actual CST Job descendants, Line10 Claim 15,
  existing-six real smoke, manifest pin/build/deploy and fleet work remain in their
  later plan phases.

Gate: PASS — P07–P09 implementation, deterministic verification, exact-history
cleanup, immutable candidate creation, unrelated-byte preservation, and staged
publication safety are complete. P10 and all target/release gates remain open.

## Terms and Abbreviations

- ACL: Access-control list.
- CST: CST Studio Suite.
- MCP: Model Context Protocol.
- RED/GREEN: A failing test followed by the smallest passing implementation.
- WSL: Windows Subsystem for Linux.
