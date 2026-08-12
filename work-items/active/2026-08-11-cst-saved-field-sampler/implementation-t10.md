# T10 implementation — vendor isolation, session ownership and settlement

Gate: PASS

## Scope and invariant

T10 installs the per-invocation `SamplerSession` owner. It borrows exactly one non-copyable `AuthorizedVendorPathLease` from the authorized snapshot, adopts one directly returned CST session, and settles in the fixed order cache clear → close without save → exact absence → lease close (one retry) → workspace removal. Success cannot outrun any incomplete owner receipt.

The concrete vendor adapter depends inward only on `cst_saved_field_port`: it validates untrusted vendor-relative names through the neutral contract, accepts only opaque lease-returned locators, contains no filesystem reopen/copy/hash fallback, and cannot settle the borrowed lease. `IsolatedVendorPathLease` now requires a complete fixed-service/principal/workspace proof before returning any path-only locator.

## Changed paths

| Owner | Path and effect |
|---|---|
| Neutral port | `src/mcphub_em_mcp/cst_saved_field_port.py`: neutral vendor-relative grammar; retry-aware lease settlement completeness. |
| Application/session | `src/mcphub_em_mcp/cst_saved_field_application.py`: one snapshot/lease/session owner and exact all-return settlement. |
| Vendor adapter | `src/mcphub_em_mcp/cst_saved_field_vendor.py`: policy/filesystem dependency removed; neutral validation only. |
| Windows isolation | `src/mcphub_em_mcp/cst_saved_field_vendor_isolation_windows.py`: required `VendorIsolationProofV1`. |
| Tests | `tests/test_cst_saved_field_t10_vendor_application.py`, `tests/test_cst_saved_field_vendor_isolation_windows.py`. |

## Authorization and contract effect

Caller strings grant no authority. The only path-capable object accepted by vendor code is the snapshot-created `AuthorizedVendorPathLease`; isolation additionally requires exact `McpLocalHubCstVendorBroker`, `NT SERVICE\McpLocalHubCstVendorBroker`, protected workspace access, daemon denial, and session 0 proof. There is no retry of vendor work or path fallback; the sole retry is the specified owner close retry during settlement.

This changes internal Python contracts only: `IsolatedVendorPathLease` gains required keyword-only `proof`; `VendorPathLeaseSettlement.complete` admits additional close attempts only when all handles are closed; and `SamplerSession` is the new application owner. No Model Context Protocol or JSON wire field changes.

Receiving-side echo: `SamplerSession` receives the exact snapshot lease once, passes the same borrowed object to vendor work, consumes session/lease/workspace receipts locally, and returns source-drift evidence only after resources are settled.

## Evidence

| Check | Receipt |
|---|---|
| CodeGraph pre-edit | Resolved neutral port, vendor, isolation, snapshot and application callers; showed no concrete production `SamplerSession` consumer. One narrower test query returned irrelevant Go symbols and was rejected. |
| Strict RED | New T10 suite: 4/4 failed for extra policy dependency, missing isolation proof, and missing session/settlement owner. |
| Focused GREEN | T10 plus vendor and Windows-isolation owner tests: 42/42 passed. |
| Saved-field differential | All `test_cst_saved_field*.py` except intentional T00 RED passed after excluding three already-open non-T10 anchors: daemon broker-client topology, stale synthetic integration route, and T00 topology inventory. The unexcluded first run reproduced only those exact three failures. |
| Static | Ruff passed; scoped formatting passed; scoped `git diff --check` passed. |
| CodeGraph post-edit | Exact current query resolved `SamplerSession`, both callers, isolation lease callers and retry-aware settlement with no stale-index banner. |

## Rollback

Revert the four production paths and two test paths above as one T10 group. This restores the prior vendor dependency, proof-less lease constructor, and absence of the session owner without touching Git index, live CST, Service Control Manager, hub, fleet, deployment, or registration.

## Terms and Abbreviations

- CST: Computer Simulation Technology Studio Suite.
- MCP: Model Context Protocol.
- RED/GREEN: failing test before implementation, then passing test after the minimal implementation.
- PASS: the phase acceptance criteria are satisfied by fresh scoped evidence.
