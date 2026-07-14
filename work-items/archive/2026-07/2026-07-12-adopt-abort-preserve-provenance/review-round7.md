# Commission round 7 — FINAL: Sol PASS (governs); Terra REVISE (out-of-scope non-owned race)

Date 2026-07-13. Round-7 = codex read-once-after-helper, return error on read-fail → preserve.

## Sol r7 (mandatory acceptance) — PASS
| Item | Verdict |
|---|---|
| Round-6 P1 (codex ignored fresh-read failure) | **CLOSED** — rollback reads once after the helper; a read error returns at codex_cli.go:244 → sentinel → preserve. |
| Read-once invariant | PASS — one authoritative post-helper read (allowHubEntry=true) / one early read (else). No stale-map fallback. |
| Sentinel → preserve | PASS — read error → append codex-cli → InstallClientRollbackIncompleteError → ExecuteAdopt preserves row+snapshots+manifest+keys. |
| Demigrate | PASS — original read + ErrBackupEntryAlreadyMigrated on the else branch, only placement changed. |
| 5 JSONC bodies | PASS — optional skip-read errors fall through; setMember/deleteMember re-read at mutate; no retained stale whole-map. |
| Prior fixes (r1 register-before, r3 revert-FIX2, r4 skip, r5 whole-file, r6 atomic-create) | PASS — all intact. |
| Round-7 test | Load-bearing (I proved it: neutered → false-success + provenance deleted). |
**Overall: PASS.** Residual = the documented non-owned external-process race (write after an authoritative read/create) — OUTSIDE this change's scope.

## Terra r7 — REVISE (P1), classified OUT-OF-SCOPE by Sol
codex_cli.go:241: after the fresh readTOML() and before the whole-map write, a NON-lock-honoring external
writer can add {S''} → codex's whole-map write clobbers it → false success. Terra's fix: version/identity-
conditional publish, retry-on-conflict, or preserve.

## Orchestrator disposition — Sol PASS governs; Terra finding filed as follow-up
Terra's race is the PRE-EXISTING, architecturally NON-OWNED cross-process config-write race: codex's
whole-map read-modify-write has this exact window in EVERY codex op (AddEntryWithConfigWriter,
RemoveEntry, restore) — NOT introduced by this change. Client configs have no cross-process lock
(CLAUDE.md; the architect stated it across rounds 3/4/5). Fixing it requires optimistic-concurrency/CAS
or versioned config writes on the whole file — a major new capability affecting the entire codex adapter,
disproportionate to a rollback data-loss fix. Sol (mandatory acceptance) explicitly scopes it out and
PASSes. FILED as follow-up: `work-items/backlog/2026-07-13-codex-whole-map-rmw-cross-process-race.md`.
PROCEED to merge on Sol's PASS.
