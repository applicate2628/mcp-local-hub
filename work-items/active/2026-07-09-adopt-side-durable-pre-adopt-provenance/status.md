# Status — adopt-side durable pre-adopt provenance

State: DESIGN COMPLETE — design.md accepted by $architect (PASS); awaiting review (architecture + security) then plan
Priority: medium
Milestone: v0.7 Adoption
Admitted: 2026-07-10 ($product-manager, see brief.md)
Designed: 2026-07-10 ($architect, see design.md — PASS)
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
- `design.md` — $architect design package (PASS): store shape, provenance schema
  (every consumer field mapped), pinned-artifact mechanism, fail-closed capture
  seam (adopt.go before :218), three known-limits handling, API-contract sketch,
  security-by-design, observability, test strategy, 10 numbered claims.
- `work-items/decisions/2026-07-10-adopt-provenance-store-shape.md` — the required
  store-shape decision (status `proposed`; `$architecture-reviewer` gate promotes).

## Next action
1. [DONE 2026-07-10] $product-manager: admitted for delivery — Priority medium,
   milestone v0.7 Adoption, no-epic. Roadmap decision package = `brief.md`.
2. [DONE 2026-07-10] $architect: decision-registry entry (new
   `adopted-entries.json`) + `design.md` (schema + fail-closed capture seam +
   known-limits handling). Gate: PASS.
3. [NEXT] Review — $architecture-reviewer (maps design.md claims 1:1; promotes the
   store-shape decision `proposed → accepted`) + $security-reviewer (snapshot is
   secret-bearing; confirm the hardened-DACL posture + the whole-file
   over-collection residual). Then $planner breaks design.md into delivery phases.
   Open findings for the user are in design.md "Findings for the user".

## Notes
- Not on the critical path of the in-flight reaper v1 (PR #527); queued behind it.
- The de-adopt design + review already exist and pin the consumer contract, so the
  architect step is schema + capture-seam, not a from-scratch design.
