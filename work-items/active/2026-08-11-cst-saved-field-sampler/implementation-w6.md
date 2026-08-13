# W6 Executable Default-Off Local Integration

Execution role: `$backend-engineer` and named integration owner

Plan: `484883EDBAD02333162C61FAF78B99AA56C402FD64D5955F0E6B65BDDEC82E14`

Accepted design: `7423D56DD33394336A06AB8C515D12F4496B4AC0533F2901BF5A0EE1436756ED`

Accepted W5 package: `DD0286FE1DDB8E3677147C40C17BA00FEEA23DA867AC019C460580355C319CC6`

## Summary

W6 replaces the worker composition root's opaque injected transaction with a
single package-owned `SavedFieldWorkerTransactionV1`. After the accepted native
pre-main receipt, that owner reconstructs the closed application request from
the broker-authorized project identity, invokes the real `sample_saved_field`
application core through the provisioned application port, canonicalizes the
result, maps only stable failure identifiers, and obtains the worker receipt
from the same owner-local application port. A transport receipt is never used
as application or containment evidence.

`WorkerCompositionV1` now accepts only the broker-authorized `project_bundle`
and provisioned application port rather than an arbitrary result-producing
transaction. `compose_default_off_runtime()` remains `None`; absent
provisioning exits 78 and cannot begin source, vendor, or CST work.

## RED then GREEN

| Probe | RED | GREEN |
|---|---|---|
| `test_worker_transaction_owns_application_and_settlement` | FAIL: import of missing `SavedFieldWorkerTransactionV1` | PASS |
| Provisioned synthetic integration | Existing route returned a literal JSON success from an injected transaction | PASS: actual frontend/daemon/broker framing reaches `run_worker`, validates bootstrap/pre-main receipt, then executes source -> session -> vendor fake adapter -> application settlement -> canonical response |

## Changed paths

| Path | SHA-256 |
|---|---|
| `servers/electromagnetics-mcp/src/mcphub_em_mcp/cst_saved_field_broker_worker.py` | `FB55B509546743A84433A20FB068633FA4742477CC6EE01AF43E1576B4A74F1A` |
| `servers/electromagnetics-mcp/tests/test_cst_saved_field_integration.py` | `98B2D22FD01885B913254C3E7541FE42EC382E493FE9B2E362DC977A5D0BC743` |
| `servers/electromagnetics-mcp/tests/test_cst_saved_field_t15_production_composition.py` | `61CE0EAC9B6975665AA37EC152F7F168E1BF98CAC960EA1DA6751A7C32AC45A7` |

The Git baseline already contained the dirty W0-W5 candidate and the T15 file
was already untracked when W6 started. No attempt was made to reset, stage, or
attribute the whole baseline diff to W6.

## Acceptance reconciliation

| Criterion | Result |
|---|---|
| W06-AC01 native frontend child and local-pipe lifecycle | PARTIAL: the accepted W5 native/Job/handle package is preserved; W6's synthetic route validates the exact pre-main objects through `run_worker`, but this turn did not launch a fresh native child or real local pipe. |
| W06-AC02 default-off and existing-six preservation | PASS on Python catalogue/stdio fixtures; absent worker composition still exits 78. No manifest pin or live state changed. |
| W06-AC03 one synthetic route, four schemas, three endpoints, bounded text | PASS: 26-test exact Python W6 set; the fake adapter performed source/session/vendor/settlement work and emitted one bounded canonical text result. This is not target policy or CST acceptance evidence. |
| W06-AC04 missing receipt/QPC/disconnect/residual failures | PASS through accepted W2-W5 focused preservation plus current pre-main validation; no transport boolean is accepted as worker settlement. |
| W06-AC05 existing six unchanged and zero solver action | PASS: `tests/test_servers.py` and `tests/test_stdio.py` in the 26-test W6 set; no source references Line10/VFEM and no live solver ran. |

## Verification

| Check | Fresh result |
|---|---|
| Exact Python W6 plan set: integration, T15 composition, servers, stdio | PASS: `26 passed`; one pre-existing Pydantic unresolved-forward-reference warning. |
| Focused T15 plus integrated route | PASS: `14 passed`; same warning. |
| Scoped Ruff check / format check | PASS / PASS: 3 W6 files. |
| `internal/daemon` | PASS: `39.188s`. |
| `internal/api` | UNVERIFIED at command gate: test process printed `ok ... 119.732s`, but the wrapper crossed its 120-second boundary and returned exit 124. |
| `internal/cli` | UNVERIFIED: the run produced no terminal output before Lead requested stopping further Go runs; it was terminated. |
| Scoped `git diff --check` | PASS. |
| Post-edit CodeGraph MCP | PASS: current source indexed; one `SavedFieldWorkerTransactionV1` owner is used by composition and the integrated test, with no stale injected production transaction path reported. |

## Receiving-side echo

| Diff-invisible invariant | Evidence |
|---|---|
| One route only | VERIFIED by post-edit CodeGraph: production composition constructs `SavedFieldWorkerTransactionV1`; default-off returns no composition. |
| No fake settlement | VERIFIED in production code: complete `WorkerSettlementV1` comes only from the provisioned application port; test fakes are named fixtures and are not containment evidence. |
| Default-off has zero side effects | VERIFIED: `compose_default_off_runtime()` returns `None`, `main()` returns 78 before request/application work. |
| Existing six and hub topology unchanged | VERIFIED by Python server/stdio fixtures; Go daemon PASS. API/CLI broad command completion remains UNVERIFIED as recorded above. |
| Final MCP text bounded and redacted | VERIFIED by existing frontend publisher and protocol bounds in the 26-test W6 set; no raw exception text is published by the transaction. |
| Capability, inheritance epoch, Job, QPC and receipt ownership preserved | VERIFIED by no W5 containment/native edits and unchanged `request.deadline` across the integrated route; fresh native child proof was not rerun in W6. |

Named regression guards: existing-wire compatibility, validation-channel,
protocol drift, MCP-boundary budget, canary-redaction and publication guards
were exercised by the exact Python W6 set. W2-W5 integrated preservation is
accepted from W5 and was not widened into a broad rerun. Expected one bounded
canonical text or stable failure; observed one bounded canonical text in the
synthetic provisioned route and stable default-off when composition is absent.

Defect-class audit: the obsolete arbitrary-result transaction had two live
participants: `WorkerCompositionV1` and the integrated T15 route. Both are
fixed. `WorkerTransactionPort` remains an internal stable interface used only
inside `BrokerWorkerApplication` and is not a provisioning injection surface.
Broker, daemon, and frontend transport receipts are not affected and remain
owner-local channel evidence.

## Wire and API statement

No public MCP, HTTP, database, queue, cache, or external Remote Procedure Call
surface changed. The private composition constructor changes from an injected
`transaction` object to `project_bundle` plus `application_port`. The broker-
worker request/response wire, success/error fields, ordering, status semantics,
QPC triple, and consumers are unchanged. No new outbound call site, timeout,
retry policy, authorization route, query, or persistence path was added.

## Risks and W7 handoff

The implementation is ready for W7 verification, but W6 cannot claim an
unqualified full gate because the exact Go command did not obtain clean
terminal evidence for `internal/api` and `internal/cli`, and W06-AC01's fresh
native-child/local-pipe observation was not repeated in this turn. W7 must run
the repo-standard full gates with a sufficient timeout and the native verifier;
it must treat either missing result as failure. No live CST, Service Control
Manager, App Control, virtual disk, Hardware Security Module, installation,
Git index, commit, or publication action occurred.

## Gate

`REVISE:verification`

## Terms and Abbreviations

- MCP: Model Context Protocol.
- QPC: Query Performance Counter.
- RPC: Remote Procedure Call.
- SCM: Service Control Manager.
- W5/W6/W7: ordered implementation phases in the accepted shipping plan.
