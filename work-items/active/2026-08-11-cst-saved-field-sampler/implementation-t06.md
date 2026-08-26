# T06 — SCM daemon service, admission and daemon-local receipt

Gate: **PASS**

## Scope and invariant

The SCM daemon is now the sole `SamplerAdmissionGate` owner. It independently loads a policy snapshot through `from_policy`, consumes the enrolled frontend capability and one-use daemon challenge, resolves exactly one `entry_id`, creates the original 60-second Query Performance Counter (QPC) triple only after admission, and passes the unchanged triple to the broker. The admitted lease remains held until the daemon locally observes the complete response write, terminal write, flush, acknowledgement, disconnect, and server-handle close receipt. Missing evidence or concurrent shutdown quarantines before another admission.

The minimal adjacent broker-client seam `invoke_admitted` accepts the daemon-owned lease and QPC triple without releasing either. The legacy `invoke` path remains compatible and delegates to that seam. Broker challenge issuance may occur after admission; its `issued_tick` must remain within the original deadline.

Authorization: frontend `entry_id`, capability digest, request digest, and request content grant no policy authority. Only the daemon-loaded immutable policy snapshot resolves the entry; the broker still receives only revision, entry, manifest digest, request, and QPC triple. No retry or source/CST fallback exists.

## Evidence

| Gate | Receipt |
|---|---|
| MCP before edit | Exact broker query resolved current `WindowsBrokerClient` and `SamplerAdmissionGate`; exact new T04/T05 paths were unresolved and irrelevant GUI results were rejected as an index gap. |
| RED | `uv run pytest tests/test_cst_saved_field_t06_daemon_service.py -q --tb=short` → exit 1, 5 failed because the daemon service module did not exist. |
| Focused GREEN | T06 daemon + broker client + admission/containment: 39 passed. |
| Prior-phase regression | T03–T05 focused set: 29 passed. |
| Static | Ruff check PASS; Ruff format check PASS; scoped `git diff --check` PASS. |
| MCP after edit | Exact-path queries resolved current `WindowsCstDaemonService`, `from_policy`, `exchange`, `shutdown`, `WindowsBrokerClient.invoke_admitted`, their call edges, and T06 test coverage. No stale/disabled banner appeared. Irrelevant GUI source in one response was rejected. |
| Wider affected run | 74 tests collected: 73 passed, 1 failed in existing `test_named_daemon_broker_worker_integration_has_one_return_route`; returned `cst_saved_field.activation_failed` with an empty broker trace, proving the failure precedes `WindowsBrokerClient.invoke`. |
| Differential | A safe `.scratch` source copy restored `cst_saved_field_broker_client_windows.py` to its exact pre-T06 `invoke` implementation while retaining the accepted T05 frontend composition. The exact named test reproduced the same `activation_failed`, empty-trace failure. Therefore the failure is a pre-existing T05 stale integration anchor: it supplies a direct `WindowsBrokerClient` where `_compose_saved_field_tool` now requires the daemon-client `invoke(entry_id, request)` contract. T06 is accepted as differential non-regression; correction of the stale T05 test is outside T06. |

## Changed paths

- `src/mcphub_em_mcp/cst_saved_field_daemon_service_windows.py`
- `src/mcphub_em_mcp/cst_saved_field_broker_client_windows.py`
- `tests/test_cst_saved_field_t06_daemon_service.py`

Rollback: reverse these three T06 path deltas together. No live service, Software Control Manager (SCM), CST, hub, fleet, configuration, index, commit, or publication was touched.

## Terms and Abbreviations

- **QPC** — Windows Query Performance Counter.
- **SCM** — Windows Service Control Manager.
- **PASS** — all required and affected evidence is green.
- **REVISE** — implementation exists, but an affected regression oracle remains unresolved.
