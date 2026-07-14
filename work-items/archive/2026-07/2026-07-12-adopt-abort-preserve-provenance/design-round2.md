# Design round-2 (refined) — supersedes round-1 §Plumbing-item-2 + §Tests

Architect gate PASS 2026-07-12. Closes Sol round-1 P1 + P2 + P3. Everything else in `design.md` stands.

## Verified code facts
1. Restore closure appended only AFTER `AddEntryWithConfigWriter` success (install.go:2747) → mutation boundary uncovered.
2. AddEntry → `SecureWriteClientConfig` has TWO post-rename error paths returning error with config ALREADY mutated (secure_write_client_config.go:71-76 + tests `...VerifyFailureLeavesNoFile` [config DELETED] / `...TransientReopenFailureKeepsPublishedFile` [config = hub relay]).
3. AddEntry is the ONLY config-mutating op between BackupKeep(:2717) and configWriter(:2734) — rest is Fprintf/append/struct-build/map-lookup.
4. `RestoreEntryFromBackupForRollbackWithConfigWriter` routes through same SecureWriteClientConfig; **nil return ⇒ rename+verify passed ⇒ live config holds pre-adopt entryName ⇒ restore-nil ⇒ PROVABLY clean**.
5. Abort branch deletes routed keys (adopt.go:355); GC reap does NOT (reapAdoptProvenanceRow removes row+snapshots only) → preserve→reverse→reap orphans routed keys (Sol P2).
6. Adopt errors reach only cobra stderr (cli/adopt.go:48) — not persisted; event is names/counts only.

## FIX 1 — register restore closure BEFORE the mutation (install.go executeInstallToWithSymlinkConsents)

Relocate the capture block + `rollback = append(...)` closure (current ~:2742-2754) to sit BETWEEN
`configWriter := symlinkConsentWriters[u.Client]` (:2734) and `AddEntryWithConfigWriter` (:2735).
Order per iteration:
```
configWriter := symlinkConsentWriters[u.Client]        // :2734 unchanged
clientRef := client; entryName := m.Name; clientName := u.Client; backupPath := bak; rollbackWriter := configWriter   // MOVED UP
rollback = append(rollback, func() {                    // MOVED UP, body byte-identical
    if err := clients.RestoreEntryFromBackupForRollbackWithConfigWriter(clientRef, backupPath, entryName, rollbackWriter); err != nil {
        rollbackClientRestoreFailures = append(rollbackClientRestoreFailures, clientName)
        fmt.Fprintf(w, "  rollback: restore %s entry in %s from backup failed: %v\n", entryName, clientName, err)
        return
    }
    fmt.Fprintf(w, "  rollback: restored %s entry state in %s from backup\n", entryName, clientName)
})
if err := clients.AddEntryWithConfigWriter(client, entry, configWriter); err != nil {  // :2735 unchanged
    return failAfterRollback(fmt.Errorf("add entry to %s: %w", u.Client, err))
}
fmt.Fprintf(w, "✓ %s → %s\n", u.Client, displayURLOf(u))   // ✓ stays AFTER AddEntry success
```
Update the closure lead comment: restore now registered before AddEntry so an AddEntry error that
left config mutated (post-rename #1/#2) is still restored; a restore that can't prove pre-adopt
state feeds the sentinel. `failAfterRollback` / `rollbackClientRestoreFailures` / the sentinel type
/ all six call sites UNCHANGED.

## FIX 2 — GC reap cleans routed keys (adopted_entries.go gcOrphanedAdoptingProvenance Phase-2)

After `reapAdoptProvenanceRow(...)` returns nil (reaped++), lease `lk` still held, store lock
released: best-effort `deleteAdoptRoutedSecrets(live.RoutedSecretKeys)` if len>0; on delete err emit
a warn event (names/counts only). Reap-first/delete-second. Provably safe: reap fires only when
classify==crash_reap (manifest absent) AND adoptRowProvablyUnmutated==true → keys unreferenced.
Symmetric with abort branch. Lock-safe (only vault lock under held lease).

## FIX 3 — wording (adopt.go preserve-branch error)
- "clients left pointing at the hub relay: %s" → "clients whose pre-adopt restoration could not be confirmed: %s"
- "or reverse it with de-adopt" → "or reverse it with de-adopt once available"

## DECISION — redaction (Sol P2): ACCEPT AS-IS, no code change
Contract = "NAMES/COUNTS only — never secret VALUES or config contents". Cause chain carries at most
a filesystem PATH, reaches only operator's own cobra stderr (not persisted); event is names-only;
GUI already redacts. Rendered string already complies (paths out of scope). Dropping cause hurts
diagnosability. Keep Unwrap().

## Bug close (arch P3-2): flip bug file status→closed IN THE FIX COMMIT.

## Failure-mode table (new ordering, failing/rollback-triggering client)
| AddEntry | config | Restore | failures[] | Install err | Adopt | Safe |
|---|---|---|---|---|---|---|
| success; later fails | reverted | nil | — | plain | abort, delete snapshot | ✓ (=today) |
| success; later fails | — | error | +c | sentinel | preserve | ✓ (=today) |
| FAIL MUTATED (#2 hub relay) | reverted entry-scoped | nil | — | plain | abort, delete | ✓ NEW: proven clean |
| FAIL MUTATED (#2 transient on restore) | reverted, reopen fails | error | +c | sentinel | preserve | ✓ over-preserve |
| FAIL MUTATED (#1 whole-file removed) | removed; restore hits same cond | error | +c | sentinel | preserve | ✓ NEW: snapshot kept |
| FAIL UNMUTATED (pre-rename) | unchanged | nil (redundant) | — | plain | abort, delete | ✓ never mutated |
| FAIL UNMUTATED, persistent write fault | unchanged | error | +c | sentinel | preserve | ✓ ONLY new path: over-preserve, correct |

Rows 3 & 5 are the P1: pre-fix → abort → snapshot DELETED (data loss); post-fix → preserve.
Residual (accepted, pre-existing, not worsened): #1 whole-file-removed on a MULTI-entry client with
DACL fixed inside the ms rollback window → entry-scoped restore partial → sibling entries lost. Not
expanded to whole-file restore (would clobber concurrent unrelated edits).

## Tests — reshape + additions
**Seam reshape (required):** replace `seedFailSecondAddAndOptionallyRestore` with a per-path
call-ordinal seam: AddEntry-write = SUCCEED | FAIL_MUTATED (realWrite THEN error) | FAIL_UNMUTATED
(error, no write); Restore-write = SUCCEED | FAIL. Backups pass through.
- **NEW P1 repro (load-bearing, MUST fail pre-fix):** `TestExecuteAdopt_PreservesWhenFailingClientLeftMutatedAndUnrestorable` — 1 present client, AddEntry FAIL_MUTATED, restore FAIL ⇒ sentinel names client; PRESERVE row(adopting)+snapshot+manifest+keys; event emitted; live config still hub relay.
- **NEW no-over-preserve-when-restorable:** FAIL_MUTATED + restore SUCCEED ⇒ no sentinel, bare cause, config reverted, abort clean.
- **Reshape 1/4/5:** B=FAIL_UNMUTATED+restore SUCCEED, A committed+restore FAIL ⇒ sentinel Clients=[A] only.
- **Reshape 2:** A-restore SUCCEED, B FAIL_UNMUTATED+restore SUCCEED ⇒ no sentinel, byte-identical bare err.
- **Reshape 6 (abort):** single client FAIL_UNMUTATED+restore SUCCEED ⇒ no sentinel ⇒ abort (row+snapshot+manifest+keys removed).
- **Multi-client (arch P3-3):** 3 clients, ≥2 un-restored ⇒ sentinel Clients = sorted union; adopt error names each.
- **Sol P2 lifecycle:** assert every SnapshotRef file exists; exact `plan.SecretRoutedKeys` present; event clients/count exact; seed 1 UNRELATED vault key → after reclaiming GC routed keys DELETED + unrelated SURVIVES; GC state "manifest absent + client still mutated ⇒ KEEP (reap 0)"; GC state "manifest absent + clients restored ⇒ reap 1 + routed keys gone".
- Grep `adopt_classifier_committed_signal_test.go`, `adopt_provenance_r2_test.go` for "routed key remains after reap" assertions the new cleanup would flip.

Gate: `go build ./... && go vet ./... && go test -count=1 -timeout 5m ./internal/api/...`
+ `go test -tags=test_state_path_env -count=1 -timeout 5m ./internal/api/ ./internal/cli/`; sweep mcphub.

## Protected / must-not-touch
`Install` signature; runRollback ordering + stack type; sentinel shape (Clients names-only + Unwrap);
`WriteConfigFileFunc`/`SecureWriteClientConfig`/`AddEntry...`/`RestoreEntry...` contracts (rejected
alternative); classifyDeadAdoptingRow/adoptRowProvablyUnmutated/reapAdoptProvenanceRow DECISION logic
(only GC loop call-site gains additive post-reap cleanup); adopt secrets+manifest pre-Install
branches; .bak backstop; non-adopt Install callers' happy-failure error text.
