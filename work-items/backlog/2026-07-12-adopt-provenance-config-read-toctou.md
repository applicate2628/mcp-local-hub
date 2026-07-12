# Backlog: adopt-provenance unmutated-proof reads client configs without a per-config lock (TOCTOU)

Filed: 2026-07-12
Priority: P2 (Sol + Terra both rated P1; downgraded here because it is PRE-EXISTING, shared with the GC lane, and fail-safe-leaning — a lost race most often yields a sha-mismatch => KEEP/refuse, not a wrong reap)
Source: Sol + Terra concurrency lanes, #532 commission
Context: PRE-EXISTING (shared property of `adoptRowProvablyUnmutated`, which the GC lane already used before #532; #532 extends the SAME predicate to capture). Filed separately per user decision 2026-07-12.

## The race

`adoptRowProvablyUnmutated` (`internal/api/adopted_entries.go:993-1012`) reads each present
client's live config with raw `os.ReadFile(adapter.ConfigPath())` — it holds the per-manifest
lease and the `adopted-entries.lock` store lock, but takes NO per-config read lock
(`internal/clients/config_lock.go:160` — read access is not under the mutation lock). So:

- Between the guard reading client A and reaping, a concurrent writer (a DIFFERENT-manifest
  install, or an EXTERNAL client app) can change A's config.
- Across multiple clients A/B, the guard can observe A-unmutated then B-unmutated at
  different instants even if no single instant has both simultaneously unmutated.
- Then capture / GC deletes snapshots on evidence that was never globally consistent.

The per-manifest lease protects ROW ownership, not the evidence bytes. It DOES exclude
concurrent same-manifest adopt/GC (the realistic committer of THIS row); the residual is
different-manifest installs + external apps writing shared config files.

## Why fail-safe-leaning (not an easy data-loss)

For the race to WRONGLY reap a committed snapshot, the client would have to (a) actually be
committed (hub-rewritten — mutated bytes) yet (b) read as byte-identical to the pre-adopt
snapshot at the guard instant. A concurrent write generally makes the whole-file sha DIFFER
=> the guard refuses/KEEPs (fail-safe). The wrong-reap requires an adversarial/bizarre
writer that momentarily restores exact pre-adopt bytes then re-mutates. Low realistic risk.

## Fix options (both lanes)

- Snapshot the client-config bytes ONCE per client under the config's own read lock and gate
  on that snapshot (single consistent read), OR hold a config read lock across the
  gate→reap window.
- OR move to the entry-shaped predicate (see backlog
  `2026-07-12-adopt-provenance-reap-predicate-native-entry-and-forget.md`) which reads the
  same config bytes but is churn-immune — still needs the single-consistent-read fix for the
  cross-client instant issue.
- Applies to BOTH lanes (GC + capture) since they share the predicate — fix once at the
  predicate owner.

## Relation to #532

Not introduced or worsened by #532 (the GC lane had this exact property; #532 reuses the
same predicate in capture). Sol: "The manifest lease protects row ownership, not the
evidence bytes." Deferred to a predicate-hardening pass.
