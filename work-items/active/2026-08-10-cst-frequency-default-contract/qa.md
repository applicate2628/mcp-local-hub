# MCP-CST-DEFAULT-001 independent QA

- Execution role: qa-engineer
- Assigned / replaced internal role: none
- Requested provider: internal
- Resolved provider: internal
- Actual execution path: Codex specialist task
- Model / profile used: unspecified by runtime
- Deviation reason: none
- Implementation artifact SHA-256: `03A13D3EB33C3DC98423E0D4A0AF84AC52650A8F642510343FEFE2FD1D516D06`
- Gate: **PASS**

## Summary

The preserved pre-fix `HEAD` (`e1eb50aa`) independently reproduces
MCP-CST-DEFAULT-001: `frequency_range_ghz` is advertised as
`anyOf: [array, null]` with `default: null`, so the required non-null/no-default
oracle fails. The current worktree publishes both frequency fields as
array-only and default-free, conditionally requires the complete grid for
`start`, `preflight`, and omitted/default action, and keeps `status`, `result`,
and `cancel` valid with only `job_id`.

All named schema, semantic, no-launch, documentation, lint, full-suite, and
HFSS regression guards passed. No CST or HFSS solve, cancellation, export, job,
or result artifact was started or created.

## Pre-run oracle hardening

| Criterion | What a weak criterion would incorrectly let pass | Strengthened oracle |
| --- | --- | --- |
| Published frequency fields | Merely finding both property names would accept `nullable` plus `default: null`. | Require `type=array`, no `default`, no null branch, exact item bounds, and conditional required lists. |
| Action compatibility | Unconditional required fields would fix start while breaking job actions. | Validate both schema and runtime routing for `status`, `result`, and `cancel` with no frequency fields. |
| Preflight safety | `valid=true` or absence of `job_id` alone would not exclude a hidden job submission. | Replace both job-manager start methods with fail-fast sentinels, compare job counts, and hash the complete probe file tree. |
| Process safety | An unchanged output dictionary would not exclude a vendor process launch. | Compare matching CST/solver process identifiers before and after the safe probe. |
| Cross-field semantics | Positive in-range arrays alone would accept samples omitting the adaptation frequency. | Require an explicit runtime rejection when 5 GHz is absent from the sample set. |
| Strictness and bounds | A happy-path preflight would not test extra fields, zero, singleton range, or null. | Validate each degenerate payload against the published Draft 2020-12 schema. |
| HFSS must-not-break | CST-only coverage could hide shared FastMCP damage. | Execute HFSS schema and preflight no-start guards plus the complete package suite. |

## Executed evidence

Commands below are recorded in their executed working directory. Raw output is
retained under `.scratch/cst-frequency-default-qa/` because this report cites it.

| Check | Verbatim command | Expected | Observed | Counts / wall time | Raw output |
| --- | --- | --- | --- | --- | --- |
| Pre-fix RED | `git archive --format=tar --output=$archive HEAD -- servers/electromagnetics-mcp/src; tar -xf $archive -C $oldRoot; $env:PYTHONPATH=Join-Path $oldRoot 'servers\electromagnetics-mcp\src'; uv run --project '<repo>\servers\electromagnetics-mcp' python -c "import asyncio; from mcphub_em_mcp import cst; tool=next(t for t in asyncio.run(cst.mcp.list_tools()) if t.name=='cst_solve'); field=tool.inputSchema['properties']['frequency_range_ghz']; print(field); assert field.get('type') == 'array' and 'default' not in field"` (publication-safe normalization: the executed absolute repository prefix is replaced by `<repo>`). | Old schema fails the corrected contract oracle. | Printed `anyOf[array,null]`, `default: None`; assertion failed with exit 1. | 0 pass, 1 expected fail, 0 skip; 0.594 s | `01-pre-fix-red.txt` |
| Targeted contract suite | `uv run pytest tests/test_feedback_contract.py tests/test_cst_contract.py -vv` | All CST contract and adjacent HFSS guards pass. | Exit 0. | 21 pass, 0 fail, 0 skip; 1.140 s | `02-targeted-pytest.txt` |
| Complete package suite | `uv run pytest -vv` | No package regression. | Exit 0. | 39 pass, 0 fail, 0 skip; 2.948 s | `03-full-pytest.txt` |
| Independent safe schema/runtime probe | `$program | uv run python -` where `$program` enumerates the 24 named schema, runtime, no-start, job-count, file-hash, and HFSS assertions listed in the next section; the PowerShell wrapper snapshots matching CST/solver process IDs before and after. | Every absolute property passes; no job, file, or process delta. | Exit 0; 24 explicit PASS lines; job count `0->0`; start calls CST `0`, HFSS `0`; files `2->2`; processes `4->4`, no new PID. | 24 pass, 0 fail, 0 skip; 0.582 s | `04-independent-safe-probe.txt` |
| Ruff, diff hygiene, README | `uv run ruff check src/mcphub_em_mcp/cst.py src/mcphub_em_mcp/strict_fastmcp.py tests/test_feedback_contract.py`; from repository root: `git diff --check`; four exact README assertions. | All static and documentation checks pass. | Exit 0 for Ruff and diff; all four README assertions pass. | 6 pass, 0 fail, 0 skip; 0.145 s | `07-static-and-docs-final.txt` |
| CST published inventory | `uv run python -c "import asyncio; from mcphub_em_mcp import cst; ts=asyncio.run(cst.mcp.list_tools()); ...; assert len(hits)==2 and {x[0] for x in hits}=={'cst_solve'} and all(x[2]=='ABSENT' and x[3]=='array' and x[4]=='ABSENT' for x in hits)"` | No sibling CST tool publishes either frequency field or an invalid default. | Only `cst_solve` contains the two fields; both are array-only, default-free, and non-nullable. | 8 pass, 0 fail, 0 skip; 0.550 s | `08-cst-schema-inventory.txt` |
| Real job-manager routing | `uv run python -c "from mcphub_em_mcp import cst; ...; [call cst_solve for status/result/cancel with qa-definitely-unknown and assert causal unknown job_id]; ..."` | Each job action reaches the existing manager without frequency validation and creates no job. | Three causal `unknown job_id` results; job count `0->0`. | 4 pass, 0 fail, 0 skip; 0.554 s | `09-routed-actions-real-manager.txt` |

The independent probe was serial. Random seed, timezone, locale, and parallel
scheduling are inapplicable to its schema and direct-call assertions. File
ordering was explicitly sorted; temporary paths were confined to the
task-owned scratch directory.

## Named regression guard mapping

| Required guard | Expected | Observed evidence |
| --- | --- | --- |
| Missing both fields | `start`, `preflight`, and omitted action are schema-invalid; runtime preflight is causal. | Schema invalid for omitted/default and explicit start; targeted parameterized runtime guard passed. |
| Missing range only | Schema-invalid and runtime reports `frequency_range_ghz`. | PASS in independent probe and targeted test. |
| Missing samples only | Schema-invalid and runtime reports `frequency_samples_ghz`. | PASS in independent probe and targeted test. |
| Valid explicit grid | `[1,20]` plus `[1,5,20]` is schema-valid and preflight-valid without synthesis. | Returned the exact supplied arrays, `valid=true`, and no `job_id`. |
| Adaptation not in samples | Runtime rejects `[1,20]` because it omits 5 GHz. | Causal `must include adaptation_frequency_ghz` rejection. |
| Status/result/cancel without frequencies | Schema-valid with non-null `job_id`; actual manager path remains causal. | All three schema validations passed; all three direct manager calls returned `unknown job_id`, not execution-input errors. |
| No nullable/default-null tools/list form | Both fields are array-only, no default, no null branch. | PASS; only two frequency-field hits across all three CST tools, both owned by `cst_solve`. |
| `confirm=false` no start | Permission error occurs before `_jobs.start`. | `requires confirm=true`; CST start-call sentinel remained zero. |
| `additionalProperties` | Unknown field is schema-invalid and rejected before job lookup. | PASS in independent schema probe and both HFSS/CST targeted tests. |
| Numeric/array bounds | Zero, singleton range, and zero sample are invalid. | All degenerate schema cases invalid; existing semantic guards also passed. |
| HFSS smoke | HFSS tools schema and preflight remain valid and do not submit a job. | Targeted and full suites pass; direct HFSS preflight valid with zero start calls. |

## No-launch evidence

The direct probe created only two synthetic, already-existing-before-call input
fixtures under its scratch output root. Before and after the calls:

- CST `JobManager` record count was `0 -> 0`;
- patched start-call counters were CST `0` and HFSS `0`;
- the complete sorted fixture tree remained two files with identical SHA-256 hashes;
- the matching CST/solver process inventory remained four processes with no new process identifier;
- no response contained a job identifier.

This proves the QA calls did not enter either solver launch boundary.

## Defect-class inventory and receiving-side echo

| Participant / owner | Classification | Evidence |
| --- | --- | --- |
| `cst_solve` start | **fixed** | Conditional execution branch requires project, output root, range, and samples; confirm gate remains before job start (`cst.py:260`, `cst.py:352`, `cst.py:362`). |
| `cst_solve` preflight | **fixed** | Same explicit schema requirements; valid grid returns before confirmation/job start (`cst.py:344-351`). |
| `cst_solve` omitted/default action | **fixed** | Conditional `else` treats absent action as execution and rejects the missing grid. |
| `cst_solve` status | **not affected** | Schema requires only non-null `job_id`; direct manager returns causal unknown-job error. |
| `cst_solve` result | **not affected** | Same routing and causal behavior. |
| `cst_solve` cancel | **not affected** | Same routing and causal behavior. |
| `cst_export_mesh` | **not affected** | Separate schema; inventory contains no frequency fields. |
| `cst_export_results` | **not affected** | Separate schema; inventory contains no frequency fields. |
| Schema owner `publish_action_requirements` | **fixed** | Owns non-null publication and action-conditional required fields (`strict_fastmcp.py:22-97`). |
| Semantic owner `cst_solve` | **not affected / preserved** | Still owns range order, positivity, uniqueness, containment, and explicit adaptation membership (`cst.py:291-328`). |
| README contract | **fixed** | Explicitly documents both required fields, no derived grid, adaptation membership, and job-action compatibility (`README.md:43-46`). |
| HFSS tools and preflight | **not affected** | Shared strict schema and direct preflight guards pass with zero job submissions. |

No sibling published invalid default remains for either frequency field. The
published CST inventory is exactly `cst_solve`, `cst_export_mesh`, and
`cst_export_results`; only `cst_solve` contains those fields.

## Failures, warnings, and residual risk

- The only failing execution is the intentional pre-fix RED assertion.
- Pytest and direct imports emit one Pydantic Settings
  `IncompleteFieldDefinitionWarning` for FastMCP's `lifespan` annotation. It is
  present in both the pre-fix reproduction and current runs and does not alter
  the tested input schema or outcomes.
- FastMCP 1.29 exposes no public input-schema override API, so the implementation
  uses the pinned private tool-manager surface and fails closed on shape drift.
- The live deployed port 9140 was intentionally not restarted or tested in this
  implementation QA lane. Deployment QA must verify fresh live `tools/list`
  bytes after commit and rollout.
- No real numerical solve or vendor-application behavior was evaluated; that is
  outside this no-launch gate.

Basic performance acceptance is adequate for this contract-only change: the
complete 39-test package ran in 2.948 seconds wall time and the independent
schema/preflight probe in 0.582 seconds. No performance budget was specified,
and no runtime solver path was entered.

## Gate

**PASS.** MCP-CST-DEFAULT-001 is closed by independent RED/GREEN evidence. Lead
may proceed to commit, deploy, verify live port 9140 with the same safe
tools/list/preflight matrix, and publish. A real CST/HFSS solver launch remains
outside this authorization.

## Terms and Abbreviations

- CST: CST Studio Suite electromagnetic solver.
- HFSS: Ansys High Frequency Structure Simulator.
- MCP: Model Context Protocol.
- QA: quality assurance.
- RED/GREEN: a regression oracle failing before the fix and passing after it.
