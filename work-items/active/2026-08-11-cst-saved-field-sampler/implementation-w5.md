# W5 Broker-Owned Worker Containment Final Verification

Execution role: `$backend-engineer`

Plan: `484883EDBAD02333162C61FAF78B99AA56C402FD64D5955F0E6B65BDDEC82E14`

Accepted design: `7423D56DD33394336A06AB8C515D12F4496B4AC0533F2901BF5A0EE1436756ED`

Accepted W4 package: `51A85E8526E72245C46509E2AB1E8EFBE73DAC41935082452B44E139D1A95D04`

Native prerequisite: `implementation-w5-native.md` SHA-256 `79B69DD1914749FF5D4E18E7BAFD8C38B58AB004F8DA8EA6EAA6B42E4593016C`.

Broker capability prerequisite: `implementation-w5-capability.md` SHA-256 `7AC82C61C45F46C723CDDE72D1C2936EE94E61E8A0C97BB4FF708314AB47113F`.

Fixture reconciliation: `implementation-w5-fixture-correction.md` SHA-256 `178FE35C8B3229977B5CBE048E8BEFD0E301A684788F364DDEB75E765FCA5A33`.

## Receiving-side echo

- Verify only W5 worker containment, native pre-main bootstrap/receipt, exact broker capability owner and all-return settlement.
- Preserve W4 transports/default-off and the accepted native worker bytes.
- Do not add mechanisms, vendor/application W6 behavior, dependencies, Git/index or live Service Control Manager/CST state.
- Advance to correction PASS only when the exact W5 gate and requested preservation gates are fresh and green.

## Verified W5 state

- One broker production route opens exact source/workspace roots, validates identities/access/type/flags, creates exact inheritable duplicates and emits a correlation-bound `BrokerCapabilityReceiptV1`.
- The singleton non-reentrant `WorkerInheritanceEpoch` begins before inheritable duplicates/pipes and releases only after atomic `CreateProcessW` and closure of the five parent copies.
- `JOB_LIST` and the ordered five-handle `HANDLE_LIST` are applied together; no suspended/breakaway/shell/PATH fallback exists.
- The native worker emits startup proof, receives the unchanged QPC deadline in `WorkerPreMainBootstrapV1`, clears and validates inherited handles, and returns a bound `WorkerPreMainReceiptV1` before any application request.
- Current native exit 78 keeps the application unavailable/default-off; W6 remains the application integration owner.
- Kernel-local observations own process exit, Job active-zero, reader joins, exact pipe closure and handle closure. Capability duplicates, originals, epoch and workspace cleanup are settled on success, failure, cancellation, timeout and partial-allocation paths.

## Acceptance reconciliation

| Criterion | Fresh result |
|---|---|
| W05-AC01 exact atomic Job/five-handle/epoch launch | PASS |
| W05-AC02 native five-flag pre-main proof before privileged work | PASS |
| W05-AC03 closed Windows namespace/path rejection | PASS |
| W05-AC04 capability-relative manifest transfer and drift rejection | PASS |
| W05-AC05 ownership through lease/session/workspace settlement | PASS |
| W05-AC06 timeout/cancel/stop settlement and foreign-process exclusion | PASS |
| W05-AC07 vendor call-order and forbidden-call zero guard | PASS |

## Fresh verification

| Check | Result |
|---|---|
| Exact plan W5 suite: T08, Windows containment, integration, vendor | PASS: `81 passed`; one pre-existing Pydantic forward-reference warning. |
| Native W2 plus worker protocol/worker/pre-main preservation | PASS: `28 passed`. |
| W4 core T03/T05/T06/broker-client preservation | PASS: `25 passed`; same warning. |
| W4 T07 preservation | PASS: `14 passed`; exact three-argument seam observes the authorized `AuthorityEntry` and workspace owner. |
| W4 T15 preservation | PASS: `12 passed`; explicit capability receipt/set and native bootstrap/receipt replace the obsolete fixture injections. |
| Combined W4/W5/native preservation | PASS: `160 passed`; zero failed/skipped/xfail. Decomposition: W5 81, native 28, W4 core 25, T07 14, T15 12. |
| Scoped W5 Ruff check / format check | PASS / PASS: 11 files. |
| Whole `src tests` Ruff check / format check | REVISE only for pre-existing `tests/test_cst_saved_field_w0_gap_baseline.py` import/format drift. |
| Scoped `git diff --check` | PASS. |
| CodeGraph | Fresh pre-verification queries; W5 files current. Pending files reported by CodeGraph were unrelated Go files. |

## Changed paths

This final-verification turn changed only this artifact. The verified W5 package remains in:

- `servers/electromagnetics-mcp/src/mcphub_em_mcp/cst_saved_field_broker_worker_protocol.py`
- `servers/electromagnetics-mcp/src/mcphub_em_mcp/cst_saved_field_broker_worker.py`
- `servers/electromagnetics-mcp/src/mcphub_em_mcp/cst_saved_field_containment_windows.py`
- `servers/electromagnetics-mcp/src/mcphub_em_mcp/cst_saved_field_broker_service_windows.py`
- `servers/electromagnetics-mcp/tests/test_cst_saved_field_t08_containment.py`
- `servers/electromagnetics-mcp/tests/test_cst_saved_field_containment_windows.py`
- `servers/electromagnetics-mcp/tests/test_cst_saved_field_integration.py`
- `servers/electromagnetics-mcp/tests/test_cst_saved_field_w5_worker_pre_main.py`

## Gate

`PASS`

The native, broker-capability-owner and fixture-reconciliation receipts match their accepted SHA-256 identities without drift. All exact W5 acceptance criteria and the requested W4/native preservation surface pass in one fresh 160-test run. No production or test file changed in this final acceptance turn.

## Wire and API statement

The W5 private broker-worker wire adds the closed bootstrap, native receipt and broker capability receipt already accepted by the design. Public MCP schemas, HTTP/database/queue/cache surfaces and the six-tool inventory are unchanged. The authorized `AuthorityEntry` is an internal broker application argument needed to derive broker-owned capability authority; no public caller controls it.

## W6 handoff

W6 is unblocked. It may enter the application only after the accepted native pre-main receipt and must preserve the exact capability/epoch/Job/settlement receipts, unchanged QPC deadline and default-off exit-78 behavior when application composition is absent. W6 must not reinterpret transport receipts as containment evidence or move vendor/application work before native pre-main acceptance.

## Terms and Abbreviations

- HANDLE_LIST: Windows explicit inherited-handle allowlist process attribute.
- Job Object: Windows kernel process-containment object.
- QPC: Query Performance Counter, the monotonic deadline clock.
- W1-W6: ordered phases in the accepted plan.
