# Design round-6 — atomic create-if-absent whole-file recovery (closes Sol r5 TOCTOU P1)

Architect gate PASS 2026-07-13. Closes Sol round-5 P1 (absence-check not atomic with whole-file replacement). Keeps round-5 whole-file recovery for the no-conflict case + round-4 skip + core P1. Additive; no new interface/lock/production-var.

## Verified facts
- `clients.CreateConfigFileIfMissing` (package var, write.go:83) ALREADY exists + production-wired: api.init() (client_write_init.go:48) swaps it to `secureCreateClientConfigIfMissingWithOperatorOpt` → hardened NtCreateFile-relative no-replace create (Windows: openDirHandleNoReparse + refusePreexistingReparsePoint + ntCreateRelative(FILE_CREATE) + ntRenameRelativeNoReplace + post-rename DACL verify; POSIX: O_CREAT|O_EXCL|O_NOFOLLOW 0600; owner-only DACL at create). Contract: (created,err): true,nil=wrote via no-replace; false,nil=EEXIST/existed (idempotent, NOT written); false,err=refusal(symlink/reparse)/hard-fail.
- Surgical fall-through is FRESH-LIVE for 5/6 bodies (json_mcp/claude_code/vscode/opencode/mimocode → setMember/deleteMemberWithWriter → mutateJSONObjectMemberPathWithWriter → readRawConfig re-reads live at mutate, jsonc.go:265, patches only E → preserves siblings). CODEX reads liveMap ONCE at codex_cli.go:208 (before helper) + surgical write uses that pre-read map (writeTOMLWithWriter(liveMap), :252-259) → does NOT re-read.
- Round-6 recovery via CreateConfigFileIfMissing BYPASSES the WriteConfigFile seam → the test harness recovery-write counter must move to the create seam.

## CHANGE 1 — helper (clients.go)
`wholeFileRestoreIfWriteTargetGone(path string, backupData []byte) (handled bool, err error)` — DROP the `writer` param.
| Branch | Result | Body meaning |
|---|---|---|
| len(backupData)==0 | (false,nil) | surgical |
| os.Stat ok OR non-ENOENT err | (false,nil) | file present → surgical (round-5 preserved; NO create) |
| os.Stat==ENOENT → CreateConfigFileIfMissing(path,backupData) err!=nil | (true,err) | refusal/fail → sentinel → PRESERVE |
| … created==true | (true,nil) | whole file recovered race-free |
| … created==false (EEXIST) | (false,nil) | CONFLICT (concurrent recreate) → fall through to surgical |
Not-a-TOCTOU: os.Stat is a non-authoritative fast-path gate ("are we in file-gone branch?"); the no-replace create is the authoritative publish. File reappears in stat→create window → EEXIST → bytes never published → fall to fresh-live surgical. Call site pattern `if handled, werr := ...; handled { return werr }` byte-identical to round-5 (only dropped arg).

## CHANGE 2 — codex re-read (codex_cli.go) — LOAD-BEARING
codex surgical write uses `liveMap` read at :208 (before helper). On EEXIST fall-through liveMap is stale-empty → clobbers S' (moves the data-loss). FIX: after the helper falls through (inside `if allowHubEntry`, gated), RE-READ liveMap/liveServers FRESH so the whole-map surgical write reflects current disk (with S'). No-op-equivalent for non-race path-#2 (fresh read == :208 read); preserves S' in conflict. Demigrate (allowHubEntry=false) keeps its single :208 read untouched.

## CHANGE 3 — 5 JSONC bodies: arg-drop only (claude_code:220, json_mcp:241, opencode:336, vscode:225, mimocode:4069). Logic unchanged (fresh readRawConfig at mutate already preserves S').

## Gating/co-callers/perf: no regression. os.Stat gate keeps create off ALL file-present paths (common path #2, serena-migrate, LSP-router). Create attempted ONLY in file-gone branch. mimo top layer o.path confirmed.

## Tests (adopt_abort_preserve_provenance_test.go)
- Seam: extend seedInstallWriteSeam to save/restore-swap `clients.CreateConfigFileIfMissing` (create-double + createCount) + `recover` outcome on clientWriteSpec. Double keys on live file-presence (disambiguate InitEmpty stub-create [present→(false,nil) delegate] from recovery create [absent→apply script]). Unspecified paths delegate to original.
  - recoverCreated → realWrite(path,backupData) + (true,nil).
  - recoverConflict → realWrite(path, recoverConflictBytes [inject external S']) then (false,nil) [absent at helper stat, present at create].
  - recoverFail → (false, injectedErr).
- NEW conflict tests:
  - `TestExecuteAdopt_Path1WholeFileGone_ConcurrentRecreateConflict_Codex`: {addEntry:addEntryFailRemoved, recover:recoverConflict, restore:restoreSucceed}. Assert final codex parses + holds BOTH S' AND E; createCount==1; writeCount==2 (AddEntry + surgical); snapshot DELETED (clean abort). FAILS without the codex re-read (stale liveMap clobbers S').
  - `..._ConcurrentRecreateConflict_JSON` (cursor): same shape; member-set fall-through preserves S' (passes on existing surgical).
- Retarget round-5 tests to create seam: TargetPresentSibling/TargetAbsentSibling/MimoTopLayerSibling → restore:restoreSucceed becomes recover:recoverCreated; assert file has E+S, writeCount==1 (AddEntry only) + createCount==1. Non-vacuous (neuter helper → surgical loses S).
- Preserve variant → recover:recoverFail; create hard-fails → (true,err) → sentinel → PRESERVE; createCount==1.
- Barrier tests + round-4 MatchGuardSkips UNCHANGED (file-present skip path, no create).

## Protected / must-not-touch
SecureCreateClientConfigIfMissing/SecureCreateOwnerOnlyFile/impl (consume as-is); entryRestoreIsNoop; withConfigLock; 5 JSONC surgical writers logic; demigrate allowHubEntry=false; abortAdoptProvenance (no .bak prune); failAfterRollback+sentinel+FIX-1; Client interface; WriteConfigFileFunc/SecureWriteClientConfig contracts; round-4 skip; barrier+MatchGuardSkips tests.

## Adjacent (NOT round-6): non-writer-only adapters (aider/goose/cline/...) lack the helper — round-5 scoping question, not a round-6 defect. Do NOT expand scope.

Gate: `go build ./... && go vet ./... && go test -count=1 -timeout 7m ./internal/api/... ./internal/clients/...` + tagged `./internal/api/ ./internal/cli/` + `-race -count=10` barrier/contention/conflict; sweep mcphub.
