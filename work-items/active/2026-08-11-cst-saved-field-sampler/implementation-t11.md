# T11 implementation — three-endpoint synthetic integration

Gate: PASS

## Scope and invariant

T11 composes the accepted frontend → Service Control Manager daemon → broker → contained worker route without touching live services or CST. The frontend owns only its daemon client and publication; the daemon owns admission and the broker client; the broker alone converts an admitted broker request to the closed worker protocol and invokes `WindowsContainedInvocation` under a complete vendor-isolation proof.

`ContainedWorkerBrokerApplicationV1` preserves the exact correlation, policy revision, entry, manifest hash, request hash, and QPC deadline while converting the worker's authoritative settlement into `BrokerSettlementV1`. It never serializes the broker-owned workspace policy to the worker.

## Changed paths

| Owner | T11 effect |
|---|---|
| Broker composition | `src/mcphub_em_mcp/cst_saved_field_broker_service_windows.py`: concrete broker-owned worker protocol/containment/vendor-isolation adapter. |
| Endpoint inventory | `src/mcphub_em_mcp/cst_saved_field_vendor_isolation_windows.py`: removed the duplicate obsolete broker endpoint descriptor constants; accepted descriptor ownership remains in policy/service owners. |
| Synthetic integration | `tests/test_cst_saved_field_integration.py`: three-endpoint route through `WindowsDaemonClient`, `WindowsCstDaemonService`, `BrokerRuntimeServiceV1`, contained invocation and worker; ordered broker/daemon/frontend receipts. |
| Dependency oracle | `tests/test_cst_saved_field_broker_topology.py`: frontend→daemon and daemon→broker-only edges. |
| Topology oracle | `tests/test_cst_saved_field_t00_topology.py`: exact supervisor CST identity opcode owner and broker composition edges. |

## RED/GREEN evidence

| Check | Receipt |
|---|---|
| MCP pre-edit | Exact CodeGraph queries found the existing integration call into `_compose_saved_field_tool`, but failed to resolve the named broker/daemon composition roots and returned unrelated UI/Go symbols; those results were rejected. |
| Strict RED | Three exact tests failed: stale direct `cst.py`→broker expectation, direct broker injection into the daemon-client composition seam, and topology inventory gaps (duplicate endpoint owner, actual supervisor opcode file omitted, broker worker/containment/vendor edges absent). |
| Focused GREEN | Initial corrected affected set: 11/11 PASS. Final production-route integration plus topology/oracle set: 5/5 PASS. |
| Ordered ledger | `enrollment:capability-consumed` → broker challenge/admission → contained worker start/authorization/transaction/settlement → broker response settlement → daemon response write/flush/ACK/close → daemon admission release → frontend read/EOF/close. Publication occurs only after the frontend receipt returns. |
| Static | Ruff lint PASS; Ruff format-check PASS on all five T11 paths. |
| MCP post-edit | CodeGraph resolved the current synthetic test and its construction of `WindowsCstDaemonService`/frontend client. It still did not resolve the new untracked broker composition symbol and returned unrelated UI; this is recorded as an index gap, not production evidence. Source execution/static checks are the oracle for that symbol. |

The parent dispatch prohibited a broad repository run. Full serial Go/Python regression remains the explicit T13 owner gate; T11 used only the affected synthetic/topology/static surface.

## Receipt and ownership matrix

| Receipt/state | Sole local owner | Gate effect |
|---|---|---|
| Broker response plus `BrokerSettlementV1` | Broker | Must be complete before daemon result construction. |
| `DaemonResponseReceiptV1` | Daemon transport writer | Alone permits daemon admission release after response write/flush/ACK/disconnect/close. |
| `FrontendTransportReceiptV1` | Frontend transport reader | Alone permits frontend result return and MCP publication after complete frame/terminal/EOF-or-cancel/client close. |
| Capability/frontend challenge | Enrollment/daemon/frontend ledgers | Consumed on the successful route; all owning phase cancellation/quarantine tests remain the failure-path oracle. |
| Broker nonce | Broker ledger | Issued after admission, consumed atomically before application dispatch; runtime service owns cancellation and shutdown cleanup. |

## Preserved boundary and rollback

The existing six tools, `safety.py`, HFSS, solve/history, live Service Control Manager state, CST, hub, fleet, registration and deployment are untouched. T12 remains responsible for deleting superseded test-only/in-process route residue after this accepted route is composed.

Rollback the five T11 paths above as one group. This removes only the new broker composition and synthetic route/oracles; it does not alter Git index or any live process/service.

## Terms and Abbreviations

- CST: Computer Simulation Technology Studio Suite.
- MCP: Model Context Protocol.
- QPC: Query Performance Counter.
- SCM: Windows Service Control Manager.
- RED/GREEN: failing test before implementation, then passing after the minimal implementation.
- PASS: the scoped phase acceptance criteria are satisfied by fresh evidence.
