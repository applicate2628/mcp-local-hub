# Security Review — Final immutable design gate

## Immutable review boundary

The following inputs were re-hashed in this review and matched exactly:

- design SHA-256 `7423D56DD33394336A06AB8C515D12F4496B4AC0533F2901BF5A0EE1436756ED`;
- proposed decision SHA-256 `49DE418E1EB95E567C1B6AA18C36A124A9EC7075AE0183FD67B0D5072537177B`;
- architecture-review `PASS` SHA-256 `0A64428AA74EA930A0630341C768B0001D45865263CB2D4336D8A16D59D053DC`;
- security-engineer `PASS` SHA-256 `FDE2B842E41C9F771109DC233C34F9C3E5A767EDF40700FBCADEAF7416D4A845`.

This is an independent security-design review. The upstream verdicts were treated as claims,
not as security evidence. No source, test, plan, Git index, service, hardware security module
(HSM), App Control policy, virtual hard disk (VHDX), signing ceremony, deployment, publication,
or release state was changed.

## Evidence and current owner path

- CodeGraph Model Context Protocol (MCP) was used before repository reads. Its first query
  reported the launch files pending index synchronization; after the watcher settled, the
  freshness query named only unrelated `internal/api/daemon_restart_watcher.go`. The current
  production path remains `HostConfig.LaunchCapability -> prepareLaunchCapability ->
  preparedLaunchCapability.apply/start/cancel -> windowsLaunchCapabilityPipe.apply ->
  SysProcAttr.AdditionalInheritedHandles`, with `StdioHost` retaining child-lifecycle ownership
  (`internal/daemon/host.go:27-50`, `internal/daemon/launch_capability.go:19-130`,
  `internal/daemon/launch_capability_windows.go:23-120`).
- The corrected design keeps that Go launch path and gives worker creation to the broker; the
  native image only revokes the exact inherited tuples and admits the package after revocation
  (`design.md:7-31,80-168`). No parallel raw frontend launcher is introduced.
- Entrust documents that a non-persistent Operator Card Set (OCS) keeps protected keys usable
  while the last required card remains inserted and removes them when it is removed; `slotinfo`
  reports token absence as `Token=-` and insertion counter zero, while its flags identify
  remote, dynamic, associated-dynamic, timed-out, and failed-secure-channel slots:
  <https://nshielddocs.entrust.com/security-world-docs/v13.7.3/hsm-user-guide/hsm-mgmt/nshield-audit-log-service.html>,
  <https://nshielddocs.entrust.com/security-world-docs/utilities/slotinfo.html>.
- Entrust also documents that `nshieldaudit ncore export --verify` re-verifies signatures,
  requires exact ESN and logID, and supports inclusive start/end indices; one client service
  should fetch a given HSM or records are split. Transaction IDs are UTF-8 strings of at most
  88 bytes, and environment injection is supported, but linkage may be lost for commands
  implemented internally rather than directly forwarded. The design therefore requires an
  observed exact Transaction-ID-equal `Sign` and does not infer it from the environment alone:
  <https://nshielddocs.entrust.com/security-world-docs/v13.7.3/hsm-user-guide/hsm-mgmt/nshield-audit-log-service.html>,
  <https://nshielddocs.entrust.com/security-world-docs/api-ncore/transaction-ids.html>,
  <https://nshielddocs.entrust.com/security-world-docs/v13.7.3/hsm-user-guide/hsm-mgmt/audit-logging.html>.
- Microsoft documents that CreateFile sharing restrictions remain effective until the handle
  closes, that read-only VHDX attachment requests a read-only disk, that CiTool updates a policy
  from a pathname, and that chain-excluding-root revocation covers every certificate except the
  root. These premises support the retained pathname/namespace contract but do not convert
  CiTool into a handle consumer:
  <https://learn.microsoft.com/en-us/windows/win32/api/fileapi/nf-fileapi-createfilew>,
  <https://learn.microsoft.com/en-us/windows/win32/api/virtdisk/ne-virtdisk-attach_virtual_disk_flag>,
  <https://learn.microsoft.com/en-us/windows/security/application-security/application-control/app-control-for-business/operations/citool-commands>,
  <https://learn.microsoft.com/en-us/windows/win32/api/wincrypt/nf-wincrypt-certgetcertificatechain>.
- Current-machine availability probes found `C:\Windows\System32\CiTool.exe`. They did not
  find `nshieldaudit`, `slotinfo`, or `nfkminfo`; the only ambient `signtool.exe` resolved to
  MSYS and is not the pinned Windows SDK signer. This is not target or ceremony evidence. The
  design correctly rejects ambient lookup and requires exact provisioned tool identities.

## Adversarial assessment

| Attack surface | Result | Security basis and falsifier disposition |
|---|---|---|
| Native pre-main handle theft | clean at design level | Parent admits one exact image; PE imports only Kernel32; delay imports and TLS are absent; first-instruction disassembly and hostile loader canaries must prove revocation before package code (`design.md:80-147`). Any extra pre-entry image or inherited descendant handle rejects registration. |
| Dynamic DLL execution before verification | clean at design level | Static PE walking is inventory only. Enforced per-AppID User Mode Code Integrity (UMCI), exact hashes, read-only VHDX, retained namespace/stream closure, and mapped-to-held equality are the actual boundary (`design.md:149-231,2591-2594`). Direct, forwarded, delay, extension, callback, and malicious TLS/DllMain loads must be denied before their first instruction. |
| AppID or hash-policy author compromise | clean at design level | Ordinary four-hash extraction, deterministic XSD authoring, independent OriginalFilename/hash proof, independent XML/CIP/signed-content verification, and two-build identity fail closed on disagreement (`design.md:149-168,2594`). AppID is expressly a selector, not identity. |
| OCS sibling signature while the key is loaded | clean at design level | One station-global owner holds the lock from a fresh pre-load audit anchor through signer/provider close, final-card removal, all-local-slot absence, key-unavailability proof, and continuous vendor-verified settlement (`design.md:263-321`). The range rejects every second, failed, differently tagged, untagged, exact-key, or key-ambiguous use. |
| Collector/export substitution or split history | clean at design level | One privileged collector per ESN owns explicit module config, service/database/binary identities and a held fresh JSON `export --verify`; absent/empty/catch-all modules, ambient directory overrides, a second collector, gaps, resets, cached verification, text output, or malformed rows disable signing (`design.md:421-537`). |
| Signature/content/chain substitution | clean at design level | Exact one-signer PKCS #7 grammar, explicit `CryptMsgControl(CMSG_CTRL_VERIFY_SIGNATURE_EX)`, held embedded-content equality, pinned current chain, and hard-fail `online-v1` are independently checked (`design.md:324-420`). Decode success, SignTool exit, cache-only status, timestamp time, alternate chain, and offline fallback are non-authoritative. |
| CiTool pathname swap | clean at design level | Import copies from held no-follow media into a single-artifact VHDX, verifies it, reattaches the same backing image read-only, and retains backing/volume/mount/ancestor/leaf handles through CiTool and immediate settlement (`design.md:557-645,2601`). If retained sharing is incompatible with CiTool, the target is incompatible; there is no reopen or media-path fallback. |
| App Control composition confused deputy | clean at design level | One policy-set owner inventories all authorities, selects one compatible base family, accounts for union/intersection behavior, serializes external writers, and requires reboot-observed committed/absent state plus unrelated canaries (`design.md:170-231,645-768`). Unknown, audit-only, broad, pending, or externally mutating authority keeps the sampler absent. |
| Prompt, path, request, or result injection | clean at design level | Caller data selects no authority; the frontend request contains no path, bytes, handle, manifest, policy revision, or authority override (`design.md:19-20,1395-1515`). Fixed schemas, Windows path identity, bounded records/results, safe error allowlists, and secret/path canaries reject traversal, alternate streams, device aliases, malformed vendor records, prompt-like content, and output leakage. No large-language-model instruction is executed. |
| Secrets, credentials, and configuration | clean at design level | The target has no signing key/provider/credential/endpoint. HSM authentication remains provider-local; capabilities stay in native buffers/pipes and are zeroed; handle locators are non-secret; service identities are credential-free; machine-neutral manifests exclude ambient state (`design.md:232-261,2188-2255`). All enablement is positive and missing proof is default-off. |
| Failure, restart, retry, and cleanup | clean at design level | Each nonce, capability, process, handle, collector export, ceremony interval, VHDX, policy transition, and admission path has one terminal owner. Ambiguity latches quarantine; restart is cleanup/reconciliation only and cannot sign, re-deploy, retry privileged work, or synthesize receipts (`design.md:33-69,300-321,2018-2144`). |

## Security surface checklist

| Required surface | Status | Review result |
|---|---|---|
| Untrusted input crossing a trust boundary | found and reviewed | Injection, deserialization/framing, device-name/path traversal, reparse/hard-link/alternate-stream mutation, request substitution, and malformed vendor output all have fail-closed probes. No server-side request forgery route is introduced. |
| Authorization and object-level substitution | found and reviewed | Entry/generation/revision, supervisor process, service SID, image, capability, nonce, request hash, source identity, policy identity, signer/key, ESN/logID/range, and VHDX identities are bound independently. Cross-entry and stale-authority substitutions perform zero privileged work. |
| New or updated dependencies | found and reviewed | MSVC/SDK, native runtime, CPython/package closure, ConfigCI, CiTool, nShield firmware/software, CNG provider, signer certificate, HSM key, collector/exporter, and target OS closure are pinned with provenance and rejection probes. Installed target/station qualification is not run because this artifact is a design-only gate. |
| New config or flag | found and reviewed | Enablement, signing, policy deployment, VHDX attachment, online revocation, collector module selection, inheritance and AppID scope are allowlist-built and fail closed. Destructive effects require positive admitted state; absent or zero values do not deploy or sign. |
| Agent or large-language-model surface | not-applicable | The tool consumes typed numeric/path metadata and does not execute model output or prompt instructions. Prompt-like and instruction-like input remains untrusted data and is covered by fixed-schema/output canaries. |
| Secret storage, injection, logging, rotation | found and reviewed | The security-engineer four-box ownership is coherent: non-exportable HSM key, device-owned KML, credential-free services, native capability buffers, exact rotation owners, and cross-channel leak scans. |
| Target/runtime falsifiers | not-run with reason | This review was explicitly limited to immutable design metadata. Native PE traces, malicious DLL first-instruction denial, App Control/VHDX lifecycle, nShield ceremony/collector replay, installed CST Claim 17, architecture Claims 7/15, and Line10 require later provisioned target gates. They are not inferred here. |

## Findings and required fixes

No critical, high, medium, or low security finding was reproduced in the immutable design.
The prior OCS sibling-use defect is corrected at design level: final-card removal and supported
all-slot readback close key availability, while the continuous vendor-verified audit interval
detects any sibling use that occurred before removal. No publication-safety exception is
requested or approved.

The following are required empirical obligations, not waivers or optional hardening:

- prove exact Security World 13.7.3 binaries, firmware, module/slot grammar, non-persistent OCS,
  no-card key unavailability, sole privileged collector, exact Transaction ID preservation,
  ObjectNew/ObjectUse join, gap-free export/replay, every crash boundary, and old-key denial;
- prove independent PE/hash/version parsing, deterministic XML/CIP/signed-content identity,
  explicit one-signer verification, exact current chain and hard-fail online revocation;
- prove complete authority-attributed App Control composition, external-writer quiescence,
  read-only VHDX and namespace continuity through an engineered CiTool/loader delay, reboot
  commit/absence, and denial of every unlisted image before its first instruction;
- prove every receipt from owner observations rather than defaults, every return cleanup,
  existing-six A/B compatibility, installed CST Claim 17, architecture Claims 7 and 15, and
  Line10 independently on the admitted target.

## Exact 18 S4 claim verdicts

`verified` below means the immutable design supplies one coherent guarantee, one owner, and a
named falsifying probe. It does not claim implementation or target execution. The table has
exactly 18 claim rows.

| Claim | S4 verdict | Independent reviewer disposition |
|---:|---|---|
| 1 | verified | Daemon authority snapshot and broker reauthorization ensure caller data selects no authority; stale, swapped, duplicate, unknown, or ambiguous entry/revision performs zero broker/source/worker/CST work. |
| 2 | verified | Enrollment admits only the kernel-authenticated current supervisor and exact CST task process; PID reuse, squatter, token/session/image and fabricated-row probes fail closed. |
| 3 | verified | `SupervisorCstIdentityAuthorizerV1` grants the daemon only the closed CST identity operation; opcode substitution and supervisor-state inequality are denied. |
| 4 | verified | Each of the three endpoint owners applies and reads back exact runtime SIDs, protected DACL, High SACL and ordered masks; owner/ACE/mask/label/anonymous/network/second-instance mutations reject. |
| 5 | verified | Existing T02 launch ownership gives one capability only to the exact direct native frontend and native pre-main consumes, closes and zeroes it before package/Python code; exact-four and all-return canaries falsify drift. |
| 6 | verified | `HubEnrollmentProtocolV1` owns independent nonce and capability terminal ledgers; ACK loss, cancel, expiry, reconnect, exit, shutdown and restart cannot replay or strand authority. |
| 7 | verified | The daemon frontend admission owner binds direct image, capability, challenge, correlation, request hash, generation, entry and deadline; clone/replay/package-load/request forgery is zero-work. |
| 8 | verified | Daemon composition alone owns admission/broker routing while same-process `cst.py` owns compatibility/publication; topology and no-direct-route probes reject a bypass. |
| 9 | verified | Broker pipe authentication admits only the current SCM daemon after mutual token/service/session/image checks, impersonation and proved revert; identity drift or failed revert is fatal. |
| 10 | verified | `BrokerProtocolV1.NonceLedger` atomically binds and consumes/cancels nonce, correlation, request, policy, manifest and unchanged QPC triple on all framing/replay/timeout/disconnect/shutdown paths. |
| 11 | verified | Broker capability ownership alone opens least-right source/workspace roots and validates access/share/reparse/final identity; missing readback creates no worker and no pathname fallback. |
| 12 | verified | Native pre-main revokes five flags before `NativePackageLoadOwner` admits only exact committed-policy code and closed Python; malicious direct/forwarded/delay/extension/callback loads must be denied before entry, with VHDX/PyConfig settlement. |
| 13 | verified | `WorkerInheritanceEpoch`, native worker and Job owner prevent sibling/descendant retention; concurrent creation, explicit duplication and post-close access probes falsify leakage. |
| 14 | verified | Typed owner-local receipts and `WindowsContainedInvocation` require observed lifecycle facts on every return; missing, contradictory, defaulted, fabricated, or residual-state receipts reject success. |
| 15 | verified | `AbsoluteInvocationBudget` carries one unchanged QPC frequency/admitted/deadline triple across owners and creates a cleanup deadline only after termination; any altered tick/frequency/deadline or staged delay fails. |
| 16 | verified | Existing six contracts remain unchanged and the seventh has exactly one protected policy, native loader, daemon and broker route; route residue, dynamic-loader, namespace and fallback graph probes reject alternatives. |
| 17 | not-verifiable (target-only) | Installed CST under the fixed broker identity must still prove locked inputs, sealed output, exact Job/no-breakaway/no-console, ResultTree behavior and foreign-CST preservation on a disposable target. No design metadata can verify this claim. |
| 18 | verified | Provisioning, signing, collector, replay, policy, T02 enrollment, broker and native-load owners pin dependencies/config/secrets and keep every incomplete install, OCS interval, rotation, rollback, restart or publication state default-off. |

Matrix total: exactly 18 claims; 17 `verified`; 0 `failed`; 1
`not-verifiable (target-only)` (Claim 17 only).

## Decision promotion and residual risk

The proposed decision may be promoted from `proposed` to `accepted` because both required
independent metadata gates are bound to the exact corrected design and decision hashes, and
this independent security review found no unresolved security defect. Promotion authorizes
downstream planning and implementation against the contracts only. It does not verify or
authorize a signing station, HSM ceremony, App Control/VHDX mutation, CiTool call, service
restart, target CST execution, registration, deployment, publication, or release.

Residual risk is concentrated in empirical integration: installed nShield behavior and audit
shape, Windows policy composition and deny-before-entry semantics, pathname coexistence with
CiTool and the loader, native receipt truthfulness, and installed CST containment. Each remains
an explicit fail-closed implementation or target gate; missing or null evidence is not clean.

## Terms and Abbreviations

- AppID — App Control per-application scope selector.
- CIP — compiled Code Integrity policy.
- CNG — Cryptography Next Generation.
- CST — CST Studio Suite.
- HSM — hardware security module.
- KML — nShield audit-log signing key.
- MCP — Model Context Protocol.
- OCS — Operator Card Set.
- QPC — Query Performance Counter.
- SCM — Service Control Manager.
- UMCI — User Mode Code Integrity.
- VHDX — Hyper-V virtual hard-disk image format.

## Gate

**PASS.** All 18 S4 claims were independently reviewed across native pre-main inheritance,
dynamic DLL loading, AppID/hash policy authoring, isolated HSM signing, collector/export/replay,
OCS final-card unload and sibling-use exclusion, exact signature/content/chain/revocation,
read-only-VHDX pathname handoff, complete App Control composition, receipts/all-return cleanup,
default-off behavior, prompt/input/output handling, dependencies, configuration, and secrets.
Seventeen claims are coherent and verified at design level; Claim 17 alone remains target-only.
The proposed decision may be promoted to `accepted` for planning and implementation. This
verdict explicitly authorizes no implementation, live service/HSM/signing/policy/VHDX/CiTool
operation, registration, deployment, publication, or release.
