# Security Constraints — Final signing, audit, policy, and runtime gate

Reviewed immutable inputs:

- design SHA-256 `7423D56DD33394336A06AB8C515D12F4496B4AC0533F2901BF5A0EE1436756ED`;
- proposed decision SHA-256 `49DE418E1EB95E567C1B6AA18C36A124A9EC7075AE0183FD67B0D5072537177B`;
- architecture-review PASS SHA-256 `0A64428AA74EA930A0630341C768B0001D45865263CB2D4336D8A16D59D053DC`;
- prior security REVISE SHA-256 `9AA793CA9E9C772902622D6ABC5AF4CDD8C731EBA3CCF6C71DF02F9F40514E0D`.

This is an independent security-design review. The architecture verdict was not treated as
security evidence. This artifact authorizes no source or test change, planning, decision
promotion, hardware security module (HSM) operation, signing, App Control mutation, virtual
hard disk (VHDX) mutation, deployment, registration, publication, release, or target inference.

## Current evidence

- A fresh CodeGraph Model Context Protocol (MCP) exploration returned current on-disk source.
  Its pending-index notice named only two unrelated files, neither in the queried CST launch
  path. The production frontend owner remains
  `HostConfig.LaunchCapability -> prepareLaunchCapability -> preparedLaunchCapability.apply/
  start/cancel -> AdditionalInheritedHandles`. The separate broker owns worker creation. No
  parallel frontend launch owner was used as a premise.
- Entrust Security World Software 13.7.3 documents that `nshieldauditd` is YAML-configured;
  absent `modules` fetches all available modules while `modules: []` fetches none; only one
  client service should fetch a given HSM; a network 5c collector must be privileged; multiple
  collectors split records; stopped-service backup is required for database consistency; and
  `nshieldaudit ncore export --verify` re-verifies signatures instead of reporting only cached
  status:
  <https://nshielddocs.entrust.com/security-world-docs/v13.7.3/hsm-user-guide/hsm-mgmt/nshield-audit-log-service.html>.
- Entrust documents that Transaction IDs are UTF-8 strings of at most 88 bytes, that the
  environment route suits a self-contained process such as SignTool, and that linkage may not
  survive commands implemented internally rather than directly forwarded:
  <https://nshielddocs.entrust.com/security-world-docs/v13.7.3/api-ncore/transaction-ids.html>.
- Entrust documents `Cmd_Sign` as conditionally producing `ObjectUse`, `ObjectNew` as the
  object-identity origin, and `{runID,objid}` rather than `objid` alone as the stable join:
  <https://nshielddocs.entrust.com/security-world-docs/v13.7.3/hsm-user-guide/hsm-mgmt/audit-logging.html>.
- Entrust documents that a non-persistent Operator Card Set (OCS) keeps protected
  keys usable while the last required card remains inserted; `K/N` states the threshold and
  total, not a fresh authorization for each cryptographic operation. The card set becomes
  unavailable only after the last required card is removed. Entrust's `slotinfo` contract makes
  absence machine-observable: `Token=-`, `IC=0`; `R`, `D`, `a`, `t`, and `f` identify remote,
  dynamic, associated-dynamic, timed-out, and failed-secure-channel slot states:
  <https://nshielddocs.entrust.com/security-world-docs/security-manual/access-control.html>,
  <https://nshielddocs.entrust.com/security-world-docs/utilities/slotinfo.html>.
- Microsoft documents exact SignTool `/sha1`, `/fd`, `/p7`, `/p7ce`, and `/p7co` behavior;
  `CryptMsgControl(CMSG_CTRL_VERIFY_SIGNATURE_EX)` is the explicit signature oracle; the chain
  flags named by `online-v1` cover chain-excluding-root revocation and a cumulative retrieval
  timeout; CiTool's supported policy mutations are pathname-based update/remove; and
  `ATTACH_VIRTUAL_DISK_FLAG_READ_ONLY` requests a read-only attachment:
  <https://learn.microsoft.com/en-us/windows-hardware/drivers/devtest/signtool>,
  <https://learn.microsoft.com/en-us/windows/win32/api/wincrypt/nf-wincrypt-cryptmsgcontrol>,
  <https://learn.microsoft.com/en-us/windows/win32/api/wincrypt/nf-wincrypt-certgetcertificatechain>,
  <https://learn.microsoft.com/en-us/windows/security/application-security/application-control/app-control-for-business/operations/citool-commands>,
  <https://learn.microsoft.com/en-us/windows/win32/api/virtdisk/ne-virtdisk-attach_virtual_disk_flag>.
- Microsoft documents symmetric CreateFile sharing: an existing incompatible access/share
  request makes a later open fail, and the share disposition remains active until the handle is
  closed. This supports the retained-handle probes but does not turn a pathname consumer into a
  handle consumer:
  <https://learn.microsoft.com/en-us/windows/win32/api/fileapi/nf-fileapi-createfilew>.

## Threat model and trust boundaries

| Boundary | Attacker position | Reachable assets | Required mediation |
|---|---|---|---|
| Policy authoring | Compromised dependency or continuous-integration pipeline | Runtime/module rows, AppID, PolicyID/BasePolicyID, options, unsigned CIP | Pinned ordinary-hash extractor; independent PE/version proof; deterministic XSD author; implementation-independent XML/CIP verifier; two-build reproducibility; disagreement is zero signing/deployment |
| Signing station and HSM | Insider or compromised signing-station dependency | Non-exportable content key, OCS-loaded authorization window, signed supplemental authority | One isolated `CstPolicySigningOwner`; exact request display; global exclusive ceremony; exact key/cert/tool pins; one admitted Sign; immediate final-card removal and independently proved token/key unload; audit range excludes every other use of the content key |
| Audit collector/export | Insider or compromised collector dependency | Request/key-use evidence, ESN/logID/index continuity, vendor-verification result | One privileged per-ESN collector; explicit config/service/database/binary identity; estate-wide sole-collector attestation; fresh held JSON `export --verify`; strict grammar/projection; continuous replay journal; no raw-KML or result-byte claim |
| Signed artifact import | Insider or compromised removable-media path | Signed CIP bytes and deployment authority | Held no-follow input; exact request/receipt/artifact hashes; explicit single-signer signature/content/current-chain/online-revocation verification; copy into protected VHDX; read-only reattach; retained namespace through CiTool and settlement |
| Machine policy lifecycle | Local administrator or external management authority | Complete active policy composition and unrelated applications | One policy-set owner/mutex/journal; complete authority-attributed inventory; selected-family union and other-base intersection proof; external-writer quiescence; CiTool-only mutation; reboot-observed commit/absence; sibling and unrelated canaries |
| Runtime image admission | Compromised package DLL or malicious local file author | Frontend capability, worker roots, embedded Python, CST workspace | Native pre-main handle revocation; exact committed policy receipt; enforced per-app User Mode Code Integrity denial before first instruction; read-only VHDX and retained namespace/stream closure; closed loader/PyConfig; all-return settlement |
| MCP and publication | Unauthenticated remote caller or malicious input author | Source identity, paths, capabilities/nonces, diagnostics, proprietary values | Caller selects no authority; fixed bounded request/result schemas; machine-neutral allowlist manifest; path and credential detectors fail closed; seventh tool default-off; existing six unchanged |

## Finding disposition

The prior `SEC-C6-SIGN-08` is corrected at design level: one isolated
`CstPolicySigningOwner` owns the non-exportable RSA-3072 key path, target Windows has no
signing route, explicit `CryptMsgControl(CMSG_CTRL_VERIFY_SIGNATURE_EX)` authenticates one
pinned signer and exact content, `online-v1` fails hard, and storage/use/leak/rotation owners
are present. The collector/export/replay correction is also truthful: the pinned vendor tool
alone re-verifies KML, while P18 validates the held output and correlates the request/key use.

### SEC-C6-OCS-09 — corrected at design level

The corrected design makes interval authority explicit. `CstPolicySigningOwner` owns the
station-global lock and the only state machine from an unavailable key, through one terminal
SignTool child, to final-card removal, all-local-slot `Token=-`/`IC=0` readback, key
unavailability, continuous audit settlement, and handoff. The owner rejects remote, dynamic,
associated-dynamic, timed-out and failed-secure-channel slot states; retains the lock through
settlement; and allows no export, release, retry, shutdown, or success receipt before closure.

The fresh vendor-verified interval begins at a pre-load `{ESN,logID,index}` anchor and ends only
after unload proof. It admits exactly one request-Transaction-ID-equal successful Sign and its
exact-key ObjectUse/ObjectNew join; every sibling, failed, differently tagged, untagged, or
key-ambiguous use rejects. Every cancellation, timeout, crash, retained/reinserted/stuck card,
provider leak, collector gap, restart, or rotation path remains cleanup-only, durably
quarantined, and default-off. This closes the prior sibling-signature window without treating
K-of-N as per-operation authentication or treating operator assertion/SignTool exit as an
unload oracle. Implementation and installed-profile evidence remain mandatory.

## Required constraints and abuse probes

| Required constraint | Mandatory falsifying probe |
|---|---|
| The OCS-loaded interval admits exactly one request-bound key use and ends before handoff. | Second-process/thread/provider Sign; different/absent Transaction ID; card retained; unload/readback failure; extra ObjectUse; crash/timeout/rotation during loaded state; require no accepted artifact and zero CiTool. |
| One per-ESN collector owns explicit configuration, service/database identity and fresh vendor verification. | Two/unprivileged collectors; absent/empty/catch-all modules; environment/config/database/exporter/output substitution; cached/text/malformed/overwrite export; require signing disabled. |
| Audit replay is continuous, request-bound and key-bound without claiming result-byte attestation. | ESN/logID/index gap/reset/reorder/replay; `{runID,objid}` reuse; wrong/missing ObjectNew key hash; wrong Sign status/transaction; crash at prepared/completed boundaries; require no second CiTool call. |
| Signature verification is explicit and independent of decode/content extraction. | Preserve embedded CIP while corrupting signature; alter signer count, DER/SPKI/OID/RSA/digest/attributes/chain/revocation; require zero import/deploy. |
| `online-v1` is current-time, exact-chain, bounded and fail-hard. | Offline/unknown/stale/timeout/revoked/alternate chain, ambient cache conflict, missing distribution data, or `offline-v1`; require zero CiTool. |
| Policy author and verifier are independent and only exact runtime/AppID/hash semantics survive. | Pin, hash, version-resource, AppID, scope, XML/XSD/CIP grammar, reference, option, signer and two-build mutations; require `policy_artifact_unavailable`. |
| Signed-policy import retains exact media-to-VHDX bytes and namespace through the pathname consumer. | Media/leaf/ancestor/reparse/hard-link/alternate-stream/write/delete mutation; detach/remount/backing swap during engineered CiTool delay; incompatible coexistence means no reopen fallback. |
| Complete App Control composition and all external writers stay closed through commit/absence. | Base/supplemental/audit/AllowAll/pending/external-policy mutation; Group Policy/MDM/CSP/WMI race; missing reboot or changed unrelated canary; sampler remains absent. |
| Native runtime executes package code only after revocation and enforced exact-policy admission. | Direct/forwarded/indirect/native malicious TLS/DllMain/CPython/extension/callback loads; every unlisted image must be denied before its first-instruction canary. |
| All returns preserve positive-enable/default-off and exact cleanup ownership. | Missing receipt/pin/detector/policy/VHDX/namespace/unload proof across fresh start, restart, rollback, cancel, timeout and crash; no uvx, wrapper, weaker policy, reopen, automatic retry, or path fallback; existing six A/B unchanged. |

## Secret and credential four boxes

| Item | Storage owner | Injection/use path | Log and serialization exclusion | Rotation or revocation owner |
|---|---|---|---|---|
| Policy-signing private key | `CstPolicySigningOwner`; exact non-exportable RSA-3072 key inside pinned nShield HSM under non-persistent OCS; no target key/provider/endpoint/PFX/PEM | Exact pinned CNG KSP/certificate/key and one SignTool child for one displayed request; no key/PIN/password bytes in argv, environment, pipe, file, or target | Canary scans request/media/stdout/stderr/log/dump/journal/manifest/temp/publication; only public pins/hashes and strict vendor JSON projection may serialize | Owner removes the final required card, proves all-slot absence and key unload, settles the complete audit range before handoff, and requires old-key disable/destroy plus the same closed interval for rotation |
| HSM audit authority | Device-protected KML; one privileged per-ESN `NShieldAuditCollectorOwner` owns explicit service/config/database | HSM produces native rows; pinned `nshieldaudit ... export --verify` owns KML/warrant verification; P18 only pins adapter/output and correlates strict rows | KML/private KNETI/database/raw segment/collector credential excluded from request, media, receipt, logs and publication; held verified JSON is public audit evidence | Collector owner finalizes/verifies old range, stops service for backup, preserves terminal identity, and admits only exact continuation or reviewed successor genesis |
| Service identities | Local Security Authority and Service Control Manager; credential-free fixed service identities | Session-0 SCM tokens and fixed protected pipe descriptors only | No password/token/license value in policy, argv, environment, protocol, logs, dumps, manifests, MCP result | Provisioner stops exact services, proves processes/Jobs/pipes/handles absent, replaces pins, and revalidates |
| Frontend capability and protocol nonces | Existing T02 native buffer/anonymous pipe and bounded daemon/broker ledgers | Exact fourth frontend handle; exact broker exchanges; environment carries only a decimal non-secret locator | Capability/nonces excluded from argv/env/manifest/log/dump/result; native/Python buffers zero on all returns | Enrollment/challenge/broker ledger owners atomically consume/cancel on success, failure, expiry, disconnect, exit, shutdown, restart |
| Worker source/workspace capabilities | Broker original/duplicate handle tables, then native worker table | Exact five-handle inheritance epoch; two non-secret locators only in bounded native prelude | No path, token, policy, Python object or authority in argv/env/log/public output | Broker/native owners clear inheritance, close parent/child copies, settle Job/root, and quarantine ambiguity |

## Exact 18 S4 security claims

`verified-design` means the immutable design supplies a coherent guarantee, one owner, and a
falsifying probe. Target execution evidence is not inferred. The table contains exactly 18
claim rows.

| Claim | S4 verdict | Exact guarantee, single owner, and enforcement probe |
|---:|---|---|
| 1 | verified-design | `{ guarantee: caller data selects no authority and stale swapped duplicate unknown or ambiguous entry performs zero broker source worker or CST work; single-owner: daemon AuthoritySnapshot then broker reauthorization; enforcement-probe: entry and revision zero-work matrix }` |
| 2 | verified-design | `{ guarantee: enrollment trusts only the kernel-authenticated current supervisor and exact CST task process; single-owner: daemon supervisor-status authenticator; enforcement-probe: squatter PID-reuse token session image and fabricated-row matrix }` |
| 3 | verified-design | `{ guarantee: daemon identity receives only the closed CST identity operation and no generic control right; single-owner: SupervisorCstIdentityAuthorizerV1; enforcement-probe: opcode denial and supervisor-state equality }` |
| 4 | verified-design | `{ guarantee: all three endpoints use exact runtime SIDs protected DACL High SACL ordered masks and readback; single-owner: each endpoint descriptor owner; enforcement-probe: owner ACE order mask label anonymous network and second-instance mutation }` |
| 5 | verified-design | `{ guarantee: one capability reaches only the exact native frontend and is revoked closed and zeroed before package or Python code; single-owner: T02 launch owner then native frontend and enrollment ledger; enforcement-probe: exact-four direct-child pre-entry and all-return secret canaries }` |
| 6 | verified-design | `{ guarantee: enrollment nonce and capability ledgers terminate independently without replay or stranded authority; single-owner: HubEnrollmentProtocolV1 ledgers; enforcement-probe: ACK-loss cancel expiry reconnect exit shutdown restart table }` |
| 7 | verified-design | `{ guarantee: frontend request binds authenticated direct image capability challenge correlation hash generation entry and deadline before admission; single-owner: daemon frontend challenge/admission owner; enforcement-probe: clone replay package-load and request-forgery matrix }` |
| 8 | verified-design | `{ guarantee: daemon alone owns admission and broker route while same-process cst.py owns compatibility and publication; single-owner: daemon composition then frontend publisher; enforcement-probe: dependency topology and no-direct-route graph }` |
| 9 | verified-design | `{ guarantee: broker admits only the current SCM daemon after mutual token service session image impersonation and proved revert; single-owner: broker pipe authenticator; enforcement-probe: identity and failed-revert matrix }` |
| 10 | verified-design | `{ guarantee: broker nonce correlation request policy manifest and QPC binding consumes or cancels atomically on all exits; single-owner: BrokerProtocolV1 NonceLedger; enforcement-probe: replay framing timeout disconnect shutdown table }` |
| 11 | verified-design | `{ guarantee: broker alone creates least-right source and workspace capabilities and missing readback creates no child or path fallback; single-owner: broker capability owner; enforcement-probe: open duplicate access share reparse identity and unavailable-proof matrix }` |
| 12 | verified-design | `{ guarantee: worker revokes five flags then admits only exact committed-policy package code and closed Python; single-owner: native pre-main then NativePackageLoadOwner; enforcement-probe: malicious dynamic-load denial before entry plus VHDX namespace and PyConfig matrix }` |
| 13 | verified-design | `{ guarantee: no sibling or descendant receives or retains worker handles; single-owner: WorkerInheritanceEpoch then native worker and Job owner; enforcement-probe: concurrent child inherited and explicit duplication matrix }` |
| 14 | verified-design | `{ guarantee: owner-local receipts prove exact lifecycle and cannot be forged or defaulted; single-owner: typed receipt owners and WindowsContainedInvocation; enforcement-probe: every-return missing contradictory and residual-state matrix }` |
| 15 | verified-design | `{ guarantee: one unchanged QPC triple crosses all owners and only cleanup receives a post-termination budget; single-owner: daemon AbsoluteInvocationBudget; enforcement-probe: altered frequency tick deadline and every-stage delay matrix }` |
| 16 | verified-design | `{ guarantee: existing six retain their contract and the seventh has exactly one protected policy loader process and protocol route; single-owner: direct profile T02 native package owner daemon and broker; enforcement-probe: A-B route residue dynamic-loader namespace and no-fallback graph }` |
| 17 | not-verifiable (target-only) | `{ guarantee: installed CST under fixed broker identity accepts locked inputs sealed output exact Job no-breakaway no-console and preserves foreign CST; single-owner: installed CST and broker admission record; enforcement-probe: disposable-target principal DACL path-lock ResultTree descendant breakaway license COM hidden-call and foreign-preservation trace }` |
| 18 | verified-design | `{ guarantee: dependencies services signing key policy artifact package identities capabilities ACLs and rollback rotate only under exact owners while incomplete state stays default-off; single-owner: provisioner CstPolicySigningOwner NShieldAuditCollectorOwner CstAppControlPolicySetOwner T02 enrollment broker and NativePackageLoadOwner; enforcement-probe: dependency provenance OCS-loaded interval second-sign final-card-removal all-slot key-unload continuous-audit exclusivity rotation policy composition rollback publication and restart matrix }` |

Matrix total: exactly 18 claims; 17 `verified-design`; 1
`not-verifiable (target-only)`. Architecture Claims 7 and 15 and security Claim 17 remain
target-only and are not inferred.

## Residual empirical obligations

- Implementation must prove the exact OCS state machine, pinned `slotinfo` all-local-slot
  absence, no sibling/provider key use, and the continuous pre-load-through-post-unload
  vendor-verified audit interval on the admitted nShield profile.
- Implementation still must prove parser independence, exact signed-envelope authenticity,
  current online revocation, collector continuity/replay recovery, media-to-VHDX identity,
  complete App Control composition, reboot commit/absence, malicious-load denial before first
  instruction, namespace/stream continuity, and all-return settlement.
- A disposable or reserved Windows/CST target must prove AppID/hash behavior, CiTool/VHDX
  coexistence, external-writer quiescence, installed CST behavior, architecture Claims 7 and 15,
  and security Claim 17. Metadata, command success, or an unengineered event log is insufficient.
- No publication-safety exception is approved.

## Gate

**PASS.** All 18 S4 claims now have coherent design-level controls, one named owner, and a
falsifying probe; Claim 17 remains explicitly target-only. The prior OCS sibling-signature gap
is closed by mandatory final-card removal, supported all-local-slot absence readback, proved key
unavailability, and one continuous vendor-verified pre-load-through-post-unload range that
rejects every sibling or ambiguous use. Collector/replay, exact signed-envelope verification,
fail-hard online revocation, deterministic AppID/hash authoring and independent verification,
complete App Control lifecycle, retained read-only-VHDX pathname handoff, native pre-main and
dynamic-load controls, and all-return default-off settlement are coherent at design level.
Ready only for an independent `security-reviewer` on these exact hashes. This verdict authorizes
no implementation, signing, HSM ceremony, App Control/VHDX mutation, deployment, registration,
publication, release, or target inference.

## Terms and Abbreviations

- AppID — App Control per-application scope selector.
- CIP — compiled Code Integrity policy.
- CNG KSP — Cryptography Next Generation key storage provider.
- CST — CST Studio Suite.
- DACL / SACL — discretionary / system access control list.
- HSM — hardware security module.
- KML — nShield audit-log signing key.
- MCP — Model Context Protocol.
- OCS — nShield Operator Card Set.
- P18 — provisioning and policy-lifecycle phase.
- PKCS #7 — Public-Key Cryptography Standards signed-message format.
- QPC — Query Performance Counter.
- SCM — Service Control Manager.
- SID — security identifier.
- UMCI — User Mode Code Integrity.
- VHDX — Hyper-V virtual hard-disk image format.
