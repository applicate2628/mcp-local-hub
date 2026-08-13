# T15 Independent Implementation Architecture Review

Date: 2026-08-12

Execution role: `$architecture-reviewer`

Review strategy: Claim-Verify. This is an independent review of immutable candidate `43fee019d46c69522ebe79be952d5f139bd4854f`; builder verdicts were inputs, not proof. No product, test, index, service, process, or live environment was mutated.

## Immutable binding

| Bound input | Identity | Result |
|---|---|---|
| Candidate commit | `43fee019d46c69522ebe79be952d5f139bd4854f` | Exact `HEAD` at review start |
| Candidate tree | `29dada47e1c7d597e5567a66f68b506dc4576cad` | Exact |
| Candidate parent | `048a30fabc10fa3e6bfc64facc9fb6da6ebe49da` | Exact |
| T14 implementation artifact | `515E6244E84B61865926620777E0BEEA741AEB651BD320503094CD3A0BE4C58B` | Recomputed exact |
| Accepted design | `AFABC3C001169D5C571D7319EA2C751CDD228E46B335C9630C0516F6EBAE6DC9` | Recomputed exact |
| Accepted decision | `18307E933D393BBD0C6B0396F47FE6AAFB0C5AE94CE39E395F8EE948371BE92A` | Bound by T14 and canonical plan |
| Canonical T15 plan | `D1C1137D062DF4652902657696D6EE488D4DAD90D6ED33CFBC8B856C7C03E99A` | Accepted input; current bytes differ only through accepted record-hygiene reconciliation recorded by T14 |

CodeGraph exact symbol/call-path exploration for `StdioHost`, supervisor CST dispatch, the frontend route, daemon service, broker service, containment, and worker returned current source with no stale, pending, or disabled banner. Immutable `git show` was used for exact candidate lines and production-entrypoint negative checks.

## Findings

| ID | Severity | Defect class / fix class | Evidence and impact |
|---|---|---|---|
| AR-T15-01 | BLOCKER | Missing production composition root / test-production topology divergence. `design-decision`. | The daemon module exposes a shutdown-safe seam but its production `main()` unconditionally raises `cst_saved_field.daemon_unavailable` (`cst_saved_field_daemon_service_windows.py:270-280`). The broker module does the same (`cst_saved_field_broker_service_windows.py:463-474`). The contained worker production `main()` calls `run_worker` without a real sampler application, so the default `_unavailable` application is selected (`cst_saved_field_broker_worker.py:207-250`). No alternate production composition or listener owner exists in the candidate graph. Therefore the accepted frontend -> daemon -> broker -> contained worker route cannot execute. |
|  |  |  | The green integration test constructs the worker transaction, contained invocation, broker transport, daemon, frontend transport, and FastMCP composition entirely inside the test (`test_cst_saved_field_integration.py:305-432`); it does not invoke any production service or worker entrypoint. This is component evidence, not production-route evidence. |
|  |  |  | Advisory HOW: the architect and backend integration owner must define and implement the fixed daemon and broker composition roots, real three-endpoint listener/provisioning adapters, and a worker transaction adapter on the accepted dependency seams. A subprocess/entrypoint guard must invoke the real module roots through synthetic operating-system adapters and traverse to `BrokerWorkerApplication`, failing if any default unavailable branch is reached. Falsifier: the exact module entrypoints complete frontend -> daemon -> broker -> contained worker -> transaction and ordered return receipts without a live Service Control Manager or CST installation. |
| AR-T15-02 | BLOCKER | Cross-owner receipt provenance erased / fabricated settlement facts. `design-decision`. | `WindowsContainedInvocation` validates kernel settlement internally, but returns only response bytes. `ContainedWorkerBrokerApplicationV1` then creates `BrokerSettlementV1` with literal `True` for `worker_signaled`, exit recorded, process-reference closed, active-job zero, readers joined, handles closed, and pipe closed (`cst_saved_field_broker_service_windows.py:297-320`). Those are causal facts owned by containment, not facts the broker application observed. The default worker additionally returns all six worker-settlement booleans as `True` without running a transaction (`cst_saved_field_broker_worker.py:207-217`). A valid-looking complete receipt can therefore be synthesized without the represented cleanup events. |
|  |  |  | Advisory HOW: containment must return a typed kernel-settlement receipt alongside the response; broker settlement must be composed only from that receipt plus the worker's actual transaction receipt. The unavailable path must not manufacture successful settlement. Falsifier: force each kernel and worker settlement fact false at its owning boundary and prove the broker cannot emit `settlement.complete`; prove quarantine precedes release on every failed return. Add a static guard rejecting literal-success construction in production receipt paths. |
| AR-T15-03 | MAJOR | Superseded unavailable-route residue. `inline-sufficient`. | `UnavailableBrokerTransport` remains in live production source (`cst_saved_field_broker_client_windows.py:171-187`) and candidate-wide reference search finds no use outside its definition. Its `cancel_and_settle` also fabricates a complete cancellation receipt. This contradicts T12 removal and T15-AC02's no-stale-route requirement. Advisory HOW: remove the dead class once the real production composition is present; keep only explicit boundary failures owned by the real transport. Falsifier: candidate-wide production search finds no unavailable/test-only transport class or synthetic-success settlement fallback. |

AR-T15-01 and AR-T15-02 require an upstream architecture decision because the correction changes composition roots and the typed containment-to-broker contract across security-sensitive resource owners. They are not safely inline-reviewable. Later T16-T18 reviews are invalid until correction and a new T15 review.

## T15 acceptance matrix

| AC | Verdict | Evidence |
|---|---|---|
| T15-AC01 | failed | Go spawn/capability and supervisor status-only components exist; constants define three endpoint owners and four schemas. Actual daemon, broker, and transaction worker entrypoints are unavailable (AR-T15-01), so both production routes are not implemented. |
| T15-AC02 | failed | Dependency seams, sampler-only broker output-root ownership, legacy existing-six `safety.py`, and existing-six optional registration are present. Stale unavailable route and incomplete all-return receipt ownership fail this AC (AR-T15-02/03). |
| T15-AC03 | failed | QPC triple propagation, split frontend/daemon receipt ordering, atomic `CreateProcessW` `JOB_LIST` plus exact `HANDLE_LIST`, vendor lease, and quarantine components are present and guarded by tests. The broker replaces the authoritative containment receipt with literal success facts, so causal settlement is not preserved. |
| T15-AC04 | failed | Exact 34-claim reconciliation below: 23 verified, 9 failed, 2 target-only. Claims 7 and 15 were not promoted. |
| T15-AC05 | verified | Review is bound to the exact candidate/tree/parent and accepted inputs above; findings route back to architecture plus backend integration ownership. |

## Exact 34-claim reconciliation

| Claim | Verdict | Candidate evidence |
|---|---|---|
| 1 | verified | Seventh-tool registration remains optional; existing six remain on their established paths. |
| 2 | verified | Sampler application has no JobManager/replay dependency. |
| 3 | verified | FastMCP boundary remains sampler-only and does not solve. |
| 4 | failed | Broker transfer types exist, but no production broker/worker composition executes the source-authority transfer. |
| 5 | verified | `SourceSnapshot` owns named pre/post hash equality in the worker transaction. |
| 6 | verified | `FrameResolver` owns exact metadata selection. |
| 7 | not-verifiable (target-only: installed CST activation, header seal, locks, ResultTree and status trace required) | No target fact was inferred. |
| 8 | verified | Response composition preserves input order and six-component semantics. |
| 9 | verified | Exact-zero classification owns `zero_ambiguous`. |
| 10 | verified | Coordinate transformation uses one closed unit contract. |
| 11 | verified | Vendor-returned identity remains the close authority. |
| 12 | failed | Aggregate settlement consumes worker facts but broker kernel facts are literal success values rather than owner receipts. |
| 13 | verified | Frontend publishes one finite bounded redacted text result. |
| 14 | verified | Line10 remains outside production source. |
| 15 | not-verifiable (target-only: independent native producer and exact numerical comparison required) | Provider independence/equality was not inferred. |
| 16 | failed | Protocol classes describe the three-channel topology, but production daemon/broker/worker roots are unavailable. |
| 17 | verified | Acquisition transaction component owns resources through transfer/rollback. |
| 18 | verified | Neutral port preserves dependency inversion. |
| 19 | verified | Workspace transaction component has one transfer/rollback owner. |
| 20 | verified | Stable no-follow handles own complete manifest transfer. |
| 21 | verified | Daemon/broker/containment components preserve one unchanged absolute QPC triple. |
| 22 | verified | Vendor component validates raw records before use/allocation. |
| 23 | verified | Daemon component uniquely resolves `entry_id`; broker component independently checks its policy row. |
| 24 | failed | Broker does not carry the causal kernel receipt across containment; it reconstructs it with literals. |
| 25 | verified | Sampler output-root ambient authority is broker-only; accepted existing-six legacy `safety.py` is outside this scope. |
| 26 | verified | Lease/share/output-seal components preserve distinct-principal ownership. |
| 27 | verified | Containment component uses atomic no-breakaway/no-console Job-list launch and validates escaped-child settlement before return. |
| 28 | failed | Four schemas and three descriptors exist, but three live owner-local service endpoints and truthful end-to-end receipts do not. |
| 29 | verified | Supervisor-bound launch capability and separate enrollment ledgers cover component clone/replay/all-exit rules. |
| 30 | failed | Descriptor constants and status-only supervisor authorization exist, but production daemon/broker endpoint listeners are not composed or provisioned. |
| 31 | verified | Frontend and daemon owner-local receipts enforce feasible read/EOF/close and write/flush/ack/close ordering. |
| 32 | failed | No production worker application is composed; production `main()` selects `_unavailable`. |
| 33 | failed | Policy-row copy/equality exists as a component, but the production broker route never transfers it to a real worker transaction. |
| 34 | failed | Windows path grammar/lease components exist, but no production vendor transaction route exercises all path roles. |

## Fresh verification receipts

| Command / probe | Result |
|---|---|
| CodeGraph exact symbol and call-path exploration | Current source returned; no stale/pending banner; production-root gap corroborated with immutable `git show` and candidate-wide reference search. |
| `go test ./internal/cli ./internal/daemon -run 'Test.*(CST|Enrollment|Supervisor|StdioHost)' -count=1` | Exit 0; `internal/cli` 56.032 s, `internal/daemon` 8.052 s. |
| `uv run --frozen --python 3.13 pytest -q` over T00/T06/T07/T08/integration files, from the electromagnetics project root | Exit 0; 35 passed in 2.9 s. The first invocation from repository root exited 1 at collection because that location does not expose the project package; the canonical project-root rerun passed. |
| Candidate-wide production search for composition and unavailable routes | No alternate daemon/broker/worker production composition; dead `UnavailableBrokerTransport` found only at its definition. |
| Candidate/index/live-state preservation | No index or Git mutation; pre-existing unrelated worktree changes were not touched; no service, CST, hub, or fleet probe was run. |

## Residuals

Target-only Claims 7 and 15 remain allocated to the later installed-CST and independent-provider gates. They do not mitigate the present candidate-local blockers. Existing-six compatibility and the atomic containment components remain valuable accepted implementation evidence, but green component tests cannot substitute for production composition or truthful cross-owner receipts.

## Gate

REVISE — return AR-T15-01 and AR-T15-02 to `$architect` for a bounded architecture decision and then to the backend integration owner; return AR-T15-03 to the same implementation correction. Re-run T15 against a new immutable candidate before T16.

## Terms and Abbreviations

- QPC: Query Performance Counter, the Windows monotonic counter used for the absolute deadline triple.
- SCM: Windows Service Control Manager.
- Settlement receipt: typed evidence that the resource owner completed the represented cleanup actions.
- Claim-Verify: review strategy that verifies every implementation claim and also searches for risks omitted from the handoff.
