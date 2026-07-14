# Design round-4 — atomic entry-scoped skip-if-unchanged folded into rollback restore

Architect gate PASS 2026-07-13. Closes round-3 TOCTOU (Sol+Terra) + round-2 sibling-edit STILL-OPEN (Sol) + P3 incomplete. FIX-1 (register-before) + reverted FIX-2 stay.

## Verified facts
1. Every adopt client's writer + nil-writer restore converge on ONE per-adapter body `restoreEntryFromBackupWithWriter(backupPath, name, allowHubEntry, writer)`.
2. `allowHubEntry=true` ⟺ rollback (install install.go:2810, serena-migrate serena_client_reconcile.go:480, LSP-router lsp_client_router.go:976). `allowHubEntry=false` ⟺ demigrate (has the `ErrBackupEntryAlreadyMigrated` guard).
3. Body runs entirely under `lockingClient`'s single `withConfigLock` hold (config_lock.go:253/259).
4. `writeBackup` = verbatim copyFile ⇒ unmutated write-target byte-identical to backup ⇒ identical parse.
5. Adopt targets = 6 distinct restore bodies: codex (reads live via readTOML), jsonMCPClient (cursor/gemini/qwen/antigravity), claude, vscode, opencode, mimo. JSON-family + mimo bodies don't read live today. mimo `readJSON()` returns MERGED view — compare MUST use TOP layer `o.path`.

## DECISION 1 — shared body gated on `allowHubEntry` (NOT a new variant)
Fold skip into the 6 bodies under `if allowHubEntry`. Caller-safety (all allowHubEntry=true):
- install rollback = the fix (P1-safe below).
- serena-migrate rollback: restores mutated→legacy (live≠backup) ⇒ skip never fires; degenerate live==backup ⇒ safe no-op.
- LSP-router rollback: post-restore GetEntry sees live==backup on skip = identical to a real restore's output.
- demigrate (allowHubEntry=false): UNTOUCHED — gate excludes it; ErrBackupEntryAlreadyMigrated preserved.
Rejected new `…IfDiffers` variant: new interface method across ~30 adapters, zero safety gain.

## DECISION 2 — single-owner helper + per-adapter insertion
New in `internal/clients/clients.go` (add `reflect`):
```
func entryRestoreIsNoop(liveEntry any, livePresent bool, backupEntry any, backupPresent bool) bool {
    if livePresent != backupPresent { return false }
    if !livePresent { return true }
    return reflect.DeepEqual(liveEntry, backupEntry)
}
```
- **codex** (already reads live): after liveServers resolved, before write branch:
  `if allowHubEntry { le,lp := liveServers[name]; be,bp := backupServers[name]; if entryRestoreIsNoop(le,lp,be,bp) { return nil } }`
- **JSON-family** (json_mcp/claude/vscode/opencode — don't read live): after backupServers computed, before setMember/deleteMember, use adapter's single-file `readJSON()`; read/parse error FALLS THROUGH to restore (no new failure mode):
  `if allowHubEntry { if lm,err:=recv.readJSON(); err==nil { ls,_:=lm[sectionKey].(map[string]any); le,lp:=ls[name]; be,bp:=backupServers[name]; if entryRestoreIsNoop(...) { return nil } } }`
- **mimo** (MUST read TOP layer o.path, NOT merged readJSON): `readRawConfig(o.path)` (nil→empty on ENOENT) → parseJSONCBytes → `liveMap[mimoCodeMCPKey]` → same compare.

## DECISION 3 — semantics + P1-non-regression
Parsed-map deep-equal of target entry VALUE at write-target layer, presence-aware. AddEntry mutates ONLY target entry (stdio→url) ⇒ mutated entry never deep-equals stdio backup ⇒ **mutated client NEVER skipped**. Only verbatim-copy (unmutated) ⇒ true.
- #2 hub relay: live {url}≠backup {command} ⇒ restore RUNS (revert / fail→sentinel→preserve).
- #1 removed: live absent, backup present ⇒ presence differs ⇒ restore RUNS (re-create).

## DECISION 4 — degenerate table
| backup | live | isNoop | action |
|---|---|---|---|
| present | present deep-equal | true | SKIP (unmutated) |
| present | present differ (#2) | false | restore (revert) |
| present | absent (#1) | false | restore (re-add) |
| absent | present | false | restore (delete) |
| absent | absent | true | SKIP |
| any | non-ENOENT read err | — | codex: body err→sentinel; JSON/mimo: fall through→restore |
**Sibling edit (Sol P2 closed):** target==backup, sibling differs ⇒ entry-scoped isNoop=true ⇒ SKIP ⇒ file untouched ⇒ sibling preserved.

## TOCTOU closed
Compare is INSIDE `restoreEntryFromBackupWithWriter` under the single withConfigLock hold. No unlocked pre-check. Lock-respecting concurrent writers blocked for the whole compare+write critical section.

## Install-closure simplification (install.go)
- DELETE `liveConfigMatchesBackup` helper + `bytes` import (verify no other use).
- DELETE the match-guard block in the rollback closure.
- Closure = call `RestoreEntryFromBackupForRollbackWithConfigWriter(...)`; err→append clientName + log; nil→log. failAfterRollback/sentinel/append UNCHANGED. Reword success log outcome-accurate (skip OR restore), e.g. "reconciled to pre-adopt backup".

## Tests
- `TestExecuteAdopt_MatchGuardSkipsRestoreOnUnmutatedClient` stays load-bearing (mechanism moves: restore returns nil BEFORE the write seam; restoreFailRemoved never fires). Adjust seam expectation: restore-write NOT invoked for unmutated client. Still fails pre-fix.
- NEW entry-scoped sibling test (per family codex/JSON/mimo): live target==backup, sibling differs ⇒ restore returns nil + write-target byte-unchanged. Fails on whole-file compare; passes entry-scoped. Load-bearing for Sol P2.
- NEW barrier atomicity test: goroutine A holds withConfigLock on the path; B calls rollback restore same path; assert B blocks until A releases. + `-race -count=10` restore-vs-AddEntry contention, no data race.
- Regression sweep: full internal/clients adapter suites + serena-reconcile + lsp-router rollback (shared body now serves them) — confirm no test asserts write-on-no-op.

## P3 doc softening (3 missed sites → "restoration could not be confirmed")
- adopt_provenance_events.go:123-127 (emitter comment "had rewritten to the hub relay could not be restored" + "un-restored clients").
- CLAUDE.md:1404 "un-restored client NAMES + count" → "NAMES + count of clients whose pre-adopt restoration could not be confirmed".
- adopt_abort_preserve_provenance_test.go:3-5 (file-doc opening comment).
- Post-grep: `grep -n "un-restored\|could not be restored\|already rewritten to the hub relay" CLAUDE.md internal/api/*.go` = zero live hits.

## Protected / must-not-touch
failAfterRollback + sentinel + rollbackClientRestoreFailures append-on-error; FIX-1 register-before ordering; demigrate allowHubEntry=false + ErrBackupEntryAlreadyMigrated; withConfigLock (no new lock/reentrancy); Client interface (no new method); non-adopt adapters' restore bodies; writeBackup/SecureWriteClientConfig/WriteConfigFileFunc contracts; reverted FIX-2.

Gate: `go build ./... && go vet ./... && go test -count=1 -timeout 6m ./internal/api/... ./internal/clients/...` + tagged `./internal/api/ ./internal/cli/`; sweep mcphub.
