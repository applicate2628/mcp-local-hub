# Design round-5 — whole-file-gone recovery ahead of the entry-scoped skip

Architect gate PASS 2026-07-13. Closes Sol round-4 P1 (path-#1 whole-file removal loses siblings + false-skip) + Terra P2 (barrier test). Additive to round-4 skip; no interface/lock/contract change.

## Verified facts
- 6 adopt-reachable rollback bodies (codex_cli:193, json_mcp:220 [cursor/gemini/qwen/antigravity], claude_code:195, vscode:200, opencode:311, mimocode:4041) each already read `backupData` (raw bytes = verbatim copyFile of pre-adopt file incl all siblings) + hold a single write-target path field + run compare+write under ONE withConfigLock hold.
- Live reads fold ENOENT→empty (readTOML/readRawConfig) ⇒ can't distinguish removed from empty ⇒ `os.Stat` is the only file-gone signal.
- `.bak` SURVIVES abort: abortAdoptProvenance removes only snapshot dir + row (never `.bak`); prune fires only on install SUCCESS path (install.go:2798), abort never reaches it. `-original` sentinel never pruned. adopt.go:343 already names `.bak-*` as recovery source.

## FIX — single-owner helper (clients.go, peer of entryRestoreIsNoop)
```
// rollback-only whole-file recovery for SecureWrite path #1 (post-rename verify-fail REMOVES the file, taking E + siblings S).
func wholeFileRestoreIfWriteTargetGone(path string, backupData []byte, writer WriteConfigFileFunc) (handled bool, err error)
//   len(backupData)==0                        -> (false, nil)   // nothing to recover
//   os.Stat(path) ok OR non-ENOENT err        -> (false, nil)   // present -> normal surgical/skip path
//   os.Stat(path)==ENOENT                      -> (true, writeConfigFileWith(writer, path, backupData))
```
**Insertion** (FIRST statement inside the existing `if allowHubEntry {`, BEFORE `entryRestoreIsNoop`):
```
if handled, werr := wholeFileRestoreIfWriteTargetGone(<path>, backupData, writer); handled { return werr }
```
`<path>` = c.path (codex/claude), j.path (json), v.path (vscode), o.path (opencode/mimo TOP layer).
- ALL 6 use whole-restore. Preserve-fallback (Sol option b) is EMERGENT: persistent path-#1 DACL condition ⇒ whole-restore write fails ⇒ err propagates ⇒ install closure appends clientName ⇒ InstallClientRollbackIncompleteError ⇒ adopt PRESERVES. No per-adapter divergence.
- mimo writes top layer o.path (backup is the o.path snapshot; merge re-forms pre-adopt).
- Ordering: whole-file check BEFORE isNoop — pre-empts the target-absent false-skip (both-absent → isNoop true).
- Verbatim bytes reproduce pre-adopt file exactly (JSONC comments incl); identical to what Restore() does.

## Both-variants-closed
| Variant | Backup | Live (file gone) | Round-4 | Round-5 |
|---|---|---|---|---|
| target-present (E+S) | E,S present | E absent ENOENT | surgical recreates only E → S lost → nil → abort → snapshot deleted | whole-restore E+S → nil (recovered) OR write-fail → preserve |
| target-absent (S only) | E absent, S present | E absent ENOENT | isNoop both-absent → SKIP → S lost silently | whole-restore fires FIRST → S recovered OR preserve |
No false positive: fires only on os.Stat==ENOENT. Path #2 (mutated, file present) → stat ok → handled=false → surgical revert unchanged. serena-migrate/LSP-router rollback callers: target present → inert.

## Tests (adopt_abort_preserve_provenance_test.go)
- NEW seam outcome `addEntryFailRemoved` (n==1): realWrite(hubBytes) THEN os.Remove(path) THEN return error (models path #1 rename-replace-then-remove → file absent). Subsequent whole-restore = n==2 write scripted by spec.restore.
- `TestExecuteAdopt_Path1WholeFileGone_TargetPresentSibling` (codex): E+S, addEntryFailRemoved+restoreSucceed ⇒ live parses + has BOTH E (stdio) and S; writeCount(codex)==2; abort with snapshot deleted (no false preserve). Fails pre-fix.
- `TestExecuteAdopt_Path1WholeFileGone_TargetAbsentSibling` (cursor/JSON entryless): only S; addEntryFailRemoved+restoreSucceed ⇒ live has S; writeCount==2 (no false-skip). Fails pre-fix.
- `TestExecuteAdopt_Path1WholeFileGone_MimoTopLayerSibling` (mimo): asserts whole-restore writes o.path top layer, S survives.
- Preserve variant: restore=restoreFailRemoved (whole-restore write fails) ⇒ adopt PRESERVES (snapshot present, sentinel, client named).

## Barrier hardening (Terra P2)
- NEW `TestRollbackRestore_BarrierBlocksSkipCompareUntilLockReleased`: live==backup; A holds withConfigLock; B calls restore ⇒ B does NOT complete within 300ms, returns nil after release. Load-bearing: unlocked pre-check would isNoop=true → skip-return immediately → trip "must NOT complete". Detects reintroduced unlocked pre-check the live≠backup test can't.
- KEEP existing barrier (live≠backup) as write-exclusion test.
- SHOULD (defense-in-depth, non-blocking): install-closure-level barrier (hold lock while driving install rollback via seam) — guards against reintroduction of the round-4-deleted install pre-check.

## Round-4 non-regression
File-present-unmutated still skips zero-writes (os.Stat ok → handled=false → entryRestoreIsNoop skip). MatchGuardSkips test (addEntryFailUnmutated → file present) → whole check inert → skip → writeCount==1.

## Protected / must-not-touch
abortAdoptProvenance (snapshot/row only — MUST NOT prune .bak); failAfterRollback+sentinel+append; FIX-1 register-before; entryRestoreIsNoop; withConfigLock (no new lock); Client interface (no new method); SecureWrite/WriteConfigFileFunc/writeBackup contracts; round-4 skip file-present path; reverted FIX-2; demigrate + non-adopt bodies.

Gate: `go build ./... && go vet ./... && go test -count=1 -timeout 6m ./internal/api/... ./internal/clients/...` + tagged `./internal/api/ ./internal/cli/` + `-race -count=10` barrier/contention; sweep mcphub.
