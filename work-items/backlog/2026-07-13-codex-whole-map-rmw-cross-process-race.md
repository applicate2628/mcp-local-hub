# Backlog: codex adapter whole-map read-modify-write has a cross-process TOCTOU (non-lock-honoring external writer)

Filed: 2026-07-13
Priority: P2 (Terra rated P1; downgraded here because it is PRE-EXISTING, architecturally NON-OWNED, affects
ALL codex ops not just rollback, and is fail-loud-adjacent for the abort-preserve fix — the whole-file/
atomic-create + sentinel already close the removed-file and stale-map variants)
Source: Terra r7 lane, adopt-abort-preserve commission (round 7). Sol r7 (mandatory acceptance) PASSed the
abort-preserve fix and explicitly classified this residual as the documented, out-of-scope non-owned race.

## The race
`internal/clients/codex_cli.go` `restoreEntryFromBackupWithWriter` (and, identically, `AddEntryWithConfigWriter`,
`RemoveEntry`, and every codex op): codex reads the WHOLE TOML config into `map[string]any`, modifies one
entry, then writes the WHOLE map back. Between the read and the write, a NON-lock-honoring external process
(the client app / editor / a sync client) can mutate the file (add/change a sibling `S''`). codex's
whole-map write then serializes its earlier snapshot and CLOBBERS `S''`, silently. In the rollback context
this reports rollback success → adopt aborts → provenance snapshot deleted.

## Why NOT owned by the abort-preserve fix
- `withConfigLock` is IN-PROCESS only (documented, CLAUDE.md); client configs have no cross-process lock.
  External-process races are the architecturally NON-OWNED class the architect scoped out across rounds 3-5.
- The window is PRE-EXISTING and pervasive: it is inherent to codex's whole-map RMW (TOML round-trips through
  `map[string]any` — codex has no member-patch primitive like the JSONC adapters' hujson `setMember`, which
  re-read at mutate time and are therefore immune). It affects `AddEntry`/`RemoveEntry`/`Restore` equally —
  the forward install path, not just rollback.
- The abort-preserve fix already closed the IN-SCOPE variants: whole-file removal (round-5 whole-file recovery),
  the stat→create overwrite (round-6 atomic no-replace create), and the stale-map-on-read-failure (round-7
  read-once + error→preserve). This residual is the remaining read→write window against an external writer.

## Fix options (a proper follow-up)
- Optimistic concurrency: read the file's version (mtime+size, or a content hash) at read time; at write time,
  under the lock, re-verify the version and either publish only if unchanged (compare-and-swap) or retry /
  return an error → sentinel → preserve on conflict. Windows: pair with the secure-write handle so the CAS is
  atomic against the rename.
- A codex member-patch primitive (mirror the JSONC `setMember`/`deleteMember` re-read-at-mutate pattern) so
  codex stops serializing a whole pre-read map — closes the window uniformly for AddEntry/RemoveEntry/restore.
- Scope: this is a codex-adapter write-model change (all codex mutating ops), NOT a rollback-only fix. Verify
  the JSONC adapters' member-set path is genuinely immune (Terra + Sol both concluded it is) or also needs CAS.

## Not doing now / why
The abort-preserve PR closes the reported P1 (adopt abort deletes provenance while a client is mutated) plus
the removed-file + stale-map hardening; Sol (mandatory acceptance) PASSed it. Building cross-process CAS for
the entire codex adapter is a separate, larger initiative disproportionate to the rollback fix.
