# Independent Security Review — Supervisor-Bound Three-Endpoint Design

## Review identity and immutable boundary

| Item | Exact reviewed value |
|---|---|
| Execution role | Independent `$security-reviewer`; upstream author verdict was not reused as proof |
| Corrected design | `design.md`; SHA-256 `AFABC3C001169D5C571D7319EA2C751CDD228E46B335C9630C0516F6EBAE6DC9` |
| Corrected proposed decision | `work-items/decisions/2026-08-12-cst-saved-field-authority-containment.md`; SHA-256 `FD81F4B2B5C14F8AAA66FC96533B8CDE4A7AF2B5738F146794D5BDC6C57212AD` |
| Independent architecture input | `architecture-review.md`; SHA-256 `18499E40CC82236F9EA256F988BB7F48342806240A1EC710E7739978BCF7601E`; input verdict `PASS`, checked only as corroboration |
| Security claims input | `security-constraints.md`; SHA-256 `A0F0D2CEF3BA016D4E4E607755D643F5415F2D56F2C9BF99E848481498A81A12`; exact 18 claims |
| Review phase | Corrected design and durable proposed decision, not implementation or target runtime |
| Prohibited and not performed | Design/decision/source/test/plan/Git/index/status/ledger mutation; build/test execution; live SCM/CST/hub/fleet/process/service/pipe action; publication |

`verified` below means the design contract has one owner, complete fail-closed behavior
and a named falsifier. It does not claim implementation or target evidence. Runtime
probes were not run because this review is explicitly immutable and design-only; their
required later execution is recorded per claim. Claim 17 remains target-only.

## Reviewed security surfaces

| Checklist surface | Result | Independent falsification result |
|---|---|---|
| Untrusted input, deserialization and path traversal | found | Closed canonical schemas, exact byte/count/depth ceilings, non-authoritative `entry_id`, one Windows path grammar, no-follow identities, complete manifest and fixed errors prevent parser/path input from selecting authority. Unknown/swapped/default entry has zero privileged work. |
| Server-side request forgery | not-applicable | All source/workspace forms are local drive objects; remote, mapped, UNC, device and network paths are rejected and the sampler defines no outbound network request. |
| Object-level authorization | found | Daemon resolves exactly one immutable policy entry and broker independently reauthorizes revision/entry/manifest before source access; caller hashes, hub visibility, prior call and direct pipe access grant nothing. |
| Supervisor/enrollment identity | found | Status client kernel-binds connected supervisor server PID/creation/token/session/canonical installed image to `SupervisorLockOwner`; server independently authenticates daemon service SID/token/session/integrity/image. First-instance squatter and self-reported identity are explicitly non-authoritative. |
| Supervisor authorization change | found | Daemon service SID is admitted only to pre-dispatch `GET_CURRENT_CST_TASK_IDENTITY_V1` with implicit `cst`. Generic status, control, respawn, reconcile, exit and other-target requests deny before generic dispatch. |
| Endpoint descriptors | found | Enrollment, frontend and broker are exactly three new sampler pipes. Runtime numeric SIDs, protected DACL, High-integrity SACL, ordered ACE type/SID/mask/inheritance and exact readback precede accept; the existing supervisor status surface has the same bounded descriptor proof. |
| Capability and replay | found | Launch value and challenges use exact CNG call; capability verifier is exact SHA-256 and compared constant-time after identity proof. Channel nonce and capability ledgers are separate; replay/reconnect/duplicate and every named failure terminalize or quarantine. |
| Handle and secret lifecycle | found | Capability uses only stdin/stdout/stderr/read-handle HANDLE_LIST, non-inheritable write end, non-secret decimal locator, exact 32-byte+EOF read, all-return close and `SecureZeroMemory`. Capability bytes are excluded from ambient/log/public surfaces. |
| Response and channel settlement | found | Daemon and frontend receipts contain only local observations. ACK occurs before disconnect; daemon-local receipt alone gates admission release/quarantine and frontend-local receipt alone gates publication. Broker transport and kernel containment receipts remain distinct. |
| Deadline, cancellation and death | found | Original integer QPC triple remains unchanged; broker issue tick is later without rebase; worker/publication/cleanup cutoffs are distinct. Disconnect/death/cancel routes converge on exact nonce/resource settlement or quarantine. |
| Broker source/output/containment | found | Broker alone reads output-root config, opens/copies policy source, owns protected workspace, worker Job/streams and settlement. Retained locks, writer-close/share-zero seal, exact-handle breakaway probe, no-console/no-breakaway Job and foreign-process preservation are explicit. |
| Agent/prompt-injection and exposure | found | Untrusted strings remain bounded data and cannot select policy, service, executable, source, cleanup or tool action. One capped canonical text result, allowlist diagnostics and cross-channel canaries exclude raw paths, identities, credentials, license/source bytes and exceptions. |
| Configuration polarity | found | Enablement requires positive exact `enabled=true`; absent, false, malformed, stale, permissive or unproved state leaves only the seventh tool unregistered. Output root is resolved once at broker boundary and injected downward. |
| Dependency trust | not-applicable | The design adds no package/library dependency; it uses pinned package code and documented platform APIs. No advisory-database lookup is applicable to a new dependency. |
| CI/publication surface | found | No publication exception is requested or approved. Config snapshots and outputs are allowlist-built and require independent path and credential/license/token detection; publication and target gates remain fail-closed. |

## Latest-correction falsification

| Correction | S4 verdict | Evidence and attack result |
|---|---|---|
| Supervisor server kernel binding | verified | `GetNamedPipeServerProcessId`, exact process open, kernel creation time, token user/session and canonical installed image must equal lock owner and installed supervisor before status; squatter/stale/PID-reuse ambiguity fails (`design.md:401-418`; decision `220-230`). |
| Status-only supervisor authorization | verified | Exact enabled daemon service identity is checked before dispatch; schema has implicit `cst` and no task selector; all generic/control/other-target opcodes deny (`design.md:407-426`; decision `223-233`). |
| Exact descriptor/readback coverage | verified | Enrollment/supervisor descriptor terms are explicit at `design.md:392-411`; frontend at `447-473`; broker at `475-518`; decision restart/promotion requires all three new sampler descriptors and supervisor identity (`decision:331-334,368-379`). |
| Launch capability mechanics | verified | Exact CNG, SHA-256, constant-time compare, four-handle allowlist, non-secret locator, exact read+EOF, close and zeroization cover every named return (`design.md:48-78`; decision `215-254`). |
| Enrollment lifecycle | verified | Channel nonce terminates separately; successful Enroll creates `ISSUED -> ENROLLED`; ACK/close keeps it armed; exact child proof consumes; authenticated fresh cancel/failure/expiry/exit/shutdown/restart cancels and removes digest (`design.md:428-445`; decision `243-254`). |
| Three endpoints/four protocols | verified | Design and decision both name pre-spawn enrollment plus application route; promotion explicitly covers all three descriptors and `HubEnrollmentProtocolV1`, `FrontendDaemonProtocolV1`, `BrokerProtocolV1`, `BrokerWorkerProtocolV1` (`decision:284-307,320-334,368-379`). Static scan found no live “both pipe descriptors” or two-pipe contract. |
| Split local receipts | verified | No `FrontendExchangeReceiptV1` remains. Daemon owns only `DaemonResponseReceiptV1`; frontend owns only `FrontendTransportReceiptV1`; broker has separate `BrokerExchangeReceiptV1` and kernel containment receipt (`design.md:25-27,531-544,776-784`). |
| Broker-only output owner | verified | Only SCM broker reads ambient `MCPHUB_EM_OUTPUT_ROOT`, creates/injects `TrustedWorkspacePolicy`, and owns output/workspace authority (`design.md:1314-1316,1575`). No decision correction adds another reader. |

No new attack surface outside the upstream claims was found.

## Exact 18-claim S4 verdict mapping

| Claim | S4 verdict | Owner and falsifying-probe result |
|---:|---|---|
| 1 | verified | SCM-daemon policy resolution followed by broker authorization owns one exact entry. Artifact scan found explicit two-entry, swapped, unknown, duplicate, mismatch and zero-work requirements. Runtime matrix not run: design-only boundary. |
| 2 | verified | Daemon supervisor-status authentication owns the kernel-bound server/task-row proof. Squatter, stale owner, PID reuse, token/session/image/lock mismatch are explicit falsifiers. Runtime pipe attack not run: live action prohibited. |
| 3 | verified | `SupervisorCstIdentityAuthorizerV1` owns one implicit-CST status opcode and denies all control/other-target operations before dispatch. Runtime opcode matrix not run: no live supervisor mutation allowed. |
| 4 | verified | Each endpoint owns numeric-SID protected descriptor and exact readback. Design/decision cover enrollment, frontend, broker and existing supervisor status surface without widening authority. Runtime descriptor mutation not run: target gate. |
| 5 | verified | StdioHost then daemon capability ledger own exact CNG/SHA/compare/HANDLE_LIST/locator/EOF/close/zeroization. Static tuple scan passed; sibling/short-read injection not run: implementation absent. |
| 6 | verified | Enrollment protocol channel ledger and capability ledger have distinct state machines and owners. ACK-loss/post-ACK failure/cancel/expiry/death paths end in one terminal state. Deterministic runtime table remains a later implementation probe. |
| 7 | verified | Daemon frontend ledger consumes authenticated enrollment plus fresh challenge/correlation/hash/generation/entry/deadline before admission. Clone/replay/reconnect/second-frame matrix is fully specified; not executed in design phase. |
| 8 | verified | Ownership graph gives daemon authentication/admission/QPC/broker client/server-local receipt and frontend existing-six/client-local receipt/publication. Static dependency/semantic scan found no direct frontend-broker/source/worker owner. |
| 9 | verified | Broker pipe authentication owns exact SCM/token/service/logon/session/integrity/image proof, impersonation and mandatory revert before work. Runtime wrong-principal/revert matrix remains target verification. |
| 10 | verified | Broker nonce ledger owns CNG nonce and correlation/deadline/policy/entry/manifest/request-hash binding with consume/cancel on every exit. Static all-return table is complete; fault injection awaits implementation. |
| 11 | verified | SCM broker `TrustedWorkspacePolicy` and `AuthorizedBundleTransfer` alone read output root/open source/copy complete manifest with retained identity locks. Static ambient-reader scan found one owner. Namespace race matrix is target-only execution. |
| 12 | verified | `AuthorizedWorkspaceSnapshot` and `AuthorizedVendorPathLease` own distinct-principal write objects and writer-close/share-zero seal before use. Design falsifiers cover lazy read, same-user writer and every return; installed behavior awaits target. |
| 13 | verified | `WindowsContainedInvocation` alone owns worker/Job/streams/handles and constructs containment receipt only from actual kernel facts, never defaults. Missing-field/breakaway/crash injection awaits implementation/target. |
| 14 | verified | Owner-local daemon/frontend receipts and ACK ordering make release/publication gates observable without remote-close claims. Static scan found no combined receipt; partial-frame/close fault injection awaits implementation. |
| 15 | verified | SCM-daemon QPC owner creates one unchanged integer triple with separate worker/publication/cleanup cutoffs. Altered-field/delay probe is specified but not run because no runtime execution is authorized. |
| 16 | verified | `cst.py` existing-six owner followed by enrollment/daemon/broker yields exactly three new sampler endpoints, four protocols and one broker-owned worker route. Semantic scan found no obsolete two-pipe/direct route. Existing-six test awaits implementation. |
| 17 | not-verifiable (target-only: installed CST/license/COM/path-lock/output-seal/Job-descendant/breakaway/no-console trace required) | The contract and falsifier are complete, but design prose cannot prove installed CST accepts locks/seal, contains every descendant, rejects breakaway or has no hidden solve/console behavior. No target execution was permitted. |
| 18 | verified | Service provisioner plus enrollment/restart owners complete credential/capability storage, injection, exclusion and rotation/revocation boxes and require three descriptor proofs/absence before registration. Actual service provisioning remains a later target probe, not a design ambiguity. |

Result: 17 `verified`, one target-only `not-verifiable`, zero `failed`. Claim 17 is
not inferred from design or from the upstream PASS.

## Findings, required fixes and publication exceptions

No critical, high, medium or low remediable security-design finding was found. No
`fix-class` item and no bug-registry record is required. No publication-safety
exception is requested or approved; publication remains fail-closed behind its own
human leak-check and target/implementation gates.

## Residual risk and mandatory later probes

- Run all endpoint descriptor, supervisor squatter/opcode, clone/replay/cancel/death,
  partial-frame and handle-leak matrices against the implementation on disposable Windows.
- Verify QPC alteration/delay, nonce/all-return fault injection, exact Job settlement,
  namespace locks and output seal with preserved receipts.
- Execute the installed CST/license/COM target trace for Claim 17, including no-console,
  descendant containment, breakaway rejection and foreign-process preservation.
- Independently review implementation security, architecture and QA before registration,
  package pinning, publication, release or deployment.

## Gate

**PASS — the corrected proposed decision may be promoted to `accepted`.** Independent
review of exact design `AFABC3C001169D5C571D7319EA2C751CDD228E46B335C9630C0516F6EBAE6DC9`
and decision `FD81F4B2B5C14F8AAA66FC96533B8CDE4A7AF2B5738F146794D5BDC6C57212AD`
found one coherent supervisor-bound enrollment, frontend, broker and worker security
contract with exactly three new sampler endpoints, four schemas, owner-local receipts,
unchanged deadline, independent entry/broker authorization, broker-only source/output
authority, bounded agent-facing output and complete credential/capability lifecycles.
Together with independent architecture PASS
`18499E40CC82236F9EA256F988BB7F48342806240A1EC710E7739978BCF7601E`, this
satisfies the decision's metadata-promotion gate. Acceptance authorizes downstream
planning/implementation against these contracts only. It does not attest current
implementation or target behavior and does not authorize registration, package
pinning, publication, release, deployment or live action. Claim 17 remains fail-closed.

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
- S4: `verified`, `failed`, or `not-verifiable (with reason)`.
- SACL: System access-control list.
- SCM: Windows Service Control Manager.
- SHA-256: Secure Hash Algorithm with a 256-bit digest.
- SID: Windows security identifier.
