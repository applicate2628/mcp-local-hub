# W9 Security Engineering Re-review — Corrected Immutable Candidate

Date: 2026-08-13

Execution role: `$security-engineer`

Review target: commit `310ea13bb63a6d4b072a2617ca3acec756f1a2b6`, tree
`88095de688c88a016dcce88e7aca7d0b22270022`, 16-path correction delta, 80-path
cumulative candidate, content SHA-256
`D953A814C7AF42330AEC825123AE6CD4EA4EEE8E1C34D1FDE7D33257094DDA83`.

| Bound input | SHA-256 |
|---|---|
| Accepted `design.md` | `7423D56DD33394336A06AB8C515D12F4496B4AC0533F2901BF5A0EE1436756ED` |
| Accepted authority-containment decision | `8EAD15D041781A05ED192107D997693E27B79175C0D0125BB09F1C0A6DE8696A` |
| W8 successor architecture review | `261A57B6B6D3758406BAFB8C93CD2CEFF32AE035AA7A8823038ADC1AA20EA42A` |
| W10 correction | `423A714B6921EEDF3A37AFF2709A1A7EEC515598BA356C31C955EF4C77E94717` |
| Successor candidate receipt | `A3ADB7A0CCFBE17FAC2866C34A7C6ABB341AADE00D227D08EDDD139BC667CF19` |

Gate-bearing source and probes came from a clean `git archive` of the exact
commit under `/.scratch/`; the shared worktree, Git index, live services, CST,
Service Control Manager (SCM), App Control, virtual hard disk (VHDX), CiTool,
hardware security module (HSM), manifest and publication state were not
mutated. CodeGraph MCP was used first; `codegraph status` then reported 2,117
files and `[OK] Index is up to date`. Current-worktree navigation was rebound to
immutable source before it drove a conclusion.

## Threat model and trust boundaries

| Boundary | Attacker position | Reachable assets and entry points | Required control |
|---|---|---|---|
| MCP caller to saved-field frontend | unauthenticated remote | request schema, bundle identifier, sampled result | Closed request schema; caller selects no path, process, pipe, service or policy authority; bounded redacted result only. |
| Go supervisor to native frontend | insider or compromised dependency | executable identity, capability and inherited handles | Existing `StdioHost` remains sole spawn owner; held image/manifest identities, exact handle list and all-return cancellation are mandatory. |
| Frontend, daemon and broker pipes | malicious local process or insider | enrollment capability, nonce, policy revision, source/workspace authority | Fixed local endpoints, authenticated current peer identities, one-use ledgers, unchanged monotonic deadline and owner-local receipts. |
| Broker to worker and vendor | insider or compromised dependency | source/workspace handles, Job, package admission and CST session | Serialized inheritance epoch, exact five-handle tuple, native pre-main proof, least-right handles, no path fallback and complete containment settlement. |
| Native package boundary | compromised dependency | embedded Python and package code execution | Handle revocation and exact package receipt precede package code; hostile ambient path/environment input cannot enable execution. |
| Test binary bootstrap | CI pipeline or malicious ambient environment | package sandbox, helper selector, package test execution and test result | A pre-root helper route must authenticate an exact helper protocol tuple; partial, conflicting, unknown or selector-mismatched state must fail nonzero before either `m.Run` or success exit. |
| Signing, provisioning and deployment | CI pipeline or insider | policy authority, signing key, VHDX, service identities and target CST | Target-only X1-X6 receipts remain mandatory; absence keeps the seventh tool default-off and authorizes no protected work. |

No large-language-model output crosses a production authority boundary. Agent
analysis cannot create a runtime receipt or activate the seventh tool.

## Required controls, finding and implementation constraints

| Priority | Control or finding | Owner and evidence | Required falsifier or correction |
|---|---|---|---|
| high | Native pre-entry, capability, endpoint, path, receipt, timeout, containment, redaction and default-off controls remain intact. | Existing Go/native/Python owners are unchanged by the correction; exact-snapshot focused and full accepted evidence remains applicable. | Mutation of identity, handle count/order, nonce, deadline, source/workspace identity, receipt bit or provision proof must deny before protected work or quarantine incomplete settlement. |
| medium | **Must-fix: pre-root TestMain dispatch trusts ambient environment or a bare positional argument without binding it to the exact helper selector/framing.** CLI `settings_registry_test.go:362-377` returns success for bare `route`/`supervise` and calls `m.Run` for any one of four sentinels. GUI `main_test.go:187-200` likewise accepts a blocking-helper sentinel or bare worker argv. | Compiled immutable binaries reproduced: spoofed CLI sentinel plus `-test.run=^$` exited 0; spoofed GUI sentinel plus `-test.run=^$` exited 0; raw CLI `supervise` exited 0. In every case package sandbox setup was bypassed. Attacker effort is one environment variable or argv token; blast radius is CI/test assurance and the ability to mask that expected tests did not run. | Bind every helper to one exact tuple of sentinel value, exact `-test.run` selector and required helper fields; bind production-shaped route/supervise children to an exact protocol marker/argv grammar. Reject sentinel-only, selector-only, wrong selector, partial required fields, conflicting helpers and unknown/raw route/supervise nonzero. Tests must prove each denial and the exact authorized helper still works without creating a package root. Do not silently exit success on incomplete framing. |
| low | `apitest.RemoveTestMainRoot` accepts a caller-provided root and uses `os.RemoveAll` without independently proving containment or reparse ancestry. | The API is compiled only into test binaries, current callers pass roots returned by their own `os.MkdirTemp`, empty input fails, retries are bounded, residue is fail-visible, and no production call path exists. No exploitable candidate route was demonstrated. | Keep the helper test-only and caller-owned. If a future caller accepts ambient/path-derived input, first add canonical parent containment and Windows reparse-point denial; that future widening requires a new security review. |
| target-only | App Control, HSM, VHDX, CiTool, SCM provisioning, installed CST, Line10, manifest promotion and deployment are absent. | X1-X6 target owners. | Exact target receipts; local fakes cannot waive them. |
| publication | Publication safety is independently blocked. | Candidate receipt records seven unchanged drive-rooted synthetic fixture literal findings and no `$security-reviewer` exception. | Replace/authorize them through the canonical publication process and obtain a fresh clean scanner result. This `$security-engineer` review cannot waive the block. |

The event-loop join is not a finding: `runSupervise` owns creation, cancellation
and join; `EventLoop.Run` observes cancellation at receive boundaries, and the
fresh held-dispatch test passed ten times. A handler already executing may delay
shutdown until its owned operation settles, but the correction adds no second
lock or circular wait. The netsh change is also clean: one existing Windows
constructor owns the command and applies the shared no-console policy.

## Secret and sensitive-value four boxes

| Item | Storage owner | Injection path | Log/serialization exclusion | Rotation/revocation owner |
|---|---|---|---|---|
| Frontend capability | Go launch owner, one 32-byte buffer and anonymous pipe | Exact fourth frontend handle; only a decimal non-secret locator is ambient | Raw value excluded from argv, logs, manifest and public result; buffer zeroed | Launch/enrollment owners consume or cancel on every terminal return, timeout, shutdown and restart |
| Enrollment/challenge/broker nonces | Bounded owner-local memory ledgers | Fixed local protocol frames bound to identity, correlation, policy and deadline | Excluded from public result and canonical artifacts | Each ledger consumes/cancels on success, failure, expiry, disconnect, exit, shutdown and restart |
| Source/workspace capabilities | Broker handle table, then native worker table | Exact ordered five-handle inheritance epoch | Raw paths, handle values and tokens excluded from public output | Broker, worker and Job owners revoke inheritance and close copies; ambiguity quarantines |
| Policy-signing private key | Target-only HSM owner | X1 exact signing ceremony; no candidate path | No key, PIN, credential or endpoint exists in this candidate | X1 owner must prove card removal, unload, audit continuity and rotation/revocation |

## Abuse cases and fresh exact-snapshot evidence

| Abuse case | Result |
|---|---|
| Hold startup reconcile dispatch, request supervisor exit | PASS 10/10: exit did not return before owned dispatch settled, then completed after release. No deadlock/cancellation abuse observed. |
| Exercise shared exact-root cleanup | PASS 10/10 across three tests: bounded retry, residue rejection and cause preservation. Current exact test-owned callers are safe; path-authority widening remains prohibited. |
| Exercise five admitted CLI helper protocols | PASS 5/5; no fresh package root. This proves authorized flows, not fail-closed selector binding. |
| Spoof CLI helper sentinel with empty selector | **FAIL security expectation:** immutable compiled binary printed `PASS` and exited 0 without package sandbox setup. |
| Spoof GUI blocking-helper sentinel with empty selector | **FAIL security expectation:** immutable compiled binary printed `PASS` and exited 0 without package sandbox setup. |
| Invoke immutable CLI test binary with raw `supervise` argv | **FAIL security expectation:** binary announced immediate exit and returned 0; no exact child marker or framing was required. |
| Run real GUI blocking-helper regression | PASS 5/5 for `TestAuditLockTerminalWorkerCancellationAfterAcquisitionReapsBeforeReturn`. |
| Substitute CST executable/handle/nonce/path/receipt or activate without provision | Unchanged accepted candidate-local controls remain fail-closed; this correction changes no CST production/native/Python mechanism. Claim 17 remains target-only. |

## Exact 18 S4 implementation claims

The table contains exactly 18 claim rows. `verified` is candidate-local proof;
`failed` is a remediable successor-candidate defect; `not-verifiable
(target-only)` requires target evidence.

| Claim | Verdict | Guarantee, single owner and enforcement probe |
|---:|---|---|
| 1 | verified | `{ guarantee: caller input selects no authority and stale, swapped, duplicate or ambiguous entry/revision performs zero protected work; single-owner: daemon authority snapshot then broker reauthorization; enforcement-probe: entry/revision zero-work matrix }` |
| 2 | verified | `{ guarantee: enrollment trusts only the kernel-authenticated current supervisor and exact CST task process; single-owner: daemon supervisor-status authenticator; enforcement-probe: PID-reuse, squatter, token, session, image and fabricated-row tests }` |
| 3 | verified | `{ guarantee: daemon identity receives only the closed CST identity operation; single-owner: supervisor CST identity authorizer; enforcement-probe: opcode substitution and state-identity inequality tests }` |
| 4 | verified | `{ guarantee: exactly three fixed local endpoints enforce protected descriptors and peer identity; single-owner: endpoint descriptor/authenticator owners; enforcement-probe: endpoint count, remote peer, descriptor, second-instance and impersonation-revert tests }` |
| 5 | verified | `{ guarantee: one capability reaches only the exact native frontend and all four handles are revoked before package code; single-owner: Go launch owner then native frontend; enforcement-probe: exact-handle and real-child pre-entry tests }` |
| 6 | verified | `{ guarantee: enrollment nonce and capability ledgers terminate independently without replay or stranded authority; single-owner: enrollment ledgers; enforcement-probe: ACK-loss, cancel, expiry, reconnect, exit, shutdown and restart tests }` |
| 7 | verified | `{ guarantee: frontend request binds direct image, capability, challenge, correlation, hash, generation, entry and deadline; single-owner: daemon frontend admission owner; enforcement-probe: replay and bound-field mutation tests }` |
| 8 | verified | `{ guarantee: daemon owns admission/broker routing while same-process cst.py owns compatibility/publication; single-owner: daemon composition then frontend publisher; enforcement-probe: call-path and no-direct-route tests }` |
| 9 | verified | `{ guarantee: broker admits only the current SCM daemon after peer checks, impersonation and proved revert; single-owner: broker authenticator; enforcement-probe: peer-field and failed-revert matrices }` |
| 10 | verified | `{ guarantee: nonce, correlation, request, policy, manifest and monotonic deadline settle atomically; single-owner: broker nonce ledger; enforcement-probe: replay, framing, timeout, disconnect and shutdown tests }` |
| 11 | verified | `{ guarantee: broker alone creates least-right source/workspace capabilities and missing readback creates no child or path fallback; single-owner: broker capability owner; enforcement-probe: path, access/share, reparse, identity, writer and transfer mutations }` |
| 12 | verified | `{ guarantee: worker revokes exactly five handles before exact-policy package code or closed Python; single-owner: native worker pre-main then package-load owner; enforcement-probe: PE verifier, handle-receipt mutations and absent-package child test }` |
| 13 | verified | `{ guarantee: no sibling or descendant retains worker handles; single-owner: inheritance epoch, native worker and Job owner; enforcement-probe: re-entry, handle-list, descendant and cleanup tests }` |
| 14 | verified | `{ guarantee: owner-local receipts prove only observed facts and cannot be forged/defaulted; single-owner: typed channel, worker, application and containment receipt owners; enforcement-probe: unknown, missing, contradictory and residual matrices }` |
| 15 | verified | `{ guarantee: one unchanged QPC triple crosses daemon, broker and worker and only cleanup gets a post-termination budget; single-owner: daemon invocation budget; enforcement-probe: frequency/tick/deadline mutation and staged-delay tests }` |
| 16 | verified | `{ guarantee: existing six retain their production contract and the seventh has one protected route with no fallback; single-owner: direct launch/package owners and daemon/broker composition; enforcement-probe: existing-six fixtures, topology and stale-route scans }` |
| 17 | not-verifiable (target-only) | `{ guarantee: installed CST accepts locked inputs and sealed output inside the exact non-breakaway hidden Job while foreign CST survives; single-owner: installed CST and broker admission record; enforcement-probe: X3 disposable-target identity, descriptor, ResultTree, Job, cleanup and foreign-process trace }` |
| 18 | failed | `{ guarantee: dependencies, test/CI evidence, signing authority, policy/package identities, capabilities, descriptors and rollback remain under exact owners, and incomplete state cannot produce an authorizing success; single-owner: test TestMain protocol dispatch for the candidate assurance boundary, then target provisioners and current runtime owners; enforcement-probe: sentinel-only, selector-only, wrong-selector, conflicting-helper and raw/incomplete route/supervise invocations must all exit nonzero, while exact helper tuples pass and create no package root; observed failure: three incomplete/spoofed invocations exited 0 }` |

Matrix total: exactly 18 claims; 16 `verified`; 1 `failed` (Claim 18); 1
`not-verifiable (target-only)` (Claim 17).

## Residual risk and advancement boundary

The CST runtime controls remain default-off and unchanged, but the successor
candidate's correction introduced an ambient-input test-bypass owner whose
success result is not bound to the helper protocol it purports to dispatch.
That is a remediable security-assurance defect, so W9 cannot advance to the
independent security reviewer until the owning implementation phase corrects
it and W8 plus W9 are rerun against a new immutable candidate. App Control,
HSM, VHDX, CiTool, SCM, installed CST, Line10, publication and deployment stay
open regardless of that correction.

## Gate

`REVISE`

Required correction: make CLI and GUI pre-root TestMain dispatch default-closed
and exact-protocol-bound as specified above, with adversarial negative tests.
Any source change creates a successor candidate and invalidates W8 onward.
Publication is additionally and independently `BLOCKED` by the seven unchanged
synthetic fixture findings; no exception exists and this role does not waive it.

## Terms and Abbreviations

- App Control: Windows application-control policy enforcement.
- CI: continuous integration.
- CST: Computer Simulation Technology electromagnetic solver.
- HSM: hardware security module.
- MCP: Model Context Protocol.
- QPC: Query Performance Counter monotonic clock.
- S4: accepted numbered security-claim set.
- SCM: Windows Service Control Manager.
- VHDX: Hyper-V virtual hard disk format.
