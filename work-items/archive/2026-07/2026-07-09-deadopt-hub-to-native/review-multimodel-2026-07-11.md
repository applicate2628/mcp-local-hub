# De-adopt design — multi-model review synthesis (2026-07-11)

LEAD arbitration of 5 independent adversarial design reviews (codex gpt-5.6-sol xhigh
+ 4 fable-5 angles) run against `design.md` rev 2026-07-10 (`a1c2bcab`) before any
implementation. All findings verified against merged master. This memo is the
authoritative input to the design REWORK; it supersedes the ad-hoc read of the
individual reports.

## Reviews aggregated
- **codex** (independent deep-review): REVISE — 4 P0 + 5 P1.
- **fable / correctness+crash+resume** (a84030ee): [pending at write time — see note].
- **fable / security+authenticity+tamper** (a66fe68a): REVISE — 2 P2 + P3s; REFUTED codex P0-1.
- **fable / contract+regression+shared-owner** (ade0ff26): REVISE — 3 HIGH + 2 moderate + 1 cross-domain.
- **fable / completeness+simplification+arbiter** (abd80ff6): REVISE — arbiter scope: all-clients-only v1.

## Cross-verification (findings that changed under diversity)
- **codex P0-1 (row+snapshot swap → command injection) — REFUTED.** fable-security traced the
  SHIPPED read pipeline: `ReadAdoptProvenance → readStateFileInodeAnchored` refuses a wrong-owner
  file UNCONDITIONALLY in every mode (`hub_mcp_state_read_inode_windows.go:194-196` /
  posix `:135-137`, owner allowlist `hub_mcp_state_dacl_windows.go:181-199`) and refuses a
  write-broadened DACL. A co-resident who deletes+recreates `adopted-entries.json` owns it with
  the ATTACKER's SID → fail-closed at `ErrWrongOwner` before any field is trusted. swap-BOTH does
  NOT pass; the sha256 gate extends owner-anchored trust to the snapshot. **The owner anchor IS the
  authenticity root; the design under-credits it. Do NOT build a new authenticity mechanism — CREDIT
  the anchor + correct the threat-model text.** Residual = allowlisted-owner attacker (account
  compromise / SYSTEM / BA), outside the co-resident model, bounded+acceptable. `MCPHUB_REQUIRE_SINGLE_USER_HOME`
  does not change the anchor (it buys confidentiality/namespace strictness, not authenticity).
- **codex P0-3 (manifest gate not atomic) — DOWNGRADED to P3.** fable-security P3-D: the in-call
  read→compare→RemoveAll window is the SAME narrow window the accepted edit-path already ships, far
  narrower than the plan-time-check alternative. Fix = wording/residual or a flock across check+delete;
  not a blocker on its own.

## LEAD DECISION — v1 = ALL-CLIENTS-ONLY ATOMIC de-adopt (CUT subset)
All 5 reviews converge (codex must-fix "resumable journal OR all-clients-only"; fable F2/G5/O1/N2).
Subset de-adopt drags in: an UNDECLARED 4th mutator + `de_adopting→adopted` reverse transition
(design.md:430-431), `UpdateAdoptExpectedManifestHash` + the `ManifestEditInWithHash` lane, a
per-client snapshot-prune gap the whole-dir `removeAdoptSnapshots` + rowful-dir GC cannot reap, an
unjournaled resume target set (retry widens/narrows scope), and the subset `/g/` branch. Cutting it
collapses the manifest mutation to the single hash-gated DELETE, makes the resume scope unambiguous
(targets ≡ `AdoptClients`, the 3 declared mutators suffice), and removes 2 internal contradictions.
Per-client detach remains available via the shipped Servers-matrix uncheck → `/api/demigrate` lane
(weaker prunable-backup guarantee). **Subset de-adopt → deferred to its own follow-up item** (needs a
journaled target set + per-client snapshot pruning + the declared 4th mutator).
Also CUT from v1: **`--reconstruct-legacy`** (O2 — separate risk profile; fail-closed no-provenance is
the complete v1 answer) and **byte-exact P2-b** (O3/F3 — brittle across binary upgrade; use owner-rendered
field-equality instead, see below).

## BLOCKING must-fix (fold into the rework, all design-level)
1. **[G1 / fable-sec P2-A / codex P1-3] Close = DELETE the row, snapshots-FIRST.** Keeping a `closed`
   tombstone permanently wedges re-adopt (capture refuses any non-`adopting` prior row,
   `adopted_entries.go:529-531`; adopt v1 pins manifest==entry name so no rename dodge) AND a crash
   between `closed`-flip and snapshot-remove strands secret-bearing snapshots no GC reaps (Phase-2
   reaps only `adopting`; Phase-3 backstop only rowless dirs). Fix: `CloseAdoptProvenance` deletes the
   row snapshots-first, mirroring shipped `abortAdoptProvenance`/`reapAdoptProvenanceRow`
   (`adopted_entries.go:815-882`). P0's `closed` branch collapses to `found=false`. Keeps the
   at-most-one-row-per-manifest invariant true.
2. **[G2 / F1 / codex P1-1] Gate-ON per-client state.** `BuildHubReconcilePlan` gate-ON REMOVES every
   per-(server,client) entry (`install_hub_reconcile.go:233-263`), so on a reconciled gate-ON host
   `GetEntry(SourceEntryName)==nil` for every client and P2f/P2-b false-refuse EVERYTHING. Fix: either
   (a) branch P2f/P2-b on gate mode — gate-ON expected per-client state = "no per-server entry +
   `mcphub-hub` aggregate present + manifest binding live" (restore = write native snapshot entry;
   absent/merged-lower = no-op) + binding removal + reconcile prune; OR (b) explicitly REFUSE gate-ON
   de-adopt in v1 with a "gate OFF first" message. Silence is not acceptable. Pick + state which.
   NOTE [F6 cross-domain]: the same gate-ON entry-removal blinds the SHIPPED `classifyDeadAdoptingRow`
   (a committed-but-unflipped `adopting` row on a gate-ON host classifies CRASH_REAP → adopt GC
   destroys the snapshots de-adopt needs) — file as an adopt-side bug; the gate-ON-aware recognizer
   should feed both.
3. **[F3 / O3] Single-owner hub-entry rendering; field-equality not byte-exact.** P2-b's exact-byte
   recompute is a SECOND shape-derivation owner (contradicts claim 6) and false-refuses after any
   binary move/upgrade (RelayExePath absolute + version-dependent URL spelling; the recognizer
   deliberately tolerates 3 loopback spellings, `managed_entries.go:367-377`). Fix: extract install's
   per-client entry construction (`install.go:2679-2687`) into ONE named owner that both install and
   de-adopt call; define equality on the install-owned FIELD SET (recognizer-tolerant), not raw bytes;
   document the upgrade-drift residual. Collapse P2f shape-match + P2-b onto the one recognizer + the
   one byte-gate (no piled gate stack).
4. **[fable-sec P2-B] Snapshot read via the anchored reader.** A plain `os.ReadFile` on the snapshot is
   an OOM lever (attacker plants a multi-GB file at the namespace-writable path; slurped before the
   hash gate) and discards the owner signal. Fix: read the snapshot through `ReadStateFileInodeAnchored`
   (`state_read_inode_anchor.go:22`) — size cap + reparse/symlink refusal + wrong-owner refusal BEFORE
   hashing — and recompute/contain the snapshot path from `(ManifestName, Client)` rather than trusting
   `SnapshotRef` as a raw path. [P3-A: also add `adopt-provenance/` `.snapshot` to
   `isSecretBearingStateFilePath` so a read-broadened snapshot hard-fails like the vault files.]
5. **[F5] bytes-restore variant on a CAPABILITY interface, not `Client`.** Mirror the shipped
   `EntryBytesChecker` (`internal/clients/entry_bytes.go:24`) pattern: a new capability interface
   implemented by exactly the adopt-reachable adapters + fail-closed at the de-adopt restore site — NOT
   a `Client`-interface method (which compile-forces never-adoptable + non-EntryBytesChecker adapters,
   e.g. antigravity, to implement a restore they can never run). Declare it as an additive shared-owner
   change.
6. **[F4] BuildHubReconcilePlan extension — correct the mechanism + declare it.** The prune is a gate-ON
   supported-client SWEEP mirroring the gate-OFF `:164-180` (not the near-dead `:181-185` continue);
   the sole caller `cli/install.go:539` changes output (declare behavioral, note the
   `manifestHasInstallSignal`-filter edge); de-adopt as the SECOND caller must apply ONLY its target
   clients' ops (not sweep other adopted servers' entries). One zero-binding truth-source shared by
   both callers.
7. **[codex P0-4 / fable-correctness P1-5] Client-config mutations need a MUTATION-POINT CAS gate.** The
   F1 mutation-point-atomic principle was applied to the MANIFEST (`ManifestDeleteInWithHash`) but NOT
   to the destructive CLIENT-CONFIG writes. P2f is a PLAN-time gate; the restore/remove executes later
   in a SEPARATELY-locked section (`config_lock.go:32-51` wraps each adapter method individually), so an
   operator hand-edit (or demigrate) between plan and execute → E3 restores the stale snapshot OVER the
   operator's fresh edit = silent data loss (same for the `absent`/`present-merged-lower` remove).
   Fix: the new bytes-restore adapter method (and a guarded-remove sibling) must be COMPARE-AND-SWAP —
   inside ONE `withConfigLock` section: re-read the live entry, require it STILL matches the expected
   hub entry (per the item-3 field-equality), THEN write the verified snapshot bytes / remove; refuse
   otherwise. This is the client-config analogue of the manifest hash-gate and also closes the
   demigrate-interleave (P3-2). BLOCKING — it is the destructive write's only atomicity guarantee.

Item-3 sharpening [fable-correctness P2-3/P2-4]: the field-equality comparison must be PER-SHAPE and is
NOT byte-reconstructible for relay clients — `RelayExePath` (the mcphub binary's absolute install-time
path) is NOT stored in the record, and antigravity is adopt-supported. Compare at the PARSED-entry
level: HTTP → exact URL match; relay → `RelayServer/RelayDaemon` + `IsMcphubBinary(RelayExePath)` (the
shipped recognizer's own discipline, `managed_entries.go:381-406`). Also correct the design's URL
formula: `/clients/<client>/mcp` is the gate-ON AGGREGATE path on the hub port, NOT the per-server
entry adopt writes (`HubLoopbackURL(rec.Port, "/mcp")`). And P1r's `present` done-test must compare at
the PARSED-shape level (via the adapter's own reader over the snapshot bytes, the `EntryBytesChecker`
direction) — a byte-level compare wedges resume immediately after a successful restore because
adapters re-serialize (byte-equivalence is UNVERIFIED per `adopted_entries.go:120-127`).

Note P1-4 [fable-correctness]: the shared-key SKIP (P3-a) contradicts the close predicate ("done when
NONE of RoutedSecretKeys present") — a skipped shared key is never absent → row wedged `de_adopting`
forever. Fix the predicate: cleanup DONE = every key deleted OR deliberately skipped-as-shared (skip
recorded in the event). Fold into the fold-in list.

## FOLD-IN (P3, same pass)
- [P3-B] empty-hash fail-closed polarity — MOOT if the `ManifestEditInWithHash` subset lane is cut
  (only the DELETE lane remains, already covered by the decision). Confirm no remaining edit-path.
- [G3] define the GUI eligibility read-surface (a provenance-manifest-names field on the scan
  response, or `GET /api/deadopt/eligible`) — the frontend must NOT invent a shape heuristic.
- [G4] per-client partial-failure response contract + CLI exit semantics (precedent
  `DemigrateReport{Restored,Failed}`, `demigrate.go:31-34`).
- [G6] widen `MarkAdoptProvenanceDeAdopting` declared transition to
  `{adopted, committed-adopting} → de_adopting` so the P0 committed-`adopting` fresh admission works.
- [G7/G8] complete the residual statements: abandoned `de_adopting` wedges re-adopt + retains secret
  snapshots (defer `de_adopting`-GC / `mcphub de-adopt --recover` to a follow-up); a concurrent plain
  `install <server>` doesn't take the adopt lease (one-sentence residual).
- [P3-E] correct the threat-model text: co-resident CANNOT flip present→absent on the shipped reader
  (owner anchor blocks both modes); real residual = allowlisted-owner attacker; strict mode buys
  namespace/confidentiality strictness, not authenticity.
- [P3-D] manifest-delete atomicity: flock across check+delete OR soften the decision wording + record
  the residual.

## PASS (verified sound — keep)
Roll-forward resume model (reject the atomic/full-rollback alternative — rollback re-writes hub
entries over restored native = worse); `ManifestDeleteInWithHash` additive-safe + fail-closed-on-empty
+ retained path guard (decision Option A holds); lock/lease/redaction contracts; routed-secret
namespacing (no over-delete); the demigrate-NOT-reused choice (its prunable-backup + double-read
restore is exactly what the pinned sha256 snapshot escapes); composing the shipped owners
(`liveEntryMatchesManifestBinding`, the restore tail, `deleteAdoptRoutedSecrets`, the uninstall
intent-cleanup core, `BuildHubReconcilePlan`).

## Next
$architect reworks `design.md` to the all-clients-only v1 scope + folds every BLOCKING + FOLD-IN item;
records a decision `deadopt-v1-all-clients-only-scope` (accepted, cites this synthesis) + a follow-up
item for subset de-adopt; corrects the Change-Surface Contract + blast radius (the capability
interface + the reconcile sweep are additive shared-owner changes). Then re-verify the reworked
security/resume surface, then plan.
