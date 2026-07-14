# Commission round 6 — verdict: REVISE (Sol + Terra CONVERGE on codex re-read-fail P1)

Date 2026-07-13. Round-6 = atomic create-if-absent (CreateConfigFileIfMissing) + codex fresh re-read on fall-through.

## Round-5 P1 (atomic-create) — CLOSED (both reviewers)
The `os.Stat` is now only a branch hint; the no-replace `CreateConfigFileIfMissing` is the AUTHORITATIVE
publish. If S' appears in the stat→create window, stale backup bytes cannot land (`created=false` → fall
through). The 5 JSON/JSONC bodies fall through to fresh read-modify-write (setMember → readRawConfig at
mutate) → no stale whole-map retained. Round-5 stat→write overwrite window CLOSED.

## NEW P1 (Sol + Terra IDENTICAL) — codex ignores the authoritative re-read failure
`internal/clients/codex_cli.go:241-249`: on the EEXIST fall-through, codex re-reads `readTOML()` but on
FAILURE silently retains the earlier (pre-recreate, empty) `liveMap`. Scenario: path #1 removes the file;
initial liveMap empty; external process recreates it with sibling S'; atomic create returns created=false;
the fresh read then fails (transient / partial TOML); codex writes `{E}` off the stale empty map →
clobbers S' → reports rollback success → adopt deletes provenance. Same silent sibling-clobber the whole
round chased, via the codex whole-map read-once-write pattern (the 5 JSONC bodies are immune).
**Fix (both):** treat the fresh read as AUTHORITATIVE — return its error → rollback sentinel → PRESERVE.
Sol's preferred structure: MOVE the codex rollback live read to AFTER the helper and use that map EXACTLY
ONCE (eliminates the stale-map-fallback possibility entirely). Add a conflict-plus-read-error test
asserting PRESERVE + unchanged recreated bytes.

## Terra minor (non-blocking)
Duplicate identical `createCount` assertion in `TestExecuteAdopt_Path1WholeFileGone_PreservesWhenWholeRestoreFails`
— remove as residue. `seam declared and not used` + `unreachable code` diagnostics are STALE, not real (both confirm).

## DISPOSITION — round-7 (small, fully specified by both reviewers)
Restructure codex `restoreEntryFromBackupWithWriter`: for `allowHubEntry=true`, read the live map ONCE
AFTER the helper falls through; on read failure return the error → sentinel → PRESERVE (never fall back
to a stale map). Keep the demigrate (`allowHubEntry=false`) single early read untouched. Add
`TestExecuteAdopt_Path1WholeFileGone_ConcurrentRecreate_CodexReadFailPreserves` (conflict + fresh-read
error → assert PRESERVE, sentinel, recreated bytes with S' unchanged). Clean the duplicate assertion.
No architect round — the fix is fully specified. Then re-run Sol+Terra (final confirm).
