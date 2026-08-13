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

Installed ConfigCI 1.0 cannot directly author the required AppID-plus-Hash rows. Its
implementation assembly version `10.0.26100.4484`, SHA-256
`6753B842637B26D37336E81111036D778B5884922BC944969D2C2FA70A49AFB2`, rejects per-app
enum `Hash=1` while its resource text incorrectly lists Hash as allowed. Ordinary Hash
authoring still emits the documented four UMCI Authenticode/page SHA-1/SHA-256 rows, and
working FileName-per-app authoring derives `AppIDs` from the application's
`OriginalFilename`. Microsoft's installed `cipolicy.xsd` explicitly permits `Allow/@Hash`
with `Allow/@AppIDs`, and Microsoft documents direct XML editing with schema validation.
The decision therefore admits only a pinned deterministic XML/CIP author plus an
independent XML/CIP/signature/chain/revocation/signed-content verifier. Microsoft separately
defines signed App Control policy constraints and `CryptMsgControl` signature verification;
content extraction alone is not authenticity. The decision therefore also requires one
isolated offline HSM-quorum operator ceremony and a request-bound audit-attestation receipt;
it does not infer that a ConfigCI upgrade, another rule level, ambient certificate selection,
PFX/password path, successful decoder or SignTool exit is safe.

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
   `lpApplicationName` equal to the canonical signed/hash-pinned package-owned
   `mcphub-cst-runtime.exe`; one mutable command line exactly
   `"<canonical-runtime>" --role=worker`; a fixed local non-reparse immutable bundle
   directory; a deliberate broker environment containing no Python search authority;
   `bInheritHandles=TRUE`; `STARTF_USESTDHANDLES`; exactly five inheritable handles in
   `HANDLE_LIST`—the three child pipe ends plus one read-only source-root directory
   capability and one restricted workspace-root directory capability—and
   `EXTENDED_STARTUPINFO_PRESENT|CREATE_UNICODE_ENVIRONMENT|CREATE_NO_WINDOW`.
   Job/process/thread/broker pipe handles are non-inheritable. Broker solely owns the
   Job handle; worker cannot inherit it because HANDLE_LIST excludes it. Broker death
   closes the last Job handle and applies kill-on-close only to that exact owned
   tree. Attribute arrays,
   command/environment buffers, and the attribute list live through
   `CreateProcessW` and are released on every return. No shell, PATH lookup,
   caller argv/environment/current-directory value, or breakaway flag exists.
   One process-wide `WorkerInheritanceEpoch` is acquired before any inheritable handle
   is created; every broker `CreateProcessW` path acquires it. The epoch remains held
   through exact process creation and immediate parent close of all five child handles,
   and releases only after those closes are recorded. Thus no concurrent broker child
   can observe an inheritance window.

   Broker opens the policy-derived source root exactly with
   `CreateFileW(path,FILE_LIST_DIRECTORY|FILE_TRAVERSE|FILE_READ_ATTRIBUTES|SYNCHRONIZE,
   FILE_SHARE_READ,NULL,OPEN_EXISTING,
   FILE_FLAG_BACKUP_SEMANTICS|FILE_FLAG_OPEN_REPARSE_POINT,NULL)`; omitted
   `FILE_SHARE_WRITE|FILE_SHARE_DELETE` deny write-capable root opens and root rename/
   delete. It opens the fresh protected workspace root exactly with
   `CreateFileW(path,FILE_LIST_DIRECTORY|FILE_TRAVERSE|FILE_READ_ATTRIBUTES|
   FILE_ADD_FILE|FILE_ADD_SUBDIRECTORY|FILE_DELETE_CHILD|SYNCHRONIZE,
   FILE_SHARE_READ|FILE_SHARE_WRITE,NULL,OPEN_EXISTING,
   FILE_FLAG_BACKUP_SEMANTICS|FILE_FLAG_OPEN_REPARSE_POINT,NULL)`; omitted
   `FILE_SHARE_DELETE` denies root rename/delete while child work remains possible.
   Both roots omit root `DELETE`, `WRITE_DAC`, `WRITE_OWNER`, and security access and
   must read back as non-inheritable directories with no reparse attribute/tag and the
   exact expected final path, volume/file identity, owner and DACL.

   For each root broker calls
   `DuplicateHandle(GetCurrentProcess(),original,GetCurrentProcess(),&duplicate,
   dwDesiredAccess=<exact role mask above>,bInheritHandle=TRUE,dwOptions=0)`;
   `DUPLICATE_SAME_ACCESS` is forbidden. `NtQueryObject(ObjectBasicInformation)` must
   return exactly that granted access; file type/directory/identity/role must equal the
   original; `GetHandleInformation` must show only `HANDLE_FLAG_INHERIT` and no protect-
   from-close. Missing or unequal readback creates no child. No path, policy location, secret,
   or authority enters argv/environment. After native startup proof, one fixed-size
   checksummed `WorkerPreMainBootstrapV1` frame carries only the two numeric inherited-
   handle locators, expected volume/file identities, access roles, correlation and
   deadline. Windows preserves
   the numeric value and granted access for an inherited handle; the locators are not
   authority without membership in this exact creation allowlist and matching kernel
   identity. Worker uses `NtCreateFile` with `OBJECT_ATTRIBUTES.RootDirectory` equal to
   the validated capability for every descendant open; absolute names and root reopen
   are forbidden. `GetFileInformationByHandleEx(FileStandardInfo,FileIdInfo)` proves
   directory type plus expected volume/file identity before source or CST work. The
   first native application instruction clears and reads back `HANDLE_FLAG_INHERIT=0`
   on the three standard handles before startup proof or stdin. After capability
   identity validation native code clears and reads back both capability flags and
   emits `WorkerPreMainReceiptV1`; broker accepts that receipt before sending the later
   application frame. Only then does the same process call `Py_InitializeFromConfig`
   with `isolated=1`, `site_import=0`, `user_site_directory=0`, `safe_path=1`,
   `use_environment=0`, `parse_argv=0`, `module_search_paths_set=1` and exact manifest-
   hashed bundled stdlib/extension/application roots, then invokes the pinned Python
   worker entry. No user/global site, `.pth`, `sitecustomize`, `usercustomize`,
   `PYTHON*`, registry, cwd or caller module path executes. Any pre-main/readback/
   configuration ambiguity closes capabilities, performs zero Python/privileged work,
   exits nonzero and quarantines.
4. Frontend/daemon use private `FrontendDaemonProtocolV1`; daemon/broker use
   `BrokerProtocolV1`; broker/worker use private `BrokerWorkerProtocolV1`. Each has one
   bounded canonical request/result exchange. Broker owns source/workspace root
   capability creation, exclusive inheritance epoch, exact five-handle inheritance,
   immediate parent duplicate closes, and final workspace-root
   deletion. Worker owns handle-relative source transfer, workspace contents,
   CST/session, five inherit-clear readbacks, inherited-handle close, and application/capability receipt; broker owns worker Job/streams and merges only
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
6. Every worker bootstrap/timeout/protocol/stream/shutdown/exception path enters broker's one
   termination state machine. A normal worker signal/exit is recorded, its exact
   process handle is closed, and the still-open
   Job is queried. `ActiveProcesses>0` immediately calls `TerminateJobObject`.
   Termination records worker signal/exit, closes worker reference, cancels
   and join readers, and query until `ActiveProcesses==0`, all within a separate
   cleanup deadline `{termination_tick+10*frequency}`, then closes Job/handles. Normal
   worker residual activity terminates immediately. Before creation failure closes all
   duplicates/originals and releases the inheritance epoch only after duplicate close,
   then removes only the fresh workspace. After creation the parent immediately closes
   all five child-side copies before epoch release; child exit closes inherited handles
   even when no valid request exists. Failed standard/capability inherit-clear or
   readback performs zero source/vendor/CST/descendant work and quarantines. On a valid
   request the child closes source after final
   post-hash and workspace last, and reports only its own close facts. Broker closes its
   retained originals and deletes the root only after child exit/Job/stream settlement.
   Missing exclusive epoch, exact root/share/duplicate/access readback, any of five child
   inherit-clear readbacks, worker signal, active-zero, reader join, child capability close,
   retained-original close, root deletion, or required receipt is
   `containment_settle_failed`; no path/config fallback exists.
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
9. Use documented Win32 APIs through existing Python `ctypes` after bootstrap and through
    the new package-owned native runtime before CPython. No third-party library is added.
   The signed/hash-pinned native bundle and admitted Go launch-profile/enrollment surface
   are explicit package dependencies. The fixed credential-free virtual-account SCM broker/
   daemon pair is an explicit mandatory runtime dependency with startup identity/health
   proof, not an ambient service or fallback. Missing broker, token, DACL, Job, or
    authenticated-channel proof leaves the sampler unregistered. Enabled frontend
    composition constructs a real `WindowsDaemonClient` from the fixed service endpoint;
    `UnavailableDaemonTransport` is only the explicit fail-closed result of absent or
     invalid startup proof, never the enabled production transport.
    `SR-C6-DLL-03` is fixed by one exact native seam. The package-owned x64
    `mcphub-cst-runtime.exe` is a no-CRT custom-entry PE built by the pinned MSVC/SDK
    record with `/Zl /NODEFAULTLIB /ENTRY:mcphub_cst_entry /SUBSYSTEM:WINDOWS
    /DEPENDENTLOADFLAG:0x800 /DYNAMICBASE /HIGHENTROPYVA /NXCOMPAT /CETCOMPAT`.
    Its direct import table contains only `KERNEL32.dll` and a generated exact function
    allowlist; delay-import, TLS directory/callback, CLR/bound-import, embedded manifest
    redirection, writable/executable section, CRT/UCRT/VCRUNTIME, C++ exception/RTTI/
    static initializer, package DLL and compiler TLS are absent. `/GS` is disabled only
    for the bounded, buffer-free, independently disassembled prelude. The same image is
    the only frontend and worker launcher; no wrapper or alternate bootstrap exists.

    Build and provisioning parse the PE independently of `dumpbin`, compare its exact
    import/delay/TLS/load-config/section/entry/mitigation facts, verify no-reparse held-file
    final path, volume/file identity, SHA-256 and offline Authenticode, and emit a
    reproducibility manifest binding MSVC/SDK hashes, flags, ordered inputs, source/output
    hashes and target-OS loader-closure schema. Both frontend and broker compare the held
    runtime receipt immediately before creation. At a target probe paused at entry RVA,
    every already mapped module must be a manifest-equal Authenticode-valid Microsoft
    System32 image; any package/user/extra/missing/substituted module blocks registration.
    Entry disassembly permits only ABI prologue and direct allowlisted KERNEL32 IAT calls
    through the last required inherit-clear/readback. Hostile application/cwd/PATH/user
    DLL/TLS/static-initializer canaries must not load or execute before that marker.

    Only after the frontend four flags, or worker three standard then two capability
    flags, are cleared and read back does one `NativePackageLoadOwner` admit
    `PackageLoadClosureV1`. Independent PE parsing recursively inventories exact CPython,
    extension/package normal/delay imports and a separate target System32/API-set set; it
    is not treated as proof against runtime-computed loads.

    P18 contains one `CstAppControlPolicySetOwner`. It alone may write the fixed sampler
    policy identity: one signed Multiple Policy Format supplemental with an explicit
    operator-selected BasePolicyID. Its sole admitted input is
    `ExactAppIdPolicyArtifactV1`: a provisioning-only pinned ConfigCI invocation emits
    ordinary unscoped Hash tuples, an independent PE implementation verifies the four UMCI
    Authenticode/page hashes, and a deterministic author under the pinned Microsoft XSD
    emits unscoped exact runtime-executable hash rows plus exact package/System32 module
    hash rows carrying exactly one runtime `OriginalFilename` AppID. AppID is a scope
    selector; exact runtime identity is the conjunction of the unscoped runtime hashes,
    signed bundle, fixed OriginalFilename and complete-policy rejection of broader rules.
    Direct `New-CIPolicyRule -AppID -Level Hash` is forbidden. It never creates a sampler base, because active bases intersect and
    a new base can block unrelated applications; it never updates, merges, converts,
    removes, disables, or changes authority for an ambient policy. Because a supplemental
    unions with its base and sibling supplementals, admission requires the selected base to
    already allow the exact supplemental signer, the entire selected base family to have no
    broader runtime-AppID allow, every other intersecting base to admit every exact row, and
    unrelated signed host canaries to preserve their baseline. The machine must be disposable
    or reserved and every Group Policy, MDM/CSP, WMI, Windows Update policy servicing or other
    automatic writer must be proved quiescent for the complete availability lease; an external
    writer cannot be serialized by the owner and is incompatible. Microsoft platform policies
    are inventoried and boot-bound but never altered. Unknown authority, audit/AllowAll/broad
    rule, stale/pending/duplicate policy, policy-
    limit state, or inventory drift keeps the sampler unregistered.

    Before signing, a code-independent P18 Go verifier validates canonical
    XML against the pinned XSD, reconstructs every semantic row, parses two byte-identical
    unsigned `ConvertFrom-CIPolicy` outputs under the pinned CIP grammar, and proves exact
    row classes/algorithms/hashes/cardinality/references/PolicyID/BasePolicyID/version/
    options/signer metadata.

    One isolated offline operator ceremony is the sole `CstPolicySigningOwner`. Target P18
    exports only `CstPolicySigningRequestV1`, binding held unsigned hash, semantic/profile
    hashes, PolicyID/BasePolicyID, forward version, exact signer pins and approval-ticket hash;
    target has no signing key/provider/credential/group/service/endpoint. The exact admitted
    HSM dependency is nShield 5s with pinned firmware at least 13.5 or nShield 5c with pinned
    firmware at least 13.6 and Security World
    Software 13.7.3. The content-signing RSA-3072 key is non-exportable, signing-only,
    `LogKeyUsage=yes`, protected by one non-persistent K-of-N Operator Card Set (OCS), and
    exposed only through the pinned CNG KSP while physical quorum is present. DACL is inventory,
    not a SYSTEM boundary. Successful use is the HSM-enforced quorum fact; unsupported
    individual operator identities or approval counts are not receipt facts.

    `CstPolicySigningOwner` holds one station-global cross-process ceremony lock and owns the
    only loaded-interval state machine: `UNAVAILABLE -> PRELOAD_AUDIT_ANCHORED ->
    LOAD_AUTHORIZED -> LOADED -> ONE_SIGN_TERMINAL -> FINAL_CARD_REMOVED ->
    KEY_UNAVAILABLE_PROVED -> AUDIT_RANGE_SETTLED -> COMPLETE`. The admitted profile is one
    exact module with enumerated local smart-card slots and excludes HSM pool, dynamic/remote
    slots, `preload pause`, second modules, and second provider/signing processes. Before card
    insertion it proves no prior ceremony/provider handle is live and obtains a fresh
    vendor-verified `{ESN,logID,collector-generation,database-identity,baseline-end-index,
    export-hash}` anchor. The lock remains held through unload and audit settlement.

    On every SignTool terminal result the owner closes/joins the child and closes all owned
    CNG provider/key handles, then requires physical removal of the final required OCS card.
    Entrust 13.7.3 specifies that a non-persistent OCS removes protected keys from HSM memory
    as soon as that card is removed. Pinned `slotinfo -m <exact-module>` must then report
    `Token=-` and `IC=0` for every admitted local slot and no remote/dynamic/associated/
    timeout/failed-secure-channel state. Enrolled non-persistence plus observed removal plus
    all-slot vendor readback is the unload/unusable oracle; CNG open, wrapper flags, timers,
    operator assertions, and SignTool exit are not. Target admission also proves outside a
    production ceremony that a fresh no-card exact-key operation cannot sign without a new
    quorum
    (<https://nshielddocs.entrust.com/security-world-docs/v13.7.3/nshield-security-world-v13-7-3-management-guide.pdf>,
    <https://nshielddocs.entrust.com/security-world-docs/v13.7.3/utilities/createocs.html>,
    <https://nshielddocs.entrust.com/security-world-docs/utilities/slotinfo.html>).

    The device's KML audit key and audit index are separate from the content key. The exact
    SignTool child receives one non-secret `NFAST_TRANSACTION_ID` equal to the request/profile
    digest. Firmware records that value, successful `Sign` status and content-key ObjectUse in
    a native nCore audit row. Exactly one `NShieldAuditCollectorOwner` per admitted HSM ESN
    owns the 5c privileged-client enrollment, explicit-ESN `nshieldauditd` YAML, collector
    service identity/health/exclusivity, per-ESN database, and serialized held JSON export.
    Its profile pins the 13.7.3 service/exporter absolute identities and hashes, config/database
    final identities/owner/DACL, KNETI public hash, absence of ambient audit-directory/config
    overrides, and an operator-attested estate inventory showing no second collector for the
    ESN. Split, unprivileged, drifted or unhealthy collection disables signing.

    Only after unload proof may pinned `nshieldaudit ncore export --esn <ESN> --logid <logID>
    --verify --file <new-output> --start <baseline-end+1> --end <post-unload-end>` settle the
    continuous loaded interval; it is the sole supported KML/warrant
    verification owner. It emits full JSON only: no text, malformed, overwrite, stdout, time
    filters, cached-only verification or direct database parsing. The collector retains and
    hashes the flushed no-follow output through receipt/media settlement.
    `CstOperatorSigningReceiptV3` binds request/artifact/profile, collector/config/exporter/
    output hashes, database final identity and collector generation, ESN/logID/range, vendor
    exit and projected rows. P18 does not parse
    raw KML or re-verify its cryptography; it independently verifies all pinned adapter facts,
    strict JSON grammar and continuous indices, then requires exactly one `Cmd_Sign` whose
    Transaction ID is byte-equal to the independently computed request/profile value, whose
    status is `OK`, and whose sibling ObjectUse joins exactly one earlier ObjectNew content-key
    hash by `{runID,objid}`. Every extra/duplicate Sign, absent/different Transaction ID,
    additional exact-key ObjectUse, or key-ambiguous Sign rejects the interval, including
    failed attempts. Since vendor
    audit does not bind result bytes, it proves only request transaction plus exact key use;
    P18 separately hashes and cryptographically verifies the held signed CIP, its embedded
    input, signer, chain and revocation. A wrapper counter, event log, SignTool output,
    result-byte attestation or station approval-count claim is never authoritative.
    These boundaries follow the vendor-documented nCore 13.5+ transaction, object-identity,
    audit and Audit Log Service contracts
    (<https://nshielddocs.entrust.com/security-world-docs/api-ncore/transaction-ids.html>,
    <https://nshielddocs.entrust.com/security-world-docs/v13.7.3/hsm-user-guide/hsm-mgmt/audit-logging.html>,
    <https://nshielddocs.entrust.com/security-world-docs/v13.6.15/hsm-user-guide/hsm-mgmt/nshield-audit-log-service.html>).

    Signing selects only the exact public certificate thumbprint in the pinned machine store
    and fixed embedded PKCS #7 App Control profile: outer signedData OID
    `1.2.840.113549.1.7.2`, embedded content OID `1.3.6.1.4.1.311.79.1`, RSA-3072, SHA-256,
    exactly one signer and one leaf certificate, authenticated attributes exactly contentType
    plus messageDigest, and no timestamp/countersigner/CRL/unauthenticated attribute. `/a`,
    subject selection, PFX, password, PIN, `/t` and `/tr` are forbidden. No private key,
    provider credential or application secret enters argv, environment, stdin, pipe,
    temporary file, manifest, journal or log. Provider-local HSM authentication may occur only
    inside its separately administered CNG provider; a provider requiring this application to
    carry a PIN/password is incompatible.

    After signing, the independent verifier parses the exact envelope, enables strong-signature
    checking and requires nonzero `CryptMsgControl(CMSG_CTRL_VERIFY_SIGNATURE_EX)` for signer
    zero with the exact pinned certificate. It independently requires the exact signer count,
    certificate/SPKI/issuer/serial, OIDs, RSA length, digest, signature algorithm/parameters,
    attribute set and message digest, then extracts an embedded CIP byte-equal to the held
    input and parses it identically. It builds one current-time chain to the pinned root,
    requires `CertVerifyCertificateChainPolicy(CERT_CHAIN_POLICY_BASE)` TRUE and `dwError==0`,
    end-entity/digitalSignature/exact Code Signing EKU, and one pinned hard-fail revocation
    profile: only `online-v1` exists. `CertGetCertificateChain` uses chain-excluding-root
    revocation, cumulative bounded retrieval and disabled root auto-update, with no cache-only,
    AIA-disable, ignore, timestamp-time, clock override or trust fallback. Exact ordered chain,
    clean trust status and base-policy `dwError==0` are required. Offline/unknown/stale/timeout/
    unreachable/absent-distribution/alternate-chain/revoked results produce zero signing
    handoff and zero CiTool. `offline-v1` is an unknown rejected profile.

    The ceremony journal keys request/unsigned/profile/certificate/provider/content-key/
    collector-profile/ESN/logID hashes. Exact completed retry returns the same artifact/receipt; pending/conflicting/
    replayed state never re-signs automatically and requires a reviewed forward version. Its
    export is one continuous vendor-verified JSON range from the admitted pre-load anchor
    through final-card/all-slot key-unavailability proof and a post-unload end index, with
    exactly the intended Sign/ObjectUse and no sibling/ambiguous use, malformed/unverified
    segment, missing index, or `AbnormalShutdown` ambiguity. `CstOcsUnloadReceiptV1` binds
    public ceremony/request/profile/key/OCS hashes; quorum/non-persistence; ESN, module/local
    slots; both audit anchors; SignTool image/process terminal fact; intended Transaction ID;
    provider/key-handle closes; removal ordinal; pre/post `slotinfo` image/argv/exit/output
    hashes and slot `{Token,IC,flags}`; Sign/intended-use/sibling-use/ambiguous counts; held
    input/output identities; and cleanup/quarantine state. Any present/nonzero/wrong/remote/
    dynamic slot, live process/handle, wrong/extra/ambiguous Sign or ObjectUse, transaction
    inequality, gap, reinsertion, or identity drift falsifies it. Ceremony closes HSM,
    provider/content-key, process/file/audit resources; target closes import/store/message/
    chain/virtual-disk contexts. Leak canaries
    cover request/receipt/removable media/argv/env/stdin/stdout/stderr/log/dump/journal/
    manifest/temp/publication. Unproved cleanup fails. Pin drift, contradictory/empty output, AppID ambiguity,
    broad/extra/missing rows, XML/CIP parser disagreement, non-identical builds, key/export/
    leak failure or signer/OID/algorithm/attribute/signature/content/chain/revocation mismatch returns
    `policy_artifact_unavailable`, performs zero deployment and leaves registration absent;
    no alternate toolchain, rule, key or trust route exists. Cancel, timeout, reader/provider/
    collector failure, stuck card, and crash/restart all enter the same terminalization. Until
    removal, all-slot absence, and audit settlement are proved, the journal remains
    `OCS_INTERVAL_QUARANTINED`, output is destroyed or retained unaccepted by exact identity,
    signing and registration stay disabled, and recovery may only finish cleanup; it cannot
    sign, rotate, retry, or hand off. HSM clear/isolation is separately approved incident
    response, never an automatic fallback.

    P18 has one `AuditReceiptReplayOwner`, separate from collection, content signing and policy deployment.
    Its write-through append-only journal is keyed by `{ESN,logID,collector-profile}` and starts
    only from a reviewed privileged-collector attestation plus vendor-verified genesis JSON
    `endIndex`. Imports serialize. A new
    request supplies a continuous range starting at the next index and ending strictly greater;
    before artifact admission the owner appends/fsyncs `prepared` with previous/candidate indices
    and exact request/receipt/CIP/staging hashes. A gap, reset, alternate log/device, concurrent
    import or different equal counter fails closed. Crash recovery permits only the exact
    prepared tuple to settle; if CiTool might have started, reconcile installed/reboot state
    without a second call. Exact completed equality returns stored result with zero CiTool.

    `NShieldAuditCollectorOwner` serializes collection and export in one durable journal. On
    every success, failure, cancellation, timeout and crash it reaps exporter process/thread,
    closes service-control/query/database/output/parent/media handles once, and deletes only an
    exact unaccepted output whose identity still matches; ambiguous cleanup disables signing.
    Restart preserves the exact profile/database and must regain continuous `query` health
    before signing resumes. Config, host, KNETI or database rotation stops signing and service,
    vendor-verifies the terminal old range, fsyncs it, takes the documented stopped-service
    consistent backup with owner/DACL preservation, proves one successor privileged collector,
    then accepts only same-log continuation or a separately reviewed new-log genesis. Live DB
    copy, dual collectors, rollback to an older profile, `serveradmin --delete`, `--force`,
    `--delete-status`, `--recreate-db`, inferred gaps and automatic weaker recovery are forbidden.

    One `SignedPolicyImportOwner` copies untrusted media through held no-follow source handles
    into a protected local single-artifact staging VHDX. It verifies final path/volume-file ID/
    link/default stream/owner/DACL/size/hash/signature/semantics, flushes and detaches the writable
    image, then reattaches the same backing VHDX with `READ_ONLY|NO_DRIVE_LETTER`. It retains
    backing/volume/mount/ancestor/leaf read handles with `FILE_SHARE_READ` only across journal
    prepare, exact CiTool child, exit and immediate identity/hash recheck. CiTool is explicitly a
    pathname consumer; its path derives from final-path equality to the held leaf, not from media.
    If installed CiTool cannot consume this retained read-only path, the target is incompatible
    and makes zero call; close/reopen and writable/media-path fallback are forbidden.
    This deliberately composes Microsoft's documented read-only virtual-disk attach with
    CiTool's pathname-only update contract; it does not invent a CiTool handle API
    (<https://learn.microsoft.com/en-us/windows/win32/api/virtdisk/ne-virtdisk-attach_virtual_disk_flag>,
    <https://learn.microsoft.com/en-us/windows/security/application-security/application-control/app-control-for-business/operations/citool-commands>).

    The stopped policy owner serializes on one protected machine mutex and durable idempotency
    journal keyed by PolicyID/version/CIP hash/BasePolicyID/pre-inventory digest. It uses
    elevated installed `CiTool.exe --update-policy` only on that retained read-only staging path and `--remove-policy` only; ConfigCI is
    offline build input, and direct copy, Group Policy, MDM/CSP, WMI or mixed deployment is
    forbidden. Install, update, removal and rollback always require an observed reboot,
    even when the platform offers rebootless behavior. After boot the owner must repeat the
    complete authority-attributed inventory, verify exact signed/enforced supplemental
    identity and deployed CIP hash, then run allowed-row, hostile direct/forwarded/indirect/
    native pre-entry denial, and unrelated-canary probes. Only that conjunction emits the
    sole `AppControlPolicySetCommittedV1`. Removal first disables registration/stops all
    services, removes only the sampler PolicyID, reboots, proves it absent from CiTool and
    both OS/EFI active locations with sibling inventory/canaries restored, and only then
    emits `AppControlPolicySetAbsentV1`. Same-key retry resumes or returns the same receipt;
    a conflicting request, crash, cancellation, timeout, unknown state, or missing reboot
    latches `policy_set_unsettled`. Forward-versioned signed prior content is the only
    content rollback; remove/reboot/absence is rollback to disabled. Neither path touches
    siblings or advances VHDX detach/selection before absence.

    Content-signing-key rotation first disables registration/request export, stops processes, settles
    target and ceremony journals, removes the old profile and requires HSM authority/audit
    proof that the old key is disabled/destroyed; target SYSTEM never owns this action. Only a
    separately reviewed base-authorized successor under the same physical quorum may sign a
    forward-versioned replacement. The same independent receipt/signature,
    chain and revocation checks plus CiTool/reboot/composition/unrelated-canary proof must
    pass before re-enable. Missing, conflicting or partially rotated state remains
    `policy_artifact_unavailable` or `policy_set_unsettled`; old-key reuse, same-version
    replacement, automatic signing retry and unsigned removal are forbidden. Audit authority
    rotates independently: stop signing, finalize/export/verify the old log without gaps, pin
    terminal `{ESN,logID,endIndex}`, reinitialize only under reviewed authority, enroll a new
    KML warrant/logID plus vendor-verified genesis JSON, and permanently deny old-logID receipts for new
    requests. Firmware upgrade without finalization, gap/reset or partial journal transition
    remains `policy_artifact_unavailable`.

    The exact committed receipt, not command success, refresh, file presence or callback,
    is consumed by provisioning, service startup, frontend/worker creation and registration.
    Under it, direct, forwarded, indirect, absolute and native requests for every unlisted
    package or Microsoft image must be denied by User Mode Code Integrity before first
    instruction. Target proof, not prose or import scanning, admits the positive allowlist
    composition. Audit, broad publisher/path/filename/default/AllowAll, reputation/managed-
    installer fallback, unsupported policy and ambiguous denial keep the sampler unregistered.

    While frontend/services are stopped and all process, Job, mapped-module, virtual-disk
    and package handles are absent, P18 creates a complete per-version VHDX, verifies it
    offline, then attaches it read-only without a drive letter beneath one protected
    service-owned mount point. It retains the virtual-disk handle and emits a host-specific
    `ProvisionedPackageIdentityV1` binding the portable closure, policy and VHDX hashes to
    backing-file/virtual-disk/volume/mount-point/root/row identities, protected DACL,
    read-only attach and absence receipts. The stopped provisioner alone may detach and
    atomically select another complete reviewed image; no mounted-image update exists.

    Before first package load the owner retains a no-follow DELETE-denying chain from host
    volume root through every mutable backing-file and mount-point ancestor, attached
    volume/version root and package row. Directories use read-attribute/synchronize,
    `FILE_SHARE_READ` only and directory/reparse flags; files add read-data and use
    `OPEN_EXISTING`. Every identity/path/locality/owner/DACL/link/reparse/hash/signature/
    cardinality fact is read back. Because Windows sharing is per stream, the unnamed
    stream handle is not claimed to block alternate data streams. Complete stream
    enumeration before/after acquisition and every load/callback boundary must find only
    `::$DATA`, while the read-only attachment must reject stream and metadata mutations.
    Any detach/remount, backing/mount/ancestor/version/file substitution, named-stream
    mutation or unavailable detector is target incompatibility without retry.

    Only then it calls `SetDefaultDllDirectories(LOAD_LIBRARY_SEARCH_SYSTEM32)` and
    absolute `LoadLibraryExW(...,DLL_LOAD_DIR|SYSTEM32)`. Windows resolves by pathname and
    may execute TLS/`DllMain` before return: retained objects establish namespace/content
    continuity, while enforced per-app UMCI is the sole pre-execution image-admission
    boundary. Mapped inventory remains defense-in-depth and settlement evidence. The owner
    resolves only generated ABI symbols, uses closed `PyConfig`, and retains the complete
    chain through application/session close, Python finalization and mapped settlement.
    Every policy-set commit/attach/acquire/load/callback/symbol/config/init/cancel/timeout/shutdown
    return reverse-closes exact owned resources; incomplete proof is
    `native_loader_settle_failed`, leaves the image read-only attached, quarantines and
    permits no weaker policy/share/search/reopen/retry/descendant/Python/source/vendor/CST/
    wrapper/launcher route. Only stopped P18 with zero-reference proof and
    `AppControlPolicySetAbsentV1` may detach and roll forward/back to a complete reviewed image.
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
     exact signed/hash-pinned `mcphub-cst-runtime.exe --role=frontend` direct child, and
     remote/anonymous/network clients are denied.
     This endpoint can request only the closed sampler operation; it grants no source
     selector and the daemon/broker policy rechecks remain authoritative.
     Before the actual CST stdio child is created, `internal/daemon.StdioHost` uses
     `BCryptGenRandom(NULL,buf,32,BCRYPT_USE_SYSTEM_PREFERRED_RNG)`, computes exact
     SHA-256, and sends only that non-authoritative verifier over the separate fixed
     daemon-owned `HubEnrollmentProtocolV1` pipe. Daemon authenticates the peer as the
     current supervisor-tracked direct CST child by independently querying the existing
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

     CST manifest schema and materialization use protected
     `launch_profile: cst-direct-v1`, not a free-form command. Provisioning resolves the
     profile to one absolute canonical signed/hash-pinned package-owned
     `mcphub-cst-runtime.exe` plus fixed `--role=frontend` and immutable bundle-manifest
     identity. `HostConfig.Command` is that exact image; StdioHost rejects PATH lookup,
     `uvx`, console-script launchers, wrapper descendants, wrong package/hash/image and
     descriptor drift whenever launch capability is enabled. This extends the existing
     T02 owner in place: `HostConfig.LaunchCapability`, `LaunchCapabilityConfig`,
     `prepareLaunchCapability`, `preparedLaunchCapability.apply/start/cancel/close`,
     `windowsLaunchCapabilityPipe`, `productionLaunchCapabilityOps` and
     `cstLaunchCapabilityConfig`. Existing `launch_capability_windows.go` replaces its
     `AdditionalInheritedHandles` path in place: it requires the slice initially empty,
     appends only the capability read handle once, keeps `NoInheritHandles=false`, and
     rejects Token, ParentProcess, ProcessAttributes, ThreadAttributes or any other
     inheritance value; direct-image verification precedes start. Installed Go 1.26.5
     appends the three standard `ProcAttr.Files` handles then this singleton, removes
     nulls and serializes the ordered four handles as
     `_PROC_THREAD_ATTRIBUTE_HANDLE_LIST` (`syscall/exec_windows.go:390-424`). No raw
     frontend STARTUPINFOEX/process owner is introduced; the platform fallback remains
     in existing `launch_capability_other.go`. No second config, prepared lifecycle,
     Windows launch file, enrollment ledger or cancellation path is introduced.
     Capability bytes pass solely through an explicit anonymous-pipe read handle in a
     four-entry `PROC_THREAD_ATTRIBUTE_HANDLE_LIST` containing stdin/stdout/stderr/read-
     handle only;
     write end is non-inheritable, `bInheritHandles=TRUE`, and fixed
     `MCPHUB_CST_LAUNCH_HANDLE=<decimal>` is a non-secret locator. Parent writes exactly
     32 bytes before start and closes its duplicates on every exit. Before any embedded
     CPython initialization, the native frontend's first application code clears and
     reads back inheritance on stdin/stdout/stderr/capability, requires exact 32-byte
     read plus EOF, closes the capability handle, clears the locator and uses
     `SecureZeroMemory` on temporary buffers. It initializes the same signed bundle's
     CPython with the closed `PyConfig`, invokes the existing `cst.py` stdio composition
     in the same authenticated OS process, supplies the launch value once as a mutable
     buffer and zeroes native/Python copies after the daemon challenge. Capability is
     absent from argv/manifest/intent/logs and only the non-secret locator is in
     environment. Daemon then sends a fresh CNG one-use
     challenge and consumes both enrollment and challenge before admission.
     Authenticated Enroll consumes only its channel nonce and creates capability state
     `ISSUED -> ENROLLED`; ACK/flush/close leaves it ENROLLED. Exact native child
     read+EOF and
     successful daemon challenge changes `ENROLLED -> CONSUMED`. Fresh independently
     authenticated `CancelEnrollmentV1`, start/write/read failure, child exit, expiry,
     shutdown or restart changes `ENROLLED -> CANCELLED` and removes the digest.
     disconnect, duplicate, reconnect, shutdown and exit terminalize both ledgers and
     close all handles and zero buffers; ambiguity quarantines. This is an admitted
     manifest-schema/materialization/provisioning, existing `HostConfig.LaunchCapability`/
     `StdioHost.Start`/`prepareLaunchCapability` Windows owner, `internal/api` enrollment/
     supervisor-status/DACL, `internal/cli/daemon.go`, and signed native-runtime bundle
     change. The current four-handle enrollment seam is reused and strengthened; only
     direct-image-profile validation and exact tuple construction are added.
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
17. Topology is exactly pre-spawn `supervisor-tracked CST StdioHost -> direct signed
    mcphub-cst-runtime.exe --role=frontend -> HubEnrollmentProtocolV1 -> SCM daemon`,
    then application `hub -> same-process embedded-Python cst.py stdio frontend ->
    FrontendDaemonProtocolV1 -> SCM daemon ->
    BrokerProtocolV1 -> broker-owned contained worker -> vendor`. Enrollment uses its
    dedicated protected descriptor, closed challenge/enroll/cancel/receipt schema and
    capability state `ISSUED -> ENROLLED -> CONSUMED|CANCELLED`; no frontend admission
    occurs before authenticated enrollment and frontend-challenge consumption. The
    frontend remains the existing-six compatibility
    and MCP publication owner. The daemon is the sole authenticated broker-pipe client
    and owns sampler admission, original 60-second deadline, broker-pipe cancellation,
    receipt validation and quarantine; it never reads source or spawns a worker.
    Broker independently authorizes policy/nonce, opens policy-derived source and fresh
    protected workspace root capabilities, atomically supplies their least-right
    duplicates with the three standard handles, spawns/contains the exact signed native
    worker image, and
    merges broker-local capability/Job/session/lease/workspace receipts. After its
    native first-instruction proof the bootstrap reconstructs only the two numeric
    locators from `WorkerPreMainBootstrapV1`, validates directory type/identity/role,
    revokes inheritance and returns `WorkerPreMainReceiptV1`. Broker accepts it before
    sending the application frame; only then does closed embedded CPython run the worker,
    perform all descendant opens handle-relatively, and return one locally settled
    capability/application result.
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
`v1` schemas; the broker-worker schema contains the native pre-main prelude/receipt and
the later locator-free application frame. Policy entries use
only `sha256-canonical-file-list-v2`. The superseded design-only v1 manifest is
rejected and regenerated out of band, never interpreted or migrated. Before first
publication there is no migrated sampler state. Migration is expand then contract:
deploy hub parsing/materialization for protected `cst-direct-v1` without selecting it;
extend the existing T02 launch-capability owner and tests in place, preserving singleton
`AdditionalInheritedHandles` and rejecting every other `SysProcAttr` inheritance,
process-identity or security field; reject/delete any duplicate config,
prepared lifecycle, platform file, enrollment/cancel ledger or compatibility alias;
install and verify every signed/hash-pinned native runtime, CPython DLL, stdlib,
extension and application `PackageLoadClosureV1` row; require the exact independent PE/
import/delay/TLS/load-config/entry/section closure, protected-root owner/DACL/local-volume/
default-stream receipt, two-build reproducibility receipt, target-OS pre-entry and
System32/API-set manifests, entry-disassembly proof, hostile DLL/TLS canaries, retained-
share slow-DllMain mutation proof and mapped-to-held CPython/module receipt; with sampler disabled A/B legacy uvx and
direct native launch against exact existing-six catalogue, signatures, request/response
bytes, validation/error bodies, stdin/stdout ordering, prompts/resources and shutdown;
atomically select the materialized direct descriptor and restart; prove enrolled PID/
image/package/parent is the direct native frontend and no uvx/wrapper exists; only then
prove repository symbol/file scans contain exactly one T02 frontend launch owner and
that the independent broker containment owner alone constructs the five-handle worker
tuple; only then remove uvx from CST required binaries. The sampler may be enabled only after later
target admission gates.

Before sampler publication rollback may restore the prior package pin and uvx descriptor
only with launch capability absent and sampler disabled. After sampler publication it
may select only a previously reviewed direct-bootstrap bundle; capability-bearing uvx/
wrapper rollback is forbidden. Removing/setting `enabled=false` leaves no worker, workspace, cache,
or saved session survives a call, and no admission waiter survives daemon
termination. A future policy/schema default flip,
transport change, non-Windows implementation, or hot reload requires a successor
decision. Rollback/recovery stops daemon (closing pipe/client lease), requests broker
settlement, then stops broker and observes both service handles signaled. Broker's
last Job handle alone is the worker-tree kill boundary. Restart revalidates policy and the
containment startup probe, exact supervisor/SCM identities, all three pipe descriptors,
all four protocol schemas and policy equality
before registering the sampler.

`BrokerWorkerProtocolV1` is corrected in place before publication: native startup proof
precedes fixed `WorkerPreMainBootstrapV1`; `WorkerPreMainReceiptV1` proves both
capabilities identity-validated and all five inherited flags clear before broker sends a
locator-free application request; its response requires `WorkerCapabilityReceiptV1`;
and broker containment requires `BrokerCapabilityReceiptV1`. Missing prelude/receipt,
bundle identity, closed `PyConfig`, root/open/share/duplicate tuples, inheritance-epoch
facts, immediate parent closes, or five native inherit-clear readbacks do not negotiate,
default, initialize Python, or select an in-process fallback. The decision remains
`proposed`; earlier acceptance does not cover this semantic change.

The migration never admits a capability-bearing runtime from hash/signature alone: exact
loader closure, reboot-observed complete-policy-set commitment, enforced per-app UMCI and
read-only-VHDX namespace receipts are mandatory
at provisioning, service startup, frontend creation and worker creation. Rollback may
select only a prior direct-bootstrap VHDX whose exact policy and target closure pass again,
and only after stopped P18 proves zero service/process/Job/mapping/handle references,
obtains `AppControlPolicySetAbsentV1` for its own supplemental and detaches the current image.
A missing
toolchain, unsupported/audit/ambiguous policy, malicious-loader canary execution, changed
operating-system closure, CPython drift, attach/stream/namespace mismatch, live reference,
or failed detach keeps the sampler default-off/quarantined; it does not reactivate uvx,
mount read-write, broaden policy, or select a wrapper.

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
- Serializing or passing a Python composition/application object: `CreateProcessW`
  crosses a process boundary and object identity is neither serializable authority nor
  a kernel capability.
- Ordinary direct Python module worker launch: CPython can import global `site`,
  executable `.pth` and customization before Python worker code revokes handles; command
  flags cannot establish pre-main proof. Native bootstrap revokes first and then uses a
  closed `PyConfig` with immutable bundle paths.
- Dynamically linked CRT or load-time CPython/package import in the native image: DLL
  entry points, TLS callbacks, and runtime static initialization can execute before the
  executable entry revokes inherited handles. The selected no-CRT KERNEL32-only PE plus
  target-observed system loader closure is the only admitted pre-revocation code.
- A separate tiny launcher which then starts another runtime: transfers the same
  capability through an extra process/launcher boundary and creates a second lifecycle.
  The one admitted PE revokes first and loads CPython in the same process afterward.
- `uvx` or another wrapper on the launch-capability route: the current manifest launches
  uvx and its documented contract resolves/installs then invokes the tool executable, so
  capability possession and authenticated PID would name the wrapper rather than the
  frontend. Protected direct launch profile is mandatory.
- Direct installed `New-CIPolicyRule -AppID -Level Hash`: the pinned implementation
  rejects Hash before rule creation despite advertising it. Empty output or a broader
  fallback cannot mediate malicious module entry.
- Silent ConfigCI upgrade, App Control Wizard output, or XML-only hand editing: none has
  an exact tool/version/hash, deterministic two-build, and independent compiled-CIP parser
  contract. Unsupported authoring stays default-off.
- Subject-name/automatic certificate selection, PFX/password files, exportable software keys,
  successful SignTool exit, decoded content equality alone, permissive revocation-ignore or
  online-to-offline fallback: none proves the exact signer or contains signing authority.
  The selected exact HSM-quorum ceremony, request-bound audit receipt, single-signer envelope
  and independent online-only signature/chain/revocation verification remain mandatory.
- Policy/source/workspace paths in argv, environment, request, or a worker-side policy
  reload: create ambient or split authority and permit a different object to be opened.
- A fourth named pipe or named shared-memory object for bootstrap: adds an independently
  authorized endpoint/name/replay lifecycle although the broker-owned anonymous stdin
  and atomic inheritance tuple already bind the child.
- `DuplicateHandle` into an already-running worker: creates a post-start authority gap,
  requires broader process rights and cannot make capability availability atomic with
  Job membership. Capability handles are therefore in the same creation-time tuple.
- `DUPLICATE_SAME_ACCESS`, omitted `dwDesiredAccess`, `FILE_SHARE_DELETE`, or leaving
  inherited flags for eventual process cleanup: each leaves effective authority or
  transitive retention outside the falsifiable contract. Exact access/options plus
  broker/worker readback and immediate revocation are mandatory.
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
- Post-return mapped-module comparison, static import scanning, preloading named rows, or a
  Python/native import hook: an admitted DLL can reach another loader path from TLS,
  `DllMain` or a callback before mediation. These remain inventory/settlement evidence;
  only target-proved enforced per-app UMCI can admit an image before first instruction.
- Ordinary protected directory or unnamed-stream share lock: `LoadLibraryExW` consumes a
  pathname rather than a held handle and Windows sharing is per stream. The selected
  read-only VHDX plus retained backing/volume/mount/ancestor/version/file chain and explicit
  stream enumeration is required; any target where detach/remount or ADS mutation succeeds
  is incompatible.
- A dedicated sampler base policy: all active base policies intersect, so it can block
  unrelated applications machine-wide. The selected artifact is a supplemental only, and
  it is admitted solely when the complete selected base-family union is already closed for
  the runtime AppID and every other base intersection plus unrelated canary stays compatible.
- Machine-wide or user-wide broad AppLocker/App Control allow/deny policy: it changes
  unrelated application behavior and does not prove this process's exact module closure.
  The selected supplemental is exact-runtime per-app, exact-hash and target-falsified.
- Target-local key guarded by a local group, service identity or DACL: an enabled SYSTEM or
  group caller can bypass the named mutex/journal and produce the same public signature. The
  target therefore has no signing authority; only the isolated HSM physical quorum may sign.
- Wrapper-signed receipt, generic CNG use count, Windows event log or station counter: none is
  the device's authenticated request-bound sequence. Only a fresh held JSON range produced by
  the sole pinned `nshieldaudit --verify` collector, joined through `{runID,objid}` ObjectNew
  to successful Transaction-ID-bound Sign/ObjectUse and anchored by the reviewed collector
  genesis, is admitted; individual approver identities and result-byte attestation are not inferred.
- Verify media bytes then pass the media path, or close/reopen a local file before CiTool:
  both break identity at a pathname-only consumer. The owner uses a read-only staging VHDX
  and retained identity chain; inability to coexist is incompatible, not a fallback trigger.
- `offline-v1`, hashed CRL/OCSP files beside CryptoAPI, or ambient cache-only revocation:
  none proves which bytes the chain engine consumed. Offline support is deleted; only bounded
  fail-hard `online-v1` is accepted.

## Promotion gate

Promotion from `proposed` to `accepted` requires both an independent Claim-Verify
architecture `PASS` and an independent security `PASS`, each explicitly bound to the
exact corrected design and proposed-decision SHA-256 values and covering atomic
admission, supervisor/enrollment/frontend/daemon/broker identity and all three pipe descriptors,
policy/manifest-v2, shared Windows namespace identity, complete
source-to-isolated-workspace transfer, exact share modes/header seal, escaped-probe
settlement, the exact no-CRT PE import/delay/TLS/entry closure, target pre-entry mapped-
module manifest, entry disassembly/canaries, read-only-VHDX provisioning and retained
backing/volume/mount/ancestor/version/file chain, independent `PackageLoadClosureV1`,
    pinned ordinary four-hash ConfigCI extraction, deterministic XSD policy authoring,
    independent runtime OriginalFilename/hash proof, target signing-authority absence,
    nShield OCS content-key, separate KML audit-key/counter and sole privileged
    collector/service/config/database/vendor-verify lifecycles, pre-load-to-post-unload OCS
    interval, final-card/all-local-slot absence, intended-transaction equality and zero
    sibling/ambiguous exact-key use across every failure/restart return,
    collector-genesis replay journal and read-only staging-VHDX CiTool handoff,
    single-signer PKCS #7 OIDs/algorithms/
    attributes, explicit CryptoAPI signature verification, pinned current-valid chain and
    hard-fail online-v1 revocation,
    XML/unsigned-CIP/signed-content parse,
    zero-deploy default-off failure, exact-runtime per-app enforced UMCI deny-before-entry for malicious dynamic loads,
    complete authority-attributed policy inventory, selected-base-family union and other-base
    intersection, serialized idempotent CiTool lifecycle and reboot-observed committed/absent events,
explicit per-stream enumeration, target System32/API-set manifest, mapped-to-held identity, and hardened
post-revocation CPython load, and
`HubEnrollmentProtocolV1`, `FrontendDaemonProtocolV1`,
`BrokerProtocolV1`, and `BrokerWorkerProtocolV1` schemas. These two review verdicts are the
complete metadata-promotion gate. Decision acceptance authorizes planning and
implementation against these contracts; it does not claim target-runtime proof.

Before sampler registration, package pinning, release, or deployment, the owning
    delivery gates still require a target Windows/CST trace proving exact signing provider/key
    identity, non-exportability/OCS/base authorization, final-card removal, exact-module
    all-local-slot absence, no-card key unusability, intended-transaction equality and zero
    sibling/ambiguous exact-key use in the pre-load-to-post-unload KML/ESN/logID/index range,
    durable replay/crash recovery, read-only staging identity/CiTool coexistence,
    secret-channel exclusion, single-signer
    envelope, signature/chain/revocation profile and rotation/old-key denial; exact AppID/hash policy
    composition, sibling-authority inventory, unrelated-canary preservation, reboot-observed
    commit/removal/absence and Code Integrity denial before first instruction for direct/forwarded/
indirect/native malicious loads; read-only VHDX/backing/volume/mount/ancestor/version/file
identity and DACL receipts; preexisting-writer, detach/remount, write/delete/rename/hardlink/
reparse and named-stream create/write/rename/delete denial through an engineered DllMain
window; exact `::$DATA` enumeration; mapped-to-held equality; and stopped zero-reference
policy removal/detach/rollback, distinct vendor token/protected DACL, read-only share masks,
isolated header write/share-0 seal and ResultTree compatibility, atomic Job
membership, truthful breakaway rejection plus exact escaped-child settlement, 60-second
termination/zero-accounting settlement, bounded streams, no visible console, and
preservation of an interleaved foreign CST process. Architecture Claims 7 and 15 and
the Line10 acceptance gate likewise remain mandatory at their stated implementation
and release stages. Acceptance neither waives them nor changes their target-only
status.

## Terms and Abbreviations

- ADS — Windows Alternate Data Stream.
- CNG — Windows Cryptography Next Generation key-storage and signing API.
- CRL / OCSP — certificate revocation list / Online Certificate Status Protocol evidence.
- CST — CST Studio Suite.
- HSM — hardware security module; the isolated ceremony's sole private-key boundary.
- IC — insertion counter reported by `slotinfo`; zero means no token is present.
- KML — nShield module audit-log signing key, distinct from the policy content-signing key.
- OCS — nShield Operator Card Set enforcing physical K-of-N key authorization.
- Job Object — Windows kernel object that manages a process group as one unit.
- MCP — Model Context Protocol.
- PKCS #7 — cryptographic signed-message envelope required for signed App Control policies.
- Win32 — Microsoft Windows native application programming interface.
