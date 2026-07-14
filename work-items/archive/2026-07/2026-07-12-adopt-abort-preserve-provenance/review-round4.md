# Commission round 4 — verdict: REVISE (Sol NEW P1 path-#1 sibling-loss; Terra barrier-test)

Date 2026-07-13. Round-4 = fold entry-scoped skip into restore bodies + drop the guard.

## CLOSED (Sol + Terra both confirm the CODE)
- Round-3 P2 TOCTOU: **CLOSED** — no unlocked pre-check; compare+write inside the single `withConfigLock` hold (codex: one readTOML; JSON/mimo: two reads, same lock hold, no unlock/relock).
- Round-3 P2 sibling-edit: **CLOSED** — `entryRestoreIsNoop` keys on `servers[name]` only.
- Round-3 P3 overstatement: **CLOSED** — all 3 sites softened.
- Skip correctness: PASS (deep-equal can't skip a mutated entry; #2 hub-relay ≠ stdio; mimo compares top layer readRawConfig; demigrate allowHubEntry=false untouched + ErrBackupEntryAlreadyMigrated active; read-error fall-through fail-safe).

## OPEN
### Sol NEW P1 — path #1 (whole-file removal) loses siblings, rollback reports success
SecureWrite post-rename path #1 (definitive owner/mode/DACL verify fail) REMOVES the whole published
file (target E + sibling S). Then:
- (target-present backup) live E absent, backup E present → not-noop → surgical restore recreates ONLY
  E (base-reads the now-missing live → S unrecoverable) → returns nil → no sentinel → ABORT → snapshot
  deleted. Live left missing S.
- (target-ABSENT backup, S present) path #1 removes file → live E absent == backup E absent →
  `entryRestoreIsNoop` returns TRUE → SKIP → removed config treated as reconciled → S lost silently.
Severity: P1 data-loss of siblings in the LIVE config. (Note: the `.bak-mcp-local-hub-*` full backup
likely survives abort — S may be operator-recoverable from it — VERIFY; but the fix reports FALSE success,
masking a degraded live state, and the snapshot is deleted.)
**Sol's required fix:** track write-target FILE/LAYER presence separately from entry presence. If the
backup layer existed but the live layer is GONE, restore the ENTIRE backup atomically through the writer
(recovers E+S — safe, no siblings to clobber when the file is gone) OR return an error so the sentinel
preserves provenance. Add path-#1 tests (pre-existing sibling; target-present + target-absent backups).
Missing seam: `addEntryFailRemoved` (path #1 during AddEntry) — current seam has only `restoreFailRemoved`
(damage during restore) + `addEntryFailMutated` (path #2).

### Terra P2 (test-only; CODE is PASS) — barrier test doesn't prove compare-under-lock
`rollback_restore_skip_test.go:159` starts live≠backup, so B always reaches the write and blocks on the
write lock — proves write-exclusion, not that the SKIP-compare is under the lock. **Fix:** init live==backup
while A holds the lock; an unlocked pre-check would return immediately (skip) and fail the block assertion.
Keep a separate live≠backup test for write-exclusion. Also: the barrier test invokes the clients-level
restore, not the install closure, so it can't detect reintroduction of the former install pre-check.

## DISPOSITION — round-5
1. Whole-file-gone recovery (Sol P1): in the restore body, when `allowHubEntry` AND backup has content AND
   the live write-target FILE/LAYER is absent (ENOENT) → restore the WHOLE backup file atomically via the
   writer (fully recovers E+S) [preferred, Sol option a], else return error → sentinel → preserve. Fixes
   both the surgical-loses-siblings and the false-skip variant. Architect picks whole-restore vs preserve
   per adapter (whole-restore is strictly better where feasible — no concurrent sibling to clobber when file gone).
2. Add `addEntryFailRemoved` seam + path-#1 tests (sibling present; target-present + target-absent).
3. Barrier test: init live==backup (Terra).
Core P1 (register-before) + reverted FIX-2 + entry-scoped skip stay. Review is CONVERGING (round-4 closed
3 of round-3's findings; 1 new). Then re-run Sol+Terra.
