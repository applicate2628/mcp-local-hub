# T12 implementation — superseded route removal

Gate: PASS

## Scope and current invariant

T12 removes the parallel in-process broker application from the live tree. The sole nonce owner is now `BrokerNonceLedgerV1`, the sole production broker application route is `BrokerRuntimeServiceV1` → `ContainedWorkerBrokerApplicationV1`, and the accepted frontend → daemon → broker → contained-worker integration remains green (`src/mcphub_em_mcp/cst_saved_field_broker_service_windows.py:207`, `:270`, `:350`; `tests/test_cst_saved_field_integration.py:237`).

The distinct-principal/vendor boundary remains intact: `VendorIsolationProofV1`, `AuthenticatedPipeSession`, and `VendorPathPlatform` are still live at `src/mcphub_em_mcp/cst_saved_field_vendor_isolation_windows.py:32`, `:163`, and `:195`. No compatibility alias or fallback replaces the deleted route.

## Exact changed paths

| Path | T12 effect |
|---|---|
| `src/mcphub_em_mcp/cst_saved_field_vendor_isolation_windows.py` | Deleted `NonceLedger`, `SavedFieldBrokerService`, `InProcessBrokerTransport`, and only their now-unused imports. |
| `tests/test_cst_saved_field_broker_pipe_windows.py` | Moved nonce one-use/expiry oracle to canonical `BrokerNonceLedgerV1`. |
| `tests/test_cst_saved_field_broker_topology.py` | Added forbidden-symbol scan and deterministic `tmp_path` sensitivity proof (`:36`, `:41`, `:53`). |
| `implementation-t12.md`, `status.md` | Canonical phase receipt and recovery state. |

## RED to GREEN evidence

| Check | Current-session receipt |
|---|---|
| CodeGraph pre-edit | Resolved the three obsolete classes in `cst_saved_field_vendor_isolation_windows.py`; the only external consumer was the old nonce test. It separately resolved the accepted broker runtime/contained application and T11 integration test. |
| Diagnostic RED | Exact live-tree scan reported five declarations/references for `NonceLedger`, `SavedFieldBrokerService`, and `InProcessBrokerTransport`. |
| First focused run | Route tests were green; migrated nonce fixture failed because its synthetic QPC deadline was 50 seconds. The fixture was corrected to the contract's exact 60 seconds; no production change resulted. |
| Sensitivity RED | Synthetic stale file was detected twice because both fixture roots initially named the same directory. Separating the roots retained the falsifier and removed duplicate observation. |
| Final focused GREEN | Broker pipe/auth/nonce, topology/residue/sensitivity, and exact T11 production-route integration: 8/8 PASS. |
| Zero residue | Exact non-work-item source/test scan for the three deleted route symbols plus `FrontendExchangeReceiptV1`: `residue_count=0`. |
| Static | Ruff lint PASS; Ruff format-check PASS on all T12 code/test paths. Scoped `git diff --check` is part of the terminal receipt below. |
| CodeGraph post-edit | Auto-sync was disabled because another process held the CodeGraph file lock beyond its retry budget; the returned Go/UI symbols were rejected. Fresh source-line reads, tests, and the zero scan are the current oracle. |

## Claims, owner, and falsifier

| Claim | Single owner | Falsifying probe |
|---|---|---|
| No parallel/direct broker application route remains in live Python source/tests. | Topology residue oracle | Reintroduce any forbidden symbol in a temporary Python file; `test_t12_residue_oracle_detects_reintroduced_parallel_route` must detect it. |
| T11 route remains current. | Broker service composition plus T11 integration test | The exact integration test must fail if frontend bypasses daemon, daemon bypasses broker, or contained worker/receipt order is removed. |
| Nonce semantics have one owner. | `BrokerNonceLedgerV1` | Broker pipe nonce test exercises 256-bit value, one-use replay rejection, and expiry rejection. |
| Existing six tools are unchanged. | Protected existing `cst.py`/HFSS owners | T12 diff contains no existing-six production path; T13 owns broad byte/shape/error regression. |

## Backend receiving-side echo

No wire schema, endpoint, HTTP/RPC status, storage, pagination, retry, or authorization contract changed. The broker still receives the same admitted `BrokerRequestV1`, validates the same policy/nonce/deadline, invokes the same closed worker schema, and returns the same `BrokerResponseV1`. T12 only deletes an unselected parallel implementation and repoints its test to the current owner.

## Rollback and boundary

Rollback the three code/test paths above as one T12 group. That would restore the superseded route and its test but would not touch Git index or live state. T13 owns full serial Go/Python/static/portability regression; no broad suite, Service Control Manager, CST, hub, fleet, registration, deployment, commit, or push ran in T12.

## Terms and Abbreviations

- CST: Computer Simulation Technology Studio Suite.
- QPC: Query Performance Counter.
- RPC: Remote Procedure Call.
- PASS: the scoped phase acceptance criteria are satisfied by fresh evidence.
