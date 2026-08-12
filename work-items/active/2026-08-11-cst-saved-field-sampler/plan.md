# Canonical Implementation Plan — CST Saved-Field Three-Endpoint Topology

Status: implementation-ready, default-off
Plan owner: `$planner`
Next execution owner: `$lead` assigning the narrowest implementation specialist for T00
Scope owner: accepted CST saved-field authority and containment design

## Accepted immutable inputs

| Input | SHA-256 | Status in this plan |
|---|---|---|
| `design.md` | `AFABC3C001169D5C571D7319EA2C751CDD228E46B335C9630C0516F6EBAE6DC9` | Normative architecture and Change-Surface Contract |
| `work-items/decisions/2026-08-12-cst-saved-field-authority-containment.md` | `18307E933D393BBD0C6B0396F47FE6AAFB0C5AE94CE39E395F8EE948371BE92A` | Accepted decision |
| `architecture-review.md` | `18499E40CC82236F9EA256F988BB7F48342806240A1EC710E7739978BCF7601E` | Accepted architecture PASS |
| `security-constraints.md` | `A0F0D2CEF3BA016D4E4E607755D643F5415F2D56F2C9BF99E848481498A81A12` | Accepted security constraints PASS |
| `security-review-design.md` | `BFC9A0F36F7FF0E07ADE7E4DC79D507FBB1BBBDCD548713B263F7BC3FF14B84A` | Independent design security PASS |

Any material change to an accepted input invalidates this plan and requires planning again from the changed artifact. CodeGraph was not used, so this plan makes no CodeGraph freshness claim.

## Outcome and fixed topology

Deliver one default-off seventh CST tool through the only accepted route:

1. pre-spawn: supervisor-tracked CST `StdioHost -> HubEnrollmentProtocolV1 -> SCM McpLocalHubCstDaemon`;
2. application: `hub -> existing hub-spawned mcphub-cst-mcp stdio frontend -> FrontendDaemonProtocolV1 -> SCM daemon -> BrokerProtocolV1 -> SCM McpLocalHubCstVendorBroker -> BrokerWorkerProtocolV1 -> contained worker -> CST`.

There are exactly three new sampler named-pipe endpoints: enrollment, frontend, and broker. There are exactly four closed schemas: `HubEnrollmentProtocolV1`, `FrontendDaemonProtocolV1`, `BrokerProtocolV1`, and `BrokerWorkerProtocolV1`. The pre-existing supervisor IPC endpoint is not a fourth sampler endpoint; it receives only the bounded status-only opcode `GET_CURRENT_CST_TASK_IDENTITY_V1` with implicit exact task `cst`.

## Change-Surface Contract carried forward

| Class | Allowed surface |
|---|---|
| Go spawn and status owners | `internal/cli/daemon.go`; `internal/daemon/host.go`; new `internal/daemon/launch_capability_windows.go`; new `internal/api/hub_enrollment_client_windows.go`; bounded supervisor status producer/client and Windows listener DACL files |
| Python frontend and services | `servers/electromagnetics-mcp/src/mcphub_em_mcp/cst.py`; new frontend/enrollment/daemon/broker protocol and service modules named by the design |
| Python worker and domain | New policy, containment, vendor-isolation, broker-worker, application, neutral port, and vendor modules named by the design; additive sampler-only changes in `safety.py` and `strict_fastmcp.py` |
| Tests and docs | Focused sampler tests; minimal `tests/test_servers.py` and `tests/test_stdio.py`; CST README/API docs; immutable `servers/cst/manifest.yaml` pin only after all target checkpoints pass |
| Protected | Existing six CST tool signatures, schemas, local call paths, errors and outputs; `jobs.py`; `cst_results.py`; HFSS source/tests/manifest; CST solve/history/mesh/1D export behavior; published artifact schemas; retained bundles; unrelated worktree changes; hub routing/filter semantics; Go `internal/process` |

The plan does not authorize a package dependency, a direct daemon-to-CST route, a frontend-to-broker route, a same-principal path-write fallback, a test-only production branch, a detached daemon route, a second Job owner, a fourth sampler pipe, or any live Service Control Manager (SCM) mutation before T19.

## Dependency and execution law

- T00 through T25 are serial. A phase begins only after every acceptance criterion (AC) in its predecessor is green and its evidence is recorded in the owning implementation artifact.
- Go source/tests (`internal/cli`, `internal/daemon`, `internal/api`, `go test ./...`) and Python source/tests (`servers/electromagnetics-mcp`, its `uv`/pytest/Ruff surfaces) are shared observed surfaces and must never execute or mutate in parallel.
- T11 is the single integration-owner phase. Before it, each phase may change only its declared owner surface. After it, cross-owner failures return to the phase that owns the violated invariant; fixes are not layered in integration code.
- T15–T18 are independent, sequential read/review lanes against one immutable candidate. A revision invalidates all later review results and returns to the owning implementation phase.
- T19–T23 are serial target lanes because they share SCM namespaces, Windows identities, CST installation/license/process state, output roots, and cleanup evidence.
- No seventh-tool catalog registration, manifest pin, product publication, release, or deployment is permitted until T19–T23 all PASS. Target harnesses before then must use disposable, default-off, non-catalog entrypoints.
- P18 is the only phase allowed to provision disposable SCM services, and it must restore exact pre-state. T24 may update the immutable pin only after all target checkpoints pass. T25 is the only deployment phase.
- Every phase is RED-first: add or select the named falsifier, demonstrate that it fails for the absent/wrong behavior, then implement and demonstrate green. A test that was already green without exercising the new invariant is not RED evidence.

## Named guards and falsifier allocation

| Guard name copied from accepted design | Primary phase | Required falsifier |
|---|---:|---|
| Existing-wire compatibility guard | T05, T23 | Before/after six-tool schema, bytes, errors, call-route, stdio framing mismatch |
| Solve-path preservation guard | T10, T20 | Any solve/history/mesh/1D call, pre-existing process impact, or save outside isolated worker |
| Validation-channel guard | T05, T13 | Existing-tool error text changes or raw seventh-tool input escapes fixed safe error |
| No-job-edge guard | T08 | Import/runtime edge to `jobs.py` or any non-broker Job owner |
| Foreign-process guard | T08, T20 | Foreign CST PID terminated, joined, attributed, or waited on |
| Complete-manifest transfer guard | T09 | Missing/extra/duplicate row, non-default stream, identity/hash drift, partial destination |
| Trusted-root injection guard | T07, T09 | New sampler frontend/daemon/application/vendor reads ambient output root, or broker accepts a missing/remote/reparse/non-owned root; existing-six legacy `safety.py` use remains unchanged (`design.md:273-277,1314-1316`) |
| Workspace-transaction guard | T09, T10 | Any create/copy/acquire return leaks a child, handle, file, or incomplete rollback receipt |
| Neutral-port guard | T03, T10 | Concrete CST/Windows import in neutral port or reverse dependency into frontend/application |
| Vendor-record guard | T10 | Malformed/ambiguous/non-finite vendor record accepted |
| Finite-budget guard | T03, T06, T08 | Limit bypass, rebased deadline, late success, unbounded frame/stream/waiter |
| Settlement-order guard | T10 | Workspace/session deletion precedes lease/vendor/output settlement |
| Contained-duration guard | T08 | Worker survives deadline, cancellation, broker crash, service stop, or Job close |
| In-server authority guard | T05, T06, T07 | Caller/frontend digest, path, policy revision, or catalog state grants authority |
| Protocol-drift guard | T03, T11 | Noncanonical JSON, unknown field, trailing bytes, correlation/hash/deadline mismatch |
| Atomic-containment guard | T08 | Worker request/user code begins without atomic `PROC_THREAD_ATTRIBUTE_JOB_LIST` membership at creation, or a suspended/post-create assignment gap exists (`design.md:1095-1107,1325-1327,1471,1594-1596`) |
| Sole-Job-handle guard | T08 | More than broker-owned Job handle or close order permits breakaway |
| Quarantine-linearization guard | T06, T11 | Failed broker/daemon settlement releases admission or admits a successor early |
| Namespace-identity guard | T09 | DOS/device/UNC/ADS/8.3/reparse/hardlink/case alias crosses identity policy |
| Vendor-byte capability-continuity guard | T09, T10 | CST reopens a path not held by the one `AuthorizedVendorPathLease` |
| MCP-boundary budget guard | T05, T13 | Oversized/late/non-finite public `TextContent` or raw internal result published |
| Canary-redaction guard | T05, T13 | Secret capability, path, SID, policy bytes, source bytes, or raw exception reaches output/log |
| Publication guard | T14, T24 | Candidate/pin mismatch, leak-scan failure, missing human approval, or dirty history |
| Supervisor kernel-binding/status-only authorization guard | T01 | Wrong server/client PID, creation time, SID, session, token, image, opcode, target, or mutable task state accepted |
| Enrollment lifecycle/handle-inventory guard | T02, T04 | Capability reuse/strand, inherited extra handle, environment secret, missing EOF/zero/close/cancel |
| Three-descriptor readback guard | T04, T07, T19 | Descriptor count/name/DACL/SACL/order/readback differs from exactly enrollment/frontend/broker |
| Split-receipt event-order guard | T05, T06, T11 | Daemon receipt asserts frontend EOF/close, frontend receipt asserts daemon close, or either settlement boundary fires early |

## Serial RED-first phases

### T00 — Baseline, ownership inventory, and RED harness

Owner: integration owner appointed by `$lead`. Depends on accepted inputs only. Mutation scope: focused test scaffolds and implementation evidence artifact; no production behavior.

| AC ID | Observable acceptance criterion |
|---|---|
| T00-AC01 | Fresh inventory cites actual `HostConfig`, `StdioHost`, `internal/cli/daemon.go`, supervisor lock/status/DACL owners and every Python composition owner; no planned path is selected by name or recency alone. |
| T00-AC02 | A topology test is RED unless it observes exactly three sampler endpoints, four protocol schemas, one pre-existing supervisor status endpoint, and the two fixed routes above. |
| T00-AC03 | Guard inventory names every guard in this plan and binds each to one deterministic test or target probe with an owner and failure oracle. |
| T00-AC04 | Baseline captures existing-six schemas, representative byte responses/errors, stdio frames, Go tests, Python tests, Ruff and format without changing source. |
| T00-AC05 | `git diff --name-only` against the admitted baseline is classified against the Change-Surface Contract; unrelated dirty paths are recorded and excluded. |

Verify: `go test ./...`; from `servers/electromagnetics-mcp`, `uv run --frozen --python 3.13 pytest -q`, `uv run --frozen --python 3.13 ruff check .`, and `uv run --frozen --python 3.13 ruff format --check .`. Evidence: command, exit code, test count, RED test names, baseline commit and protected-path inventory. Revert group: T00 test scaffolds only.

### T01 — Supervisor IPC bounded CST identity status

Owner: Go backend engineer. Depends on T00. Scope: existing supervisor status producer/client, Windows address/listener authorization and `internal/cli/daemon.go` composition only.

| AC ID | Observable acceptance criterion |
|---|---|
| T01-AC01 | RED matrix rejects `GET_CURRENT_CST_TASK_IDENTITY_V1` before implementation and later admits it only for the exact daemon numeric service SID with implicit task `cst`. |
| T01-AC02 | Client kernel-binds supervisor PID, creation time, token/session and canonical installed image to `SupervisorLockOwner`; server independently binds daemon SID/token/session/integrity/image before dispatch. |
| T01-AC03 | Generic status, control, respawn, reconcile, exit, explicit/other target, malformed and replayed requests deny before generic dispatch and leave supervisor task state byte-for-byte unchanged. |
| T01-AC04 | Response contains only the exact current CST task identity fields required by enrollment; absence, ambiguity, stale generation or mismatch fails closed. |
| T01-AC05 | Existing supervisor opcodes, clients and status schemas retain their prior contract under `go test ./...`. |

Verify: focused `go test ./internal/api -run 'TestSupervisorCstIdentity|TestSupervisorStatusAuthorization' -count=1`, then `go test ./...`. Evidence: allow/deny table and before/after state digest. Revert group: T01 Go status extension.

### T02 — Go StdioHost launch capability and enrollment client

Owner: Go backend engineer. Depends on T01. Scope: `internal/daemon/host.go`, new `internal/daemon/launch_capability_windows.go`, new `internal/api/hub_enrollment_client_windows.go`, `internal/cli/daemon.go`.

| AC ID | Observable acceptance criterion |
|---|---|
| T02-AC01 | RED handle-inventory test fails until the actual `StdioHost` spawn owner generates exactly 32 bytes using CNG `BCryptGenRandom` system-preferred randomness and enrolls the exact SHA-256 digest before spawn. |
| T02-AC02 | Capability state is exactly `ISSUED -> ENROLLED -> CONSUMED|CANCELLED`; ACK/flush/close leaves `ENROLLED`, valid exact read+EOF+challenge consumes, and every start/write/read/exit/expiry/shutdown/restart failure cancels. |
| T02-AC03 | `PROC_THREAD_ATTRIBUTE_HANDLE_LIST` contains stdin, stdout, stderr and the capability read handle only; write side and all unrelated handles are non-inheritable. |
| T02-AC04 | Environment carries only decimal `MCPHUB_CST_LAUNCH_HANDLE`; capability bytes never enter env/argv/logs, child reads exactly 32 bytes plus EOF, both processes close handles, and buffers are zeroed with `SecureZeroMemory`. |
| T02-AC05 | Non-CST StdioHost launches and all existing manifest/route/port/process behavior are unchanged. |

Verify: focused `go test ./internal/daemon ./internal/api -run 'Test.*LaunchCapability|Test.*EnrollmentClient|Test.*HandleList' -count=1`, then `go test ./...`. Evidence: deterministic injected all-return table and inherited-handle inventory. Revert group: T02 capability/enrollment client.

### T03 — Neutral contracts, four schemas, policy and budgets

Owner: Python backend engineer. Depends on T02. Scope: new neutral port, policy, frontend/broker/worker protocol value modules and focused tests; no service or CST call.

| AC ID | Observable acceptance criterion |
|---|---|
| T03-AC01 | RED schema tests fail until all four V1 protocols use bounded closed canonical JSON, exact enums/fields, one correlation/request hash/deadline chain, no unknown/trailing data, and safe destructive-default polarity. |
| T03-AC02 | Policy validation requires absolute local owner/access-controlled path, immutable unique `entry_id`, revision, exact endpoint descriptors and manifest-v2 identity; absent/invalid/disabled is default-off. |
| T03-AC03 | `AbsoluteInvocationBudget` is one unchanged integer QPC triple `{qpc_frequency, admitted_tick, deadline_tick=admitted_tick+60*qpc_frequency}`; no receiver rebases it and only cleanup gets a distinct 10-second deadline. |
| T03-AC04 | Neutral port owns immutable request/vendor/acquisition/batch/failure/receipt types including `AuthorizedVendorPathLease` and imports no CST or Windows implementation. |
| T03-AC05 | Protocol ceiling, nonce, replay, correlation, framing, deadline, policy hash and malformed-input matrices are RED before their owner validators exist and green afterward. |

Verify: focused `pytest -q` for policy, wire-schema, budget and protocol tests, then Ruff/format. Evidence: exact schema snapshots, import graph and boundary matrices. Revert group: T03 neutral/protocol/policy modules.

### T04 — HubEnrollmentProtocolV1 server and enrollment descriptor

Owner: Python backend engineer. Depends on T03. Scope: new `cst_saved_field_hub_enrollment_windows.py` and daemon enrollment composition tests; no live SCM.

| AC ID | Observable acceptance criterion |
|---|---|
| T04-AC01 | RED authentication matrix fails until daemon queries supervisor IPC independently and matches peer PID, kernel creation time, canonical installed image/package, parent, token/session and current generation/task row. |
| T04-AC02 | Channel nonce and launch-capability ledger are independent; one bounded digest enroll/cancel/receipt sequence cannot consume, duplicate, replay or strand authority. |
| T04-AC03 | Every success, ACK loss, post-ACK failure, fresh authenticated cancel, expiry, child exit, disconnect, service stop, shutdown and restart has a terminal state and complete handle/nonce cleanup. |
| T04-AC04 | Enrollment endpoint descriptor is fixed, numeric-SID protected, local-only, High-integrity audited, read back in exact order, and is one of exactly three sampler descriptors. |
| T04-AC05 | No detached daemon, unauthenticated child, test-only bypass, digest-only authorization or direct frontend enrollment fallback exists. |

Verify: focused enrollment protocol/state/auth/descriptor pytest tests using Win32-safe synthetic fakes. Evidence: lifecycle trace, identity mismatch matrix, descriptor readback. Revert group: T04 enrollment server.

### T05 — Thin CST frontend and FrontendDaemonProtocolV1

Owner: Python frontend engineer. Depends on T04. Scope: `cst.py`, new frontend protocol/client, additive `strict_fastmcp.py`, minimal server/stdio tests.

| AC ID | Observable acceptance criterion |
|---|---|
| T05-AC01 | RED six-tool oracle detects any schema/byte/error/call-route/stdio change; after implementation all existing six remain byte- and shape-compatible on their local paths. |
| T05-AC02 | Seventh tool alone reads the inherited capability once, loads only enabled entry inventory/endpoint proof, sends non-authoritative `entry_id`, and uses `WindowsDaemonClient`; it imports no broker, source, worker, vendor or containment module. |
| T05-AC03 | Frontend challenge ledger provides one-use correlation/capability proof with cancellation on every return and no direct daemon authority claim. |
| T05-AC04 | `FrontendTransportReceiptV1` contains only frontend-observed response/terminal completion, EOF-or-cancel and client-handle close; only its complete local conjunction plus unchanged deadline permits publication. |
| T05-AC05 | Fixed safe validation text applies only to seventh-tool pre-entry failures; bounded `TextContent` contains no secret/path/SID/raw exception/internal receipt and existing tool error channels are unchanged. |

Verify: focused six-tool, stdio, frontend protocol, receipt-order, boundary-budget and redaction tests, then full Python tests/Ruff/format. Evidence: before/after wire corpus, import graph, event trace and canary scan. Revert group: T05 frontend-only change.

### T06 — SCM daemon service, admission and daemon-local receipt

Owner: Python backend engineer. Depends on T05. Scope: new `cst_saved_field_daemon_service_windows.py` and daemon-only tests; no live SCM provisioning.

| AC ID | Observable acceptance criterion |
|---|---|
| T06-AC01 | RED composition test fails until daemon independently loads policy, authenticates the enrolled frontend child, resolves unique `entry_id`, and owns the sole `SamplerAdmissionGate`. |
| T06-AC02 | Admission atomically seals availability/revision/generation/active/waiter state, emits one downstream-observable settlement, and creates the original QPC triple only after admission. |
| T06-AC03 | Daemon constructs the real broker client, passes no source path/bytes/handle, validates nested broker receipts, and has no source/safety/containment/vendor/worker import. |
| T06-AC04 | `DaemonResponseReceiptV1` contains only daemon-observed writes, terminal write, flush, ACK, disconnect and server-handle close; it never claims frontend EOF/client close and alone gates release/quarantine. |
| T06-AC05 | Every denial, timeout, broker failure, partial response, missing ACK, disconnect, cancellation, shutdown and restart quarantines/releases exactly once with no early successor admission. |

Verify: focused admission, daemon composition, QPC, broker-client fake and receipt-order pytest tests. Evidence: linearized event traces for every return. Revert group: T06 daemon service.

### T07 — SCM broker service, BrokerProtocolV1 and runtime descriptors

Owner: Python platform/backend engineer. Depends on T06. Scope: new broker service/protocol server, Windows service identity/descriptor/readback code and synthetic tests; no live SCM.

| AC ID | Observable acceptance criterion |
|---|---|
| T07-AC01 | RED authorization matrix fails until broker independently loads policy and admits only exact numeric daemon service SID/token/session/integrity/image over the fixed broker descriptor. |
| T07-AC02 | Broker challenge/request binds CNG nonce, correlation, unchanged QPC triple, revision, unique entry, manifest and request hash; nonce atomically consumes once or cancels on every exit. |
| T07-AC03 | For the new saved-field sampler route, only SCM broker composition reads `MCPHUB_EM_OUTPUT_ROOT`, validates it as absolute/local/non-reparse/owned/access-controlled, and injects `TrustedWorkspacePolicy`; sampler frontend/daemon/application/vendor modules do not read it, while existing-six legacy `safety.py` use remains unchanged (`design.md:273-277,1314-1316`). |
| T07-AC04 | Enrollment, frontend and broker descriptors use exact numeric SIDs, protected DACLs, High SACLs, fixed names and ordered readback; tests reject symbolic/unresolved/default ACLs and any fourth sampler descriptor. |
| T07-AC05 | Broker service has no MCP/frontend import and returns only owner-observed broker facts under bounded framing. |

Verify: focused broker authorization, nonce, policy-root and three-descriptor synthetic tests. Evidence: exact allow/deny and descriptor matrices. Revert group: T07 broker service/protocol.

### T08 — Worker launch, Job containment and all-return cleanup

Owner: Windows containment engineer. Depends on T07. Scope: new containment and broker-worker protocol/entrypoint launch surfaces; no CST installation required.

| AC ID | Observable acceptance criterion |
|---|---|
| T08-AC01 | RED atomic-launch test fails until broker calls exact non-shell `CreateProcessW` with `PROC_THREAD_ATTRIBUTE_JOB_LIST=[job]`, `PROC_THREAD_ATTRIBUTE_HANDLE_LIST=[child_stdin,child_stdout,child_stderr]`, `EXTENDED_STARTUPINFO_PRESENT | CREATE_UNICODE_ENVIRONMENT | CREATE_NO_WINDOW`, no console and no breakaway flag; exact-handle Job membership is verified before the bounded request or user code, with no `CREATE_SUSPENDED`, post-create `AssignProcessToJobObject`, `ResumeThread`, or execution gap (`design.md:1095-1107,1325-1327,1471`). |
| T08-AC02 | Exact PID/creation/token/session/image/package/parent tuple, Job membership and escaped-handle probe bind the worker; parent spoof, breakaway, extra handle and wrong image fail before user code. |
| T08-AC03 | One absolute QPC deadline governs startup/read/write/execution/settlement; watchdog, readers and cancellation are bounded and cleanup alone uses the separate 10-second deadline. |
| T08-AC04 | Normal, exception, timeout, cancellation, worker crash, broker crash, service stop, shutdown and restart terminate descendants, close every handle/stream/thread and return complete kernel containment evidence. |
| T08-AC05 | Foreign/pre-existing CST processes are never joined, attributed, waited on or terminated; no `jobs.py`, `internal/process`, detached spawn or alternate Job owner is introduced. |

Verify: Win32-safe synthetic containment tests plus WSL/non-Windows import tests; no live CST. Evidence: creation-tuple trace, Job accounting, handle inventory and all-exit table. Revert group: T08 containment/worker launch.

### T09 — Windows path identity, manifest transfer and workspace transaction

Owner: Python security/backend engineer. Depends on T08. Scope: sampler-only additive `safety.py`, policy/path/transfer/workspace modules and tests.

| AC ID | Observable acceptance criterion |
|---|---|
| T09-AC01 | RED path matrix rejects relative/remote/device/reserved/ADS/non-default-stream/reparse/hardlink/8.3/case/ancestor-swap aliases until one closed `WindowsPathIdentityV1` grammar proves exact handles and IDs. |
| T09-AC02 | Broker worker opens each independently authorized source, holds ancestors/objects without delete sharing, copies every manifest-v2 row directly, and proves complete destination equality and stable source hashes. |
| T09-AC03 | Missing/extra/duplicate rows, identity/hash/stream drift, partial copy and post-open namespace mutation fail closed without exposing ordinary source paths to daemon/frontend. |
| T09-AC04 | Workspace factory owns the child and every handle from creation to one complete normal-return snapshot transfer; every earlier return removes the child and supplies a complete rollback receipt. |
| T09-AC05 | Snapshot is the sole factory of one non-copyable `AuthorizedVendorPathLease`; no path-based `exists`, reopen, cleanup or same-principal write handoff remains. |

Verify: focused path-identity, reserved-device, manifest, root, transaction and every-return tests. Evidence: identity/manifest/rollback matrices. Revert group: T09 safety/transfer.

### T10 — Vendor isolation, AuthorizedVendorPathLease and settlement

Owner: Python backend/vendor engineer. Depends on T09. Scope: new vendor-isolation, vendor, neutral-port consumer, application and session settlement modules; no live CST.

| AC ID | Observable acceptance criterion |
|---|---|
| T10-AC01 | RED dependency/authority tests fail until path-only writes occur only under fixed broker virtual account in protected workspace and vendor adapter depends inward only on neutral port. |
| T10-AC02 | Read-only inputs remain held `GENERIC_READ` with exactly `FILE_SHARE_READ`; write-capable project/header never crosses daemon principal and unknown output becomes input only after writer close, share-zero seal, identity recheck and hash. |
| T10-AC03 | One borrowed `AuthorizedVendorPathLease` covers every unavoidable CST path reopen and remains owned by `SamplerSession` until cache/session settlement; adapter cannot derive, reopen or settle paths. |
| T10-AC04 | Fixed activation/sample sequence preserves Claim 7 semantics, validates finite typed vendor records, performs no solve/history/mesh/1D work, and does not interpret output as full FEM coefficients. |
| T10-AC05 | Acquisition, vendor handle/process, output seal, lease, session, workspace and application settlement order is exact on success and every failure; fail-once-close remains visible and success cannot outrun cleanup. |

Verify: neutral fake-vendor tests for call order, record validation, principal isolation, lazy-read, output-seal, acquisition and all-return settlement. Evidence: call/ownership/close trace. Revert group: T10 vendor/application.

### T11 — Three-endpoint synthetic integration

Owner: appointed integration owner. Depends on T10. Scope: composition roots and synthetic integration tests only; no live SCM/CST.

| AC ID | Observable acceptance criterion |
|---|---|
| T11-AC01 | RED route test fails unless pre-spawn and application routes traverse exactly the accepted owners and exactly three sampler pipes/four schemas with no direct or parallel path. |
| T11-AC02 | Split receipt ordering proves broker receipt precedes daemon result, daemon-local receipt alone settles admission, and frontend-local receipt alone permits publication; neither asserts remote/future facts. |
| T11-AC03 | Capability, frontend and broker nonce ledgers cancel/consume atomically across partial frame, flush, missing ACK, EOF, cancellation, disconnect, timeout, crash, service stop and restart. |
| T11-AC04 | Entry revision, correlation, hashes and unchanged QPC triple agree across every hop while `broker_issued_tick` is later and cleanup deadline is separate. |
| T11-AC05 | Integration import graph preserves dependency direction and broker-only sampler output-root/source/worker authority; existing six and their legacy `safety.py` use remain local and unchanged. |

Verify: exact synthetic end-to-end protocol and event-order suites, then both full Go and Python suites serially. Evidence: route graph, ordered event ledger, receipt matrix. Revert group: T11 composition wiring.

### T12 — Remove superseded routes and prove residue zero

Owner: integration owner with architecture reviewer follow-up. Depends on T11. Scope: only obsolete artifacts within admitted implementation surface.

| AC ID | Observable acceptance criterion |
|---|---|
| T12-AC01 | RED stale-route scan identifies any detached daemon, unavailable/test-only production route, direct in-process sampler, direct frontend-to-broker/CST or one/two-pipe topology. |
| T12-AC02 | Superseded source, tests, fixtures, docs, exports, entrypoints and aliases are deleted rather than retained as fallbacks or historical comments in live code. |
| T12-AC03 | Static scans find zero `FrontendExchangeReceiptV1`, generic supervisor authorization, hardcoded receipt, same-principal write fallback, sampler output-root reader in frontend/daemon/application/vendor, or fourth sampler descriptor. |
| T12-AC04 | Removal does not touch protected existing-six, HFSS, solve/history, hub routing/filter, `jobs.py`, `cst_results.py` or `internal/process` surfaces. |
| T12-AC05 | Full tests prove removal sensitivity by temporarily reintroducing a stale edge in the test oracle and observing RED. |

Verify: design-derived forbidden-symbol/topology scan plus full tests and `git diff --check`. Evidence: query set with zero-result proofs and sensitivity trace. Revert group: T12 deletions only.

### T13 — Full regression, static quality and portability

Owner: QA engineer. Depends on T12. Scope: tests only; implementation changes return to owner phase.

| AC ID | Observable acceptance criterion |
|---|---|
| T13-AC01 | Full Python suite, Ruff and format pass under frozen Python 3.13 environment; every named guard has at least one observed RED and GREEN result. |
| T13-AC02 | Full Go suite and vet pass; Windows-only code is build-tagged and non-Windows/WSL import or compile checks fail safely without weakening Windows invariants. |
| T13-AC03 | Win32-safe synthetic matrices cover authorization, descriptors, handles, Jobs, path identity, replay, QPC, receipts, quarantine, shutdown and restart without touching live SCM/CST. |
| T13-AC04 | Existing-six baseline is byte/shape/error/route equivalent and protected surfaces show no unauthorized diff. |
| T13-AC05 | Publication canaries and machine-local paths, credentials, source bytes, raw logs and hardcoded policy/receipt values are absent from the candidate diff. |

Verify: exact commands from T00 plus `go vet ./...`, targeted Windows synthetic suite, WSL/non-Windows checks and `git diff --check`. Evidence: fresh command transcript summary, not raw logs in canonical artifacts. Revert group: return failures to T01–T12 owner.

### T14 — Immutable candidate and local history seal

Owner: integration owner. Depends on T13. Scope: candidate commit/history only; no push, pin, registration or deployment.

| AC ID | Observable acceptance criterion |
|---|---|
| T14-AC01 | One candidate commit is created only after bootstrap premises, scope, no-kostyl test and recovery path are recorded; SHA-256/commit ID binds all review inputs. |
| T14-AC02 | Candidate diff is exactly the admitted Change-Surface Contract and contains no unrelated worktree change, generated trash, secrets or machine-local absolute paths. |
| T14-AC03 | Self-introduced broke-then-fixed churn and superseded live implementation history are removed before any publication; accepted work-item history remains untouched. |
| T14-AC04 | Fresh `git diff --check`, full Go/Python checks and publication-safety scan bind the exact candidate range. |
| T14-AC05 | No manifest pin, catalog registration, SCM provisioning, live probe, push, release or deploy occurs. |

Verify: candidate range inspection, repository publication-safety script resolved by current environment, and full checks. Evidence: candidate ID, range receipt, leak-check summary. Revert group: local candidate commit via reviewed local-history rollback only.

### T15 — Independent implementation architecture review

Owner: `$architecture-reviewer`. Depends on immutable T14 candidate. Read-only review artifact only.

| AC ID | Observable acceptance criterion |
|---|---|
| T15-AC01 | Reviewer verifies actual spawn owner, exactly three endpoints/four schemas, supervisor status-only extension and both accepted routes against source/tests. |
| T15-AC02 | Reviewer verifies dependency direction, broker-only sampler output root with existing-six legacy `safety.py` preserved, existing-six transparency, no stale route and all-return owner boundaries. |
| T15-AC03 | Reviewer verifies unchanged QPC triple, split receipt feasibility/order, atomic `CreateProcessW` JOB_LIST/HANDLE_LIST containment with membership before request/user code and no suspended/post-create assignment gap, vendor lease ownership and quarantine linearization. |
| T15-AC04 | All 34 accepted design claims are reconciled; Claims 7 and 15 remain target-only rather than falsely promoted. |
| T15-AC05 | Verdict binds exact candidate and accepted input SHAs; any finding returns to its owner phase and invalidates later reviews. |

Verify: independent review with source/test citations and one terminal verdict in its own artifact. Evidence: claim matrix and candidate binding. Revert group: none; revision returns to implementation.

### T16 — Security-engineer implementation reconciliation

Owner: `$security-engineer`. Depends on T15 PASS. Read-only review artifact only.

| AC ID | Observable acceptance criterion |
|---|---|
| T16-AC01 | Threat constraints are mapped to implemented owners for supervisor/enrollment/frontend/broker/worker/path/vendor/publication boundaries. |
| T16-AC02 | Exact nonce, capability, handle, SID/DACL/SACL, descriptor, replay, deadline, receipt and all-exit matrices are verified against candidate code/tests. |
| T16-AC03 | Secrets/source/path/policy authority cannot cross forbidden frames, env, argv, logs, errors or MCP output; broker-only authority is demonstrated. |
| T16-AC04 | Root-cause falsifiers cover same-user attackers, pipe squatters, parent/image spoof, namespace race, worker breakaway, foreign CST and receipt forgery. |
| T16-AC05 | Residual target-only risks are explicit and allocated to T19–T22; no design constraint is silently waived. |

Verify: independent implementation threat reconciliation. Evidence: constraint-to-test matrix bound to candidate. Revert group: none.

### T17 — Independent security-reviewer verification

Owner: `$security-reviewer`. Depends on T16 PASS. Read-only review artifact only.

| AC ID | Observable acceptance criterion |
|---|---|
| T17-AC01 | Reviewer independently validates authentication/authorization and exact status-only/three-descriptor runtime contract. |
| T17-AC02 | Reviewer validates capability/nonce state machines, replay cancellation, HANDLE_LIST/env secrecy and resource cleanup on every return. |
| T17-AC03 | Reviewer validates containment, path identity, distinct principal, output seal, lease continuity, settlement and foreign preservation. |
| T17-AC04 | Reviewer validates safe errors, budget ceilings, publication canaries and absence of hardcoded machine/policy/receipt authority. |
| T17-AC05 | PASS is exact-candidate-bound; any unresolved material finding blocks T18 and returns to the owning phase. |

Verify: independent security review and fresh selected test reruns. Evidence: complete finding/claim matrix. Revert group: none.

### T18 — Independent implementation QA

Owner: `$qa-engineer`. Depends on T17 PASS. Read-only/test execution; fixes return to owner.

| AC ID | Observable acceptance criterion |
|---|---|
| T18-AC01 | QA re-runs full Go/Python/static/format/Win32-safe synthetic/WSL suites on exact candidate and records fresh counts. |
| T18-AC02 | QA verifies RED sensitivity for every named guard and all exact protocol/auth/replay/QPC/receipt/quarantine matrices. |
| T18-AC03 | QA verifies worker/Job/breakaway/resource, vendor lease/output seal/settlement and existing-six baselines including all returns. |
| T18-AC04 | QA verifies publication/history/index/protected/dependency/orphan/foreign-CST hygiene without live SCM/CST/hub/fleet mutation. |
| T18-AC05 | Candidate remains immutable and no registration/pin/publication/deploy has occurred; any defect invalidates T15–T18. |

Verify: complete candidate-bound QA matrix. Evidence: exact commands, exit codes, test counts and checked/unchecked target boundary. Revert group: none.

### T19 — P18 disposable Windows SCM and descriptor readback

Owner: Windows platform engineer, observed by QA/security reviewer. Depends on T18 PASS. Target-only, authorized disposable host.

| AC ID | Observable acceptance criterion |
|---|---|
| T19-AC01 | Fresh pre-state records SCM services, service SIDs, endpoint namespace, task row, running processes and cleanup oracle; target is disposable and publication/catalog remain off. |
| T19-AC02 | P18 alone provisions both services with exact identities, default-off policy and no production registration, then validates service start/stop/restart/failure paths. |
| T19-AC03 | Numeric service SIDs and exact ordered DACL/SACL readback match enrollment/frontend/broker descriptors; supervisor status authorization admits only exact daemon/status opcode. |
| T19-AC04 | Enrollment binds actual supervisor row and launch capability lifecycle; wrong PID/time/image/package/parent/generation and every injected exit fail closed without stranded handles. |
| T19-AC05 | Rollback removes disposable services/descriptors/policy/output roots and restores byte-for-byte pre-state; residue or ambiguous readback is FAIL. |

Verify: approved P18 target script/runbook with preserved sanitized receipts. Evidence: pre/post readback, service/process/handle diff and rollback receipt. Revert group: P18 provision transaction.

### T20 — Installed CST Claim 7, containment and foreign preservation

Owner: QA engineer with CST domain observer. Depends on T19 PASS. Target-only installed CST harness; catalog remains off.

| AC ID | Observable acceptance criterion |
|---|---|
| T20-AC01 | Actual installed CST proves Claim 7 activation/header/ResultTree/status route using one contained fresh descendant with no visible console/window. |
| T20-AC02 | Exact worker/Job tuple, descendant accounting, broker-crash kill-on-close, timeout/cancel/service-stop cleanup and kernel containment receipt are observed on target. |
| T20-AC03 | Pre-created output and `AuthorizedVendorPathLease` are compatible with actual CST lazy reads/writes; output is consumed only after writer close/share-zero seal/hash. |
| T20-AC04 | Pre-existing/foreign CST sentinels remain alive, unjoined, unattributed and unmodified through success and every failure route. |
| T20-AC05 | Solve/history/mesh/1D behavior is absent; all created descendants, handles, workspace and outputs settle with no residual target state. |

Verify: approved installed-CST target matrix. Evidence: sanitized process tree, window/console observation, Job/lease/output/cleanup receipts and foreign sentinel liveness. Revert group: disposable invocation resources only.

### T21 — Independent native provider qualification

Owner: independent computational scientist/provider assessor. Depends on T20 PASS. Test-only evidence; no production import or config edge.

| AC ID | Observable acceptance criterion |
|---|---|
| T21-AC01 | Provider identity, implementation independence, native execution path, version/build and input provenance are established by direct target probe. |
| T21-AC02 | Provider does not import or call sampler/vendor production implementation and shares only public evidence schemas. |
| T21-AC03 | Required ports, E/H fields, materials/classes, row identity, units and finite six-component outputs are available without fitting to sampler output. |
| T21-AC04 | Native run is reproducible with bounded sanitized artifacts and exact completion oracle; missing/ambiguous provider is FAIL, not waiver. |
| T21-AC05 | Production candidate remains unchanged and default-off; qualification evidence is separately hashed. |

Verify: provider-specific native target run after provider is admitted. Evidence: independence trace and evidence artifact hash. Revert group: test evidence only.

### T22 — Line10 Claim 15 acceptance

Owner: independent acceptance comparator/QA. Depends on T21 PASS. Test-only comparator importing public schemas only.

| AC ID | Observable acceptance criterion |
|---|---|
| T22-AC01 | Exactly four calls cover two ports, E then H, both required materials/classes and exact Line10 labels/order. |
| T22-AC02 | Each field proves 96 local rows and 90 unique rows with deterministic row identity and no missing/duplicate/unmatched records. |
| T22-AC03 | All six components, units, finiteness and zero semantics agree with the independently qualified native provider under accepted tolerances; comparator performs no fitting. |
| T22-AC04 | Claim 15 PASS requires the mechanical comparison report and provider-independence evidence; null/missing/sub-verdict is FAIL. |
| T22-AC05 | Dependency scan proves no production edge/string/config/import to Line10, VFEM or comparator. |

Verify: test-only Line10 comparator and production dependency scan. Evidence: four-call mechanical report and Claim 15 verdict. Revert group: comparator/evidence only.

### T23 — Existing-six disposable end-to-end regression

Owner: QA engineer. Depends on T22 PASS. Target/disposable frontend and hub path; no manifest pin or production catalog mutation.

| AC ID | Observable acceptance criterion |
|---|---|
| T23-AC01 | Each existing tool retains exact registration name/schema and local call path; none crosses enrollment/frontend-daemon/broker pipes. |
| T23-AC02 | Representative success/error outputs and stdio framing are byte/shape compatible with T00 baseline. |
| T23-AC03 | Policy absent, disabled, invalid and services unavailable omit only the seventh tool while all six remain live and unchanged. |
| T23-AC04 | Existing launch, restart, shutdown and hub supervision behavior remains unchanged for non-CST and six-tool calls. |
| T23-AC05 | No target residue, route/filter change, solve behavior change or protected-surface drift remains. |

Verify: disposable six-tool hub/frontend contract suite against T00 corpus. Evidence: per-tool route and byte/shape diff. Revert group: none; failures return to T02/T05/T11.

### T24 — Docs, immutable pin, product build and publication admission

Owner: integration owner, human publication reviewer. Depends on T19–T23 PASS and unchanged T14 candidate except reviewed docs/pin commit.

| AC ID | Observable acceptance criterion |
|---|---|
| T24-AC01 | README/API/operator docs describe exact three endpoints, four schemas, default-off policy, P18 provisioning, enable/restart, diagnostics, quarantine and rollback without stale topology. |
| T24-AC02 | `servers/cst/manifest.yaml` changes only immutable pin/description authorized by accepted design, preserves command/transport/port/client/daemon shape, and binds reviewed implementation commit. |
| T24-AC03 | `pwsh ./build.ps1` produces the installable Windows product from exact reviewed commits; plain `go build` is not treated as publication evidence. |
| T24-AC04 | Full checks, target evidence hashes, `git diff --check`, protected-path scan, leak/publication-safety scan and clean history are fresh for the exact publication range. |
| T24-AC05 | Human explicitly reviews and approves publication after the safety scan; without approval no push/release occurs. |

Verify: docs/topology scan, manifest diff, product build, full tests and publication-safety scan. Evidence: reviewed range receipt, artifact hash and human approval. Revert group: docs/pin commit; no force-push of published history.

### T25 — Default-off deployment, controlled enablement and rollback

Owner: platform engineer with human operator. Depends on T24 publication approval and published immutable artifacts.

| AC ID | Observable acceptance criterion |
|---|---|
| T25-AC01 | Installation starts with policy absent/disabled and seventh tool unregistered; existing six and hub remain healthy. |
| T25-AC02 | Services, numeric SIDs, three endpoint descriptors and immutable binaries/policy are read back before enablement; mismatch aborts. |
| T25-AC03 | Controlled enable plus required restart exposes exactly the seventh tool and one accepted route; smoke call reproduces target receipts without weakening deadlines or authority. |
| T25-AC04 | Disable/restart removes only the seventh registration, settles active work, stops services in order and preserves existing six/foreign CST. |
| T25-AC05 | Rollback restores prior pin/config/services/descriptors and verifies no process, handle, workspace, pipe, policy or catalog residue; failed rollback is operational FAIL. |

Verify: approved deployment runbook with pre/enable/disable/rollback state snapshots. Evidence: immutable versions, registration/schema list, receipt and residue diff. Revert group: published rollback package and exact pre-state restoration.

## Bounded history ledger — not live instructions

| Historical item | Disposition |
|---|---|
| Replaced plan SHA `FBB757B98797C90B7C9FD9B4C4998DCB01788241C5A4D39DE62D1532FD3C684E` and its P10–P24/old AC identifiers | Retired by this plan. They may support provenance only; they confer no implementation, target, registration, pin, publication or deployment authority. |
| Prior implementation candidate `5ff268dc13b2be9ca9500b5441634f0594538b94` and earlier implementation/review artifacts | Historical evidence only. Their tests and review claims do not satisfy any T00–T25 criterion unless freshly reproduced against the new immutable candidate. |
| Earlier one-/two-pipe, direct-owner, detached/unavailable or test-only topology text | Superseded architecture residue; it must not remain in live source/tests/docs or be used as fallback. |
| Accepted design/review SHAs listed at the top | Normative inputs, not evidence that implementation or target checkpoints already passed. Claims 7 and 15 remain target-only. |

## Completion oracle

The plan is complete only when T00–T25 are all evidenced, all reviews and target checkpoints bind the exact immutable candidates, the published pin binds the reviewed implementation, default-off installation and rollback are demonstrated, all 34 design claims reconcile, Claims 7 and 15 have target evidence, existing-six compatibility is preserved, and the three-endpoint/four-schema topology has zero stale route residue.

## Terms and Abbreviations

| Term | Meaning |
|---|---|
| AC | Acceptance criterion with one stable unique identifier |
| CNG | Windows Cryptography Next Generation API used for capability and nonce randomness |
| CST | Computer Simulation Technology electromagnetic solver/runtime |
| DACL / SACL | Discretionary / System Access Control List on a Windows securable object |
| FEM | Finite-element method |
| HANDLE_LIST | Explicit Windows process-creation list of the only handles a child may inherit |
| IPC | Inter-process communication |
| Job | Windows Job Object used as the sole worker containment boundary |
| MCP | Model Context Protocol |
| P18 | Authorized disposable target phase for SCM service provisioning and security-descriptor readback |
| QPC | Windows QueryPerformanceCounter monotonic tick source |
| SCM | Windows Service Control Manager |
| SID | Windows Security Identifier; runtime authorization uses exact numeric SIDs |
| WSL | Windows Subsystem for Linux, used only for non-Windows portability checks |

Gate: PASS
