# Architecture Review — OCS final-card unload and complete key-use interval

Reviewed exact corrected design SHA-256
`7423D56DD33394336A06AB8C515D12F4496B4AC0533F2901BF5A0EE1436756ED`,
proposed decision SHA-256
`49DE418E1EB95E567C1B6AA18C36A124A9EC7075AE0183FD67B0D5072537177B`,
prior architecture `PASS` SHA-256
`9D38E08AEF593EA4BC32C14D98930061AA4086AA7B6103A7B826F8BA6B7712EC`,
and independent security `REVISE` SHA-256
`9AA793CA9E9C772902622D6ABC5AF4CDD8C731EBA3CCF6C71DF02F9F40514E0D`.

This is an independent architecture-design re-review. It authorizes no implementation,
planning, decision promotion, signing, hardware security module (HSM) operation, App Control
or virtual hard disk (VHDX) mutation, deployment, registration, publication, release, or
target-runtime inference.

## Reviewed surfaces and evidence

- Corrected design: `design.md:232-537,557-645,677-768,1017-1040,2028-2144,
  2516-2617,2619-2679,2712-2799`.
- Matching proposed decision: `work-items/decisions/
  2026-08-12-cst-saved-field-authority-containment.md:284-430,907-960`.
- Prior architecture acceptance of the collector, replay, signed-policy staging, App Control,
  VHDX, native loader, and process-containment owners: `architecture-review.md` at exact prior
  SHA-256 `9D38E08A...B7712EC`.
- Security finding `SEC-C6-OCS-09`, including its loaded-interval abuse case, required
  correction, and falsifying probe: `security-constraints.md` at exact SHA-256
  `9AA793CA...514E0D`.
- CodeGraph Model Context Protocol (MCP) was used before file inspection. It returned current
  on-disk source for `HostConfig.LaunchCapability -> prepareLaunchCapability ->
  preparedLaunchCapability.apply/start/cancel -> windowsLaunchCapabilityPipe.apply ->
  AdditionalInheritedHandles`. The only pending-index notice named unrelated
  `internal/vcpkgmcp/lastfailure/producer_limits_test.go`; no stale target file was used.
- Entrust primary documentation confirms the decision-driving properties:
  [createocs 13.7.3](https://nshielddocs.entrust.com/security-world-docs/v13.7.3/utilities/createocs.html),
  [slotinfo](https://nshielddocs.entrust.com/security-world-docs/utilities/slotinfo.html),
  [13.7.3 Security Manual](https://nshielddocs.entrust.com/security-world-docs/nshield-security-world-v13-7-3-security-manual.pdf),
  [Transaction IDs](https://nshielddocs.entrust.com/security-world-docs/api-ncore/transaction-ids.html),
  [Audit Logging](https://nshielddocs.entrust.com/security-world-docs/v13.7.3/hsm-user-guide/hsm-mgmt/audit-logging.html),
  and [nShield Audit Log Service](https://nshielddocs.entrust.com/security-world-docs/v13.7.3/hsm-user-guide/hsm-mgmt/nshield-audit-log-service.html).

Entrust documents that a non-persistent Operator Card Set (OCS) keeps its protected keys
usable only while the final required card remains inserted and removes those keys from HSM
memory when that card is removed. `slotinfo -m` enumerates the module's slots; `Token=-` and
`IC=0` mean no token, while `R/D/a/t/f` identify remote, dynamic, associated-dynamic, timeout,
or failed-secure-channel states. Audit documentation supports one collector client per HSM,
privileged-client enrollment for network 5c collection, fresh `export --verify`, transaction
identifiers, `Sign` plus `ObjectUse`, and the `{runID,objid}` to `ObjectNew` identity join.

## Prior finding dispositions

| Finding | Disposition | Evidence |
|---|---|---|
| `SEC-C6-OCS-09` — the OCS-loaded interval permitted a sibling signature before final-card removal | fixed | One `CstPolicySigningOwner` now owns `UNAVAILABLE` through `AUDIT_RANGE_SETTLED`; it holds a station-global lock, closes the signer/provider handles, requires final-card removal, verifies every admitted local slot as absent, and settles one continuous vendor-verified pre-load-to-post-unload range before handoff (`design.md:263-321,421-435`; decision lines 296-355,383-410). |
|  |  | The range admits exactly one request-bound successful `Sign`/`ObjectUse`, joins its exact key through `{runID,objid}` and `ObjectNew`, and rejects every second, failed, transaction-mismatched, exact-key, or key-ambiguous use. |
| `AR-C6-AUDIT-14` — collector/export evidence boundary | fixed and preserved | Exactly one `NShieldAuditCollectorOwner` retains explicit ESN/config/service/database/export identities and fresh held JSON `export --verify`; P18 consumes the vendor-authenticated result without claiming raw-KML parsing (`design.md:472-518`). |
| `AR-C6-AUDIT-12` — audit replay ownership | fixed and preserved | One append-only `AuditReceiptReplayOwner` admits reviewed genesis plus continuous strictly greater ranges, exact prepared recovery, or byte-identical completed replay; ambiguity cannot issue another CiTool call (`design.md:520-537`). |
| `AR-C6-HANDOFF-13` — pathname-only CiTool staging | fixed and preserved | One import owner retains media-to-read-only-VHDX content and namespace identities through CiTool and immediate settlement; incompatibility has no reopen or media-path fallback (`design.md:580-610`). |

## Architecture assessment

| Surface | S4 verdict | Assessment |
|---|---|---|
| Non-persistent OCS semantics | verified | The design uses the vendor property at its exact strength: interval authority ends only when the final required card is removed; it does not treat quorum as per-operation approval. |
| Physical unload readback | verified | Observed removal plus pinned exact-module `slotinfo` requires `Token=-`, `IC=0` on every enumerated local slot and rejects every remote/dynamic/ambiguous state. Timeout, SignTool exit, a wrapper flag, and operator assertion are explicitly non-authoritative. |
| One-use interval | verified | A pre-load vendor anchor precedes insertion; post-unload fresh vendor verification must cover the exact continuous range and reject every extra/failed/ambiguous Sign or exact-key ObjectUse. |
| Process and handle lifetime | verified | The station lock spans signer/provider close, card removal, slot readback, export, and audit settlement. No artifact release, retry, rotation, shutdown completion, or lock release may precede closure. |
| Failure, card-stuck, timeout, and restart | verified | All returns converge on the same terminalization. Unproved removal/readback/audit continuity writes durable `OCS_INTERVAL_QUARANTINED`; recovery is cleanup-only and cannot sign, rotate, retry, or hand off. |
| Collector and exporter | verified | One per-ESN owner controls the explicit collector and held output; pinned vendor `--verify` owns KML verification. A second collector, split history, cached verification, drift, or cleanup ambiguity disables signing. |
| Replay and policy deployment | verified | The replay journal serializes exact ranges and prepared settlement before independent signed-CIP verification or any CiTool effect; policy deployment remains under one separate policy-set writer. |
| Signed-policy VHDX handoff | verified | The staging owner retains the same backing image, read-only attachment, volume/mount/ancestor/leaf chain, and exact pathname through the one CiTool call and settlement. |
| Existing runtime launch owners | verified | Fresh CodeGraph evidence preserves the existing Go frontend launch lifecycle and Windows handle adapter; the correction introduces no competing source-process owner. |
| Target behavior | not-verifiable (with reason) | Exact installed nShield, Windows App Control, VHDX/CiTool, CST activation, and Line10 behavior require the later engineered target probes; this design review does not infer them. |

## Architecture laws and anti-layering verdict

| Defect class | Verdict | Reason |
|---|---|---|
| OCS interval authority | CLEAN-SINGLE-OWNER | `CstPolicySigningOwner` alone owns the state machine, station lock, card-removal gate, and ceremony-local receipt. |
| Physical token absence | JUSTIFIED-DEPTH | Vendor non-persistence, observed card removal, and vendor slot readback cross physical/operator, HSM, and tool trust boundaries; the target no-card signing probe is later corroboration and explicitly cannot substitute for production unload proof. |
| Audit collection and KML verification | CLEAN-SINGLE-OWNER | One collector owns lifecycle/output; the pinned vendor exporter alone owns KML verification; P18 validates the handoff instead of duplicating KML cryptography. |
| Audit replay | CLEAN-SINGLE-OWNER | P18 alone owns durable index advancement, crash recovery, and exactly-once CiTool admission. |
| Policy staging and lifecycle | CLEAN-SINGLE-OWNER | The import owner owns pathname staging; `CstAppControlPolicySetOwner` alone mutates the sampler supplemental and emits committed/absent events. |
| Runtime launch and load | CLEAN-SINGLE-OWNER | Existing `StdioHost` owns frontend creation, broker owns worker creation, and native loader owns post-revocation package loading. |
| Failure idiom | CLEAN-SINGLE-OWNER | Leaves return typed failures; composition/lifecycle owners select quarantine, cleanup, or termination. No sibling failure idiom is introduced. |

## Diff-invisible invariants

| Invariant | S4 verdict | Named falsifying probe |
|---|---|---|
| No ceremony handoff while any admitted card/token state remains present or ambiguous | verified | Retained/reinserted/stuck card; wrong module; each local slot present or IC nonzero; R/D/a/t/f flags; slotinfo timeout/parse/identity failure; require quarantine, no receipt, no CiTool. |
| The complete OCS-loaded interval contains exactly one intended key use | verified | Second process/thread/provider handle; different, absent, duplicate, or truncated transaction identifier; failed Sign; extra exact-key ObjectUse; ambiguous ObjectNew join; require no accepted artifact. |
| Card removal cannot race export, release, retry, rotation, shutdown, or restart | verified | Pause at every state transition; crash and restart before/after signer close, removal, slot readback, exporter start, output flush, and journal append; only cleanup may resume. |
| Exactly one collector obtains a continuous per-ESN history | verified | Dual/unprivileged collector; empty/catch-all/wrong YAML; service/database/exporter substitution; split/gapped indexes; require signing disabled. |
| Replay ambiguity cannot cause a second deployment mutation | verified | Crash at export/prepared/CiTool/completed boundaries; only exact prepared settlement or byte-identical completed replay is accepted. |
| CiTool consumes only the continuously held read-only staged artifact | verified | Media/leaf/ancestor/stream/backing mutation, detach/remount, VHDX swap, or lock incompatibility yields zero fallback. |
| Existing six tools remain compatible and the sampler remains default-off | verified | Legacy/direct A/B plus sampler-absence checks across every OCS, collector, policy, VHDX, runtime, and target-admission failure. |

## Exact 34-claim S4 mapping

`verified` means the immutable design supplies a coherent guarantee, one owner, and a
falsifying probe. It does not claim implementation or target-runtime completion. This table
has exactly 34 claim rows.

| Claim | S4 verdict | Owner and probe disposition |
|---:|---|---|
| 1 | verified | CST composition/direct materializer; existing-six wire/stdio A/B and inventory. |
| 2 | verified | Saved-field application owner; restart replay and no Job-state edge. |
| 3 | verified | Sampler FastMCP policy; validation and forbidden solve/remesh scan. |
| 4 | verified | Broker containment/native revocation/transfer; PE, handle and byte-continuity matrices. |
| 5 | verified | `SourceSnapshot`; per-role mutation suppresses success. |
| 6 | verified | `FrameResolver`; permutation, ambiguity and no-filename-fallback table. |
| 7 | not-verifiable (target-only) | CST vendor port; installed-CST activation/header/ResultTree trace remains mandatory. |
| 8 | verified | `SavedFieldResponseV1`; raw order and fixed value-kind checks. |
| 9 | verified | Response zero classifier; signed-zero/nonzero table. |
| 10 | verified | Coordinate contract/`UnitTransform`; pre/post validation and exact scaling. |
| 11 | verified | `OwnedSamplerSession`; attributed handle and foreign-process guard. |
| 12 | verified | `CallSettlement`; every return echoes complete receipts. |
| 13 | verified | MCP publisher; bounded canonical result and redaction checks. |
| 14 | verified | Test-only comparator; no production Line10/vector finite-element-method edge. |
| 15 | not-verifiable (target-only) | Line10 comparator; independent native producer and four-call agreement remain target evidence. |
| 16 | verified | Direct materializer/native closure/StdioHost/daemon/broker; topology, A/B and stale-route probes. |
| 17 | verified | `open_owned_sampler_session`; complete-token transfer and rollback. |
| 18 | verified | Neutral port; import graph and CST-object-free contract. |
| 19 | verified | Workspace factory; partial-child removal and sibling preservation. |
| 20 | verified | `AuthorizedBundleTransfer`; no-follow identity and mutation matrix. |
| 21 | verified | SCM-daemon absolute budget; exact-limit, one-over and all-stage delays. |
| 22 | verified | CST adapter; malformed record and bounded aggregate validation. |
| 23 | verified | Daemon authority snapshot then broker authorization; stale/swapped/duplicate zero-work matrix. |
| 24 | verified | Signing, collector, replay, import, policy, containment and native owners; every-return matrix includes retained/stuck cards, all-slot readback, provider handles, siblings, collector/export, restart and VHDX/policy settlement. |
| 25 | verified | `TrustedWorkspacePolicy`; owner/access/locality/reparse matrix. |
| 26 | verified | Workspace snapshot/vendor lease/isolation owner; principal/share/seal/swap probes. |
| 27 | verified | Broker epoch then native revocation; five handles, PE/TLS/disassembly and descendant probes. |
| 28 | verified | Protocol/native-prelude/receipt owners; framing and settlement matrices. |
| 29 | verified | Existing T02 `exec.Cmd` plus provisioner/frontend ledger; four-tuple and duplicate-owner probes. |
| 30 | verified | `CstPolicySigningOwner`, collector and replay owner; final-card/all-slot unload, exact one-use range, second signer, provider leak, crash/restart/rotation and direct-target-sign probes. |
| 31 | verified | Admission gate and local receipts; linearization, quarantine and restart trace. |
| 32 | verified | Ceremony/collector/replay/import/policy/runtime chain; zero-source matrix includes unload, sibling-use, export, replay, signature, VHDX and commit failures. |
| 33 | verified | Native revocation then unload/audit-bound verified CIP and VHDX/load owners; malicious-load and all-settlement matrices. |
| 34 | verified | `WindowsPathIdentityV1` and vendor lease; namespace/share/principal/swap matrix. |

Matrix total: exactly 34 claims; 32 `verified`; 0 `failed`; 2
`not-verifiable (target-only)` (Claims 7 and 15 only).

## Residual risk and next owner

- Exact Security World 13.7.3 binary identities, OCS configuration, every-slot output grammar,
  HSM key-unavailability behavior, complete audit JSON, sole-collector estate attestation, and
  fault-injected restart behavior still require later provisioning/ceremony evidence.
- App Control composition, VHDX/CiTool coexistence, reboot settlement, deny-before-entry,
  installed CST behavior, Claim 7, Claim 15, and Line10 remain target gates.
- The next required independent role is `security-engineer`, followed by a separate
  `security-reviewer`, both bound to these exact replacement hashes before decision promotion.

## Gate

**PASS.** `SEC-C6-OCS-09` is corrected at design/decision level without weakening the prior
collector, replay, signed-policy staging, App Control, VHDX, native-loader, or containment
owners. The ceremony now has one explicit non-persistent OCS authorization interval: a fresh
vendor-verified pre-load anchor; one station-global owner and lock; one terminal SignTool
child; provider/key-handle close; mandatory final-card removal; exact-module readback proving
every admitted local slot has `Token=-`, `IC=0`, and no remote/dynamic/failure state; then one
continuous post-unload vendor-verified range with exactly the intended transaction-bound
Sign/ObjectUse and no sibling, failed, exact-key, or ambiguous use. Card-stuck, readback,
provider, collector, export, timeout, cancellation, crash, restart, rotation, cleanup, replay,
policy, and VHDX failures remain durable default-off quarantine with no retry, alternate
signing path, second CiTool call, or staging fallback. Claims 7 and 15 alone remain target-only.
Proceed to fresh independent security gates on these exact hashes; this verdict authorizes no
implementation, signing, HSM operation, policy/VHDX mutation, deployment, registration,
publication, or release.

## Terms and Abbreviations

- CIP — compiled Code Integrity policy.
- CNG — Cryptography Next Generation.
- CST — CST Studio Suite.
- HSM — hardware security module.
- IC — insertion counter reported by `slotinfo`; zero means no token is present.
- KML — nShield module audit-log signing key.
- MCP — Model Context Protocol.
- OCS — Operator Card Set.
- P18 — provisioning and policy-lifecycle phase.
- S4 — per-claim verdict vocabulary: verified, failed, or not-verifiable.
- VHDX — Hyper-V virtual hard-disk image format.
