# Security Constraints Re-review — Supervisor-Bound Three-Endpoint Design

## Review identity and boundary

| Item | Exact reviewed value |
|---|---|
| Execution role | `$security-engineer` |
| Corrected design | `design.md`; SHA-256 `AFABC3C001169D5C571D7319EA2C751CDD228E46B335C9630C0516F6EBAE6DC9` |
| Corrected proposed decision | `work-items/decisions/2026-08-12-cst-saved-field-authority-containment.md`; SHA-256 `FD81F4B2B5C14F8AAA66FC96533B8CDE4A7AF2B5738F146794D5BDC6C57212AD` |
| Prior security constraints | This artifact before decision-only correction; SHA-256 `13CB198021AF207D10CF5A658EDFEA2D443DBA7CB87841228627083B3C9DCD64`; prior `PASS` was bound to the unchanged design and superseded decision hash |
| Prior architecture review | Supplied immutable review SHA-256 `9891078475577FCEDC76ABC870DD71DE45814636F4421C2EABBE088DB515E5AF`; its recorded enrollment-lifecycle and stale-owner findings were rechecked directly against the current immutable design/decision |
| Review scope | Decision-only closure of `AR-C6-DECISION-06`; regression check for all three descriptors, four protocol schemas, enrollment/frontend/broker lifecycles, split receipts, identities, entry authorization, QPC, broker-only source/output/containment, credentials and publication controls |
| Prohibited and not performed | Source/test/plan/decision/Git/index mutation; build/test/runtime execution; live SCM/CST/hub/fleet/process/service action; publication |

This is a security-design gate only. It does not attest that implementation,
provisioning, installed CST behavior, or release evidence conforms.

## Threat model and trust boundaries

| Boundary | Attacker position, reachable assets and entry points | Required control and disposition |
|---|---|---|
| MCP caller → frontend | Authenticated caller or prompt-injected content can choose `entry_id`, request fields and diagnostic-triggering values; assets are proprietary policy entries, CST capacity and agent context; entry point is the seventh MCP tool. | Closed bounded schema, non-authoritative selector, fixed operation, safe errors, bounded canonical result and no caller-controlled tool/policy/action dispatch. Daemon uniquely resolves `entry_id`; broker independently reauthorizes. Prompt/tool misuse is mediated by the fixed tool operation, policy allowlist and output validation. Satisfied (`design.md:18-20,631-686,1631`). |
| Same-owner local process → enrollment endpoint | Insider or compromised same-user process can race the pipe, clone the installed hub, replay a digest or claim PID/generation; assets are pending child authority and downstream admission. | Runtime-numeric protected DACL/High SACL/readback limits connection; DACL identity is not authority. Daemon authenticates the enrollment peer against the independently queried exact CST task row. Enrollment challenge, correlation, five-second expiry and separate channel/capability ledgers stop replay. Satisfied (`design.md:392-445`; decision `215-254`). |
| Daemon → supervisor status IPC | Compromised daemon or same-user pipe squatter can fabricate status or try supervisor control/other-target methods; assets are the enrollment trust root and all supervised tasks. | Client kernel-binds server PID/creation/token/session/canonical installed image to `SupervisorLockOwner`; server independently authenticates daemon service SID/token/session/integrity/image. Only pre-dispatch `GET_CURRENT_CST_TASK_IDENTITY_V1` with implicit `cst` is admitted; generic status/control/respawn/reconcile/exit/other target is denied. Numeric protected DACL, High SACL and exact ordered readback are mandatory. Satisfied (`design.md:401-426`; decision `218-233`). |
| Hub spawn owner → frontend child | Compromised sibling or ambient channel can steal capability bytes or inherit handles; assets are the per-child bearer capability. | Exact CNG 32-byte generation, SHA-256 non-secret verifier, constant-time compare after peer authentication, exact stdio-plus-read-handle allowlist, non-inheritable write end, non-secret decimal locator only in environment, exact read+EOF, all-return close and `SecureZeroMemory`. Satisfied (`design.md:48-78`; decision `235-254`). |
| Frontend → SCM daemon | Same-owner clone, stale child, PID reuse, captured request, reconnect, second frame, cancel or death can seek admission; assets are policy entry, broker authority and QPC budget. | Live PID/creation/image/package/parent/generation binds to the ENROLLED capability; fresh CNG frontend nonce, correlation/request hash and atomic consume precede admission. Replay and ambiguity do zero privileged work or quarantine. Satisfied (`design.md:447-473`). |
| Daemon response → frontend | Either peer can falsely claim the other's later close or publish/release on partial settlement; assets are public result and later admission. | Daemon-local `DaemonResponseReceiptV1` alone gates release/quarantine after writes, flush, ACK, disconnect and local close. Frontend-local `FrontendTransportReceiptV1` alone gates publication after reads, EOF/cancel and local close. Neither asserts remote facts. Satisfied (`design.md:26-27,306,531-544,776-784`; decision `88-102`). |
| SCM daemon → broker | Local insider, wrong service, stale PID, anonymous/network client or replay can seek source/worker authority; assets are source capability, protected workspace, worker and license. | Fixed local first-instance pipe, numeric SID descriptor readback, mutual SCM process/token/session/integrity/image proof, impersonation/revert, one-use CNG nonce, correlation/request/policy/manifest binding and unchanged QPC triple. Satisfied (`design.md:474-518`; decision `191-206,255-283`). |
| Broker → source/workspace/worker/output | Compromised worker or namespace racer can swap inputs, forge output, break away, leave descendants or synthesize settlement; assets are proprietary bytes and host processes. | Broker alone reads `MCPHUB_EM_OUTPUT_ROOT`, injects trusted workspace policy, opens/copies stable policy-derived source capabilities, owns distinct-principal workspace and worker Job. Retained locks, writer-close/share-zero output seal, atomic no-console/no-breakaway launch, exact-handle escaped probe and kernel-only containment receipt fail closed. Satisfied (`design.md:308-341,791-1091,1314-1316,1575`). |
| Credentials/capabilities → logs, frames and release | Insider, compromised dependency or CI pipeline can collect secrets or untrusted diagnostics; assets are service tokens, launch capability, license/source data and publication safety. | Service credentials remain SCM/LSA-owned; launch bytes exist only in zeroed local buffers and one inherited pipe. Allowlist-built frames/diagnostics/output exclude secrets, paths and raw payload. Cross-channel canaries and publication leak-check fail closed. No new dependency is introduced. Satisfied at design level (`design.md:374-389,1264-1304`). |

## Prior-finding disposition

| Finding | Current disposition and evidence |
|---|---|
| `SEC-C6-D-01` — no exact multi-entry selector | Corrected: request carries non-authoritative unique `entry_id`; daemon resolves it; broker independently authorizes; no implicit default (`design.md:19,306-307,631-633`). |
| `SEC-C6-D-02` — no exact hub-child/replay proof | Corrected by authenticated supervisor task row, per-child capability and frontend challenge ledgers (`design.md:401-473`). |
| `SEC-C6-D-03` — missing frontend terminal receipt | Corrected by separate exact owner-local daemon/frontend receipts (`design.md:26-27,531-544`). |
| `SEC-C6-D-04` — stale admission/QPC/broker ownership | Corrected: SCM daemon alone owns admission/QPC/broker client; frontend explicitly never does (`design.md:302-306,520-544`). |
| `SEC-C6-D-05` — enrollment caller not authenticated | Corrected: supervisor server is kernel-bound to lock owner/installed identity and task row; enrollment peer is compared to that independent row (`design.md:401-418`). |
| `SEC-C6-D-06` — impossible combined post-close receipt | Corrected by ACK-before-disconnect and independent local receipt gates with no remote-close claim (`design.md:531-544`). |
| `SEC-C6-D-07` — cryptography/HANDLE_LIST mechanically open | Corrected by exact CNG/SHA-256/constant compare, four-handle allowlist, locator, EOF, close and zeroization contract (`design.md:48-78`). |
| `SEC-C6-D-08` — supervisor oracle spoofable | Corrected by `GetNamedPipeServerProcessId` plus exact process creation/token/session/image/lock-owner equality before status (`design.md:401-406`). |
| `SEC-C6-D-09` — supervisor DACL expansion granted generic control | Corrected by exact daemon authentication and pre-dispatch implicit-CST status-only opcode; all generic/control/other-target methods deny (`design.md:407-426`). |
| `SEC-C6-D-10` — stale receipts/ledger/pipe count | Corrected: exactly three feature pipe endpoints, three channel receipts, explicit enrollment/frontend/broker ledgers, daemon-local release and frontend-local publication (`design.md:276,304-306,1300-1302,1636-1639`; decision `304-311`). |
| `AR-C6-ENROLL-04` — ACK consumed authority too early | Corrected: channel nonce terminates independently; capability moves `ISSUED -> ENROLLED -> CONSUMED|CANCELLED`; fresh authenticated cancellation covers post-ACK failures (`design.md:428-445`; decision `243-254`). |
| `AR-C6-RESIDUE-05` / output-root owner | Corrected: topology names pre-spawn enrollment plus application route, Claim 28/30 name three endpoints/receipts, and only SCM broker reads the output root (`design.md:7-12,520-544,1314,1575,1636-1638`). |
| `AR-C6-DECISION-06` — decision omitted enrollment topology and full promotion coverage | Corrected without security regression: normative decision topology now names pre-spawn `StdioHost -> HubEnrollmentProtocolV1 -> SCM daemon` and the complete application route; lifecycle/restart/promotion name all three descriptors, all four protocol schemas, enrollment capability lifecycle, supervisor identity and owner-local daemon/frontend receipts (`decision:284-307,311-334,368-379`). |

No remediable security-design finding remains in the exact reviewed inputs.

## Credential and capability four boxes

| Secret or credential | Storage owner | Injection path | Log/serialization exclusion | Rotation/revocation owner |
|---|---|---|---|---|
| Daemon/broker service identity | LSA/SCM; no password or application-readable secret | SCM creates exact session-0 virtual-service tokens; broker worker inherits broker token | Policy, argv, environment, frames, logs, diagnostics, dumps and MCP output exclude token/SID/password/license values | Elevated installer/operator stops exact services, proves processes/Jobs/pipes/workspaces/old ACLs/tokens absent, rotates pinned package or creates successor identity, then revalidates |
| Per-child launch capability | StdioHost locked 32-byte buffer, one anonymous pipe, child buffer; daemon stores only non-secret SHA-256 verifier | Exact inherited read handle in child HANDLE_LIST; environment carries only non-secret decimal locator | Capability bytes excluded from environment, argv, manifest, intent, frames except authenticated proof request, logs, diagnostics, dumps and MCP output; canary scan fails closed | StdioHost and daemon enrollment owners consume/cancel one entry on child proof, failure, expiry, exit, shutdown or restart; close handles and zero buffers |
| Frontend/broker challenge nonces | Restart-scoped bounded daemon/broker ledgers only | Generated by exact CNG call and returned only on authenticated local pipes | No raw nonce in public result/log/diagnostics; correlation-only bounded receipts | Respective ledger owner atomically consumes/cancels on every exit; ambiguity quarantines and restart clears only after full revalidation |

## Required abuse-case verification

| Abuse case | Required safe result |
|---|---|
| Same-owner supervisor-pipe squatter echoes lock hello/task row | Kernel server identity mismatch rejects before status/enrollment; zero admission/broker work. |
| Daemon service token sends control, other-target or generic status opcode | Pre-dispatch denial with zero supervisor/task mutation; exact implicit-CST identity query alone succeeds. |
| Enrollment replay, ACK loss, post-ACK child-create/write/read failure or fresh cancel | Channel nonce terminal; capability remains ENROLLED only when valid, then becomes exactly CONSUMED or CANCELLED; digest removed; handles/buffers settled. |
| Capability short/zero/overlong read, sibling inheritance or environment/log canary | Child rejected; sibling has no handle; capability absent from ambient/public channels; quarantine on ambiguous cleanup. |
| Frontend partial result, missing ACK, flush/EOF/local-close failure | Daemon release and frontend publication obey separate local gates; no remote/future close is asserted. |
| Unknown/swapped/duplicate `entry_id`, replayed broker nonce or altered QPC triple | Zero source/worker/CST work; original deadline is never rebased. |
| Source alias/swap/write, output writer still open, breakaway or broker death | Stable locks/output seal/Job containment fail closed; owned tree settles; foreign CST is untouched; later admission quarantines on missing proof. |
| Prompt injection in caller/vendor strings | No policy, executable, service, source, cleanup or tool action is selected; bounded allowlisted output contains no raw injected diagnostic. |

## Exact 18 S4 security claims

1. `{ guarantee: Every invocation selects exactly one already-authorized policy entry without caller data creating authority and unknown swapped duplicate stale or ambiguous selection performs zero broker source worker or CST work; single-owner: SCM-daemon AuthoritySnapshot resolution followed by broker authorization; enforcement-probe: one-entry and two-entry valid swapped unknown duplicate mismatch stale-revision and zero-work matrix }`.
2. `{ guarantee: Enrollment trusts only a kernel-authenticated current supervisor server bound to SupervisorLockOwner installed identity and its exact current CST task row while self-reported hello digest PID image generation and policy-owner SID grant nothing; single-owner: daemon supervisor-status authentication owner; enforcement-probe: genuine supervisor versus first-instance squatter stale owner PID reuse token session image owner-change fabricated-row and missing-lock matrix }`.
3. `{ guarantee: Exact daemon service identity may invoke only bounded pre-dispatch GET_CURRENT_CST_TASK_IDENTITY_V1 with implicit cst and can invoke no control generic status other-target respawn reconcile or exit operation; single-owner: SupervisorCstIdentityAuthorizerV1; enforcement-probe: exact service-token opcode allowlist denial matrix plus before-after supervisor task-state equality }`.
4. `{ guarantee: Enrollment supervisor frontend and broker pipe descriptors use runtime numeric SIDs protected DACL High-integrity SACL exact ordered ACE masks and post-create readback before accept or dispatch; single-owner: each endpoint descriptor owner at its process boundary; enforcement-probe: exact descriptor readback plus mutated owner control ACE order SID mask inheritance label anonymous network interactive and second-instance matrix }`.
5. `{ guarantee: One CNG-generated 256-bit per-child capability reaches only that child through one inherited read-once anonymous-pipe handle while daemon retains only a non-secret SHA-256 verifier and every buffer handle and ledger entry settles on every exit; single-owner: StdioHost spawn owner followed by daemon capability-ledger owner; enforcement-probe: CNG digest constant-compare exact HANDLE_LIST sibling-read locator short-overlong read start-failure cancel timeout exit zeroization and leak-canary matrix }`.
6. `{ guarantee: Enrollment channel nonce terminates independently while capability authority moves exactly ISSUED to ENROLLED to CONSUMED or CANCELLED and ACK loss post-ACK failure fresh cancel expiry exit shutdown restart duplicate and replay cannot strand or reuse authority; single-owner: HubEnrollmentProtocolV1 channel ledger followed by capability enrollment ledger; enforcement-probe: deterministic state-handle table for every transition and injected return }`.
7. `{ guarantee: Every frontend request binds authenticated enrollment fresh CNG challenge correlation request hash restart generation entry and original deadline and consumes both enrollment and frontend nonce before admission; single-owner: SCM-daemon FrontendChallengeLedger and admission owner; enforcement-probe: clone stale child PID reuse replay reconnect trailing second-frame cancel timeout shutdown and frontend-exit matrix }`.
8. `{ guarantee: SCM daemon alone owns frontend authentication entry resolution admission policy generation original QPC triple broker client daemon-local response receipt and quarantine while frontend owns existing-six compatibility client-local receipt and publication; single-owner: SCM-daemon composition followed by frontend publisher; enforcement-probe: whole-artifact dependency owner receipt handle and zero-direct-route graph }`.
9. `{ guarantee: Broker pipe admits only current SCM daemon whose exact token service SID logon session integrity privileges image and SCM process pass mutual authentication impersonation and revert before work; single-owner: broker Windows pipe authentication owner; enforcement-probe: descriptor anonymous network foreign-service stale-PID token session integrity privilege image second-client impersonation and failed-revert matrix }`.
10. `{ guarantee: Every broker request is CNG challenge correlation unchanged deadline policy entry manifest and request-hash bound with atomic one-use nonce consumption or cancellation on every exit; single-owner: BrokerProtocolV1 NonceLedger owner; enforcement-probe: issue cancel consume expiry replay policy hash framing disconnect timeout shutdown and service-stop matrix }`.
11. `{ guarantee: Only broker opens policy-derived stable source capabilities reads ambient output root copies every complete manifest row directly into protected workspace and preserves no-follow locks until consumption; single-owner: SCM-broker TrustedWorkspacePolicy and AuthorizedBundleTransfer owner; enforcement-probe: ambient-reader dependency scan plus rename reparse hard-link stream short-name ancestor leaf swap and complete-manifest byte-continuity matrix }`.
12. `{ guarantee: Every write-capable project or header stays in distinct broker-principal workspace and unknown output becomes input only after writer close share-zero seal identity recheck and hash on every return; single-owner: AuthorizedWorkspaceSnapshot and AuthorizedVendorPathLease owner; enforcement-probe: same-user writer lazy-read output-seal post-consumption hash and every-return matrix }`.
13. `{ guarantee: Broker alone owns exact worker process thread Job stream watchdog and token handles and containment success uses only actual kernel signal exit reference-close active-zero reader handle residual timeout exit-code stderr and first-instruction facts without synthesized defaults; single-owner: WindowsContainedInvocation; enforcement-probe: creation transfer one-field-missing contradictory residual cancel crash breakaway and every-return matrix }`.
14. `{ guarantee: Daemon and frontend each attest only local terminal facts with ACK before disconnect so daemon receipt alone gates release or quarantine and frontend receipt alone gates publication without remote or future close claims; single-owner: DaemonResponseReceiptV1 and FrontendTransportReceiptV1 owner-local boundaries; enforcement-probe: partial frame flush missing-ACK EOF cancel trailing correlation server-close client-close and event-order matrix }`.
15. `{ guarantee: One unchanged integer QPC frequency admitted tick and deadline tick triple crosses daemon broker and worker with no rebase so worker response ends two seconds before original expiry publication ends at expiry and only cleanup gets ten seconds after termination; single-owner: SCM-daemon AbsoluteInvocationBudget owner; enforcement-probe: altered field frequency mismatch future expired transport delay queue delay every-stage block and post-expiry zero-work matrix }`.
16. `{ guarantee: Existing six tools retain local cst.py paths and only seventh crosses one authenticated enrollment endpoint frontend endpoint broker endpoint and broker-owned worker with no alternate authority source workspace result or obsolete route; single-owner: cst.py compatibility owner followed by enrollment daemon and broker; enforcement-probe: exact six-tool regression three-feature-endpoint four-protocol three-receipt dependency scan and one-route trace }`.
17. `{ guarantee: Installed CST runs as one fresh fixed broker-identity worker accepts retained input locks and share-zero output sealing keeps every descendant in exact Job rejects breakaway produces no console or hidden solve path and fails registration on incompatibility without fallback; single-owner: version-bound installed CST and broker admission record; enforcement-probe: disposable target Windows service-token pipe path-lock output-seal ResultTree descendant breakaway no-window hidden-call license COM and foreign-preservation trace }`. **TARGET-ONLY — NOT INFERRED OR VERIFIED BY THIS DESIGN REVIEW.**
18. `{ guarantee: Credential-free fixed services exactly three protected feature pipes and per-child capability provision rotate revoke and roll back only under exact owners and incomplete stale wrong or replaced state leaves seventh tool unregistered with no credential verifier nonce token process Job workspace or authority reuse while existing six remain live; single-owner: service provisioner plus supervisor enrollment and restart-admission owners; enforcement-probe: credential-capability four boxes three-descriptor readback partial-provision reverse rollback rotation revocation old-resource absence and restart-revalidation matrix }`.

Claims 1–18 each name one owner or one explicit sequential trust boundary and a
falsifying probe. Claim 17 is deliberately target-only; prose, mocks and static review
cannot promote it.

## Residual empirical obligations

- Verify the exact installed supervisor/enrollment/frontend/broker descriptor and mutual
  process/token bindings, status-only opcode denial and ledger transitions on disposable
  Windows services before registration.
- Run clone/squatter/PID-reuse/replay/cancel/death/partial-frame/handle-leak abuse matrices
  and prove all ledgers/resources terminal or quarantine.
- Run target CST path-lock, output-seal, Job-descendant, breakaway, no-console, license,
  Component Object Model and foreign-process traces. Claim 17 stays fail-closed until then.
- Perform independent publication-safety leak scanning and the separate
  `$security-reviewer` gate before any implementation or publication promotion.

## Gate

**PASS — ready for independent `$security-reviewer` design review.** The exact
unchanged design and corrected proposed decision close every prior remediable security-design
finding: supervisor status is kernel-bound to the lock owner and installed identity;
daemon access is exact implicit-CST status-only; enrollment/supervisor/feature pipes have
numeric protected descriptors and readback; enrollment uses separate channel and
`ISSUED -> ENROLLED -> CONSUMED|CANCELLED` authority lifecycles; CNG, SHA-256,
constant-time comparison, HANDLE_LIST, locator and zeroization are exact; daemon and
frontend receipts remain owner-local; topology has exactly three feature endpoints; and
broker alone owns source/output/worker containment. `AR-C6-DECISION-06` is closed: the
decision now carries the enrollment topology, all three descriptors, all four schemas,
both local receipt gates and the complete promotion coverage. Claim 17 remains target-only and is
not inferred. This PASS authorizes only the next independent security-reviewer design
gate; it does not authorize implementation, provisioning, registration, release,
publication, deployment or live action.

## Terms and Abbreviations

- ACK: Acknowledgement frame.
- CNG: Windows Cryptography API: Next Generation.
- COM: Component Object Model.
- CST: Computer Simulation Technology electromagnetic solver suite.
- DACL: Discretionary access-control list.
- EOF: End of file; transport-observed peer completion.
- IPC: Inter-process communication.
- Job: Windows Job Object controlling one owned process tree.
- LSA: Local Security Authority.
- MCP: Model Context Protocol.
- PID: Process identifier; not sufficient authority by itself.
- QPC: Windows Query Performance Counter.
- SACL: System access-control list.
- SCM: Windows Service Control Manager.
- SHA-256: Secure Hash Algorithm with a 256-bit digest.
- SID: Windows security identifier.
