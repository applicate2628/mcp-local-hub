# Design round-3 — supersedes design-round2 §FIX-2 + §Tests. Closes Sol P1 + Sol P2 + Sol P3 + Terra P2.

Architect gate PASS 2026-07-12. FIX-1 (register-before-mutation) + FIX-3 (operator error wording) UNTOUCHED.

## Verified facts
- `writeBackup` = `copyFile(livePath, bakPath, 0600)` (byte-verbatim) ⇒ unmutated live config `bytes.Equal` backup.
- `withConfigLock` NON-reentrant per-path mutex; `RestoreEntryFromBackupForRollbackWithConfigWriter` takes it ⇒ guard read MUST be UNLOCKED (else self-deadlock).
- `Client.ConfigPath()` forwarded via lockingClient ⇒ `clientRef.ConfigPath()` valid.
- `emitAdoptProvenanceRoutedKeysOrphaned` = zero other callers. `deleteAdoptRoutedSecrets` = abort caller keeps it alive.
- Triple-orphan bug `2026-07-12-adopt-preinstall-crash-orphan-triple.md` (open, route-to de-adopt) already owns "GC reaps row+snapshot only; routed keys linger; de-adopt cleans"; de-adopt review already has idempotent `deleteAdoptRoutedSecrets` before `CloseAdoptProvenance`.

## CHANGE 1 — REVERT FIX-2 (Sol P1 cross-adopt key deletion + Terra P2 crash-safety)
- `internal/api/adopted_entries.go`: DELETE the post-reap routed-key block (~lines 1167-1182); reap returns to round-1 form (reapAdoptProvenanceRow + emitAdoptProvenanceOrphanReaped + reaped++, NO vault delete).
- `internal/api/adopt_provenance_events.go`: DELETE `emitAdoptProvenanceRoutedKeysOrphaned` (~156-169). Confirmed zero other callers.
- `deleteAdoptRoutedSecrets` retained (abort caller adopt.go:355).
- Follow-up (NO new bug): append preserve→reversal→GC-reap scenario to the EXISTING open bug `2026-07-12-adopt-preinstall-crash-orphan-triple.md` (second route to same routed-key residual; owner = de-adopt hash-gated --reclaim-crashed). Keep Status: open.
- Accepted bounded residual: reversed-preserve GC reap leaves routed keys (owner-only vault; operator/de-adopt removable; same class as snapshot-orphan residual). Strictly safer than FIX-2's cross-adopt deletion.

## CHANGE 2 — FIX-1 MATCH-GUARD (Sol P2)
New private helper `internal/api/install.go`:
```
liveConfigMatchesBackup(livePath, backupPath string) (matched bool, err error)
  // os.ReadFile both; return (true,nil) IFF both read OK AND bytes.Equal; any read err ⇒ (false,err). No parse.
```
Guard = FIRST statement inside the EXISTING FIX-1 restore closure (uses captured clientRef/backupPath/clientName/entryName):
```
matched, mErr := liveConfigMatchesBackup(clientRef.ConfigPath(), backupPath)
if mErr == nil && matched {
    // byte-identical to pre-adopt backup ⇒ AddEntry did NOT mutate (pre-rename fail). SKIP restore:
    // running SecureWriteClientConfig on an untouched config risks its OWN post-rename verify-failure
    // removing a clean file (Sol P2). Skipped ⇒ provably unmutated ⇒ NEVER appends to failures (no sentinel).
    fmt.Fprintf(w, "  rollback: %s entry in %s unchanged since backup — restore skipped\n", entryName, clientName)
    return
}
if mErr != nil {
    fmt.Fprintf(w, "  rollback: %s in %s: could not compare live vs backup (%v) — restoring to be safe\n", entryName, clientName, mErr)
}
// ... EXISTING restore-then-append-on-failure body UNCHANGED ...
```
- Skip ONLY on exact whole-file byte-equality. Fail-safe: read-err or differ ⇒ restore. Under-skips, never over-skips ⇒ can't skip a mutated client.
- Helper = private api (format-agnostic whole-file bytes; no new Client/adapter method — rejected entry-scoped value compare [needs new adapter method] + clients-pkg method [one-caller indirection]).
- UNLOCKED read (mandatory: withConfigLock non-reentrant). No concurrent internal writer in rollback window; external editor is pre-existing non-owned race equally affecting restore.
- **P1-non-regression:** #2 hub-relay: live≠backup ⇒ restore RUNS (fail⇒sentinel⇒preserve). #1 removed: read ENOENT ⇒ restore RUNS. Unmutated: live==backup ⇒ SKIP (no damage, no sentinel, correct abort). Guard skips only on exact pre-adopt byte-match ⇒ P1 impossible.
- Update FIX-1 closure lead comment: 3 outcomes (skip-unmutated / restore-success / restore-fail→sentinel); skipped client never enters sentinel.

## CHANGE 3 — Tests
- Seam: add `restoreFailMutated` (realWrite restore bytes THEN error) + `restoreFailRemoved` (remove/truncate live THEN error) to installWriteSeam; keep restoreSucceed/restoreFail. Additive.
- **NEW load-bearing (fails pre-guard):** `TestExecuteAdopt_MatchGuardSkipsRestoreOnUnmutatedClient` — 1 client, addEntry FAIL_UNMUTATED + restore restoreFailRemoved ⇒ assert (1) NO sentinel; (2) live config byte-identical to seeded pre-adopt (damaging restore never ran); (3) adopt takes ABORT branch (row+snapshot+manifest+keys removed). Pre-guard: restore runs, removes file (damage), appends → wrongful preserve → FAIL.
- Snapshot BYTE assertions: in assertPreservedProvenance + Test-8, read each SnapshotRef + `bytes.Equal` vs known seeded pre-adopt config (not just os.Stat). Test-8 reversal: assert live now bytes.Equal snapshot.
- Flip Test-8 routed-key: header→"routed keys deferred to de-adopt"; reap stays ==1; replace "routed key survived (FIX-2 must delete)" with assert ALL plan.SecretRoutedKeys REMAIN after reclaim GC; keep unrelated-key-survives; precondition→"prove routed keys NOT autonomously deleted".
- Existing 1/2/5/6/Union unaffected (FAIL_UNMUTATED restore now skipped = effect-noop; mutated outcomes unchanged; P1-repro passes: mutated⇒guard runs restore⇒fail⇒sentinel).

## CHANGE 4 — Doc/telemetry (Sol P3)
Concept everywhere: "restoration could not be confirmed; the client may have been mutated (rewritten to hub relay) OR a restore write itself failed on an otherwise-untouched config."
- install.go:2483 (sentinel doc), install.go:2562-2566 (var comment, +skipped-client-never-appends), adopt.go:325-327 (preserve comment). Operator error string :337-343 already correct — leave.
- CLAUDE.md:1393-1395 "Eight events…-routed-keys-orphaned" → "Seven events…-preserved" (drop orphaned); 1398-1404 soften -preserved + note GC leaves routed keys (de-adopt); 1405-1408 DELETE -routed-keys-orphaned para; §Residual ~1424-1431 add reversed-preserve-leaves-keys sentence.
- Consistency grep after: `grep -n "routed-keys-orphaned\|Eight events\|already rewritten to the hub relay" CLAUDE.md internal/api/*.go` = zero live hits.

## Protected / must-not-touch
FIX-1 register-before position; sentinel shape + failAfterRollback + failures-accumulation; reapAdoptProvenanceRow/classifyDeadAdoptingRow/adoptRowProvablyUnmutated DECISION logic; deleteAdoptRoutedSecrets + abort caller; Restore/AddEntry/SecureWrite/WriteConfigFileFunc contracts (no new interface); abort-branch ordering + operator error text; withConfigLock (guard adds NO lock).

Gate: `go build ./... && go vet ./... && go test -count=1 -timeout 5m ./internal/api/...` + `go test -tags=test_state_path_env ... ./internal/api/ ./internal/cli/`; sweep mcphub.
