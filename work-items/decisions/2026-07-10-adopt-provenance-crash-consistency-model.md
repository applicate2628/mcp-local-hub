---
status: accepted
date: 2026-07-10
accepted: 2026-07-10 (validated empirically — security re-verify PASS + Codex bot PASS after the model converged the r2/r3/r4 crash/race/mutation edge clusters; shipped in PR #528)
slug: adopt-provenance-crash-consistency-model
deciders: $architect (design addendum); $architecture-reviewer (promotes proposed → accepted)
context: work-items/active/2026-07-09-adopt-side-durable-pre-adopt-provenance/design.md ("## Crash-consistency + concurrency model (bot r2 resolution)")
supersedes: none
superseded-by: none
---

# Decision: the adopt-provenance capture/GC lifecycle is classified by THREE orthogonal signals, not by `operation_state` + manifest-existence

## Context

Round-1 (HEAD `45073703`) resolved the Codex bot's crash/concurrency findings with
per-edge guards that all used `operation_state` + **manifest-existence** as the proxy
for an `adopting` row's true condition. That proxy is ambiguous, so each guard
revealed the next edge (classic edge-mine): the bot's r2 review surfaced four
interlocking crash/race findings (P1 live-row reap, P2 pre-promote-crash-not-reapable,
P2 rowless snapshot, P2 swallowed cleanup) plus a MiMoCode merged-layer false-abort and
a wire-shape leak. The fixes cannot be independent guards; they need one coherent model.

## Decision

An `adopting` provenance row's condition is classified by **three orthogonal signals,
each with exactly one owner**, so the capture path and the GC can never diverge again:

1. **Owner liveness** — a per-manifest adopt **lease** (`gofrs/flock` on
   `<state-dir>/adopt-provenance/<manifest>.lease`), acquired non-blocking (`TryLock`)
   at the top of `ExecuteAdoptWithOpts` and held across capture→promote/abort. The
   lease IS the liveness authority: `TryLock`-fail ⇒ a live same-manifest adopt exists
   (FAIL CLOSED); `TryLock`-success ⇒ the row's owner is provably dead (reap-eligible).
   Chosen over a `pid`+`start_time` liveness token (equivalent correctness, more code +
   a cross-process liveness probe + pid-recycling edges) and over fail-closed-on-any
   (blunt; blocks same-manifest retry until GC). `flock` is already the repo's liveness
   primitive (`adopted-entries.lock`, `state_file_helper.go` per-file `.lock`).
2. **Install committed** — hub-binding-live via the existing single owner
   `liveEntryMatchesManifestBinding` (`internal/api/managed_entries.go:355-408`, already
   reused by `demigrate.go:426` + `lsp_client_router.go:313`), composed over the row's
   `adopt_clients`. This REPLACES manifest-existence as the committed-vs-crash
   discriminator for a dead-owner row: manifest-exists **and** a live binding ⇒ KEEP
   (committed-but-unflipped); manifest-exists **but no** live binding ⇒ REAP
   (pre-promote crash). No second shape-derivation path (consistent with arch F3).
3. **Durable anchor** — ROW-FIRST capture ordering (a minimal `adopting` row is written
   before any secret-bearing snapshot) plus a snapshot-dir-driven GC backstop (reap any
   `adopt-provenance/<m>/` dir with no store row, under the lease). No crash window
   leaves an unreclaimable rowless secret snapshot.

Capture-reap and GC both route through ONE classifier
(`classifyDeadAdoptingRow`, lease-precondition + signal 2), giving one committed-signal
owner and one reap decision.

Two additional single-owner rules ride the same capture logic:

- **MiMoCode write-target-absent** is a first-class capture state
  (`original_state:"present-merged-lower"`, no snapshot), NOT a capture failure. The hub
  only mutates the write target (`ConfigPath()`); when the entry resolves from a lower
  merge layer (`GetEntry` non-nil) and the write target is absent (`fs.ErrNotExist`),
  de-adopt restores by removing the hub entry from the write target and the untouched
  lower layer re-emerges. A genuinely-vanished present-at-Build entry (`GetEntry` nil)
  still fails closed. Additive enum value (no schema-version bump).
- **`AdoptPlan.PresentAtBuild`** is unexported (or `json:"-"`) so the internal
  fail-closed set never serializes into the `/api/adopt/plan` response.

## Consequences

- One new lock leaf per manifest (`<manifest>.lease`); lock order
  `<manifest>.lease → adopted-entries.lock → <snapshot>.lock` (acyclic, `TryLock`-based
  reaper acquisition, deadlock-free).
- The round-1 guards (`adopted_entries.go:372-388` sole finding-1 defense, `:678`
  manifest-exists keep-guard, `:394/:425` snapshot-before-row order, `:399` swallowed
  cleanup, `:491-493` ENOENT-is-a-failure) are collapsed into this model, not extended.
- De-adopt gains one consumer-contract obligation: handle `present-merged-lower` by
  removing the hub entry from the write target (no snapshot restore).
- The only remaining bounded residual: an abandoned cross-manifest orphan's owner-only
  secret snapshot lingers ≤ 24 h until the next adopt/supervisor-startup GC (unchanged
  from the base design; the lease now makes an earlier reap safe if tightened later).

### Addendum 2026-07-12 (#532 — committed-drift KEEP + capture-lane symmetry; case-3 residual)

- **Committed-but-manifest-present KEEP (Signal 2b, #532):** a dead-owner `adopting` row
  whose live hub binding has DRIFTED away (gate-ON reconcile / port-edit+reinstall /
  demigrate move-or-drop the binding but LEAVE the manifest) is KEPT via manifest-existence,
  not reaped. The manifest is the "last artifact standing" of a committed adopt (written at
  `ManifestCreate` before `Install`; every narrower Install footprint — client config,
  managed-entries row, supervisor-intent descriptor, backup — is removed by some routine op
  that leaves the manifest live), so manifest-exists is the correct fail-closed committed
  anchor.
- **case-3 residual (ACCEPTED — do NOT "fix" by removing the guards):** a crash AFTER
  `ManifestCreate` but BEFORE `Install` leaves an `adopting` row whose manifest exists but
  which never committed a binding. Signal 2b KEEPs it, so its owner-only secret snapshot
  lingers. This is INDISTINGUISHABLE on disk from a fully-committed adopt whose config was
  later reverted (both: manifest present, configs == snapshot, no live binding) — the
  "did Install run" fact leaves no on-disk trace — so KEEP is the correct destructive-default
  polarity (wrong-toward-REAP destroys a committed adopt's de-adopt linkage; wrong-toward-KEEP
  is a bounded owner-only residual whose secrets also live in the vault). It SELF-HEALS: to
  re-adopt M the operator must remove the stale manifest, after which the next capture-UPSERT
  reclassifies the row (`CrashReap`) and reaps the snapshot. The full triple-orphan cleanup
  (manifest + vault keys + row) is routed to de-adopt, not the GC (bug
  `2026-07-12-adopt-preinstall-crash-orphan-triple.md`). Architect adjudication 2026-07-12.
- **"One reap decision" (claim 22) restored across BOTH reap lanes:** #532 added the
  positive-evidence Part-2 gate (`adoptRowProvablyUnmutatedFn`) to the GC reap; the SAME gate
  is applied to the capture-UPSERT reap (a committed-but-manifest-deleted row that classifies
  `CrashReap` is refused, not reaped) so both reap return-paths route through classify + Part-2
  (all-return-paths discipline). Disclosed over-block (fail-safe refuse on config churn /
  absent-fanout clients) + the sharper churn-immune predicate + a `forget` escape are tracked
  in backlog `2026-07-12-adopt-provenance-reap-predicate-native-entry-and-forget.md`.

## Falsifiable claims

See design addendum claims 16-22 (each `{ guarantee, single-owner, enforcement-probe }`);
they are the 1:1 review-finding inputs for the `$architecture-reviewer` gate that
promotes this decision `proposed → accepted`.
