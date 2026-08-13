# W7 In-Scope Backend Correction

Execution role: `$backend-engineer` integration correction owner

Input W7: `05C62691BC8210ECF5B35CB8EC2C56DC782BEB382D175F6FAA3F242C463AAB3A` (`REVISE`)

Input bug: `879BAA7DF07025131B88D9E5EBEDF85FA0E4364400916AF7FE5B03CC015C1FCA`

## Receiving-side echo

- Correct only W7 in-scope endpoint ownership, worker capability receipt, W0 formatting, and the missing real Windows frontend/Go/native/local-pipe proof.
- Preserve default-off behavior, the existing six tools, live Service Control Manager, policy, CST, dependency state, Git index, and unrelated dirty paths.
- Do not manufacture a production provision receipt: the end-to-end test uses a `t.TempDir` image/manifest and an in-memory synthetic receipt.

## Diagnosis and RED evidence

| Gap | Fresh RED |
|---|---|
| Fixed endpoint ownership | `test_t00_accepted_three_endpoint_four_schema_topology`: duplicate broker/frontend literal owners in policy plus transports. |
| Worker receipt | W0 receipt parameter: authoritative `WorkerCapabilityReceiptV1` absent. |
| Real launch path | New Windows E2E first exposed `RtlSecureZeroMemory` as a nonexistent `ntdll` export, then failed `native frontend never crossed the fixed local pipe`. |

Root causes were duplicated concrete endpoint literals, an unimplemented worker-local receipt schema/producer port, a never-exercised invalid zeroization procedure lookup, and a Python-only W6 integration substitute.

## Implemented package

- `cst_saved_field_endpoints.py` is the single literal owner. Policy, broker transport and daemon transport consume its constants; existing enrollment/daemon/broker consumers continue through the same canonical tuple.
- `WorkerCapabilityReceiptV1` is a closed, correlation-bound wire value owned by the broker-worker protocol. It records only worker-local handle-relative use, source/workspace close readbacks and absence of new authority. It contains no broker epoch/access/share/type/parent-close fact from `BrokerCapabilityReceiptV1`.
- `SavedFieldWorkerTransactionV1` obtains the worker capability receipt from the provisioned application port after its actual settlement readbacks; `BrokerWorkerResponseV1` carries, strictly parses and validates it.
- W0 imports/formatting were repaired mechanically.
- `windowsSecureZero32` now zeroes the fixed buffer directly and keeps it live; no unavailable dynamic procedure is invoked.
- Windows E2E crosses the real `StdioHost`/`exec.Cmd` launch owner, Go-generated enrolled capability pipe, scratch-built compile-time-only native frontend, and a unique test named pipe. The receiver hashes the 32 delivered bytes and matches the digest captured by enrollment.
- The native test route exists only under `CST_TEST_FRONTEND_E2E`. Normal production rebuild excludes it; the production image remained byte-identical.

## Changed paths owned by this correction

- `internal/daemon/launch_capability_windows.go`
- `internal/daemon/cst_direct_frontend_e2e_windows_test.go`
- `servers/electromagnetics-mcp/native/cst-runtime/{mcphub_cst_runtime.c,build_test_frontend.ps1,cst-native-runtime-manifest-v1.json}`
- `servers/electromagnetics-mcp/src/mcphub_em_mcp/{cst_saved_field_endpoints.py,cst_saved_field_policy.py,cst_saved_field_broker_client_windows.py,cst_saved_field_daemon_service_windows.py,cst_saved_field_broker_worker_protocol.py,cst_saved_field_broker_worker.py}`
- `servers/electromagnetics-mcp/tests/{test_cst_saved_field_w0_gap_baseline.py,test_cst_saved_field_broker_worker.py,test_cst_saved_field_integration.py,test_cst_saved_field_t15_production_composition.py}`

## Verification

| Check | Fresh result |
|---|---|
| Full frozen Python | PASS: 635 tests, known Pydantic warning only. |
| Ruff check / format check | PASS / PASS; 78 files formatted. |
| `go test ./internal/daemon -count=1` | PASS in 38.313 s, including the real Windows E2E. |
| Native clean two-build + independent PE/manifest verifier | PASS; production image SHA-256 stayed `38D87C50F716E334F89628D4F35604534C3A35BDCF35F378F1D939137BAB89E3`. |
| `git diff --check` | PASS. |
| Git index | Preserved empty (`cached_count=0`). |
| Post-edit CodeGraph MCP | Fresh for every corrected target after watcher recheck; one unrelated file remained pending elsewhere. It shows one endpoint constant owner and current Go zeroization owner. |

## Contract statement

Public MCP tools, HTTP/RPC status codes, authorization, pagination, persistence, retry and timeout contracts are unchanged. The private broker-worker response gains one mandatory closed `capability_receipt`; all current consumers were updated. No outbound remote call was added. The test named pipe is bounded by a five-second context and exists only in the Windows test process.

## Gate and QA handoff

Gate: `PASS`

QA should rerun W7 on the exact combined candidate. This package closes only the four in-scope findings; the nine separately routed full-Go failures remain outside this artifact and receive no PASS claim here.

## Terms and Abbreviations

- CST: Computer Simulation Technology.
- E2E: end-to-end.
- MCP: Model Context Protocol.
- PE: Portable Executable.
- RPC: Remote Procedure Call.
- W7: working-candidate regression and immutable-candidate gate.
