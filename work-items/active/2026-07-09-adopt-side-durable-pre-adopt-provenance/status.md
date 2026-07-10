# Status — adopt-side durable pre-adopt provenance

State: ADMITTED — awaiting architect design
Priority: medium
Milestone: v0.7 Adoption
Admitted: 2026-07-10 ($product-manager, see brief.md)
Blocks: 2026-07-09-deadopt-hub-to-native (its `Depends-on:` target)
Owner-to-be: internal/api/adopt.go (see research.md Q5)

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

## Next action
1. [DONE 2026-07-10] $product-manager: admitted for delivery — Priority medium,
   milestone v0.7 Adoption, no-epic. Roadmap decision package = `brief.md`.
2. [NEXT — active stage] $architect:
   a. Decision-registry entry: `adopted-entries.json` (new file) vs
      schema-extension of `managed-entries.json` (review.md:77-82 requires an
      accepted decision) — record in `work-items/decisions/`.
   b. Design the durable provenance schema + the fail-closed capture seam
      (adopt.go before line 218), per research.md Q4 consumer contract + Q5
      owner/seam, and design around the research-flagged known limits
      (brief.md "Next stage assignment").

## Notes
- Not on the critical path of the in-flight reaper v1 (PR #527); queued behind it.
- The de-adopt design + review already exist and pin the consumer contract, so the
  architect step is schema + capture-seam, not a from-scratch design.
