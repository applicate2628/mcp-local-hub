# Status — adopt-side durable pre-adopt provenance

State: DESIGN REVISED — ready for planning. Arch review PASS + security review REVISE folded in (6 MUST-FIX); store-shape decision PROMOTED proposed→accepted by the arch-review gate (decision-file frontmatter reconciliation pending — reviewer/archivist owned)
Priority: medium
Milestone: v0.7 Adoption
Admitted: 2026-07-10 ($product-manager, see brief.md)
Designed: 2026-07-10 ($architect, see design.md — PASS)
Reviewed: 2026-07-10 (architecture PASS; security REVISE, 0 P0/P1) → revised same day (design.md "Revision" section)
Blocks: 2026-07-09-deadopt-hub-to-native (its `Depends-on:` target)
Owner-to-be: internal/api/adopt.go + new internal/api/adopted_entries.go (see design.md)

## What this is
The de-adopt work-item (`2026-07-09-deadopt-hub-to-native`) is BLOCKED
(review verdict REVISE/BLOCKED) on durable pre-adopt provenance: an operator who
adopts an unmanaged stdio MCP server into hub management must be able to de-adopt
it back to its exact prior client-config state. Today (evidence in research.md)
the pre-adopt entry is lost at the `AddEntry` overwrite (`internal/api/install.go:2689`)
— only an unlabelled, prunable, whole-file backup survives, neither pinned nor
adopt-scoped. This item captures durable, adopt-scoped, per-entry provenance
BEFORE the config rewrite commits.

## Artifacts
- `research.md` — $analyst read-only memo (PASS). Adopt code path, exact loss
  point, reuse-candidate ranking, owner+seam identification, admission gates.
- `brief.md` — $product-manager admission / roadmap decision package (PASS).
- `design.md` — $architect design package (PASS, revised 2026-07-10): store shape,
  provenance schema (every consumer field mapped), pinned-artifact mechanism,
  fail-closed capture seam (adopt.go before :218), orphan lifecycle + upsert + GC,
  three known-limits handling, API-contract sketch, security-by-design,
  consumer-contract handoff, observability, test strategy, 15 numbered claims.
  See its "Revision (2026-07-10)" section for the review fold-in.
- `work-items/decisions/2026-07-10-adopt-provenance-store-shape.md` — the store-shape
  decision, PROMOTED proposed→accepted by the $architecture-reviewer gate
  (decision-file frontmatter reconciliation pending — reviewer/archivist owned).

## Next action
1. [DONE 2026-07-10] $product-manager: admitted for delivery — Priority medium,
   milestone v0.7 Adoption, no-epic. Roadmap decision package = `brief.md`.
2. [DONE 2026-07-10] $architect: decision-registry entry (new
   `adopted-entries.json`, now ACCEPTED) + `design.md` (schema + fail-closed
   capture seam + known-limits handling). Gate: PASS.
3. [DONE 2026-07-10] Review: $architecture-reviewer PASS (store-shape promoted
   proposed→accepted); $security-reviewer REVISE (0 P0/P1, core posture verified).
   Both converged on the orphan-lifecycle gap. Design REVISED same day folding in
   6 MUST-FIX (F1 hash-timing, F2/P2-2 upsert+GC, P2-1 fail-closed snapshot gate,
   F3 drop expected_hub_shape, F4 fail-closed classify, F5 whole-file-hash honesty).
4. [NEXT] $planner breaks design.md into delivery phases, respecting the F7 scope
   boundary (no stub bodies for de-adopt-owned mutators; `ReadAdoptProvenance` IS
   in-scope). SHOULD-TRACK follow-ups (P3-1 minimal-entry snapshot, P3-2
   shared-key scan, managed-entries tuple-recording scope choice) are in design.md
   "Follow-ups & planning notes".

## Notes
- Not on the critical path of the in-flight reaper v1 (PR #527); queued behind it.
- The de-adopt design + review already exist and pin the consumer contract, so the
  architect step is schema + capture-seam, not a from-scratch design.
