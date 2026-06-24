---
title: mimocode managed enable-only overlay over a below-target survivor re-emerges after RemoveEntry (intended B4 rollback; operator may expect the managed enable to pin it)
severity: P3
found-by: codex-bot (PR #425 follow-up review) + architect (GATE REVISE → PATH-B)
found-in-phase: PR #425 follow-up — managed-OR simplification (architect-ruled ACCEPTABLE RESIDUAL, PATH-B)
affected-surface: >
  internal/clients/mimocode.go mimoCodeManagedLayerReResolves
  (managed-own-value-only predicate; the managed verdict never reads
  readMergedLayersExcluding / the below-layer merge)
context: adjacent-finding
status: open
---

## Summary

`mimoCodeManagedLayerReResolves(name)` is deliberately NARROWED to read ONLY
the managed layer's OWN value (the macOS MDM plist or the cross-platform
managed config dir, via `mimoCodeManagedLayerShadows` + the disable-only
subtract through `mimoCodeShadowIsDisableOnlyOverride`). It NEVER reads the
below-layer / file merge (`readMergedLayersExcluding`).

This replaced the prior EFFECTIVE-managed-merge revision, which the architect
ruled a WRONG ABSTRACTION (it created a two-owner invariant — the RemoveEntry
managed pre-check vs the post-delete B4 guard — impossible to hold by hand, and
gave the F3 enable-over-lower feature an irreducible conflict with B4). The
narrowing makes the pre-check structurally incapable of diverging from B4, at
the cost of two documented residuals below.

## Residual F3-over-below-layer (the intended-rollback re-emergence)

A managed `{enabled:true}`-only overlay (a bare enable flag, no
type/command/url) is correctly NOT a shadow, so `mimoCodeManagedLayerShadows`
returns `Kind == ""` and `mimoCodeManagedLayerReResolves` reports the managed
layer retains NOTHING on its own → the write-target entry is REMOVABLE.

If a content-bearing `config.json` entry survives BELOW the write target, then
after `RemoveEntry` deletes the write-target key, MiMoCode field-merges the
managed `{enabled:true}` overlay onto that surviving lower entry and the server
RE-EMERGES from `config.json`.

**This is CORRECT.** A name present only BELOW the write target is an operator
entry the hub never wrote; its re-emergence is the INTENDED B4 rollback
behavior (the operator's prior config re-emerges), not a leak. The B4
post-delete guard excludes the below-target layer from
`mimoCodeHigherLayerDefining` for exactly this reason, so it too ALLOWS the
delete. So "removable" is the right verdict and the pre-check and B4 AGREE.

The residual is ONLY a perception mismatch: an operator who deployed a managed
`{enabled:true}`-bare-flag overlay MIGHT expect that managed enable to PIN the
server (keep it active and un-removable). It does not — the managed enable
re-activates a lower survivor but the hub still removes the write-target entry
it owns, and the survivor is the operator's own below-layer config. There is
**NO data loss**: the `config.json` entry survives intact, and re-emergence is
the desired rollback.

This requires a NON-DEFAULT bare-enable-flag MDM/managed posture: a managed
overlay that carries `enabled:true` and NOTHING else, layered over a
content-bearing `config.json` below the hub write target. A managed FULL
redefine or a DISABLING overlay is a shadow (`Kind != ""`) and is handled by
the predicate normally (retained / disable-only-removable respectively).

## Residual F4 managed-chain (exotic, no data loss)

A chain of managed overlays across both managed layers (the MDM plist
`{enabled:true}` over a managed-config-dir `{enabled:false}` over a surviving
lower command) would, under the prior effective-merge revision, have re-enabled
to active and been retained. Under the managed-own-value-only predicate each
managed layer is judged by its OWN shadow shape: the MDM enable-only overlay is
`Kind == ""` (not a shadow), so it does not retain on its own; the
managed-config-dir `{enabled:false}` is a disabling shadow but classified
disable-only → removable. So such a chain reports removable and the server
re-emerges from whichever layer (managed-config-dir or the lower command) still
supplies content.

This is an EXOTIC, deliberately-deployed multi-managed-layer posture with NO
data loss — the re-emergence again lands on the operator's own surviving
layer, and no hub-owned content is lost. It is not produced by any default or
normal operator workflow.

## Why this is the right shape (architect ruling, PATH-B)

- The EFFECTIVE-managed-merge that "fixed" these cases was a WRONG ABSTRACTION:
  it made the managed verdict depend on the below-layer/file merge, creating a
  two-owner invariant the pre-check and B4 could not be guaranteed to share.
  Three of the four bot edges it introduced were consistency-taxes of that
  split; the fourth (F3-over-below-layer) was the feature's irreducible
  conflict with B4's intended below-layer rollback.
- The managed-own-value-only predicate composes the SAME two readers the B4
  post-delete guard uses (`mimoCodeManagedLayerShadows` +
  `mimoCodeShadowIsDisableOnlyOverride`), so the pre-check and B4 CANNOT diverge
  BY CONSTRUCTION — the four edges are structurally unreachable.
- Both residuals are perception-only / exotic, cause NO data loss, and resolve
  into intended-rollback re-emergence onto the operator's own surviving layer.

## Documentation

A pointer comment is recorded inline in the
`mimoCodeManagedLayerReResolves` doc (internal/clients/mimocode.go), at the
no-managed-shadow branch, referencing this entry.

## Possible fix directions (for the orchestrator to prioritize, low priority)

1. Leave as documented residual — the realistic blast radius (a non-default
   bare-enable-flag managed posture, no data loss, intended-rollback
   re-emergence) does not justify re-introducing the two-owner effective-merge
   invariant the architect rejected.
2. If a future requirement genuinely needs a managed `{enabled:true}`-bare
   overlay to PIN a below-layer server (keep it un-removable), that is a
   distinct feature with its own owner — it must NOT be re-folded into the
   managed re-resolve predicate without solving the pre-check/B4 single-owner
   problem (e.g. by having BOTH the pre-check and B4 delegate to one
   merge-aware owner, not by re-splitting the verdict across two readers).
