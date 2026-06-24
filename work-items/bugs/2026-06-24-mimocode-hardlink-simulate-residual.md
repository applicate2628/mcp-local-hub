---
title: mimocode re-resolve simulate models a deliberately hard-linked lower-layer config.json as also losing the key
severity: P3
found-by: codex-bot (PR #425 follow-up review)
found-in-phase: PR #425 follow-up — FINDING 1 (architect-ruled ACCEPTABLE RESIDUAL, GATE PASS)
affected-surface: >
  internal/clients/mimocode.go readMergedLayersExcluding (write-target
  match via mimoCodePathsSamePhysical → os.SameFile, inode identity)
context: adjacent-finding
status: open
---

## Summary

`readMergedLayersExcluding(skipName)` simulates the merged read AFTER a
hypothetical `RemoveEntry(skipName)` by dropping `skipName` from a COPY of
the WRITE-TARGET layer's `mcp` map before folding. It identifies the write
target by **inode identity** — `mimoCodePathsSamePhysical(f, o.path)` →
`os.SameFile` — not by path string. The exclusion therefore fires for ANY
layer file that shares the write target's inode.

If an operator DELIBERATELY hard-links a LOWER-layer `config.json` to the
write-target `mimocode.json` (two distinct MiMoCode global layers pointing
at one inode), the simulate models BOTH layers as losing `skipName`, so it
predicts the lower inode's entry also disappears and reports the name
removable.

PRODUCTION diverges: `RemoveEntry` writes via an atomic temp-file + rename
(`setMember`/`deleteMember`), which BREAKS the hard link and leaves the
lower inode's entry LIVE. So the name actually re-emerges from the
still-live lower layer after the removal.

## Why it is an ACCEPTABLE residual (architect ruling, GATE PASS)

- The re-emergence lands on the **BELOW-target layer**, which is the
  INTENDED-rollback layer (a name present only below the write target is an
  operator entry the hub never wrote — its re-emergence is the desired
  rollback behavior, not a leak).
- It requires a **deliberate, non-default manual hard-link** of two
  distinct MiMoCode global config layers — not a configuration the hub or
  any normal operator workflow produces.
- There is **NO data loss**: the lower-layer entry survives intact; the only
  defect is a FALSE-removable prediction that resolves into an
  intended-rollback re-emergence.

## Why NOT switch to path-string equality

`mimoCodePathsSamePhysical` (os.SameFile / inode identity) is REQUIRED by
its four shadow-walk callers (the shadow-detection / at-or-above-ownership
walks at internal/clients/mimocode.go ~1262 / ~1429 / ~1459 / ~1470): they
must treat a path that IS the write target reached by a different name
(symlink, `.`/`..`, case-fold) as the write target. Switching to
path-string comparison there would re-open a real shadow-detection bug
(a write-target reachable by an alias would no longer be recognized as the
write target). The inode-identity helper stays correct for those callers;
this residual is specific to the simulate's hard-link edge and is bounded
as above.

## Documentation

A `KNOWN LIMITATION` comment block is recorded inline at the
`readMergedLayersExcluding` write-target match
(internal/clients/mimocode.go, just above the
`mimoCodePathsSamePhysical(f, o.path)` exclusion), pointing at this entry.

## Possible fix directions (for the orchestrator to prioritize, low priority)

1. After the simulate predicts a name removable, additionally confirm the
   physical write-target file's OWN value defines the name before reporting
   it removable when an inode collision among layer files is detected
   (detect the rare hard-link case explicitly and fall to the conservative
   "not removable" side).
2. Leave as documented residual — the realistic blast radius does not
   justify added complexity in the hot re-resolve path.
