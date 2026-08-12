---
status: accepted
date: 2026-08-12
slug: cst-saved-field-authority-containment
owner: CST saved-field composition boundary
decided-by: architecture-reviewer and security-reviewer
supersedes: none
relates-to:
  - work-items/active/2026-08-11-cst-saved-field-sampler/design.md
---

# Default-off CST saved-field authority and per-invocation Windows containment

## Context

`cst_sample_saved_field` can read proprietary retained bundles and create CST
processes. Hub catalogue visibility is not an authorization boundary because a
bare-client, direct-daemon, or gate-off call can bypass it. The installed FastMCP
path is synchronous, so a caller or hub timeout cannot settle an in-process CST
call. Existing solve/export tools and pre-existing CST processes must remain
unchanged.

Microsoft documents that `PROC_THREAD_ATTRIBUTE_JOB_LIST` assigns a child to the
listed Job Objects during `CreateProcess`, and that ordinary descendants remain in
the job unless breakaway is enabled. It also documents that Job Object completion
messages are not guaranteed; direct `QueryInformationJobObject` accounting is
therefore the settlement authority, not a notification
(<https://learn.microsoft.com/en-us/windows/desktop/api/processthreadsapi/nf-processthreadsapi-updateprocthreadattribute>,
<https://learn.microsoft.com/en-us/windows/win32/procthread/job-objects>,
<https://learn.microsoft.com/en-us/windows/win32/api/jobapi2/nf-jobapi2-queryinformationjobobject>).
Microsoft also documents Windows reserved/alias path rules and named data streams;
ordinary default-file enumeration does not authorize `name:stream:$DATA`
(<https://learn.microsoft.com/en-us/windows/win32/fileio/naming-a-file>,
<https://learn.microsoft.com/en-us/windows/win32/fileio/file-streams>).
The installed CST adapter uses path strings for `Result3D` open/save and ResultTree
registration. A stable-handle copy followed by an ordinary path therefore leaves an
authorization discontinuity: a same-owner process can replace the leaf or an
ancestor before CST reopens it. Microsoft documents that delete access includes
rename and that omitting `FILE_SHARE_DELETE` denies later delete/rename access while
the handle remains open
(<https://learn.microsoft.com/en-us/windows/win32/api/fileapi/nf-fileapi-createfilea>,
<https://learn.microsoft.com/en-us/windows-hardware/drivers/ddi/wdm/nf-wdm-ntcreatefile>,
<https://learn.microsoft.com/en-us/windows/win32/api/fileapi/nf-fileapi-setfileinformationbyhandle>).

## Proposed decision

1. The sampler is default-off. The CST composition root registers it only when a
   restart-loaded, owner-restricted, local, non-reparse policy file validates and
   has `enabled=true`. The policy independently authorizes exact local roots,
   project-relative anchors, and complete `sha256-canonical-file-list-v2`
   bundle-manifest identities under one closed Windows path/object grammar. Caller flags,
   expected hashes, hub visibility, and prior calls never grant authority.
2. The hub-spawned `mcphub-cst-mcp` process remains the stdio frontend and the sole
   compatibility owner for the existing six tools. Only the seventh sampler crosses a
   fixed local frontend pipe to SCM service `McpLocalHubCstDaemon`. That service owns
   all-route sampler admission and is the sole authenticated broker client. It performs
   one-use launch/challenge validation, atomic `SamplerAdmissionGate.acquire_and_seal`,
   resolves the request's non-authoritative stable `entry_id` to exactly one unique
   immutable policy entry, creates one absolute QPC deadline, then sends one authority-only
   `BrokerRequestV1`. Broker independently loads/matches policy and alone opens source
   capability handles. Frontend and daemon perform no source filesystem/content
   operation and send no source path, bytes, or handle.
   `FrontendDaemonRequestV1` carries only `entry_id`, the closed sampler request,
   launch capability, frontend challenge nonce, correlation and request hash. Missing,
   unknown, duplicate, swapped, revision-ambiguous or mismatched selection has no
   implicit/default fallback and performs zero broker/source/CST work.
3. Broker alone launches one fresh package-owned Windows worker in a newly created
   unnamed Job Object. The job is configured before launch with
   `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE`, no breakaway flags, a finite active-process
   cap, and accounting. `CreateProcessW` uses
   `PROC_THREAD_ATTRIBUTE_JOB_LIST` and a strict inherited-handle list, making job
   membership atomic with process creation. `CreateProcessW` uses non-null
   `lpApplicationName` equal to the resolved pinned interpreter; one mutable command
   line exactly `"<resolved_executable>" -I -s -E -m
    mcphub_em_mcp.cst_saved_field_broker_worker` using that identical path and no variable
   argument; a fixed local non-reparse interpreter
   directory; deliberate broker environment with Python isolated flags;
   `bInheritHandles=TRUE`; `STARTF_USESTDHANDLES`; exactly three inheritable child
   pipe ends in `HANDLE_LIST`; and
   `EXTENDED_STARTUPINFO_PRESENT|CREATE_UNICODE_ENVIRONMENT|CREATE_NO_WINDOW`.
   Job/process/thread/broker pipe handles are non-inheritable. Broker solely owns the
   Job handle; worker cannot inherit it because HANDLE_LIST excludes it. Broker death
   closes the last Job handle and applies kill-on-close only to that exact owned
   tree. Attribute arrays,
   command/environment buffers, and the attribute list live through
   `CreateProcessW` and are released on every return. No shell, PATH lookup,
   caller argv/environment/current-directory value, or breakaway flag exists.
4. Frontend/daemon use private `FrontendDaemonProtocolV1`; daemon/broker use
   `BrokerProtocolV1`; broker/worker use private `BrokerWorkerProtocolV1`. Each has one
   bounded canonical request/result exchange. Worker owns source transfer, workspace,
   CST/session, and application receipt; broker owns worker Job/streams and merges only
   owner-observed containment settlement; each transport owner separately records frame,
   flush, EOF, cancellation and handle-close facts after they occur. A response frame
   cannot claim that its own pipe is already closed. Results flow only worker to broker
   to daemon to frontend; no source-authority channel or intermediate workspace exists.
   Daemon locally owns `DaemonResponseReceiptV1` for correlation, response/terminal
   writes, flush, frontend ACK, disconnect and server-handle close; it alone gates
   admission release/quarantine and asserts no frontend EOF/close. Frontend locally owns
   `FrontendTransportReceiptV1` for correlation, response/terminal reads, EOF-or-cancel
   and client-handle close; it alone gates publication and asserts no server close.
   Partial, trailing, mismatched or locally ambiguous evidence fails its local gate;
   neither side requires impossible post-close notification from the peer.
5. SCM daemon creates integer QPC triple `{frequency, admitted_tick,
   deadline_tick=admitted_tick+60*frequency}` before broker work and propagates the
   unchanged integers through broker and worker protocols. Each receiver verifies system
   QPC frequency equality, `deadline_tick=admitted_tick+60*frequency`, and
   `admitted_tick<=QPC_now<deadline_tick`; it never converts remaining
   time into a new deadline. Worker response is due by `deadline_tick-2*frequency`,
   publication before `deadline_tick`. After the
   deadline only fixed bounded failure construction and settlement may run—never
   source/vendor access or success serialization.
6. Every worker timeout/protocol/stream/shutdown/exception path enters broker's one
   termination state machine. A normal worker signal/exit is recorded, its exact
   process handle is closed, and the still-open
   Job is queried. `ActiveProcesses>0` immediately calls `TerminateJobObject`.
   Termination records worker signal/exit, closes worker reference, cancels
   and join readers, and query until `ActiveProcesses==0`, all within a separate
   cleanup deadline `{termination_tick+10*frequency}`, then closes Job/handles. Normal
   worker residual activity terminates immediately. Missing worker signal, active-zero, reader
   join, or required handle-close proof is `containment_settle_failed`.
7. `SamplerAdmissionGate` is the sole writer of availability, immutable policy
   revision/generation, one active lease, and one waiter bounded to 1.000 second.
   After unproved settlement, `quarantine_and_release` under one lock latches
   `quarantined`, revokes the active lease, releases the permit, and then wakes a
   waiter. Every awakened/future route must recheck after acquire and immediately
   before work; it returns `containment_quarantined` or `policy_revision_changed`
   with zero lexical/source/broker work and releases cleanly. The gate has no
   in-process clear. Recovery stops daemon then asks/stops broker; only broker owns/
   closes worker Job. Both exact service handles must signal and startup revalidates.
8. Completion-port messages and process identifiers are diagnostics only. They
   never grant close/kill/absence authority. Production does not enumerate or touch
   foreign processes. Target admission must interleave a foreign CST process and
   prove it remains alive while every worker/CST descendant joins and leaves only
   the invocation job.
9. Use Python standard-library `ctypes` over documented Win32 APIs; no package-library
   or Go hub dependency is added. The fixed credential-free virtual-account SCM broker/
   daemon pair is an explicit mandatory runtime dependency with startup identity/health
   proof, not an ambient service or fallback. Missing broker, token, DACL, Job, or
    authenticated-channel proof leaves the sampler unregistered. Enabled frontend
    composition constructs a real `WindowsDaemonClient` from the fixed service endpoint;
    `UnavailableDaemonTransport` is only the explicit fail-closed result of absent or
    invalid startup proof, never the enabled production transport.
10. One `WindowsPathIdentityV1` contract applies to the policy file, policy/workspace
   roots and project, source and destination walkers, vendor candidate paths,
   clean payload/header creation, and
   registration. It admits ordinary `X:\\` only, with the drive colon as the sole
   colon; rejects streams, special/device/UNC/mapped prefixes, reserved names,
   trailing dot/space, tilde aliases, wildcards/controls, and non-NFC components
   before filesystem access. Its single reject-only reserved-device predicate first
   rejects colon/stream and trailing-dot/space forms and requires NFC-exact input,
   then builds a comparison-only invariant-case key with extension removal,
   defensive Win32 trailing trim, and `¹`/`²`/`³` mapping to `1`/`2`/`3`. It rejects
   ASCII `COM1`..`COM9`/`LPT1`..`LPT9` and Unicode
   `COM¹`/`COM²`/`COM³`/`LPT¹`/`LPT²`/`LPT³`, case-insensitively and despite an
   extension. The comparison key is never emitted or accepted as a canonical path,
   and no normalization occurs after the predicate accepts. The contract then
   requires unique canonical long-name/final-path,
   volume plus 128-bit file ID, link-count-one, no-reparse, and only unnamed
   `::$DATA` proof. Missing alternate-name/identity proof fails closed.
11. `AuthorizedBundleTransfer` copies every and only manifest-v2 row from one exact
   stable no-follow source handle to a new destination handle under finite count,
   byte, and absolute-time budgets. Before CST, it re-enumerates/re-hashes the entire
   destination and requires exact policy equality in cardinality, canonical path,
   type, default stream, size, and SHA-256. The committed copied snapshot is the
    authorization guarantee; the three source post-hashes are provenance only.
12. Path-only CST execution requires fixed SCM service
    `McpLocalHubCstVendorBroker` running as credential-free virtual account
    `NT SERVICE\McpLocalHubCstVendorBroker`; the only client is fixed SCM service
    `McpLocalHubCstDaemon` under its distinct virtual account/service SID. Both are
    session-0, pinned-image services. The broker owns a SYSTEM/broker-owned protected
    non-inheriting DACL workspace, fresh CST worker, and exact token/process/thread/Job
    handles. The daemon SID is denied workspace and process/token access. Read-only payload/header
    inputs use `GENERIC_READ` with exactly `FILE_SHARE_READ`; ancestors allow read/write
    sharing but omit delete sharing. A project CST may write is output-class and never
    a read-only authorized input. Only the isolated SID may write unknown generated
    header bytes; after Save/writer close, the owner acquires share-0 read, validates/
    bounds/hashes the object, then creates known-hash clean inputs held read-only
    through ResultTree/sample/cache/session settlement. Same-principal share-write or
    close/reopen, random-name, unlocked, and unisolated fallbacks are forbidden. Target
    CST/license/COM incompatibility leaves the sampler unregistered.
13. The startup breakaway capability is true only when the explicit probe reports
    `breakaway_denied=true` and `breakaway_created=false`. Created, allowed, missing,
    contradictory, or ambiguous evidence is containment failure before request work;
    it cannot be normalized into denial and causes quarantine. If creation succeeds,
    the containment owner retains exact `PROCESS_INFORMATION`, terminates by exact
    process handle, boundedly waits/records exit, closes exact thread/process handles,
    and records absence before returning unavailable/quarantined. Any incomplete step
    is `containment_settle_failed`; PID discovery and Job cleanup are not substitutes.
14. The sole broker IPC endpoint is single-instance local message pipe
    `\\.\pipe\mcp-local-hub-cst-saved-field-v1`, created duplex/overlapped with first-
    instance and remote-client rejection. Its protected DACL grants SYSTEM/broker owner
    rights and only read-data/write-data/read-attributes/synchronize to the exact daemon
    service SID, explicitly denies Anonymous/Network, and grants no generic-all,
    create-instance, DACL/owner-write, or delete right to the client. Its SACL is a
    High-integrity no-write-up label. Exact descriptor readback is mandatory. Client
    verifies server PID from SCM plus token/service SID/session/integrity/image; server
    verifies client PID from SCM, calls `ImpersonateNamedPipeClient`, compares exact
    user/enabled service/logon SID/session/integrity/prohibited privileges, and always
     calls `RevertToSelf` in `finally` before parsing/privileged work. Failed impersonation
     does zero work; failed/unproved revert terminates broker and quarantines.
     At service startup, both virtual-service-account names are resolved with
     `LookupAccountNameW`; the returned binary SIDs are converted to canonical numeric
     SID strings for SDDL construction. Symbolic placeholders are forbidden. The applied
     descriptor is read back and compared as owner, protected DACL, mandatory label, ACE
     type/order/SID/mask/inheritance before the pipe accepts a client.

     The separate frontend endpoint is fixed single-instance local message pipe
     `\\.\pipe\mcp-local-hub-cst-saved-field-frontend-v1`, owned by the daemon service.
     Its protected descriptor grants the daemon and SYSTEM owner rights and only the
     restart-loaded policy-owner SID the minimum client data/synchronize rights. Daemon
     verifies the client token user equals that owner SID, client PID/image/package is the
     pinned `mcphub-cst-mcp` frontend, and remote/anonymous/network clients are denied.
     This endpoint can request only the closed sampler operation; it grants no source
     selector and the daemon/broker policy rechecks remain authoritative.
     Before the actual CST stdio child is created, `internal/daemon.StdioHost` uses
     `BCryptGenRandom(NULL,buf,32,BCRYPT_USE_SYSTEM_PREFERRED_RNG)`, computes exact
     SHA-256, and sends only that non-authoritative verifier over the separate fixed
     daemon-owned `HubEnrollmentProtocolV1` pipe. Daemon authenticates the peer as the
     current supervisor-tracked CST wrapper by independently querying the existing
     supervisor IPC. Client first calls `GetNamedPipeServerProcessId`, opens that exact
     process and proves kernel creation time/token user/session/canonical installed image
     equal `SupervisorLockOwner{PID,StartedAt}` and canonical installed supervisor; a
     first-instance squatter fails. Supervisor independently proves connected client PID,
     exact enabled daemon service SID, token/session/integrity and pinned daemon image.
     Daemon service SID may invoke only exact pre-dispatch opcode
     `GET_CURRENT_CST_TASK_IDENTITY_V1` with implicit task `cst`; generic status/control/
     respawn/reconcile/exit and other targets deny. Peer PID/kernel creation generation/canonical installed image/token
     user/session must equal its exact current task row and supervisor hello must match
     `SupervisorLockOwner{PID,StartedAt}`. Supervisor is a user process, not an SCM
     service; no service SID is asserted. Policy-owner DACL access and digest alone grant
     no enrollment. Enrollment and supervisor pipes use runtime numeric SIDs, exact owner,
     protected DACL, High-integrity SACL, ordered ACE type/SID/mask/inheritance and exact
     readback; any mismatch fails before accept/dispatch.

     Capability bytes pass solely through an explicit anonymous-pipe read handle in a
     `PROC_THREAD_ATTRIBUTE_HANDLE_LIST` containing stdin/stdout/stderr/read-handle only;
     write end is non-inheritable, `bInheritHandles=TRUE`, and fixed
     `MCPHUB_CST_LAUNCH_HANDLE=<decimal>` is a non-secret locator. Parent writes exactly
     32 bytes before start, closes both copies on every exit; child requires exact
     32-byte read plus EOF, closes immediately, clears locator, and both sides use
     `SecureZeroMemory`. Capability is absent from argv/manifest/intent/logs and only
     the non-secret locator is in environment. Daemon then sends a fresh CNG one-use
     challenge and consumes both enrollment and challenge before admission. Start
     failure, invalid child, short/overlong read, timeout, cancel,
     Authenticated Enroll consumes only its channel nonce and creates capability state
     `ISSUED -> ENROLLED`; ACK/flush/close leaves it ENROLLED. Exact child read+EOF and
     successful daemon challenge changes `ENROLLED -> CONSUMED`. Fresh independently
     authenticated `CancelEnrollmentV1`, start/write/read failure, child exit, expiry,
     shutdown or restart changes `ENROLLED -> CANCELLED` and removes the digest.
     disconnect, duplicate, reconnect, shutdown and exit terminalize both ledgers and
     close all handles; ambiguity quarantines. This is an admitted `HostConfig`/
     `StdioHost.Start` Windows adapter, `internal/api` enrollment/supervisor-status/DACL,
     and `internal/cli/daemon.go` change because the current host has no inherited-
     capability or enrollment seam.
15. Broker issues one 256-bit `BCryptGenRandom` nonce with five-second monotonic expiry.
     `issued_tick` is the broker's current QPC sample satisfying
     `admitted_tick<=issued_tick<min(deadline_tick, expires_tick)`; latency therefore does
     not require `issued_tick==admitted_tick`, while the original deadline triple remains
     byte-for-byte unchanged.
    One maximum-131072-byte canonical v1 request echoes it and binds unique 128-bit
    correlation, broker-loaded policy revision/entry/manifest-v2 and request hash. Nonce
     is atomically consumed before authorization even on later failure. Before request
     acceptance, every post-challenge local/encoding/transport/cancel/disconnect/timeout
     exit sends `CancelChallengeV1`; broker atomically changes the exact outstanding ledger
     entry from `ISSUED` to `CANCELLED` and returns a typed terminal receipt. Accepted
     exchange atomically changes it from `ISSUED` to `CONSUMED` before authorization.
     Duplicate, missing, or unproved terminalization quarantines both admission and broker
     service until restart. Broker independently
    rechecks policy/quarantine/SCM identity before copy/worker start. Replay/stale/wrong/
    malformed/trailing/second frames do zero privileged work; one bounded response uses
    the existing ceiling. Disconnect/cancel before authorization closes with zero work;
    after creation it terminates/waits/closes worker/Job/token, settles leases/workspace,
    suppresses success, and consumes nonce. Unproved pipe/revert/resource settlement is
    `containment_settle_failed` and quarantine; no reconnect continues a request.
16. Credential four-box: LSA/SCM solely stores and injects virtual-account tokens—no
    password exists and application code never calls `LogonUser`; policy/argv/env/frames/
    logs/diagnostics/dumps/manifests expose no credential/token/SID/license value; elevated
    installer/operator owns service disable/delete, package rotation, and identity
    revocation. Rotation stops both exact services, proves service processes/Jobs/pipes/
    workers/workspaces absent, replaces pinned package, restarts, and revalidates. An
    identity change requires a successor fixed service name/SID and decision; old ACL/
    pipe/service/token state is removed/proved absent. Any revoked/disabled/wrong/stale
    identity leaves sampler unregistered with no token/session reuse.
17. Topology is exactly pre-spawn `supervisor-tracked CST StdioHost ->
    HubEnrollmentProtocolV1 -> SCM daemon`, then application `hub -> existing
    mcphub-cst-mcp stdio frontend -> FrontendDaemonProtocolV1 -> SCM daemon ->
    BrokerProtocolV1 -> broker-owned contained worker -> vendor`. Enrollment uses its
    dedicated protected descriptor, closed challenge/enroll/cancel/receipt schema and
    capability state `ISSUED -> ENROLLED -> CONSUMED|CANCELLED`; no frontend admission
    occurs before authenticated enrollment and frontend-challenge consumption. The
    frontend remains the existing-six compatibility
    and MCP publication owner. The daemon is the sole authenticated broker-pipe client
    and owns sampler admission, original 60-second deadline, broker-pipe cancellation,
    receipt validation and quarantine; it never reads source or spawns a worker.
    Broker independently authorizes policy/nonce, opens policy-derived source handles,
    transfers directly into its protected workspace, spawns/contains the fresh worker,
    and merges worker/Job/session/lease/workspace receipts. Worker returns one settled
    encoded result only to broker; broker returns one `BrokerResponseV1` only to daemon;
    daemon returns one bounded sampler result only to the frontend. Daemon release is
    gated only by `DaemonResponseReceiptV1`; frontend publication is gated only by
    `FrontendTransportReceiptV1`.
    No frontend/broker or daemon/worker direct channel, intermediate workspace,
    source-byte/path/handle proxy, or second source-authority channel exists. The original deadline's remaining
    duration is monotonically narrowed at broker/worker; only the existing 10-second
    cleanup budget follows termination. Pipe disconnect/cancel/daemon death makes
    broker settle its exact worker before close; broker death/absent receipt makes
    daemon quarantine, while broker Job kill-on-close owns worker-tree death.

## Lifecycle, migration, and rollback

Policy is an immutable equal frontend/daemon/broker process-lifetime snapshot. Process-lifetime
mutable sampler state is `SamplerAdmissionGate`, one enrollment capability ledger
`ISSUED -> ENROLLED -> CONSUMED|CANCELLED`, one frontend challenge ledger and the
broker nonce ledger: policy revision/generation, one active lease, one bounded waiter,
at most one entry per ledger, and `available` -> `quarantined` one-way. There is no watcher or hot
reload: an operator writes/replaces the policy out of band and restarts the CST
frontend and both services. Missing, disabled, malformed, remote, permissive, replaced, or unsupported
policy leaves the sampler unregistered while the existing six tools remain live.

Enrollment, frontend, broker and broker-worker protocols and policy start at explicit
`v1` schemas; policy entries use
only `sha256-canonical-file-list-v2`. The superseded design-only v1 manifest is
rejected and regenerated out of band, never interpreted or migrated. Before first
publication there is no migrated sampler state. Rollback restores the prior package
pin and removes/sets `enabled=false` in the policy; no worker, workspace, cache,
or saved session survives a call, and no admission waiter survives daemon
termination. A future policy/schema default flip,
transport change, non-Windows implementation, or hot reload requires a successor
decision. Rollback/recovery stops daemon (closing pipe/client lease), requests broker
settlement, then stops broker and observes both service handles signaled. Broker's
last Job handle alone is the worker-tree kill boundary. Restart revalidates policy and the
containment startup probe, exact supervisor/SCM identities, all three pipe descriptors,
all four protocol schemas and policy equality
before registering the sampler.

The former daemon-spawned `cst_saved_field_helper.py`, helper protocol, parent helper
Job/stdio readers, helper workspace transfer, and helper result route are removed,
not retained as aliases or fallbacks. Their unpublished internal v1 frames are not
migrated or accepted. This semantic correction returns the decision to `proposed`; prior architecture and
security acceptance do not apply to the new vendor-path lease or corrected breakaway
truth table. Daemon/broker/worker are one immutable package pin, so settlement
receipt is updated atomically to include vendor-path-lease settlement; an older or
missing field fails protocol validation rather than negotiating.

## Rejected alternatives

- Hub-only visibility or a caller Boolean: bypassable or forgeable; not authority.
- Synchronous CST inside the long-lived daemon: no safe way to enforce duration or
  settle a blocked vendor call.
- `CreateProcess` followed by ordinary assignment: the child can execute before
  assignment. Atomic `PROC_THREAD_ATTRIBUTE_JOB_LIST` is the selected owner.
- Completion-port `ACTIVE_PROCESS_ZERO` as the sole oracle: Microsoft states job
  notifications are not guaranteed. Direct job accounting plus the worker handle is
  required.
- Persistent worker pool: retains cross-call CST/process state and violates restart
  independence and exact per-invocation containment.
- Validate an ordinary copied path immediately before CST and revalidate afterward:
  CST can consume a replaced object during the gap; post-check is too late.
- Random names or same-owner directory access control: neither preserves object
  identity against the admitted same-owner adversary.
- Close capability handles for CST compatibility and reacquire afterward: recreates
  the capability discontinuity and is not an authorized fallback.
- Direct native handles would be preferable, but no verified installed CST surface
  accepts them. A future handle-native adapter requires a successor decision.
- Same-principal `FILE_SHARE_WRITE`, close/reopen, random names, or DACLs that grant
  the CST user's same token: none authenticates the writer of unknown output bytes.

## Promotion gate

Promotion from `proposed` to `accepted` requires both an independent Claim-Verify
architecture `PASS` and an independent security `PASS`, each explicitly bound to the
exact corrected design and proposed-decision SHA-256 values and covering atomic
admission, supervisor/enrollment/frontend/daemon/broker identity and all three pipe descriptors,
policy/manifest-v2, shared Windows namespace identity, complete
source-to-isolated-workspace transfer, exact share modes/header seal, escaped-probe
settlement, and `HubEnrollmentProtocolV1`, `FrontendDaemonProtocolV1`,
`BrokerProtocolV1`, and `BrokerWorkerProtocolV1` schemas. These two review verdicts are the
complete metadata-promotion gate. Decision acceptance authorizes planning and
implementation against these contracts; it does not claim target-runtime proof.

Before sampler registration, package pinning, release, or deployment, the owning
delivery gates still require a target Windows/CST trace proving file-ID/stream/alias
fail-closed behavior, distinct vendor token/protected DACL, read-only share masks,
isolated header write/share-0 seal and ResultTree compatibility, atomic Job
membership, truthful breakaway rejection plus exact escaped-child settlement, 60-second
termination/zero-accounting settlement, bounded streams, no visible console, and
preservation of an interleaved foreign CST process. Architecture Claims 7 and 15 and
the Line10 acceptance gate likewise remain mandatory at their stated implementation
and release stages. Acceptance neither waives them nor changes their target-only
status.

## Terms and Abbreviations

- ADS — Windows Alternate Data Stream.
- CST — CST Studio Suite.
- Job Object — Windows kernel object that manages a process group as one unit.
- MCP — Model Context Protocol.
- Win32 — Microsoft Windows native application programming interface.
