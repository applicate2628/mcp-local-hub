# W5 Fixture Reconciliation Verification

Execution role: `$qa-engineer`

Accepted W5 verification: `implementation-w5.md` SHA-256 `3EC3B72D5020A571B65FD84982068C084C93B3B20466E002D41D079DE9931A0F`.

Accepted broker-capability prerequisite: `implementation-w5-capability.md` SHA-256 `7AC82C61C45F46C723CDDE72D1C2936EE94E61E8A0C97BB4FF708314AB47113F`.

Scope was test-fixture reconciliation only. No production, Git index, commit, push, live Service Control Manager, CST, hub, registration, dependency or external state was changed by this lane.

## Acceptance preflight

| Criterion | What a weak criterion would let pass | Falsifying requirement |
|---|---|---|
| T07 current application seam | A lambda that accepts arbitrary positional arguments without proving the injected objects. | The fixture has an exact three-argument signature and the test observes the authorized `AuthorityEntry` plus workspace policy. |
| T15 containment settlement | A fabricated incomplete or default-valued capability path that fails before the selected containment bit is tested. | Supply an explicit complete `BrokerCapabilityReceiptV1` and `WorkerCapabilitySetV1`; independently set each of the seven containment facts false and require the stable settlement failure. |
| Worker composition | A removed diagnostics/self-attestation path or a born-green mock that ignores the native receipt. | Supply one matching `WorkerPreMainBootstrapV1` / `WorkerPreMainReceiptV1` pair and assert those exact objects are passed to `run_worker`; no diagnostics option exists. |
| W4/W5 preservation | Focused tests alone while adjacent owner routes regress. | Run the exact W5, native, W4 core, T07 and T15 preservation surfaces together with zero skips or xfails. |

Fixed inputs make clock values, random bytes, filesystem order and handle identities deterministic. Timezone, locale and parallel scheduling do not participate in these fixtures; pytest was run serially.

## Corrected test fixtures

| Path | Reconciliation and sensitivity |
|---|---|
| `servers/electromagnetics-mcp/tests/test_cst_saved_field_t07_broker_service.py` | `_service` now exposes the exact `(request, entry, workspace)` seam and the success oracle asserts one authorized `line10-e` entry and a workspace-policy object arrived. Restoring the old two-argument lambda reproduces the preserved TypeError. |
| `servers/electromagnetics-mcp/tests/test_cst_saved_field_t15_production_composition.py` | The seven containment-bit cases now cross the current application seam with an explicit authority entry, restricted workspace fixture, complete capability receipt and capability set. Each selected false containment fact remains the only reason for rejection. |
|  | Worker composition now supplies a matching bootstrap/native pre-main receipt and asserts exact forwarding. The obsolete `diagnostics` and `startup_observation` injection is absent. This path was already untracked in the shared worktree; this lane did not add it to the index. |

## RED evidence and failure classification

| Surface | Verbatim command | Fresh result | Raw output |
|---|---|---|---|
| T07 | `uv run --frozen --python 3.13 pytest -q --tb=short tests/test_cst_saved_field_t07_broker_service.py` | Exit 1; 1 failed, 13 passed; 0.552 s. Exact error: two-argument fixture lambda received three arguments. Classification: obsolete fixture / accepted internal contract reconciliation. | `/.scratch/cst-w5-fixture-qa-20260813/t07-red.txt` |
| T15 | `uv run --frozen --python 3.13 pytest -q --tb=short tests/test_cst_saved_field_t15_production_composition.py` | Exit 1; 8 failed, 4 passed; 0.581 s. Seven cases omitted `workspace_policy`; one supplied removed `diagnostics`. Classification: obsolete fixture / accepted internal contract reconciliation. | `/.scratch/cst-w5-fixture-qa-20260813/t15-red.txt` |

The Lead explicitly assigned this bounded fixture reconciliation after accepting the current W5 contracts, so this QA lane was authorized to update these obsolete fixtures rather than return them to a production implementer. Assertions were strengthened, not weakened.

## Fresh verification

| Surface | Verbatim command | Result | Wall time | Raw output |
|---|---|---|---|---|
| T07 focused | `uv run --frozen --python 3.13 pytest -q --tb=short tests/test_cst_saved_field_t07_broker_service.py` | PASS: 14 passed; 0 failed/skipped/xfail. | 0.461 s | `/.scratch/cst-w5-fixture-qa-20260813/t07-green.txt` |
| T15 focused | `uv run --frozen --python 3.13 pytest -q --tb=short tests/test_cst_saved_field_t15_production_composition.py` | PASS: 12 passed; 0 failed/skipped/xfail. | 0.441 s | `/.scratch/cst-w5-fixture-qa-20260813/t15-green.txt` |
| W4/W5 preservation | `uv run --frozen --python 3.13 pytest -q --tb=short tests/test_cst_saved_field_t08_containment.py tests/test_cst_saved_field_containment_windows.py tests/test_cst_saved_field_integration.py tests/test_cst_saved_field_vendor.py tests/test_cst_native_runtime_w2.py tests/test_cst_saved_field_broker_worker_protocol.py tests/test_cst_saved_field_broker_worker.py tests/test_cst_saved_field_w5_worker_pre_main.py tests/test_cst_saved_field_t03_contracts.py tests/test_cst_saved_field_t05_frontend.py tests/test_cst_saved_field_t06_daemon_service.py tests/test_cst_saved_field_broker_client_windows.py tests/test_cst_saved_field_t07_broker_service.py tests/test_cst_saved_field_t15_production_composition.py` | PASS: 160 passed; 0 failed/skipped/xfail. Decomposition: W5 81, native 28, W4 core 25, T07 14, T15 12. One pre-existing Pydantic unresolved-forward-reference warning. | 2.410 s | `/.scratch/cst-w5-fixture-qa-20260813/preservation.txt` |
| Scoped Ruff | `uv run --frozen --python 3.13 ruff check tests/test_cst_saved_field_t07_broker_service.py tests/test_cst_saved_field_t15_production_composition.py` | PASS: all checks passed. | 0.094 s | `/.scratch/cst-w5-fixture-qa-20260813/ruff-check.txt` |
| Scoped format | `uv run --frozen --python 3.13 ruff format --check tests/test_cst_saved_field_t07_broker_service.py tests/test_cst_saved_field_t15_production_composition.py` | PASS: 2 files already formatted. | 0.090 s | `/.scratch/cst-w5-fixture-qa-20260813/ruff-format.txt` |
| Scoped diff | `git diff --check -- tests/test_cst_saved_field_t07_broker_service.py tests/test_cst_saved_field_t15_production_composition.py` | PASS: empty output. | 0.063 s | `/.scratch/cst-w5-fixture-qa-20260813/diff-check.txt` |

## Receiving-side echo

| Required invariant / guard | Result |
|---|---|
| Exact W5 capability owner and all-return settlement stay intact. | VERIFIED by 81 W5 tests plus the seven independently false containment-fact cases. |
| W4 transport/default-off behavior stays intact. | VERIFIED by 25 W4 core tests plus T07 14 and T15 12. |
| Accepted native worker bytes and pre-main contract stay intact. | VERIFIED by 28 native/worker/pre-main tests and exact bootstrap/receipt forwarding assertion. |
| No production or live-state mutation. | VERIFIED for this lane: the only mutation calls targeted the two test files and this canonical QA artifact; no Git/index/live operation was used. |
| Named regression guard: old two-argument application signature fails. | Expected failure reproduced before correction; current exact three-argument fixture passes and observes both injected owners. |
| Named regression guard: removed diagnostics injection fails. | Expected constructor TypeError reproduced before correction; current composition accepts only the explicit bootstrap/native receipt and passes. |

Defect-class inventory: the one T07 two-argument fixture, all seven T15 containment-return cases, and the one T15 diagnostics fixture are `fixed`. Other T07/T15 tests are `not-affected`; all pass in the same fresh runs.

Post-edit CodeGraph MCP resolved the current T07 three-argument fixture and the T15 bootstrap/pre-main receipt call path without a stale banner for either target. Its pending-index warnings named unrelated files only.

## Residual risk and gate

The preservation surface is synthetic and does not claim live App Control, Service Control Manager, installed CST, HSM signing or target promotion. That boundary is unchanged from the accepted W5 artifact.

`PASS:W5-fixture-reconciliation`

Resume W5 from `implementation-w5.md` with its sole preservation blocker closed. The Lead may accept W5 and dispatch the next planned W6 integration owner; no additional W5 production correction is indicated by this evidence.

## Terms and Abbreviations

- CST: CST Studio Suite.
- HSM: Hardware Security Module.
- MCP: Model Context Protocol.
- SCM: Windows Service Control Manager.
- W4-W6: ordered delivery phases in the accepted plan.
