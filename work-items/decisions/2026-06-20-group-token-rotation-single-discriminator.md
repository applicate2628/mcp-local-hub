---
status: accepted
date: 2026-06-20
slug: group-token-rotation-single-discriminator
deciders: architect (opus) + lead
pr: 395
---

# Group-token rotation: one discriminator (cfg.Groups + in-memory active-set), delete the tombstone machinery

## Context

PR #395 bundles a CLEAN secure-write post-rename fix and a group-token ROTATION feature added to close one
audit P3 (a group DELETE whose token-prune FAILED leaves the `g:<name>` row, so a re-created same-name
group reuses the stale/leaked token). The rotation drew SIX P2 bot findings in one round
(durable-orphan-proof / tombstones, revoke-stale-active-snapshots-on-rotation-failure,
don't-downgrade-missing-tombstones, don't-mask-tombstone-write-failures, don't-rotate-clean-after-stale-empty-snapshot,
drop-stale-snapshots-on-orphan-rotation-failure) — a P3 fix spawning 6 P2 bugs = over-engineering signal.

## Architect findings (verified, file:line)

- The threatened asset is loopback-only behind two more gates: the `g:<group>` token is checked
  (`ConstantTimeCompareToken`, hub_mcp_handler.go:226) only AFTER the loopback/DNS-rebind guard
  (:126-132) and BEFORE the instance-id gate (:234-239); the listener binds 127.0.0.1 only
  (gui/server.go:915). A co-resident who could replay a stale token can already read
  hub-mcp-tokens.json directly. Low severity by construction.
- The trigger is doubly rare AND self-correcting: the row only survives when `pruneHubTokensLocked` fails
  (a state-file write failure); the GUI already forces `restart_required=true` (gui/groups.go:455), and on
  the next publish `cfg.Groups` no longer declares the deleted group, so its row is never ensured/served.
- The tombstone gives near-zero marginal durability: it is written through the SAME `writeHubMcpStateFile`
  pipeline (hub_mcp_tokens.go:183) that just failed to prune — in the realistic failure mode it fails for
  the same root cause.
- All 6 P2s live in ONE place: the snapshot-vs-tombstone reconciliation (two persistent discriminators that
  must agree / whose write can fail). The bug class is intrinsic to having a SECOND source of truth.
- The simple invariant already sits in the function: `PublishGroupsSnapshotLocked` already loads
  `cfg.Groups` (hub_mcp_resolver.go:417) — the authoritative current declared set. The tombstone is
  redundant with it.

## Decision (Option B — code reduction; Option C fallback)

DELETE the tombstone machinery (`hub-mcp-group-token-orphans.json`, `groupTokenOrphanTable`, the
mark/clear/load/write orphan functions, the `rotateUntracked` flag, the delete-path tombstone write).
REPLACE the rotate/tombstone block with ONE discriminator: **rotate a `g:<k>` row iff k is declared in the
current `cfg.Groups` AND the row pre-exists AND k was NOT in this process's prior in-memory live
active-set** (the recreate-over-stale case). Cold-start trusts `cfg.Groups` as authoritative-active (a
declared group's pre-existing row is a legitimate token that must survive a clean restart); a stale row for
a group not in `cfg.Groups` is never ensured/served. Rotation write-failure still fails loud (no stale
token served) — just without a tombstone.

FALLBACK (Option C) if the cold-start branch cannot be made deterministic without an on-disk marker (the
architect found no such case): drop all three rotation files from #395, ship the clean secure-write half,
re-file the P3 as a backlog item. A P3 must not ship a 6-P2 feature.

## Why (over keep-and-patch)

The 6 P2s vanish BY CONSTRUCTION: with the second discriminator (tombstone) deleted there is one
in-memory + one on-disk-config source, nothing to reconcile, no second write that can fail. Net change is a
REDUCTION (deletes more than it adds). Closes the original P3 (recreate lands as declared + not-live ⇒
rotated). Preserves live sessions + cold-start tokens. The secure-write half is untouched.

## Protected (unchanged)

secure_write_{posix,windows,client_config}.go; ensureHubTokensLocked "never rotate existing rows" contract;
ConstantTimeCompareToken / loopback / instance-id gates; the live /g/ routing.
