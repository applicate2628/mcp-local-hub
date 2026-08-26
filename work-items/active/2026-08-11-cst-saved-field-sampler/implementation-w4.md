# W4 Python Frontend and Service Composition Package

Execution role: `$backend-engineer`

Plan: `484883EDBAD02333162C61FAF78B99AA56C402FD64D5955F0E6B65BDDEC82E14`

Accepted design: `7423D56DD33394336A06AB8C515D12F4496B4AC0533F2901BF5A0EE1436756ED`

Accepted W3 package: `671ACFE9709FCB7E8040FBA5F3D2BB29728885E725DB0283BACE3566B772AD40`

## Receiving-side echo

- Implement W4 only: Python frontend, enrollment/daemon/broker protocol and service composition.
- Preserve W3 default-off receipt boundary; create or select no provision receipt.
- Do not touch Go/native, live Service Control Manager, App Control, CST, Git index, dependencies, or W5 worker five-handle containment.
- Preserve the existing six MCP tools, schemas and public error mapping.

## Implemented surface

- Frontend and daemon response receipts are closed serializable dataclass wire values with strict correlation and Boolean validation.
- The frontend client owns a fixed named-pipe endpoint, bounded canonical framing, one connection per challenge/exchange, explicit flush/terminal/ACK/EOF/close observations, cancellation, and stable redacted failures.
- Daemon and broker production `main()` entrypoints accept no injected Python object and remain default-off when no provisioned closed runtime composition exists. Existing injected compositions remain private test seams.
- The production topology is closed to three endpoints and four schemas.
- `WindowsNamedPipeBrokerTransport` uses the fixed broker endpoint, a provision-authenticated startup proof and bounded canonical length-prefixed frames. It owns one challenge/exchange channel and derives `BrokerExchangeReceiptV1` only from local write, flush, response, terminal, ACK, EOF and close observations.
- `WindowsBrokerClient` requires the matching complete local broker receipt before validating or returning the broker response; mismatch or incompleteness quarantines the daemon admission lease.
- `BrokerExchangeReceiptV1` is a closed daemon-local wire value containing no worker or containment facts.
- No worker or broker settlement was synthesized.

## Changed paths

- `src/mcphub_em_mcp/cst_saved_field_frontend_protocol.py`
- `src/mcphub_em_mcp/cst_saved_field_daemon_client_windows.py`
- `src/mcphub_em_mcp/cst_saved_field_daemon_service_windows.py`
- `src/mcphub_em_mcp/cst_saved_field_broker_client_windows.py`
- `src/mcphub_em_mcp/cst_saved_field_broker_service_windows.py`
- `tests/test_cst_saved_field_t15_production_composition.py`

The daemon/broker service files already contained uncommitted T15 composition changes before W4. W4 preserved those bytes and added only fixed-root/default-off/topology hunks.

## Verification

- Exact W4 T03/integration/T15 plus adjacent T05/T06/broker-client: PASS, 44 tests.
- W1 contract suite: 9 PASS and four expected RED tests remain exclusively W5 worker entry/bootstrap/five-handle contracts.
- Scoped Ruff check and format: PASS.
- Whole-tree Ruff format: BLOCKED by pre-existing formatting drift in `tests/test_cst_saved_field_w0_gap_baseline.py`; W4 did not edit it.
- `git diff --check`: PASS.

## Gate

`PASS`

The bounded correction implements the remaining W4 transport and client receipt gate without adding a dependency, selecting a provision receipt or asserting SCM, authentication, worker or containment facts that the local channel does not observe. Absent or incomplete provision/authentication startup proof returns unavailable before opening the broker channel.

## W5 handoff

W5 remains owner of `WorkerPreMainBootstrapV1`, `WorkerPreMainReceiptV1`, the exact five-handle tuple, containment receipt and authoritative worker settlement. It must not treat the W4 broker transport receipt as containment evidence.

## Terms and Abbreviations

- MCP: Model Context Protocol.
- SCM: Windows Service Control Manager.
- W4/W5: ordered implementation phases in the accepted plan.
