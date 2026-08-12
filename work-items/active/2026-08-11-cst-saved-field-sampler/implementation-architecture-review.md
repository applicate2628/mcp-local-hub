# C6 Implementation Architecture Review

Date: 2026-08-12

Execution role: `$architecture-reviewer`

Candidate: `5ff268dc13b2be9ca9500b5441634f0594538b94` (C6, local and unpublished)

Gate scope: P17 independent architecture review of the immutable P16 candidate.
This review does not attest target CST behavior, Line10 numerical agreement, live
Service Control Manager (SCM) provisioning, hub/fleet mutation, release, or
publication. Claims 7 and 15 remain target-only.

## Accepted inputs

| Input | SHA-256 |
|---|---|
| Accepted design | `6C47670725FFB2E715BD78915131F50E693A9B8975A053AE9DB6C2399FD8C172` |
| Accepted decision | `E12202B83EE25AE3B8092EBF6D77DFB30C2335CE35C295979826AE0F74A59DA0` |
| Accepted plan | `FBB757B98797C90B7C9FD9B4C4998DCB01788241C5A4D39DE62D1532FD3C684E` |
| Implementation handoff | `457892D6316A109784C6FE3C28481346098CD4FE8A8AB8C95FFCF0437BE33620` |
| Design architecture PASS | `EF84998E20F199F20974FEEF54E7698A6FA3396FF01584B7CE44894BFFBBA4E5` |
| Security constraints PASS | `5ADE835A0C997E4FD761E7926A1FE409B17E6FFC965CE437EC1A4A4CFE7A7035` |
| Independent design security PASS | `9ABB4224DEE0FB1022B8627D180BDAB7C1823671519CA27C31924E5B1715E5FD` |

All input hashes were recomputed during this review. Candidate source remained
byte-identical to C6 after all checks. The old C5 architecture review concerned the
removed daemon-owned helper topology and is not an accepted verdict over C6.

## Reviewed surfaces

The review inspected the accepted numbered claims, decision topology/lifecycle,
P10-P17 acceptance criteria, C6 diff and call graph, and these current surfaces:

- `cst.py`, including restart composition, authorization, invocation, and publication;
- `cst_saved_field.py`, `cst_saved_field_port.py`, `cst_saved_field_vendor.py`;
- both broker protocols and `cst_saved_field_broker_client_windows.py`;
- `cst_saved_field_broker_worker.py` and its private protocol;
- `cst_saved_field_containment_windows.py`;
- `cst_saved_field_policy.py`, `cst_saved_field_transfer.py`, and
  `cst_saved_field_vendor_isolation_windows.py`;
- every C6 saved-field test, with focused inspection of broker topology, client,
  protocol, pipe, worker, containment, integration, service provisioning, transfer,
  vendor isolation, deadline, and composition guards;
- package dependency manifests and C6 changes outside the package.

## Fresh evidence

| Check | Result |
|---|---|
| Candidate binding | `HEAD` and reviewed product source equal exact C6 `5ff268dc13b2be9ca9500b5441634f0594538b94`; parent is `2658ac85a0e1ee88b01f920af94c2664201e7a1c`. |
| Full package | `uv run --frozen --python 3.13 pytest -q`: **508 passed**, one pre-existing Pydantic warning. |
| Focused architecture set | Broker topology/client/pipe/protocol/worker/containment/integration/provisioning: **59 passed**. |
| Static/format | Ruff check **PASS**; Ruff format **PASS**, 53 files already formatted. |
| Diff | `git diff --check 5ff268dc^ 5ff268dc` **PASS**; dependency, lock, Go, hub-process-owner, HFSS, and existing CST result/job owners have no C6 delta. |
| C6 stale-route scan | Exact source/test scan for the obsolete helper topology returned zero matches. |
| Production route inventory | Production definitions exist for the daemon client, broker core, worker, and containment owner, but no production edge connects them. Only tests instantiate `SavedFieldBrokerService`; only tests instantiate `WindowsContainedInvocation`; only tests instantiate `InProcessBrokerTransport`. |
| Current composition | `cst.py:669-677` always gives the daemon `UnavailableBrokerTransport`, whose startup proof is incomplete; therefore even an enabled valid policy never registers the sampler. |
| Named integration oracle | `test_cst_saved_field_integration.py:286-312` injects `BrokerWorkerApplication` directly into `SavedFieldBrokerService` and connects it via the explicitly test-only `InProcessBrokerTransport`; it does not traverse the pipe, SCM services, `WindowsContainedInvocation`, or the module's real worker entry point. |

Green tests establish substantial component behavior, but do not falsify the missing
production edges below.

## Blocking findings

### AR-C6-01 — production topology is not composed

- Severity: **CRITICAL**.
- Defect class: missing composition owner / disconnected architecture graph.
- Violated laws: dependency graph, entry-point thinness, state-synchronization
  ownership, C6 current-state coherence.
- Evidence:
  - `cst.py:669-677` creates only `UnavailableBrokerTransport`; its incomplete startup
    proof makes `_compose_saved_field_tool` return false at `cst.py:651-666`.
  - `SavedFieldBrokerService` has no production caller; its only construction is the
    in-process integration test (`test_cst_saved_field_integration.py:292-304`).
  - `WindowsContainedInvocation` has no production caller from broker service; its
    constructions are tests and the unused `ContainedSamplerRunner` seam.
  - `cst_saved_field_broker_worker.main` invokes `run_worker` with `_unavailable`, not
    a production `BrokerWorkerApplication`/transfer/vendor transaction
    (`cst_saved_field_broker_worker.py:207-250`).
  - the only service operations are data-only `dry_run_provisioning` and
    `dry_run_rollback`; no daemon pipe transport, broker service entry point, or fixed
    service binary composition exists.
- Failure scenario: a valid enabled v2 policy still exposes exactly six tools; no
  install or target configuration can make the checked-in production process traverse
  daemon -> authenticated pipe -> broker -> contained worker -> vendor.
- Single owner: CST daemon/broker/worker composition roots.
- Enforcement probe: start both non-live injected service entry points through the
  production factories and prove the exact call graph reaches
  `WindowsContainedInvocation`, then make the ordinary CST composition root register
  the seventh tool only from that startup proof. The test must forbid
  `InProcessBrokerTransport` and direct worker injection.
- fix-class: `design-decision`.
- ADVISORY HOW (non-binding): implement the accepted composition seams and fixed
  service entry points, then replace the placeholder transport only through the
  authenticated pipe owner. Material alternatives (in-process fallback or
  daemon-owned worker) violate the accepted isolation and are not equivalent.
- Route: `$architect` because the absent edges span composition roots, service
  lifecycle, transport, containment, and vendor transaction ownership.

### AR-C6-02 — broker synthesizes containment settlement

- Severity: **CRITICAL**.
- Defect class: fabricated lifecycle receipt / split resource-lifetime authority.
- Violated laws: D4 resource lifetime, single-owner invariant, all-return-paths
  discipline, end-to-end channel verification.
- Evidence: `SavedFieldBrokerService.exchange` calls an arbitrary synchronous worker
  callable and then hard-codes `worker_signaled`, `worker_exit_recorded`,
  `worker_reference_closed`, `job_active_zero`, `readers_joined`, and `pipe_closed` to
  true (`cst_saved_field_vendor_isolation_windows.py:271-307`). It consumes only the
  worker's application settlement. No receipt from `WindowsContainedInvocation` or
  kernel accounting reaches this response. `InProcessBrokerTransport.cancel_and_settle`
  likewise returns a constant-complete receipt (`:324-342`).
- Failure scenario: a worker or descendant can remain active, a reader/handle can fail
  to settle, or cancellation can do nothing, while the broker emits a complete receipt
  and the daemon publishes success/releases admission. The authoritative Job owner is
  bypassed rather than observed.
- Single owner: broker `WindowsContainedInvocation` termination state machine.
- Enforcement probe: inject every worker-start/read/protocol/timeout/residual/cancel/
  disconnect/daemon-death failure through the production broker route and require the
  exact kernel-owned receipt fields; delete all constant-success construction, and
  prove a single false field latches daemon quarantine before release.
- fix-class: `design-decision`.
- ADVISORY HOW (non-binding): make the broker service consume a typed result from the
  one containment owner and construct `BrokerSettlementV1` only from that result plus
  worker application receipts. Cancellation must enter the same state machine.
- Route: `$architect`; resource ownership and receipt contracts span modules and are
  security/concurrency sensitive.

### AR-C6-03 — the cross-process QPC challenge requires equal ticks

- Severity: **HIGH**.
- Defect class: impossible distributed-time precondition / protocol contract drift.
- Violated laws: deterministic ambient-input control, wire-shape verification,
  general-case invariant ownership.
- Evidence:
  - daemon samples `admitted_tick`, then performs transport work
    (`cst_saved_field_broker_client_windows.py:230-233`);
  - broker later rejects unless its current `tick == deadline.admitted_tick`
    (`cst_saved_field_vendor_isolation_windows.py:263-267`);
  - daemon also rejects unless returned `challenge.issued_tick == admitted_tick`
    (`cst_saved_field_broker_client_windows.py:234-239`);
  - tests hide elapsed time by injecting constant counters (`10` on both sides at
    `test_cst_saved_field_integration.py:296-309`).
- Failure scenario: on every real named-pipe round trip QPC advances between daemon
  admission and broker observation; a healthy broker rejects the first challenge as
  protocol-invalid. This blocks all real invocations even after AR-C6-01 is wired.
- Single owner: `BrokerProtocolV1` QPC/deadline contract.
- Enforcement probe: use strictly increasing shared QPC counters across an actual
  delayed transport and prove unchanged frequency/deadline integers, monotonic
  narrowing, five-second nonce expiry bounded by the original deadline, and no new
  deadline sampling.
- fix-class: `design-decision`.
- ADVISORY HOW (non-binding): preserve the daemon's original admitted/deadline ticks,
  but validate the broker observation as a later tick within that interval and bind
  the challenge to both values. Exact contract selection belongs to the architect and
  security owners.
- Route: `$architect`; changing this affects both protocols and all participants.

### AR-C6-04 — post-challenge failures leak the nonce lifecycle

- Severity: **HIGH**.
- Defect class: incomplete all-return coverage / abandoned shared-state lease.
- Violated laws: D4 resource lifetime, all-return-paths discipline, retry/re-entry
  safety.
- Evidence: after `challenge()` succeeds, validation of the challenge, authority-only
  request filtering, correlation/request construction, and encoding occur outside the
  inner `exchange` try/cancel path (`cst_saved_field_broker_client_windows.py:233-255`).
  Any failure at lines 234-253 releases only daemon admission in the outer handler;
  it neither consumes nor cancels the broker nonce. `NonceLedger.issue` refuses a new
  challenge while any outstanding value exists
  (`cst_saved_field_vendor_isolation_windows.py:217-229`).
- Failure scenario: one malformed/late/local construction failure after challenge
  leaves the process-lifetime broker ledger occupied; every later call returns
  `broker_busy` until restart, without the required quarantine/settlement semantics.
- Single owner: broker challenge/nonce lease lifecycle, driven by daemon transport
  cancellation.
- Enforcement probe: inject one failure at every return point after challenge issue
  and before/during exchange; each must consume or explicitly cancel the exact nonce,
  leave no outstanding challenge, and either release cleanly before privileged work or
  quarantine on unproved settlement.
- fix-class: `design-decision`.
- ADVISORY HOW (non-binding): represent the challenge as a typed lease with exactly
  one terminal consume/cancel operation, and place request construction plus exchange
  inside its owner-controlled lifetime. Timeout expiry alone is not a settlement
  receipt.
- Route: `$architect`; this changes the protocol lifecycle and cross-process state
  ownership.

## Anti-layering and dependency assessment

| Defect class | Verdict | Reason |
|---|---|---|
| Daemon/broker/worker route | `PILED` | Test-only direct composition sits beside disconnected production placeholders; it is not the single current production route. |
| Containment settlement | `PILED` | Broker reasserts the containment owner's facts as constants without a trust-boundary receipt. |
| Deadline/QPC | `PILED` | Protocol QPC integers and containment-local `time.monotonic()` 60-second resampling are separate budget owners; the original cross-process deadline does not reach the containment call. |
| Vendor/application ownership | `CLEAN-SINGLE-OWNER` within isolated component tests | Neutral port, transfer, lease, and application owners are coherent when directly invoked, but are not connected to production. |
| Obsolete helper removal | `CLEAN-SINGLE-OWNER` | Old helper source/tests and semantic residue are absent from the live package. |

The three `PILED` classes independently require `REVISE`; no publication should
preserve a test-only route plus placeholder production route as two truths.

## Claims 1-34

S4 verdict vocabulary: `verified`, `failed`, `not-verifiable (with reason)`.

| Claim | Verdict | Owner/probe result on C6 |
|---:|---|---|
| 1 | verified | CST registration seam preserves the existing-six inventory and package tests. The seventh tool is currently unreachable, which is covered separately by failed topology claims. |
| 2 | verified | SavedField application restart/no-Job-state guards pass in the isolated application path. |
| 3 | verified | FastMCP literal-false validation and forbidden solve/Job dependency guards pass. |
| 4 | failed | No production broker transfer/worker route exists; the daemon uses only `UnavailableBrokerTransport`. AR-C6-01. |
| 5 | verified | SourceSnapshot mutation guards pass for application/transfer component paths. |
| 6 | verified | FrameResolver exact metadata/selector and ambiguity matrices pass. |
| 7 | not-verifiable (target CST activation trace is P19) | Fake vendor call order does not establish Result3D/header/ResultTree compatibility. |
| 8 | verified | `SavedFieldResponseV1` order and exact six-component wire semantics pass. |
| 9 | verified | Exact-zero ambiguity classifier matrices pass. |
| 10 | verified | Closed m/mm unit contract and transform matrices pass. |
| 11 | verified | Owned session identity component guards close only returned identities and preserve foreign ones; target liveness remains later. |
| 12 | failed | Worker application receipts exist, but broker fabricates containment/pipe settlement and cancel settlement. AR-C6-02. |
| 13 | verified | MCP publisher returns one bounded redacted text item with no structured duplicate. |
| 14 | verified | No production Line10/VFEM import, string, config, or dependency edge exists. |
| 15 | not-verifiable (independent native Line10 comparison is P20-P21) | No numerical target result is inferred. |
| 16 | failed | There is no single production daemon admission -> broker invocation -> broker Job boundary; containment is disconnected and has a second local 60-second owner. AR-C6-01/02/03. |
| 17 | verified | `open_owned_sampler_session` acquisition-transaction component tests cover partial primitives and exact close behavior. |
| 18 | verified | Application contracts depend inward on the neutral port; no concrete CST adapter import crosses that boundary. |
| 19 | verified | Workspace creation/rollback component matrices retain one typed lease and preserve siblings. |
| 20 | verified | Stable no-follow manifest transfer and Windows identity component matrices pass. |
| 21 | failed | QPC challenge equality is impossible with elapsed transport time and containment resamples its own monotonic 60-second deadline instead of consuming the propagated QPC deadline. AR-C6-03. |
| 22 | verified | Vendor raw-record validation and bounded iterator matrices pass before selection/allocation. |
| 23 | failed | Policy/nonce classes exist, but production has no broker authorization/pipe route; enabled valid policy still leaves the tool unregistered. AR-C6-01. |
| 24 | failed | Containment unit tests pass, but broker never invokes the owner and instead hard-codes all termination facts true. AR-C6-02. |
| 25 | verified | TrustedWorkspacePolicy local/non-reparse/owner/access matrices pass. |
| 26 | verified | Vendor lease share-mode/output-seal/ancestor-swap component matrices pass; production reachability is covered by Claim 34 failure. |
| 27 | failed | Exact CreateProcess/startup/breakaway component probes pass, but no production broker edge invokes them or returns their settlement. AR-C6-01/02. |
| 28 | failed | Both closed schemas validate, but the only end-to-end exchange is test-only in-process and bypasses pipe framing, containment, and real worker main. AR-C6-01/02. |
| 29 | failed | No production broker snapshot/authorization or fresh contained application worker is composed; worker main defaults to `_unavailable`. AR-C6-01. |
| 30 | failed | Dependency manifests are clean and typed SCM/pipe constants exist, but fixed services and the protected named pipe are not executable production dependencies—only dry-run receipts. AR-C6-01. |
| 31 | failed | Admission-gate unit races pass, but a post-challenge/pre-exchange failure abandons the broker nonce and releases admission without consume/cancel or quarantine. AR-C6-04. |
| 32 | failed | Source/application work is not executed in a production broker-owned contained worker, and the original QPC deadline is not the containment deadline. AR-C6-01/03. |
| 33 | failed | Transfer equality is verified as a component, but no production broker worker performs it before CST. AR-C6-01. |
| 34 | failed | One grammar/lease implementation exists and its matrices pass, but the production route to isolated vendor candidates/header/registration is absent. AR-C6-01. |

Summary: 19 verified, 13 failed, 2 not-verifiable. Claims 7 and 15 remain
correctly target-only and do not cause this REVISE; the thirteen implementation
failures do.

## Blast radius and required gate

The corrective surface spans daemon composition, broker service/pipe transport,
worker composition, containment result plumbing, both protocol lifecycle contracts,
nonce settlement, and service entry points. It is not an inline code cleanup. The
accepted decision's owners remain appropriate, but the implementation does not yet
realize their propagation path. Any source correction creates a new candidate and
invalidates this review.

Required before re-review:

1. an architect-approved correction mapping the four findings to one production
   composition and lifecycle contract;
2. implementation on a new immutable candidate;
3. behavioral production-factory topology, increasing-QPC, all-return nonce, and
   kernel-receipt falsifiers;
4. repeat architecture, security, and QA gates. Claims 7/15 remain later target gates.

## Gate

**REVISE.** C6 removes the obsolete helper and its isolated components pass 508 tests,
but it does not implement the accepted executable daemon -> broker -> broker-owned
worker topology. Production always installs an unavailable broker, the named
integration test substitutes a direct in-process route, containment settlement is
fabricated, the QPC handshake rejects real elapsed time, and post-challenge failures
leak nonce ownership. P17-AC05 and thirteen claims therefore fail.

## Terms and Abbreviations

- CST: Computer Simulation Technology electromagnetic solver suite.
- MCP: Model Context Protocol.
- QPC: Query Performance Counter, the Windows monotonic counter.
- SCM: Windows Service Control Manager.
- S4: canonical per-claim verdict vocabulary.
- SID: Windows security identifier.
