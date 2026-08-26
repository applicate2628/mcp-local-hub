# T00 Backend Implementation Package — Baseline, Ownership, and RED Harness

Gate: **PASS**

Execution role: `$backend-engineer` under the main-conversation `$lead`.
Scope: T00 only. Production behavior changed: **none**.
Baseline: `5ff268dc13b2be9ca9500b5441634f0594538b94` (`master`).

## Accepted input check

The current files matched all immutable dispatch receipts before execution:

| Input | Observed SHA-256 |
|---|---|
| `design.md` | `AFABC3C001169D5C571D7319EA2C751CDD228E46B335C9630C0516F6EBAE6DC9` |
| accepted decision | `18307E933D393BBD0C6B0396F47FE6AAFB0C5AE94CE39E395F8EE948371BE92A` |
| `architecture-review.md` | `18499E40CC82236F9EA256F988BB7F48342806240A1EC710E7739978BCF7601E` |
| `security-constraints.md` | `A0F0D2CEF3BA016D4E4E607755D643F5415F2D56F2C9BF99E848481498A81A12` |
| `security-review-design.md` | `BFC9A0F36F7FF0E07ADE7E4DC79D507FBB1BBBDCD548713B263F7BC3FF14B84A` |
| `plan.md` | `8DD78E5B6EC48ED7671403C98695B4F364C8E13430C0FE49396E426B76CAE3EA` |

CodeGraph was probed before source navigation and returned current on-disk source with no stale-file or disabled-sync banner.

## T00-AC01 ownership inventory

Selection is based on executable ownership and call edges, not filename age.

| Owner | Current evidence and T00 conclusion |
|---|---|
| Go host configuration | `internal/daemon/host.go:27-45` owns `HostConfig`; it currently has command, arguments, environment, unset-environment, working directory, and log path but no launch-capability seam. |
| Actual stdio spawn | `internal/daemon/host.go:93-104` owns `StdioHost`; `internal/daemon/host.go:271-288` creates the child and its environment. |
| Go composition | `internal/cli/daemon.go:280-302` constructs `HostConfig`, `StdioHost`, and starts it for `stdio-bridge`. |
| Supervisor lock identity | `internal/api/supervisor_lock.go:54-99` owns `SupervisorLockOwner{PID, StartedAt}` and writes it only after lock acquisition. |
| Supervisor status client | `internal/api/supervisor_ipc_status_client.go:44-90` reads the lock owner, resolves the canonical endpoint, and dials with a five-second bound; `internal/api/supervisor_ipc_status_client.go:139-155` owns current status rows. |
| Supervisor status authentication | `internal/api/supervisor_ipc_status_client.go:203-221` validates hello against `SupervisorLockOwner`. The CST-only opcode is absent, which the RED harness reports. |
| Windows status endpoint/DACL | `internal/api/supervisor_ipc_address_windows.go:51-95` owns the SID-keyed endpoint; `internal/cli/supervise_ipc_windows.go:72-80,114-117` owns `winio.ListenPipe`, configured security, and effective-descriptor readback. |
| Python frontend composition | `servers/electromagnetics-mcp/src/mcphub_em_mcp/cst.py:640-680` restart-loads policy and composes the optional tool. It currently imports/constructs the broker client directly, an obsolete route under the accepted design. |
| Policy | `cst_saved_field_policy.py:110,1008` owns platform validation and restart snapshot loading. |
| Application/neutral port | `cst_saved_field.py:366` owns the application transaction; `cst_saved_field_port.py:10-169` owns neutral records and application port. |
| Broker client/protocol | `cst_saved_field_broker_client_windows.py:190` owns the current client; `cst_saved_field_broker_protocol.py` owns daemon/broker frames. |
| Worker/protocol | `cst_saved_field_broker_worker.py:30,249` owns application and entry point; `cst_saved_field_broker_worker_protocol.py` owns broker/worker frames. |
| Containment | `cst_saved_field_containment_windows.py:344` owns contained invocation. |
| Transfer/workspace | `cst_saved_field_transfer.py:672` owns workspace lease creation. |
| Vendor isolation/broker service candidate | `cst_saved_field_vendor_isolation_windows.py:241` owns the earlier in-process broker service candidate; the accepted standalone broker service entry point is absent. |
| Missing accepted owners | `cst_saved_field_hub_enrollment_windows.py`, `cst_saved_field_frontend_protocol.py`, `cst_saved_field_daemon_client_windows.py`, `cst_saved_field_daemon_service_windows.py`, `cst_saved_field_broker_service_windows.py`, `internal/daemon/launch_capability_windows.go`, and `internal/api/hub_enrollment_client_windows.go` are absent and named by the RED receipt. |

## T00-AC02 topology RED receipt

Named regression guard:

```text
uv run --frozen --python 3.13 pytest -q tests/test_cst_saved_field_t00_topology.py::test_t00_accepted_three_endpoint_four_schema_topology
```

Expected: fail with stable prefix `T00-TOPOLOGY-RED` until source exposes exactly the three accepted sampler endpoints, four schema owners, one pre-existing supervisor endpoint plus CST-only status opcode, and the fixed enrollment/application routes.

Observed: **RED**, exit `1`, one failed test in `2.0s`. The failure enumerated:

- missing enrollment and frontend endpoints; broker endpoint was the sole observed sampler endpoint;
- missing `HubEnrollmentProtocolV1` and `FrontendDaemonProtocolV1` owners;
- earlier broker/worker protocol files do not yet declare their accepted protocol-owner tokens;
- missing `GET_CURRENT_CST_TASK_IDENTITY_V1`;
- missing Go launch-capability and enrollment-client owners;
- `cst.py` has no daemon-client edge and retains direct broker-client/protocol edges;
- missing standalone SCM daemon and broker service owners.

This is the required absent/wrong-behavior falsifier. T00 does not alter production to turn the later topology green.

## T00-AC03 guard inventory

Each guard is bound now; later phases must retain the deterministic name or record an reviewed equivalent.

| Guard | Owner / phase | Deterministic falsifier and failure oracle |
|---|---|---|
| Existing-wire compatibility | integration / T05,T23 | `test_existing_six_semantic_schemas_and_cst_function_identities_are_frozen` plus `test_real_stdio_handshake`; any schema/body hash, tool set, error-byte corpus, or stdio inventory mismatch fails. |
| Solve-path preservation | application/vendor / T10,T20 | `test_saved_field_solve_path_preservation`; any solve/history/mesh/1D call, save, or foreign-process effect fails. |
| Validation-channel | MCP publisher / T05,T13 | existing-six error-byte corpus plus `test_saved_field_validation_channel`; any existing error drift or raw seventh-tool input in the public error fails. |
| No-job-edge | containment / T08 | `test_saved_field_no_job_edge`; AST/import inventory fails on `jobs.py` or a non-broker Job owner. |
| Foreign-process | containment / T08,T20 | `test_saved_field_foreign_process_guard`; synthetic/target PID census fails on foreign terminate/join/wait/attribution. |
| Complete-manifest transfer | transfer / T09 | `test_saved_field_complete_manifest_transfer`; missing/extra/duplicate/default-stream/identity/hash/partial-destination case fails. |
| Trusted-root injection | broker policy / T07,T09 | `test_saved_field_trusted_root_injection`; ambient read or missing/remote/reparse/non-owned root acceptance fails. |
| Workspace transaction | transfer/vendor / T09,T10 | `test_saved_field_workspace_transaction_all_returns`; any child/handle/file or incomplete rollback receipt fails. |
| Neutral port | application / T03,T10 | `test_saved_field_neutral_port_dependencies`; CST/Windows import or reverse frontend/application edge fails. |
| Vendor record | vendor / T10 | `test_saved_field_vendor_record_matrix`; malformed, ambiguous, non-finite, over-limit record acceptance fails. |
| Finite budget | protocol/admission/containment / T03,T06,T08 | `test_saved_field_finite_budget_matrix`; limit bypass, deadline rebase, late success, or unbounded frame/stream/waiter fails. |
| Settlement order | worker/vendor / T10 | `test_saved_field_settlement_order_all_returns`; deletion before lease/vendor/output settlement fails. |
| Contained duration | containment / T08 | `test_saved_field_contained_duration_matrix`; surviving worker after deadline/cancel/crash/stop/Job close fails. |
| In-server authority | frontend/daemon/broker / T05,T06,T07 | `test_saved_field_in_server_authority_matrix`; caller/frontend digest/path/revision/catalog granting authority fails. |
| Protocol drift | protocol owners / T03,T11 | `test_saved_field_protocol_drift_matrix`; noncanonical/unknown/trailing/mismatched correlation/hash/deadline acceptance fails. |
| Atomic containment | containment / T08 | `test_saved_field_atomic_containment`; first-instruction proof before suspended assignment to Job fails. |
| Sole Job handle | containment / T08 | `test_saved_field_sole_job_handle_all_returns`; second owner/handle or unsafe close order fails. |
| Quarantine linearization | daemon / T06,T11 | `test_saved_field_quarantine_linearization`; successor admission before failed settlement terminalization fails. |
| Namespace identity | transfer/path / T09 | `test_saved_field_namespace_identity_matrix`; DOS/device/UNC/ADS/8.3/reparse/hardlink/case alias acceptance fails. |
| Vendor-byte capability continuity | lease/vendor / T09,T10 | `test_saved_field_vendor_byte_capability_continuity`; CST path reopen without the one live lease fails. |
| MCP-boundary budget | publisher / T05,T13 | `test_saved_field_mcp_boundary_budget`; oversized/late/non-finite/raw internal result publication fails. |
| Canary redaction | all public/diagnostic boundaries / T05,T13 | `test_saved_field_canary_redaction_all_returns`; capability/path/SID/policy/source/raw exception canary occurrence fails. |
| Publication | integration/human / T14,T24 | immutable candidate/pin/hash plus publication-safety receipt; mismatch, leak, dirty history, or missing human approval fails. |
| Supervisor kernel binding/status-only authorization | Go status owner / T01 | `TestSupervisorCstIdentity*` and `TestSupervisorStatusAuthorization*`; any wrong PID/time/SID/session/token/image/opcode/target acceptance or mutable state change fails. |
| Enrollment lifecycle/handle inventory | Go spawn + enrollment / T02,T04 | `Test*LaunchCapability`, `Test*EnrollmentClient`, `Test*HandleList`; reuse/strand/extra inherited handle/env secret/missing EOF-zero-close-cancel fails. |
| Three-descriptor readback | service descriptors / T04,T07,T19 | `test_saved_field_three_descriptor_readback`; any count/name/DACL/SACL/order/readback mismatch fails. |
| Split-receipt event order | frontend/daemon / T05,T06,T11 | `test_saved_field_split_receipt_event_order`; cross-owner self-attestation or early settlement fails. |

## T00-AC04 baseline receipts

| Surface | Command | Result |
|---|---|---|
| Go full baseline, attempt 1 | `go test ./...` | **UNVERIFIED**: outer command timed out at `124s`, exit `124`, no terminal test output. |
| Go full baseline, attempt 2 | `go test ./...` | **UNVERIFIED**: bounded outer command timed out at `304s`, exit `124`, no terminal test output. Exact owned `go.exe` and `cli.test.exe` survivors were identified by PID/creation/command line, stopped, and re-probed absent. |
| Go full baseline, Lead rerun | `go test ./... -count=1 -timeout 12m` | Baseline captured: exit `1` in `378.7s`. Truncated output named routing anchor failures and one CLI tempdir-cleanup failure; the bounded differential below resolved the exact participants and baseline relation. |
| Go focused ownership/status | `go test ./internal/daemon ./internal/api -run 'TestSupervisorIntentLockOwnershipProductionInventory|TestDialSupervisorIPCStatus_HappyPath|TestNewStdioHost' -count=1 -timeout=45s` | PASS, exit `0`, `21.5s`; daemon had no matching test, API passed. This is supplemental and does not replace the required full suite. |
| Python full baseline | `uv run --frozen --python 3.13 pytest -q` | PASS, exit `0`, **508 tests**, `17.6s`; one pre-existing Pydantic forward-reference warning. Collection after adding the RED harness is 509 tests. |
| Existing-six focused | `pytest -q tests/test_servers.py tests/test_stdio.py ...::test_existing_six...` | PASS, exit `0`, **7 tests**, `6.8s`. |
| Ruff | `uv run --frozen --python 3.13 ruff check .` | PASS, exit `0`, `1.7s`. |
| Format | `uv run --frozen --python 3.13 ruff format --check .` | PASS, exit `0`, 54 files, `0.8s`. |

Existing-six canonical schema SHA-256 values remain:

| Tool | Schema SHA-256 | Empty-request error SHA-256 / bytes |
|---|---|---|
| `hfss_solve` | `ea92c1932d3617ccba7d7632bd080a06f77e176c8d4007c8436fae3f07061d97` | `38ba50d3c280eade5c2753d8dabf8f3e515d5a98a2b69f5ae1eeaa332eb20c02` / 91 |
| `hfss_export_mesh` | `1b679fae82efddb4c4bc23a13397425c75cd99707a87046c987ea55028189822` | `89925555e5e3abdd4fc564a8085c0ffb273da0911117e40d73da67f53165b571` / 236 |
| `hfss_export_sparams` | `09387c3aef2bfe742fac5b3789eb890951972c8bd3bc19153cac0dec39f46f2c` | `4ae0d6f222750b19081602281c2e7e996a66e1a0993aebc45a1b0d3dba5dc63c` / 242 |
| `cst_solve` | `f4a211e8a5a08751d1af59f5c523d73fb008f12267a6cbfc2e220163afcc293e` | `19cd5129e7c666747eb2fee84b7f9723cf9a9598114fbc48ff74a475b55fbbfc` / 90 |
| `cst_export_mesh` | `3bceb5355a44e3d76365bbdfa83df4e3d81259cf4cc9972698ed368606ff1eae` | `3c1b0f1b72a64a2599a70ffab2819336bbe17e047c7130fee43243647257b733` / 234 |
| `cst_export_results` | `8d096418c564189962342ccb08effe101167996f80fed0a74412ae4a6b095bee` | `ce3d065ec5a7a9a1f238ef97fd442e60a1db9283b5d15e583bcf862404e708a0` / 240 |

The focused real stdio handshake passed for both servers and observed exactly the three HFSS plus three CST tools. Success byte fixtures require vendor/job execution and remain target-bound; T00 recorded deterministic schema, validation-error bytes, and stdio frames without invoking CST/HFSS.

### Full-Go differential triage

One focused rerun, and no second broad run, resolved the truncated full-suite output:

```text
go test ./internal/api/lsp_routing ./internal/api/serena_routing ./internal/cli \
  -run '^(TestRefreshCapturesEntriesBeforeRegistryRelease|TestSuperviseCommand_SweepsOldBinariesOnStartup)$' \
  -count=1 -timeout=4m -v
```

Observed exit `1` in `3.0s`:

| Package-qualified test | Focused result | Immutable-HEAD classification |
|---|---|---|
| `internal/api/lsp_routing.TestRefreshCapturesEntriesBeforeRegistryRelease` | FAIL: `capture=-1 release=12527` | Pre-existing test/source mismatch. `resolver_test.go:27-30` searches the literal `entries = r.reg.LSPEntries()`, while unchanged `resolver.go` uses the declaration `entries := r.reg.LSPEntries()`. |
| `internal/api/serena_routing.TestRefreshCapturesEntriesBeforeRegistryRelease` | FAIL: `capture=-1 release=9947` | Same pre-existing mismatch. `resolver_test.go:27-30` searches `entries = r.reg.SerenaEntries()`, while unchanged `resolver.go:237` uses `entries := r.reg.SerenaEntries()`. |
| `internal/cli.TestSuperviseCommand_SweepsOldBinariesOnStartup` | PASS in `0.13s` | Broad-suite tempdir-cleanup failure did not reproduce; it is a baseline flake/interaction, not a T00 regression. No stronger causal classification is asserted. |

`git diff --exit-code HEAD` returned `0` for both routing `resolver.go`/`resolver_test.go` pairs and `internal/cli/supervise_test.go`. The routing tests obtain their source path from `runtime.Caller`, read only their sibling `resolver.go`, and fail before exercising port allocation. Thus this run executed the exact immutable-HEAD test/source bytes for the failing predicate even though the unrelated worktree retained another tracked modification.

The sole unrelated tracked production diff, `internal/api/port_alloc_excluded_windows.go`, changes construction of the `netsh ... excludedportrange` command to call `process.NoConsole`. It cannot cause either observed routing failure: those failures are the deterministic result of `bytes.Index` over unchanged sibling source, and CodeGraph found no port-allocation edge in the test predicate. It also did not cause the named CLI failure in the focused run, where that test passed. Classification: **not affected** for all three named participants.

## T00-AC05 diff classification

Pre-T00 admitted baseline dirt:

- `internal/api/port_alloc_excluded_windows.go` — unrelated modified production path; excluded and untouched;
- `work-items/README.md` and numerous work-item records — unrelated lifecycle activity; excluded and untouched;
- the CST saved-field work-item artifacts and earlier sampler candidate are untracked relative to baseline and are treated as accepted-input/historical candidate surfaces, not silently clean baseline.

T00-owned changes are only:

- `servers/electromagnetics-mcp/tests/test_cst_saved_field_t00_topology.py`;
- this `implementation-t00.md`;
- the bounded T00 checkpoint in `status.md`.

No index, commit, push, live CST, Service Control Manager, hub, fleet, deploy, registration, or manifest mutation occurred.

## Backend contract notes

- Wire/API before/after: N/A; T00 changes no endpoint, handler, request, response, status code, field, ordering, pagination, or named consumer.
- Authorization: N/A; no route or handler was added.
- Outbound calls/timeouts/retries: N/A; no production call site was added or changed.
- Query cardinality/index expectation: N/A; no data query was added.
- Resource ownership: the timed-out second Go test tree was the only owned residual process; exact parent/child identities were settled and absence was re-probed.

## Receiving-side echo

| Required echo | Result |
|---|---|
| Named regression guard | Expected `T00-TOPOLOGY-RED`; observed exit `1`, one deterministic failure with the exact missing/forbidden topology participants listed above. |
| Existing six remain unchanged | **Verified** by 7-test schema/body/stdio guard and the six schema/error hashes. |
| T00 has no production behavior change | **Verified** by changed-path classification: only a test and canonical work-item artifacts are T00-owned. |
| No live SCM/CST/hub/fleet/deploy/registration mutation | **Verified** from executed-command inventory; all commands were source reads, hashes, test/static checks, process census, or exact owned-test cleanup. |
| Full Go baseline relation to T00 | **Verified non-regression**. The terminal full run is not green, but its two reproducible failures are stale literal-anchor tests over immutable-HEAD files; the third named failure did not reproduce. None is caused by the test/doc/status-only T00 delta or the unrelated port-allocation diff. |
| Defect-class audit | The dispatch cited no single defect class; `not-triggered`. The topology falsifier nevertheless classifies every accepted endpoint, schema owner, supervisor endpoint/opcode, Go spawn owner, and Python route participant as present/missing/forbidden. |

## Gate rationale and rollback

T00-AC01, AC02, AC03, AC04, and AC05 are evidenced. The plan requires a captured baseline and a regression classification; it does not authorize fixing unrelated immutable-HEAD test drift in T00. The terminal Go baseline is non-green but now fully classified for the named failures, while the T00-owned topology test is intentionally RED and all T00-owned static checks are green. **Gate PASS** as a non-regression baseline; the two stale routing anchor tests and non-reproduced CLI cleanup interaction remain adjacent baseline debt, not T01 prerequisites.

Rollback group: delete only `tests/test_cst_saved_field_t00_topology.py` and this T00 evidence/status delta. No production rollback exists because production was not changed.
