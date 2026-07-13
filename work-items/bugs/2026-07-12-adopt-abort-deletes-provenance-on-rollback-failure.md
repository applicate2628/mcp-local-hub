# Bug: adopt abort deletes pre-adopt provenance even when Install's client-config rollback FAILED

Status: closed (fixed on branch fix/adopt-abort-preserve-provenance-on-rollback-failure)
Filed: 2026-07-12
Severity: P1 (Sol / final-decisions lane) — data-loss class, but with an Install-backup backstop (fable: "pre-existing residual"). Fix-owner to reconcile.
Source: Sol (codex gpt-5.6-sol xhigh) wider destructive-path review during #532 commission
Context: PRE-EXISTING (NOT introduced by #532; #532 touched only the capture-UPSERT reap). Filed separately per user decision 2026-07-12 (merge #532, fix P1s separately).

## The path

`ExecuteAdoptWithOpts` (`internal/api/adopt.go:310-330`): on ANY `Install` error it calls
`abortAdoptProvenance(rec)` (`:317`), which **unconditionally** deletes the current adopt's
row + secret snapshots (`internal/api/adopted_entries.go:888`), then `ManifestDelete` +
`deleteAdoptRoutedSecrets`.

But `Install`'s own compensating rollback of the client configs it already rewrote is
**best-effort and cannot surface failures**: rollback callbacks cannot return errors and
client-restore failures are only PRINTED (`internal/api/install.go:2532`, `:2702`). So if
`Install` partially committed (rewrote client A's config to a hub relay) then failed on
client B, and its rollback then FAILED to restore A, then:

- client A is left MUTATED (still hub-rewritten — effectively committed on A);
- `abortAdoptProvenance` deletes the pre-adopt snapshot of A — the copy needed to restore
  A's original entry.

Net: A is committed-but-orphaned, and the provenance to reverse it is destroyed.

## Backstop (why fable rated it a residual, not P1)

`Install` writes prune-exempt **timestamped backups** (`.bak-mcp-local-hub-*`,
`install.go:2666-2677, :2702-2708`) BEFORE rewriting each client config. So even after the
provenance snapshot is deleted, A's original config may still be recoverable from Install's
own backup. Data is only truly lost if BOTH the provenance snapshot is deleted AND Install's
backup is unavailable/pruned. So the realistic window is narrow (Install-fail AND
rollback-fail AND backup-unavailable).

## Fix (defense-in-depth, per Sol)

Make the abort PRESERVE the provenance whenever Install's rollback failed or its outcome is
uncertain — i.e. `Install` must return a signal (rollback-complete vs rollback-uncertain),
and `ExecuteAdoptWithOpts` must skip `abortAdoptProvenance` (keep the row + snapshots,
leave a recoverable `adopting` row) when rollback is uncertain. This keeps BOTH recovery
copies (Install backup + provenance snapshot) when a partial commit could not be cleanly
reversed. Touches `Install`'s error-return contract (rollback outcome) — a moderate surface,
hence a separate PR from #532.

## Relation to #532

#532 closes the capture-UPSERT reap gap (a PRIOR row destroyed on re-adopt). This is the
SAME data-safety class on a DIFFERENT path (the CURRENT adopt's abort cleanup on Install
failure). Sol confirmed capture-UPSERT + GC are the only *classifier-driven* reaps (both now
gated); this abort path is a *non-classifier* destructive path, so it is genuinely separate
scope. Next up after #532 merges.
