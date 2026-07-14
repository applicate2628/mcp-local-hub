# Commission round 5 — verdict: Terra PASS; Sol REVISE (1 NEW P1 TOCTOU)

Date 2026-07-13. Round-5 = whole-file-gone recovery helper before the entry-skip.

## Sol round-4 P1 — ALL VARIANTS CLOSED (Sol confirms)
- target-present (E+S, file gone) → os.Stat ENOENT → whole-restore E+S. CLOSED.
- target-absent (S only) → helper before isNoop → whole-restore S (no false-skip). CLOSED.
- persistent restore failure → handled=true,err!=nil → sentinel → PRESERVE. CLOSED.
- mimo top-layer o.path, gating (demigrate untouched), empty-backup defer — all correct.

## Terra r5 — PASS (all 3 angles: concurrency/control-flow/redaction)

## Sol r5 NEW P1 — absence check not atomic with whole-file replacement (TOCTOU)
`internal/clients/clients.go` helper: `os.Stat(path)` observes ENOENT, then `writeConfigFileWith` writes
the whole stale backup. Between them a NON-lock-honoring EXTERNAL process (client app / editor / sync)
can recreate the file with a new/modified sibling S'; the whole-backup write clobbers S' with stale
pre-adopt bytes → returns nil → false rollback success → abort deletes the snapshot → S' lost.
`withConfigLock` excludes only lock-honoring participants, not external processes.
Sol's required fix: publish whole backup ONLY if the target is STILL absent, as ONE atomic writer op
(create/no-replace or rename/no-replace). If destination exists at write time → not-handled/conflict →
re-read → surgical entry restore (which re-reads fresh live + preserves external siblings). A conflict
must NEVER fall through as successful reconciliation. Add a barrier test (recreate file with changed
sibling between absence-observation and publication; prove the whole backup does NOT overwrite it).

Orchestrator note: this is a WORSE-than-surgical variant of the pre-existing NON-OWNED cross-process
config-write race (client configs have no cross-process lock). Narrow (needs path #1 + external recreate
+ microsecond timing), but Sol rates P1 (silent data-loss).

## DECISION — user chose "Round 6: fix it properly" (2026-07-13)
Atomic create-no-replace + on-conflict-surgical. The whole-file write becomes create-if-absent
(O_CREAT|O_EXCL semantics, owner-only-hardened per EnsureClientConfigStub / the planned
SecureCreateClientConfigIfMissing): success ⇒ file created from backup (was absent, race-free); EEXIST
(concurrent recreate) ⇒ helper returns handled=false ⇒ body falls through to the surgical restore
(re-reads fresh live, preserves external S'). Add the race-window barrier test. Core P1 + round-4 P1 +
round-5 skip all stay. Architect designs; then backend; then re-run Sol+Terra.
