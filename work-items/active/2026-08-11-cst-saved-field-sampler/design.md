# Architecture Design — CST Saved-Field Point Sampler

## Decision summary

### C6 implementation-gap semantic correction

The deployable topology is exactly `supervisor-tracked CST StdioHost -> direct pinned
mcphub-cst-runtime.exe --role=frontend -> HubEnrollmentProtocolV1 -> SCM
McpLocalHubCstDaemon`, then `hub -> same embedded-Python cst.py stdio frontend ->
FrontendDaemonProtocolV1 -> SCM McpLocalHubCstDaemon ->
BrokerProtocolV1 -> SCM McpLocalHubCstVendorBroker -> contained worker -> CST`.
The existing six tools and their current local call paths remain owned by `cst.py`; only
the seventh tool crosses the frontend pipe. All sections below use this same ownership.

Concrete P10-P17 production seams are:

| Module / symbol | Required production ownership |
|---|---|
| `cst.py::_compose_saved_field_tool` | With enabled valid policy, construct `WindowsDaemonClient(WindowsNamedPipeDaemonTransport(FRONTEND_ENDPOINT))`; register only after launch-capability handle intake and current endpoint/service/image/policy proof. `UnavailableDaemonTransport` is fail-closed only. Existing six compositions do not change. |
| `cst_saved_field_frontend_protocol.py` | Own bounded challenge, `FrontendDaemonRequestV1`, `FrontendDaemonResultV1`, cancellation, correlation, framing and frontend nonce terminalization. Request contains the non-authoritative stable `entry_id` plus `SavedFieldRequestV1`; it contains no source path, bytes, handle, manifest, policy revision or authority override. |
| `cst_saved_field_daemon_service_windows.py::main` / `run_service` | Fixed SCM service entry point; independently load policy, own `SamplerAdmissionGate`, create the original QPC triple, authorize the closed sampler request, use the real broker client, validate broker plus transport receipts, quarantine on ambiguity, return one bounded result. |
| `cst_saved_field_broker_service_windows.py::main` / `run_service` | Fixed SCM broker entry point; independently load policy, resolve/apply/read back exact pipe security, authenticate daemon SCM PID/token/service SID/session/image, own nonce ledger, open source/workspace root capabilities, construct `WorkerPreMainBootstrapV1`, invoke containment, delete the workspace root after child close, and settle terminally. |
| Native CST runtime bootstrap (`mcphub-cst-runtime.exe`) | The one package-owned pinned x64 Windows PE image for both fixed roles. Its load-time import table contains only `KERNEL32.dll`; it has no delay imports, TLS directory/callback, C/C++ runtime, static initializer, package DLL, wrapper, or alternate launcher. A custom no-CRT entry revokes and reads back the frontend four-tuple or worker standard three before package-extensible code can run. Worker then validates/revokes its two capabilities. Only after revocation does the same image validate the enforced exact-runtime per-app module policy, retain the read-only-VHDX namespace/content closure, load the absolute pinned CPython DLL with closed flags, initialize isolated `PyConfig`, and invoke one pinned entry function. |
| `cst_saved_field_broker_worker.py::main` | Runs only after native startup/prelude/revocation and isolated embedded-CPython initialization; decodes the application request, composes from native-validated handle objects, runs one transaction and returns one result. Python owns no pre-main inheritance fact. |
| `WindowsContainedInvocation.invoke` | Launch exact pinned `mcphub-cst-runtime.exe --role=worker`, atomically create five-handle child, validate native startup proof, send `WorkerPreMainBootstrapV1`, validate native revocation receipt, then send the application request; return only actual kernel/owner facts. |
| `SavedFieldBrokerService.exchange` | Consume the exact containment receipt without defaults or Boolean synthesis and create `BrokerResponseV1` from worker/session/workspace/lease facts only. |
| `WindowsNamedPipeBrokerTransport.exchange` | Observe the response frame, flush/terminal frame, EOF/cancel and local handle close, then return `BrokerExchangeReceiptV1`; pipe settlement is not a pre-close field inside `BrokerResponseV1`. Daemon success requires both receipts. |
| `WindowsNamedPipeDaemonTransport.exchange` | Frontend locally returns `FrontendTransportReceiptV1{correlation,response_frame_complete,terminal_frame_complete,eof_or_cancel,client_handle_closed}` after its own observations; publication requires this receipt. It asserts no daemon-local fact. |
| Frontend daemon-pipe server | Locally returns `DaemonResponseReceiptV1{correlation,response_frame_written,terminal_frame_written,flush_complete,ack_received,disconnect_complete,server_handle_closed}`; daemon admission release requires this receipt. It asserts no frontend EOF/client-close fact. |
| Go hub launch owner and enrollment client | Extend the existing T02 owner in place: `internal/cli/daemon.go::cstLaunchCapabilityConfig` supplies `daemon.HostConfig.LaunchCapability`; `internal/daemon/host.go::StdioHost.Start` creates the direct pinned `exec.Cmd` and its three standard pipes, then calls existing `prepareLaunchCapability`/`preparedLaunchCapability.apply|start|cancel`; `internal/daemon/launch_capability.go` owns enrollment, digest, write, locator and all-return cancellation; existing platform files `launch_capability_windows.go`/`launch_capability_other.go` own the additional inherited handle. `windowsLaunchCapabilityPipe.apply` preserves `exec.Cmd` ownership: require singleton empty `SysProcAttr.AdditionalInheritedHandles`, append exactly the capability read handle once, keep `NoInheritHandles=false`, and reject any pre-existing `Token`, `ParentProcess`, process/thread security attributes or other inheritance field/handle. Installed Go 1.26.5 `syscall.StartProcess` appends the three `ProcAttr.Files` standard handles then `AdditionalInheritedHandles`, removes nulls, and serializes exactly that slice as `_PROC_THREAD_ATTRIBUTE_HANDLE_LIST` (`$GOROOT/src/syscall/exec_windows.go:390-424`). No raw frontend `STARTUPINFOEX` owner is added; direct-image validation remains before `exec.Cmd.Start`. |
| `HubEnrollmentProtocolV1` | New Go client plus Python server in `cst_saved_field_hub_enrollment_windows.py`; daemon authenticates the current supervisor-tracked CST host by independently querying extended supervisor IPC status and comparing peer PID/kernel creation time/canonical installed image/token/session with its exact task row. |

The daemon samples `{frequency, admitted_tick, deadline_tick=admitted_tick+60*frequency}`
once. Broker challenge `issued_tick` is its later current QPC sample, not a copy of
`admitted_tick`; it must satisfy `admitted_tick<=issued_tick<deadline_tick`, and
`expires_tick=min(issued_tick+5*frequency, deadline_tick)`. Every receiver verifies the
unchanged triple and current bounds. Containment derives waits and the worker cutoff from
that triple; only after termination may it create `termination_tick+10*frequency` for
cleanup.

`NonceLedger` owns `ISSUED -> CONSUMED|CANCELLED` exactly once. No-challenge failures
return without a ledger entry. After challenge, local validation/encoding failure,
exchange failure, cancellation, timeout, disconnect and shutdown must all attempt
`CancelChallengeV1` in `finally`; a missing or nonterminal receipt quarantines. Once the
broker accepts a request it consumes before authorization; every subsequent validation,
policy, source, launch, worker, response, disconnect and service-stop exit runs the one
settlement state machine and leaves the nonce terminal. Duplicate/replay/stale terminal
operations do zero privileged work.

The frontend boundary has its own restart-scoped `FrontendChallengeLedger` with the
same terminal states. Before child creation the actual `StdioHost` owner generates one
256-bit launch value using `BCryptGenRandom(NULL, buffer, 32,
BCRYPT_USE_SYSTEM_PREFERRED_RNG)`, computes exact SHA-256 over the 32 bytes, and sends
only that non-authoritative digest plus task/generation/correlation over
`HubEnrollmentProtocolV1`. The child alone reads and
closes the inherited anonymous-pipe handle. On connection the daemon binds the live
client PID/creation time/image/package/parent host PID to that pending enrollment, sends
a fresh 256-bit challenge, and accepts one request only when it contains the launch
value, nonce, correlation and request hash. It hashes with SHA-256, constant-time
compares, and treats the digest only as verifier after authenticated enrollment; the
digest alone grants no authority. It consumes
both pending launch enrollment and challenge before admission, and retains no reusable
secret. Every start failure, invalid child, timeout, cancel, disconnect, duplicate,
reconnect, shutdown and frontend exit cancels or consumes both ledgers and closes every
handle; ambiguity quarantines the seventh tool. The existing T02 Go seam already owns
`HostConfig.LaunchCapability`, enrollment, read-once pipe creation/delivery and bounded
cancellation; this design extends that owner with exact direct-image/profile validation
and constrains its existing Windows `AdditionalInheritedHandles` application to a
singleton capability handle after rejecting every conflicting `SysProcAttr` field.
The direct child is not `uvx`: the installed v0.9.21
binary and Astral's official tool documentation establish that `uvx` is `uv tool run`,
resolves/installs an isolated tool environment, and invokes its executable; neither
surface promises exact-process or inherited-handle preservation. The protected CST
manifest therefore uses `launch_profile: cst-direct-v1`; provisioning resolves its
immutable bundle record to the absolute pinned `mcphub-cst-runtime.exe` and supplies
that path in `HostConfig.Command` with fixed `--role=frontend`. PATH lookup, console-
script launchers and wrapper descendants are rejected. The non-secret decimal handle locator may use fixed
`MCPHUB_CST_LAUNCH_HANDLE`; capability bytes never enter environment.

### Minimal pre-revocation PE loader closure

`SR-C6-DLL-03` is fixed by one exact seam, not by a later self-check. The package owns
one x64 `mcphub-cst-runtime.exe` built by the pinned Microsoft Visual C++ (MSVC)
toolchain with `/Zl`, `/NODEFAULTLIB`, `/ENTRY:mcphub_cst_entry`,
`/SUBSYSTEM:WINDOWS`, `/DEPENDENTLOADFLAG:0x800`, `/DYNAMICBASE`,
`/HIGHENTROPYVA`, `/NXCOMPAT`, and `/CETCOMPAT`. The prelude is freestanding
C/assembler compiled without C++ exceptions, run-time type information, security-cookie
helpers, thread-safe static initialization, or compiler thread-local storage (TLS). It
links only `Kernel32.lib`; it neither links nor statically embeds a C/C++ runtime. `/GS`
is disabled only for this mechanically bounded prelude translation unit, which contains
no variable-size stack buffer and is independently disassembled; the post-revocation
code uses checked fixed-size arithmetic and the same no-runtime Win32 surface. Microsoft
documents that `/NODEFAULTLIB` removes implicit libraries and may require `/ENTRY`, that
a custom `/ENTRY` bypasses the default CRT entry which otherwise initializes runtime
state and C++ static constructors, and that an alternate entry must initialize the
security cookie before `/GS` functions; the selected prelude contains no such function
or implicit initialization
(<https://learn.microsoft.com/en-us/cpp/build/reference/nodefaultlib-ignore-libraries?view=msvc-170>,
<https://learn.microsoft.com/en-us/cpp/build/reference/entry-entry-point-symbol?view=msvc-170>,
<https://learn.microsoft.com/en-us/cpp/build/reference/gs-buffer-security-check?view=msvc-170>).

The executable admission contract is exact: machine AMD64; one entry RVA equal to
`mcphub_cst_entry`; normal import descriptors contain only case-insensitive
`KERNEL32.dll` and a generated allowlist of the exact prelude functions; delay-import
table absent; TLS data-directory RVA and size both zero; CLR and bound-import directories,
embedded manifest redirection, and writable/executable sections absent; relocations and
the declared mitigations present. No package DLL, CPython DLL, extension, CRT/UCRT/
VCRUNTIME DLL, application module, plug-in, policy-controlled module, or user-writable
module may be in the load-time graph. `KERNEL32.dll` is the only direct DLL import.
Windows may realize its own loader/forwarder closure before entry; that closure is
admitted only by a target probe which pauses at the entry RVA and proves every already
mapped non-executable module is an Authenticode-valid Microsoft image whose final
handle path is under target `%SystemRoot%\\System32`, and whose `{basename,
volume/file-id,sha256,signer}` row exactly equals the provisioned target-OS closure
manifest. Any extra, missing, substituted, package, or user module blocks registration.
Microsoft documents load-time DLL entry points during process initialization and PE TLS
callbacks, so an import/TLS declaration or observed pre-entry module outside this closure
is a security failure
(<https://learn.microsoft.com/en-us/windows/win32/dlls/dynamic-link-library-entry-point-function>,
<https://learn.microsoft.com/en-us/windows/win32/debug/pe-format>).

The provisioner opens every native bundle row without reparse traversal, verifies final
path plus volume/file identity, SHA-256 and offline Authenticode policy, and retains the
runtime image handle through launch admission. The frontend `exec.Cmd` owner and broker
worker owner both compare the selected executable to this receipt immediately before
creation; child self-verification is post-revocation and cannot replace parent admission.
The build emits a publication-safe reproducibility manifest containing repo-neutral
MSVC/SDK versions and hashes, compiler/linker flags, ordered object/library inputs,
source hashes, PE header/import/delay/TLS/load-config/section facts, output hash/signature
identity, and target-OS loader-closure schema. Two clean builds must be byte-identical
before signing and structurally identical after signing. An installed-workstation probe
found MSVC tools 14.51.36231 (`cl` 19.51.36252, `link`/`dumpbin` 14.51.36252) only through
the Visual Studio installation record, not ambient PATH; this proves availability, not
candidate conformance.

At the first executable instruction the custom entry uses only its admitted KERNEL32
import-address table. Frontend clears and reads back the four exact handles before
parsing or reading the locator. Worker clears and reads back its three standard handles
before emitting startup proof, then receives the fixed prelude, validates the two
directory capabilities, clears and reads back both, and emits `WorkerPreMainReceiptV1`.
Entry-RVA disassembly must show only ABI prologue and direct allowlisted IAT calls through
the last required readback; there is no indirect call, callback registration, heap/CRT/
static initializer, thread creation, DLL load, child creation, or package/user code. A
debugger/loader trace with hostile application-directory, current-directory, PATH and
user-directory DLL/TLS canaries must reach the revocation marker without loading or
executing a canary, and a descendant attempt after the marker must inherit none of the
four/five handles.

After all role-required flags are proved clear, one `NativePackageLoadOwner` acquires
`PackageLoadClosureV1` before any package DLL is mapped. Build/package tooling, using an
independent Portable Executable (PE) parser rather than the Windows loader or `dumpbin`,
starts at the exact CPython DLL and every admitted `.pyd`/package DLL, recursively walks
normal and delay-import tables, resolves package basenames only inside one package image,
and emits the closed ordered package set plus a separate target-operating-system
System32/API-set set. Static parsing is inventory, not the dynamic-load security boundary.
The portable manifest binds two disjoint exact-hash User Mode Code Integrity (UMCI) rule
classes: unscoped exact runtime-executable hash rows, and per-application exact-hash rows
for every admitted package and target System32 module whose sole `AppIDs` value is the
runtime's canonical `OriginalFilename`. The AppID string is a scope selector, not
cryptographic identity; exact runtime identity is the conjunction of its unscoped hashes,
signed bundle receipt, fixed `OriginalFilename`, and complete-policy proof that no other
admitted image can claim that AppID through a broader rule. Missing, extra, ambiguous,
unresolved, reordered or runtime-discovered modules fail packaging or startup. The manifest
binds schema, role, relative canonical name, PE machine, size, SHA-256, Authenticode signer,
normal/delay dependency edges, App Control rule identity, hash algorithm/page scope, AppID
scope, and package-versus-System32 class. Python module paths name only admitted extension/
stdlib/application rows. The two clean builds must reproduce the portable closure,
canonical policy XML, and unsigned CIP byte-for-byte.

P18 provisioning, while the frontend and both services are stopped and their process,
Job, mapped-module, virtual-disk and package handles are proved absent, constructs one
complete per-version VHDX, verifies it offline, and attaches it using
`ATTACH_VIRTUAL_DISK_FLAG_READ_ONLY|ATTACH_VIRTUAL_DISK_FLAG_NO_DRIVE_LETTER` beneath one
protected service-owned mount point. Microsoft defines the first flag as attaching the
virtual disk read-only; any target on which a write, stream creation or filesystem metadata
mutation succeeds is incompatible
(<https://learn.microsoft.com/en-us/windows/win32/api/virtdisk/ne-virtdisk-attach_virtual_disk_flag>,
<https://learn.microsoft.com/en-us/windows-server/administration/windows-commands/attach-vdisk>).
The backing VHDX, mount-point chain and attached volume are on one local fixed host volume,
SYSTEM-owned, non-reparse and protected by a DACL whose effective masks are read back.
Runtime principals have read/execute/synchronize only; update authority exists only in the
stopped provisioner, which detaches the old image before constructing or selecting a
complete reviewed successor. There is no in-place mounted-image update.

P18 contains one `CstAppControlPolicySetOwner`, the sole sampler policy writer and the
sole owner of the machine-policy compatibility decision. It never claims ownership of,
updates, converts, merges, removes, disables, or changes the deployment authority of an
ambient policy. Its only writable policy identity is one signed Multiple Policy Format
**supplemental** policy with a fixed sampler PolicyID and an explicit operator-selected
BasePolicyID. Microsoft documents that multiple base policies intersect, whereas a base
and its supplementals union; a new sampler base is therefore forbidden because its
machine-wide intersection can block unrelated applications, while the selected supplemental
is admitted only when the complete selected base family is already closed against broader
allows for this AppID
(<https://learn.microsoft.com/en-sg/windows/security/application-security/application-control/app-control-for-business/design/deploy-multiple-appcontrol-policies>,
<https://learn.microsoft.com/en-us/powershell/module/configci/set-cipolicyidinfo>).

The build-owned `ExactAppIdPolicyArtifactV1` is the sole source accepted by
`CstAppControlPolicySetOwner`; direct `New-CIPolicyRule -AppID -Level Hash` is forbidden.
Its authoring tuple pins Windows build, Windows PowerShell 5.1, ConfigCI assembly
file/product version plus SHA-256, `%windir%\schemas\CodeIntegrity\cipolicy.xsd` SHA-256,
converter grammar version, canonicalizer version, closure-manifest hash, fixed sampler
PolicyID/version, operator-selected BasePolicyID, runtime `OriginalFilename`, and policy
signer identities. The provisioning-only author invokes pinned ConfigCI solely as
`New-CIPolicyRule -DriverFilePath <held-row> -Level Hash` without `AppID`; for every UMCI
PE it requires exactly the documented Authenticode SHA-1/SHA-256 and page SHA-1/SHA-256
tuple, rejects kernel/duplicate/unknown rows, and compares those values with an independent
PE Authenticode-hash implementation. A separate version-resource parser obtains one ASCII
invariant-uppercase runtime `OriginalFilename`; it must equal both the signed bundle
manifest and the `AppIDs` value observed from a working pinned FileName-per-app oracle.

The author emits canonical XML directly under the pinned Microsoft XSD. Runtime executable
hash rows have no `AppIDs`; every package/target-System32 module hash row has exactly that
one runtime `AppIDs` value; deterministic IDs/order and exact PolicyID/BasePolicyID/version/
options/signer/update metadata come only from the closed input tuple. Microsoft documents
both per-app module rules and direct XML editing validated against this XSD
(<https://learn.microsoft.com/en-us/windows/security/application-security/application-control/app-control-for-business/design/use-appcontrol-policy-to-control-specific-plug-ins-add-ins-and-modules>,
<https://learn.microsoft.com/en-us/windows/security/application-security/application-control/app-control-for-business/deployment/audit-appcontrol-policies>).
Audit mode, deny-policy replacement, broad publisher/path/filename/default rules, inherited
`AllowAll`, managed-installer/reputation fallback, an unscoped module/Microsoft/System32
allow, macro AppID, or any attribute outside the allowlist is forbidden.

Before signing, a Go verifier owned by the P18 provisioner, sharing no XML writer,
canonicalizer, ConfigCI object model, or hash implementation with the author, validates the
canonical XML against the pinned XSD and independently reconstructs every semantic row.
Pinned `ConvertFrom-CIPolicy` converts that XML twice; both unsigned CIPs must be
byte-identical. The same verifier independently parses the pinned CIP binary grammar and
requires exact runtime-unscoped/module-AppID row classes, algorithms, hashes, cardinality,
references, PolicyID/BasePolicyID/version/options, and signer/update metadata equal to the
manifest and XML.

One offline operator ceremony, named `CstPolicySigningOwner`, is the sole private-key-use
boundary between that accepted unsigned CIP and the signed artifact handed back to P18.
There is no signing key, provider, provider credential, signer group, signer service, remote
signing endpoint, or signing-capable `SYSTEM` path on the target host. P18 exports one
content-addressed `CstPolicySigningRequestV1` containing only schema, request id, held
unsigned-CIP SHA-256, independently reconstructed semantic-manifest hash, closure/profile
hashes, PolicyID, BasePolicyID, forward-only version, exact certificate DER/SPKI/issuer/serial
pins and approved change-ticket hash. It imports no executable or private material from the
ceremony, only the signed CIP and `CstOperatorSigningReceiptV3`.

The ceremony runs on an isolated measured signing station with no inbound network service,
remote logon, target trust relationship, or unattended signing account. Its one admitted
device profile is an Entrust nShield 5s with exact pinned firmware at least 13.5 or nShield
5c with exact pinned firmware at least 13.6
and Security World Software 13.7.3. One non-exportable RSA-3072 content-signing key exists
only in that hardware security module (HSM), is protected by one non-persistent K-of-N
Operator Card Set (OCS), has signing-only/export-zero access control and `LogKeyUsage=yes`,
and is exposed to the pinned Windows Cryptography Next Generation key storage provider
(CNG KSP) only while the physical quorum is present. Its profile pins station boot
attestation, absolute Windows SDK `signtool.exe` path/hash, certificate DER/SPKI/issuer/
serial, exact provider/key object hash and attestation, OCS hash/quorum/non-persistence,
selected-base authorization, root SPKI and envelope grammar. A Windows discretionary access
control list is inventory only: it is never claimed to constrain `SYSTEM`. Successful use of
the selected key is the device-enforced quorum fact; neither the station nor the target
asserts individual operator identities or a distinct-approval count that nShield does not
export as authenticated per-operation evidence. Operators compare the displayed request
hash, semantic manifest, PolicyID/BasePolicyID, forward version and ticket hash to the
independent approval. PFX/PEM/import/export, persistent or remotely readable OCS, automatic
or subject-name selection, unattended credentials, reusable PIN/password material,
alternate KSP/key, and target-initiated online signing are forbidden.

#### OCS-loaded interval and final-card unload gate

`CstPolicySigningOwner` owns one station-global cross-process ceremony lock and the only
content-key state machine: `UNAVAILABLE -> PRELOAD_AUDIT_ANCHORED -> LOAD_AUTHORIZED ->
LOADED -> ONE_SIGN_TERMINAL -> FINAL_CARD_REMOVED -> KEY_UNAVAILABLE_PROVED ->
AUDIT_RANGE_SETTLED -> COMPLETE`. The profile admits one exact non-persistent,
non-remotely-readable OCS, one HSM module, and enumerated local smart-card slots; HSM pool
mode, dynamic/remote slots, `preload pause`, another module, or another provider/signing
process is incompatible. Before card insertion or CNG key open, the owner proves no earlier
ceremony, SignTool, provider/key handle, or pending journal is live and accepts a fresh
vendor-verified pre-load anchor `{ESN,logID,collector_generation,database_identity,
baseline_end_index,export_sha256}`. The lock remains held through post-unload audit settlement.

After the sole SignTool child reaches any terminal result, the owner closes and joins it and
closes every ceremony-owned CNG provider/key handle. Operators must then remove the final
required OCS card before audit export, artifact release, media handoff, retry, shutdown, or
process exit. Entrust Security World 13.7.3 specifies that a non-persistent OCS keeps keys
usable only while the last required card remains inserted and removes them from HSM memory
as soon as it is removed. The supported unload proof is the conjunction of that enrolled
non-persistent property, observed final-card removal, and a fresh pinned vendor
`slotinfo -m <exact-module>` readback: every admitted local slot reports `Token=-` and `IC=0`,
with no remote, dynamic, associated-dynamic, timeout, or failed-secure-channel state. A CNG
key-open result, wrapper flag, operator assertion, timeout expiry, or SignTool exit is not an
unload oracle. Target-profile admission additionally proves that, after this exact proof and
without reinsertion, a fresh exact-provider/key operation cannot sign without a new quorum;
this corroborating test is outside production ceremonies and never substitutes for card
absence.

Only after unload proof does the collector drain and pinned vendor exporter verify the one
continuous index range `baseline_end_index+1..post_unload_end_index`. Strict JSON admits
exactly one `Cmd_Sign`: its Transaction ID is byte-equal to the independently computed
request/profile value, status is `OK`, and its sibling `ObjectUse{key}` joins by exact
`{runID,objid}` to the pinned earlier `ObjectNew` content-key hash. Every duplicate/second
Sign, absent/different Transaction ID, additional ObjectUse of the exact key, or Sign not
provably bound to a different key rejects the range, including failed attempts. Matching-row
existence, a time window, or a wrapper counter is insufficient.

`CstOcsUnloadReceiptV1` is a required ceremony-local handoff gate, not deployment authority.
It contains only bounded public fields: schema/ceremony id; request/profile/content-key/OCS
hashes; quorum and `persistent=false`; ESN, module and local-slot ids; pre-load anchor;
SignTool image hash, process creation identity and terminal status; intended Transaction ID;
provider/key-handle close results; removal ordinal; pre/post `slotinfo` image/argv/exit/output
hashes and per-slot `{Token,IC,flags}`; post-unload audit end/export hash; total Sign count;
intended Sign index/status and ObjectUse `{runID,objid}`; exact-key sibling-use count zero;
ambiguous-Sign count zero; held input/output identities; and terminal cleanup/quarantine state.
Falsifiers are any present/nonzero/wrong/remote/dynamic slot, live signer/provider handle,
missing/gapped index, wrong/extra/ambiguous Sign or ObjectUse, transaction inequality, card
reinsertion, identity drift, incomplete cleanup, or receipt disagreement. It records no card
secret, passphrase, PIN, or claimed HSM-authenticated operator identity.

Every return after `PRELOAD_AUDIT_ANCHORED` executes the same terminalization. Cancel,
timeout, SignTool/provider/reader/collector failure, and restart still require process/handle
close, final-card removal, slot readback, and post-unload audit drain before lock release. A
stuck card, lost reader, unprovable unload, crash with unrecoverable held-output identity,
audit gap, extra key use, or ambiguous provider state writes durable
`OCS_INTERVAL_QUARANTINED`, destroys or retains the exact unaccepted output under held
identity, disables signing and sampler registration, and permits no retry or success receipt.
Restart is cleanup-only: it may finish removal/readback/audit settlement but cannot load,
sign, rotate, or hand off. HSM clear/isolation is separately approved incident response,
never an automatic fallback.

The sole ceremony invocation selects that exact certificate/key profile and uses the pinned equivalent of
`sign /sm /sha1 <public-thumbprint> /fd sha256 /p7 <protected-output-dir> /p7ce Embedded
/p7co 1.3.6.1.4.1.311.79.1 <held-unsigned-cip>`; it never uses `/a`, `/n`, `/f`, `/p`, `/t`
or `/tr`. Authentication stays inside the HSM. The single-threaded SignTool child receives
one non-secret `NFAST_TRANSACTION_ID` equal to base64url(SHA-512 of the complete canonical
`CstPolicySigningRequestV1` plus signing-profile hash); no other process shares that
environment. Firmware 13.5+ records that transaction id on the nCore `Sign` command. The
ceremony holds input/output handles, rechecks the input hash, and waits for the sole
`NShieldAuditCollectorOwner` below to settle a covering verified export before emitting
`CstOperatorSigningReceiptV3`. This receipt contains no alleged raw nCore segment, KML
warrant or independently parseable KML signature. It contains a deterministic projection
`{schema, request/signing-profile hashes, unsigned/signed-CIP hashes, certificate pins,
content-key object hash, OCS policy hash, collector-profile/config/exporter hashes,
database final identity and collector generation,
ESN, logID, startIndex, endIndex, JSON-export SHA-256, vendor-verify exit status,
Sign index/status/transaction id, ObjectUse runID/objid, ObjectNew key-hash tuple,
`CstOcsUnloadReceiptV1` hash and exact pre-load/post-unload audit anchors,
station/tool/profile hashes}`. The station also emits a separate
`CstCeremonySettlementV1` local cleanup record; it is a required ceremony-local handoff gate
but is neither KML-attested nor target deployment authority.

The supported verification boundary is deliberately asymmetric. Pinned Security World
Software 13.7.3 `nshieldaudit ncore export --esn <ESN> --logid <logID> --verify --file
<new-output> --start <previous-end+1> --end <candidate-end>` is the sole owner that re-verifies
KML signatures and their warrant chain. It writes full JSON, never `--text`, never
`--malformed`, never `--overwrite`, and never stdout. The ceremony wrapper does not interpret
an undocumented database or raw segment format. P18 does **not** re-verify KML cryptography;
it independently pins the exporter PE identity/version/hash, exact argv, closed environment,
collector/config/database identities, exit status, held output identity/SHA-256, strict
version-profiled JSON grammar, requested ESN/logID/range and continuous indices. It rejects
unknown fields where the pinned grammar is closed, truncated/trailing content, duplicate
keys, reordered or missing indices, any malformed/unverified/missing-index status, and any
projection disagreement.

Within that verified JSON, P18 requires exactly one `Command{cmd=Sign,status=OK}` in the
complete pre-load-anchor-through-post-unload range, carrying
the exact request/profile Transaction ID and its sibling `ObjectUse{objid,Sign}`. The key
identity is not read from `ObjectUse`: P18 joins its exact `{segment.header.runID,objid}` to
one earlier `ObjectNew{objid,type=Key,key-hash}` in the same continuous admitted logID
history and requires that key hash equal the pinned content-key object hash. Every other Sign,
every extra ObjectUse of this key, and every key-ambiguous Sign rejects the range even when its
Transaction ID differs or is absent. `objid` alone is
never stable across reboot. Because Entrust's supported export contract does not
cryptographically bind the signed CIP result bytes to the Sign record, vendor audit proves
only that the request-bound transaction used the exact content key successfully. P18
separately hashes the held signed CIP, requires that hash in request/receipt/prepared journal,
then independently verifies its PKCS #7 signature, embedded unsigned-CIP equality, signer,
chain and revocation. No claim is made that nShield audit attests result bytes, wrapper
projection, station settlement, individual approvers or approval count.

The device-protected KML audit signing key and audit index remain distinct from the
content-signing key. There is no general KML signing API and no wrapper-synthesized counter,
approval count, Windows event-log fact or SignTool stdout fact. P18 rejects missing,
vendor-unverified, malformed, gapped, reset, replayed, wrong-device/logID/key/request/profile
or ambiguous evidence before signature verification or any CiTool call. The receipt grants
no deployment authority. Ceremony media/storage contains only public request/policy/audit
artifacts and is wiped after verified handoff; no key bytes, OCS/PIN material, provider
credential or application secret enters argv, environment, stdin, pipe, target, request,
receipt, journal, log or temporary file.

The independent P18 signed-envelope verifier does not trust SignTool exit status, decode or
content extraction as authenticity. It requires one DER-minimal PKCS #7 `ContentInfo` with
outer `signedData` OID `1.2.840.113549.1.7.2`, embedded content-type OID
`1.3.6.1.4.1.311.79.1`, exactly one `SignerInfo`, exactly one embedded leaf certificate and
no CRL/countersigner/timestamp. The signer identifier must equal the pinned issuer-DER and
serial; leaf DER and SubjectPublicKeyInfo hashes must equal the profile; key algorithm is
RSA 3072; digest set and signer digest are exactly SHA-256 OID
`2.16.840.1.101.3.4.2.1`; signer signature algorithm is exactly `rsaEncryption` OID
`1.2.840.113549.1.1.1` with NULL parameters. Authenticated attributes are exactly one
`contentType` (`1.2.840.113549.1.9.3`) equal to the App Control policy-content OID and one
`messageDigest` (`1.2.840.113549.1.9.4`) equal to SHA-256 of the embedded CIP; signing-time
and every unauthenticated attribute are forbidden. Target provisioning must first reproduce
this exact pinned SignTool envelope grammar; any different encoding is incompatible until a
reviewed successor profile, never an automatic relaxation.

After final decode/update, the verifier first enables strong-signature checking and calls
`CryptMsgControl(CMSG_CTRL_VERIFY_SIGNATURE_EX)` for signer index zero with the exact pinned
certificate context; only a nonzero return is cryptographic success. It separately obtains
the embedded bytes and requires byte equality to the held unsigned CIP plus the same
independent semantic parse. It builds exactly one chain at current verification time to the
pinned root and requires `CertVerifyCertificateChainPolicy(CERT_CHAIN_POLICY_BASE)` to return
TRUE **and** `dwError==0`, BasicConstraints end-entity, digitalSignature key usage and exact
Code Signing EKU `1.3.6.1.5.5.7.3.3`. Exactly one revocation profile exists: `online-v1`.
The target verifier creates the pinned-root/exact-intermediate chain at current system time,
calls `CertGetCertificateChain` with `CERT_CHAIN_REVOCATION_CHECK_CHAIN_EXCLUDE_ROOT |
CERT_CHAIN_REVOCATION_ACCUMULATIVE_TIMEOUT | CERT_CHAIN_DISABLE_AUTH_ROOT_AUTO_UPDATE`, a
nonzero bounded `dwUrlRetrievalTimeout`, and no cache-only, AIA-disable, revocation-ignore,
weak-signature, timestamp-time, wall-clock-override or trust-fallback flag. It requires the
returned ordered certificate DER hashes to equal the profile, every trust/error status clean,
and `CertVerifyCertificateChainPolicy(CERT_CHAIN_POLICY_BASE)` TRUE with `dwError==0`.
Offline, unknown, stale, timeout, unreachable distribution URL, absent distribution data,
alternate chain, revoked certificate or ambiguity is `policy_revocation_unavailable` and
produces zero signing handoff and zero CiTool calls. `offline-v1`, protected revocation-
evidence directories and ambient-cache assertions do not exist and are rejected as unknown
profile values. The certificate is current-valid at ceremony, import verification,
deployment and reboot commit.

`CstPolicySigningOwner` serializes one ceremony journal keyed by `{request hash, unsigned CIP
SHA-256, signing-profile hash, certificate DER hash, provider/content-key hash, collector
profile hash, ESN, logID}`.
An identical completed request returns the same artifact/receipt; pending, conflicting,
replayed or ambiguous requests never sign automatically and require a new reviewed forward
version. Its export must be produced by the exact vendor `--verify` adapter over a continuous
range from the admitted pre-load audit baseline through proved final-card removal/key
unavailability and the post-unload end index, with exactly the intended request-bound Sign and
no sibling/ambiguous Sign or exact-key ObjectUse, and with no malformed/unverified segment,
missing audit index or `AbnormalShutdown` ambiguity. Cancellation/timeout rejects and wipes or
quarantines unverified output by held identity. On every return the ceremony closes signer and
exporter process/thread, HSM/provider/content-key, input/output and collector-query resources,
then proves final-card removal, all-slot absence and the continuous post-unload audit range
before it may release the lock; failure remains durably quarantined across restart. Target import closes
file/store/certificate/message/chain/virtual-disk contexts and refuses success if cleanup is
unproved. Diagnostics expose only stable ids and public hashes. Canary gates cover request,
receipt, removable media, argv/environment/stdin/stdout/stderr, logs, dumps, journals,
manifests, temporary paths and publication artifacts; any credential/private material outside
the HSM is a typed failure.

Rotation or revocation first disables registration and request export, stops all three
processes, settles target policy journals and the ceremony journal, removes the old request
profile and requires vendor-verified nCore evidence that the old content key is disabled or
destroyed. Content-key rotation does not rotate KML/logID. Audit-authority rotation is a
separate fail-closed operation: finalize/export/verify the old audit log without gaps, stop
all signing, preserve the terminal old `{ESN,logID,endIndex}`, reinitialize only under an
independently approved successor profile, pin the successor warrant/logID and a
vendor-verified genesis JSON export, and explicitly deny all future old-logID receipts. Target group/SYSTEM never
participates in either operation. It then admits a separately reviewed successor content key
and/or audit profile already authorized by the selected base. The same physical quorum signs a forward-versioned successor,
repeats independent signature/chain/revocation verification, CiTool update, reboot,
composition and unrelated-canary proof, and only then re-enables registration. Failure or
ambiguity latches `policy_artifact_unavailable` or `policy_set_unsettled`; old-key reuse,
weaker signer, same-version replacement, unsigned removal and automatic retry are forbidden.

Entrust documents that firmware 13.5+ nCore audit rows carry Transaction IDs; each audit
segment carries ESN, KML `logID`, runID and start/end audit indices and is signed by the
device-protected KML; indices increment per record, gaps and abnormal shutdown are observable;
and `{runID,objid}` uniquely identifies an object while `ObjectNew` supplies its key hash.
Security World Software 13.7.3 documents that `nshieldauditd` continuously retrieves/removes
logs into per-ESN databases; exactly one client service should fetch a given HSM, a network
5c collector must be a privileged client, and `nshieldaudit ncore export --verify` re-verifies
signatures while exporting JSON. It does not document that JSON as a raw signed-segment wire
format or expose a supported independent KML verifier, so this design does not claim either
(<https://nshielddocs.entrust.com/security-world-docs/v13.7.3/api-ncore/transaction-ids.html>,
<https://nshielddocs.entrust.com/security-world-docs/v13.7.3/hsm-user-guide/hsm-mgmt/audit-logging.html>,
<https://nshielddocs.entrust.com/security-world-docs/v13.7.3/hsm-user-guide/hsm-mgmt/nshield-audit-log-service.html>,
<https://nshielddocs.entrust.com/security-world-docs/v13.7.3/utilities/createocs.html>,
<https://nshielddocs.entrust.com/security-world-docs/v13.7.3/nshield-security-world-v13-7-3-management-guide.pdf>,
<https://nshielddocs.entrust.com/security-world-docs/utilities/slotinfo.html>).

Exactly one `NShieldAuditCollectorOwner` exists for the admitted HSM ESN. It is a
provisioning/ceremony-station service owner, not P18, the content signer or the HSM. Its
`NShieldCollectorProfileV1` pins the 5c ESN/logID, collector host and Windows service identity,
Security World Software 13.7.3 installer/binary hashes, absolute `nshieldauditd` and
`nshieldaudit` paths/hashes, explicit YAML `modules: [<exact-ESN>]`, absence of catch-all or
other ESNs, configuration-file and per-ESN database final paths/volume-file identities/
owner/DACL/hashes, effective absence of `NFAST_AUDIT_LOGDIR` and
`NFAST_AUDIT_SERVICE_CONF`, KNETI public hash, privileged-client enrollment attestation and
an HSM-estate inventory attesting that no other client service is enabled to fetch that ESN.
The profile contains no KNETI private material. Enrollment is an operator-reviewed 5c
administrative action; P18 only verifies the signed public profile/attestation hash and never
claims to rediscover global collector exclusivity from one host.

This owner alone configures, starts, stops, restarts, monitors, exports, backs up, restores
and rotates the database. Healthy means the exact service identity is running, its configured
ESN is detected, the per-ESN database is readable only by the admitted service/admin boundary,
`nshieldaudit ncore query --esn <ESN> --logid <logID>` reports the expected next continuous
index with `KML Status: Verification chain OK`, zero malformed/unverified segments, no missing
indexes and the expected finalization state, and source staging is not accumulating beyond the
admitted operational threshold. The query is health evidence; every receipt still requires a
fresh `export --verify`. A second collector, configuration/environment/database/binary drift,
split or missing indices, stale cached-only verification, absent privilege, non-OK health or
unproved service identity disables signing and emits `cst_saved_field.audit_collector_unavailable`.

For each request the owner serializes export with collection under one journal. It creates a
new protected request directory, derives one non-existing output pathname beneath a held
parent, invokes the exact JSON command above, waits for terminal exit, opens the output
no-follow, flushes and retains it, proves final-path/file identity/owner/DACL/default stream/
size/SHA-256, strictly parses the pinned JSON grammar and retains that handle through receipt
and media settlement. Concurrent export, stdout, text, malformed, overwrite, time filters,
ambient overrides and direct database parsing are forbidden. On cancel/timeout/crash it kills
and reaps the exporter, closes handles, deletes only the exact unaccepted output when identity
still matches, and leaves a durable failed/pending row that blocks another request until exact
reconciliation. Every return records and closes service-control, process/thread, parent/file,
database-query and media resources once; unproved cleanup keeps signing disabled.

Collector restart preserves the exact config/profile/database identity, first closes exports,
stops signing, stops then starts only the named service and requires the same continuity/health
probe before resuming. Configuration, host, KNETI or database rotation is a forward-only
profile change: stop signing and the collector, obtain a final vendor-verified range through
the old database end, fsync the terminal journal, take the vendor-documented consistent
stopped-service database backup with ownership/DACLs, prove the successor is the sole
privileged collector, then start it and accept only exact continuation for the same logID or a
separately reviewed genesis for a new logID. Automatic `serveradmin --delete`, `--force`,
`--delete-status`, `--recreate-db`, copying a live database, truncation, merge, dual running
collectors and rollback to an older profile are forbidden. Failure retains the old terminal
evidence, disables signing and requires human/vendor-supported recovery; it never fills a gap.

P18 contains one `AuditReceiptReplayOwner`, distinct from the content-signing ceremony and
from `CstAppControlPolicySetOwner`. It owns a write-through append-only journal keyed by
`{ESN, logID, collector-profile hash}`. Enrollment installs a reviewed 5c privileged-client
and sole-collector attestation plus a vendor-verified genesis JSON export and its exact
`endIndex`; an absent or self-selected first anchor is not accepted. Imports are serialized.
A new request must supply a continuous vendor-verified JSON range beginning at the journal's
next index and ending at a strictly greater `endIndex`, with
exactly one successful request-bound Sign row for the pinned content-key object. Before any
artifact admission the owner appends and flushes a `prepared` row containing previous and
candidate indices plus exact request/receipt/signed-CIP/staging-image hashes. A gap, duplicate
outside the same request, lower index, alternate run/log/device, malformed/unverified segment,
abnormal-shutdown ambiguity, counter reset or concurrent import fails closed. A prepared row
blocks every different import. After crash, only its exact byte-identical tuple may resume
settlement; if CiTool might have started, P18 quarantines and reconciles installed policy and
reboot state before deciding committed or rollback, and never calls CiTool again on ambiguity.
A completed exact replay returns the stored result with zero verifier mutation and zero
CiTool; equality for any different request/artifact/receipt is replay failure. `completed`
or terminal `failed` is appended and flushed only after the stated policy/staging settlement.

Microsoft documents PKCS #7, RSA 2048/3072/4096, SHA-256/384/512 and the App Control content
OID for signed policies; this design deliberately selects the single RSA-3072/SHA-256/no-
timestamp profile above. Microsoft also documents `CryptMsgControl` as the signature-
verification operation, `CertGetCertificateChain` as the revocation-check owner, and
`CertVerifyCertificateChainPolicy` as requiring inspection of `dwError`
(<https://learn.microsoft.com/en-us/windows/security/application-security/application-control/app-control-for-business/deployment/use-signed-policies-to-protect-appcontrol-against-tampering>,
<https://learn.microsoft.com/en-us/windows/win32/api/wincrypt/nf-wincrypt-cryptmsgcontrol>,
<https://learn.microsoft.com/en-us/windows/win32/api/wincrypt/nf-wincrypt-certgetcertificatechain>,
<https://learn.microsoft.com/en-us/windows/win32/api/wincrypt/nf-wincrypt-certverifycertificatechainpolicy>,
<https://learn.microsoft.com/en-us/windows/win32/seccng/key-storage-property-identifiers>,
<https://learn.microsoft.com/en-us/windows-hardware/manufacture/desktop/secure-boot-key-generation-and-signing-using-hsm--example?view=windows-10>).
Any unsupported pin, authoring/signing exception, empty/extra/substituted row or signer,
broad attribute, parser disagreement, conversion drift, non-identical clean build,
signature/content/chain/revocation mismatch or unproved close returns stable
`cst_saved_field.policy_artifact_unavailable`; no CIP is handed to deployment, the seventh
tool remains unregistered, and no alternate rule, signer, key, trust or authoring route is
selected.

Before any mutation, the stopped owner takes a protected machine-wide policy-lifecycle
mutex and an fsync/replace durable journal keyed by `{sampler PolicyID, policy version,
CIP SHA-256, BasePolicyID, pre-inventory digest}`. Under one lock it obtains elevated
`CiTool --list-policies --json`, resolves every active and staged base/supplemental policy,
its PolicyID/BasePolicyID/version/mode/signed/enforced/file identity and declared deployment
authority, and compares the complete set to an operator-approved inventory. The target must
be a disposable or explicitly reserved machine on which Group Policy, MDM/CSP, WMI, Windows
Update policy servicing and every other automatic redeployment authority are proved quiescent
for the complete sampler-availability lease; the owner cannot serialize an external writer,
so an externally managed sibling is incompatible rather than merely watched. Microsoft
platform policies are inventoried and boot-bound but never altered. Unknown, duplicate,
audit-only, externally managed, stale-file, single-policy-format, policy-limit,
pending-reboot, missing-source, or unowned-authority state is incompatible. Every active
supplemental of the selected base is parsed with the selected base; their union must have no
rule capable of admitting any unlisted image for the runtime AppID. Every other active base
is parsed separately; its intersection must admit every exact manifest image. The owner also
captures unrelated signed host-application canaries before mutation. A sibling or platform
policy may coexist only under that quiescent lease; any unexplained change of its normalized
policy hash, state, authority, or canary result before commit or before any process creation
revokes availability and keeps the sampler unregistered. This is a complete-set compatibility
gate, not permission to administer siblings. The selected base must already allow signed
supplementals and name the sampler supplemental signer; P18 does not add that authority.

One P18 `SignedPolicyImportOwner`, subordinate to `CstAppControlPolicySetOwner`, owns the
untrusted-media-to-CiTool handoff. It opens removable-media root/ancestors/leaf with
`OPEN_EXISTING|FILE_FLAG_OPEN_REPARSE_POINT`, rejects every reparse/hard-link/alternate-stream/
nonlocal/path-grammar case, holds those handles, and copies bytes only through the source
handle into a new single-artifact staging VHDX beneath a protected local fixed-volume root.
It verifies exact source and destination volume/file identity, link count, final path, owner,
effective DACL, default-stream-only enumeration, size, SHA-256, PKCS #7 signature and policy
semantics. It then flushes file and volume, detaches the writable staging image, retains the
protected backing-file and ancestor identities, and attaches that same VHDX with
`ATTACH_VIRTUAL_DISK_FLAG_READ_ONLY|ATTACH_VIRTUAL_DISK_FLAG_NO_DRIVE_LETTER`, never
`PERMANENT_LIFETIME`. On the attached volume it opens and retains no-follow read/synchronize
handles for volume, mount, every ancestor and the exact CIP leaf with `FILE_SHARE_READ` only,
repeats all identity/stream/hash/signature/semantic checks, and derives the CiTool pathname
only from `GetFinalPathNameByHandle` equality to that held leaf. Read-only attach, rather than
a claimed handle-input API, is the immutable path owner; Microsoft documents CiTool only as
`--update-policy <Path>`. The retained handles span journal prepare, exact child creation,
CiTool exit, immediate leaf/backing/volume/namespace/hash recheck and pending-state settlement.
Every return/cancel/timeout closes child/thread then leaf-to-root/volume/virtual-disk/source
handles in reverse order; failed close/detach/removal latches `policy_set_unsettled` and keeps
the staging image quarantined. Crash recovery reopens only the journaled backing identity,
reattaches read-only and revalidates before policy reconciliation. A completed/failed staging
image is removed only after services are stopped, no CiTool process/handle remains, journal
state is terminal, detachment and exact backing identity are proved. If target CiTool cannot
open the read-only mounted path while these locks are held, the target is incompatible: zero
CiTool side effect and no close/reopen, media-path, writable-directory or post-hoc fallback.
Microsoft documents both that `READ_ONLY` attaches a virtual disk read-only and that CiTool's
update contract accepts a pathname, not a file handle; Windows file sharing compares every
new access/share request against existing opens
(<https://learn.microsoft.com/en-us/windows/win32/api/virtdisk/ne-virtdisk-attach_virtual_disk_flag>,
<https://learn.microsoft.com/en-us/windows/security/application-security/application-control/app-control-for-business/operations/citool-commands>,
<https://learn.microsoft.com/en-us/windows/win32/fileio/creating-and-opening-files>).

The only deployment mechanism is elevated installed `CiTool.exe --update-policy` for the
exact signed supplemental CIP at that retained read-only staging path. `ConfigCI` is build/provisioning input only; direct copy,
Group Policy, Mobile Device Management (MDM), Configuration Service Provider (CSP), Windows
Management Instrumentation (WMI), and mixed-authority deployment are not alternate routes.
Every install/update is deliberately **reboot-committed** even where Windows offers a
rebootless path: `CiTool --refresh`, command success, file presence, or a callback is only
pending evidence. After an observed boot-identity change, the owner reacquires the mutex,
requires the journaled pending tuple, repeats the complete inventory/authority digest,
requires the exact supplemental as signed/enforced with the selected BasePolicyID/version,
re-hashes its deployed CIP, proves all exact manifest loads and direct/forwarded/indirect/
absolute/native malicious `DllMain` denials before first instruction, and proves every
unrelated canary retains its baseline result. Only then it emits the single downstream
`AppControlPolicySetCommittedV1` event and immutable receipt. Service startup, frontend
creation, worker creation, `ProvisionedPackageIdentityV1`, and sampler registration consume
that exact receipt; no route interprets policy state independently.

Update, removal, rollback, cancellation, timeout, and crash recovery are serialized by the
same owner and key. An identical committed request is a no-op returning the same receipt;
an identical pending request resumes observation; any conflicting request fails closed.
Removal first disables sampler registration and stops frontend/services, then uses only
`CiTool --remove-policy <sampler PolicyID>` and always reboots. After boot it requires the
sampler PolicyID absent from `CiTool`, absent from both operating-system and EFI active-policy
locations, the exact ambient sibling inventory restored, and unrelated canaries unchanged
before emitting `AppControlPolicySetAbsentV1`. Microsoft documents that signed supplemental
policies can be removed like unsigned policies, while policy removal and redeployment method
must be settled explicitly
(<https://learn.microsoft.com/en-us/windows/security/application-security/application-control/app-control-for-business/deployment/disable-appcontrol-policies>,
<https://learn.microsoft.com/en-us/windows/security/application-security/application-control/app-control-for-business/operations/citool-commands>).
Rollback to prior content is a forward-versioned, signed replacement with the same PolicyID
and BasePolicyID followed by the same reboot/commit proof; rollback to disabled is the exact
remove/reboot/absence sequence. A missing reboot, nonzero/ambiguous tool result, inventory or
authority drift, failed canary, failed absence, abandoned journal, or lock-owner death latches
`cst_saved_field.policy_set_unsettled`; no service restart, VHDX detach, package selection,
weaker policy, rebootless inference, or sampler registration is permitted.

The current-session installed-surface probe found `CiTool.exe` version `10.0.26100.1301`
with update/remove/list/refresh commands and ConfigCI module `1.0` with
`Set-CIPolicyIdInfo`, `ConvertFrom-CIPolicy`, `Get-CIPolicyIdInfo`,
`Set-CIPolicyVersion`, and `Set-RuleOption`. Its ConfigCI implementation assembly is
file/product version `10.0.26100.4484`, SHA-256
`6753B842637B26D37336E81111036D778B5884922BC944969D2C2FA70A49AFB2`; the installed
XSD is SHA-256 `A839C34EA7EE7EE5881DDA01485D7639290271FD456B5E45262C14BC644C7EEC`.
Installed implementation inspection proves the advertised direct AppID/hash route is
contradictory: `RuleLevel.Hash` is enum value 1, but `NewCIPolicyRule.ProcessRecord`
accepts per-app values only FileName (2), SignedVersion (3), FilePublisher (5), and
WHQLFilePublisher (11), while its exception text incorrectly lists Hash as allowed.
Read-only object probes confirm FileName-per-app derives `AppIDs=NOTEPAD.EXE` from the
application `OriginalFilename`, while ordinary Hash emits exactly the four UMCI Authenticode/
page SHA-1/SHA-256 rows documented by Microsoft. The XSD explicitly permits `Allow/@Hash`
with optional `Allow/@AppIDs`, and installed `ConvertFrom-CIPolicy` serializes both fields
into its binary records. These facts justify only the pinned transform-and-independent-parse
route above; they do not authorize the broken direct cmdlet path or an unverified ConfigCI
upgrade. The unelevated `CiTool --list-policies --json` probe returned access denied
(`OperationResult=-2147024891`), so binary availability is not policy-state evidence and
the provisioner fails closed unless every elevated target probe succeeds.

Provisioning emits `ProvisionedPackageIdentityV1`, bound to the exact
`PackageLoadClosureV1`, `AppControlPolicySetCommittedV1`, enforced supplemental and VHDX
SHA-256 values. It includes the canonical absolute mount root, backing-file/virtual-disk/
volume/mount-point identities, every row's volume/file ID, owner/protected-DACL receipt,
read-only-attach receipt, complete active-policy inventory digest, supplemental PolicyID/
BasePolicyID/version/hash/authority and commit boot identity. This host-specific receipt is
not compared across clean builds; reinstalling or selecting a reviewed image necessarily
creates and re-reviews a new host receipt without changing the portable content manifest.

At runtime the owner retains the open virtual-disk handle, attached-volume root and the
no-follow DELETE-denying handle chain from host volume root through every mutable backing-
file and mount-point ancestor, version root and package row. Directory handles request
`FILE_READ_ATTRIBUTES|SYNCHRONIZE`, `FILE_SHARE_READ` only and
`FILE_FLAG_BACKUP_SEMANTICS|FILE_FLAG_OPEN_REPARSE_POINT`; files request
`FILE_READ_DATA|FILE_READ_ATTRIBUTES|SYNCHRONIZE`, `FILE_SHARE_READ`, `OPEN_EXISTING` and
`FILE_FLAG_OPEN_REPARSE_POINT`. Each proves canonical final path, identity, owner/protected
DACL, local volume, link count one, no reparse, size/hash/signature/signer and manifest
cardinality. Sharing symmetry rejects a preexisting writer/delete handle and denies later
write/delete/rename/hard-link/reparse substitution on those objects
(<https://learn.microsoft.com/en-us/windows/win32/api/fileapi/nf-fileapi-createfilew>).
Because sharing modes are maintained per stream, the unnamed-stream handle is explicitly
not claimed to lock named streams. The owner instead enumerates all streams before and
after acquisition and every load/callback boundary and requires exactly unnamed
`::$DATA`; the read-only attached volume must make every named-stream create/write/rename/
delete probe fail. Detector unavailability or any successful namespace/stream mutation is
target incompatibility with no relaxed retry.

Only after the complete namespace, UMCI and handle receipts are admitted does the same PE
call `SetDefaultDllDirectories(LOAD_LIBRARY_SEARCH_SYSTEM32)` and exactly
`LoadLibraryExW(absolute_python_dll,NULL,
LOAD_LIBRARY_SEARCH_DLL_LOAD_DIR|LOAD_LIBRARY_SEARCH_SYSTEM32)`. The fully qualified root
is the retained CPython row's path on the read-only attached volume; application/current/
PATH/user/default/altered routes and unflagged `LoadLibrary*` remain forbidden. Windows
requires `hFile=NULL`, resolves this call by pathname and can execute TLS/`DllMain` before
return, so held handles are namespace/content continuity only; the enforced per-app UMCI
policy is the sole pre-execution image-admission boundary
(<https://learn.microsoft.com/en-us/windows/win32/api/libloaderapi/nf-libloaderapi-loadlibraryexw>,
<https://learn.microsoft.com/en-us/windows/win32/dlls/dynamic-link-library-entry-point-function>).

System32/API-set dependencies remain a distinct exact target manifest. Code Integrity must
deny every package or Microsoft image outside the exact per-app rules before first
instruction, including a runtime-computed load from TLS, `DllMain`, CPython, extension or
callback. After each admitted load, mapped inventory must equal the package closure plus
the target System32/API-set manifest, and each package mapping must equal its retained row.
That comparison is defense-in-depth and settlement evidence, never dynamic-load admission.

The owner resolves a generated exact CPython ABI symbol allowlist with `GetProcAddress`,
builds `PyConfig{isolated=1,site_import=0,user_site_directory=0,safe_path=1,
use_environment=0,parse_argv=0,module_search_paths_set=1}`, and permits imports only from
manifest-hashed stdlib/extension/application rows. Every package handle stays live through
`LoadLibraryExW`, dependency mapping and `DllMain`, `PyConfig`, application/session close,
`Py_FinalizeEx` where applicable, and mapped-module settlement; the long-lived frontend
retains them until its process teardown. Authenticode success means exact `WinVerifyTrust`
return zero, never generic HRESULT success
(<https://learn.microsoft.com/en-us/windows/win32/api/wintrust/nf-wintrust-winverifytrust>).

Every parent admission, custom-entry clear/readback, namespace/policy/closure acquisition,
module load/symbol/config/init, role dispatch, cancellation, timeout and shutdown return
has one reverse-order settlement: stop application work; finalize Python when initialized;
settle mapped modules; `FreeLibrary` only when safe before successful initialization;
close each retained package, directory, mount, volume, virtual-disk, module, capability and
root handle exactly once; zero secret/config buffers; emit no success; exit nonzero; and let
the existing frontend enrollment or broker Job owner terminalize/quarantine. The stopped
P18 provisioner alone may then verify zero service/process/Job/mapping/handle references,
    request the policy-set owner's serialized remove/reboot/absence lifecycle, then detach the
    read-only VHDX and select a complete reviewed successor. Detach is forbidden before
    `AppControlPolicySetAbsentV1`. A failed close, live mapping, policy-set/inventory drift,
    missing committed/absent event, detach ambiguity or namespace mismatch is
    `native_loader_settle_failed`; it leaves the read-only image attached, latches quarantine
    and blocks update/rollback. No return may restore inheritance, relax a
policy/share/search/DACL, reacquire/retry, start a child, initialize Python before revocation,
open source/vendor/CST state, select a wrapper, or fall back to another launcher.

Exact handle order: create anonymous pipe with inheritable read end and non-inheritable
write end; duplicate/mark only the child read end inheritable; CNG-fill 32-byte locked
buffer; enroll digest and receive bounded ACK; write exactly 32 bytes before start;
build `STARTUPINFOEX` with `PROC_THREAD_ATTRIBUTE_HANDLE_LIST` containing stdin,
stdout, stderr and capability read handle only, `bInheritHandles=TRUE`, and the fixed
non-secret handle-value locator in child environment; start the exact bootstrap image;
parent closes its read copy and write end. Native frontend bootstrap first clears and
reads back inheritance on stdin/stdout/stderr/capability, parses locator, reads exactly 32 bytes and requires EOF
with no 33rd byte, closes handle, then clears locator and zeroes buffers with
`SecureZeroMemory`; only then does it initialize embedded CPython and expose the secret
once as a mutable buffer to `cst.py`, which zeroes it after the daemon challenge.
Create/enroll/write/start/short/zero/overlong read, cancellation,
timeout, shutdown and exit close every created handle, zero every capability buffer,
and terminalize enrollment once.

Both service names are resolved at runtime with `LookupAccountNameW`; binary SIDs are
converted to canonical numeric strings before SDDL construction. Symbolic SID text is
forbidden. Each descriptor must compile, apply, and read back exactly by owner,
protected DACL, mandatory label, ordered ACE type/SID/mask/inheritance. The frontend
pipe admits only the restart-loaded policy-owner token plus pinned frontend PID/image/
package; the broker pipe admits only the current SCM daemon identity. Neither identity
check grants source authority.

P10-P17 may implement and verify these injected service/transport/receipt seams without
installing or mutating live Service Control Manager state. P18 alone may provision both
disposable services, resolve their real service SIDs, compile/apply/read back both pipe
descriptors, and prove current PID/token/session/image plus rollback/absence. Installed
CST behavior and architecture Claims 7 and 15 remain target-only and cannot be inferred.

Design one restart-independent Model Context Protocol (MCP) tool,
`cst_sample_saved_field`, as an additive CST-server capability. The tool accepts
an absolute `.cst` project anchor, an exact saved-field selector, and at most 256
physical points. It resolves one saved electric-field (E) or magnetic-field (H)
frame by metadata, copies the complete project bundle to a fresh local workspace,
activates only the copied field through the required `Result3D` -> CST-generated
header -> `ResultTree` -> `GetFieldVector` sequence, returns ordered complex point
values, settles every sampler-owned resource before success is observable, and
publishes exactly one bounded canonical JSON `TextContent` result.

The approved boundary is default-off and Windows-only. The daemon loads policy,
seals admission, and authenticates directly to the broker. Broker independently
authorizes that entry, opens the exact policy-derived source capability, creates the
protected workspace capability, and alone spawns a fresh no-console worker atomically
inside its kill-on-close Job. The worker receives only those two bounded kernel
capabilities through the broker-owned creation/request transaction, transfers the
authorized source directly into that workspace, and runs CST. One absolute
60.000-second invocation budget begins before the first admitted source operation
and covers broker authorization/worker launch, every source read/hash/copy, CST work, final hashing,
success encoding, protocol validation, and publication. Caller fields never grant
authority.

The source bundle is never opened by a mutating CST API and is never passed to a
write-capable component. Existing solve/export tools and their job-bound contracts
remain unchanged. The production tool does not select triangles, understand
Line10, compare finite-element-method (FEM) coefficients, launch an independent
export, interpolate, fit, rephase, rescale, solve, or remesh.

The cross-cutting owner is proposed in decision
`2026-08-12-cst-saved-field-authority-containment`. Candidate
`14a9b6b4cb9fc1e7248bd3b782b9e00d499181df` is a superseded evidence baseline,
not an implementation constraint.

## Accepted evidence baseline

- The accepted research memo is current at SHA-256
  `0EB32AB7A1D7E09B7835760308267DC0EEF5D9351302EC1879C6ED704E1FC22D`.
  Commit `2658ac85a0e1ee88b01f920af94c2664201e7a1c` is the accepted
  pre-candidate baseline, not current HEAD. Current HEAD is
  `14a9b6b4cb9fc1e7248bd3b782b9e00d499181df`, and its CST source contains a
  superseded fourth-tool candidate; all nine candidate surfaces named under
  migration are evidence only and are not implementation constraints
  (`architecture-review.md:111-119`).
- Existing exporters depend on an in-memory successful solve job
  (`servers/electromagnetics-mcp/src/mcphub_em_mcp/jobs.py:220`), whereas this
  capability must start from a retained bundle and survive server restart
  (`work-items/active/2026-08-11-cst-saved-field-sampler/research.md:54`).
- The existing solve path proves the project-anchor plus sibling-directory bundle
  shape but does not prove exclusive Design Environment ownership
  (`servers/electromagnetics-mcp/src/mcphub_em_mcp/cst.py:153`,
  `servers/electromagnetics-mcp/src/mcphub_em_mcp/cst.py:162`), but that path later
  saves and solves (`servers/electromagnetics-mcp/src/mcphub_em_mcp/cst.py:215`).
  The sampler may reuse only the copy mechanism. Its project-open seam must instead
  return an exact vendor-attributed sampler session/process identity and may not
  enter the solve runner.
- Reusable SHA-256 and publication-safe relative artifact primitives exist
  (`servers/electromagnetics-mcp/src/mcphub_em_mcp/provenance.py:12`,
  `servers/electromagnetics-mcp/src/mcphub_em_mcp/provenance.py:70`).
- The authoritative input/output, activation, source-integrity, process-ownership,
  zero-ambiguity, and Line10 acceptance requirements are
  `<vfem-repo>/docs/tooling/mcp-hfss-cst-requirements.md:44`,
  `<vfem-repo>/docs/tooling/mcp-hfss-cst-requirements.md:95`,
  `<vfem-repo>/docs/tooling/mcp-hfss-cst-requirements.md:119`, and
  `<vfem-repo>/docs/tooling/mcp-hfss-cst-requirements.md:143`.
- Vendor dynamic edges have not been executed in this architecture lane
  (`work-items/active/2026-08-11-cst-saved-field-sampler/research.md:57`). Any
  vendor-version interpretation below therefore fails closed until its named
  target-environment probe passes.
- Independent review verified that installed MCP 1.29.0 validates before tool
  entry, directly invokes synchronous functions, and otherwise serializes returned
  objects through its own result converter
  (`work-items/active/2026-08-11-cst-saved-field-sampler/architecture-review.md:118`,
  `work-items/active/2026-08-11-cst-saved-field-sampler/architecture-review.md:148`,
  `work-items/active/2026-08-11-cst-saved-field-sampler/architecture-review.md:176`).
- Security constraints are accepted at SHA-256
  `E3FF52C6F35D617BDA3E774838C4E88441C5195CBB32A11E113ECE33EF17715C`.
  Candidate-C local/fake evidence does not prove transport authority, network-path
  rejection, or resource budgets (`security-constraints.md:28-30`).
- The implementation architecture review is accepted at SHA-256
  `77AFFA4B7272794156EF1E72EDB23D4BD6928F490B2908692214C5CEF8D62FFF`.
  Its design findings are receipt loss, concrete-adapter dependency, and workspace
  initialization leakage (`implementation-architecture-review.md:90-121`).
- The existing hub authenticates a stable client/group `scopeKey` before dispatch
  (`internal/api/hub_mcp_handler.go:189-242`) and already applies a group-scoped
  visibility filter at `tools/list` and call revalidation
  (`internal/api/hub_mcp_resolver.go:75-96`,
  `internal/api/hub_mcp_aggregator.go:863-878`). The accepted owner decision states
  that this filter is not an access-control boundary because direct daemon ports
  and gate-off paths bypass it
  (`work-items/decisions/2026-06-18-groups-namespaces-tool-visibility.md`).
- The hub's 60-second `PerCallWallClockCap` bounds its wait only
  (`internal/api/hub_mcp_aggregator.go:128-138`,
  `internal/api/hub_mcp_aggregator.go:972-990`). It neither interrupts the installed
  synchronous FastMCP call nor proves vendor/workspace settlement.
- Installed MCP 1.29.0 constructs the published schema and validates arguments in
  `Tool.run` before calling the function; it wraps the raw exception text in
  `ToolError` (`mcp/server/fastmcp/tools/base.py:72-117`). A sampler-specific safe
  error map therefore belongs at the existing `strict_fastmcp` composition seam,
  not in the application function after entry.
- Microsoft documents that `PROC_THREAD_ATTRIBUTE_JOB_LIST` assigns listed jobs
  during child creation on Windows 10/Server 2016 and newer; this removes the
  create-then-assign execution race
  (<https://learn.microsoft.com/en-us/windows/desktop/api/processthreadsapi/nf-processthreadsapi-updateprocthreadattribute>).
- Microsoft documents that non-null `CreateProcessW.lpApplicationName` names the
  executable module directly, `STARTF_USESTDHANDLES` makes the three supplied
  standard handles authoritative, and `CancelSynchronousIo` targets synchronous I/O
  issued by an exact thread handle
  (<https://learn.microsoft.com/en-us/windows/win32/api/processthreadsapi/nf-processthreadsapi-createprocessw>,
  <https://learn.microsoft.com/en-us/windows/win32/api/processthreadsapi/nf-processthreadsapi-cancelsynchronousio>).
- Microsoft documents descendant job inheritance, breakaway limit behavior, and
  kill-on-last-handle-close semantics
  (<https://learn.microsoft.com/en-us/windows/win32/procthread/job-objects>).
  Completion-port messages are explicitly non-guaranteed, so direct job accounting
  is the settlement oracle
  (<https://learn.microsoft.com/en-us/windows/win32/api/winnt/ns-winnt-jobobject_associate_completion_port>,
  <https://learn.microsoft.com/en-us/windows/win32/api/jobapi2/nf-jobapi2-queryinformationjobobject>).
- A current installed-runtime probe on Windows 10 build 26100 / Python 3.14.6 found
  `ctypes` access to Create/Set/Query/Terminate Job Object, process-attribute-list,
  `CreateProcessW`, `IsProcessInJob`, wait, handle-close, `CancelSynchronousIo`,
  and `CancelIoEx` entry points. Microsoft requires handles in
  `PROC_THREAD_ATTRIBUTE_HANDLE_LIST` to be inheritable with
  `bInheritHandles=TRUE`, and `STARTF_USESTDHANDLES` for the three standard
  handles. No new package dependency is required. Target CST descendant behavior
  remains an empirical admission gate.
- Microsoft documents that an inherited handle preserves its numeric value and access
  and that the parent must communicate that value by interprocess communication; it
  also requires every `PROC_THREAD_ATTRIBUTE_HANDLE_LIST` member to be inheritable and
  `bInheritHandles=TRUE`
  (<https://learn.microsoft.com/en-us/windows/win32/procthread/inheritance>,
  <https://learn.microsoft.com/en-us/windows/win32/api/processthreadsapi/nf-processthreadsapi-updateprocthreadattribute>).
  `WorkerPreMainBootstrapV1` therefore transports only the two numeric locators over
  the already-owned stdin channel after native startup proof and before CPython
  initialization, never through argv or environment, while the kernel objects and
  granted access originate solely at broker creation.
- Microsoft documents that `SetHandleInformation(handle,HANDLE_FLAG_INHERIT,0)` clears
  transitive inheritance, `GetHandleInformation` reads the effective flag, and
  `CreateFileW.dwShareMode` remains effective until handle close
  (<https://learn.microsoft.com/en-us/windows/win32/api/handleapi/nf-handleapi-sethandleinformation>,
  <https://learn.microsoft.com/en-us/windows/win32/api/handleapi/nf-handleapi-gethandleinformation>,
  <https://learn.microsoft.com/en-us/windows/win32/api/fileapi/nf-fileapi-createfilew>).
  The broker therefore owns one exclusive inheritance epoch, and the worker proves all
  five inherited handles non-inheritable before any descendant can be created.
- CPython documents that normal startup imports `site`, executable `.pth` lines run at
  every startup, `sitecustomize` may execute arbitrary customization, and `-I` does not
  imply `-S` (<https://docs.python.org/3.13/library/site.html>,
  <https://docs.python.org/3.13/using/cmdline.html>). Therefore no ordinary
  `python.exe -I -m ...` process may receive an inheritable authority handle. The native
  bootstrap clears/readbacks first, then uses `PyConfig` with `isolated=1`,
  `site_import=0`, `user_site_directory=0`, `safe_path=1`, `use_environment=0`,
  `parse_argv=0`, and `module_search_paths_set=1` containing only package-manifest-
  hashed stdlib/extension/application roots beside the pinned bundle. Missing or extra
  `sys.path`, `.pth`, `sitecustomize`, `usercustomize`, `PYTHON*`, registry path, current
  directory or user/global site influence fails startup before Python entry.
- Architecture review SHA-256
  `B480EC9C7C06C64508AB45C87F080B84686DE45178F3899067197C8EDD8B680E`
  reported blocking `AR-C6-APPID-09`: installed ConfigCI 1.0 advertises AppID plus Hash
  but emits no rule. Current installed-surface inspection resolves the mechanism: the
  implementation's per-app level guard excludes enum `Hash=1`, despite its resource text.
  Microsoft documents `-AppID` for per-app module control, Authenticode/page hash
  cardinality, and direct XML editing checked against
  `%windir%\schemas\CodeIntegrity\cipolicy.xsd`; the pinned XSD permits `Allow/@Hash`
  and `Allow/@AppIDs`. A versioned,
  deterministic transform plus independently parsed XML and CIP is therefore the only
  admitted correction; an upgrade, broader rule, or empty-rule fallback remains forbidden.
- The independent architecture review at SHA-256
  `03418AC94820DD25D3A639D9801910FF1003510B5171AD86B3A02F972E55C8A7`
  gives 32 verified claims, target-only verdicts for Claims 7 and 15, zero failed
  claims, and one governance finding, AR-CONT-07: decision-metadata promotion and
  later target admission were conflated (`architecture-review.md:142-151`). The
  independent security review at SHA-256
  `6D07A604828B14E2C0A0F27D709D2FA9B765A3651AD855AD1A7A9B42B2D0DF50`
  leaves only SR-02-R1: the shared pre-filesystem reserved-device predicate omitted
  Microsoft's superscript COM/LPT aliases (`security-review.md:80-115`).
- Microsoft documents Windows reserved names and normalization aliases, named file
  streams, file identity/link-count metadata, normalized final paths, directory
  entry long/short-name metadata, and stream enumeration
  (<https://learn.microsoft.com/en-us/windows/win32/fileio/naming-a-file>,
  <https://learn.microsoft.com/en-us/windows/win32/fileio/file-streams>,
  <https://learn.microsoft.com/en-us/windows/win32/api/fileapi/nf-fileapi-getfileinformationbyhandle>,
  <https://learn.microsoft.com/en-us/windows/win32/api/fileapi/nf-fileapi-getfinalpathnamebyhandlew>,
  <https://learn.microsoft.com/en-us/windows/win32/api/winbase/ns-winbase-file_id_both_dir_info>,
  <https://learn.microsoft.com/en-us/windows/win32/api/fileapi/nf-fileapi-findfirststreamw>).
  A current installed Windows 10 build 26100 / Python 3.14.6 symbol probe confirms
  `CreateFileW`, `GetFinalPathNameByHandleW`, `GetFileInformationByHandle`,
  `GetFileInformationByHandleEx`, `FindFirstStreamW`, `FindNextStreamW`,
  `GetVolumeInformationByHandleW`, and `SetFileInformationByHandle`; no dependency
  change is required. The naming documentation explicitly treats superscript
  digits `¹`, `²`, and `³` as digits in the reserved names `COM¹`..`COM³` and
  `LPT¹`..`LPT³`, including when an extension follows the reserved stem.

## Change-Surface Contract

| Named field | Contract |
|---|---|
| **Intended change surface** | `servers/electromagnetics-mcp/src/mcphub_em_mcp/cst.py`: preserve six registrations/call paths; add only the seventh registration, inherited launch-capability intake, `WindowsDaemonClient`, and new-tool-only MCP publisher. |
|  | New `cst_saved_field_frontend_protocol.py` and `cst_saved_field_daemon_client_windows.py`: frontend challenge/request/result/cancel, launch-capability binding, `FrontendChallengeLedger`, separate `DaemonResponseReceiptV1` and `FrontendTransportReceiptV1`, correlation/framing and fail-closed transport. |
|  | New `cst_saved_field_hub_enrollment_windows.py`: daemon-owned `HubEnrollmentProtocolV1` listener, current-supervisor/current-CST-host authentication, bounded pending digest ledger and receipts. |
|  | New `cst_saved_field_daemon_service_windows.py`: SCM entrypoint and sole `SamplerAdmissionGate`, policy resolution, original QPC deadline, broker client, nested receipt validation and quarantine owner. |
|  | New `cst_saved_field_broker_service_windows.py`: SCM broker entrypoint, independent policy, authenticated pipe, source/workspace capability creation, worker/containment composition, final root deletion and settlement. |
|  | Modify, do not parallelize, the existing T02 Go owner: `internal/daemon/host.go` (`HostConfig.LaunchCapability`, `StdioHost.Start`, direct `exec.CommandContext`, three `*Pipe` standard handles); `internal/daemon/launch_capability.go` (`LaunchCapabilityConfig`, `prepareLaunchCapability`, `preparedLaunchCapability.apply/start/cancel/close`, `handleInventory`); `internal/daemon/launch_capability_windows.go` (`productionLaunchCapabilityOps`, `windowsLaunchCapabilityPipe.apply/close/writeAndClose/locator`); `internal/daemon/launch_capability_other.go` fail-closed/default-off platform behavior; `internal/cli/daemon.go::cstLaunchCapabilityConfig`; existing `internal/api` enrollment/supervisor-status owners. Preserve `exec.Cmd`/installed Go process creation. Strengthen `windowsLaunchCapabilityPipe.apply` to accept only an otherwise empty compatible `SysProcAttr`, set exactly one `AdditionalInheritedHandles` capability value, and reject all conflicting inheritance/process-identity/security fields; verify exact `cst-direct-v1` image before start. Delete no T02 owner file; delete any implementation-introduced duplicate raw-process launch owner/helper and reject compatibility aliases. Repo evidence: current `HostConfig.LaunchCapability` is `internal/daemon/host.go:46-50`; command and standard pipes are `internal/daemon/host.go:283-333`; lifecycle/schema are `internal/daemon/launch_capability.go:17-140`; Windows singleton addition is `internal/daemon/launch_capability_windows.go:103-119`; installed Go 1.26.5 produces HANDLE_LIST at `$GOROOT/src/syscall/exec_windows.go:390-424`; CLI composition is `internal/cli/daemon.go::cstLaunchCapabilityConfig`. |
|  | New package-owned native `mcphub-cst-runtime.exe` plus immutable embedded CPython DLL/stdlib/extension/application manifest; two fixed roles implement pre-Python frontend/worker handle revocation and then call existing `cst.py` or worker entry. Build/release signs and hashes every bundle row. |
|  | New provisioning-only `servers/electromagnetics-mcp/policy/windows/author_exact_appid_policy.ps1`: under pinned Windows PowerShell/ConfigCI/XSD identities, request only unscoped Hash rule objects, independently verify their PE hashes and runtime `OriginalFilename`, and emit canonical `ExactAppIdPolicyArtifactV1` XML plus unsigned CIP. It never deploys or reads live policy. |
|  | P18 `internal/cli` gains a Windows-only XML/CIP verifier and content-addressed request/export plus signed-result/receipt import. The offline operator ceremony is the sole `CstPolicySigningOwner`; the target has no signing key/provider/credential/group/service/endpoint. P18 verifies the ceremony audit-attestation receipt, signed envelope, exact online-only chain/revocation profile and artifact semantics before the existing `CstAppControlPolicySetOwner`; no runtime sampler module authors or signs policy. |
|  | `servers/cst/manifest.yaml`, manifest schema/materialization and CST provisioning: replace `command: uvx`/`--from ...` with protected `launch_profile: cst-direct-v1`; installer records absolute canonical bootstrap/package paths and SHA-256 identities in the supervisor descriptor; StdioHost rejects PATH/wrapper launch for capability mode. Transport, port, bindings and daemon shape remain unchanged. |
|  | New `cst_saved_field_policy.py`: typed `v1` policy, owner/access/locality validation, shared closed Windows path grammar, complete canonical manifest-v2 identity, and immutable process snapshot. |
|  | New `cst_saved_field_broker_protocol.py`: daemon↔broker challenge/request/response/settlement; new `cst_saved_field_broker_worker_protocol.py`: native `WorkerPreMainBootstrapV1`/`WorkerPreMainReceiptV1`, later broker↔worker application request/result, and owner-local capability receipts. |
|  | New `cst_saved_field_containment_windows.py`: Win32 Job Object, exact atomic five-handle process-creation tuple, absolute deadline/watchdog, bounded cancellable streams, accounting, termination, capability receipts, settlement, and handle cleanup. |
|  | New `cst_saved_field_broker_worker.py`: broker-only fixed entry point proving startup before stdin, reconstructing two bounded inherited capabilities, and performing every handle-relative source open/read/hash/copy, application/vendor work, settlement, capability close and final encoding. |
|  | New `cst_saved_field.py`: own request/result models, selection, units, source integrity, budgets, settlement aggregate, and post-entry IDs. |
|  | New neutral `cst_saved_field_port.py`: own vendor protocols plus acquisition/failure/batch/receipt values imported by both core and adapter. |
|  | New `cst_saved_field_vendor.py`: depend inward on the neutral port and own transactional acquisition, record validation, and fixed activation/sampling while borrowing one required `AuthorizedVendorPathLease`; it never accepts ordinary workspace paths or performs same-principal write handoff. |
|  | New `cst_saved_field_vendor_isolation_windows.py`: own the fixed credential-free Windows virtual service account `NT SERVICE\McpLocalHubCstVendorBroker`, fixed single-instance local named pipe, protected vendor workspace, fresh per-invocation CST worker, exact token/process/Job handles, and complete teardown. This boundary is mandatory for any path-only CST write; unavailable/identity-ambiguous isolation means no registration. |
|  | `safety.py`: additive typed trusted-root, canonical Windows file-object proof, complete handle-stable source-to-workspace transfer, role-specific read-only vendor leases that omit write/delete sharing, no-follow access, and transactional workspace factories; ambient config remains composition-owned. |
|  | `strict_fastmcp.py`: additive sampler-only safe-validation-error policy; empty policy preserves every existing tool error channel. |
|  | Focused new sampler tests plus minimal inventory updates in `tests/test_servers.py` and `tests/test_stdio.py`; CST README/API documentation. |
|  | `servers/electromagnetics-mcp/README.md`, focused tests, and `servers/cst/manifest.yaml` immutable pin/description may change only after all gates. |
| **Approved extension seams** | Existing `@mcp.tool()` registration in `cst.py`; seventh-tool-only daemon client and result publisher. Existing six injected `_cst_module`/`_cst_version` behavior is unchanged. |
|  | `CstPolicySigningRequestV1` and `CstOperatorSigningReceiptV3` are the only cross-ceremony seam; `ExactAppIdPolicyArtifactV1` is the only P18 verification seam and the read-only staged identity is the only deployment input. Request/vendor-audit/replay/signing/online-revocation/staging or independent JSON/XML/CIP/signature/content disagreement emits `policy_artifact_unavailable` and leaves registration inert. Receipts contain public vendor-verified projection and artifact hashes only and grant no deployment authority. |
|  | SCM daemon `SamplerAdmissionGate` is the sole writer of availability/revision/generation/active/waiter state; every transition emits downstream-observable `cst_saved_field.admission_settled`. Frontend has no admission latch. |
|  | One optional `MCPHUB_EM_CST_SAVED_FIELD_POLICY` absolute path is restart-loaded independently by frontend, SCM daemon and broker through the same validated policy contract. Frontend uses only enabled/entry inventory/endpoint proof; daemon resolves `entry_id`; broker reauthorizes exact revision/entry/manifest. Absent/invalid/disabled at any owner means no seventh-tool registration/readiness. |
|  | `strict_fastmcp` may map only this tool's pre-entry `ToolError` to one fixed safe validation error; all existing names retain current behavior. |
|  | A transactional vendor-port `open_owned_sampler_session(copy)` contract that owns every factory-local resource until one complete `OwnedSamplerSession` normal-return transfer. |
|  | It rolls back every exact factory-local handle on earlier failure. `_open_owned_project` is explicitly not an approved sampler seam. |
|  | One neutral `AuthorizedVendorPathLease` protocol obtained only from the isolated vendor workspace snapshot; read-only inputs use `GENERIC_READ` plus `FILE_SHARE_READ` only, while write-capable project/header roles never cross into the daemon principal. It remains owned by `SamplerSession` until cache/session settlement. |
|  | A frontend MCP result publisher validates the daemon result plus `FrontendTransportReceiptV1` and emits its JSON `TextContent`; only this tool's exposed validation text is replaced by the fixed safe error. |
|  | Standard-library `ctypes` over documented Win32 APIs, `provenance.sha256_file`, neutral test-faked ports, and platform-specific no-follow/access-control probes. |
| **Protected / must-not-touch surfaces** | Signatures, schemas, behavior, errors and Python application paths of all existing six tools; their process launcher changes only from uvx wrapper to direct pinned embedded-Python bootstrap and must pass byte/stdio compatibility. `jobs.py`; `cst_results.py`; HFSS source/tests/manifest. |
|  | CST solve history/calls, mesh and 1D exports, published artifact schemas, retained bundles, pre-existing processes, and unrelated dirty worktree. |
|  | Hub routing/filter semantics and CST transport/port/client bindings/daemon shape remain protected. The CST command contract is explicitly migrated to `cst-direct-v1`; no other manifest command changes. Go change includes manifest schema/materialization/provisioning plus CST-only StdioHost direct-image enforcement, enrollment/status surfaces; unrelated process containment is untouched. |
| **Declared blast radius** | One default-off CST tool/schema/docs/tests; enrollment/frontend/daemon/broker protocols and service entrypoints; one offline policy-author leaf, P18 request/export plus receipt/import verifier, and one isolated operator HSM ceremony; no target signing authority or application secret; policy/worker/containment leaves; two fixed SCM identities, three fixed local pipes, one hub-spawn capability, one admission gate; per call, one enrollment, one frontend exchange and one broker request, protected workspace/fresh worker, manifest transfer, lease, absolute deadline, bounded streams, Job, and vendor tree. |
|  | Shared `strict_fastmcp.py` gains a default-inert per-tool error policy; existing six error contracts are guarded byte-for-byte. |
|  | No third-party package-library dependency, cross-call CST worker/cache/session, solver action, HFSS change, route/filter change, or existing response-field change. The signed native runtime bundle, protected CST launch-profile migration, two services and hub launch-capability seam are explicit runtime dependencies without fallback. |
|  | Operator policy, daemon admission/quarantine, frontend and broker nonce ledgers, pending one-use launch enrollment, and per-invocation broker-worker state are the only new persisted/mutable/process surfaces. Launch enrollment is memory-only and restart-scoped. |

`create_workspace_lease` owns the disposable child from its creation until one
complete normal-return lease transfer to `SamplerSession`; on every earlier exit it
returns a complete rollback receipt. Independently,
`open_owned_sampler_session` exclusively owns every vendor-created handle/process
resource during acquisition; its normal-return transfer moves that vendor-resource
ownership to `SamplerSession` with no overlap.
The application owner merges cleanup receipts into worker-local
`cst_saved_field.session_settled`; worker success cannot reach broker until it
records complete authorized-copy equality, monitored source hashes unchanged,
workspace-content settlement, inherited capability closure, exact session closure, and
the attributed owned process absent. Broker separately owns root deletion.
Broker containment merges it with broker-owned Job/stream/exact-five-handle evidence
into `BrokerResponseV1`;
broker transport adds `BrokerExchangeReceiptV1` only after terminal channel events.
Daemon merges admission and broker receipts into `FrontendDaemonResultV1`, then its
daemon server adds `DaemonResponseReceiptV1`; frontend client independently adds
`FrontendTransportReceiptV1`. Frontend alone publishes after its local conjunction; daemon alone emits
`cst_saved_field.request_settled` and releases or quarantines admission exactly once.

## Components and dependency direction

| Component | Single responsibility and owned invariant | Dependencies |
|---|---|---|
| CST stdio frontend (`cst.py`) | Preserve existing-six composition and local calls. For the seventh tool only: load enabled policy inventory, accept read-once inherited launch capability, construct `WindowsDaemonClient`, send non-authoritative `entry_id` plus request, validate daemon result with `FrontendTransportReceiptV1`, and publish one `TextContent`. It owns no admission, QPC, broker client, source or worker. | Depends on frontend protocol/client, policy inventory, neutral wire contract and `strict_fastmcp`; never broker protocol/containment/vendor/worker. |
| Frontend protocol/client (`cst_saved_field_frontend_protocol.py`, `cst_saved_field_daemon_client_windows.py`) | Own challenge, pending launch capability proof, correlation/request hash, `FrontendDaemonRequestV1`, result/cancel framing, `FrontendChallengeLedger` and client-observed `FrontendTransportReceiptV1`. | Imported only by frontend and SCM daemon; no source locator or policy authority. |
| Hub enrollment protocol (`internal/api/hub_enrollment_client_windows.go`, `cst_saved_field_hub_enrollment_windows.py`) | Own third fixed local channel, bounded enroll/cancel/receipt frames, authenticated current-supervisor/current-host proof and pending digest terminal ledger. | Go spawn owner is client; SCM daemon is server. It queries supervisor IPC independently; digest is non-authoritative. |
| Existing T02 hub spawn owner (`HostConfig.LaunchCapability`, `StdioHost.Start`, `prepareLaunchCapability`, `preparedLaunchCapability`, `windowsLaunchCapabilityPipe`, `cstLaunchCapabilityConfig`) | Materialize protected `cst-direct-v1`, verify immutable bundle/image identity before start, CNG-generate one launch capability, enroll its digest, and let the existing `exec.Cmd` owner create the exact native frontend. `windowsLaunchCapabilityPipe.apply` requires an otherwise empty compatible `SysProcAttr`, sets the singleton capability `AdditionalInheritedHandles`, and rejects any other inherited handle, token, parent-process or process/thread security attributes. Installed Go combines the three exact standard pipe handles plus that singleton into the ordered four-handle HANDLE_LIST. | CST transport/port/client bindings remain unchanged; the manifest command is intentionally migrated and environment contains only the non-secret locator. No raw frontend process creator, second config/prepared state/Windows file/cancellation ledger, or unverified inheritance field exists. |
| Offline exact-AppID policy author (`policy/windows/author_exact_appid_policy.ps1`) | Consume only pinned bundle/closure/base/signer inputs; obtain ordinary ConfigCI Hash tuples without AppID, independently verify PE hashes and runtime `OriginalFilename`, and emit canonical XSD-valid XML plus unsigned CIP twice. It has no deployment, live inventory, service, or runtime import surface. | Depends only on pinned Windows PowerShell 5.1, exact ConfigCI/XSD identities, held package rows, and the build manifest. The broken direct Hash-plus-AppID cmdlet path and every alternate rule level are forbidden. |
| Offline operator `CstPolicySigningOwner` | On an isolated measured station, accept one exact `CstPolicySigningRequestV1`; use only the nShield non-exportable content key after non-persistent OCS K-of-N approval; emit the held signed CIP plus `CstOperatorSigningReceiptV3` only after the collector owner returns one request-bound vendor-verified JSON export. | Depends on pinned nShield 5s firmware >=13.5 or 5c firmware >=13.6, Security World Software 13.7.3, SDK SignTool, exact CNG KSP/content-key/OCS and collector profiles and physical operator approval. It exposes no listener/service/credential/key to target Windows. |
| `NShieldAuditCollectorOwner` (one exact station/service per HSM ESN) | Solely own 5c privileged-client enrollment, explicit-ESN `nshieldauditd` config, per-ESN database, service health/exclusivity, continuous collection and serialized held `nshieldaudit ncore export --verify` JSON. Vendor utility owns KML/warrant verification; collector owns lifecycle and output identity, never content signing or deployment. | Depends on pinned 13.7.3 service/exporter binaries, exact 5c ESN/KNETI public identity and operator-attested estate-wide absence of another collector. Split collection, cached-only export or database/config drift disables signing. |
| P18 `AuditReceiptReplayOwner` (`internal/cli`, Windows-only) | Own the sole durable append-only per-ESN/logID/collector-profile replay anchor and serialized prepared/completed/failed transitions. Strictly greater continuous new ranges advance it; exact completed equality returns stored result; every other equality/gap/reset/concurrency/crash ambiguity is fail-closed. | Consumes held vendor-verified JSON plus the enrolled collector/genesis attestation. It independently validates projection, indices and `{runID,objid}` ObjectNew-to-Sign/ObjectUse binding but does not re-verify KML cryptography, sign, deploy or select audit identity from the receipt. |
| P18 `SignedPolicyImportOwner` (`internal/cli`, Windows-only) | Copy from held no-follow untrusted-media handles into a protected single-artifact VHDX, verify exact bytes/identity/signature/streams, reattach read-only, retain backing/volume/mount/ancestor/leaf identities through path-based CiTool and settlement, then clean up only from terminal state. | Depends on documented Virtual Disk and file-identity/share APIs. CiTool remains a pathname consumer; failure to coexist with the retained read-only path makes the target incompatible and has no reopen fallback. |
| P18 independent policy-artifact verifier (`internal/cli`, Windows-only) | Independently verify collector/export/request/replay state and strict documented JSON projection, parse XSD-valid XML, unsigned CIP binary records, and the signed-CIP envelope; cryptographically verify one pinned signer, exact OIDs/algorithms/attributes/content, current certificate/chain/revocation profile, embedded-CIP byte equality and the held signed-CIP hash before staging. | Depends on standard Go, the version-pinned documented nShield JSON projection, Windows CryptoAPI and version-pinned CIP/envelope grammars; shares no author/signing code, object model, canonicalizer, or hash implementation and never parses the vendor audit database or KML wire format. Produces typed `ExactAppIdPolicyArtifactV1` or `policy_artifact_unavailable`. |
| SCM daemon composition (`cst_saved_field_daemon_service_windows.py`) | Independently load policy, authenticate exact enrolled frontend child, own enrollment/frontend ledgers, resolve exact `entry_id`, own `SamplerAdmissionGate`, create unchanged QPC triple, construct real broker client, validate broker receipts, construct daemon result and quarantine/release only from daemon-local `DaemonResponseReceiptV1`. | Depends on enrollment/frontend protocol servers, policy, broker protocol/client and QPC; never source/safety/containment/vendor/worker and never waits for `FrontendTransportReceiptV1`. |
| Saved-field policy owner (`cst_saved_field_policy.py`) | Validate owner-restricted local policy and declared roots; own immutable unique `entry_id` descriptors and process revision for each frontend/daemon/broker snapshot. | No source access at frontend/daemon call admission; no caller/hub authority. |
| SCM broker composition (`cst_saved_field_broker_service_windows.py`) | Independently load policy, resolve service SIDs/descriptors, host broker pipe, authenticate daemon, own broker nonce, source-root and workspace-root capabilities, worker/containment route, and produce only owner-observed result facts. | Depends on broker protocol/server, containment, worker protocol and vendor isolation; never frontend protocol or MCP. |
| Windows containment owner (`cst_saved_field_containment_windows.py`) | Broker-side owner of one process-wide `WorkerInheritanceEpoch`, the fresh worker Job, exact atomic five-handle launch, duplicate inherit/access readback, immediate parent duplicate close, watchdog, readers/cancellation, accounting, termination, broker capability receipt, startup proof, result receipt, and close. | Depends on `ctypes` and broker-worker protocol; every broker `CreateProcessW` path must acquire the same epoch and use an exact handle allowlist; never imported by daemon. |
| Windows vendor-isolation owner (`cst_saved_field_vendor_isolation_windows.py`) | Own SCM service `McpLocalHubCstVendorBroker` running as virtual account `NT SERVICE\McpLocalHubCstVendorBroker`; authenticate only SCM service `McpLocalHubCstDaemon`; create a SYSTEM/broker-owned protected-DACL workspace; launch one fresh CST worker by inheriting the broker token; return exact token/process/Job/pipe/workspace receipts; settle all exits. | Uses documented SCM, LSA, token, access-control, named-pipe, process, and Job APIs. No password exists and no credential/token enters policy, logs, frames, or application/vendor code. |
| Broker protocol owner (`cst_saved_field_broker_protocol.py`) | Own daemon↔broker challenge/request/response/settlement, nonce/correlation/deadline/policy binding, and ceilings. | Imported only by daemon and broker. |
| Broker-worker protocol owner (`cst_saved_field_broker_worker_protocol.py`) | Own fixed native `WorkerPreMainBootstrapV1`/`WorkerPreMainReceiptV1`, the later broker application request, `WorkerCapabilityReceiptV1`, and worker result/settlement; no daemon source handoff, path, policy loader, or authority selector. | Native bootstrap, broker and Python worker share only these versioned neutral schemas. |
| Native worker bootstrap (`mcphub-cst-runtime.exe --role=worker`) | Its first application instruction clears/readbacks inheritance on stdin/stdout/stderr and emits startup containment proof without reading stdin. It receives the fixed pre-main frame, reconstructs and identity-validates only its two directory capabilities, clears/readbacks both inheritance flags, emits `WorkerPreMainReceiptV1`, and only then initializes embedded CPython through the closed `PyConfig`. Any failure closes capabilities, emits no success and permits zero Python/source/vendor/CST/descendant work. | Direct-created only by broker from the signed immutable bundle; no PATH, console-script, site, environment, registry, cwd or caller module path participates. |
| Broker worker composition (`cst_saved_field_broker_worker.py`) | Runs only after broker accepts `WorkerPreMainReceiptV1`; receives native-validated capability objects plus one application frame, performs the closed transaction and returns the truthful application/capability result. Python never owns or self-attests pre-main execution or inherited-flag facts. | Invoked inside the same native worker process after isolated CPython initialization; never policy/config/env/path authority. |
| Saved-field application owner (`cst_saved_field.py`) | Own contracts, selection, order, units, hashes, zero semantics, and atomic result object. | Depends on vendor port and provenance hashing. |
|  | Own the normalized `CallSettlement` aggregate; consume every workspace/acquisition/session receipt on success and error. |  |
|  | Emit the sole downstream settlement event from that aggregate. |  |
|  |  | Also depends on trusted workspace. |
|  |  | Does not import `jobs.py`, `cst_results.py`, or solve code. |
| Neutral saved-field port (`cst_saved_field_port.py`) | Own wire request and immutable vendor/acquisition/batch/failure/receipt values, including `AuthorizedVendorPathLease`; no CST implementation. | Imported by protocol/core/worker/adapter, never as a concrete reverse edge. |
| CST vendor adapter (`cst_saved_field_vendor.py`) | Validate raw vendor records; execute inside the isolated vendor worker; perform the exact activation/sample sequence only with relative roles resolved by one required borrowed `AuthorizedVendorPathLease`. It may pass lease-returned path strings to path-only CST calls but cannot derive, reopen, or settle workspace paths itself. | Depends inward only on neutral port. |
|  |  | Receives CST API objects and owned session; no import back to `cst.py` or application core. |
|  | Return typed values/statuses; never interpret them as FEM coefficients or a whole-port oracle. |  |
| `open_owned_sampler_session` acquisition transaction | Sole owner of every vendor resource from the first create/open operation through complete-token validation. | Returns normally only after complete validation. |
|  |  | The return transfers into the pre-created `SamplerSession` adoption slot. |
|  | On any earlier error/exception, close each exact local handle without save and return settlement evidence with the typed failure. | Never uses process inventory or set difference as authority. |
| `OwnedSamplerSession` vendor contract | Return opaque close/liveness handle plus exact vendor-attributed process identity. | Vendor adapter owns creation. |
|  | Never derive authority from process-set difference. | `SamplerSession` receives ownership exactly once. |
| `SamplerSession` | Own workspace contents/session lifecycle and the sole `AuthorizedVendorPathLease` obtained from its adopted snapshot. | Owns resources inside the inherited workspace capability; vendor borrows them only for the call. |
|  | After transfer, solely own returned handle/identity, held vendor-path objects, generated headers, diagnostics-only snapshots, and content cleanup. Lease settlement and child capability close precede broker-owned workspace-root deletion. |  |
| `FrameResolver` | Purely select exactly one candidate by E/H, port, mode, frequency, optional exact selector, hashes, and optional adaptive-pass identity. | Typed candidate list only; no filesystem or CST access. |
| `SavedFieldResponseV1` | Own names/order, units/provenance, hash proof, finite numbers, and non-coefficient declaration. | Pure resolved/sample records. |
| MCP result publisher (`cst.py`) | Validate `FrontendDaemonResultV1` plus client-observed `FrontendTransportReceiptV1`, check original deadline, and cap final `TextContent.text`; no source/vendor access or re-encoding. | Receives no `BrokerResponseV1` and has no broker dependency. |
| Safe FastMCP error policy (`strict_fastmcp.py`) | Replace only this tool's pre-entry validation exception text with a fixed safe error. | Runs before application entry; no raw argument value is copied. |
| `AbsoluteInvocationBudget` | SCM daemon creates integer `{qpc_frequency, admitted_tick, deadline_tick=admitted_tick+60*qpc_frequency}` after admission and propagates it unchanged through daemon result, broker and worker protocols. Every receiver verifies frequency/equation/current bounds; no rebase/reset. Worker-frame cutoff is `deadline_tick-2*qpc_frequency`; publication requires `now<deadline_tick`. | Cleanup alone gets `cleanup_deadline_tick=termination_tick+10*qpc_frequency`; it permits no source/vendor/success work. |
| `SamplerBudgetPolicy` | Own all finite request/tree/file/metadata/candidate/concurrency/duration limits. | Resolved once and injected from composition root. |
| `TrustedWorkspacePolicy` | Own local/non-reparse root, operating-system owner/access rules, and no-follow factory selection. | Resolved once and injected from composition root. |
| Workspace creation transaction | Own the exact child from creation until normal-return `WorkspaceLease` transfer. | Rolls back on every post-create failure. |
| `SamplerAdmissionGate` | Sole writer of availability, policy revision, active lease, and bounded waiter state; atomically seals admission or quarantine and emits one downstream `admission_settled` result. | SCM-daemon process state only; frontend has no gate and no route-specific precheck grants authority. |
| `WindowsPathIdentity` | Own one closed grammar and exact-handle identity proof for the policy file, policy/workspace roots and project, source/destination walkers, vendor candidates, clean payload/header, and registration paths. | Standard-library Win32 calls only; no CST dependency. |
| `AuthorizedBundleTransfer` | Copy every manifest-v2 row by handle-relative opens under the inherited source-root capability directly into the inherited protected workspace capability, prove identities and complete destination equality, then transfer `AuthorizedWorkspaceSnapshot`. | Python worker owns the bounded transaction only after accepted native `WorkerPreMainBootstrapV1`/`WorkerPreMainReceiptV1`; broker alone derived both capabilities from independently loaded policy and daemon supplies no source path/bytes/handle. |
| `AuthorizedVendorPathLease` | Inside the isolated vendor principal, retain ancestors with `FILE_SHARE_READ|FILE_SHARE_WRITE` but not `FILE_SHARE_DELETE`; retain every read-only payload/header with `GENERIC_READ` and exactly `FILE_SHARE_READ` (no write/delete sharing); classify the copied project as output/write-capable unless target proof establishes read-only access. | Concrete owner is the isolated snapshot/session boundary. Unknown generated outputs are isolation-owned and are sealed to the read-only mode only after CST Save returns and the CST writer handle is closed. No daemon path access, `Path.exists`, or path-based cleanup is permitted. |
| Acceptance comparator (test-only) | Join public output to independent evidence; validate Line10 labels/counts; issue PASS/FAIL without fitting. | Depends on public schemas only. |
|  |  | May not import sampler/vendor implementation modules. |

Allowed dependency graph:

`cst.py -> {cst_saved_field_port.py, cst_saved_field_policy.py,
cst_saved_field_frontend_protocol.py, cst_saved_field_daemon_client_windows.py,
strict_fastmcp.py}`;
`internal/daemon.StdioHost -> internal/api.HubEnrollmentClientV1`;
`cst_saved_field_daemon_service_windows.py -> {
cst_saved_field_hub_enrollment_windows.py, cst_saved_field_frontend_protocol.py, cst_saved_field_policy.py,
cst_saved_field_broker_protocol.py, cst_saved_field_broker_client_windows.py}`;
`cst_saved_field_broker_service_windows.py -> {
cst_saved_field_broker_protocol.py, cst_saved_field_broker_worker_protocol.py,
cst_saved_field_containment_windows.py, cst_saved_field_vendor_isolation_windows.py}`;
`cst_saved_field_broker_worker.py -> {cst_saved_field.py,
cst_saved_field_vendor.py, cst_saved_field_port.py,
cst_saved_field_broker_worker_protocol.py}`;
`cst_saved_field.py -> {cst_saved_field_port.py, provenance.py, safety.py}`; and
`cst_saved_field_vendor.py -> cst_saved_field_port.py`.

The offline policy-author edge is `author_exact_appid_policy.ps1 -> {pinned ConfigCI,
pinned cipolicy.xsd, immutable bundle manifest}`. The independent deployment edge is
`internal/cli P18 verifier -> ExactAppIdPolicyArtifactV1 -> CstAppControlPolicySetOwner`.
No runtime frontend/daemon/broker/worker edge points back to the author or parser.

Only broker-worker composition imports both application and concrete vendor. Frontend
never imports broker protocol/client. The SCM
daemon never imports/initializes CST, containment, worker, or source access for this tool. Application core never imports
the concrete adapter. Policy/containment/safety modules receive typed values and do
not read ambient environment/configuration.

The acceptance comparator points only to the MCP wire contract and a separate
native-evidence schema. No production edge points to Line10, VFEM, or the
acceptance comparator.

## Credential-free broker identity and IPC contract

The only admitted broker account is the Windows virtual service account
`NT SERVICE\McpLocalHubCstVendorBroker` for SCM service
`McpLocalHubCstVendorBroker`; the only admitted client is SCM service
`McpLocalHubCstDaemon` with service SID `NT SERVICE\McpLocalHubCstDaemon`. Both run
in session 0. Password-backed users, group managed service accounts, built-in
`LocalService`/`NetworkService`/`LocalSystem`, interactive users, and selectable
account names are rejected: built-ins are shared/overprivileged, while a virtual
account is credential-free, host-local, and has one service-specific SID. Target
CST/license incompatibility with that identity fails registration without fallback.

| Credential box | Exact owner and contract | Falsifier |
|---|---|---|
| Storage | LSA/SCM alone owns virtual-account material; no password or application-readable secret exists. Elevated provisioning creates both fixed services, sets service-SID type, pinned executable/image paths, session-0 noninteractive launch, and SYSTEM/Administrators-only service-configuration DACL. Runtime code cannot modify SCM/LSA. | Any password/secret store, configurable account name, writable service configuration, or application `LogonUser` path fails provisioning. |
| Injection | SCM/LSA creates the exact broker and daemon tokens at service start; the broker creates its fresh CST worker by ordinary token inheritance. Startup verifies token user/service SID, session 0, integrity, privileges, image pin, and current SCM PID before registration. | Missing/wrong SID, stale PID/token, interactive/network logon, unexpected privilege, or unpinned image leaves sampler absent and tears down any started resource. |
| Exclusion | Policy, environment, argv, manifests, all three protocol frames, supervisor intent, logs, diagnostics, dumps requested by this feature, and MCP output contain no password, reusable token, launch capability, SID value, license value, or service-control secret. The launch capability exists only in hub memory, one inherited read-once pipe and daemon's pending hash until terminal consumption. | Cross-channel credential/capability/SID/token/license canaries must be absent; any leak blocks publication and quarantines runtime admission. |
| Rotation/revocation | There is no password rotation. Operator/installer owns service disable/delete, binary/package rotation, and identity replacement. Rotation stops both exact SCM services, waits exact service process handles/Jobs/workspaces absent, replaces the pinned package, restarts, and revalidates SID/PID/policy revision. Identity change requires a successor fixed service name/SID and decision; old service is disabled, its pipe/ACL removed, and old tokens/workers proved gone before registration. | Disable/delete/wrong-SID/version/policy-revision during startup or a live call revokes admission, terminates/settles exact resources, consumes outstanding nonces, and requires full service restart; no stale token/session/workspace is reused. |

The enrollment endpoint is
`\\.\pipe\mcp-local-hub-cst-saved-field-enrollment-v1`, a daemon-owned fixed
single/first-instance remote-rejecting message pipe. Its runtime-resolved protected
DACL owner is the daemon service SID; its protected DACL grants SYSTEM and daemon
service SID full owner/server rights plus the policy-owner SID only
connect/data/synchronize rights; daemon never treats that SID as enrollment authority.
Its SACL is High-integrity no-write-up. Runtime numeric SIDs build SDDL; post-create
readback must exactly match owner, protected control, ordered allow/deny ACE
type/SID/mask/inheritance and mandatory label before accept.
On connection daemon obtains peer token/PID/session/image and independently calls the
existing supervisor IPC status channel. Before trusting hello, daemon calls
`GetNamedPipeServerProcessId`, opens that exact PID query-only, and proves kernel
creation time, token user/session and canonical installed `mcphub` image equal both
`SupervisorLockOwner{PID,StartedAt}` and the installed canonical supervisor identity;
a first-instance squatter fails before status request. Supervisor independently obtains
the connected client PID, impersonates it, and proves exact enabled daemon service SID,
token user/session/integrity and pinned daemon image before dispatch. The supervisor
pipe's runtime-numeric protected DACL grants its owner SID/SYSTEM normal owner rights
and daemon service SID only read/write/synchronize; High-integrity SACL and exact
owner/control/ordered-ACE/label readback are mandatory. Status returns kernel-backed
`PIDGeneration`/creation time
for the exact CST task. The supervisor is a user process, not an SCM service; no
supervisor service SID is asserted. Enrollment succeeds only when peer PID, kernel
creation time, canonical installed `mcphub` image, token user/session, exact CST task
and generation equal the current supervisor status row, and the supervisor IPC hello
matches the current `SupervisorLockOwner{PID,StartedAt}`. Self-asserted frame values,
policy-owner SID, digest, parent PID or image alone grant nothing.

The daemon service SID is authorized server-side for exactly opcode
`GET_CURRENT_CST_TASK_IDENTITY_V1` with implicit exact task `cst`; the schema has no
task selector. All generic status, control, respawn, reconcile, exit and other-target
opcodes are denied before generic dispatch. `SupervisorCstIdentityAuthorizerV1` owns
this per-opcode branch; if the existing dispatch cannot prove pre-dispatch separation,
implementation must use a distinct status-only supervisor endpoint within the same
declared supervisor pipe surface, not broaden generic capability.

`HubEnrollmentProtocolV1` is closed canonical JSON, maximum 4096 bytes, with one
daemon CNG challenge and one `Enroll{challenge,correlation,task,generation,
capability_sha256}` or `Cancel{correlation}` frame, followed by one terminal receipt.
Both challenges use `BCryptGenRandom(NULL,buf,32,BCRYPT_USE_SYSTEM_PREFERRED_RNG)`;
digest is exact SHA-256 of 32 capability bytes and compared only after peer/status
authentication. One pending entry maximum, five-second expiry, atomic
channel nonce `ISSUED->CONSUMED|CANCELLED`. Successful authenticated Enroll consumes
only that channel nonce and atomically creates capability ledger
`ISSUED->ENROLLED`; ACK/flush/channel close leaves it ENROLLED, never consumed.
Exact child 32-byte+EOF intake followed by successful daemon frontend challenge changes
`ENROLLED->CONSUMED`. Authenticated `CancelEnrollmentV1{correlation}` on a fresh
independently authenticated enrollment exchange, start/write/read failure, child exit,
expiry, hub/daemon shutdown or restart changes `ENROLLED->CANCELLED` and removes the
pending digest. Duplicate/cancel-after-consume/replay performs zero admission.
Frame/read/write/flush/
disconnect/close failure terminalizes or quarantines; supervisor/status ambiguity
rejects before enrollment. `HubEnrollmentReceiptV1` records only server-observed peer,
challenge/correlation/generation/digest-ledger terminal state and channel settlement.

The frontend endpoint is
`\\.\pipe\mcp-local-hub-cst-saved-field-frontend-v1`, created by the SCM daemon as
single/first-instance, duplex/overlapped, message-mode and remote-rejecting. SYSTEM and
daemon service SID own it; the restart-loaded policy-owner SID receives only data,
attributes and synchronize rights. Before frontend creation, the actual
`StdioHost.Start` owner enrolls one pending launch-capability hash bound to its current
supervised host generation, host PID/creation time and pinned frontend image/package.
The secret travels only through an explicit anonymous-pipe read handle included in the
child's Windows HANDLE_LIST; it is absent from `HostConfig.Env`, argv, manifest,
supervisor intent and diagnostics. Frontend reads exactly 32 bytes once, closes the
handle, and never persists or logs them.

On accept the daemon obtains client PID, creation time, image/package and parent host
PID/creation time, and requires exact equality with the one pending enrollment and the
current supervised CST host generation before parsing a request. It then sends one
five-second 256-bit challenge. `FrontendDaemonRequestV1` contains exact schema,
non-authoritative `entry_id`, closed `SavedFieldRequestV1`, launch capability, challenge
nonce, unique correlation and request hash. Daemon constant-time compares the launch
hash and atomically consumes both enrollment and frontend nonce before admission. It
independently resolves exactly one unique policy entry by `entry_id`; missing, unknown,
duplicate, revision-ambiguous or request/entry mismatch returns
`cst_saved_field.not_authorized` with zero broker/source/CST work. No implicit first or
default entry exists, and no source path/bytes/handle or caller manifest crosses this
pipe. Same-owner separately launched identical binaries, stale children, PID reuse,
lookalikes, replay, reconnect, second/trailing frames and prior-restart values fail.
Every post-challenge exit reaches `CONSUMED|CANCELLED`; missing terminal proof
quarantines the seventh tool.

The broker endpoint is `\\.\pipe\mcp-local-hub-cst-saved-field-v1`, created by the
broker with `PIPE_ACCESS_DUPLEX|FILE_FLAG_FIRST_PIPE_INSTANCE|FILE_FLAG_OVERLAPPED`,
message/read-message/wait mode, `PIPE_REJECT_REMOTE_CLIENTS`, and `nMaxInstances=1`.
Its security descriptor has a protected DACL: SYSTEM and the broker service SID get
the exact pipe full-control rights needed by the owner; the daemon service SID gets
only `FILE_READ_DATA|FILE_WRITE_DATA|FILE_READ_ATTRIBUTES|SYNCHRONIZE`; Anonymous
and Network SIDs are explicitly denied; no Everyone/Users/interactive ACE and no
`FILE_CREATE_PIPE_INSTANCE`, `WRITE_DAC`, `WRITE_OWNER`, `DELETE`, or generic-all
right is granted to the client. The SACL contains a High-integrity mandatory label
with no-write-up; both service tokens must verify High-or-System integrity. Failure
to create/read back the exact DACL/SACL is startup failure, not a default-descriptor
fallback.

Before sending bytes, the daemon verifies `GetNamedPipeServerProcessId` equals the
current PID returned by SCM for `McpLocalHubCstVendorBroker`, opens that exact process
query-only, and verifies token user/service SID, session 0, integrity, and pinned
image. On accept, the broker verifies `GetNamedPipeClientProcessId` equals SCM's
current `McpLocalHubCstDaemon` PID, calls `ImpersonateNamedPipeClient`, opens the
thread token, and compares exact token user, enabled daemon service SID, logon SID,
session 0, integrity, and prohibited privileges. All comparisons precede parsing or
privileged work. `RevertToSelf` executes in `finally` on every success/failure path.
Failed impersonation performs zero work; failed/uncertain revert terminates the broker
process immediately so SCM closure settles the channel, and the daemon quarantines.
No privileged broker code runs on a thread whose self-token restoration is unproved.

After mutual token binding, the broker sends one bounded challenge containing
protocol `v1`, a 256-bit `BCryptGenRandom` nonce, broker policy revision, and a
monotonic five-second expiry. Exactly one canonical request (maximum 131,072 bytes)
must echo that nonce and carry unique 128-bit correlation, exact policy revision,
entry ID, manifest-v2 hash, request hash, and closed operation schema. The broker
atomically consumes the nonce before authorization—even on later failure—and compares
all authority fields with its independently loaded immutable policy plus current
quarantine/SCM identity before copy or worker start. Stale/replayed/duplicate/nonced-
wrong/trailing/second frames do zero copy/CST work. One bounded canonical response
uses the existing 1,114,112-byte ceiling; raw pipe bytes never reach diagnostics.

| Pipe/authorization return | Required owner action and receipt |
|---|---|
| Descriptor, bind, SCM PID/token, session, integrity, impersonation, or request validation fails | Consume any issued nonce, cancel/close exact overlapped pipe operations and instance, prove `RevertToSelf`, record zero copy/worker, return stable broker-auth failure. |
| `RevertToSelf` fails or cannot be proved | Do not reuse the thread; terminate broker service process immediately, settle exact pipe/process observation from daemon, latch quarantine, and require SCM restart. |
| Client disconnects/cancels before nonce consumption/authorization | Cancel overlapped I/O, consume challenge, close instance; zero workspace/CST work and no reconnect continuation. |
| Disconnect/cancel/timeout after workspace or worker creation | Consume nonce permanently, cancel pipe I/O, terminate/wait/close exact worker Job/process/thread/token, settle session/leases/workspace, suppress response/success, then close pipe. |
| Normal response | Complete worker/session/lease/workspace settlement first, write one bounded frame, flush, disconnect, cancel no-longer-needed I/O, close instance, and record nonce/correlation terminal once. |
| Pipe close/flush/cancel, worker, token, Job, lease, or workspace settlement is unproved | `containment_settle_failed`; quarantine before admission release. No retry on the old nonce/connection; a later call requires a fresh connection/challenge. |

Topology is singular: pre-spawn `supervisor-tracked CST StdioHost ->
HubEnrollmentProtocolV1 -> SCM daemon`, then application `hub -> existing stdio
frontend -> FrontendDaemonProtocolV1 -> SCM daemon -> BrokerProtocolV1 ->
broker-owned contained worker -> vendor`. Frontend
owns existing-six compatibility, seventh-tool transport and MCP publication only.
Daemon owns frontend authentication/nonce, entry resolution, admission, original
deadline, broker authentication, nested receipt validation, and quarantine. Broker owns
policy reauthorization, nonce, source/workspace root capability opens, protected
workspace-root lifecycle, exact five-handle worker creation, Job/streams, and merged
worker settlement. Worker owns its inherited duplicates, handle-relative transfer,
workspace contents, snapshot/session/vendor resources until its response and capability
close; broker deletes the root only after child exit and close proof. No frontend↔broker
or daemon↔worker channel exists; frontend/daemon never supply
source bytes/path/handle, and worker result returns only through broker. On pipe cancel/
disconnect the daemon cancels its I/O and broker terminates/settles its worker before
the pipe closes; absent broker receipt makes daemon quarantine. On daemon death the
pipe breaks and broker performs the same settlement. On broker death SCM process loss/
pipe break makes daemon quarantine; broker worker Job kill-on-close owns tree death.
Daemon sends one `FrontendDaemonResultV1` only after broker plus
`BrokerExchangeReceiptV1` validation. Frontend sends correlation ACK after complete
result/terminal-frame validation and before daemon disconnect. Daemon locally forms
`DaemonResponseReceiptV1` from response/terminal writes, flush, ACK, disconnect and its
handle close, then releases admission or quarantines; it never waits for or asserts
frontend EOF/client close. Frontend observes disconnect as EOF, closes its handle and
locally forms `FrontendTransportReceiptV1` from response/terminal read, EOF-or-cancel
and client close, then publishes or suppresses; it never asserts server close.
ACK loss/flush/server-close failure quarantines daemon admission even if frontend later
sees bytes; missing EOF/client-close suppresses frontend publication without requiring
impossible notification back to the closed server. Any partial/trailing/mismatched path
suppresses success and each owner terminalizes its own nonce/resources exactly once.

## Operator authority policy contract

`MCPHUB_EM_CST_SAVED_FIELD_POLICY` is the only enablement input. Frontend, SCM daemon,
and broker each read it once at their own restart. Missing/empty means disabled;
relative, remote, reparse, unreadable, malformed, unsupported, or access-policy
failure means disabled with a safe local diagnostic. Disabled state registers no
sampler tool, creates no watcher/thread/worker/workspace, and leaves the existing
six tools unchanged.

The policy is closed canonical JSON, at most 1,048,576 UTF-8 bytes:

| Field | Exact `mcphub.cst.saved_field_authority.v1` contract |
|---|---|
| `schema` | Exact string `mcphub.cst.saved_field_authority.v1`. |
| `enabled` | Exact Boolean; only `true` permits registration. Absence/false is disabled. |
| `entries` | 1..128 closed entry objects; unique `entry_id`; no two entries may resolve to the same root identity plus project-relative path. |
| `entry_id` | 1..64 ASCII characters matching `[a-z0-9][a-z0-9._-]*`; diagnostics may use it. |
| `root` | Ordinary local DOS-drive directory `X:\\...`, at most 4,096 Unicode scalars / 16,384 UTF-8 bytes. The colon at index 1 after one ASCII drive letter is the sole admitted colon; UNC, extended/device/NT/GLOBALROOT/volume-GUID, remote/mapped drive, link, mount, reparse, stream, and alias spellings are forbidden. |
| `root_identity` | Required `volume_serial` as unsigned 64-bit integer and `file_id` as exactly 32 lowercase hexadecimal digits, generated out of band from the held root. |
| `project_relative` | 1..1,024 Unicode scalars / 4,096 UTF-8 bytes, at most 32 canonical Windows components separated in policy JSON by `/`, final suffix `.cst`; every component obeys `WindowsPathIdentityV1` below. |
| `project_sha256` | Exactly 64 lowercase hexadecimal digits. |
| `mesh_sha256` | Exactly 64 lowercase hexadecimal digits for sibling `Result/3d.slim`. |
| `bundle_manifest_sha256` | Exactly 64 lowercase hexadecimal digits for `sha256-canonical-file-list-v2`. |

The policy file and parent directory are opened locally/no-follow. Their owner is
the server process token's user security identifier. Effective Windows access may
grant only that owner, `SYSTEM`, and Builtin Administrators; no inherited or explicit
entry may grant any other account/group read or write. The same rule applies to each
configured root. These identities come from the operating system, never fields in
the policy.

`WindowsPathIdentityV1` is shared without reimplementation by policy-file/root/
project and trusted-workspace parsing, both walkers, vendor-record validation,
clean-payload/header construction, and ResultTree
registration. Before any filesystem query, a full input must be ordinary
`[A-Za-z]:\\` drive-absolute form; the drive colon is valid and every other colon or
stream suffix is rejected. Relative components are NFC-exact, non-empty, neither
`.` nor `..`, at most 255 UTF-16 code units, do not end in dot or space, contain no
control or `< > : " / \\ | ? *`, and contain no `~`.

Reserved-device recognition is one reject-only operation in that same lexical
predicate, not a transformation that can make a rejected spelling admissible. For
each raw component, it first rejects every colon/stream suffix and trailing dot or
space, requires the raw value to equal its Unicode Normalization Form C (NFC), then
derives a comparison-only alias key by applying invariant case folding, stripping
the first dot and everything after it, applying Win32 trailing-dot/space trimming
defensively, and mapping `¹` -> `1`, `²` -> `2`, and `³` -> `3`. It rejects keys
`CON`, `PRN`, `AUX`, `NUL`, `CLOCK$`, `CONIN$`, `CONOUT$`, `COM1`..`COM9`, and
`LPT1`..`LPT9`. Thus ASCII `COM1`..`COM9`/`LPT1`..`LPT9` and documented Unicode
aliases `COM¹`/`COM²`/`COM³` and `LPT¹`/`LPT²`/`LPT³` are rejected
case-insensitively, with any extension; an attempted stream suffix or trailing
dot/space is already rejected before alias-key construction. Alias-key operations
are never emitted as a canonical path and never replace the NFC-exact input. The
predicate performs the raw and derived-key checks in one call and accepts only
after both finish, so no normalization step occurs after validation that can
introduce an accepted device alias. Raw/normalized UNC,
extended-length, Win32-device, NT-object-manager, `GLOBALROOT`, `Volume{...}`,
other device, and mapped-drive forms are rejected before stat/open/enumeration/content
access. This intentionally excludes legitimate tilde names and named streams in P0
to keep one unambiguous default-stream object model.

After lexical acceptance and before child content access, each held parent directory
is enumerated with long-name, short-name, and file-ID metadata. The requested name
must equal the unique NFC long name exactly, may equal no reported short name, and
casefold+NFC keys must be unique. Every opened directory/file must round-trip through
`GetFinalPathNameByHandleW` to the canonical long DOS-drive path and through
`GetFileInformationByHandle[Ex]` to the expected volume serial plus 128-bit file ID.
Reparse points are rejected; each regular file must report link count exactly one;
while its exact handle denies write/delete sharing, stream enumeration on the
handle-derived final path must return exactly unnamed `::$DATA`, followed by the
same file-ID recheck. Unsupported or incomplete
long/short-name, stream, link-count, final-path, volume, or file-ID proof fails closed
as identity ambiguity. Thus hard links, 8.3 access aliases, non-default Alternate
Data Streams (ADS), and alternate file objects cannot satisfy a manifest row.

`sha256-canonical-file-list-v2` includes the project anchor plus every and only
regular default-stream file under its same-stem sibling directory. Each row is
`{path,type:"regular",stream:"::$DATA",size,sha256}` with the unique canonical NFC
long forward-slash relative path, unsigned byte size, and lowercase hash. Rows sort
by UTF-8 path bytes and canonical JSON has no insignificant whitespace. The SHA-256
of that UTF-8 array is the manifest identity; project and mesh hashes are also
checked independently. Runtime-only source and destination receipts bind each row
to volume serial, 128-bit file ID, link count one, exact final path, stream proof,
and metadata-before/after observations; those machine identities are not persisted
in the portable policy hash.

Mutable invocation admission has one owner, `SamplerAdmissionGate`; immutable
resource allow/deny remains `AuthoritySnapshot`. First,
`SamplerAdmissionGate.acquire_and_seal()` obtains the bounded concurrency permit,
then under its single lock rechecks `available` plus the exact immutable policy
revision and creates one non-transferable admission lease. Immediately before any
broker/source work, `lease.authorize_start()` rechecks the same latch,
revision, lease generation, and active identity under that lock. Only then does the
daemon performs lexical `root + project_relative` match; a miss returns
`cst_saved_field.not_authorized` with zero filesystem calls. It starts the absolute
invocation budget and sends an authority-only broker request. Broker independently
matches policy and its Job-contained worker executes the transfer below. Daemon never
opens, stats, enumerates, hashes, copies, or transports source.
Caller `expected_*` hashes remain integrity assertions but never create or widen
authority.

The successfully loaded policy becomes an immutable `AuthoritySnapshot` keyed by
the canonical policy SHA-256 revision. There is no hot reload, watcher, cache update,
or prior-call mutation. Operator change/revocation is atomic file replacement plus
CST-daemon restart. Restart revalidates the entire file and roots; failed reload
leaves the sampler unregistered, not running under the previous snapshot.

## Stable external input contract

The tool has closed objects at every level (`additionalProperties=false`). Installed
FastMCP 1.29.0 validates this model before function entry and raises its own tool
validation error before calling the function. The sampler-specific policy registered
at the FastMCP composition boundary replaces that exception text with a framework
error `CallToolResult` (`isError=true`) containing only
`cst_saved_field.invalid_request`. That path emits no sampler diagnostic event and
has no application `failure_id`. Existing tools retain the unmodified framework
path. All checks requiring application events occur after entry.

| Field | Exact contract |
|---|---|
| `project_bundle` | Required ordinary drive-absolute `X:\\...\\name.cst`, at most 4,096 Unicode scalars and 16,384 UTF-8 bytes. The drive colon is valid; every component and every other colon must pass `WindowsPathIdentityV1` before any filesystem access. |
|  | Reject raw/normalized UNC, extended/device/NT/GLOBALROOT/volume, mapped/remote, ADS, reserved, trailing-dot/space, tilde/8.3-shaped, non-NFC, wildcard, or control forms lexically; input is never echoed. |
|  | The held long-name/file-ID proof must bind the local regular project and same-stem retained directory uniquely. Reparse, hard-link, named-stream, short-name, case/NFC alias, escaping, or unprovable alternate-name identity fails before content read. |
| `expected_project_sha256` | Required; `null` or 64 hexadecimal digits for source `.cst`. Compare case-insensitively; output lowercase. |
|  | `null` means caller lacks this identity, not that source-integrity checks are skipped. |
| `field` | Required exact enum `E` or `H`. No case folding or aliases. |
| `result.port`, `result.mode` | Required positive integers. |
| `result.frequency_hz` | Required positive finite JSON number. |
| `result.frequency_tolerance_hz` | Required finite JSON number greater than or equal to zero. The same inclusive absolute tolerance is applied to inventory, initial `Result3D`, and post-registration frequencies. |
| `result.frame_selector` | Optional `null` or non-empty data-only string, at most 1,024 Unicode scalars and 4,096 UTF-8 bytes. |
|  | Exact match to one frame ID, machine-neutral tree path, or bundle-relative payload path; no substring/glob/case-fold/inference/fallback. |
| `result.expected_field_sha256` | Required key; `null` or 64 hexadecimal digits for the selected source `.sct` payload. |
| `result.expected_mesh_sha256` | Required key; `null` or 64 hexadecimal digits for source `Result/3d.slim` bytes, distinct from the canonical mesh-topology hash used by mesh export. |
| `result.adaptive_pass` | Required key; `null` or a canonical data-only string at most 256 Unicode scalars and 1,024 UTF-8 bytes. |
|  | A non-null request requires equal candidate metadata; unavailable metadata is a failure, not a wildcard. |
| `points` | Required 1..256 closed objects; unique non-empty `id` at most 128 Unicode scalars and 512 UTF-8 bytes; `xyz` exactly three finite numbers. |
|  | Input order and duplicate physical coordinates are preserved; only IDs must be unique. |
| `coordinate_unit` | Required exact enum `m` or `mm`, matching the package's currently admitted coordinate units (`servers/electromagnetics-mcp/src/mcphub_em_mcp/slim.py:141`). Other units fail explicitly. |
| `allow_solve` | Optional literal `false`, default `false`. `true`, `null`, non-boolean, or unknown field is a FastMCP pre-entry validation error; the sampler is not entered. |
|  | No post-entry `solve_forbidden` failure exists, and no internal layer has a solver dependency. |
| `max_points` | Optional integer 1..256, default 256. `len(points)` must be no greater than this caller-declared bound and the server cap. It does not raise the server cap. |

The cap is exactly 1,048,576 UTF-8 bytes of the single canonical JSON string placed
in `CallToolResult.content[0].text`. The MCP/JSON-RPC envelope and transport framing
are excluded, and `structuredContent` is omitted so the result is not duplicated.
The composition-root publisher measures the final text it emits; exceeding the cap
fails atomically with no partial rows or persistent spill artifact.

The injected `SamplerBudgetPolicyV1` is fixed for the lifetime of one call and is
not caller-raiseable. These P0 limits are part of the admission contract:

| Budget role | Exact P0 ceiling and enforcement point |
|---|---|
| Source relative depth | 32 path components; bounded no-follow walker rejects the 33rd before descending. |
| Source tree entries | 20,000 total directory entries and 10,000 regular default-stream files; the same counts bound source enumeration, transfer receipts, and complete destination-manifest validation before CST. |
| Source file bytes | 8 GiB per regular file; reject from no-follow handle metadata before read/hash/copy. |
| Source aggregate bytes | 16 GiB across admitted regular files; reject before copying the crossing file. |
| Vendor candidates | 4,096 records and 8 MiB total validated metadata; the port must deliver a bounded iterator, not a preallocated unbounded list. |
| Vendor/caller metadata | 4,096 UTF-8 bytes per vendor string; caller-specific lower limits above remain authoritative. |
| Concurrent sampler calls | One active sealed lease and at most one waiter. A second/expired waiter rejects before lexical/source/broker work. |
| Absolute invocation duration | 60.000 seconds from immediately after atomic admission seal plus lexical match and before descriptor construction, launch, or any source operation through final MCP publication. No source/vendor/success work is allowed at or after the deadline. |
| Launch/setup sub-budget | 5.000 seconds from admitted start to successful atomic child creation; it is part of, never additional to, the absolute budget. |
| Worker-frame sub-budget | Complete settled worker frame is due by `deadline_tick-2*frequency`; all work/encoding precedes it. |
| Daemon publication reserve | Final 2.000 seconds; publication requires QPC now below unchanged `deadline_tick`. |
| Containment settlement | `termination_tick+10*frequency` only for worker/pipe/resource settlement; never work/success. |
| Worker stdin/stdout/stderr | 131,072 / 1,114,112 / 65,536 bytes; overflow settles exact Job. |
| Job active processes | 16, enforced by Job Object active-process limit; breakaway flags remain unset. |
| Published response | Exactly 1,048,576 UTF-8 bytes of final `TextContent.text`; existing atomic publisher owns it. |

Any traversed entry whose size/type changes between enumeration and no-follow open,
or whose cumulative accounting cannot be proved, fails closed. Limits are checked
before allocating/copying the next unit. Budget failure after workspace/session
creation still follows the single settlement path and returns no partial content.

## Stable success output contract

The top-level schema identifier is `mcphub.cst.saved_field_sample.v1`. A success
object contains exactly these groups:

| Key | Required contents |
|---|---|
| `schema` | Exact string `mcphub.cst.saved_field_sample.v1`. |
| `value_kind` | Exact string `sampled_field_vector`; `fem_basis_coefficients=false`. |
| `field` | Selected `E` or `H`. |
| `component_order` | Exact array `ReX, ReY, ReZ, ImX, ImY, ImZ`. |
| `coordinate` | `input_unit`, resolved `project_unit`, and finite `input_to_project_scale`. The scale is the only coordinate transformation. Returned `xyz` stays in the caller's input unit. |
| `field_metadata` | Verified non-empty `field_unit`; `complex_encoding=real_imag`; `value_transform=none`; `phasor_reference=CST_native`. |
|  | Verified non-empty `time_dependence` and `time_dependence_status=verified`; no success before exact installed-version convention is known. |
| `frame` | Actual port, mode, frequency, local frame ID, machine-neutral tree path, bundle-relative payload path, and adaptive pass or `null`. |
|  | `#0003`-like IDs are never represented as frequencies. |
| `source_identity` | Sanitized CST product/version; lowercase SHA-256 for source `.cst`, selected `.sct`, `Result/3d.slim`, and the complete authorized manifest-v2; no absolute path, file ID, user name, PID, license text, or raw exception. |
| `source_integrity` | The three provenance-monitored source roles `.cst`, `Result/3d.slim`, and selected `.sct` carry before/after SHA-256 plus `unchanged=true`; aggregate true. This is a provenance signal, not the full authorization snapshot. |
| `authorized_workspace` | `manifest_schema=sha256-canonical-file-list-v2`, policy and complete destination manifest SHA-256, exact regular-file count/aggregate bytes, `complete_manifest_match=true`, `default_stream_only=true`, `unique_file_identity=true`. No path or machine file identity is published. |
|  | Authorization guarantees the immutable copied snapshot presented at vendor start. It does not claim every ancillary source file remains unchanged after its stable source handle is copied and closed. |
| `activation` | Initial/post-registration frequencies, Efield3D/Hfield3D type, version-keyed status policy, generated-header flag. |
|  | Contains no temporary path. |
| `sampling` | `requested_point_count`, `returned_point_count`, caller/server limits, `input_order_preserved=true`, and `interpolation=none_by_mcphub`. |
| `lifecycle` | `settled_event`, `closed_without_save=true`, `owned_sessions_created=1`, `owned_sessions_remaining=0`. |
|  | `owned_processes_created=1`, `owned_processes_remaining=0`, `temporary_workspace_retained=false`, `source_opened_by_mutating_api=false`. |
|  | No process identity/PID or self-referential byte-count field is published. |
| `points` | Same input length/order; keys: `id`, `xyz`, `ReX`, `ReY`, `ReZ`, `ImX`, `ImY`, `ImZ`, raw status, zero flag. |
|  | All six components are finite JSON numbers. |

`zero_ambiguous=true` if and only if all six returned components compare exactly
equal to numeric zero (including signed zero). It means “the sampler observed a
zero vector but this contract does not assert a physical zero.” There is no
epsilon, material inference, or `physical_zero` claim. Nonzero rows have
`zero_ambiguous=false`; that flag does not validate their physical correctness.

`vendor_status_raw` is evidence only. A numeric status is accepted only when the
exact CST product/version has a target-verified status-policy entry and the
activation, frequency, arity, finiteness, cleanup, and hash checks also pass.
Observed `-1` is not a cross-version success constant.

Success is all-or-nothing. Any point failure, post-hash mismatch, cleanup failure,
unknown vendor status, missing metadata, or final `TextContent.text` byte overflow
suppresses the entire success object.

## Internal data model

| Record | Fields and invariant |
|---|---|
| `AuthorityPolicyV1` | Closed persisted schema above; parsed independently by frontend, SCM daemon and broker from a trusted held file. |
| `AuthoritySnapshot` | Immutable policy revision, policy owner/access proof, and declared entry/root identities; no file watcher or mutable cache. |
| `AuthorizedBundleDescriptor` | Broker-created from its policy: entry ID/revision/root/project/manifest/budgets; daemon cannot construct it and supplies no source locator. Worker alone proves it. |
| `BrokerRequestV1` | Daemon-to-broker authority-only envelope: schema, correlation, nonce, exact policy revision/entry/manifest, request, and unchanged `{qpc_frequency,admitted_tick,deadline_tick}`; no source path/bytes/handle. |
| `BrokerResponseV1` | Broker-to-daemon closed success/failure plus complete nested worker/application/containment settlement; it never contains its transport's later flush/EOF/close. Correlation/policy/entry identities equal request. |
| `FrontendDaemonRequestV1` | Non-authoritative exact `entry_id`, closed `SavedFieldRequestV1`, launch capability, challenge nonce, correlation and request hash; no source path/bytes/handle, manifest or caller policy revision. |
| `FrontendDaemonResultV1` | Daemon-owned bounded success/failure with correlation, resolved entry identity, unchanged QPC triple and nested broker/containment facts; never self-attests frontend pipe flush/EOF/close. |
| `DaemonResponseReceiptV1` | Daemon-local correlation, response/terminal write, flush, ACK, disconnect and server-handle-close observations; gates admission release/quarantine and asserts no frontend EOF/close. |
| `FrontendTransportReceiptV1` | Frontend-local correlation, response/terminal read, EOF-or-cancel and client-handle-close observations; gates publication and asserts no daemon disconnect/close. |
| `WorkerPreMainBootstrapV1` | Native fixed-size versioned/checksummed prelude sent only after native startup proof: correlation, deadline, non-Boolean pointer-sized source/workspace locators, distinct roles, expected volume/file IDs and exact access masks. Native bootstrap consumes it before CPython; no path, policy/config locator, token, secret or Python object. |
| `WorkerPreMainReceiptV1` | Native proof of exact bootstrap/bundle image, three standard clear/readbacks, capability type/identity/access validation, two capability clear/readbacks, and `python_initialized=false`; broker must accept it before application frame. |
| `BrokerCapabilityReceiptV1` | Broker-local proof with correlation-bound inheritance-epoch ID; exact root `CreateFileW` tuples and returned identities; original inherit flags clear; exact `DuplicateHandle` desired access/options; duplicate effective access/type/identity and inherit-flag readback true; exclusive epoch acquisition; ordered five-entry HANDLE_LIST plus JOB_LIST; `CreateProcessW` completion; immediate parent duplicate closes while still inside the epoch; epoch release; retained-original settlement. It cannot attest worker flag clear or close. |
| `WorkerCapabilityReceiptV1` | Python worker-local proof consuming immutable native pre-main receipt: every descendant open handle-relative, both capabilities closed, and no new authority. It cannot attest native flag clear or parent closure. |
| `BrokerWorkerRequestV1` / `BrokerWorkerResponseV1` | Application request/result after accepted native prelude; request has correlation/policy/manifest/request/deadline but no handle locator. Broker merges separately owned broker/native/Python receipts. |
| `AbsoluteInvocationBudget` | Exact QPC integer triple plus cutoffs/stage; every receiver checks unchanged values and current QPC, never remaining-duration rebasing. |
| `SamplerAdmissionGate` | SCM-daemon process-lifetime owner of `available|quarantined`, immutable policy revision, monotonically increasing generation, zero/one active sealed lease, and zero/one bounded waiter. `acquire_and_seal`, `authorize_start`, normal release, and quarantine-plus-release are its only atomic transitions; no route reads state independently and no in-process clear exists. |
| `SamplerAdmissionLease` | Daemon generation/revision token rechecked immediately before broker work and released once after final broker/pipe/publication settlement. |
| `ContainmentSettlement` | Broker-owned worker Job/create/signal/exit/reference-close/active-zero/readers/handles receipt. No PID authority. |
| `DaemonCallSettlement` | SCM-daemon merge of admission, broker result, `BrokerExchangeReceiptV1`, `DaemonResponseReceiptV1`, unchanged deadline and quarantine; sole `request_settled`, no source recheck. Frontend separately owns public publication after `FrontendTransportReceiptV1`. |
| `SavedFieldRequestV1` | Closed wire model matching the input table; validated before filesystem or CST access. |
| `SamplerBudgetPolicyV1` | Immutable exact ceilings for strings, tree depth/entries/files/bytes, candidate count/metadata bytes, concurrency, response, absolute invocation duration, sub-budgets, and cleanup-only settlement. |
| `TrustedWorkspacePolicy` | Injected local root identity, platform owner/access predicate, and no-follow/open factory. Contains no ambient configuration reader. |
| `SourceBundleAnchor` | Raw-locality proof, no-follow source handles/identities, sibling root, source `Result/3d.slim`, bounded inventory, and a no-write capability. |
|  | It exposes read/hash/copy handles, never a writable source path/handle. |
| `SourceSnapshot` | Roles and lowercase SHA-256 values for `.cst`, mesh, and—after copied-frame resolution—the corresponding source `.sct`; copy hashes and post-call source hashes must agree. |
| `WindowsPathIdentityV1` | Shared lexical grammar plus held-directory long/short-name enumeration, final canonical path, volume serial, 128-bit file ID, link-count-one, no-reparse, and unnamed-default-stream-only proof. Absence or ambiguity of any proof is failure. |
| `AuthorizedManifestV2` | Exact portable rows `{path,type,stream,size,sha256}`, count, aggregate bytes, canonical SHA-256, and project/mesh roles. Machine-specific identities remain in the transfer receipt, not policy/output. |
| `AuthorizedBundleTransfer` | Worker transaction owning handle-relative descendants of the two broker-sealed root capabilities and counters until complete protected-workspace validation; it cannot open either root by name. |
| `AuthorizedWorkspaceSnapshot` | Committed workspace lease plus complete destination manifest-v2, per-row destination identity proof, default-stream-only/unique-file-ID proof, and the sole factory for one `AuthorizedVendorPathLease`. It exposes no ordinary vendor path and refuses settlement while a lease remains open. |
| `AuthorizedVendorPathLease` | Non-copyable isolation-owned transaction recording role, relative name, desired access, exact share mask, volume/file ID, stream/link/reparse proof, producer principal, pre/post hash where known, and close result. Read-only inputs are held `GENERIC_READ/FILE_SHARE_READ`; write-capable roles remain solely inside the distinct vendor principal and become read-only-exclusive before consumption. |
| `IsolatedVendorInvocation` | Exact broker authentication, distinct token user/logon SID proof, protected workspace DACL/owner proof, fresh worker process/thread/Job handles, protocol/stream ceilings, and settlement receipt. The daemon/interactive SID has no file access and no process-token/handle authority over this invocation. |
| `RawVendorCandidate` | Exact untrusted keys: `field`, `port`, `mode`, `frequency_hz`, `frame_id`, `tree_path`, `payload_relative`, `adaptive_pass`, `project_unit`, `field_unit`, `time_dependence`, `time_dependence_status`, `field_sha256`, `activation_type`, and `status_policy`. Missing/extra keys fail. |
| `FieldFrameCandidate` | Fully validated candidate: exact E/H; positive non-Boolean port/mode; positive finite frequency; bounded non-control data strings; `WindowsPathIdentityV1` canonical payload matching exactly one committed manifest row; optional bounded pass; lowercase 64-hex hash recomputed from the held workspace payload; admitted units/time-dependence/status. |
|  | `activation_type` must be exactly E→`Efield3D` or H→`Hfield3D`; `status_policy` is a version-admitted mapping of 1..32 exact non-Boolean integers to exact Booleans. No other activation/status value is admitted. |
| `RawVendorActivation` | Untrusted initial/post-registration frequency, generated-header identity, selected tree item, and sample batch. Both frequencies must be positive finite and within the same request tolerance; identities must remain no-follow/contained and bounded. |
|  | Batch row count must equal request count; each row has one exact non-Boolean status integer admitted by the validated policy and exactly six finite numeric components. Extra/missing/duplicate rows fail atomically. |
| `ResolvedFrame` | Exactly one candidate plus proof of each request predicate and expected-identity comparison. |
| `UnitTransform` | Exact input unit, project unit, and fixed finite multiplicative scale. Initial support is only `m` and `mm`; unsupported project units fail. |
| `SampleVector` | Input ID/coordinate, six finite components in the canonical order, raw vendor status, and exact-zero ambiguity flag. It is explicitly not a FEM degree-of-freedom record. |
| `OwnedSamplerSession` | Vendor-returned opaque session handle, exclusive process identity, handle-specific liveness probe, close-without-save operation, and non-transferable ownership token. |
| `AcquisitionSettlement` | Acquisition stage, transfer-committed flag, exact handles received, per-handle close attempts/outcomes, identity/liveness evidence, and safely attributed owned-resource remainder. |
|  | It contains no process-snapshot close authority and accompanies every pre-transfer failure. |
| `WorkspaceSettlement` | Created-child count, transfer-committed flag, rollback attempt/result, and attributed child remainder; returned on success and every post-create failure. |
| `WorkspaceLease` | Exact child identity plus cleanup authority transferred once from workspace factory to `SamplerSession`. |
| `SamplerSession` | Temporary root, authorized workspace snapshot, empty vendor-path-lease and owned-session adoption slots, diagnostic snapshots, stage, failures, and settled flag. |
|  | No mutable global resource registry; the separate admission gate is composition-owned and carries no session/process handle. |
| `CallSettlement` | Application-owned normalized merge of workspace, complete manifest-transfer/snapshot, vendor-path-lease, acquisition, post-transfer session, monitored source-hash, and budget evidence. |
|  | It is the only source for `session_settled`; no receipt field may be defaulted when a producer supplied it. |
| `PublishedSavedFieldResult` | Exactly one `CallToolResult` text item containing canonical finite JSON; no structured-content duplicate; cap applies to its final UTF-8 bytes. |
| `SavedFieldFailure` | Stable `failure_id`, stage, safe message, optional causal failure ID, and private cause chain for stderr diagnostics. Leaves return/raise this typed failure; only `cst.py` translates it to MCP. |
| `NativeFieldEvidenceV1` | Test-only producer identity/method, frame identities, units/convention, hashes, and six-component point rows. |
|  | Independence is false if its producer imports or calls sampler/vendor implementation. |

## Frame resolution and activation interaction

Step 1 starts in the stdio frontend and crosses the authenticated frontend exchange;
step 2 executes in the SCM daemon; steps 3–12 execute only in the broker-owned
contained worker; step 13 returns worker→broker→daemon→frontend. No CST object or source path/
bytes/handle crosses BrokerProtocolV1. The daemon starts one monotonic absolute deadline immediately after the atomic
admission seal and lexical admission in step 2 and before descriptor construction.
Every later source or success-producing operation is budgeted by that deadline.

1. FastMCP validates the complete closed wire model, including literal
   `allow_solve=false`, before tool entry. The sampler-only policy at
   `strict_fastmcp` catches that tool's validation `ToolError` and substitutes fixed
   `cst_saved_field.invalid_request` text with no rejected value. No sampler event
   exists. After entry, frontend sends only `entry_id` plus the closed request. The SCM
   daemon independently resolves that entry, applies semantic cross-field checks and calls only
   `SamplerAdmissionGate.acquire_and_seal()`. The gate admits one active call and one
   bounded waiter; after permit acquisition it authoritatively rechecks availability
   and exact policy revision under one lock. Quarantine/revision rejection releases
   the permit without lexical or filesystem work. No earlier latch observation can
   grant authority.
2. Immediately before broker work the sealed lease calls
   `authorize_start()`, which again checks availability, policy revision,
   generation, and active-lease identity under the same lock. The daemon then
   performs lexical policy match with zero filesystem calls, starts
   `AbsoluteInvocationBudget`, authenticates directly to broker, receives one nonce,
    and sends one `BrokerRequestV1` containing input, correlation, policy revision/
   entry/manifest and deadline only—no source path/bytes/handle—by the 5.000-second
   cutoff. Every earlier failure releases the lease. The daemon never accesses source.
3. Broker consumes/authorizes the nonce against independently loaded policy, opens one
   no-write source-root capability, creates and opens one restricted workspace-root
   capability, and spawns one contained worker with the exact inheritable duplicates,
   access/readbacks and exclusive inheritance epoch defined below.
   Broker composition validates `TrustedWorkspacePolicy`: local, non-reparse stable root;
   current operator/service owner; and restrictive effective access. The workspace
   factory transaction creates one child and retains root ownership
   through ACL/mode, resolution, containment, and identity checks. Every failure
   after creation removes that exact child before returning `WorkspaceSettlement`;
   normal return atomically transfers a complete `WorkspaceLease` into
   `AuthorizedBundleTransfer`; vendor/session code cannot yet observe it.
4. After its first-instruction startup proof and before any source/CST operation, the
   native worker reads one `WorkerPreMainBootstrapV1`, validates the two capabilities,
   emits `WorkerPreMainReceiptV1`; only after broker acceptance does embedded Python read
   one `BrokerWorkerRequestV1`, reconstructing
   only the exact two inherited directory handles, and proves their object type,
   correlation, expected volume/file identity, and fixed source/workspace roles. It
   validates `WindowsPathIdentityV1` by handle-relative enumeration under the source
   capability and every enumerated long component before child content access. For each expected
   manifest-v2 row in canonical order, open one exact no-follow source handle with
   write/delete sharing denied, prove final path/volume/file ID/link-count-one/
   default-stream-only identity, verify metadata, stream SHA-256 bytes from that same
   stable handle into one newly created no-follow destination handle, and verify
   source metadata did not change before close. Reject extra, missing, duplicate-ID,
   alias, hard-link, reparse, stream, count, size, deadline, or hash drift. Never use
   recursive path copy, root path reconstruction, or reopen source bytes by name.
5. While the transaction still owns the workspace, enumerate every destination
   directory through held handles, re-open every row, prove the same namespace and
   unique destination identities, recompute all row metadata/hashes, and require the
   complete destination `AuthorizedManifestV2`—cardinality, paths, types, unnamed
   streams, sizes, hashes, and canonical aggregate—to equal policy exactly. Only a
   successful equality check commits `AuthorizedWorkspaceSnapshot`; any mismatch
   removes the workspace and returns before CST/session creation. This is the
   authorization linearization point. It guarantees the exact copied snapshot
   presented at vendor start, while the existing three source post-hashes remain
   provenance monitors rather than a claim about all ancillary source files.
6. Inventory only on the committed authorized snapshot through the neutral
   bounded-iterator port.
   Validate every raw record before selection or path construction: exact record
   shape and the exact keys above; non-Boolean integer fields; E/H and consistent
   fixed activation enum; positive port/mode/frequency; admitted units,
   time-dependence status, and version-bound status policy; lowercase recomputed
   hash; bounded no-control data strings and total bytes; contained clean relative
   payload that resolves by canonical manifest row and destination file identity;
   candidate/map cardinality limits; and duplicate identities. Any malformed
   record fails atomically.
   Filter validated candidates in fixed order: field -> port -> mode -> inclusive
   frequency tolerance -> optional exact selector -> optional adaptive pass. Zero
   candidates is missing; more than one is ambiguous. No rank/first/fallback exists.
   Apply all non-null expected identities before opening a write-capable CST project
   session. No candidate, header, clean payload, or registration path can address a
   named stream, alias, hard link, or file absent from the authorized snapshot.
7. After the challenge-bound broker authorization, the isolation owner creates one
   fresh vendor invocation under fixed virtual account
   `NT SERVICE\McpLocalHubCstVendorBroker`, distinct from fixed daemon service SID and
   every interactive SID. A protected non-inheriting DACL owned by SYSTEM or the
   broker grants workspace access only to SYSTEM, Administrators for recovery, and
   that broker SID; it explicitly denies the daemon service SID. The broker
   proves the CST worker token user/logon SID, protected DACL, exact Job membership,
   and authenticated bounded request before copying authorized bytes into its
   workspace. Any same SID, accessible DACL, unauthenticated/replayed/stale request,
   reusable CST process, or missing proof is
   `cst_saved_field.vendor_isolation_unavailable` before vendor parsing and latches
   quarantine. No credential/token is serialized or logged.

   A pre-created `SamplerSession` inside that invocation adopts
   `AuthorizedWorkspaceSnapshot`, obtains and adopts its sole
   `AuthorizedVendorPathLease`, and calls transactional
   `open_owned_sampler_session` for only the copied project. The transaction is the
   sole owner of every vendor resource until it has a vendor-returned opaque handle,
   non-transferable token, exact attributed process identity, and handle-specific
   liveness probe. It then performs one exception-safe normal-return transfer into
   the session's empty adoption slot. The transfer point is after complete-token
   validation and before any other vendor/application operation. Adoption of the
   already validated token into the empty slot is non-throwing; any exception before
   normal return leaves the transaction armed. Normal return and adoption are one
   contract outcome, so no intermediate unowned state exists.
   Every earlier error or exception leaves the transaction armed to close without
   save each exact local handle once. Process snapshots remain diagnostics and
   never authorize attachment, connection, close, or proof of ownership. Both the
   normal `(OwnedSamplerSession, AcquisitionSettlement)` result and the error's
   `AcquisitionSettlement` are immediately consumed into application-owned
   `CallSettlement`; neither may be stored and discarded.

   The path lease opens each read-only payload/header with `GENERIC_READ` and exactly
   `FILE_SHARE_READ`, omitting both `FILE_SHARE_WRITE` and `FILE_SHARE_DELETE`; every
   ancestor uses directory read access and `FILE_SHARE_READ|FILE_SHARE_WRITE` while
   omitting delete sharing. Thus readers may coexist, while same-object writes and
   leaf/ancestor delete/rename are denied for the full lazy-read interval. Microsoft
   documents that requested access must be compatible with every existing share mask
   and that sharing remains effective until handle close
   (<https://learn.microsoft.com/en-us/windows/win32/api/fileapi/nf-fileapi-createfilea>,
   <https://learn.microsoft.com/en-us/windows-hardware/drivers/ddi/wdm/nf-wdm-ntcreatefile>,
   <https://learn.microsoft.com/en-us/windows/win32/api/fileapi/nf-fileapi-setfileinformationbyhandle>).
   Holding only the leaf is insufficient: every ancestor directory is retained so a
   deterministic ancestor rename cannot redirect CST's unavoidable path lookup.

### Owned-session acquisition transaction

The underlying admitted vendor primitive must synchronously return a directly
closable handle with the first created sampler resource. A primitive that can
create a process/project yet raise without returning any safe handle does not
satisfy this P0 port and cannot be used. Its target capability gate fails; process
inventory is not a recovery substitute.

| Acquisition point and failure | Rollback authority and exact action | Settlement evidence and outcome |
|---|---|---|
| Before create/open: capability/import/license precondition fails. | No vendor resource exists; the transaction owns no handle. | Settlement records zero created and transfer not committed. |
|  |  | Preserve `cst_unavailable`. |
| Create/open raises before a handle is returned. | The admitted primitive must prove zero creation or its own direct-handle rollback. | Record the primitive outcome. |
|  |  | Absent proof, use `session_settle_failed` and fail the target capability gate. |
|  | Process lists/set differences authorize no action. | No production success is admissible from an unprovable primitive. |
| A copied-project/session handle exists; token or exact identity fails. | The transaction closes that exact returned handle once without save. | Preserve `session_ownership_ambiguous` only after proved rollback. |
|  | It never attaches to a discovered PID. | Otherwise override with `session_settle_failed`, retaining the ownership failure as cause. |
| Exact identity exists; handle-specific liveness binding fails. | Close the exact returned handle once. | Use only handle/identity-specific evidence. |
|  |  | Failure to prove absence is `session_settle_failed`; snapshots are non-authoritative. |
| Complete fields exist; token assembly/validation fails. | Close the exact local handle once without save, then run its liveness probe. | Record close and liveness outcomes. |
|  |  | Successful rollback preserves the causal acquisition failure. |
| Immediately before transfer, including an injected exception. | The armed transaction closes the exact local handle once and probes liveness. | Record transfer not committed and the rollback outcome. |
| Complete token crosses normal return into the empty session slot. | The transaction disarms; only `SamplerSession` may close thereafter. | Record transfer committed. |
|  |  | Later session settlement owns close/liveness evidence. |

On every pre-transfer failure, the transaction completes its rollback attempts
before exposing the typed failure plus `AcquisitionSettlement` to the application
owner. The application `finally` still attempts workspace-content cleanup and source
post-hashes, merges those results with the acquisition/capability evidence, and emits exactly
one `cst_saved_field.session_settled`. A failed rollback or absent proof that a
safely attributed created resource is gone overrides the causal failure with
`cst_saved_field.session_settle_failed`.
8. Before each path-only operation the isolated vendor asks the lease for a named
   relative role. For `Result3D` open, the copied `.sct` and all ancestors are already held;
   the adapter passes that borrowed path to CST, then revalidates its known authorized
   hash before reading frequency/metadata. A copied project that CST may modify is
   classified write-capable and exists only in the protected vendor workspace; it is
   never represented as a read-only authorized input.

   For `Result3D.Save`, only the isolated vendor principal receives write authority.
   The lease owner reserves the canonical relative name under a held directory, then
   closes its reservation immediately before the synchronous Save only inside the
   protected workspace. This is not an authorization handoff to the daemon user: the
   DACL excludes that principal throughout. On return the owner opens the generated
   object with `GENERIC_READ`, share mask `0`, proves creator workspace/principal,
   canonical identity/default stream/link/no-reparse/non-empty bounds, hashes it, and
   keeps that exclusive handle until the header is copied to its canonical clean name.
   That clean header and clean payload are then held `GENERIC_READ/FILE_SHARE_READ`
   with known hashes through ResultTree/sample settlement. If Save requires a
   daemon-accessible directory, another writer, replace/delete permission outside the
   isolated owner, or cannot be sealed exclusively before consumption, target
   capability is FAIL. No same-principal close/reopen, share-write, unlocked, or
   unisolated fallback exists; handwritten headers remain forbidden.
9. Register only the leased clean payload/header as `Efield3D` or `Hfield3D`, select
   the new disposable `ResultTree` item, and re-read `GetFieldFrequency()`. Retain
   every lease through selection and all point sampling because CST may read lazily.
   Revalidate the exact inputs after register, after selection/frequency, and after
   the last sample. A mismatch fails; no alternate item is tried.

### Path-only vendor operation and return-path contract

| Vendor operation / return | Required lease state and postcondition |
|---|---|
| `open_result3d` normal return | Read-only payload is held `GENERIC_READ/FILE_SHARE_READ`; ancestors omit delete sharing; same identity and authorized hash validate after return. A write-capable project stays isolation-owned and is sealed before any bytes become an input. |
| `open_result3d` typed failure, exception, timeout, or partial vendor resource | The lease remains owned by `SamplerSession`; acquisition rollback runs first, then every held path object closes exactly once. No worker/vendor path delete or existence probe may settle it. |
| `save_result3d_generated_header` normal return | Only distinct vendor SID can write in the protected directory. After Save/writer close, owner acquires share-0 read handle, validates/bounds/hashes the unknown CST bytes, then converts them to a known-hash clean read-only input. |
| `save_result3d_generated_header` sharing violation, foreign writer, inaccessible isolation, replacement, exception, or timeout | `vendor_isolation_unavailable` or causal `activation_failed`; terminate/settle isolated invocation. No same-principal close/reopen, share-write, unlink/recreate, or alternate filename. |
| Clean payload/header copy normal return | Each destination is created by the isolated owner relative to held activation directory, hashed/revalidated, then retained `GENERIC_READ/FILE_SHARE_READ`; no writer can coexist. |
| Clean copy failure or identity drift | `authorized_copy_changed`; ResultTree registration count remains zero and all acquired objects settle by capability. |
| `register_result_tree` normal return | Both clean inputs and ancestors stay held and revalidate after registration; the ResultTree item is then selectable. |
| Register failure/exception/timeout | No selection/sample follows; cache/session cleanup runs while leases remain held, then leases close before workspace settlement. |
| Select and frequency normal returns | All registration inputs stay held and revalidate after each operation; exact selected item/frequency holds. |
| Select/frequency failure/exception/timeout | No alternate item and no point loop; common settlement order applies. |
| Each `get_field_vector` normal return | Leases remain live across the call and all six values/status validate; no per-point path reopen. |
| Point failure, malformed row, exception, timeout, or cancellation | No partial success; cache/session cleanup, final path revalidation, lease close, snapshot settlement, and workspace settlement are all attempted. |
| Cache clear / session close normal or exceptional return | Read-only locks remain live until both operations were attempted because vendor lazy reads are conservatively assumed; only then are exact path objects and ancestors revalidated/closed, followed by isolated worker/Job/token/broker settlement. |
| Lease close, snapshot settle, or workspace delete failure | `session_settle_failed` overrides success, the receipt records the exact failed owner stage, and the daemon admission owner quarantines after broker settlement. A fail-once close is retried once by the same lease owner only; no other owner/path fallback participates. |

The falsifier is one deterministic same-user adversary that successfully renames,
deletes, replaces, or ancestor-swaps any copied payload, generated header, clean
payload/header, or registration input while its lease is active, or any trace in
which CST consumes bytes whose held file identity/hash differs from the authorized
object. Either result falsifies this contract and blocks registration/pinning.
10. Convert each input coordinate by the one recorded scale and invoke
    `GetFieldVector` in input order. Validate the complete raw batch: exact row
    cardinality/order, one exact non-Boolean version-admitted status integer, six
    finite components, and no additional transform per row. Preserve duplicate
    physical points and distinct IDs.
11. In one `finally`-owned settlement path, invoke `ClearGeometryDataCache`, close
     only the exact vendor-returned owned handle without save, revalidate and close
     every vendor-path object/ancestor exactly once, then settle the snapshot and
     delete the workspace; run the session handle's liveness probe, take a diagnostics-
     only process snapshot, and hash monitored source files. Continue after individual
     cleanup errors so every owned resource receives an attempt. No path/`Path.exists`
     cleanup, worker-side delete, or snapshot-discovered PID authority exists.
12. Merge the complete `WorkspaceSettlement`, authorized manifest-transfer/snapshot,
     vendor-path-lease, `AcquisitionSettlement`, session, monitored post-hash, and budget evidence into
     `CallSettlement`; missing producer fields
     are an internal contract failure, never default success. Emit
     `cst_saved_field.session_settled`. Produce application success only if
     source hashes match, the attributed owned process is absent, the owned handle
     and every path lease and inherited capability closed. Broker performs and receipts
     final workspace-root deletion after worker exit; success is not externally visible
     until that broker-owned deletion succeeds.
13. Before the worker-frame cutoff, the worker completes final source hashes,
    application settlement, and canonical success encoding. Broker accepts one bounded
    frame only after worker Job settlement, merges its receipts, and returns one
    `BrokerResponseV1`. Daemon accepts only the settled response plus pipe settlement,
    validates schema/correlation/policy/receipt and final UTF-8 count, emits `request_settled`,
    and publishes those bytes directly as one `TextContent` before the absolute
    deadline with `structuredContent` absent. It neither repeats source hashes nor
    re-encodes success. Overflow becomes `response_too_large`; no success content is
    published. At or after the deadline only fixed bounded failure construction and
    cleanup are allowed—never source/vendor access or success serialization.

No step calls `run_solver`, `save(include_results=True)`, mesh adaptation, job
submission, cached job lookup, or result fallback.

## Broker-owned contained-worker execution and sibling exit paths

The daemon never imports/calls CST or spawns this worker. After admission and direct
BrokerProtocolV1 authorization, the broker alone invokes `WindowsContainedInvocation`.
It validates and uses the unchanged QPC frequency/admitted/deadline integer triple;
no local deadline is created. Termination gets only `termination_tick+10*frequency`
and never permits work:

Before registration, the same owner performs one fixed inert breakaway probe using
the pinned package probe executable, fixed empty input, `CREATE_NO_WINDOW`, no shell,
and the startup Job. `CreateProcessW`'s exact `PROCESS_INFORMATION` is always owner
state. Failed creation records `breakaway_denied=true`, `breakaway_created=false`,
then closes any returned thread/process handles. Successful creation records the
truthful inverse, immediately calls `TerminateProcess` on the exact returned process
handle, waits boundedly for that handle to signal, records exit, closes the exact
thread and process handles, and only then closes/settles the startup Job. It never
looks up a PID. Created/allowed/missing/contradictory proof always fails startup and
quarantines; if terminate, wait, exit, handle close, or absence receipt is incomplete,
startup remains blocked with `containment_settle_failed`. Quarantine is not a cleanup
substitute, and the escaped probe cannot be delegated to ordinary Job termination.

1. Provision one immutable local non-reparse CST runtime bundle containing signed/hash-
   pinned `mcphub-cst-runtime.exe`, exact CPython DLL, standard library, extension
   modules and application tree. For every call pass non-null `lpApplicationName` equal
   to the bootstrap's exact absolute held-file identity and command line exactly
   `"<bootstrap>" --role=worker`; current directory is the immutable bundle root.
   Parent launch admission validates the held runtime image, PE closure, signature and
   bundle-manifest hashes before creation; child self-verification repeats only after
   revocation and before Python initialization.
   No `python.exe`, `uvx`, console-script, shell, PATH, registry path, cwd module, caller/
   policy argv or ambient `PYTHON*` value participates.
2. Acquire the broker-process-wide, non-reentrant `WorkerInheritanceEpoch` before any
   inheritable handle is created. Every broker `CreateProcessW` route acquires this same
   owner, so no sibling creation overlaps the epoch. Create broker-owned anonymous
   stdin/stdout/stderr pipes. Open the source root exactly as
   `CreateFileW(canonical_source_root,
   FILE_LIST_DIRECTORY|FILE_TRAVERSE|FILE_READ_ATTRIBUTES|SYNCHRONIZE,
   FILE_SHARE_READ,NULL,OPEN_EXISTING,
   FILE_FLAG_BACKUP_SEMANTICS|FILE_FLAG_OPEN_REPARSE_POINT,NULL)`; omitting
   `FILE_SHARE_WRITE|FILE_SHARE_DELETE` denies another write-capable root open and root
   delete/rename for the held interval. Open the fresh protected workspace root exactly
   as `CreateFileW(canonical_workspace_root,
   FILE_LIST_DIRECTORY|FILE_TRAVERSE|FILE_READ_ATTRIBUTES|FILE_ADD_FILE|
   FILE_ADD_SUBDIRECTORY|FILE_DELETE_CHILD|SYNCHRONIZE,
   FILE_SHARE_READ|FILE_SHARE_WRITE,NULL,OPEN_EXISTING,
   FILE_FLAG_BACKUP_SEMANTICS|FILE_FLAG_OPEN_REPARSE_POINT,NULL)`; omitting
   `FILE_SHARE_DELETE` denies root delete/rename while allowing isolated child work.
   Both opens reject a non-directory, `FILE_ATTRIBUTE_REPARSE_POINT`, nonzero reparse
   tag, wrong final path, volume/file identity, owner or DACL. Originals must read back
   `HANDLE_FLAG_INHERIT=0`.

   Duplicate each original within the current process with exact
   `DuplicateHandle(GetCurrentProcess(),original,GetCurrentProcess(),&duplicate,
   dwDesiredAccess=<that root's exact access mask>,bInheritHandle=TRUE,dwOptions=0)`;
   `DUPLICATE_SAME_ACCESS` is forbidden. `NtQueryObject(ObjectBasicInformation)` must
   read back exactly the requested `GrantedAccess`; object type, directory/identity/role
   must equal the original; `GetHandleInformation` must read
   `HANDLE_FLAG_INHERIT=1` and `HANDLE_FLAG_PROTECT_FROM_CLOSE=0`. Any unavailable or
   unequal readback closes all handles and releases the epoch with no child. Exactly five
   child handles—stdin read, stdout write, stderr write, source-root duplicate,
   workspace-root duplicate—appear in the ordered
   `PROC_THREAD_ATTRIBUTE_HANDLE_LIST`; all broker pipe ends, retained originals,
   Job/process/thread handles, and every other service handle are non-inheritable.
   Start bounded stdout/stderr reader threads and retain exact native
   thread handles so settlement can call `CancelSynchronousIo`; retain the pipe read
   ends so `CancelIoEx`/close can unblock pending reads. Broker owns and joins both
   readers on every settled exit.
3. Create one unnamed Job Object and set, before any process exists,
   `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE | JOB_OBJECT_LIMIT_ACTIVE_PROCESS`, active
   process limit 16, and no `BREAKAWAY_OK` or `SILENT_BREAKAWAY_OK`. Broker owns
   the sole non-inheritable Job handle; the watchdog only borrows it and neither the
   worker nor descendants can inherit it. Completion-port messages, if collected,
   are diagnostics only.
4. Build one live `STARTUPINFOEX` with correct `cb`, `STARTF_USESTDHANDLES`, and the
   exact three valid child standard handles. Its attribute list contains exactly
   `PROC_THREAD_ATTRIBUTE_JOB_LIST=[job]` and
   `PROC_THREAD_ATTRIBUTE_HANDLE_LIST=[child_stdin_read,child_stdout_write,
   child_stderr_write,source_root_capability,workspace_root_capability]` in that order.
   Process/thread security attributes are `NULL`; `bInheritHandles=TRUE`; creation
   flags are exactly `EXTENDED_STARTUPINFO_PRESENT | CREATE_UNICODE_ENVIRONMENT |
   CREATE_NO_WINDOW`. The command/environment buffers, arrays, and attribute list
   remain live through `CreateProcessW` and are destroyed on every return. No
   suspended gap or post-create assignment exists: documented JOB_LIST assignment is
   part of creation, and no breakaway flag is passed. Immediately when `CreateProcessW`
   returns—before Job/startup validation—the broker closes its parent copies of all five
   child handles while still holding `WorkerInheritanceEpoch`, records each close, and
   only then releases the epoch. No inheritable duplicate survives the epoch in broker.
5. Require `IsProcessInJob(exact_worker_handle, exact_job)==true` and close the returned
   primary-thread handle. Before launch require the admitted no-CRT PE/import/TLS receipt
   and target-OS pre-entry loader-closure receipt above. At the native bootstrap's first
   executable instruction, before CPython initialization or package/user extensible code,
   it calls
   `SetHandleInformation(handle,HANDLE_FLAG_INHERIT,0)` on stdin/stdout/stderr and
   requires `GetHandleInformation` to read `HANDLE_FLAG_INHERIT=0` on all three. Only
   then it emits native `WorkerStartupProofV1` proving exact bootstrap image/bundle,
   Job membership, no console/
   breakaway, standard-handle bindings and the three actual clear/readbacks. Failure
   exits nonzero, creates no descendant and causes broker quarantine. Broker validates
   that proof, then writes fixed-size, versioned native `WorkerPreMainBootstrapV1` whose
   checksum-bound fields name the two numeric capability values inherited unchanged by
   Windows, expected kernel identities/roles, correlation and deadline. Native bootstrap
   validates both handles and clears/readbacks both inherit flags, then emits
   `WorkerPreMainReceiptV1`. Broker validates that receipt before sending any application
   request—all before
   their absolute/sub-budget cutoffs. The first-instruction proof does not self-attest
   unknown request locators; parent `BrokerCapabilityReceiptV1` proves the exclusive
   creation epoch/allowlist/parent closes and native pre-main receipt plus later
   `WorkerCapabilityReceiptV1` prove revocation/use/close.
   Any failure after child creation enters
   the same exact-Job termination state machine; a pre-child setup or
   `CreateProcessW` failure closes only already-created broker resources.
6. Only after the native five-handle revocation receipt does bootstrap apply the hardened
   absolute CPython load, transitive-module and mapped-identity checks above, then call
   `Py_InitializeFromConfig` with the closed configuration and invoke exact
   `mcphub_em_mcp.cst_saved_field_broker_worker:main` through the embedded API. A test
   marker written by the first native instruction and audit hook on Python initialization
   proves ordering; injected `.pth`/sitecustomize/usercustomize/global/user site and
   `PYTHON*` canaries never execute. The subsequent application frame is four-byte
   big-endian length plus canonical UTF-8 JSON, maximum 131,072 bytes, and contains no
   capability locator. Missing/invalid prelude/receipt or Python path/config mismatch
   closes capabilities, exits nonzero, performs zero source/vendor/CST/descendant work
   and quarantines. Descendants are opened with `NtCreateFile` using the validated
   root in `OBJECT_ATTRIBUTES.RootDirectory`, `OBJ_DONT_REPARSE`, and role-exact
   `FILE_DIRECTORY_FILE` or `FILE_NON_DIRECTORY_FILE|FILE_OPEN_REPARSE_POINT`; absolute
   names and root reopen are forbidden. It owns every
   handle-relative source read/hash/copy plus workspace contents and CST, and writes
   exactly one already encoded
   `BrokerWorkerResponseV1` no larger than 1,114,112 bytes by deadline minus 2.000 seconds.
   EOF, trailing bytes, second frame, invalid UTF-8/JSON/schema/correlation, missing
   receipts, nonzero exit, or any stderr byte is protocol failure. Stderr is drained
   to 65,536 bytes then discarded; only count/overflow is observable.
7. Normal completion first requires the exact worker handle signaled. Record its exit
   code, close that exact process handle, then query the still-open Job directly.
   Closing the process handle before querying is required because Job accounting may
   retain an exited process while a reference remains. If `ActiveProcesses>0`, call
   `TerminateJobObject` immediately; a normal worker exit never makes residual
   descendants acceptable. Only `ActiveProcesses==0`, complete EOF, bounded reader
   joins, valid settled frame, required handle closes, and publication before the
   absolute deadline can produce success.
8. One broker termination state machine owns timeout, missed launch/worker/publication
   cutoff, normal exit with residual activity, worker nonzero/crash, protocol/stream
   overflow, shutdown, broker exception, and reader nonjoin. It immediately calls
   `TerminateJobObject` on this Job, records worker signal/exit and closes worker handle before zero
   query, cancels blocked reader I/O via exact thread/pipe handles, closes the read
   ends, boundedly joins both readers, polls direct Job accounting to
   `ActiveProcesses==0`, and then closes the Job and remaining handles. The separate
   10.000-second settlement budget permits only these cleanup/proof actions and fixed
   bounded failure construction.

Capability cleanup has one exhaustive owner table; operating-system process teardown is
the fallback close for an inherited child handle, never a success receipt:

| Return point | Broker action | Worker action / admissible receipt |
|---|---|---|
| Source/workspace open, tuple/readback, pipe or duplicate fails before `CreateProcessW` | While holding epoch, close every created inheritable handle and retained original; remove only the exact fresh workspace child; close pipes/Job/attributes; release epoch last. | No child and no worker receipt. |
| `CreateProcessW` fails | While holding epoch, close all five child-side copies, then release epoch; close retained capability originals, pipes/Job/attributes and remove exact workspace child. | No child and no worker receipt. |
| Child created, standard-handle clear/readback or startup proof absent/invalid, request write fails, timeout/cancel/shutdown | Parent already closed all five copies before epoch release; terminate/wait exact Job, settle streams/process, close retained originals, then remove exact workspace root. | Standard-clear ambiguity creates no descendant/source/vendor/CST work; process exit closes both inherited capabilities; no `WorkerCapabilityReceiptV1`, therefore no success. |
| Request/bootstrap malformed, capability identity/role mismatch, or capability clear/readback fails | Terminate/wait exact Job if not already exited, settle streams/process, close retained originals, remove exact workspace root, return fixed `worker_bootstrap_invalid` or `worker_handle_revocation_failed` and quarantine. | Close both capabilities, perform zero source/vendor/CST/descendant work; process exit closes any remaining inherited handle; nonzero exit and no success receipt. |
| Application/vendor failure after bootstrap | Retain originals until child exit; merge only actual child/Job/stream evidence, close originals and delete root after leases/session settle. | Settle session/content, close source capability after final authorized post-hash, close workspace capability last, emit a truthful failure `WorkerCapabilityReceiptV1`, then exit. |
| Normal completion | Require valid worker response plus five successful worker inherit-clear readbacks, child signal/reference-close, Job active-zero, reader join and child capability-close receipt; close retained originals and delete workspace root before broker success. | Settle session/content, close source capability after post-hash, close workspace capability last, emit truthful success `WorkerCapabilityReceiptV1`, frame, and exit. |
| Any epoch/access/share/type/identity/inherit/close/delete/Job/stream fact absent or contradictory | Quarantine; never default a missing field/receipt and never retry with a path, ambient policy, or new capability. | No success; process exit remains the terminal handle close while the missing proof is reported as `containment_settle_failed`. |
9. If worker signal, process-reference close, active-zero, reader joins, or any
   required handle close cannot be proved within settlement, the SCM daemon
   calls `SamplerAdmissionGate.quarantine_and_release(active_lease)`. Under one lock
   it changes `available` to `quarantined`, revokes the active lease, records the
   policy revision/generation, releases the permit, and only then wakes a waiter. It returns
   `cst_saved_field.containment_settle_failed`; every later frontend,
   direct-daemon, or gate-off call returns
   `cst_saved_field.containment_quarantined` before source/broker work. There is no
   in-process clear.

Broker solely owns the non-inheritable Job handle. On broker crash, that last handle
closes and kill-on-close targets only the invocation Job. Worker cannot keep it alive because JOB_LIST assigns
membership but HANDLE_LIST excludes the Job handle. Target admission must prove the
   worker/CST tree disappears and an interleaved foreign CST process remains alive.

`SamplerAdmissionGate` owns admission through final daemon settlement.
It permits one active lease and one waiter for at most 1.000 second; extra/expired
waiters return `resource_busy`. A waiter awakened by normal release must still pass
the post-acquire and pre-work state/revision checks. A waiter awakened by
`quarantine_and_release` returns `containment_quarantined`, releases cleanly, and
cannot perform lexical/source/broker work. Proved normal/error completion calls
`release(active_lease)` only after final bounded response construction; under the
same lock it clears active identity, releases, and wakes. Every transition emits one
`admission_settled`. The installed synchronous FastMCP 1.29.0 path still
cannot observe a client cancellation while the function is running. Cancellation or
disconnect cannot abandon cleanup: daemon cancels pipe; broker settles worker within
the 60+10-second bound, and transport may
suppress the late response. This is the exact external contract, not a cancellation
claim.

| Sibling path | Lifecycle owner and exact outcome |
|---|---|
| Policy absent/disabled/invalid/unsupported | Composition root omits registration; existing six tools remain live. |
| Pre-entry schema/type/extra-field failure | Fixed `invalid_request`; no broker event. |
| Admission waiter limit/expiry | `resource_busy`; zero lexical/source/broker work. |
| Quarantine while waiting | `containment_quarantined`; zero lexical/source/broker work. |
| Policy revision/generation mismatch | `policy_revision_changed`; zero source/broker work. |
| Lexical policy miss | `not_authorized`; zero filesystem/broker calls. |
| Pre-Job setup/CreateProcess failure with no child | Containment owner closes every created pipe/thread/attribute/Job handle and returns `containment_unavailable`; no source access occurred. |
| Authorized identity/manifest mismatch | Worker returns typed failure before CST; broker settles Job. |
| Worker normal exit, active zero | Broker records/settles and returns nested receipt. |
| Worker residual activity | Broker terminates exact Job and returns `containment_residual_process`. |
| Absolute cutoff | Daemon cancels pipe; broker terminates/settles worker; proved receipt yields `deadline_exceeded`, otherwise quarantine. |
| Worker protocol/stream crash | Broker terminates/settles Job; raw bytes discarded. |
| Client cancellation/disconnect | Daemon cancels pipe; broker settlement is mandatory; transport suppresses late result. |
| Reader EOF/join absent | Broker cancels worker I/O and settles; missing proof quarantines. |
| Graceful daemon shutdown | Daemon closes client lease and requests broker settlement; it never closes worker Job. |
| Broker exception/shutdown | Broker `finally` settles exact worker Job before response/exit. |
| Broker termination | Its sole Job handle kill-on-close terminates worker tree; daemon pipe loss quarantines. |
|  | Restart revalidates policy/containment and has no prior call state; target crash proof must show no owned tree remains and foreign CST is unchanged. |

## Failure modes and observable signals

Every post-entry semantic failure is an MCP tool error whose message begins with
the stable `failure_id`; the same ID and stage go to the injected diagnostic port.
Pre-entry FastMCP validation remains framework-owned, but the sampler-specific
`strict_fastmcp` policy replaces its raw exception text with fixed
`cst_saved_field.invalid_request`. Unexpected causes are chained privately and
never rendered into MCP text, stderr evidence, or settlement fields.

| Failure mode | Stable observable discriminator | Neighbor distinction |
|---|---|---|
| Policy missing/disabled | Tool absent from catalogue plus local `cst_saved_field.policy_disabled` | Existing six tools remain; no call can enter. |
| Policy malformed/remote/reparse/wrong owner or access/unsupported platform/API | Tool absent plus local `cst_saved_field.policy_invalid` with safe stage | No stale policy fallback or partial registration. |
| ConfigCI/XSD/converter pin mismatch, contradictory/empty hash authoring, runtime AppID ambiguity, non-four-row UMCI cardinality, independent hash/XML/CIP parser disagreement, two-build drift, signing-key/provider/ACL/export-policy failure, signer/OID/algorithm/attribute/content/chain/revocation mismatch, leak or unproved signing cleanup | Tool absent plus provisioning-only `cst_saved_field.policy_artifact_unavailable` with fixed author/hash/appid/xml/cip/key/sign/signature/chain/revocation/close stage | No policy deployment call, no alternate rule level/toolchain/key/trust profile, and no registration; existing six tools remain live. |
| Complete App Control inventory, deployment authority, base-family union, other-base intersection, reboot commit, canary, update/removal journal, or absence proof is unknown or changes | `cst_saved_field.policy_set_unsettled` with fixed inventory/authority/composition/deploy/reboot/commit/remove/absence stage; no `AppControlPolicySetCommittedV1` or `AppControlPolicySetAbsentV1` | The owner touches only its supplemental; siblings stay untouched, sampler stays unregistered, services stay stopped, and VHDX selection/detach does not advance. |
| Lexically unlisted root/project | `cst_saved_field.not_authorized` | Zero filesystem/worker/vendor calls; no path echo. |
| Authorized root identity/access or exact project/mesh/manifest hash mismatch | `cst_saved_field.bundle_not_authorized` with identity role | Only bounded broker-worker authorization reads occurred; no CST creation. |
| Type/closed-schema/literal rejection, including `allow_solve` true/null/non-boolean | FastMCP `isError=true` with only `cst_saved_field.invalid_request` | Sampler was not entered; raw value is absent. |
| Post-entry cross-field invalidity, including duplicate IDs or points over caller `max_points` | `cst_saved_field.invalid_request` | Wire model passed, but semantic relation failed before source access. |
| Unsupported input coordinate unit | FastMCP `isError=true` with only `cst_saved_field.invalid_request` | Closed enum rejects before entry. |
| Lexical ADS/extra colon, reserved/device/globalroot/volume/UNC/mapped form (including ASCII or superscript-digit COM/LPT aliases with extension/case/trailing/stream variants), trailing dot/space, tilde/8.3-shaped, wildcard/control/non-NFC component | `cst_saved_field.path_namespace_invalid` with fixed role/stage | Rejected before filesystem access; ordinary drive colon remains valid; raw path not echoed. |
| Long/short-name enumeration, exact final path, volume/file ID, link-count-one, default-stream-only, case/NFC uniqueness, or no-reparse proof fails | `cst_saved_field.path_identity_ambiguous` with fixed proof role | Rejected before child content or CST; no alias/hard-link/stream fallback. |
| Source path/tree/file/byte budget crosses its ceiling | `cst_saved_field.resource_limit_exceeded` with budget role | Before copying the crossing unit; distinct from malformed layout. |
| Trusted root owner/access/locality/reparse policy fails | `cst_saved_field.workspace_policy_invalid` | Before creating a child or copying source bytes. |
| Workspace post-create initialization fails and exact-child rollback succeeds | causal failure ID plus `WorkspaceSettlement` | Child remainder is zero; no vendor entry. |
| Workspace exact-child rollback fails or remainder is nonzero | `cst_saved_field.workspace_settle_failed` with cause | Overrides causal failure and blocks reuse. |
| Admission waiter capacity/expiry | `cst_saved_field.resource_busy` | Before source/broker work. |
| Composition-root gate is quarantined, including latch while caller waits | `cst_saved_field.containment_quarantined` with bounded recovery-required stage | Mandatory post-acquire recheck releases cleanly before lexical/source/worker access on every route; no in-process clear. |
| Sealed policy revision/generation differs after acquire or at pre-work recheck | `cst_saved_field.policy_revision_changed` with fixed admission stage | No source/worker access; lease/permit released; no stale snapshot fallback. |
| Job/attribute/pipe/reader/worker create or membership verification fails | `cst_saved_field.containment_unavailable` with fixed stage | Broker closes the exact Job/handles; no in-daemon CST fallback. |
| Runtime PE receipt, exact-runtime per-app enforced policy, malicious-load denial, read-only-VHDX attach/namespace/stream proof, complete `PackageLoadClosureV1`, target System32/API-set manifest, entry disassembly, image identity, hardened CPython load, ABI symbol or mapped equality fails | `cst_saved_field.native_loader_invalid` with fixed loader stage | Before Python/source/vendor/CST or descendant work for pre-load failures; a load-time failure may have run only images admitted before entry by the exact policy, then terminalizes/quarantines. No audit/broad policy, read-write mount, weaker share/search, reopen, wrapper, alternate launcher, hash-only fallback, import hook or child self-check substitution. |
| Native package/module/root handle, mapping, Python finalization, or reverse-order loader settlement cannot be proved complete | `cst_saved_field.native_loader_settle_failed` with fixed close/mapping stage | Overrides the causal failure, emits no success, exits the process, and latches quarantine; no retry or package update is permitted. |
| Broker-worker request/response length, framing, UTF-8, schema, correlation, trailing bytes, stderr, or exit contract fails | `cst_saved_field.worker_protocol_invalid` with fixed stage | Raw stream/exception absent; broker terminates and settles the exact Job. |
| Runtime Job active-process cap is exceeded or contained child creation denies breakaway | `cst_saved_field.containment_limit_exceeded` | Exact invocation Job settles; no alternate/uncontained launch. |
| Startup breakaway probe unexpectedly creates a child and its exact handle terminates, signals, and closes completely | `cst_saved_field.containment_unavailable` with `breakaway_created` stage | Truthful denial proof is false; startup remains unavailable/quarantined despite complete escaped-child cleanup. |
| Created breakaway probe cannot be terminated, waited, exit-recorded, handle-closed, or proved absent by its exact handle | `cst_saved_field.containment_settle_failed` with `breakaway_probe_settlement` stage | Overrides startup cause; no PID/Job fallback and registration stays blocked. |
| Worker exits normally but Job accounting remains above zero | `cst_saved_field.containment_residual_process` after proved termination/zero settlement | Normal exit is not success while any descendant remains; foreign CST is untouched. |
| Missing `Result/3d.slim` | `cst_saved_field.mesh_missing` | Bundle exists; required solved mesh does not. |
| Expected project or mesh hash mismatch | `cst_saved_field.source_identity_mismatch` with role | Before copy/API; distinct from a concurrent change. |
| Any manifest row changes while its stable transfer handle is active, or a monitored project/mesh/selected-field source changes by postflight | `cst_saved_field.source_changed` with role/stage | Detects bounded transfer/provenance drift; does not claim postflight coverage for ancillary source files. |
| Complete destination manifest cardinality/path/type/stream/size/hash differs from policy, or transfer receipt is incomplete | `cst_saved_field.authorized_copy_changed` with fixed manifest stage | Entire workspace removed before CST; no partial-role success or daemon re-read. |
| A vendor input/ancestor cannot be leased, or its identity changes before/after a path-only CST call | `cst_saved_field.authorized_copy_changed` with fixed `vendor_path_lease` role/stage | No later vendor operation; no reopen, path-delete, or unlocked fallback. |
| Fixed virtual accounts/SCM identities, protected pipe/workspace DACL/SACL, mutual PID/token/session binding, impersonation/revert, fresh nonce, policy binding, or broker authorization is unavailable/ambiguous | `cst_saved_field.vendor_isolation_unavailable` with fixed broker stage | Before any copy/path-only CST write; no direct, same-principal, stale-token, replay, or in-daemon fallback. |
| Generated-header Save cannot run solely inside the isolated principal or cannot be sealed share-0 then converted to a known-hash read-only input | `cst_saved_field.activation_failed` with fixed `generated_header_capability` stage | Target capability fails closed; no share-write, daemon-visible, or unlocked retry. |
| No metadata candidate matches | `cst_saved_field.frame_missing` | Candidate count zero after named predicate. |
| Multiple candidates match | `cst_saved_field.frame_ambiguous` | Candidate count greater than one; no first-frame choice. |
| Exact selector refers to a different/duplicate candidate | `cst_saved_field.frame_selector_mismatch` | Selector-specific, separate from ordinary metadata absence. |
| Expected field hash/adaptive pass mismatch | `cst_saved_field.field_identity_mismatch` with identity role | Frame exists but provenance does not match. |
| Project unit, field unit, or required frame metadata unavailable/unsupported | `cst_saved_field.metadata_unavailable` with metadata role | Post-entry vendor metadata failure; no guessing. |
| Raw vendor record violates type/enum/finite/string/path/status/cardinality budget | `cst_saved_field.vendor_record_invalid` with record field role | Before selection, session creation, activation, or response allocation. |
| Vendor candidate/metadata aggregate budget crosses its ceiling | `cst_saved_field.resource_limit_exceeded` with vendor budget role | Bounded iterator stops atomically; no partial candidate set. |
| CST library/import/license/open unavailable | `cst_saved_field.cst_unavailable` with stage | Infrastructure/vendor start failure; no solve retry. |
| Create/open cannot prove zero creation before a closable handle is returned | `cst_saved_field.session_settle_failed` with acquisition stage | Capability failure; no PID fallback or production success. |
| Pre-transfer handle/token/identity/liveness is invalid | `cst_saved_field.session_ownership_ambiguous` after proved exact-handle rollback | Acquisition transaction owns rollback. |
|  |  | Snapshot deltas are never closed. |
| Pre-transfer close fails or safely attributed absence cannot be proved | `cst_saved_field.session_settle_failed` with causal failure ID | Overrides acquisition cause; no complete token crossed transfer. |
| Result3D open/frequency/save/header or ResultTree register/select/recheck fails | `cst_saved_field.activation_failed` with activation stage | Before point loop or at exact activation stage. |
| CST product/version or raw `GetFieldVector` status lacks verified policy | `cst_saved_field.vendor_status_unverified` | Raw value preserved in private diagnostics; never reclassified by guess. |
| Point call returns wrong arity/nonfinite data/error | `cst_saved_field.point_sample_failed` with point index/ID | Point-specific and atomic; no partial result. |
| Final canonical `TextContent.text` exceeds 1,048,576 UTF-8 bytes | `cst_saved_field.response_too_large` | Publisher-boundary failure; envelope excluded; no content/truncation/spill. |
| Post-hash, owned-handle close/liveness, cache clear, vendor-path lease close, snapshot settle, or workspace delete fails | `cst_saved_field.session_settle_failed` | Overrides success; fail-once path-handle close is retried once by its sole owner, and any remaining failure causes quarantine. |
| Authorization, worker-frame, publication, or absolute cutoff is crossed and settlement proves zero | `cst_saved_field.deadline_exceeded` | No worker content; only cleanup follows. |
| Worker settlement proof incomplete | `cst_saved_field.containment_settle_failed` | Daemon quarantines before admission release. |
| Client cancel/disconnect after entry | No sampler failure ID; daemon emits one `request_settled` within normal or 60+10 bound | Installed sync path cannot observe it; transport may suppress response. |
| Broker termination | pipe break, no MCP success | Job kill-on-close; daemon quarantines. |
| Unclassified implementation exception | `cst_saved_field.internal_error` | Safe generic wire message; full cause remains in local diagnostics. |

## Observability

- One diagnostic port is injected at the composition root; the application/vendor
  layers do not read logging environment variables or write protocol stdout.
- Daemon stable events are `policy_loaded`, `admission_wait_started`,
  `admission_sealed`, `admission_rejected`, `admission_settled`, `invocation_admitted`,
  `request_authorized`, `worker_started`, `termination_started`,
  `worker_signal_recorded`, `worker_reference_closed`, `readers_cancelled`,
  `job_active_zero`, `sampler_quarantined`, and `request_settled`. Worker receipt events are
  `source_snapshotted`, `manifest_row_transferred`, `authorized_copy_verified`,
  `workspace_owned`, `frame_resolved`,
  `session_owned`, `field_registered`, `sampling_completed`, `budget_rejected`, and
  `session_settled`. Failures carry only stable ID and last completed stage.
- Provisioning emits exactly one `policy_artifact_verified` only after independent XML,
  unsigned-CIP, request-bound HSM ceremony audit receipt, explicit signature, online chain/revocation and
  signed-content equality passes; failures emit only
  `policy_artifact_unavailable` plus the fixed stage and pinned-input digest. Neither event
  contains an absolute path, raw policy XML/CIP, private key/provider credential, certificate
  bytes, or live-policy state. Public certificate/key/profile hashes are allowlisted.
- A per-call correlation ID may appear only in local stderr diagnostics. It is not
  part of deterministic output or publication evidence.
- Diagnostics use an allowlist of fixed IDs, stages, roles, counts, Booleans, and
  publication-approved hashes. They exclude raw/rejected input, absolute paths,
  usernames, PIDs/handles/tokens, license text, project bytes, vendor strings/status
  text, coordinates, and raw exception dumps. Sanitization never logs the value it
  rejects; private exception chaining stays in memory only.
- Worker `session_settled` includes every normalized `WorkspaceSettlement`,
  `AuthorizedVendorPathLease` settlement, and `AcquisitionSettlement` field,
  post-transfer cleanup, budget role, source-hash
  result, and exact attributed remainder. It is the sole downstream settlement
  worker signal, including pre-transfer failure; receipt echo is equality-tested.
- `invocation_admitted` records fixed stage plus monotonic durations/sub-budget
  values only; it exposes no wall-clock timestamp or source value.
- Daemon `request_settled` includes policy revision/entry ID, authorization result,
  every normalized worker settlement field when present, all
  `ContainmentSettlement` fields, absolute-deadline stage, direct final active count,
  stream counts, availability-latch state, and publication result. It contains no
  daemon source recheck and is the only daemon-side settled event.
- `sampler_quarantined` is emitted after the atomic latch transition and before
  semaphore release. Later rejected calls emit only fixed
  `containment_quarantined` with policy revision and no source/worker data.
- FastMCP pre-entry validation produces no sampler event and only the fixed safe
  MCP error. The MCP result publisher
  emits `response_published` only after settlement and records the exact capped
  `TextContent.text` UTF-8 byte count in local diagnostics, not in response JSON.
- `admission_settled` is the single downstream-observable event for every sealed,
  rejected, normally released, or quarantine-revoked admission transition; it is
  emitted after the gate transition and contains no resource handle.
- Admission diagnostics contain only policy revision, lease generation, bounded
  wait class, decision, and gate state; namespace/manifest diagnostics add fixed
  role, proof stage, counts, bytes, and aggregate hash only. They never contain raw
  components, paths, stream/short names, file IDs, or rejected values.
- Authorization allow/deny diagnostics contain policy revision, entry ID when
  matched, identity role, and decision only—never caller path or file values.
  Containment diagnostics contain stages/counts/Booleans and no PID/handle/raw bytes.
- No metrics/network listener, background exporter, persistent log, policy watcher,
  or mutable resource registry is introduced. The three fixed local service pipes are
  the only listeners. Cross-call mutation is limited to pending one-use hub enrollment,
  frontend/broker nonce ledgers and SCM-daemon `SamplerAdmissionGate`. Per-call pipe readers terminate and join
  before proved `request_settled`; inability to prove that condition quarantines.

## Security and resource safety

| Control | Single owner | Enforceable contract and falsifying probe |
|---|---|---|
| Windows namespace boundary | `WindowsPathIdentityV1` | One grammar rejects every extra colon/ADS, special prefix, mapped/remote form, reserved/device/globalroot/volume name, trailing dot/space, tilde alias shape, wildcard/control/non-NFC component before filesystem access; ordinary `X:\\` drive colon remains valid. |
|  |  | Held-parent long/short-name enumeration precedes child content; exact final path, volume/file ID, link count one, no reparse, and only `::$DATA` prove each object. Missing/ambiguous proof fails closed. |
| Complete source-to-copy authority | `AuthorizedBundleTransfer` | For every and only manifest-v2 row, one stable no-follow source handle supplies hash and copy bytes to one new destination handle under finite count/byte/time ceilings. Extra/missing/changed/aliased/linked/streamed rows fail before CST. |
|  |  | Destination equals broker policy before snapshot; daemon performs no re-read. |
| Disposable-copy boundary | `AuthorizedWorkspaceSnapshot` plus its sole `AuthorizedVendorPathLease` | The snapshot exposes no ordinary path. The lease retains workspace/ancestor/exact-input handles without delete sharing, pre-creates and holds outputs, and lends path strings only during named synchronous CST calls. Clean payload/header and ResultTree registration use the same held-object proof. |
| Trusted output root | SCM broker composition | Read ambient `MCPHUB_EM_OUTPUT_ROOT` once at broker startup, convert it to injected `TrustedWorkspacePolicy`, and reject a missing, relative, remote, reparse, or policy-noncompliant root before workspace creation. Frontend/daemon/application/vendor modules do not read this ambient configuration. |
|  |  | Windows: resolved local non-reparse directory is owned by the configured service/operator security identifier; its effective discretionary access control list grants only that owner, `SYSTEM`, and Builtin Administrators, with no inherited access broadening. |
| Transactional workspace creation | `create_workspace_lease` in safety owner | The factory retains the child path from the first successful create through permission/identity verification and initialization. It transfers one complete `WorkspaceLease` only after all checks pass. |
|  |  | Failure at create, permission set, owner verification, initialization, or transfer rolls back only the exact factory-created child and returns a complete `WorkspaceSettlement`; unrelated siblings are never touched. |
| Source immutability | `AuthorizedWorkspaceSnapshot` | Only the complete policy-equal disposable snapshot can create the vendor-path lease reaching `Result3D`, `ResultTree`, generated-header staging, or any write-capable project operation. Three named source-role post-hashes remain provenance monitors; no claim is made that every ancillary source remains unchanged after its stable handle closes. |
| Vendor data | CST vendor adapter | Treat every vendor field as untrusted: require the full `RawVendorCandidate` record, validate type, length, containment, local/no-follow path identity, finite frequency, non-empty bounded unit, enum status, payload/header pairing, and complete role hashes before selection or session acquisition. |
|  |  | Vendor script shape is fixed. Only validated finite numbers use round-trip rendering; validated contained result identifiers pass one adversarially tested escape routine. Caller executable fragments are impossible. |
| Bounded work | `SamplerBudgetPolicyV1` in application owner | Enforce the exact path/tree/file/byte/candidate/metadata/point/response/concurrency limits in the contracts above. Check before allocation where knowable and during bounded iteration/copy otherwise. Exhaustion settles owned resources before returning `resource_limit_exceeded`. |
|  |  | Admission failures release before lexical/source/broker work. |
| Absolute duration | `AbsoluteInvocationBudget` in SCM daemon | Start after atomic admission seal/entry resolution and before broker descriptor/launch/source I/O; cover every source/hash/copy/vendor/final-hash/encode/read/validate/publish stage with launch and final-publication reserves. At deadline allow only fixed failure construction and cleanup. |
|  |  | Any stage block must produce settled `deadline_exceeded` by the absolute limit plus at most 10.000 seconds cleanup; no sub-budget extends the limit. |
| Exact Windows launch | `WindowsContainedInvocation` | Use the exact non-null application path, fixed quoted argv, explicit local cwd, isolated Python flags, deliberate inherited broker environment, NULL security attributes, `bInheritHandles=TRUE`, `STARTF_USESTDHANDLES`, ordered five-handle allowlist, JOB_LIST, exact creation flags, no shell/PATH/breakaway, and live buffers through `CreateProcessW`. |
| Pre-revocation PE loader closure | Native runtime build/provisioning owner plus the same-image custom entry | Direct import is exactly KERNEL32; delay import and TLS are absent; no CRT/static initializer/package DLL precedes entry; target debugger/loader trace admits only manifest-equal signed Microsoft System32 loader closure. Entry-RVA disassembly reaches every role-required inherit-clear/readback through direct allowlisted IAT calls only. |
| Post-revocation runtime load | P18 App Control policy plus same-image `NativePackageLoadOwner` | Enforced exact-runtime per-app UMCI rules deny every unlisted package or System32 image before first instruction; exact CPython loads only by absolute `LoadLibraryExW(DLL_LOAD_DIR|SYSTEM32)` from a read-only attached VHDX after revocation. Held backing-volume/mount/ancestor/version/file objects bind namespace/content, explicit stream enumeration requires only `::$DATA`, mapped rows and generated ABI symbols equal manifest, and closed `PyConfig` denies ambient import authority. |
| Policy-signing secret and authenticity | Offline operator `CstPolicySigningOwner`, sole `NShieldAuditCollectorOwner`, then P18 replay/receipt/envelope verifier | One exact non-exportable RSA-3072 content key exists only in nShield under non-persistent OCS K-of-N. Each sign emits a Transaction-ID-bound Sign/ObjectUse row; the pinned vendor exporter re-verifies its audit range, while P18 independently proves request/key correlation and the signed CIP's exact hash/signature/content/current chain/online-v1 revocation. Target has no signing route. |
|  |  | Export, wrong key/cert/provider, extra signer/attribute/certificate, corrupt signature, timestamp, chain/revocation unknown, leak canary, concurrent/ambiguous retry or unproved close leaves registration absent and performs zero deployment. Rotation disables registration/signing and settles old state before a reviewed base-authorized successor can sign a forward version. |
| Audit replay and deployment-input continuity | `NShieldAuditCollectorOwner`, P18 `AuditReceiptReplayOwner`, then `SignedPolicyImportOwner` | A reviewed collector/genesis attestation anchors ESN/logID/index; only fresh vendor-verified continuous strictly-greater JSON ranges or exact completed equality are admissible. Untrusted media never reaches CiTool: exact bytes move into a protected VHDX that is detached and reattached read-only, with backing/volume/mount/ancestor/leaf handles retained through CiTool and post-call revalidation. |
|  |  | Gap/reset/wrong logID/concurrent or crash ambiguity yields zero new CiTool call. A deterministic verify-to-open pause must reject media/leaf/ancestor/reparse/hard-link/ADS/write/delete/substitute mutations; inability of CiTool to read the held read-only path is incompatible, never a close/reopen fallback. |
| Vendor duration and settlement | Broker `WindowsContainedInvocation` | Timeout/residual worker terminates exact Job; accept only after worker signal/exit/reference close, active zero/readers/handles by cleanup deadline. |
|  |  | `PROC_THREAD_ATTRIBUTE_JOB_LIST` makes membership atomic at creation; active-process cap is 16 and breakaway flags are never enabled. Target trace must prove installed CST descendants remain contained. |
| Atomic invocation admission | `SamplerAdmissionGate` | One gate seals availability and exact policy revision after acquire, rechecks immediately before lexical/descriptor work, and owns active/waiting/quarantine transitions. No route-local observation grants entry. |
| Caller/input authority | Immutable `AuthoritySnapshot` | Tool absent unless policy enables it; broker-worker binds broker-policy source to protected workspace before CST. |
|  |  | Every hub/bare/direct/gate-off call reaches this same in-server owner. Caller flags/hashes and hub visibility never grant or widen authority. |
| Worker cleanup | Broker `WindowsContainedInvocation` | Broker solely owns Job/worker/pipes/readers/watchdog; worker cannot inherit Job handle. |
|  |  | Normal, residual-normal, error, timeout, shutdown, overflow, exception, and reader-stall paths use one state machine. Missing any required settlement proof suppresses all content and requests quarantine. |
| Fail-latched containment | `SamplerAdmissionGate` | Any unproved broker/worker settlement quarantines before release; future calls fail before broker work. |
|  |  | No call or in-process administrative path clears it. Only full daemon termination plus restart and fresh policy/containment startup validation restores availability; foreign processes are never inspected or touched. |
| Session/process cleanup | `open_owned_sampler_session`, then `OwnedSamplerSession` after transfer | Close authority comes only from exact vendor-returned handle/token/identity. Never connect, close, kill, or assign ownership by PID/name/path/set difference; never change an operating-system owner. |
|  |  | A live attributed resource, incomplete settlement receipt, or inability to prove zero creation/exact rollback suppresses success and returns `session_settle_failed` after all safe cleanup attempts. |
| Diagnostics and errors | Sampler-safe FastMCP policy plus application publisher | Errors, events, and responses are allowlist-built from stable identifiers, counts, stages, booleans, hashes, and bounded enum metadata. Never render exception strings or rejected input, absolute/resolved paths, PIDs, usernames, security identifiers, command text, environment, license data, or raw proprietary values. |
|  |  | Canary tests inject secrets into every input/vendor/exception field and assert absence from FastMCP error text, application failures, events, captured logging, and success JSON. Existing six tools retain their current error behavior because the mapper is registered for this tool only. |
| Evidence/export | Acceptance orchestrator | Production persists no evidence file. Bounded raw acceptance evidence may exist only under repository `/.scratch/`; the delivery artifact is redacted, machine-neutral, count/hash/schema based, and never contains retained projects or raw proprietary field payloads. |

### Policy-signing private-key four boxes

| Box | Single-owner contract | Falsifying probe |
|---|---|---|
| Storage | Enterprise/operator authority provisions one exact RSA-3072 key only inside an attested HSM on the isolated station; export policy zero and M-of-N hardware authorization are pinned. Target contains no key, KSP, credential, signer identity, service or endpoint; DACLs are not treated as a SYSTEM boundary. | Wrong HSM/provider/key/attestation/policy, successful private/PKCS8 export, target key/provider discovery, inbound ceremony endpoint, or signing without physical quorum keeps registration/deployment absent. |
| Use/injection | Target P18 exports one public content-addressed request. Offline `CstPolicySigningOwner` compares its displayed hashes to approval, obtains non-persistent OCS K-of-N physical authorization, invokes exact pinned SignTool/KSP with request/profile Transaction ID, and returns the held signed CIP plus `CstOperatorSigningReceiptV3` bound to the collector's verified JSON hash. | Direct target group/SYSTEM sign, wrong request/ticket/OCS/content-key/transaction/station/tool/collector/export, replay/concurrency/cancel/timeout or absent verified range yields zero CiTool calls. |
| Leak exclusion | Key and operator authentication never leave HSM; request/receipt are allowlist-built public hashes. Ceremony and target close all handles and wipe removable/work storage. | Canaries across request/receipt/media/argv/env/stdin/stdout/stderr/log/dump/journal/manifest/temp/publication and every cleanup failure prevent verified handoff/deploy. |
| Rotation/revocation | Disable registration/request export, stop processes, settle target/ceremony journals, obtain HSM authority/audit proof old key is disabled/destroyed, then admit a reviewed base-authorized successor and forward version through the same physical quorum and P18 online verification. | Old-key sign, target-local rotation, pending/same-version replacement, missing audit/quorum/base/reboot/canary or partial settlement remains default-off/unsettled. |

### Audit-attestation key and counter four boxes

| Box | Single-owner contract | Falsifying probe |
|---|---|---|
| Storage | nShield firmware alone creates and protects KML, separate from the content key; exact ESN, KML `logID`, firmware/software versions and warrant chain are pinned. The hardware audit index increments for each audit record since logging starts. No target/station file contains KML private material. | Private-key export/use attempt, alternate ESN/logID/warrant, software-signed segment, fabricated or host-derived counter, missing genesis, or unpinned firmware/software keeps registration absent. |
| Use/injection | Firmware signs native nCore segments; the sole collector persists them and pinned `nshieldaudit ... export --verify` owns KML/warrant verification while producing held JSON. P18 verifies exporter/command/database/output identity, contiguous indices and one successful Transaction-ID-bound Sign/ObjectUse whose `{runID,objid}` joins one ObjectNew with the pinned key hash. It independently verifies held signed-CIP bytes; vendor audit is not claimed to attest those bytes. | Second/unprivileged collector, config/database/exporter substitution, cached/text/malformed export, wrong Transaction ID/status/`{runID,objid}`/ObjectNew hash, gap or output mutation yields zero artifact admission and zero CiTool. |
| Leak exclusion | KML never leaves the HSM; ceremony media carries only the held vendor-verified JSON export and allowlist-built public receipt/artifact hashes, never raw database or KNETI/OCS/private material. Collector/service credentials and OCS material are absent from request, receipt, argv/environment, logs, dumps, journal, media and target. | KML/KNETI/collector-credential/OCS canaries on every channel, raw database/segment leakage or JSON accepted without the pinned exporter/profile fails the ceremony and requires cleanup before handoff. |
| Rotation/revocation | Audit rotation is independent of content-key rotation. Stop signing/collector; finalize and vendor-verify the old log without gaps; durably pin terminal `{ESN,logID,endIndex}`; consistently back up the stopped database; reinitialize under reviewed authority; enroll one successor privileged collector and vendor-verified genesis JSON; permanently reject old logID for new requests. | Dual collector, live-database copy, forced reinit/firmware upgrade without finalization, index reset/gap, old-logID new request, missing terminal/genesis or partial journal transition remains `policy_artifact_unavailable`. |

The tool has no dependency edge to solve, remesh, job submission, or fitting. The
only child launch is the fixed package worker by the broker containment owner; no shell,
caller executable/argv/environment fragment, or uncontained fallback exists. Literal
`allow_solve=false` and dependency/static-call guards prove absence downstream.
Registration/pin/deployment still require independent review and the target Job/CST
containment proof; missing proof fails closed without weakening this design.

## Contract and persisted-state migration

The MCP wire change is additive, but the protected CST launch contract is deliberately
migrated. An enabled policy adds one tool; existing-six schemas/responses/errors/ports/
stdio framing and `cst.py` call paths remain byte-compatible. `servers/cst/manifest.yaml`
replaces `command: uvx` plus `--from ... mcphub-cst-mcp` with
`launch_profile: cst-direct-v1`. This profile has no generic fallback: installer resolves
the reviewed runtime bundle to canonical absolute bootstrap/package paths and hashes in
the supervisor descriptor, and StdioHost creates that exact image directly.

The new tool's intended contract fixes two operational surfaces: sampler-specific
pre-entry validation emits FastMCP `isError=true` with only the stable
`cst_saved_field.invalid_request` text and no sampler event, while success JSON is
carried once as canonical `TextContent.text`; `structuredContent` is absent. The
1,048,576-byte cap applies to that text's UTF-8 bytes, not the MCP/JSON-RPC envelope
or framing. Both broker protocols are internal and all processes come from the
same immutable package pin; mismatch fails rather than negotiating.

The new persisted state is operator-owned authority policy `v1` plus
`CstPolicySigningProfileV1`, whose public fields pin certificate/key-provider/trust identity
but contain no secret or private key material. Its bundle identity now names
`sha256-canonical-file-list-v2`; the earlier design-only v1
manifest algorithm is not accepted or migrated. Operators must regenerate every
entry out of band before enablement. The only new process-lifetime mutable state is
`SamplerAdmissionGate`: policy revision/generation, one active lease, one bounded
waiter, and `available|quarantined`. It persists only for that daemon process and
carries no source/session/process handle or caller data. Expand/contract is: deploy hub
support that can parse/materialize but does not select `cst-direct-v1`; extend existing
T02 `LaunchCapabilityConfig`/`prepareLaunchCapability`/`windowsLaunchCapabilityPipe`
and their existing tests in place, preserving singleton `AdditionalInheritedHandles`
and rejecting all other inheritance/process-identity/security fields;
delete/reject any duplicate launch-capability helper/config/prepared state/ledger;
install and verify
the immutable runtime bundle; with sampler `enabled=false`, A/B-run direct bootstrap and
old uvx launch against all existing-six inventory/signature/request/error/response/stdin/
stdout fixtures; atomically migrate the CST descriptor/profile and restart; prove the
direct child PID/image/parent equals the supervisor task and uvx is absent; scan proves
one `HostConfig.LaunchCapability`, one `prepareLaunchCapability`, one platform Windows
implementation and one cancel ledger with no alias/fallback; only then
remove uvx from CST required binaries. There is no interval in which launch capability
is supplied to uvx. Next provision validated policy with `enabled=false` -> generate/validate manifest-v2 and namespace proofs -> run
policy/containment target gates -> atomically set
`enabled=true` and restart -> verify catalogue and one admitted call -> advance the
immutable package/manifest pin. Compatibility for the existing six is indefinite.

Rollback stops daemon and broker services, observes both exact process handles
signaled, and requires broker Job/worker/workspace absent. Then restore
`enabled=false`/remove the policy, restart to prove the tool absent and no invocation
active, and restore the prior CST package pin. Quarantine recovery uses the same full
termination/restart boundary and requires fresh policy and containment-startup
validation before registration; no in-process reset exists. Before sampler publication,
rollback may atomically restore the prior uvx descriptor only with launch capability
disabled/absent, restoring existing six but keeping sampler unregistered. After sampler
publication, rollback restores the prior reviewed direct-bootstrap bundle/profile;
capability-bearing uvx rollback is forbidden. A leftover policy is ignored by old code.
No worker/workspace/session/cache/job state is migrated or
retained. Manifest-v1 policies fail closed; rollback regenerates v2 rather than
silently interpreting v1 rows. Policy schema/default, hot reload, non-Windows containment, or broker
protocol compatibility across package pins requires a successor decision.

### Invalidated downstream plan surface

| Current plan section | Invalidation boundary |
|---|---|
| `Change-Surface Contract allocation` | Replace in-process composition with daemon/broker/broker-worker policy, protocol, containment owners, and the new decision dependency. |
| Phase 0 | Add default-off/policy-manifest-v2/protocol/Job/atomic-admission/quarantine/Windows-namespace RED contracts and protected-six disabled catalogue baseline. |
| Phase 1 | Move wire contracts to neutral port; add admission lease, policy, absolute budget, daemon/broker and broker/worker protocols, containment, path identity, manifest transfer/snapshot, and daemon settlement records. |
| Phase 2 | Keep only atomic acquire/seal/recheck plus lexical admission in the daemon; broker owns worker creation and every filesystem/identity/hash/copy plus workspace/application action executes in that worker. |
| Phase 3 | Precede vendor/acquisition with complete authorized workspace transfer inside the broker-owned worker; carry full manifest/identity receipts through broker to daemon. |
| Phase 4 | Replace direct application call with atomic admission owner, exact non-shell `CreateProcessW` tuple, absolute deadline/watchdog, cancellable bounded streams, already-encoded publisher, and safe FastMCP mapper. |
| Phase 5 | Add deterministic queued-waiter quarantine race, full ancillary-manifest drift, ADS/alias/hardlink/stream matrices to all prior deadline/residual/reader/crash/restart gates. |
| Phase 6 | Review decision, trust, admission, transfer, both broker protocols, launch ownership, quarantine, and security. |
| Phase 7 | Add exact Windows namespace/stream/file-ID plus Job/CST membership, breakaway, broker-crash kill-on-close, all-stage cutoff, quarantine, no-console, and foreign-preservation proof while preserving Claim 7 activation. |
| Phase 8 | Native-provider qualification semantics remain; inputs now require authorized broker-contained calls and daemon settlement evidence. |
| Phase 9 | Line10 counts/order/oracle remain; orchestration now checks policy/Job receipts without entering production modules. |
| Phase 10 | Existing-six smoke runs with policy absent, manifest-v1, invalid, disabled, and enabled-v2; all old contracts stay equal. |
| Phase 11 | Document v2 policy generation/revocation, namespace rejection, complete transfer, admission/quarantine, broker/worker timeout diagnostics, target evidence, and default-off catalogue. |
| Phase 12 | Stage default-off pin, provision manifest-v2 policy, run namespace/transfer/admission/target gates, enable/restart, verify, and retain full-daemon-termination plus disable-first rollback/quarantine recovery. |
| Dependency/rollback, risk/stop, final reconciliation | Rebuild around proposed decision, immutable policy revision, atomic admission, manifest-v2 authorized snapshot, shared Windows identity, absolute budget, exact Job containment, one-way quarantine, and target admission. |

Candidate `14a9b6b4cb9fc1e7248bd3b782b9e00d499181df` is superseded across all nine
surfaces: `cst.py`, `cst_saved_field.py`, `cst_saved_field_vendor.py`, `safety.py`,
`test_cst_saved_field_contract.py`, `test_cst_saved_field_integration.py`,
`test_cst_saved_field_vendor.py`, `test_servers.py`, and `test_stdio.py` under
`servers/electromagnetics-mcp`. Its pure resolver/vendor evidence may inform new
work, but no source/test is carried forward without reallocation to the owners above
and re-review. Claims 7 and 15 and Line10 semantics remain unchanged.

## Independent native comparison seam and Line10 acceptance

The production tool stops at sampled CST-native point values. A separate test-only
acceptance orchestrator consumes three inputs:

1. a VFEM-owned, machine-neutral point manifest naming exactly eight selected
   triangles with `triangle_id`, port, material, and `interior` or
   `interface-near`, plus six P2 and six degree-4 quadrature positions per triangle;
2. public `mcphub.cst.saved_field_sample.v1` responses; and
3. a `NativeFieldEvidenceV1` artifact created by an independently verified CST
   native-export path that does not call/import `GetFieldVector`, sampler modules,
   or the vendor activation adapter.

The orchestrator validates 96 triangle-local rows and 90 unique physical
coordinates per field. It issues four calls in fixed order: port 1 E, port 2 E,
then—only after both E comparisons pass—port 1 H and port 2 H. Each call contains
the 48 triangle-local points belonging to that port. It verifies the frame metadata
at 3 GHz, all required labels, point order, hashes, lifecycle settlement, and the
union counts. It then joins native/sampler rows by ID and exact coordinate after
the declared unit conversion.

Numeric acceptance is conservative exact equality of all six parsed finite binary
values after round-trip (`.17g`) native serialization, with identical field unit
and phasor metadata. There is no tolerance, interpolation, global scale, phase,
sign flip, or fitted transformation. Any mismatch, missing row, duplicate row,
incompatible zero mask, unit/convention mismatch, or unverified native producer is
FAIL. This may reject a numerically equivalent but differently rounded exporter;
that is an admitted false-negative tradeoff, not permission to invent a tolerance.

Zero rows remain `zero_ambiguous` in the MCP artifact even if the independent
artifact also reports zero; the comparator may record equality but may not relabel
them as FEM coefficients or physical zeros. The existing VFEM comparator owns any
later physical interpretation.

The native provider is deliberately unresolved in this design:
`ASCIIExport.SetPointFile` is not admitted because its installed input/selection
contract remains unverified (`<vfem-repo>/docs/tooling/mcp-hfss-cst-requirements.md:166`).
`ASSUMPTION (UNVERIFIED)`: an independent installed-CST provider can produce the
required point artifact without sharing the sampler path. Resolving probe: a
separate binary/selection smoke on the retained disposable copy, with producer
call trace, source hashes, and process settlement preserved under `/.scratch/`.
Until that probe passes, Line10 acceptance is mechanically FAIL, but the production
architecture remains fail-closed rather than selecting a false oracle.

Raw acceptance output remains under `/.scratch/`; the bounded deliverable contains
schemas, relative identities, hashes, counts, comparison verdicts, CST version,
and redacted process counts only—never the retained project, absolute paths, PIDs,
license data, or raw proprietary field payloads.

## Alternatives and tradeoffs

| Alternative | Benefit | Decisive rejection driver |
|---|---|---|
| Extend `cst_export_results(job_id)` | Fewer public names; reuse results import. | Coupled to in-memory jobs (`jobs.py:220`) and 1D CSV export (`cst.py:429`); cannot survive restart without changing its contract. |
|  |  | Rejected by the separate-tool/no-resolve requirement (`mcp-hfss-cst-requirements.md:44`). |
| Hub catalogue visibility or caller `authorized=true` | Reuses current routing/schema surfaces. | Not an authority boundary: bare-client/direct-daemon and gate-off paths bypass group visibility, while caller data is forgeable. |
| `CREATE_SUSPENDED` then `AssignProcessToJobObject` then `ResumeThread` | Works on older Windows and is conceptually explicit. | Requires primary-thread-handle ownership and more rollback points; official JOB_LIST assigns at creation on the admitted Windows baseline and is strictly smaller. |
| Validate/copy to an ordinary path immediately before CST and revalidate afterward | Minimal adapter change. | Authorization ends before the vendor lookup; a same-user deterministic name, ancestor swap, or in-place write can make CST consume attacker bytes before post-check. |
| Randomized names or restrictive directory DACL alone | Reduces accidental collision. | The admitted threat is a same-owner principal; naming secrecy and owner-equal access control do not preserve object identity across a path reopen. |
| Same-principal close/reopen or `FILE_SHARE_WRITE` so CST can save normally, then reacquire/hash | Maximizes path-API compatibility. | Reintroduces the exact capability discontinuity; identity/hash after consumption cannot authenticate which writer produced unknown header bytes. |
| Same-principal random name or tighter DACL | Small operational change. | The admitted writer has the same token/owner authority; name secrecy and an ACL granting CST's same principal do not distinguish writers. |
| Pass native handles directly to CST | Strongest and simplest object continuity. | The installed adapter surface currently exposes path-only `OpenResult3D`/`Save`/ResultTree operations. A handle-native installed API remains preferable if a future verified surface provides it. |
| Direct `New-CIPolicyRule -AppID -Level Hash` on installed ConfigCI 1.0 | Matches the advertised syntax. | Installed assembly `10.0.26100.4484` rejects enum `Hash=1` in its per-app guard and emits no rule; accepting empty output or changing level would erase the sole pre-entry boundary. |
| Silently upgrade ConfigCI, use the App Control Wizard, or hand-edit XML without an independent CIP parser | May avoid the installed defect. | No exact version/hash and reproducibility contract is proven, GUI output is not a deterministic build input, and XSD validity alone does not prove the compiled CIP preserved AppID/hash semantics. Unsupported versions remain default-off. |

The chosen fresh broker-owned worker trades repeated process/bundle/API-open cost for
source isolation, restart independence, hard duration ownership, and local reasoning.
The long-lived daemon remains free of CST session state. No latency claim is made;
optimization or a persistent pool requires a live profile and successor decision.

## Test strategy

| Gate / named probe | Required result |
|---|---|
| `test_saved_field_wire_schema_v1` | Closed nested schema; exact enums/bounds; `allow_solve` only false; existing six schemas unchanged. |
| `test_saved_field_framework_validation_boundary` | Unknown nested key and `allow_solve` true/null/non-boolean return FastMCP `isError=true` with only `cst_saved_field.invalid_request`, no sampler event, and no function entry. |
|  | The same malformed call to each existing tool retains its pre-change FastMCP error behavior. |
|  | Post-entry cross-field invalidity returns `cst_saved_field.invalid_request` and one diagnostic event. |
| `test_saved_field_frame_resolution_table` | Missing/ambiguous/exact selector/frequency-boundary/hash/pass cases produce the specified IDs; filesystem order and `#NNNN` do not affect choice. |
| `test_saved_field_component_order_and_zero_semantics` | Raw encoded row order is exact; no transform; exact six-zero vectors alone set `zero_ambiguous=true`; nonfinite/wrong-arity rows fail atomically. |
| `test_saved_field_unit_transform` | `m<->mm` scales exact; input coordinates unchanged; unsupported project unit returns `metadata_unavailable`. |
|  | Unsupported input unit is covered by the FastMCP pre-entry validation test. |
| `test_saved_field_complete_manifest_transfer` | For every ancillary and named project/mesh/field row, inject add/remove/rename/replacement/size/hash/metadata drift at enumerate, pre-open, read, copy, source-close, destination-enumerate, and pre-commit boundaries. |
|  | No CST/session call before complete equality; daemon source-call counter stays zero. |
| `test_saved_field_windows_path_identity_v1` | Across policy root/project, source/destination rows, vendor payload, clean payload, header, and registration, reject extra colon/ADS, trailing dot/space, reserved/DOS/NT/GLOBALROOT/volume/UNC/mapped, wildcard/control/non-NFC, tilde/8.3 alias, reparse, hard link, case/NFC collision, named stream, and unavailable alternate-name/identity proof. |
|  | Every lexical form fails before filesystem access; every metadata ambiguity fails before child content/CST. Ordinary canonical `C:\\allowed\\project.cst` passes the drive-colon grammar. |
| `test_saved_field_reserved_device_alias_properties` | Generate every path-producing/consuming role crossed with `COM`/`LPT`, mixed case, ASCII digits `1`..`9`, superscript digits `¹`/`²`/`³`, zero/one/multiple extensions, ADS suffixes, and trailing dot/space variants; each reserved equivalence-class row returns `path_namespace_invalid` with filesystem/worker/workspace/CST counters all zero. |
|  | Mutate the ordering of NFC, case-fold, extension split, trailing trim, and superscript mapping in a test double; any ordering that could accept an alias falsifies the contract. Positive properties retain the sole ordinary drive colon and admit canonical non-reserved `C:\\allowed\\project.cst` without rewriting its components. |
| `test_saved_field_local_nofollow_boundary` | Held-parent long/short-name enumeration plus final path/volume/file-ID/link/stream proof keeps every source/destination/generated role inside one local identity through deterministic swaps. |
| `test_saved_field_authority_policy_v1` | Missing/disabled/invalid policy omits tool; closed schema, byte/entry/path/hash bounds, owner/effective-access/locality/reparse checks, duplicate entries, and restart-only reload are exact. |
|  | Lexical miss makes zero filesystem calls; broker-worker alone proves source identity/destination equality before CST. |
| `test_saved_field_trusted_root_policy` | Injected Windows owner/effective-access matrix admits only owner, SYSTEM, and Builtin Administrators. |
|  | Missing/relative/remote/reparse/broadly accessible root fails before child creation; application/vendor modules do not read ambient config. |
| `test_saved_field_workspace_factory_transaction` | Inject failure at child create, permissions, identity check, initialization, and immediately before lease transfer. |
|  | Every created child is removed exactly once, unrelated siblings survive, and complete receipt fields reach `session_settled` without defaults. |
| `test_saved_field_budget_boundaries` | Exact-limit and one-over cases cover path scalars/UTF-8, depth, entries, files, per-file/aggregate bytes, candidates, aggregate/per-string metadata, points, response, and concurrency. |
|  | One active plus one 1.000-second waiter is exact; a second waiter/expiry fails before lexical/source/workspace/vendor; every post-acquisition limit failure settles before response. |
| `test_saved_field_admission_quarantine_linearization` | A deterministic barrier queues B behind active A. A latches quarantine and atomically revokes/releases; B acquires, rechecks latch/revision, returns `containment_quarantined`, releases cleanly, and leaves lexical/source/worker counters zero. |
|  | Repeat hub, bare-client, direct-daemon, and gate-off routes; separately mutate policy generation between acquire and `authorize_start` and require `policy_revision_changed` with zero work. |
| `test_saved_field_vendor_record_validation` | Per-field malformed/missing/wrong-type/overlong/nonfinite/enum/path/role/hash/pairing cases fail as `vendor_record_invalid` before selection/session. |
|  | Candidate and metadata aggregate overflow stops the bounded iterator without partial selection. |
| `test_saved_field_safe_error_redaction` | Canary values in caller data, vendor records, rejected path/stream/short names, file IDs, exception strings, environment, license text, security IDs, and raw statuses are absent from FastMCP errors, app failures, events, captured stderr/logging, and success. |
| `test_saved_field_vendor_call_order` | Fake surface records exact Result3D/header/ResultTree/sample/cache-clear/close order. |
|  | No solver/save/remesh method is called; only CST-generated header is used. |
| `test_saved_field_vendor_path_capability_continuity` | At copied payload/project open, generated-header save/seal, clean payload/header, ResultTree registration, selection/frequency, every point, cache clear, and session close, assert exact access/share table: read-only leaf=`GENERIC_READ/FILE_SHARE_READ`; ancestor omits delete share; write-capable project/header exists only under distinct vendor SID; sealed output uses share `0`. Deterministic in-place writes, leaf replacement, and every ancestor rename from daemon/interactive identity all fail before CST consumption; foreign siblings survive. |
|  | Inject failure/exception/timeout after each vendor return point: no later operation runs, no partial result escapes, every exact handle receives one owner attempt, and a fail-once close is retried once without worker/path fallback. Remaining failure makes `vendor_path_lease_settled=false`, returns `session_settle_failed`, and quarantines. |
|  | Target CST must run in the fresh isolated-principal worker, read locked known-hash inputs, write the unknown header solely inside its protected workspace, release the writer so the owner can acquire share-0 read/seal, and then accept locked clean inputs through ResultTree/sample settlement. Same-principal requirement, reusable external CST process, inaccessible license/COM, foreign writer, or inability to seal is FAIL with sampler still unregistered. |
| `test_saved_field_vendor_principal_isolation` | Provision only fixed credential-free virtual accounts `NT SERVICE\McpLocalHubCstDaemon` and `NT SERVICE\McpLocalHubCstVendorBroker`; prove SCM/LSA token injection, pinned images/session 0, exact service SIDs, protected SCM/pipe/workspace DACL/SACL, no application secret, and all-return termination/wait/handle/token/workspace settlement. Disable/delete/replace SID/rotate binary and require no stale registration/resource. |
| `test_saved_field_broker_pipe_authorization_v1` | Assert exact local single-instance pipe flags/name/rights/SACL; client verifies SCM server PID/token/image; server verifies SCM client PID then impersonates and compares exact user/service/logon SID/session/integrity before parsing; `RevertToSelf` succeeds in every path. Anonymous, Network, foreign SID, wrong session/PID/image, second daemon, malformed/trailing/second frame, stale policy/entry/manifest, expired/replayed/wrong nonce, disconnect, timeout, impersonation failure, and forced revert failure do zero privileged work or exact post-start settlement. Failed revert terminates broker/quarantines. |
| `test_cst_native_pe_loader_closure_v1` | Parse the candidate with both `dumpbin` and an independent PE parser: AMD64/custom entry, direct KERNEL32 exact-function allowlist, `/DEPENDENTLOADFLAG:0x800`, declared mitigations and safe sections; zero delay/TLS/CLR/bound-import/manifest-redirection/CRT/package imports. Disassemble entry through all role-required clear/readbacks and reject indirect calls, callbacks, initialization or child/DLL-load paths. Two clean unsigned builds match byte-for-byte and signed structures match the manifest. |
| `test_cst_native_preentry_target_probe_v1` | Pause both roles at entry RVA on the disposable target; enumerate mapped modules by held-file identity/hash/signer and require exact provisioned Microsoft System32 closure. Plant DLL/TLS/static-initializer canaries in application, cwd, PATH and user directories; none loads or executes. After the marker a child inherits none of the frontend four or worker five handles. |
| `test_cst_native_postrevocation_python_load_v1` | Mutate absolute CPython identity/hash/signature, transitive bundle DLLs, search flags, ABI exports and every ambient search directory. Only manifest-equal bundle/System32 modules loaded by absolute `LoadLibraryExW(DLL_LOAD_DIR|SYSTEM32)` pass after revocation; every failure produces `native_loader_invalid`, zero Python/source/vendor/CST/descendant work, complete handle/module/buffer settlement and quarantine. |
| `test_saved_field_owned_session_identity` | Interleave sampler identity 200 and foreign PID 300 after baseline 100; ambiguity closes only returned owned handle and never connects/closes 300. |
| `test_saved_field_partial_acquisition_transaction` | Inject failure after handle return, identity bind, liveness bind, complete-token validation, and immediately before transfer. |
|  | Each closes the exact local handle once, never touches interleaved foreign PID 300, reports `transfer_committed=false`, and emits one settlement event after workspace/hash attempts. |
|  | A raise-before-handle primitive must prove zero creation/direct rollback; absent proof yields `session_settle_failed` and target capability FAIL. |
| `test_saved_field_cleanup_all_paths` | Each entered pre-/post-transfer success/failure stage attempts owner-appropriate cleanup, emits one settled event, and leaves no safely attributed owned resource. |
| `test_saved_field_broker_worker_protocol_v1` | Exact native prelude/application frames, correlation/deadline, `WorkerPreMainBootstrapV1`/`WorkerPreMainReceiptV1`, and nested owner receipts; missing/Boolean/equal/standard/out-of-range locators, identity/role mismatch, checksum/EOF/trailing/stderr/overflow reject before CPython/source/CST. Application frames contain no handle locators. |
| `test_saved_field_worker_process_bootstrap_real_child` | A real direct Win32 `mcphub-cst-runtime.exe --role=worker` child receives no Python object or path/env/argv authority. Instrumented native marker proves the first executed application instruction clears/readbacks three standard handles and proves Job/no-console before stdin; the native prelude validates both capabilities, clears/readbacks them and returns `WorkerPreMainReceiptV1` before `Py_InitializeFromConfig`. Injected global/user site, `.pth`, `sitecustomize`, `usercustomize`, `PYTHON*`, registry and cwd canaries never execute; every PyConfig field/path mutation fails closed. Only after broker accepts the receipt does embedded Python perform a handle-relative synthetic transaction and return truthful receipts. Every failure has zero Python/source/vendor/CST/descendant count and quarantines. |
| `test_saved_field_windows_atomic_containment` | Broker receipt proves one exclusive inheritance epoch, atomic JOB_LIST plus exact ordered five-entry HANDLE_LIST, true duplicate flags/access then immediate parent closes before epoch release; concurrent broker child receives none. Worker proof/receipt proves all five flags clear, and a spawned descendant receives none. Breakaway proof and exact created-child cleanup are complete. |
|  | Inject every create failure and prove settlement with zero worker execution before membership. |
| `test_saved_field_absolute_deadline_all_stages` | Block broker/worker/source/vendor/encode/daemon validation/publication; unchanged QPC triple never resets. |
|  | Launch completes within its 5.000-second slice, success frame by deadline minus 2.000 seconds, publication by 60.000 seconds; any crossing terminates/settles and returns `deadline_exceeded`; after deadline source/vendor/success counters do not change. |
| `test_saved_field_createprocess_tuple` | Assert non-null exact signed/hash-pinned native `lpApplicationName`, fixed `"<canonical-runtime>" --role=worker`, pinned bundle cwd, closed environment, NULL security attributes, `bInheritHandles=TRUE`, `STARTF_USESTDHANDLES`, exact ordered five-handle allowlist, JOB_LIST/flags, live buffers and no shell/PATH/python command/breakaway. Field-by-field root-open/share/reparse and DuplicateHandle desired-access/options/inherit/effective-access mutation tests reject every changed bit and unavailable readback. |
| `test_cst_direct_frontend_launch_profile_v1` | Provision/materialize `launch_profile: cst-direct-v1` to one absolute canonical signed/hash-pinned runtime image and fixed `--role=frontend`; StdioHost direct child PID/image/package/parent equals enrollment and no wrapper exists. Extend existing lifecycle and handle-list tests: before apply, `AdditionalInheritedHandles` is empty and `NoInheritHandles=false`, Token/ParentProcess/ProcessAttributes/ThreadAttributes are zero/nil; after apply it contains exactly the capability read handle once and every mutation/prepopulation rejects. A Windows real-child probe plus installed Go 1.26.5 source-bound oracle proves `ProcAttr.Files[0:3]` followed by the singleton additional handle becomes `_PROC_THREAD_ATTRIBUTE_HANDLE_LIST` and the child observes only those four roles. `prepareLaunchCapability` remains sole enrollment owner and every exit terminalizes; no raw frontend process creator or duplicate owner exists. |
| `test_cst_existing_six_direct_bootstrap_compatibility` | With sampler disabled, A/B legacy `uvx` baseline and direct bootstrap use the same pinned `cst.py`; exact existing-six catalogue, signatures, request/response bytes, validation/error bodies, stdin/stdout ordering, prompts/resources and shutdown behavior match. Only after this gate may provisioning remove `uvx` as a CST runtime dependency. |
| `test_cst_package_load_closure_v1` | Independent PE parser emits the exact recursive CPython, extension/package normal/delay rows and target System32/API-set rows. P18 binds exact hashes to one enforced per-app UMCI policy and one read-only VHDX; reject audit/broad/default/AllowAll/reputation/managed-installer fallback, missing/extra/ambiguous/runtime-discovered modules, policy composition drift, dependency drift and alternate roots before execution. `ProvisionedPackageIdentityV1` binds closure, policy and VHDX hashes plus host identities; reinstall changes only that host receipt. |
| `test_cst_exact_appid_policy_artifact_v1` | Pin Windows/PowerShell/ConfigCI assembly/XSD/converter grammar; ordinary Hash authoring for the runtime and at least one package and System32 PE yields exactly four UMCI hashes matching the independent PE implementation. Runtime `OriginalFilename`, independent version-resource parse and working FileName-per-app oracle agree. Canonical XML has only unscoped runtime hash rows and module hash rows with exactly one AppID; independent XSD semantic parse and separate CIP-binary parse reconstruct identical algorithms/hashes/cardinality/references/PolicyID/BasePolicyID/version/options/signer metadata. Two unsigned builds are byte-identical; independently verified signed-envelope content equals the unsigned CIP. Mutate every pin, hash, algorithm, AppID, scope, rule kind, ID/ref, cardinality, base/options/signer field and signed content; each returns `policy_artifact_unavailable`, makes zero deployment calls, and leaves registration absent. |
| `test_cst_policy_signing_owner_v3` | Target contains no signing key/provider/credential/group/service/endpoint. Pin nShield firmware/software, key/OCS/KSP/SignTool/Transaction ID and one 5c privileged collector. Exact `nshieldaudit ... export --verify` owns KML verification; P18 verifies its binary/argv/config/database/exit/held-JSON identity, continuity and exact `{runID,objid}` ObjectNew-key-hash to successful Sign/ObjectUse join, then independently verifies held signed-CIP hash, PKCS #7, content, chain and online-v1. Dual/unprivileged collector, cached/text/malformed output, wrong ObjectNew, raw-format claim, leak or cleanup/rotation mutation yields zero deployment. |
| `test_cst_audit_receipt_replay_journal_v3` | Seed reviewed collector/genesis attestation, then exercise first vendor-verified continuous receipt, strictly-greater request, gaps, duplicate/reorder/lower/reset/reinit, concurrent exports/imports, collector restart/config/database/KNETI/content-key/audit-key rotation, and crashes around export flush/prepared/CiTool/completed flush. Only exact prepared recovery may settle and exact completed request/receipt/artifact equality returns stored result with zero CiTool. |
| `test_cst_signed_policy_import_owner_v1` | Deterministically pause after media verification, after protected copy, after read-only attach and while CiTool has the pathname. Attempt media/leaf/ancestor removal, rename, reparse, hard-link, ADS, in-place write, valid-policy substitution, VHDX detach/remount and backing replacement. The retained read-only leaf identity/hash must be the one revalidated after CiTool; otherwise there is zero call or an unsettled quarantine. A target on which CiTool cannot read while the stated handles and read-only attach exist is incompatible with no close/reopen fallback. |
| `test_cst_appcontrol_policy_set_lifecycle_v1` | Seed base, supplemental, deny, audit, AllowAll, signed, external-authority, stale-file, pending-reboot and policy-limit siblings. Under a protected mutex and durable journal, verify complete inventory; selected-base-family union admits no unlisted runtime image, every other-base intersection admits all exact rows, pre/post unrelated canaries are identical, only the sampler supplemental is written, update/remove are idempotent, and no committed/absent event exists before an observed reboot plus exact post-boot inventory. Inject crash/cancel/timeout/drift at every stage; sampler stays unregistered and no sibling/VHDX is changed. |
| `test_cst_package_dynamic_load_policy_v1` | For the exact runtime AppID under the exact `AppControlPolicySetCommittedV1` receipt, malicious admitted TLS, `DllMain`, CPython, extension and callback paths invoke direct/forwarded/indirect/absolute/native loaders for an unlisted package DLL and unlisted Microsoft System32 DLL. Code Integrity denies each before its first-instruction canary and records the exact policy/rule verdict; allowed rows still load. Unsupported edition, ambiguous event, broader base-family union, restrictive other-base intersection, audit-only result or any canary execution keeps sampler unregistered. |
| `test_cst_package_load_lock_window_v1` | Retain the VHDX/backing-volume/mount-point/ancestor/version/file chain with exact no-follow DELETE-denying tuples through an engineered slow load/`DllMain` window. Preexisting writers fail acquisition; read-only attachment makes file/metadata/named-stream writes fail. From administrator, SYSTEM, runtime/service and foreign principals attempt backing-file or mount replacement, detach/remount, ancestor/version rename, hard-link/reparse/junction substitution and ADS create/write/rename/delete. Every attempt fails before package instruction, all stream enumerations report exactly `::$DATA`, admitted mapping succeeds, and mapped identity equals retained rows. |
| `test_cst_package_load_settlement_v1` | Inject failure/cancel/timeout at policy-set commit, virtual-disk/volume/mount/ancestor/file acquire, load/DllMain return, symbol/PyConfig/init/application/finalize/mapped-settle and every close. Runtime reverse-closes exact owned objects once or returns `native_loader_settle_failed`, leaves the image read-only attached and quarantines. Only after stopped P18 proves zero process/Job/mapping/handle references and `AppControlPolicySetAbsentV1` may it detach and atomically select a complete reviewed VHDX; every failed remove/reboot/absence/detach/rollback remains quarantined with no weaker retry. |
|  | Mutate tuple/decoys/handles; creation fails before worker work. |
| `test_saved_field_timeout_settlement` | Block each worker/vendor stage; broker settles exact Job by cleanup deadline. |
| `test_saved_field_normal_residual_routes_termination` | Worker residual descendant triggers exact broker Job termination. |
| `test_saved_field_worker_reference_order` | Worker signal -> exit -> reference close -> Job zero -> reader joins -> Job close. |
| `test_saved_field_reader_cancellation` | Ordinary contained/escaped pipe writers and blocked readers require exact `CancelSynchronousIo`/`CancelIoEx`, read-end close, bounded join, and zero proof; missing proof quarantines. |
| `test_saved_field_quarantine_all_routes` | Fail broker/worker receipt fields; gate quarantines and all routes do zero source/broker work until restart. |
| `test_saved_field_shutdown_and_restart` | Daemon shutdown closes pipe/requests settlement but never Job; broker shutdown/crash alone closes Job and kills worker tree. |
| `test_saved_field_mcp_result_boundary` | Capture actual FastMCP 1.29.0 `CallToolResult`; exactly one `TextContent`, no structured duplicate. |
|  | Exponents, Unicode IDs, and 256 rows prove cap against final text UTF-8 bytes; overflow returns `response_too_large`. |
| `test_saved_field_restart_replay` | Two fresh instances over identical inputs select the same frame and return semantically identical output. |
|  | No prior-call resource/cache/vendor state participates; injected composition-owned admission gate starts available in each fresh instance. |
| Existing `pytest` suite | Every existing test passes; exact CST inventory changes only from three to four names; all existing tool schemas/fixture outputs remain unchanged. |
| Real stdio handshake | Catalogue contains the three unchanged HFSS tools and four CST tools with the exact new schema; prompts/resources remain unchanged unless separately admitted. |
| Target CST activation smoke | Version, exclusive owned-session/process identity, header, call order, units/phasor, status, hashes, full vendor-record validation, and settlement pass on a copy. |
|  | Trace every installed-API pre-transfer failure point and prove direct-handle rollback or zero creation without PID attachment. |
|  | No solver is launched. |
| Target Windows containment smoke | First worker instruction proves Job membership; every installed CST descendant is a member; active-process cap/breakaway/no-console/stream limits and exact launch tuple hold. |
|  | Timeout, normal-exit-with-live-descendant, blocked-reader, quarantine, and broker-crash traces prove signal/handle-close/active-zero order and complete owned-tree disappearance while an interleaved foreign CST handle remains live and never belonged to the Job. |
| Line10 acceptance oracle | Four calls cover two ports, E then H, 96 local/90 unique rows per field, both materials/classes. |
|  | Exact independent equality, unchanged source hashes, and zero owned-process remainder; any unmet clause is FAIL. |
| Independent Claim-Verify architecture review | After both blockers are resolved and the design is revised, maps every numbered claim 1:1 and returns PASS before replanning/implementation. |

No solver run, bundle mutation, live process operation, or test execution belongs to
this architecture lane. The target CST and Line10 probes are later explicit QA
gates, not facts inferred from unit tests.

## Diff-invisible invariants

| Pre-existing coupling at risk | Named regression guard and expected result |
|---|---|
| Existing registration and closed schemas | **Existing-wire compatibility guard:** before/after schemas are semantically equal. |
|  | Policy absent/invalid/disabled keeps exact six-tool inventory; enabled valid policy adds only `cst_sample_saved_field`. |
| Solve copy dependency is injected; solve open is not | **Solve-path preservation guard:** existing solve tests pass. |
|  | No changed statement in existing tool bodies/`_runner`; static scan rejects sampler use of `_open_owned_project`. |
| FastMCP validates before entry | **Validation-channel guard:** malformed/type/literal sampler cases produce only fixed safe framework error and no sampler event/entry. |
|  | Existing tool validation behavior is byte/shape compatible; cross-field sampler semantics enter application and produce the registered stable failure ID/event. |
| `JobManager` owns process-local jobs | **No-job-edge guard:** new modules do not import `jobs.py`, call `_jobs`, accept `job_id`, or alter exporters. |
| CST process list is shared mutable state | **Foreign-process guard:** vendor-returned owned identity closes; interleaved foreign PID remains untouched. |
|  | Static scan rejects connect/close authority derived from snapshot/set difference in acquisition and session paths. |
|  | Live probe checks exact-handle rollback and attributed identity liveness at every pre-transfer failure point. |
| Source/result files can be mutable | **Complete-manifest transfer guard:** fakes fail any write/open-for-write/save under source root; every policy row is copied from its exact stable handle and the complete destination manifest equals policy before CST. |
|  | Ancillary add/remove/mutate/rename, ADS, alias, hard-link, reparse, identity, count, or hash drift fails before vendor start; three source post-hashes remain explicit provenance only. |
| Output-root configuration is ambient | **Trusted-root capability guard:** only SCM broker composition reads the configured root and opens the protected workspace; frontend/daemon/safety/app/vendor receive no ambient root and worker receives only the inherited workspace handle plus sealed identity/role locator. |
|  | Owner/effective-access/locality/reparse failures occur before child creation. |
| Workspace creation has pre-transfer failure windows | **Workspace-transaction guard:** exact child remains factory-owned until complete lease transfer. |
|  | Every injected failure removes that child, preserves siblings, and echoes full receipt into sole settlement event. |
| Application and adapter risk circular ownership | **Neutral-port guard:** application and vendor import only `cst_saved_field_port`; application never imports concrete vendor. |
|  | Import-graph test rejects inward dependency reversal or CST objects in the neutral contract. |
| Vendor output is untrusted | **Vendor-record guard:** every raw field, candidate path under `WindowsPathIdentityV1`, and aggregate budget is validated before selection/session. |
|  | Missing/malformed/nonfinite/escaping/ADS/alias/hard-link/stream/over-budget records fail atomically without exception text leakage. |
| Work can grow before sampling | **Finite-budget guard:** exact/one-over tests cover every declared role, full source/destination manifest passes, the absolute invocation/sub-budgets, and one-active/one-waiter admission. |
|  | No caller can raise a limit; all source I/O occurs in the broker-owned worker under the same deadline, and all entered failures settle before a stable error. |
| ResultTree/header are session state | **Settlement-order guard:** success is unreachable before cache clear, owned close/liveness, removal, and post-hash. |
|  | Before transfer the armed acquisition transaction must settle exact local handles; after transfer only `SamplerSession` closes. Its vendor-path lease stays live through cache/session settlement, then closes before snapshot/workspace settlement. |
|  | Exactly one worker `session_settled` and one daemon `request_settled` fire in order; client disconnect cannot abandon either owner. |
| FastMCP call can outlive hub observation | **Contained-duration guard:** the absolute 60 seconds start before descriptor/launch/source work and cover final publication; after deadline only fixed error construction and settlement execute. |
|  | Every timeout returns only after signal/exit, worker-reference close, active zero, reader joins, and required handle closes, or returns `containment_settle_failed` and latches quarantine. Target trace preserves an interleaved foreign CST process. |
| Hub visibility has bypass paths | **In-server authority guard:** one atomic acquire/seal/pre-work gate owns every route; policy absent/invalid/quarantined/revision-stale denies before lexical/source/worker, and contained complete-manifest-v2 mismatch denies before CST. |
|  | Caller inputs never confer authority; immutable revision/entry plus complete policy-equal destination owns every allow decision. |
| Broker boundaries duplicate contracts | **Protocol drift guard:** each boundary imports its one neutral schema; mismatch fails publication. |
|  | Candidate/application receipts are equality-checked into `request_settled`. |
| Child launch can leak console/handles/processes | **Atomic-inheritance guard:** one exclusive broker epoch spans creation of all inheritable handles through exact non-shell `CreateProcessW`, immediate parent-copy close and epoch release; JOB_LIST, ordered five-entry HANDLE_LIST, root open/share/reparse and duplicate access/inherit readbacks are asserted field-by-field. Worker clears/readbacks all five flags before any descendant-capable work. |
|  | Concurrent broker-child and worker-descendant probes must inherit none; any failed/unavailable clear/readback produces zero source/vendor/CST/descendant work and quarantine. |
| Windows loader can execute DLL/TLS/static initialization before executable entry | **Pre-entry loader-closure guard:** build/provisioning admit only the no-CRT KERNEL32-only PE; independent PE scan, entry-RVA disassembly and target mapped-module manifest prove no package/user code or callback precedes every required clear/readback. |
| Windows loader can execute package DLL/TLS code before `LoadLibraryExW` returns | **Package code-admission and namespace guard:** exact-runtime enforced per-app UMCI is the sole deny-before-entry owner; a read-only VHDX plus retained backing/volume/mount/ancestor/version/file chain owns pathname/content continuity, while static PE closure and mapped inventory remain inventory/settlement evidence. |
|  | Malicious TLS/DllMain/CPython/extension/callback loads must be denied before first instruction. A deterministic slow-callback matrix separately proves preexisting-writer and detach/remount/write/delete/rename/hardlink/reparse/ADS denial, exact stream enumeration/mapped identity, and reverse cleanup/quarantine without weakened policy or mount retry. |
| A decoded signed CIP can contain expected bytes without authenticating the intended signer, and OCS authority can outlive one approved operation | **Policy-signing authenticity and loaded-interval guard:** one `CstPolicySigningOwner` proves the exact key and fixed envelope; final-card removal plus exact-module all-local-slot `slotinfo` absence proves non-persistent OCS unload before a continuous vendor-verified range may settle; the independent verifier still requires exact signature/content/current chain/revocation before CiTool. |
|  | Second-process/thread/provider Sign, different/absent Transaction ID, extra/ambiguous Sign or exact-key ObjectUse, retained/stuck/reinserted card, nonzero insertion counter, provider leak, cancel/timeout/crash/restart, and rotation-during-loaded-state all yield zero handoff/deployment and durable quarantine. |
|  | **Settlement-order guard:** worker signal/exit precedes reference close then Job zero/readers/Job close. |
| Broker crash can leave worker/CST alive | **Sole-Job-handle guard:** broker alone owns Job; worker HANDLE_LIST excludes it; crash trace proves tree death/foreign preservation. |
| Failed containment can poison later calls | **Quarantine linearization guard:** under one owner, failed settlement latches, revokes the active lease, releases, and wakes; every queued/future caller must recheck after acquire and immediately before work. |
|  | Deterministic paused-waiter schedule returns `containment_quarantined` with lexical/source/worker counters zero across all routes until restart/revalidation. |
| Windows aliases can bypass path rows | **Namespace identity guard:** one shared grammar and exact long-name/final-path/volume-file-ID/link/default-stream proof applies to policy, walkers, candidates, clean payload/header, and registration; ordinary drive colon passes and every ambiguous alias fails before CST. |
| Path-only CST APIs can reopen or mutate an object | **Vendor-byte capability-continuity guard:** read-only inputs omit write/delete sharing, while all write-capable project/header work occurs under a distinct broker principal and protected DACL; unknown output is sealed share-0 before becoming a known-hash input. Deterministic in-place writes/leaf/ancestor swaps fail and no same-principal fallback exists. |
| Canonical text is published behavior | **MCP-boundary budget guard:** capture actual `CallToolResult`; one text item, no structured duplicate. |
|  | Final `TextContent.text` UTF-8 bytes alone determine pass/fail at 1,048,576. |
| Error/event surfaces can leak untrusted data | **Canary-redaction guard:** injected canaries are absent across FastMCP, application, diagnostics, stderr/log capture, and success. |
| Manifest pin controls live code | **Publication guard:** prior pin remains until human approval; the new pin binds the reviewed native bundle manifest, exact `cst-direct-v1` descriptor and signed/hash-pinned frontend/worker images. Existing-six A/B byte/stdio compatibility, direct-child identity, uvx absence and rollback polarity all pass before migration. |
|  | No live fleet operation is part of implementation verification. |

## Numbered architecture claims

1. `{ guarantee: Existing six tool signatures schemas behavior outputs and stdio bytes remain unchanged across the protected uvx-to-direct-bootstrap launcher migration and the only wire addition is cst_sample_saved_field, single-owner: CST composition root registration plus cst-direct-v1 materialization seam, enforcement-probe: existing-wire A/B compatibility guard plus full existing pytest and stdio inventory tests }`.
2. `{ guarantee: Every success is derived from the request's retained bundle without JobManager or prior-call state, single-owner: SavedField application owner, enforcement-probe: test_saved_field_restart_replay and no-job-edge import/call-graph scan }`.
3. `{ guarantee: allow_solve=true is rejected before sampler entry/source access with only the fixed safe FastMCP error and no sampler layer can invoke solve/remesh, single-owner: sampler-specific FastMCP composition policy, enforcement-probe: framework-validation boundary test plus existing-tool compatibility and forbidden-symbol/import guards for run_solver save(include_results=True) adaptation and _jobs }`.
4. `{ guarantee: Daemon supplies no source path bytes or handle and broker alone converts policy into exact source/workspace capability tuples while an admitted no-CRT KERNEL32-only worker clears and reads back all five inheritance flags before package/user code, CPython or descendant-capable work, single-owner: broker WindowsContainedInvocation followed by native PE loader-closure/revocation and AuthorizedBundleTransfer, enforcement-probe: PE import-delay-TLS scan entry disassembly hostile-DLL target trace native marker concurrent-child descendant exact-HANDLE_LIST manifest and byte-continuity matrices }`.
5. `{ guarantee: Source .cst Result/3d.slim and selected .sct hashes are equal before/after every success, single-owner: SourceSnapshot, enforcement-probe: deliberate per-role mutation cases return cst_saved_field.source_changed and suppress success }`.
6. `{ guarantee: Exactly one frame is selected by declared metadata and optional exact selector with no filename-frequency inference or fallback, single-owner: FrameResolver, enforcement-probe: test_saved_field_frame_resolution_table including permuted candidate order and ambiguous #NNNN cases }`.
7. `{ guarantee: Activation uses Result3D plus an isolation-principal-only CST-generated header sealed share-0 into a known-hash read-only input plus locked clean payload/header and ResultTree registration/selection/frequency recheck before sampling, single-owner: CST saved-field vendor port, enforcement-probe: vendor call-order principal-isolation byte-continuity tests and target CST isolated activation trace }`.
8. `{ guarantee: Returned rows preserve request order and components are ReX ReY ReZ ImX ImY ImZ sampled field values rather than FEM coefficients, single-owner: SavedFieldResponseV1, enforcement-probe: raw-wire order test and fixed value_kind/fem_basis_coefficients assertions }`.
9. `{ guarantee: An exact six-component zero is always labeled zero_ambiguous and never physical_zero, single-owner: SavedFieldResponseV1 zero classifier, enforcement-probe: signed-zero/nonzero table in test_saved_field_component_order_and_zero_semantics }`.
10. `{ guarantee: One coordinate-unit contract admits only m/mm and applies one explicit exact scale while unsupported input fails pre-entry and unsupported project metadata fails post-entry, single-owner: SavedField coordinate-unit contract generating the wire enum and UnitTransform, enforcement-probe: test_saved_field_framework_validation_boundary plus test_saved_field_unit_transform }`.
11. `{ guarantee: Close authority comes only from one vendor-returned owned session/process identity and no snapshot-discovered foreign CST/MCP process is connected closed or required to disappear, single-owner: OwnedSamplerSession vendor contract, enforcement-probe: test_saved_field_owned_session_identity plus foreign-process guard and target handle-liveness trace }`.
12. `{ guarantee: Every normal or exceptional acquisition and vendor-path-lease exit returns a complete normalized receipt whose exact fields are consumed without defaults and echoed in the sole session_settled event after all safe settlement attempts, single-owner: Saved-field application CallSettlement aggregate, enforcement-probe: test_saved_field_partial_acquisition_transaction workspace factory transaction and vendor-path capability-continuity receipt equality on every injected exit }`.
13. `{ guarantee: Published success is finite machine-neutral contains no absolute paths PIDs or untrusted diagnostic values and its sole TextContent.text is at most 1048576 UTF-8 bytes with structuredContent absent, single-owner: composition-root MCP result publisher, enforcement-probe: test_saved_field_mcp_result_boundary plus MCP-boundary budget canary-redaction and publication-safety guards }`.
14. `{ guarantee: Line10-specific triangle selection and independent comparison remain outside installed production modules, single-owner: test-only acceptance comparator boundary, enforcement-probe: dependency graph shows no production import/string/config edge to Line10 or VFEM and acceptance comparator imports public schemas only }`.
15. `{ guarantee: Line10 cannot PASS without an independently verified native producer and exact six-component agreement for both fields ports and materials, single-owner: Line10 acceptance comparator, enforcement-probe: provider-independence call trace plus four-call 96-local/90-unique mechanical acceptance report }`.
16. `{ guarantee: Existing six tools retain the same cst.py/stdin/stdout contracts through one direct signed no-CRT frontend whose pre-entry module closure is exact; only the seventh crosses enrollment frontend daemon broker and contained-worker boundaries with no parallel route, single-owner: cst-direct-v1 materializer/native loader-closure provisioner plus StdioHost cst.py daemon and broker composition, enforcement-probe: PE closure target pre-entry modules existing-six uvx/direct A/B exact-child topology cancellation crash handle-leak and stale-route scan }`.
17. `{ guarantee: Until exception-safe complete-token transfer open_owned_sampler_session owns every factory-local vendor resource and closes each exact returned handle once on every earlier error without PID-derived attachment, single-owner: open_owned_sampler_session acquisition transaction, enforcement-probe: test_saved_field_partial_acquisition_transaction at every primitive boundary including raise-before-handle proof }`.
18. `{ guarantee: Application contracts never import the concrete CST adapter and concrete vendor code depends inward on the neutral port only, single-owner: cst_saved_field_port neutral contract, enforcement-probe: neutral-port import-graph guard and CST-object-free protocol surface test }`.
19. `{ guarantee: Workspace creation either transfers one fully initialized policy-compliant lease or removes the exact partially created child and preserves every sibling, single-owner: create_workspace_lease transaction, enforcement-probe: test_saved_field_workspace_factory_transaction at create permission identity initialization and transfer boundaries }`.
20. `{ guarantee: Source inventory hashing and copying use exact stable no-follow handles for every manifest row while raw resolved reparse mapped-drive hard-link stream alias and swap cases cannot supply content, single-owner: AuthorizedBundleTransfer, enforcement-probe: test_saved_field_complete_manifest_transfer plus test_saved_field_windows_path_identity_v1 across every regular-file role }`.
21. `{ guarantee: Every policy request tree file byte candidate metadata point worker stream process response concurrency and duration role has an exact finite non-caller-raiseable ceiling and one absolute 60-second budget begins in the SCM daemon before descriptor launch or source I/O then crosses broker worker daemon result and frontend publication unchanged with no source vendor or success work after expiry, single-owner: SCM-daemon AbsoluteInvocationBudget, enforcement-probe: exact-limit one-over altered-triple and all-stage-delay tests }`.
22. `{ guarantee: No raw vendor record participates in selection acquisition activation or allocation until every declared type length finite enum containment pairing hash and aggregate constraint passes, single-owner: CST saved-field vendor adapter, enforcement-probe: test_saved_field_vendor_record_validation malformed-field and bounded-iterator matrices }`.
23. `{ guarantee: Policy absent disabled malformed unsupported owner-access-invalid or revision-stale at frontend daemon or broker leaves the sampler unregistered while non-authoritative entry_id resolves exactly one unique daemon policy entry and broker independently authorizes its root project manifest; missing duplicate swapped mismatch or implicit-default selection performs zero broker source worker or CST work, single-owner: SCM-daemon AuthoritySnapshot resolution followed by broker authorization, enforcement-probe: one/two-entry valid swapped unknown duplicate mismatch stale-revision and zero-work matrices }`.
24. `{ guarantee: Every bundle/PE-loader identity policy request/OCS-loaded-interval/vendor-audit/signature/online-revocation/replay-journal/staging-handoff/policy-set/VHDX/namespace/load/callback/settlement failure performs zero deploy and no ceremony lock releases after load until final-card removal all-slot absence and post-unload audit settlement are proved, single-owner: offline CstPolicySigningOwner then NShieldAuditCollectorOwner P18 AuditReceiptReplayOwner SignedPolicyImportOwner CstAppControlPolicySetOwner broker WindowsContainedInvocation and NativePackageLoadOwner, enforcement-probe: every-return wrong-request/OCS/key/card-stuck/slot-readback/provider-handle/collector/export/transaction/sibling-Sign/ObjectUse/logID/index/gap/restart/leak plus policy/VHDX/load/containment matrices }`.
25. `{ guarantee: Every disposable workspace is created beneath one injected local non-reparse Windows root with the exact configured owner and effective access policy before source bytes are copied, single-owner: TrustedWorkspacePolicy, enforcement-probe: test_saved_field_trusted_root_policy Windows owner access locality and reparse matrix }`.
26. `{ guarantee: Every path-only read input remains a canonical known-hash object held GENERIC_READ with FILE_SHARE_READ only through lazy reads while every write-capable project/header is confined to the distinct-principal protected vendor workspace and unknown header becomes input only after share-0 sealing, single-owner: AuthorizedWorkspaceSnapshot AuthorizedVendorPathLease and Windows vendor-isolation owner, enforcement-probe: manifest path-identity principal-isolation in-place-write ancestor-swap output-seal and fail-once-close matrices }`.
27. `{ guarantee: Worker creation is atomic Job containment of the admitted signed no-CRT runtime with only the ordered five handles during one epoch and no package/user pre-entry code, concurrent broker child or descendant can retain them after clear/readback before hardened CPython load, single-owner: broker WorkerInheritanceEpoch followed by native PE loader-closure/revocation gate, enforcement-probe: five-handle tuple PE/TLS scan entry disassembly pre-entry canaries concurrent child descendant access-after-close escaped-probe and target trace }`.
28. `{ guarantee: Enrollment frontend broker and broker-worker protocols each exchange one bounded canonical correlation-bound sequence including a fixed native pre-main prelude before the application frame while each of exactly three named pipes has its owner-local channel receipt; daemon admission release depends only on BrokerExchangeReceiptV1 plus DaemonResponseReceiptV1 and frontend publication only on FrontendTransportReceiptV1, single-owner: four protocol owners plus native bootstrap followed by three named-pipe receipt owners, enforcement-probe: enrollment/status authorization native-prelude ordering plus partial flush ACK EOF cancel trailing correlation local-close and nested receipt matrices on all three pipes }`.
29. `{ guarantee: Existing T02 HostConfig.LaunchCapability StdioHost exec.Cmd and platform pipe remain the sole hub launch lifecycle; installed Go serializes three standards plus one capability into the ordered four-handle list while parent admission binds the exact no-CRT PE closure, single-owner: existing T02 exec.Cmd owner plus cst-direct-v1 materializer/native provisioner and FrontendChallengeLedger, enforcement-probe: lifecycle returns SysProcAttr mutation installed-Go HANDLE_LIST held-image PE receipt real-child four-role pre-entry canary wrapper absence duplicate-owner replay cancel zeroization matrix }`.
30. `{ guarantee: The isolated offline CstPolicySigningOwner is the sole content-key-use path and one non-persistent OCS-loaded interval cannot complete until final-card removal every admitted local slot absent and a vendor-verified pre-load-to-post-unload range contains exactly one intended Transaction-ID-equal Sign/ObjectUse with zero sibling or ambiguous key use, single-owner: CstPolicySigningOwner then NShieldAuditCollectorOwner and P18 AuditReceiptReplayOwner, enforcement-probe: second-process/thread/provider Sign different/absent/duplicate Transaction ID retained/reinserted/stuck card remote/dynamic slot provider leak extra/ambiguous Sign/ObjectUse gap/crash/restart/rotation and target direct-sign denial matrices }`.
31. `{ guarantee: SCM-daemon admission seals/rechecks lease and failed broker or daemon-local response settlement quarantines before exactly-once release while frontend-local EOF/close independently gates publication, single-owner: SamplerAdmissionGate plus DaemonResponseReceiptV1 and FrontendTransportReceiptV1 owner-local boundaries, enforcement-probe: event-order admission linearization partial-response ACK EOF local-close quarantine and restart trace }`.
32. `{ guarantee: Source/vendor work begins only after native revocation and an artifact whose OCS ceremony has final-card/all-slot unload proof plus a continuous pre-load-to-post-unload vendor-verified range containing exactly the one intended key use durable replay independent signature/content/current-chain/online-v1 verification immutable staging and reboot-committed policy, single-owner: CstPolicySigningOwner collector P18 replay/import/policy owners then broker runtime owners, enforcement-probe: retained/stuck card provider leak sibling Sign/ObjectUse dual collector/export substitution/ObjectNew/gap/crash/restart and staging-swap/CiTool zero-source matrices }`.
33. `{ guarantee: Before CPython/CST only the exact verified signed-CIP whose intended Transaction ID is equal and sole exact-key Sign/ObjectUse is enclosed by proved pre-load/post-unload audit anchors and final-card/all-slot absence may enter P18 CiTool/reboot settlement while retained VHDX identities deny unlisted images before instruction, single-owner: native PE revocation then CstPolicySigningOwner NShieldAuditCollectorOwner P18 replay/import/policy and VHDX/NativePackageLoadOwner, enforcement-probe: PE pre-entry card-retained/unload/readback/direct-sign/audit-continuity sibling/ambiguous Sign/ObjectUse/ObjectNew/signer/revocation staging/VHDX mutation malicious-load and settlement matrices }`.
34. `{ guarantee: One Windows grammar and exact identity proof covers policy transfer isolated-vendor candidates clean inputs generated header and registration while read-only objects additionally deny write/delete sharing and write-capable objects remain under the distinct vendor SID until sealed, single-owner: WindowsPathIdentityV1 plus AuthorizedVendorPathLease, enforcement-probe: path-identity reserved-device principal-isolation in-place-write stream alias ancestor-swap and unavailable-proof matrices }`.

## Final review finding disposition

| Finding | Disposition | Updated single owner and falsifying probe |
|---|---|---|
| F1 | Corrected in design: all normal/error acquisition and workspace receipts are normalized, consumed, and echoed without defaults. | Application `CallSettlement`; injected-exit receipt equality tests. |
| F2 | Corrected in design: neutral/app-owned port breaks the application-to-concrete-vendor edge. | `cst_saved_field_port.py`; import-graph/CST-object-free contract guard. |
| F3 | Corrected in design: workspace factory owns its child until complete lease transfer and rolls back every partial initialization. | `create_workspace_lease`; per-step failure and sibling-preservation matrix. |
| F4 | No design change: missing implementation oracle coverage is an implementation/QA correction against Claims 7 and 15. | Planner/implementer must restore the named target and independent-oracle gates. |
| F5 | No design change: repository README/catalogue update remains Plan Phase 11 after admissibility. | Phase-11 documentation diff and inventory test. |
| F6 | No design change: artifact echo is an implementation correction against the existing settlement contract. | Receipt-field equality tests named by F1. |
| AR-CONT-01 | Corrected: unchanged QPC deadline covers broker/worker/source/vendor/publication. | All-stage block and zero daemon-source probes. |
| AR-CONT-02 | Corrected: worker signal/exit/reference-close/Job-zero/readers/Job-close order. | `test_saved_field_worker_reference_order`. |
| AR-CONT-03 | Corrected: normal residual activity immediately terminates; all timeout/protocol/crash/shutdown/exception/reader-stall paths share one cancellation/termination/settlement state machine. | `WindowsContainedInvocation`; residual-normal, reader-cancellation, and sibling-path matrices. |
| AR-CONT-04 | Corrected: the complete `CreateProcessW` tuple, exact inheritable allowlist, buffer lifetime, no shell/search/breakaway, and standard-handle flags are contractual. | `WindowsContainedInvocation`; `test_saved_field_createprocess_tuple`. |
| AR-CONT-05 | Corrected: Claim 16 covers daemon pipe/admission plus broker Job-contained worker/CST resources. | Claim 16 topology/lifecycle guards. |
| AR-CONT-06 / Claim 31 | Corrected: one atomic admission owner performs post-acquire and immediate pre-work latch/revision/generation checks; quarantine atomically latches, revokes, releases, then wakes. | `SamplerAdmissionGate`; deterministic paused-waiter all-route linearization probe. |
| AR-CONT-07 | Corrected under the Lead-authorized stage separation: independent architecture Claim-Verify PASS plus independent corrected-design security PASS are the complete proposed-to-accepted metadata promotion gate. Target Windows/CST traces, Claims 7/15, and Line10 remain later implementation/release/registration/deploy admission gates and are not represented as complete by decision acceptance. | Decision `Promotion gate`; artifact-bound review verdicts, followed by separately recorded target/Line10 admission evidence before register/pin/release/deploy. |
| SEC-H-01 / C01 / C02 | Preserved and completed with SR-02: network, mapped, reparse, swap, ADS, alias, hard-link, stream, and generated-header roles use one local canonical identity boundary. | `WindowsPathIdentityV1`; complete namespace/identity matrix. |
| SEC-H-02 / C05 / C14 | Preserved and completed with SR-01: default-off in-server policy covers every route and its exact complete manifest-v2 identity is bound to the complete destination before CST. | `AuthoritySnapshot` plus `AuthorizedBundleTransfer`; route and complete-copy matrices. |
| SEC-H-03 / C12 | Corrected in design: atomic per-invocation Job containment owns 60-second termination, zero-accounting settlement, streams/process/concurrency limits, and foreign preservation. | `WindowsContainedInvocation`; fake every-stage tests plus mandatory target CST membership/breakaway/foreign trace. |
| SEC-M-01 / C07 | Corrected in design: full raw vendor-record and aggregate validation precedes selection/session. | CST vendor adapter; per-field malformed and bounded-iterator tests. |
| SEC-M-02 / C13 | Corrected in design: sampler-only fixed FastMCP errors and allowlist app diagnostics cover every channel. | Safe FastMCP policy and publisher; cross-channel canary test plus existing-tool compatibility. |
| SEC-M-03 / C18 | Corrected in design: composition injects local non-reparse policy/workspace roots with exact Windows owner/effective-access rules. | `AuthoritySnapshot` and `TrustedWorkspacePolicy`; Windows owner/access matrices. |
| SEC-H-04 | Corrected: any unproved required settlement evidence atomically latches quarantine, revokes active admission, releases, and wakes; all acquired waiters recheck and all routes deny until full restart/revalidation. | `SamplerAdmissionGate`; admission linearization, quarantine-all-routes, and target restart probes. |
| SEC-M-04 | Corrected: process creation tuple and sole broker Job-handle ownership are exact. | Tuple mutation and broker-crash target proof. |
| SR-01 / SEC-C04 / SEC-C05 / SEC-C14 | Corrected: every manifest-v2 row is transferred from its exact stable source handle and the complete destination manifest must equal policy before CST; authorization is the copied snapshot, while three source post-hashes are provenance. | `AuthorizedBundleTransfer`; ancillary add/remove/mutate/rename and full destination equality matrix. |
| SR-02-R1 / SEC-C02 / SEC-C07 / SEC-C14 | Corrected: one default-stream-only Windows grammar rejects the documented superscript-digit COM/LPT aliases and ASCII device aliases, case-insensitively and despite extensions/stream/trailing variants, before filesystem access; exact long-name/final-path/volume-file-ID/link/no-reparse/stream proof still covers every path-producing/consuming role. | `WindowsPathIdentityV1`; generated ASCII/superscript/case/extension/stream/trailing/normalization-order properties plus the full identity matrix. |
| SR-C5-01 | Corrected in design: authorization now remains continuous through every unavoidable CST path reopen. One snapshot-created lease holds every ancestor and exact input/output without delete sharing, pre-creates the Save target, revalidates after every operation, and has no ordinary-path fallback. | `AuthorizedVendorPathLease`; deterministic leaf/ancestor swaps, installed CST pre-created-output compatibility, all-return-path settlement, and fail-once-close probes. |
| SR-C5-02 | Corrected in design: startup breakaway proof is true only for the exact conjunction `breakaway_denied=true` and `breakaway_created=false`; created/allowed/missing/ambiguous evidence rejects before request work and quarantines. | `WindowsContainedInvocation`; inverse Boolean/probe-shape matrix plus target explicit breakaway trace. |
| SR-DESIGN-C5-01 | Corrected: read-only inputs omit write/delete sharing; any path-only write executes only under fixed broker virtual account/protected DACL, and unknown header bytes become inputs only after share-0 seal/hash. Same-principal path-only execution is inadmissible. | Windows vendor-isolation owner plus `AuthorizedVendorPathLease`; in-place-write, principal/DACL, header-seal, all-return, and target compatibility probes. |
| SR-DESIGN-C5-02 | Corrected: successful breakaway probe creation retains exact PROCESS_INFORMATION and requires exact-handle terminate, bounded wait/exit, thread/process close, and receipt before unavailable/quarantine return. | `WindowsContainedInvocation`; every-step failure injection and target escaped-child trace without PID/Job substitution. |
| SEC-BROKER-01 | Corrected: one fixed local single-instance named pipe has exact DACL/SACL/rights, mutual SCM PID/token/image verification, server impersonation plus exact client-token comparison and fatal-on-unproved `RevertToSelf`, broker-issued one-use nonce, independent policy binding, bounded one-request frames, and disconnect/cancel settlement. | Windows vendor-isolation owner; `test_saved_field_broker_pipe_authorization_v1` complete abuse/return matrix. |
| SEC-BROKER-02 | Corrected: broker and daemon are fixed credential-free SCM virtual accounts; LSA/SCM owns storage/injection, all channels exclude secrets/tokens/SIDs, and operator-owned disable/delete/package/identity rotation proves old resources absent before restart. | Windows service provisioner plus isolation owner; four-box provisioning/revocation/rotation probes. |
| AR-BROKER-TOPOLOGY-01 | Corrected: daemon is sole broker client; broker alone owns worker/source/transfer/vendor/receipt. | End-to-end topology, zero-source daemon, no-extra-channel, nested-receipt and crash guards. |
| SEC-C6-FH-01 | Corrected: a single broker-wide inheritance epoch excludes concurrent creation until all five parent copies close, and worker clears/readbacks standard inheritance before startup proof plus capability inheritance after identity validation and before descendant-capable work. | Concurrent broker-child and worker-descendant inheritance probes, access-after-worker-close, five flag readbacks, all-return quarantine/zero-work matrix. |
| SEC-C6-FH-02 | Corrected: both root `CreateFileW` access/share/disposition/reparse tuples and both `DuplicateHandle` desired-access/inherit/options tuples are exact; effective access/type/identity/flags are read back and no missing proof defaults. | Field-by-field tuple mutation, conflicting root open/rename/delete, reparse, effective-access and unavailable-readback matrix. |
| SR-C6-BOOT-01 | Corrected: the exact signed native worker executes the first application instruction, native startup proof, capability identity validation and all five inherit-flag clear/readbacks before any CPython initialization; only an accepted `WorkerPreMainReceiptV1` permits closed `PyConfig` initialization and the application frame. | Real direct-child native-first marker; hostile global/user site, `.pth`, customization, `PYTHON*`, registry and cwd canaries; PyConfig/prelude/receipt mutation and all-return zero-Python/zero-work tests. |
| SR-C6-FRONT-02 | Corrected: protected `cst-direct-v1` materializes the exact signed/hash-pinned native frontend as StdioHost's direct child; uvx/wrappers are forbidden on the capability route, while the same embedded `cst.py` retains existing-six stdio ownership. | Direct PID/image/package/parent enrollment, wrapper-absence, capability read/EOF/zeroization/all-exit tests and disabled-sampler legacy-vs-direct existing-six A/B gate. |
| AR-C6-LOAD-05 | Superseded by the stricter correction: post-return comparison and static parsing are inventory only. P18 owns enforced exact-runtime per-app UMCI and one read-only VHDX; `NativePackageLoadOwner` retains the complete pathname/content chain across callbacks and settlement. | Exact policy/VHDX receipts, malicious dynamic-load deny-before-entry, slow callback namespace/stream matrix, mapped-to-held identity, stopped update/rollback and every-return settlement tests. |
| SEC-C6-DYNLOAD-06 | Corrected without trusting static parsing, Python import hooks or post-load detection: P18 installs one enforced per-app UMCI exact-hash package/System32 allowlist for the exact runtime; absent target proof that every unlisted direct/forwarded/indirect/native loader request is denied before first instruction keeps the sampler unregistered. | P18 policy owner then `NativePackageLoadOwner`; `test_cst_package_dynamic_load_policy_v1` malicious TLS/DllMain/CPython/extension/callback matrix and exact Code Integrity event receipt. |
| SEC-C6-NAMESPACE-07 | Corrected by a read-only service-owned VHDX plus retained backing-volume/mount/ancestor/version/file chain; loader remains pathname-based and handles are not claimed as loader inputs. Named streams are a separate per-stream surface proved by explicit pre/post enumeration and read-only-volume mutation denial. | P18 VHDX owner then `NativePackageLoadOwner`; slow-window detach/remount/ancestor/reparse/hardlink/ADS matrix, exact `::$DATA` enumeration, and all-return update/rollback settlement. |
| AR-C6-APPID-09 | Corrected without trusting the contradictory direct cmdlet path: pinned ConfigCI emits only ordinary four-hash tuples, a deterministic XSD author scopes module rows to the independently proved runtime `OriginalFilename`, and a code-independent P18 verifier parses canonical XML, unsigned CIP, and extracted signed content before any deployment handoff. Any disagreement is `policy_artifact_unavailable` and default-off. | `ExactAppIdPolicyArtifactV1`; four-hash/AppID oracle, independent PE hash/version, XML/XSD/CIP/signed-content mutation, two-build identity, and zero-deploy-on-failure probes. |
| SEC-C6-SIGN-08 | Corrected: the sole `CstPolicySigningOwner` is an isolated offline operator ceremony whose non-exportable HSM RSA-3072 key requires physical M-of-N approval for every exact displayed request; target has no signing authority. P18 verifies the request-bound HSM audit receipt, exact single-signer envelope, held content, pinned current chain and fail-hard online-v1 revocation before CiTool. | Ceremony then independent P18 verifier; target group/SYSTEM direct-sign denial plus request/quorum/audit/replay/key/export/signer/signature/online-revocation/leak/cleanup/rotation matrices require zero deployment on every mutation. |
| SEC-C6-OCS-09 | Corrected without treating quorum as per-operation authority: one locked ceremony spans a vendor-verified pre-load anchor through Sign terminal, final-card removal, all-local-slot absence, and post-unload audit settlement with exactly one intended Transaction-ID-equal Sign/ObjectUse and zero sibling/ambiguous exact-key use. Every failure/restart stays default-off and quarantined until closure. | `CstPolicySigningOwner` plus `NShieldAuditCollectorOwner`; second-process/thread/provider Sign, wrong/missing Transaction ID, extra/ambiguous Sign/ObjectUse, retained/reinserted/stuck card, wrong/remote/dynamic slot, provider leak, collector gap, cancel/timeout/crash/restart/rotation and receipt mutation matrix. |
| AR-C6-SIGNER-10 | Corrected: Windows DACL is explicitly not a SYSTEM boundary. Target has no signing key/provider/service/endpoint; only the isolated HSM ceremony can use the key, and every operation requires physical M-of-N approval plus a request-bound audit-attestation receipt. | Direct target group/SYSTEM `NCryptOpenKey`/sign attempts, fake service/endpoint, no-quorum sign, request substitution, receipt replay/counter regression and old-key sign all yield no accepted artifact and zero CiTool. |
| AR-C6-REVOKE-11 | Corrected by deletion rather than a hidden cache adapter: `offline-v1` is rejected. The only profile is bounded `online-v1`, with explicit chain-excluding-root revocation, cumulative timeout, pinned chain and fail-hard offline/unknown/timeout semantics. | Seed conflicting ambient cache; remove network/AIA/CRL/OCSP, expire/revoke/replace chain rows, force timeout and submit `offline-v1`; acceptance tracks only a successful exact online chain and every other case yields zero CiTool. |
| AR-C6-AUDIT-12 | Preserved under the supported vendor boundary: KML/index remain device-owned, exact `nshieldaudit --verify` is the KML-verification owner, and P18 owns only strict JSON projection/correlation plus durable replay. Unsupported individual approver identities/count remain excluded. | ESN/logID/runID/index/transaction/status/`{runID,objid}`/ObjectNew-key mutation; cached/text/malformed export; gap/reset/concurrency/crash and independent content/audit rotation all fail closed. |
| AR-C6-AUDIT-14 | Corrected by admitting exactly one `NShieldAuditCollectorOwner` per HSM ESN with privileged 5c enrollment, explicit config, database/service/export identities, continuous health/exclusivity, held JSON `export --verify`, exact ObjectNew-to-Sign/ObjectUse join and all-return/restart/rotation lifecycle. P18 no longer claims unsupported raw KML parsing or result-byte attestation. | Two collectors, unprivileged enrollment, config/database/exporter/output substitution, stale cached verify, runID/objid reuse with another ObjectNew hash, concurrent export and every crash boundary yield no accepted artifact and zero CiTool. |
| AR-C6-HANDOFF-13 | Corrected without claiming CiTool consumes a handle. `SignedPolicyImportOwner` copies from held no-follow untrusted-media handles into a protected single-artifact VHDX, verifies exact file/signature/stream identity, flushes/detaches and reattaches the same backing VHDX read-only, then retains backing/volume/mount/ancestor/leaf handles across the derived pathname CiTool call and immediate revalidation. | Engineered verify-to-open/use pause with media/leaf/ancestor/reparse/hard-link/ADS/write/delete/valid-policy substitution, VHDX detach/remount/backing swap, crash and cleanup injection. If CiTool cannot consume the retained read-only path the target is incompatible; no reopen or media-path fallback. |

## Decision promotion versus runtime admission

The proposed decision may move to `accepted` only after two independent reviews of
this exact corrected design and proposed decision: Claim-Verify architecture `PASS`
and security `PASS`. Those review artifacts and their bound SHA-256 values are the
complete decision-metadata promotion evidence. Acceptance authorizes downstream
planning and implementation against the stable authority/containment contracts; it
does not attest that a target runtime exists or that target behavior passed.

Target Windows/CST containment evidence, Claim 7 activation/header/ResultTree/status
evidence, Claim 15 independent native comparison, and Line10 acceptance remain
mandatory, separately recorded implementation/release/registration/deploy admission
gates. No sampler registration, package pin, release, or deployment may cross its
applicable gate without that target evidence. This separation changes governance
ordering only; it does not downgrade, waive, or mark any empirical claim verified.

## Open empirical obligations

- `ASSUMPTION (UNVERIFIED)`: the installed CST API can inventory all required
  frame metadata and a non-empty field unit without filename inference. Resolving
  probe: target activation smoke enumerating typed candidate records on only a
  disposable copy. Missing metadata produces `metadata_unavailable`.
- `ASSUMPTION (UNVERIFIED)`: the installed API or versioned official evidence can
  establish the time-dependence sign. Resolving probe: official versioned CST
  documentation or a target native phase-reference experiment. Until resolved,
  the call fails with `cst_saved_field.metadata_unavailable`; it does not guess
  or return an incomplete success contract.
- `ASSUMPTION (UNVERIFIED)`: the observed raw `-1` status applies to the exact
  target CST version and activation path. Resolving probe: version-bound target
  trace with nonzero E positive control and independent evidence. Unknown
  version/status fails.
- `ASSUMPTION (UNVERIFIED)`: the installed CST API can create/open the copied
  project while synchronously returning a directly closable exclusive handle,
  exact process identity, and handle-specific liveness before any transfer.
  Resolving probe: inject every installed-API acquisition failure point with an
  interleaved foreign Design Environment and prove zero creation or exact-handle
  rollback. Missing identity with proved rollback is `session_ownership_ambiguous`;
  unprovable creation/rollback is `session_settle_failed`. PID inference is forbidden.
- `ASSUMPTION (UNVERIFIED)`: the installed CST API/license/COM stack runs as one fresh
  worker under fixed virtual account `NT SERVICE\McpLocalHubCstVendorBroker`, accepts read-only
  inputs held `GENERIC_READ/FILE_SHARE_READ`, writes its header only inside the
  protected vendor workspace, releases the writer so the owner can seal share-0, and
  then accepts known-hash locked clean inputs through lazy sampling. Resolving probe:
  target principal/token/DACL/process trace plus deterministic same-user in-place-write,
  leaf/ancestor replacement, header-writer, and all-return settlement attempts. Any
  same-principal requirement, reusable external worker, inaccessible license/COM,
  share violation, foreign writer, or inability to seal is target capability FAIL;
  sampler remains unregistered and no same-principal/unlocked fallback is admitted.
- `ASSUMPTION (UNVERIFIED)`: a qualifying independent native provider exists;
  its resolving probe is specified in the Line10 section. Until admitted, Line10
  cannot PASS.
- `ASSUMPTION (UNVERIFIED)`: the exact installed CST launch path and every descendant
  remain in the creation-time Job and cannot use WMI or breakaway to escape. Resolving
  probe: on target Windows, record first-worker-instruction membership, active Job
  accounting, every CST descendant membership, an explicit breakaway attempt, 60s
  termination, and broker-crash kill-on-close while a held foreign CST process handle
  stays live and never belongs to the job. Failure returns
  `containment_unavailable`/`containment_settle_failed` and blocks registration/pin.
- `ASSUMPTION (UNVERIFIED)`: the target Windows edition, complete authority-attributed active
  App Control inventory, explicitly selected compatible base family, every intersecting base,
  and the signed sampler supplemental together enforce this runtime's positive exact-hash
  package/System32 module allowlist before image entry without changing unrelated host-canary
  outcomes; a read-only VHDX plus retained namespace chain resists detach/remount/ancestor/
  stream mutation throughout loader callbacks. Resolving probe:
  `test_cst_appcontrol_policy_set_lifecycle_v1`,
  `test_cst_package_dynamic_load_policy_v1` plus
  `test_cst_package_load_lock_window_v1` on the deployment target. Any unsupported,
  unknown authority, audit/broad/ambiguous union, restrictive intersection, missing reboot-
  committed/absent receipt, unrelated-canary change, first-instruction canary, successful
  namespace/ADS mutation or unavailable detector keeps the sampler unregistered; no import-hook,
  post-load, ordinary-directory or read-write-volume fallback exists.

The authority and race-free initial membership are no longer assumptions: policy is
inside every CST-server route, and official/installed Windows surfaces provide
creation-time JOB_LIST assignment. All remaining target/vendor/native assumptions
have explicit fail-closed IDs or admission gates and remain mandatory before the
corresponding target/Line10 gate can pass.

## Terms and Abbreviations

- 8.3 name — optional Windows short alternate filename; never an admitted path spelling here.
- ADS — Windows Alternate Data Stream; P0 admits only the unnamed `::$DATA` stream.
- ACL — access-control list.
- API — application programming interface.
- CNG — Windows Cryptography Next Generation key-storage and signing API.
- CRL / OCSP — certificate revocation list / Online Certificate Status Protocol evidence.
- CST — CST Studio Suite.
- DACL — Windows discretionary access-control list.
- E / H — complex electric / magnetic field.
- FEM — finite element method.
- JSON — JavaScript Object Notation.
- MCP — Model Context Protocol.
- P2 — the six quadratic positions associated with one triangle.
- PID — process identifier; never an ownership authority in this design.
- PKCS #7 — cryptographic signed-message envelope required for signed App Control policies.
- Job Object — Windows kernel process-group and limit object.
- HSM — hardware security module; the isolated ceremony's sole private-key boundary.
- IC — insertion counter reported by `slotinfo`; zero means no token is present.
- KML — nShield module audit-log signing key, distinct from the policy content-signing key.
- OCS — nShield Operator Card Set enforcing physical K-of-N content-key authorization.
- NFC — Unicode Normalization Form C.
- SID — Windows security identifier.
- SHA-256 — Secure Hash Algorithm 256-bit file identity.
- SLIM — CST's retained mesh container; `Result/3d.slim` is hashed as bytes here.
- UNC — Windows Universal Naming Convention network-path form.
- VFEM — the separate vector finite-element-method consumer/comparator.
- `Result3D` / `ResultTree` — CST APIs used to open/register/select copied saved fields.
- `zero_ambiguous` — sampled all-zero vector with no physical-zero assertion.

**Gate decision: PASS** — AR-C6-AUDIT-14 is corrected at design level without weakening
AR-C6-AUDIT-12 or AR-C6-HANDOFF-13 and
without weakening AR-C6-APPID-09, SEC-C6-SIGN-08, AR-C6-SIGNER-10 or AR-C6-REVOKE-11.
The exact nShield 5s/5c profile keeps non-persistent OCS K-of-N on the content key and device
KML/index authority, but the supported evidence boundary is now truthful: one privileged
`NShieldAuditCollectorOwner` per HSM owns explicit config/database/health/exclusivity and the
held JSON `nshieldaudit ncore export --verify`; the vendor utility owns KML verification.
P18 independently pins that adapter/output and correlates exact Transaction ID plus
`{runID,objid}` ObjectNew key hash to successful Sign/ObjectUse, while separately verifying
the held signed CIP hash/signature/content/chain/revocation. It does not claim raw KML parsing
or that vendor audit attests result bytes. A collector/genesis-anchored `AuditReceiptReplayOwner`
durably accepts only continuous strictly-greater ranges, exact prepared crash recovery and
byte-identical completed replay. Unsupported individual approver identities/count and every
wrapper/event/stdout counter remain excluded. Untrusted media cannot reach
CiTool: `SignedPolicyImportOwner` copies to a protected single-artifact VHDX, detaches and
reattaches it read-only, then retains backing/volume/mount/ancestor/leaf identity through the
pathname-only CiTool call and post-call settlement. A target unable to consume this exact
retained read-only path is incompatible, with no close/reopen or media-path fallback.
The prior design remains one pinned offline hash author, one
deterministic XSD author, one isolated physical-quorum HSM `CstPolicySigningOwner`, and one
code-independent P18 request/receipt/XML/CIP/signature/online-chain verifier. Target
group/SYSTEM has no signing route; `offline-v1` is rejected. The broken installed direct AppID-plus-Hash cmdlet path is
forbidden; every pin, cardinality, AppID, hash, parse, reproducibility, key/provider/export,
signer/OID/algorithm/attribute/signature/chain/revocation/leak or cleanup mismatch
returns `policy_artifact_unavailable`, performs zero deployment, and leaves the sampler
default-off. The verified `ExactAppIdPolicyArtifactV1` alone may reach one
`CstAppControlPolicySetOwner`, one signed supplemental identity, and one serialized durable
install/update/remove lifecycle on a disposable or reserved machine whose external policy
writers are proved quiescent for the availability lease. The owner inventories and attributes
every active or staged policy, proves the selected base already permits the exact supplemental
signer, its family union is closed and every other base intersection
admits the exact closure, never mutates a sibling, and emits no committed/absent event until
an observed reboot plus exact post-boot inventory and hostile/unrelated canaries agree. Any
unknown authority, composition or lifecycle state leaves the sampler unregistered and the
VHDX unchanged. The exact non-exportable signing key exists only inside the isolated HSM;
target/application code has no signing provider, endpoint, private bytes, PFX, PIN or password.
Missing ceremony attestation/quorum/request receipt, base signer authorization, or pinned
online-v1 revocation proof means no signing or registration. Rotation stops
registration/services and settles journals before a reviewed base-authorized successor may
sign a forward version and repeat reboot/canary proof. `SEC-C6-DYNLOAD-06` and
`SEC-C6-NAMESPACE-07` remain corrected without
claiming that static parsing, Python hooks, held file handles or post-load comparison can
mediate Windows loader execution. The only native image remains the no-CRT custom-entry PE;
frontend four-handle and worker five-handle revocation finish before package code. P18 also
owns one complete read-only VHDX. The target must prove every unlisted direct/forwarded/
indirect/native load is denied before first instruction; unsupported, audit, broad,
ambiguous or unsettled policy composition is FAIL.
`NativePackageLoadOwner` retains the backing-volume/mount/ancestor/version/file chain and
explicitly enumerates streams across mapping/callback/settlement; the read-only attachment
must reject detach/remount, metadata and ADS mutation. `LoadLibraryExW` remains pathname-
based and the retained handles are continuity, not loader inputs. Every mapped row still
binds to the pre-load receipt, while any policy, namespace or settlement failure exits/
quarantines through existing owners with no weaker route. The design otherwise retains one
implementable three-channel
topology: authenticated `HubEnrollmentProtocolV1`, frontend request/result pipe, and
daemon-to-broker pipe. The actual `StdioHost` launch owner binds the current
supervisor-tracked frontend through a read-once capability plus one-use daemon
challenge; non-authoritative `entry_id` is
independently resolved by the SCM daemon; kernel, broker-transport and frontend-
transport receipts stay with their observing owners. Broker-to-worker authority crosses
no new endpoint: broker atomically creates the exact signed native child with JOB_LIST
and exactly five inherited handles, then communicates only two non-secret handle
locators and expected kernel identities in `WorkerPreMainBootstrapV1` after native
startup proof. Native worker returns `WorkerPreMainReceiptV1`; broker accepts it before
the application frame or embedded CPython initialization. Broker, native bootstrap and
Python worker own disjoint facts. One exclusive broker inheritance epoch ends only after
immediate close of all five parent copies; native worker clears/readbacks three standard
flags before startup proof and both capability flags before CPython or descendant work.
The direct frontend likewise revokes/consumes its launch handle before embedded CPython,
and `cst-direct-v1` binds its exact direct image with no wrapper. Exact root access/share/
reparse and duplicate access/inherit tuples are receipt-bound;
every return closes or process-terminalizes both
child capabilities plus broker originals before root deletion. Admission release/publication
require the complete ordered conjunction. All return paths terminalize launch/frontend/
broker nonces or quarantine, and no live direct-frontend broker/admission/QPC owner
remains. The linked decision remains `proposed` pending fresh independent architecture
and security review over the exact new hashes. Claims 7 and 15 remain target-only and
are not inferred by this design PASS.

## Gate

**PASS.** SEC-C6-OCS-09 is corrected at design/decision level by one owner-held interval
from the vendor-verified pre-load anchor through mandatory final-card removal, supported
all-local-slot unload readback, and post-unload vendor-verified audit settlement. The exact
34 claims remain the review input; Claims 7 and 15 alone remain target-only. The linked
decision remains `proposed` pending fresh independent architecture and security review of
the exact replacement hashes. This authorizes no implementation, signing ceremony, HSM or
policy mutation, CiTool call, registration, deployment, publication, or release.
