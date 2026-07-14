# Closure — adopt-abort-preserve-provenance (B / abort-P1)

Closed: 2026-07-13
Outcome: DELIVERED — merged as PR #533 (squash `e1e3f029` on master), deployed, fleet live-verified.

## What shipped
Fix for a P1 data-loss: `mcphub adopt`, on an Install failure, unconditionally deleted the durable
pre-adopt provenance (row + secret snapshots) during cleanup even when Install's own client-config
rollback FAILED to restore a client left pointing at the hub relay — destroying the recovery copy
de-adopt needs while the client was still mutated. Install now returns an additive typed sentinel
`InstallClientRollbackIncompleteError`; adopt PRESERVES the partially-committed state instead of aborting.

Hardening landed alongside (7 review rounds):
- register the restore compensator BEFORE AddEntry (post-rename verify/reopen can return error with the
  config already mutated/removed);
- entry-scoped skip-if-unchanged folded into the 6 adopt restore bodies (single-owner `entryRestoreIsNoop`,
  under withConfigLock) — closes the whole-file-compare TOCTOU + sibling-edit;
- whole-file-gone recovery via the hardened no-replace `CreateConfigFileIfMissing` (EEXIST → surgical
  fall-through; codex reads its whole map once after the helper, error → sentinel → preserve);
- reverted the GC routed-key auto-deletion (could delete a key owned by another live adopt; not crash-safe)
  → routed-key cleanup routed to de-adopt.

## Verification / gates
- 7-round commission: Sol (mandatory acceptance) r7 PASS; architecture-reviewer PASS; Terra. Caught 5+ real
  P1s across rounds (register-before, FIX-2 cross-adopt key delete, path-#1 sibling loss, codex stale-map).
- Bot (Codex Cloud) PASS on HEAD `7bf63331` ("no major issues", 0 inline, 0 open threads).
- My own: build/vet clean; api+clients tests pass (tagged); `-race -count=5..10` clean, 0 data races;
  every fix carries a load-bearing test proven non-vacuous (neuter → fails).
- Deploy: build v0.4.24 commit e1e3f029 → rename-aside + copy to C:\...\.local\bin → orphan-daemon trap
  fired (documented) → recovered via kill-all + `supervise --ensure-alive` → fleet 22 Running + 1 Stopped
  (weekly-refresh) on the new binary.

## Residual risk (tracked, out of scope)
- Codex whole-map read-modify-write cross-process TOCTOU (external non-lock-honoring writer between read
  and write) — PRE-EXISTING, architecturally non-owned (client configs have no cross-process lock), shared
  by all codex ops. Sol (acceptance) explicitly scoped it out. Filed:
  `work-items/backlog/2026-07-13-codex-whole-map-rmw-cross-process-race.md`.
- Routed-key GC-reclaim orphan → de-adopt (appended to `work-items/bugs/2026-07-12-adopt-preinstall-crash-orphan-triple.md`).

## Retrospective
- The commission (Sol carrying fable's adversarial lane after fable's budget ran out) earned its 7 rounds:
  the core P1 was closed in round 1, but each round surfaced a real, Sol-confirmed correctness edge in a
  genuinely subtle area (crash-consistency of client-config rollback across SecureWrite's two post-rename
  failure modes × entry-scoped restore × sibling preservation × cross-process races). Unlike #532 (an
  information-theoretic edge-mine), every finding here had a clean fix and the review converged.
- Lesson saved: rollback compensation must register BEFORE the mutation when the writer can fail-with-side-
  effect (atomic-rename returns error with the file already mutated). See
  `feedback_rollback_compensation_register_before_mutation`.

Archive location: work-items/archive/2026-07/2026-07-12-adopt-abort-preserve-provenance/
