# T07 — SCM broker service, protocol server and runtime descriptors

Gate: **PASS**

## Scope and invariant

The Software Control Manager (SCM) broker is the sole saved-field owner of broker policy admission, `MCPHUB_EM_OUTPUT_ROOT` composition, broker nonces, daemon-peer authorization, and broker descriptor readback. It independently resolves one enabled policy entry and binds the manifest, request digest, policy revision, correlation and unchanged Query Performance Counter (QPC) deadline triple. Challenge issuance may occur after admission but never rebases the original deadline; its one-use nonce is consumed or cancelled on every broker exit.

Authorization: only the exact numeric daemon service security identifier, service token, SCM process identity, session zero, High integrity level, pinned image, and absent dangerous privileges are admitted over the fixed broker endpoint. Caller request content, hashes and entry identifiers confer no authority. There is no retry, alternate endpoint, in-process fallback, frontend import, or MCP import; failures return the fixed safe broker contract.

For the new sampler route, only `cst_saved_field_broker_service_windows.py` reads `MCPHUB_EM_OUTPUT_ROOT` and injects `TrustedWorkspacePolicy`. The sampler frontend, daemon, application and vendor modules do not read it. The accepted existing-six legacy reader in `safety.py`, and its HFSS/CST callers, remain unchanged.

## Evidence

| Gate | Receipt |
|---|---|
| MCP before edit | Exact queries resolved the current challenge equality guard, broker client/transport, descriptor placeholders and their callers. The first exact Python ownership query returned unrelated Go/UI results and was rejected as an index gap. |
| RED | `uv run pytest tests/test_cst_saved_field_t07_broker_service.py -q --tb=short` → exit 1, 14 failed because the broker service module and T07 owners were absent. |
| Initial GREEN | T07 + broker pipe/client → 18 passed, 1 ownership-oracle failure: the original plan incorrectly included the accepted legacy `safety.py` reader. Work stopped without modifying the legacy six. |
| Corrected authority | Corrected plan SHA `17E8F9C25033A62C236F1D07F16731A3F7CCFF20263BA83F21BC43C26A4D26B2` scopes sole ambient ownership to the new sampler route and preserves `design.md:273-277,1314-1316`. The oracle now scans only `cst_saved_field*.py`; legacy `safety.py`, `hfss.py` and `cst.py` are untouched. |
| Focused GREEN | T03–T07 plus broker protocol, pipe and client: 56 passed. |
| Static | Ruff check PASS; Ruff format check PASS (`5 files already formatted`); scoped `git diff --check` PASS. |
| MCP after edit | Exact query resolved current `build_broker_descriptor`, `validate_sampler_descriptors`, `load_output_workspace_policy`, their callers and T07 tests. No stale or disabled-index banner appeared. A narrower request listed the exact new test symbol but returned unrelated source, so that response was not used as source evidence. |

## Changed paths

- `src/mcphub_em_mcp/cst_saved_field_broker_service_windows.py`
- `src/mcphub_em_mcp/cst_saved_field_broker_protocol.py`
- `src/mcphub_em_mcp/cst_saved_field_vendor_isolation_windows.py`
- `tests/test_cst_saved_field_broker_pipe_windows.py`
- `tests/test_cst_saved_field_t07_broker_service.py`

Wire-level effect: `BrokerChallengeV1` now accepts a broker-issued tick later than admission while retaining the unchanged original QPC triple and bounding expiry to the earlier of five seconds or the original deadline. `BrokerResponseV1` fields, ordering, failure identifiers and consumers are unchanged. The new service endpoint is synthetic/default-off in this phase; no live SCM registration or transport mutation occurred.

Rollback: reverse these five T07 path deltas together. No live SCM service, CST process, hub, fleet, configuration, index, commit, push, deployment, or registration was touched.

## Terms and Abbreviations

- **MCP** — Model Context Protocol.
- **QPC** — Windows Query Performance Counter.
- **SCM** — Windows Service Control Manager.
- **PASS** — all phase acceptance criteria and focused regression checks are green.
