# C6 Implementation Security Review — CST Saved-Field Sampler

## Review identity and immutable boundary

| Item | Reviewed value |
|---|---|
| Execution role | `$security-engineer` |
| Immutable candidate | `5ff268dc13b2be9ca9500b5441634f0594538b94` |
| Accepted security constraints | `security-constraints.md`; SHA-256 `5ADE835A0C997E4FD761E7926A1FE409B17E6FFC965CE437EC1A4A4CFE7A7035` |
| Independent design security review | `security-review-design.md`; SHA-256 `9ABB4224DEE0FB1022B8627D180BDAB7C1823671519CA27C31924E5B1715E5FD` |
| Accepted design and plan | `design.md`; SHA-256 `6C47670725FFB2E715BD78915131F50E693A9B8975A053AE9DB6C2399FD8C172`; `plan.md`; SHA-256 `FBB757B98797C90B7C9FD9B4C4998DCB01788241C5A4D39DE62D1532FD3C684E` |
| Implementation handoff | `implementation-broker.md`; SHA-256 `457892D6316A109784C6FE3C28481346098CD4FE8A8AB8C95FFCF0437BE33620` |
| Prohibited and not performed | Source/test/Git/index mutation; live Service Control Manager (SCM), Computer Simulation Technology (CST), hub or fleet action; service installation; named-pipe creation; publication |

This review checks whether the immutable candidate implements the accepted security
contract. It does not infer installed-CST Claim 17 from unit or synthetic evidence.

## Threat model and trust boundaries

| Boundary | Attacker position, reachable assets, entry points | Required implementation result |
|---|---|---|
| Model Context Protocol caller → daemon | Authenticated caller or prompt-injected request can target source bundles, CST capacity and public output through every sampler route. | Closed request schema, exact policy authority, zero source/broker work on rejection, finite canonical response and safe identifiers. |
| Local process → broker pipe | Local insider, anonymous/network client, foreign service, stale process or wrong token can target broker privilege, source authority, worker and license through the fixed pipe. | Applicable local single-instance pipe descriptor, exact current SCM process/token/session/integrity/image proof, impersonation and proved revert before parsing. |
| Authenticated daemon → broker | Compromised or stale daemon can replay, reframe or confuse policy/source authority through challenge and request frames. | Atomic one-use nonce, correlation/revision/entry/manifest/request-hash/QPC binding, closed framing, one terminal connection ledger. |
| Broker → worker/CST | Faulty or compromised child can escape, inherit authority, outlive cancellation or fabricate settlement through process creation and response frames. | Atomic no-console Job launch, exact handles, denied breakaway, owner-produced settlement facts, unchanged deadline and fail-latched quarantine. |
| Source/workspace → vendor path API | Same-user writer or namespace racer can replace authorized bytes through alias, ancestor/leaf rename, in-place write, hard link, stream or output reopen. | Broker-principal protected workspace, retained stable capabilities, exact share modes, complete copy and writer-close/share-zero output seal. |
| Vendor/protocol → public result and diagnostics | Proprietary bytes, paths, identities, credentials or prompt-like content can escape through errors, logs, frames or success text. | Closed safe failure identifiers, bounded canonical text and cross-channel exclusion of sensitive values. |

## Blocking findings

### SEC-C6-I-01 — HIGH — no deployable daemon-to-broker security boundary

Root cause: the production composition constructs only `UnavailableBrokerTransport`
(`cst.py:669-677`). The actual broker service core, authenticated pipe session,
descriptor, contained invocation, vendor-path lease and provisioning contract have no
production call site: current-session exact-symbol scan found
`SavedFieldBrokerService`, `AuthenticatedPipeSession`, `PipeSecurityDescriptorV1`,
`CtypesWindowsKernel`, `WindowsContainedInvocation`, `IsolatedVendorPathLease` and
`service_contract` instantiated only by tests. `pyproject.toml:18-20` exposes daemon
entry points only and no broker service entry point. Consequently a valid policy can
never yield an authenticated live broker, and P18 has no immutable executable route to
provision or probe.

Impact: Claims 1-5, 8, 10, 11 and 18 are not implemented end to end. The safe
default-off behavior is preserved, but this candidate cannot be enabled as the
accepted feature and cannot truthfully produce target settlement evidence.

Required fix: add one production Windows named-pipe client/SCM broker service route
that derives peer proof from the impersonated token and current SCM process, resolves
the independent broker policy snapshot, owns one contained worker invocation and one
vendor transaction, and returns only owner-produced nested receipts. Wire that route
into restart composition; keep `UnavailableBrokerTransport` solely as the fail-closed
unavailable result. Add safe adapter tests proving the production composition, not an
in-process substitute. No service should be installed during the correction.

### SEC-C6-I-02 — HIGH — broker settlement facts are fabricated at the wrong layer

Root cause: `SavedFieldBrokerService.exchange` constructs
`BrokerSettlementV1` with `worker_signaled`, `worker_exit_recorded`,
`worker_reference_closed`, `job_active_zero`, `readers_joined` and `pipe_closed`
hard-coded `True` (`cst_saved_field_vendor_isolation_windows.py:288-307`). Its injected
worker returns only `WorkerSettlementV1`, so the service has no evidence source for
those broker-owned facts. The only end-to-end test passes an in-process function and
therefore never falsifies process/Job/pipe settlement.

Impact: a worker response can be elevated into a complete broker receipt without the
Job, process, readers or pipe owners attesting their state. Claims 8, 10 and 11 fail;
success/quarantine linearization is not trustworthy.

Required fix: make the broker containment/pipe owner return a typed exact receipt
populated from `KernelInvocationResult`, reader joins, handle ledger and pipe terminal
state. The broker response must consume those fields without defaults or constants.
Every normal/error/cancel/disconnect/daemon-death/broker-death return must be
deterministically falsified one field at a time.

### SEC-C6-I-03 — HIGH — the original QPC authority is replaced by a fresh timer

Root cause: daemon creates the accepted `QpcDeadlineV1`, but
`WindowsContainedInvocation.invoke_after_startup` accepts only a local float `start`
and creates fresh `start+60`, `start+58` and `start+70` deadlines
(`cst_saved_field_containment_windows.py:384-412`). The worker checks only the final
deadline before/after the whole transaction (`cst_saved_field_broker_worker.py:46-60`),
not the required `deadline_tick-2*frequency` success cutoff. No production composition
connects either implementation to the broker request.

Impact: queue/transport delay can be reset at worker containment, extending authority
beyond the daemon admission deadline; cleanup time is preallocated before termination
rather than created from the termination tick. Security Claim 12 and the contained-
duration guard fail.

Required fix: pass the unchanged integer QPC triple through the actual containment
owner; verify local frequency/current tick at each receiver; use worker cutoff
`deadline_tick-2*frequency`, publication cutoff `deadline_tick`, and create the
additional ten-second cleanup deadline only when termination begins. Add engineered
queue/transport/factory/write/response delays that prove zero source/vendor/success
work after the original cutoff.

### SEC-C6-I-04 — HIGH — the declared pipe DACL is not valid Windows SDDL

Root cause: `PIPE_DACL_SDDL` contains symbolic strings
`S-1-5-80-BROKER` and `S-1-5-80-DAEMON`
(`cst_saved_field_vendor_isolation_windows.py:36-44`) rather than resolved numeric
service security identifiers. A current-session read-only Win32 probe passed the exact
DACL to `ConvertStringSecurityDescriptorToSecurityDescriptorW`: DACL and combined
DACL+SACL both returned `ok=False`, Win32 error `1337` (`ERROR_INVALID_SID`); the SACL
alone returned `ok=True`.

Impact: the fixed pipe cannot be created with the declared descriptor. Claim 2 fails
before authentication, and substituting a permissive descriptor would violate the
contract.

Required fix: resolve both fixed virtual-service accounts through an explicit trusted
provisioning owner, construct canonical numeric-SID SDDL, compile it, apply it, and
read back exact effective rights. Fail closed if resolution, compilation, application
or readback differs. Add the exact Win32 parse/readback falsifier and wrong-SID matrix.

## Preserved controls and verification evidence

| Control | Current result |
|---|---|
| Default-off registration | Preserved: production transport reports incomplete startup proof, so no sampler is registered. |
| Protocol shape/replay primitives | Closed canonical bounded frames, 256-bit one-use nonce, correlation/revision/entry/manifest/request hash and QPC fields exist; end-to-end pipe/reconnect enforcement remains blocked by SEC-C6-I-01. |
| Impersonation ordering | `AuthenticatedPipeSession` reverts in `finally` before parse and quarantines failed revert; actual token derivation/pipe adapter is absent. |
| Atomic worker primitive | Safe Win32 suite exercises exact Job/handle-list/no-console launch, cleanup, breakaway truth and no orphan; it is not wired to the broker route. |
| Source/input/output primitives | Stable-handle transfer, Windows identity grammar, retained input shares and share-zero output seal suites pass; distinct broker-principal effective access remains P18 target evidence after production wiring. |
| Public response | One canonical text item capped at 1,048,576 UTF-8 bytes, no structured duplicate, safe identifiers and canary-safe failures pass. |
| Fresh full package | Exit `0`; 508 tests collected and the full 508-test run completed without failure. One pre-existing Pydantic warning remains. |
| Static checks | Ruff check passed; Ruff format reports 53 files already formatted; `git diff --check HEAD^ HEAD` passed; dependency/protected-path diff was empty. |
| Live safety | No SCM service, named pipe, CST project, hub or fleet state was opened or mutated. |

Passing unit tests do not close the four findings because the tests exercise detached
fixtures or in-process substitutes rather than the production trust channel.

## Abuse cases and verification expectations

1. Anonymous, network, interactive, foreign-service, stale-process, wrong-token,
   session, integrity, privilege, image and second-client attempts must reach the real
   pipe adapter and perform zero parse/source/worker work.
2. Malformed, duplicate, trailing, replayed, expired, stale-policy and reconnect frames
   must consume/terminate the exact ledger without granting later authority.
3. Each nested settlement field must be forced false on every return path; success and
   later admission remain suppressed, with quarantine when settlement is unproved.
4. Queue and transport delay must leave the original QPC triple unchanged; blocks at
   authorization, transfer, vendor call, writer, reader and response publication must
   not rebase time.
5. Ancestor/leaf swaps, reparse points, hard links, alternate streams and in-place
   writes must fail under the actual broker identity while interleaved foreign CST is
   untouched.
6. Success/error/log/stderr/frame/manifest canaries must exclude source paths/bytes,
   account/SID/PID/handle/token/credential/license/environment/command/proprietary
   content and prompt-like payloads.

## Credential four-box

| Box | Current implementation disposition |
|---|---|
| Storage owner | Fixed virtual-service account names and credential-free intent exist; no password or application `LogonUser` path was found. Actual LSA/SCM service state is not created in this phase. |
| Injection path | Intended SCM session-0 tokens are modeled by `ServiceSpec` and `PeerTokenProofV1`; the production service/pipe adapter that derives those proofs is missing. |
| Log/serialization exclusion | Protocol schemas and public failures exclude credential/token/SID/license values; current safe tests contain no live credential. |
| Rotation/revocation owner | Dry-run rollback names stop/settle/delete/revoke/absence/restart ordering; no executable elevated owner or restart revalidation route exists yet. |

## Exact 18 security claims

1. `{ guarantee: Every source is bounded local non-reparse and authorized by equal immutable daemon and broker policy snapshots before content access while remote device mapped reparse alias unlisted and caller-selected forms perform zero source worker or CST work; single-owner: AuthoritySnapshot plus broker authorization owner; enforcement-probe: production daemon and broker policy mismatch/local/remote/mapped/reparse/alias/unlisted/caller-selector matrix with source worker and CST counters; verdict: failed by SEC-C6-I-01 }`.
2. `{ guarantee: One fixed local single-instance pipe admits only the current SCM daemon process whose exact token user service identity logon session integrity privileges and pinned image pass broker impersonation checks before parsing or privileged work; single-owner: Windows vendor-isolation owner; enforcement-probe: real descriptor compile/readback plus anonymous/network/interactive/foreign-service/stale-PID/wrong-token/session/integrity/privilege/image/second-client/failed-revert matrix; verdict: failed by SEC-C6-I-01 and SEC-C6-I-04 }`.
3. `{ guarantee: Every broker request is challenge correlation policy entry manifest request-hash and expiry bound with atomic one-use nonce consumption so direct stale replayed duplicated malformed trailing second-frame and reconnect attempts perform zero source worker or CST work; single-owner: BrokerProtocolV1 authorization owner; enforcement-probe: actual pipe challenge/expiry/replay/correlation/revision/manifest/hash/framing/disconnect/restart-ledger matrix; verdict: failed end to end by SEC-C6-I-01 }`.
4. `{ guarantee: Daemon is the sole authenticated broker client and owns only admission original QPC triple pipe cancellation final receipt publication and quarantine while it never reads source creates sampler workspace launches sampler child or holds worker Job stream token session or lease authority; single-owner: CST composition root; enforcement-probe: production dependency and handle-ownership graph plus daemon zero-source/child/workspace counters on every route; verdict: failed by SEC-C6-I-01 }`.
5. `{ guarantee: Only broker boundary opens policy-derived stable source capabilities copies complete authorized manifest directly into its protected workspace and launches fresh contained worker with no daemon proxy intermediate helper workspace second authority channel alias or fallback; single-owner: broker AuthorizedBundleTransfer and containment composition; enforcement-probe: production byte-continuity/complete-manifest/no-extra-channel/process-ancestry trace; verdict: failed by SEC-C6-I-01 }`.
6. `{ guarantee: No reparse alias hard link alternate stream source swap workspace swap ancestor rename leaf replacement or in-place write changes a known CST input after authorization; single-owner: WindowsPathIdentityV1 plus AuthorizedVendorPathLease; enforcement-probe: namespace/identity/write-share/ancestor/leaf-swap/hard-link/stream/retained-handle matrix; verdict: verified for the implementation primitive, with target principal behavior deferred }`.
7. `{ guarantee: Every write-capable project or generated header remains solely inside distinct broker-principal protected workspace and every known input is retained read-only with exact share masks while unknown output becomes input only after writer close share-zero seal identity proof and hash; single-owner: Windows vendor-isolation owner plus AuthorizedWorkspaceSnapshot; enforcement-probe: actual broker-principal effective-access/daemon-denial/same-user-writer/lazy-read/output-seal/post-consumption-hash matrix; verdict: failed end to end by SEC-C6-I-01 despite verified lock/seal primitives }`.
8. `{ guarantee: Broker alone owns exact worker process thread Job stream watchdog and token handles and worker owns source snapshot session vendor lease and workspace resources only as one non-transferable broker invocation until complete nested response or settlement; single-owner: WindowsContainedInvocation plus broker-worker composition; enforcement-probe: production creation/transfer/vendor/return/exception boundaries with exact owner and receipt equality; verdict: failed by SEC-C6-I-01 and SEC-C6-I-02 }`.
9. `{ guarantee: Process identifiers names paths snapshots and set differences never alone authorize attach close terminate token access or absence and interleaved foreign CST remains untouched; single-owner: exact process and Job handle owners plus OwnedSamplerSession; enforcement-probe: PID-reuse/identity-mismatch/static-authority/foreign-process/acquisition/timeout/cancel/crash/breakaway traces; verdict: verified for current handle-based primitives, target CST trace deferred }`.
10. `{ guarantee: Cancellation disconnect daemon death broker death shutdown timeout crash and normal residual activity settle exact pipe nonce impersonation worker signal exit reference Job active zero readers token source session lease workspace and handle evidence before success or later admission; single-owner: broker containment owner plus daemon SamplerAdmissionGate at authenticated receipt boundary; enforcement-probe: production every-return/death/cancel race with nested receipt equality/foreign preservation/all-route quarantine; verdict: failed by SEC-C6-I-01 and SEC-C6-I-02 }`.
11. `{ guarantee: Broker death closes sole non-inheritable worker Job handle and kills only owned worker tree while daemon death is observed by broker pipe loss and drives broker-owned exact worker settlement before local channel close; single-owner: broker WindowsContainedInvocation plus BrokerProtocolV1 disconnect owner; enforcement-probe: deterministic independent broker/daemon process-crash traces proving owned-tree disappearance/foreign preservation/no success/settlement or quarantine; verdict: failed by SEC-C6-I-01 and SEC-C6-I-02 }`.
12. `{ guarantee: One unchanged integer QPC frequency admitted tick and deadline tick triple crosses daemon broker and worker and every receiver validates frequency equation equality and current tick without rebasing so worker success ends two seconds before original expiry publication ends at expiry and only cleanup uses ten additional seconds; single-owner: AbsoluteInvocationBudget plus BrokerProtocolV1 and BrokerWorkerProtocolV1; enforcement-probe: altered-field/frequency/future/expired/transport-delay/queue-delay/every-stage-block/post-expiry-zero-work matrix; verdict: failed by SEC-C6-I-03 }`.
13. `{ guarantee: Request policy filesystem transfer pipe frame vendor record point process concurrency settlement and output resources have finite non-caller-raiseable ceilings and overflow or exhaustion fails atomically without partial success or weaker route; single-owner: SamplerBudgetPolicy plus protocol containment and result owners; enforcement-probe: exact-and-one-over plus combined-budget exhaustion across every return path; verdict: verified in current bounded primitives }`.
14. `{ guarantee: Public success is exactly one canonical finite TextContent text no larger than 1048576 UTF-8 bytes with structured content absent and no truncation spill partial row raw frame or untrusted re-encoding; single-owner: composition-root result publisher; enforcement-probe: actual FastMCP exact-limit/one-over/canonicalization/partial-frame/broker-byte canary capture; verdict: verified }`.
15. `{ guarantee: Public text logs stderr frames diagnostics feature-requested dumps and publication manifests exclude raw source paths bytes account or security identifier values process identifiers handles tokens credentials commands environment license exceptions and proprietary payload; single-owner: safe diagnostic protocol and publication owners; enforcement-probe: cross-channel credential/machine-path/proprietary/prompt-injection canaries with fail-closed publication; verdict: verified for current emitted surfaces, production pipe/service channels pending SEC-C6-I-01 }`.
16. `{ guarantee: No obsolete daemon-spawned helper helper protocol parent-owned sampler Job or stdio helper workspace transfer helper result alias compatibility frame or daemon-owned worker crash route remains live in design decision dependency graph plan tests rollback or runtime; single-owner: topology decision and CST composition root; enforcement-probe: exact stale-symbol/relation/dependency/owner scan plus one-process-tree/one-pipe/one-request trace; verdict: verified for obsolete-route deletion, while the required replacement route is absent under SEC-C6-I-01 }`.
17. `{ guarantee: Installed CST must run as one fresh fixed broker-identity worker accept retained read locks and share-zero output sealing keep every descendant in exact Job reject breakaway produce no console or hidden solve path and fail registration on incompatibility without fallback; single-owner: version-bound installed CST and broker admission record; enforcement-probe: disposable target Windows service token/pipe/path-lock/output-seal/ResultTree/descendant/breakaway/no-window/hidden-call/license/COM/foreign-preservation trace; verdict: not-verifiable until the named target gate and never inferred here }`.
18. `{ guarantee: Credential-free fixed daemon and broker identities protected service pipe policy source workspace and worker boundaries rotate or revoke only under elevated operator ownership and disabled wrong stale replaced or incompletely settled identity leaves sampler unregistered with no token nonce session process workspace or authority reuse; single-owner: service provisioner AuthoritySnapshot vendor-isolation owner and restart admission; enforcement-probe: production storage/injection/exclusion/rotation/revocation/wrong-identity/old-resource-absence/restart-revalidation four-box matrix; verdict: failed by SEC-C6-I-01 and SEC-C6-I-04 }`.

## Gate

**REVISE.** Immutable candidate `5ff268dc13b2be9ca9500b5441634f0594538b94`
preserves default-off safety and several strong primitives, but it does not implement a
production daemon→authenticated-pipe→broker→contained-worker route, fabricates
broker-owned settlement facts, rebases the accepted QPC deadline, and declares an
unparseable pipe DACL. These are remediable implementation defects and require a new
immutable candidate followed by fresh architecture, security-engineer, independent
security-reviewer and QA review. Claim 17 remains separately target-only and is not
used to justify this verdict.

## Terms and Abbreviations

- ACL: Access-control list.
- COM: Component Object Model.
- CST: Computer Simulation Technology electromagnetic solver suite.
- DACL: Discretionary access-control list.
- Job: Windows Job Object controlling one owned process tree.
- LSA: Local Security Authority.
- MCP: Model Context Protocol.
- PID: Process identifier; never sufficient ownership authority by itself.
- QPC: Query Performance Counter, the Windows monotonic clock.
- SACL: System access-control list.
- SCM: Windows Service Control Manager.
- SDDL: Security Descriptor Definition Language.
- SID: Security identifier.
