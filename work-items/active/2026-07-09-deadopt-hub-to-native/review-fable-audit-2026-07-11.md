# De-adopt design rework (f0798f9d) — 3-fable audit synthesis (2026-07-11, round 2)

LEAD arbitration of 3 independent fable-5 audits of the REWORKED `design.md` (commit
`f0798f9d`, all-clients-only v1), each verifying against merged master this session.
Lenses: (1) traceability+completeness+stale-text, (2) security+authenticity+resume,
(3) shared-owner+contract+simpler-alternative. This memo supersedes the individual
reports and is the authoritative input to the SECOND architect rework pass.

## Unanimous verdict: REVISE (all design-text-level; no architecture change)

The structural rework is SOUND — all 7 BLOCKING fixes from round 1 are present-and-correct
(2 via decision-recorded rescopes that are coherent). Fixes 3/5/6 CONFIRMED correct by
fable-3; the security core (CAS gate, anchored snapshot read, owner-anchor crediting,
gate-ON fail-closed, secret close-predicate) CONFIRMED by fable-2; the all-clients-only cut
CONFIRMED as the right minimal v1 (JUSTIFIED-DEPTH, not layering) by fable-3. But the design
under-specifies the restore primitive and the close/resume state machine at exactly their
hardest points, and each unspecified point resolves — depending on how the implementer reads
it — to silent data loss or a permanently-wedged `de_adopting` row. After these fixes a
DELTA-CHECK of the amended sections suffices (no full re-audit round) — all 3 fables agree.

## BLOCKING must-fix (fold into the second rework)

### B1 — Restore primitive: compose the SHIPPED restore core, NOT lossy GetEntry [fable-3 P1-1; converges fable-1 P2-1, fable-2 P2-E]
STANDOUT correctness defect. The design's `CASRestoreEntryFromBytes` sketch ("extract entryName
from snapshotBytes via the adapter's own reader and write it") reads most naturally as
`GetEntry`→`AddEntry`. That path is LOSSY for the canonical adopt source (direct-stdio
`command`/`args`/`env`): `clients.MCPEntry` (`clients.go:24-48`) has NO Command/Args fields, so
a GetEntry round-trip restores an empty-URL husk — destroying the exact "original secret-literal
spelling intact" guarantee the snapshot mechanism exists to deliver. The repo already documents
this trap: install rollback restores from the backup FILE, not GetEntry→AddEntry, "because
GetEntry intentionally projects many client schemas into a small MCPEntry shape and can be lossy
for direct stdio entries" (`install.go:2642-2647`).
- FIX: pin `CASRestoreEntryFromBytes` = the CAS `match` gate + a **bytes-parameterized refactor
  of the shipped per-adapter `restoreEntryFromBackup` core** (`claude_code.go:170-187`,
  `codex_cli.go:168-185`, `amazon_q.go`, `aider.go`, `continue.go`, … — each has stdio-verbatim
  restore + remove-on-absent tests). Refactor each adapter's `restoreEntryFromBackup` body into a
  `restoreEntryFromBytes(configBytes, name, …)` core; the backup-path variant becomes a thin
  file-reading wrapper (existing `RestoreEntryFromBackup*` callers stay byte-unchanged).
- Explicitly BAN the `GetEntry`→`AddEntry` round-trip for the restore payload AND ban a parallel
  per-adapter extraction re-implementation (second owner).
- Pin the RESUME done-test comparison to the SAME raw-subtree/bytes core, NOT lean-`MCPEntry`
  equality (two stdio husks both project to `URL:""` → a wrongly-restored entry would read
  RESTORE-DONE). [fable-1 P2-1, fable-2 P2-E, fable-3 P1-1 all hit this from different angles.]
- Blast-radius consequence: upgrade shared-owner change (2)'s declaration from "additive
  capability interface" to **"additive interface + behavior-preserving extraction refactor of the
  per-adapter restore bodies."**
- CAS **nil-live contract** [fable-2 P2-E]: the re-read `live` can be nil (entry vanished between
  plan and execute); the match closure passes it straight into `liveEntryMatchesManifestBinding`
  which derefs `live.URL` (`managed_entries.go:378`; shipped callers nil-guard at
  `adopted_entries.go:459-461`). Define per-branch: `CASGuardedRemoveEntry` nil-live = already-done
  idempotent success; `CASRestoreEntryFromBytes` nil-live = conflict-refuse (fail-closed —
  restoring into an operator-emptied slot resurrects against intent). Without this the first
  vanished-entry execute PANICS.

### B2 — Close/resume state machine: the two hardest points [fable-2 P1-A + P1-B]
The rework fixed the `closed`-tombstone wedge (G1) but left the two structurally-hardest windows
under-specified; each resolves to data loss or a permanent wedge.
- **B2a — per-client `Failed` close-gating [P1-A].** The E-list is sequential and never states
  whether E4/E5/E6 run when E3 left a client Failed. Ungated (E6 always deletes row+snapshots) →
  a Failed client's pre-adopt original is destroyed while its config still points at a
  deleted manifest → permanent data loss, retry impossible (`present` restore fail-closes on the
  missing snapshot forever). Gated (E6 requires all-restored) → a PERMANENT CAS conflict (operator
  legitimately edited between plan and execute — the case fix 7 exists for) makes the predicate
  unsatisfiable → row wedged `de_adopting` forever → re-adopt permanently blocked
  (`adopted_entries.go:529-531`) + secret snapshots never reaped (Phase-2 reaps only `adopting`).
  - FIX: state the gate — E4/E5 may proceed under Failed clients (they touch row+snapshots only,
    which E4/E5 don't); **E6 runs ONLY when every target client is RESTORE-DONE AND secrets are
    deleted-or-skipped**. Then resolve the permanent-conflict horn deliberately: EITHER an operator
    escape (`--accept-conflict <client>` → that client terminally-done-with-warning; defensible —
    a live entry that is no longer the hub entry means the operator already took the slot) OR an
    explicitly recorded accepted-wedge residual pointing at the G7 follow-up. Silence = the same
    wedge class P1-4 fixed for secrets, left open for clients.
- **B2b — crash INSIDE close [P1-B].** Snapshots-first ordering (correct for the secret-strand)
  opens a window: crash after `removeAdoptSnapshots`, before the row write → row=`de_adopting`,
  snapshot dir GONE, all clients already restored to native. On resume the `present` RESTORE-DONE
  test's second conjunct ("parsed-entry == snapshot's entry") is UNEVALUABLE (snapshot gone) →
  routed to "snapshot missing → Failed, fail-closed" → with the (necessary) gated close, the row
  NEVER closes → permanent wedge. Also reachable via manual snapshot-dir deletion.
  - FIX: add an explicit close-resume branch to the P3 derivation — row=`de_adopting` AND manifest
    absent AND snapshot dir absent AND live entry ≠ hub entry → classify RESTORE-DONE (close was
    mid-flight; finish the row delete). KEEP the fail-closed missing-snapshot rule for the
    pre-restore case (live entry STILL hub + snapshot missing → Failed). Add a crash-inside-close
    case to test 7.

### B3 — G7/G8 residuals: gate-decision claims them folded, body omits them [fable-1 P1-1 + fable-2 P2-C — CONVERGENT]
The gate decision (design.md ~:572) lists "G7/G8 residuals" as resolved; a body sweep finds
neither. Add both:
- **G7:** an abandoned `de_adopting` row (crash + operator never retries) wedges re-adopt +
  retains secret-bearing snapshots no GC reaps — with `de_adopting`-GC / `mcphub de-adopt
  --recover` explicitly DEFERRED to the follow-up. (Structurally identical to the `closed`-wedge
  the rework fixed; the failure table currently covers only the retried path.)
- **G8:** a concurrent plain `install <server>` / GUI Apply / hub reconcile takes only config
  locks, NOT the `<manifest>.lease` — so in the E3→E4 window it can rewrite hub entries over
  just-restored native entries (CAS does not protect against writers that come AFTER de-adopt's
  own write), and a gate-ON reconcile emits `ClientUpdateRemove{EntryName: server}`
  (`install_hub_reconcile.go:256-262`) against the just-restored entry. Operator-driven + bounded
  → one-sentence residual, but in the BODY, not just the checklist.

### B4 — E2 must re-verify committed-ness under the held lease [fable-2 P2-D]
Plan P1's committed-adopting admission is evaluated BEFORE E1's lease acquire → stale at E2. An
in-flight adopt can fail (its hub entries removed) with its `abortAdoptProvenance` write also
failing, freeing the lease; de-adopt E2 then flips a now-UNCOMMITTED `adopting` orphan to
`de_adopting`, converting a GC-reapable row into a GC-immune one feeding the B2a wedge.
- FIX: E2's `MarkAdoptProvenanceDeAdopting`, when the row is `adopting`, re-runs
  `classifyDeadAdoptingRow == COMMITTED_KEEP` under the held lease before flipping; refuse
  otherwise (adopt GC owns it). One sentence in the mutator contract. [Ties to G6's widened
  transition `{adopted, committed-adopting} → de_adopting`.]

### B5 — remove-on-absent inside restore → fail-closed, not destroy [fable-3 P2-1]
For the v1 `present` caller the entry is capture-GUARANTEED in the pinned bytes
(`EntryBytesChecker` verifies before pinning; sha256 pins those bytes). Entry-absent-in-verified-
snapshot is an impossible state signaling record/snapshot inconsistency; silently removing the
live entry destroys config on a corrupted premise AND duplicates `CASGuardedRemoveEntry`'s
ownership of "remove."
- FIX: run shipped `EntryPresentInBytes` on the verified bytes before the restore core; REFUSE
  fail-closed (client → Failed) when absent. Removal stays exclusively `CASGuardedRemoveEntry`'s.

### B6 — Snapshot size-cap symmetry [fable-2 P2-F]
Capture writes via `WriteStateFileBytesAtomic` with NO size limit; the design caps the restore
read at ~1 MiB (default) / ~16 MiB. A legitimately larger config (a grown `~/.claude.json`)
adopts fine today and is then PERMANENTLY unrestorable → feeds the B2a Failed-client horn. The
filed adjacent bug covers the too-SMALL direction only.
- FIX: state the symmetry invariant — enforce the same cap at adopt CAPTURE time (refuse while the
  operator can still abort with zero side effects), OR size the restore cap from the recorded
  snapshot size (trustworthy once the owner-anchored + sha-gated read passes).

## FOLD-IN (P3, same pass)
- **CAS lock ownership pinned** [fable-3 P3-1]: the lock lives in `lockingClient`'s forwarders
  (`config_lock.go:223-245` type-assert-inside-lock precedent); concrete CAS bodies are LOCK-FREE
  (the per-path mutex is non-reentrant `:24-30` → a concrete body calling `withConfigLock` self-
  deadlocks). One sentence.
- **Restore guard polarity** [fable-3 P3-2]: pin guarded-refuse (a hub-shaped snapshot entry means
  adopt absorbed an already-hub-managed entry → refuse + report Failed) as the default, over the
  `ForRollback` verbatim variant.
- **`stateFileReadCapBytes` = suffix/dir-segment clause**, not an exact-basename case
  (`state_read_caps.go:28-42`); `<client>.snapshot` has a variable basename [fable-3 P3-3,
  fable-1, fable-2 P3]. Same for `isSecretBearingStateFilePath` (full-path or `.snapshot`-suffix).
- **C6 landing residue** [fable-3 P3-4, fable-1 P3-3]: `adopted_entries.go:983-996` declares 3
  mutators "the de-adopt work-item authors them" (v1 authors 2) and `:154-156` describes the
  superseded subset contract. The IMPLEMENTATION change must repoint `UpdateAdoptExpectedManifestHash`
  at the subset follow-up + update the tombstone comment (C6 stale-relation hygiene). Add one design/
  plan line directing this.
- **Restore drops unknown per-entry fields** [fable-2 P3]: the parsed restore preserves env VALUES
  (secret literals) verbatim but drops fields the adapter struct doesn't model — one honesty
  sentence (consistent with the shipped `functional-equivalent` label `adopted_entries.go:120-127`).
- **Gate-ON F6 sentence** [fable-2 fix-5 note]: until the filed adopt-side bug is fixed, a
  committed-but-unflipped `adopting` row >24h old on a gate-ON host can lose its snapshots to a
  subsequent adopt's GC BEFORE the operator gates OFF to de-adopt — bounded (promote-flip crashes
  only; `adopted` rows GC-immune), outside de-adopt scope, but worth a sentence in the gate-ON §.
- **Doc cleanup** [fable-1 P3-1/P3-2]: stale anchor `adopt.go:537-551` → `:355` (key-NAMES print);
  drop the "(T6)" label residue on test 9.

## PASS (verified sound — keep, do not touch)
- All 7 round-1 BLOCKING fixes structurally present-and-correct (fable-1 per-item table).
- Fixes 3/5/6 correct (fable-3): capability interface (not `Client` method); reconcile-sweep
  resolved-by-descope with mechanism preserved in the follow-up stub; single-owner rendering via
  the simpler no-renderer alternative (v1 never constructs an entry).
- Security core (fable-2): CAS mutation gate is mutation-point-atomic; anchored snapshot read
  hardened end-to-end (reparse/symlink + size cap + wrong-owner BEFORE hash; path from
  (ManifestName, Client)); owner-anchor CREDITED not rebuilt (codex P0-1 stays refuted); gate-ON
  refusal fail-closed + F6 filed adopt-side; secret close-predicate cannot wedge.
- Stale-text hunt CLEAN (fable-1) except the P3-2 label; Change-Surface declares exactly 3 additive
  shared-owner changes (amend B1 to note the restore-body refactor); both decisions accepted in the
  registry; the 3 bugs + follow-up stub all exist on disk.
- All-clients-only, gate-OFF-only v1 = the right minimal cut (fable-3 over-engineering assessment):
  `targets ≡ AdoptClients` kills the journal; per-client detach keeps the demigrate workaround;
  gate-ON refusal is fail-safe; fail-closed no-provenance replaces `--reconstruct-legacy`. Plan-time
  checks + mutation-point CAS/hash gates are JUSTIFIED-DEPTH (real TOCTOU boundary), not layering.

## Next
`$architect` reworks `design.md` for B1-B6 + the FOLD-IN list (all design-text; the B1 restore-core
refactor + the B2 state-machine branches are the substance). Then a DELTA-CHECK of the amended
sections (restore primitive, close/resume machine, gate decision, change-surface) — NOT a full
re-audit — then `$planner`.
