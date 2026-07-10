---
status: proposed
date: 2026-07-10
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

## Falsifiable claims

See design addendum claims 16-22 (each `{ guarantee, single-owner, enforcement-probe }`);
they are the 1:1 review-finding inputs for the `$architecture-reviewer` gate that
promotes this decision `proposed → accepted`.
