# W8 Independent Implementation Architecture Review

Date: 2026-08-13

Execution role: `$architecture-reviewer`

Review strategy: independent Claim-Verify over the immutable W7 working-result candidate. Builder and W7 verdicts were treated as inputs, not proof. Product source, tests, Git index, services, CST, Service Control Manager, App Control, virtual disks, signing state and publication state were not mutated. Fresh tests ran only from a `git archive` snapshot under `/.scratch/`.

## Immutable binding

| Bound input | Identity | Result |
|---|---|---|
| Candidate commit | `bab886092ae0a4148c05f1e057eeedd73731eedf` | Exact `HEAD` at review start |
| Candidate tree | `1850fd616f6585b31727c725b37de236c09d527a` | Exact |
| Candidate changed paths | `66` | Exact `git diff-tree` count |
| Candidate content SHA-256 | `D8DA50B229BF8120A581B91531264103C00D5B6919122C07B85251782050108A` | Bound by W7 receipt |
| Accepted design | `7423D56DD33394336A06AB8C515D12F4496B4AC0533F2901BF5A0EE1436756ED` | Recomputed exact |
| Accepted decision | `8EAD15D041781A05ED192107D997693E27B79175C0D0125BB09F1C0A6DE8696A` | Recomputed exact |
| Accepted plan | `484883EDBAD02333162C61FAF78B99AA56C402FD64D5955F0E6B65BDDEC82E14` | Recomputed exact |
| Terminal W7 artifact | `1E099E74EDFCF098B042A4D4385EFFC962729FE566A2C5C87CFE835CCEF6C9E1` | Recomputed exact |

The live worktree had unrelated dirty paths, including `internal/api/port_alloc_excluded_windows.go`; therefore CodeGraph was used for current owner/call-path navigation only. Its fresh CST queries had no stale banner for a candidate path. One later query reported only unrelated `internal/gui/cleanup.go` pending. Every gate-bearing source claim below was rebound to `git show bab886092ae0a4148c05f1e057eeedd73731eedf:<path>` or executed from the archived candidate snapshot.

## Reviewed surfaces and topology

The review read the complete accepted 34-claim list, W0-W7 acceptance criteria and receipts; the exact 66-path candidate inventory; the native C entry/runtime, build, closure builder, independent PE verifier and manifest; Go `HostConfig`, `StdioHost.Start`, launch-capability lifecycle, Windows handle application and CST CLI composition; Python endpoint/policy owners, frontend/daemon/broker transports and services, broker-worker protocol, containment, worker composition, application/vendor/transfer owners; and the focused native/Go/Python architectural guards.

Observed call and ownership chain:

`cstLaunchCapabilityConfig -> HostConfig -> StdioHost.Start -> prepareLaunchCapability -> windowsLaunchCapabilityPipe.apply -> exec.Cmd.Start -> native frontend -> enrollment/frontend pipe -> SCM-daemon admission -> broker pipe -> WindowsContainedInvocation -> native worker -> SavedFieldWorkerTransactionV1`.

- `exec.Cmd` remains the sole frontend creator. The CST capability route replaces command/argv with the admitted direct image and forbids a wrapper fallback (`internal/cli/daemon.go:285-299`; `internal/daemon/host.go:247-251`).
- The Windows Go owner accepts an otherwise compatible `SysProcAttr`, then adds exactly one capability handle; standard input/output/error remain owned by `exec.Cmd` (`internal/daemon/launch_capability_windows.go:108-124`).
- One endpoint registry owns exactly enrollment, frontend and broker endpoints; service topology admits exactly four closed schemas (`cst_saved_field_endpoints.py:5-8`; `cst_saved_field_daemon_service_windows.py:35-40`).
- The worker launch owner forms one ordered five-handle list under one inheritance epoch and returns typed containment observations. Broker settlement copies those observations plus the worker-owned application settlement; no production literal-all-true settlement route remains (`cst_saved_field_containment_windows.py:1051-1174`; `cst_saved_field_broker_service_windows.py:330-384`).
- Daemon, broker and worker production roots are deliberately default-off until target provisioning supplies closed runtime composition (`cst_saved_field_daemon_service_windows.py:322-336`; `cst_saved_field_broker_service_windows.py:574-586`; `cst_saved_field_broker_worker.py:178-196`). The synthetic W6 route supplies named non-live ports through the same composition seams; it is not a second production route.

## Findings

No blocking architecture, layering, dependency-direction, stale-route, receipt-ownership or avoidable-debt finding was found in the reviewed candidate.

The prior T15 defect class is explicitly closed across all participants: containment returns owner-local kernel observations; the worker returns owner-local application/capability settlement; the broker composes but does not fabricate either. Candidate-wide search found no `UnavailableBrokerTransport`, raw parallel frontend creator, production `JobManager`, Line10/VFEM edge, or CST-capability uvx/npx fallback. Generic uvx/npx comments in the shared non-CST host describe live existing-daemon behavior and are not stale CST-route residue.

Anti-layering verdicts:

| Defect class | Verdict | Single owner |
|---|---|---|
| Frontend creation/capability delivery | `CLEAN-SINGLE-OWNER` | Existing Go `StdioHost` / `exec.Cmd` owner |
| Worker creation/containment | `CLEAN-SINGLE-OWNER` | Broker `WorkerInheritanceEpoch` plus `WindowsContainedInvocation` |
| Transport receipts | `CLEAN-SINGLE-OWNER` | Each of frontend, daemon-response and broker channel owners observes only its own events |
| Application/vendor settlement | `CLEAN-SINGLE-OWNER` | Worker `SavedFieldWorkerTransactionV1` and provisioned application port |
| Default-off admission | `JUSTIFIED-DEPTH` | Independent frontend, daemon, broker and native trust boundaries fail closed; no trusted result is redundantly re-decided |

## Exact 34-claim reconciliation

Canonical verdict vocabulary is `verified`, `failed`, or `not-verifiable (with reason)`.

| Claim | Verdict | Candidate evidence and enforcement probe |
|---:|---|---|
| 1 | verified | Existing-six registration and stdio fixtures pass in the 598-test snapshot selection; direct CST launch remains receipt-selected and default-off. |
| 2 | verified | `SavedFieldWorkerTransactionV1` is request-local and has no JobManager/replay edge; no-job search is clean. |
| 3 | verified | Sampler validation rejects solve policy before application work; forbidden solve/job guards pass. |
| 4 | verified | Broker alone opens source/workspace capabilities; native worker five-handle revocation/pre-main guards and containment tests pass. |
| 5 | verified | `SourceSnapshot` pre/post equality and deliberate mutation matrices pass. |
| 6 | verified | `FrameResolver` owns exact metadata selection; ambiguity/order matrices pass. |
| 7 | not-verifiable (target-only: installed CST Result3D/header seal/ResultTree/status trace is required) | Synthetic vendor tests do not promote this target fact. |
| 8 | verified | Response models preserve request order and exact six sampled components; component/order tests pass. |
| 9 | verified | Exact-zero classifier remains `zero_ambiguous`; signed-zero table passes. |
| 10 | verified | One m/mm unit contract and `UnitTransform` own scaling; boundary/unit tests pass. |
| 11 | verified | Only vendor-returned owned identity authorizes close; foreign-process guards pass. |
| 12 | verified | Application aggregates exact no-default receipts after settlement attempts; injected acquisition/lease failure matrices pass. |
| 13 | verified | Frontend emits one bounded redacted text result with no structured duplicate; boundary/canary tests pass. |
| 14 | verified | Immutable production search has no Line10 or VFEM import/string/config edge. |
| 15 | not-verifiable (target-only: independent native producer and exact dual-field/port/material comparison are required) | No synthetic or local result was treated as provider independence. |
| 16 | verified | One direct native frontend owns existing six; only the seventh crosses the fixed service/worker topology; exact-child, closure and stale-route probes pass. |
| 17 | verified | Acquisition transaction retains and closes factory-local resources on every partial return; failure-injection tests pass. |
| 18 | verified | Application depends on the neutral saved-field port; concrete CST adapter depends inward and import-graph guards pass. |
| 19 | verified | Workspace lease has one create/transfer/rollback owner and sibling-preservation tests pass. |
| 20 | verified | Authorized transfer uses stable no-follow handles and complete-manifest equality; alias/swap matrices pass. |
| 21 | verified | SCM daemon creates one unchanged QPC budget propagated through daemon, broker and worker; altered-triple/deadline tests pass. |
| 22 | verified | Vendor records are validated before selection/allocation; malformed and bounded-iterator matrices pass. |
| 23 | verified | Daemon uniquely resolves entry ID and broker independently authorizes exact revision/root/manifest; default-off/zero-work matrices pass. |
| 24 | verified | Working candidate contains no deploy/sign/import route; every absent target receipt leaves production default-off. Containment failures quarantine before any success. X-phase target settlement remains unclaimed. |
| 25 | verified | `TrustedWorkspacePolicy` owns one injected local protected root; owner/access/reparse matrices pass. |
| 26 | verified | Read-only and write-capable roles remain separated across snapshot/lease/vendor-isolation owners; share/access/seal matrices pass. |
| 27 | verified | One broker-wide epoch, atomic Job plus ordered five-handle list and native pre-entry revocation are enforced; native/containment falsifiers pass. |
| 28 | verified | Four closed protocols and exactly three endpoint owners preserve bounded correlation sequences and owner-local receipts; partial-frame/ACK/EOF/close matrices pass. |
| 29 | verified | Existing Go launch lifecycle is sole owner; held image/manifest identity, singleton handle, real-child and cancellation guards pass. |
| 30 | verified | Candidate exposes no content-key-use/signing path; target signing remains impossible until the X-phase sole owner and audit receipts exist. No local evidence is promoted. |
| 31 | verified | Daemon admission settles or quarantines before release, while frontend local receipt independently gates publication; deterministic event-order tests pass. |
| 32 | verified | Production source/vendor work is unreachable because target ceremony/policy/package composition is absent; only explicitly synthetic W6 ports traverse application code. Zero-source default-off probes pass. |
| 33 | verified | Native runtime revokes handles before package admission and fails `native_loader_invalid` without a provisioned identity; no CiTool/VHDX/policy receipt is fabricated. Native verifier and absent-receipt child probes pass. |
| 34 | verified | One Windows path grammar/identity contract covers admitted roles; reserved-device, sharing, principal, swap, stream and unavailable-proof matrices pass. |

Result: 32 `verified`, 0 `failed`, 2 `not-verifiable (target-only)`. Claims 7 and 15 alone remain target-only as required.

## W0-W7 acceptance reconciliation

| AC | Verdict | Architecture evidence |
|---|---|---|
| W00-AC01 | verified | W0 receipt classifies the pre-candidate tree; W7 commit contains exactly 66 admitted paths. |
| W00-AC02 | verified | W7 receipt records 44 unrelated hashes equal; current review changed none. |
| W00-AC03 | verified | Fresh CodeGraph returned the sole Go spawn path and intended Python topology; no duplicate raw launch path exists. |
| W00-AC04 | verified | W0 RED gaps correspond exactly to later W2-W6 owners; no unrelated mechanism was introduced. |
| W00-AC05 | verified | Existing-six baseline is non-empty and passes in fresh snapshot tests. |
| W01-AC01 | verified | Native RED contract is satisfied by independent verifier plus mutation tests. |
| W01-AC02 | verified | Go receipt/SysProcAttr/singleton-handle contract passes focused exact-snapshot tests. |
| W01-AC03 | verified | Fixed no-argument service roots and closed composition tests pass; injected arbitrary transaction production path is absent. |
| W01-AC04 | verified | Owner-local receipt chain exists; literal/caller-supplied success routes are absent. |
| W01-AC05 | verified | Default-off exact-six/zero-side-effect tests pass. |
| W02-AC01 | verified | Independent PE verifier reports AMD64 custom entry, KERNEL32-only admitted imports and required mitigations. |
| W02-AC02 | verified | Entry disassembly/real-child tests enforce ordered role handle revocation/readback. |
| W02-AC03 | verified | Closure builder rejects missing/extra/ambiguous/dynamic rows deterministically. |
| W02-AC04 | verified | Absent provision receipt exits 78/`native_loader_invalid` before package work. |
| W02-AC05 | verified | Closed isolated PyConfig contract and hostile ambient-input tests pass. |
| W02-AC06 | verified | Deterministic unsigned image/manifest verification passes; signing remains explicitly X-phase. |
| W03-AC01 | verified | Only exact CST/default identity can read the closed direct-image receipt; absence keeps legacy path/default-off. |
| W03-AC02 | verified | Otherwise-compatible SysProcAttr plus exactly one added handle is enforced. |
| W03-AC03 | verified | CNG, 32-byte/EOF, cancel-once, zeroization and all-return lifecycle guards pass. |
| W03-AC04 | verified | Held image/manifest are rechecked immediately before start; native child revokes all four frontend handles. |
| W03-AC05 | verified | No `internal/process`, HTTPHost or non-CST launch owner change exists. |
| W04-AC01 | verified | Fixed service roots require closed composition; missing composition creates no partial listener. |
| W04-AC02 | verified | Enrollment binds current supervisor/frontend, consumes one challenge/capability and exposes no reusable authority. |
| W04-AC03 | verified | Seventh tool remains absent without enabled inventory/package proof; exact six remains. |
| W04-AC04 | verified | Daemon owns admission/QPC; broker independently reauthorizes and owns source/workspace capabilities. |
| W04-AC05 | verified | Channel receipts derive only local framing/flush/ACK/EOF/close facts and incomplete receipts quarantine. |
| W04-AC06 | verified | Queued/disconnect/timeout/crash/shutdown settlement matrices pass. |
| W05-AC01 | verified | Atomic Job and ordered five-handle creation under one epoch passes containment guards. |
| W05-AC02 | verified | Native five-flag pre-main receipt gates request/application work. |
| W05-AC03 | verified | Shared Windows path grammar rejects alias/reparse/hardlink/stream ambiguity. |
| W05-AC04 | verified | Exact manifest capability transfer and destination equality/drift rejection pass. |
| W05-AC05 | verified | Session/vendor lease ownership survives through seal/close/settlement; no default receipt fields exist. |
| W05-AC06 | verified | All-return Job/readers/handles settlement and foreign-process exclusion tests pass. |
| W05-AC07 | verified | Vendor call-order and forbidden solver/history/mesh/1D guards pass; target CST behavior remains Claim 7. |
| W06-AC01 | verified | Focused Go real-child test crosses `StdioHost` capability delivery into native frontend; Python local-pipe topology tests observe owner receipts. |
| W06-AC02 | verified | Absent/invalid policy or provision receipt keeps exact six and performs zero package/source/vendor/CST work. |
| W06-AC03 | verified | Explicit synthetic non-live ports traverse four schemas/three endpoints to one bounded canonical text result without target claims. |
| W06-AC04 | verified | Receipt-bit, handle, schema, QPC, disconnect and residual-resource mutation matrices fail closed/quarantine. |
| W06-AC05 | verified | Existing-six fixtures pass and production Line10/VFEM/solve edges are absent. |
| W07-AC01 | verified | Terminal W7 artifact is hash-bound; fresh W8 native, focused Go and 598-test Python checks corroborate relevant architecture surfaces. The unrelated clean-HEAD `internal/api` compile defect remains outside candidate scope. |
| W07-AC02 | verified | Each architecture guard has named accepted RED/GREEN evidence; W8 reran the smallest load-bearing falsifiers. |
| W07-AC03 | verified | Immutable commit/tree, 66-path allowlist, diff hygiene and empty post-commit index receipt are exact. |
| W07-AC04 | verified | One endpoint registry, one spawn owner, one worker receipt route and no stale fake-settlement route remain. |
| W07-AC05 | verified | Commit/tree/content identities bind source, tests, native manifest and accepted artifacts; no live pin/publication mutation exists. |

## Fresh verification receipts

| Probe | Terminal result |
|---|---|
| CodeGraph MCP, Go/native/Python owner and call-path queries | Fresh candidate CST files; no candidate-path stale banner. One unrelated GUI file later reported pending. |
| `pwsh verify.ps1 -Unsigned` in archived native runtime | PASS; image SHA-256 `38D87C50F716E334F89628D4F35604534C3A35BDCF35F378F1D939137BAB89E3`. |
| Focused Go `Test.*(CstDirect|LaunchCapability|Enrollment|Supervisor|StdioHost)` | `internal/daemon` PASS 7.694s; `internal/cli` PASS 56.046s. `internal/api` failed to build only on the known clean-HEAD missing `newExcludedPortNetshCommand`; candidate contains no `internal/api` path. |
| Full archived `go test ./internal/daemon -count=1` | PASS 41.420s. |
| Full archived `go test ./internal/cli -count=1` | UNVERIFIED: command timed out after 304s without a terminal package result. Not used as PASS evidence; focused CST CLI gate above is terminal PASS. |
| Focused archived Python architecture set | PASS: 80 tests under CPython 3.13.11. |
| Broad archived Python selection over all saved-field/native plus existing-six tests | PASS: 598 tests; one pre-existing Pydantic forward-reference warning. |
| `git show --check` and exact candidate path count | PASS; 66 paths. |

The architecture gate does not substitute the timed-out broad CLI run with an inference. W8 requires the smallest named falsifiers, which have terminal PASS for the changed CLI/daemon launch surfaces. The broad CLI timeout remains an explicit QA/operational residual for W10 rather than a hidden architecture waiver.

## Blast radius and residual risk

The candidate adds a default-off working-result route across native, Go and Python owners but does not activate it in the installed fleet. Existing six tools and non-CST launch routes stay on their existing owners. Target signing, App Control, VHDX, CiTool, Service Control Manager provisioning, installed CST behavior, Claim 7, Claim 15, manifest migration, publication and deployment remain outside this candidate and must not be inferred from W8 PASS.

Residual architectural risk is bounded to the planned X-phase target composition and empirical compatibility gates. Any source or candidate-path change invalidates this review. The full `internal/cli` timeout is `UNVERIFIED`, not a candidate failure and not a green result.

## Gate

`PASS`

The immutable W7 candidate remains aligned with the accepted design and decision, satisfies all candidate-local W0-W7 architecture criteria, preserves the single-owner launch/containment/receipt topology, and leaves Claims 7 and 15 target-only. W9 may proceed to `$security-engineer`; this report does not authorize installation, registration, publication, push or deployment.

## Terms and Abbreviations

- App Control: Windows application-control policy enforcement.
- CNG: Windows Cryptography Next Generation random source.
- CST: Computer Simulation Technology electromagnetic solver.
- MCP: Model Context Protocol.
- PE: Portable Executable.
- QPC: Query Performance Counter monotonic deadline clock.
- SCM: Windows Service Control Manager.
- VHDX: Hyper-V virtual hard disk format.
- W0-W10: working-result phases in the accepted delivery plan.
