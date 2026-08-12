# Decision-Only Three-Endpoint Architecture Re-review — CST Saved-Field Sampler

Reviewed: 2026-08-12
Execution role: `$architecture-reviewer`
Actual execution path: bounded immutable decision-delta re-review
Model / profile used: unspecified by runtime

## Immutable review boundary

| Input | Exact reviewed value |
|---|---|
| Unchanged corrected design | `design.md` SHA-256 `AFABC3C001169D5C571D7319EA2C751CDD228E46B335C9630C0516F6EBAE6DC9` |
| Corrected proposed decision | `work-items/decisions/2026-08-12-cst-saved-field-authority-containment.md` SHA-256 `FD81F4B2B5C14F8AAA66FC96533B8CDE4A7AF2B5738F146794D5BDC6C57212AD` |
| Prior architecture review | SHA-256 `4F9EAE9AD22ED06FD7957E2FE6758237ECDE348AF63D06FFC6A48B5321581957` |
| Bounded scope | Sole prior `AR-C6-DECISION-06`; Claims 16/28/30; exact three descriptors, four schemas, enrollment lifecycle, split receipt gates; regression scan over all 34 claims |
| Prohibited and not performed | Source/test/plan/decision/Git/index/status mutation; implementation/test execution; live SCM/CST/hub/fleet/process/service/pipe action; publication |

## Prior-finding disposition

| Prior finding | Re-review disposition |
|---|---|
| `AR-C6-DECISION-06` — proposed decision retained pre-enrollment topology and two-descriptor promotion gate | **fixed**. Decision item 17 now includes the exact pre-spawn `supervisor-tracked CST StdioHost -> HubEnrollmentProtocolV1 -> SCM daemon` route followed by the application frontend→daemon→broker→worker route (`decision:284-307`). Lifecycle names enrollment, frontend and broker ledgers, all three descriptors and all four schemas (`decision:309-334`). Promotion requires supervisor/enrollment/frontend/daemon/broker identity, all three pipe descriptors and all four protocol schemas (`decision:368-379`). |

No blocking or advisory architecture finding remains in the bounded delta.

## Exact correction verification

| Surface | S4 verdict | Evidence and result |
|---|---|---|
| Durable topology | verified | Decision now carries both exact sequences and keeps frontend existing-six/publication, daemon admission/QPC/broker client, broker source/Job/worker and worker vendor responsibilities aligned with design (`decision:284-307`; `design.md:7-12,520-544`). |
| Exactly three new sampler endpoints | verified | Enrollment, frontend and broker endpoints are the only new sampler pipes. Decision restart and promotion gates require all three descriptor proofs (`decision:332-333,373-377`; `design.md:392-400,447-486,1638`). |
| Supervisor status-only surface | verified | Promotion explicitly covers supervisor identity; the accepted design retains kernel server binding, exact task row, exact daemon service identity, one status-only opcode and fail-closed descriptor readback. This existing supervisor IPC surface is not miscounted as a fourth new sampler endpoint. |
| Four protocol schemas | verified | Enrollment, frontend, broker and broker-worker are all explicit versioned schemas and all four are promotion-gated (`decision:320-333,376-377`; `design.md:1636`). |
| Enrollment channel/capability ledgers | verified | Authenticated Enroll consumes only channel nonce and creates `ISSUED -> ENROLLED`; ACK/flush/close keeps it armed; exact child read plus daemon challenge consumes; authenticated cancel and every named failure/expiry/shutdown/restart cancel/remove (`decision:235-250,287-290,311-315`; `design.md:428-445`). |
| Enrollment all-return resources | verified | CNG bytes, exact digest, HANDLE_LIST, locator, exact read+EOF, close/zero and terminal state owners remain unchanged and aligned; decision correction adds no alternate resource path. |
| Split daemon/frontend gates | verified | Daemon release depends only on `DaemonResponseReceiptV1`; frontend publication only on `FrontendTransportReceiptV1`; neither asserts remote/future close facts (`decision:96-102,299-301`; `design.md:520-544`). |
| Existing-six compatibility | verified | Decision still assigns existing-six compatibility/local behavior to the frontend and routes only the seventh tool across enrollment/frontend/broker boundaries. No protected-six or route/filter/manifest surface was broadened. |
| Output-root owner | verified | Unchanged design retains SCM broker as the sole ambient `MCPHUB_EM_OUTPUT_ROOT` reader and its named guard agrees; decision delta introduces no competing reader. |
| C6 residue | verified | Exact/semantic scan finds no remaining “both pipe descriptors,” two-protected-pipe, combined frontend receipt, direct frontend broker/admission/QPC, or frontend output-root owner. Decision and design now state one current topology/count/lifecycle truth. |
| Anti-layering | verified | Supervisor authentication, enrollment ledgers, split transport receipts, broker output-root and worker containment each remain single-owner. Decision correction replaces stale truth instead of adding a parallel compatibility route. |

## Exact 34-claim matrix

`verified` is architecture-contract verification, not implementation or target-runtime proof.

| Claim | S4 verdict | Owner/probe result |
|---:|---|---|
| 1 | verified | Existing-six compatibility and sole new MCP tool remain explicit. |
| 2 | verified | Application replay independence/no-JobManager edge remain explicit. |
| 3 | verified | Sampler-only no-solve FastMCP boundary remains single-owner. |
| 4 | verified | Broker transfer alone carries source authority. |
| 5 | verified | `SourceSnapshot` owns named pre/post hash equality. |
| 6 | verified | `FrameResolver` owns exact metadata selection. |
| 7 | not-verifiable (target-only: installed CST activation/header seal/locks/ResultTree/status trace required) | No target fact is inferred. |
| 8 | verified | Response owner preserves order and six-component semantics. |
| 9 | verified | Exact-zero classifier owns `zero_ambiguous`. |
| 10 | verified | One closed coordinate-unit contract owns transformation. |
| 11 | verified | Vendor-returned identity remains sole close authority. |
| 12 | verified | Application aggregate consumes exact acquisition/lease receipts. |
| 13 | verified | Frontend publisher owns one finite bounded redacted text result. |
| 14 | verified | Line10 remains outside production. |
| 15 | not-verifiable (target-only: independent native producer and exact numerical comparison required) | Provider independence/equality cannot be inferred. |
| 16 | verified | Design and decision now share the authenticated three-channel route, existing-six preservation and single broker-contained execution path. |
| 17 | verified | Acquisition transaction owns exact resources until transfer. |
| 18 | verified | Neutral port preserves dependency inversion. |
| 19 | verified | Workspace transaction has one transfer/rollback owner. |
| 20 | verified | Stable no-follow handles own complete manifest transfer. |
| 21 | verified | SCM daemon owns one unchanged absolute QPC budget. |
| 22 | verified | Vendor validates raw records before use/allocation. |
| 23 | verified | Daemon uniquely resolves `entry_id`; broker independently authorizes policy. |
| 24 | verified | Broker containment owns Job termination and causal receipt. |
| 25 | verified | Broker-only trusted-root owner and injection guard agree. |
| 26 | verified | Lease/share/output-seal owners preserve distinct-principal authority. |
| 27 | verified | Containment owns atomic no-breakaway/no-console launch and escaped-child settlement. |
| 28 | verified | All four protocols and exactly three owner-local channel receipts are aligned and promotion-gated. |
| 29 | verified | Supervisor-bound capability and separate channel/capability ledgers cover clone/replay/all exits. |
| 30 | verified | Exactly three protected sampler descriptors plus status-only supervisor authorization/readback are required without a stored-secret fallback. |
| 31 | verified | Owner-local daemon/frontend receipts make release/publication ordering feasible. |
| 32 | verified | Post-admission source/vendor/encoding work remains broker-worker-only. |
| 33 | verified | Broker transfer owns direct policy-row copy/equality. |
| 34 | verified | One Windows identity grammar/lease owns every path role. |

Claims 7 and 15 remain target-only exactly as required. The matrix contains 32 `verified`,
two target-only `not-verifiable`, and zero `failed` claims.

## Residual risk and next boundary

This PASS accepts only the immutable architecture/decision contract and authorizes its next
downstream planning/review step. It does not prove implementation, Windows Service Control
Manager provisioning, CST behavior, target containment, Claim 7, Claim 15, Line10, package
pinning, registration, release or deployment. Those gates remain mandatory.

## Gate

**PASS** — `AR-C6-DECISION-06` is fixed completely. The unchanged design and corrected
proposed decision now agree on the pre-spawn enrollment route, application route, exactly
three protected sampler descriptors, all four protocol schemas, separate enrollment
ledgers, split receipt gates, single output-root owner and all 34 architecture claims.
Claims 7 and 15 remain target-only.

## Terms and Abbreviations

- ACK — acknowledgement frame.
- C6 — superseding-change rule requiring stale live relations to be erased.
- CNG — Windows Cryptography API: Next Generation.
- CST — Computer Simulation Technology solver environment.
- DACL — discretionary access control list.
- MCP — Model Context Protocol.
- QPC — Windows Query Performance Counter.
- SCM — Windows Service Control Manager.
- SID — Windows security identifier.
- S4 — `verified`, `failed`, or `not-verifiable (reason)` claim verdict.
