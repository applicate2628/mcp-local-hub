# PR #588 architecture review round 1

Date: 2026-07-27

## Gate status

Both independent read-only Codex CLI reviews returned substantive `REVISE`
artifacts, but neither counts as a completed external gate: the Windows
transport batch invoked `codex.cmd` without `call`, so batch control never
returned to persist the required exit-code file.

| Lane | Output | SHA-256 | Artifact verdict | Completion oracle |
| --- | --- | --- | --- | --- |
| Claim verification | `.scratch/external-reviews/claim.out` | `14E304836034D2D54FF2851CF4FA58F382E80A1E412EABADF09E7C672EA9A35B` | `REVISE` | `UNVERIFIED`: output/header/gate present; no auth/quota/truncation marker; process exit not recorded |
| Adversarial recovery | `.scratch/external-reviews/adversarial.out` | `32D879C40A3D8BFC4DFF7044C678869D77BCB4E67974AE1566218150B06D504F` | `REVISE` | `UNVERIFIED`: output/header/gate present; no auth/quota/truncation marker; process exit not recorded |

The outputs remain accepted as read-only finding evidence after the Lead
independently reproduced their source traces. They are not accepted as the
final architecture gates; corrected `call`-based transports must re-run both
lanes after implementation and QA.

## Source-verified revision classes

| Class | Root source trace | Required contract revision |
| --- | --- | --- |
| F1 split authorization/mutation | Serena separates pre-fingerprint, backup, journal prepare, and `AddEntry`; LSP separates pre-state check, backup, prepare, and add/remove | Mutate only through an adapter-owned conditional critical section, or perform no mutation and retain pending state |
| F2 value equality is not causation | `settleMCPFrontReconcileAttempts` treats durable `prepared` and no-write precondition `conflict` alike and promotes when live equals intended | Re-entry cannot auto-promote durable prepared/conflict rows by state equality alone; absence of a durable post-observation is unresolved ownership |
| F3 Serena absent inverse | Owned rollback always calls restore-from-bytes, whose CAS core refuses a snapshot with no Serena entry | Branch on immutable baseline: present restores pinned bytes; absent performs CAS-guarded removal |
| F4 dependency barrier retry | The CLI omits terminal legacy conflict rows from the next recovery input; baseline-only legacy rows are not live-verified | Reconstruct every dependency group from all persisted rows on every rollback and require proved live/restored legacy readiness before canonical inversion |
| A-01 pin authority split | Rollback consumes `Rows`, while pin verification iterates `Serena.Applied`; a valid row can bypass path/checksum validation | Version-3 `Rows` is the sole authority and every Serena row resolves to exactly one matching pin before any write |
| A-02 non-falsifying probes | Claim-9 helper test does not call a real mutator; claim-16 helper test does not drive the caller's durable re-read | Add mutation-order and stale-memory-versus-durable-retirement tests that fail under the corresponding production mutations |
| A-03 missing durable decision | The cross-package persisted-state contract has no decision-registry owner | File and reference one architecture decision before implementation |
| Superseded helpers | Pre-version-3 merge/commit/fingerprint/verification helpers remain test-referenced beside the new row owner | Delete superseded decision logic or make any retained projection purely derived and non-authoritative |

## Safety

The reviewers were read-only and ran no Go command, application, Graphical User
Interface (GUI), tray, or supervisor. The transport defect affected evidence
completion only; it did not mutate repository or operator state.

## Gate

`REVISE` — architect decision required before implementation.

## Terms and Abbreviations

- CAS: compare-and-set.
- GUI: Graphical User Interface.
- LSP: Language Server Protocol.
- QA: Quality Assurance.
