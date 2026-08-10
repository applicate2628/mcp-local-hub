# MCP-CST-DEFAULT-001 implementation package

- Execution role: backend-engineer
- Assigned / replaced internal role: none
- Requested provider: internal
- Resolved provider: internal
- Actual execution path: Codex specialist task
- Model / profile used: unspecified by runtime
- Deviation reason: none

## Summary

The public `cst_solve` schema no longer advertises `null` defaults for the two
frequency-grid fields. `start`, `preflight`, and the default omitted-action path
now require `project_path`, `output_root`, `frequency_range_ghz`, and
`frequency_samples_ghz`. `status`, `result`, and `cancel` require `job_id` and do
not require frequency fields. No frequency bounds or samples are synthesized.

The existing owners remain separated:

- `strict_fastmcp.publish_action_requirements` owns the public action-conditional
  JSON Schema declaration and fails closed if FastMCP changes the expected
  generated schema shape (`strict_fastmcp.py:22-97`).
- `cst_solve` remains the single owner of semantic and cross-field validation,
  including range order, sample containment, uniqueness, and explicit
  adaptation-frequency membership (`cst.py:260-359`).

## Diagnostic and verified hypothesis chain

1. Fresh `tools/list` output advertised each frequency field as
   `anyOf(array, null)` with `default: null`, and advertised no required fields.
2. Safe local preflight calls with both fields missing, either field missing, or
   samples omitting the 5 GHz adaptation frequency failed at
   `cst.py:310-328`; explicit range `[1,20]` plus samples `[1,5,20]` passed.
3. `cst.py:283-286` routes `status`, `result`, and `cancel` before frequency
   validation, so making the frequency fields unconditionally required would
   break an existing public action contract.
4. Therefore the verified root cause was a mismatch between FastMCP's flat
   optional function-argument schema and action-specific validation. The fix is
   an action-conditional public schema, not an invented default grid.

No implementation-driving assumptions remain unverified.

## Changed files

| File | Change |
| --- | --- |
| `src/mcphub_em_mcp/strict_fastmcp.py` | Added one fail-closed action-requirements schema owner. It unwraps configured nullable execution fields and publishes one `if/then/else` requirement. |
| `src/mcphub_em_mcp/cst.py` | Registers the `cst_solve` action contract after tool registration and makes the tool description explicit. Runtime validation and job behavior are unchanged. |
| `tests/test_feedback_contract.py` | Adds the required schema, no-launch preflight matrix, adaptation-membership, and job-action compatibility guards. |
| `README.md` | Documents both required frequency fields, explicit adaptation membership, no derived grid, and job-action compatibility. |

## Wire-level before and after

| Surface | Before | After |
| --- | --- | --- |
| `frequency_range_ghz` | `anyOf: [array, null]`, `default: null`; no action-specific required declaration. | Array-only, no default, exactly two positive items; required for start/preflight/default action. |
| `frequency_samples_ghz` | `anyOf: [array, null]`, `default: null`; no action-specific required declaration. | Array-only, no default, 1 to 10000 positive items; required for start/preflight/default action. |
| `action=status/result/cancel` | Flat schema did not express `job_id` requirement; runtime router required it. | Conditional schema requires non-null string `job_id`; frequency fields remain omitted. |
| `action` omitted | Runtime interpreted omission as `start`, but flat schema allowed the invalid null frequency defaults. | Conditional `else` treats omission like start and requires the complete execution input. |
| Success and error envelopes | FastMCP MCP result/error shapes. | Unchanged. Schema-aware clients can reject incomplete input before the call; server semantic errors remain causal for direct callers. |

Affected consumers are MCP clients that inspect `tools/list` for `cst_solve`.
HFSS schemas and all CST exporter schemas are unchanged.

Authorization: no authorization boundary changed. The local-loopback CST MCP tool
remains available to an already admitted MCP session; this change only narrows
its advertised input contract. No outbound HTTP, database, queue, cache, or RPC
call was added or modified, so no timeout/retry/query-plan decision applies.

## Defect-class inventory

| Public surface / return path | Classification | Evidence |
| --- | --- | --- |
| `cst_solve` tools/list schema, start | Fixed | Conditional `else` requires both explicit frequency fields and removes their null/default forms (`cst.py:362-376`). |
|  |  | Existing `confirm=true` gate remains after all semantic validation (`cst.py:352`). |
| `cst_solve` tools/list schema, preflight | Fixed | Same explicit execution requirements; preflight returns before confirmation or job start (`cst.py:344-351`). |
| `cst_solve`, status | Not affected | Routed before execution validation and schema requires only `job_id` (`cst.py:283-286`). |
| `cst_solve`, result | Not affected | Same routed path; regression guard calls it without frequencies. |
| `cst_solve`, cancel | Not affected | Same routed path; regression guard calls it without frequencies. |
| `cst_solve`, default omitted action | Fixed | Runtime default remains start and conditional schema `else` requires execution input. |
| `cst_export_mesh` | Not affected | Separate tool with required `job_id`; no frequency input. |
| `cst_export_results` | Not affected | Separate tool with required `job_id`; no frequency input. |
| `_settings_history` | Not affected | Consumes already validated explicit settings; no public default or validation return path. |
| HFSS tools | Out of scope and unchanged | No HFSS source or test expectation was changed. |

## Verification evidence

RED was captured before the production edit:

```text
FAILED test_r1_cst_schema_declares_two_positive_frequency_bounds
KeyError: 'type'
```

The failure occurred because the old property was nullable `anyOf`, not an
array-only schema.

Fresh targeted GREEN:

```text
uv run pytest tests/test_cst_contract.py tests/test_feedback_contract.py -q
..................... [100%]
21 passed

uv run ruff check src/mcphub_em_mcp/cst.py \
  src/mcphub_em_mcp/strict_fastmcp.py tests/test_feedback_contract.py
All checks passed!

git diff --check
exit 0
```

Fresh safe wire/no-launch probe:

```text
frequency_range_ghz: type=array, has_default=false, allows_null=false
frequency_samples_ghz: type=array, has_default=false, allows_null=false
start missing both: schema invalid
preflight missing range: schema invalid
preflight missing samples: schema invalid
preflight explicit null range: schema invalid
preflight valid [1,20]/[1,5,20]: schema valid, valid=true, no job_id
status/result/cancel with job_id and no frequencies: schema valid
start valid grid plus confirm=false: PermissionError requires confirm=true
job_start_calls=0; temporary directory contents unchanged
```

No real CST solver, vendor interface, confirmed start, cancellation, export, job,
or output artifact was created.

A post-probe process inventory did find CST distributed-computing controller and
solver processes whose recorded creation time was 2026-08-09 19:14, before this
implementation lane. They were not stopped or modified. The test sentinel and
unchanged directory prove this lane made zero job-start calls and created no new
solver artifacts; independent live QA must continue to distinguish those
pre-existing processes from any process owned by a future authorized job.

## Receiving-side echo

- Named regression guard: RED then GREEN covers missing both fields, each field
  missing independently, explicit `[1,20]` / `[1,5,20]`, adaptation frequency
  absent from samples, and status/result/cancel without frequency fields.
- Must-not-break surfaces: status/result/cancel compatibility is explicitly
  guarded; preflight returns no `job_id`; `confirm=false` never calls
  `_jobs.start`; `additionalProperties=false` and numeric/cross-field validation
  remain covered by the existing suite.
- Diff-invisible invariants: no frequency values are synthesized; explicit
  adaptation membership remains enforced; verification created zero jobs and
  zero solver artifacts.
- Defect-class inventory: every public CST tool and every `cst_solve` action is
  classified above.

## Risks and unknowns

- FastMCP 1.29 has no public input-schema override API. The shared schema seam
  therefore uses its installed `_tool_manager.get_tool` surface. The project is
  pinned to MCP 1.29.x, and the helper fails server startup on an incompatible
  generated schema rather than silently publishing a false contract.
- Live port 9140 was not restarted or probed in this implementation lane. The
  independent QA/deployment owner must verify the deployed `tools/list` bytes
  after the product commit and manifest pin are published.
- The host already has CST distributed-computing processes created on the prior
  day. Future real-job QA must snapshot and attribute process ownership rather
  than treating process presence alone as evidence of a new launch.
