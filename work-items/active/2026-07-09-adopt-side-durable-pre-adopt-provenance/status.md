# Status — adopt-side durable pre-adopt provenance

State: IMPLEMENTATION COMPLETE — all 5 code+docs phases (A storage → B capture/lifecycle → C fail-closed seam → D orphan GC → E docs) delivered on branch `feat/adopt-provenance`. Both post-implementation review gates PASS (architecture PASS; security PASS after the P2 fail-closed-present-at-Build fix). **Codex bot r2 review surfaced a crash/concurrency edge-cluster** on the round-1 (HEAD `45073703`) point-fixes — resolved by a coherent $architect model (2026-07-10): see design.md "## Crash-consistency + concurrency model (bot r2 resolution)" + decision `work-items/decisions/2026-07-10-adopt-provenance-crash-consistency-model.md` (`status: proposed`). That model is DESIGN-ONLY; $planner must fold it into the plan and $backend-engineer re-implement the r2 delta before the PR loop. Next = re-plan r2 delta → implement → QA + PR + bot loop + merge. The 2 flagged-optional phases (F1 tuple-recording, F2 supervise-GC) remain DEFERRED (lead decisions below).
Priority: medium
Milestone: v0.7 Adoption
Admitted: 2026-07-10 ($product-manager, see brief.md)
Designed: 2026-07-10 ($architect, see design.md — PASS)
Planned: 2026-07-10 ($planner, see plan.md — PASS; 5 phases A-E + F1/F2 flagged-optional)
Reviewed: 2026-07-10 (design gate: architecture PASS; security REVISE, 0 P0/P1) → revised same day (design.md "Revision" section)
Implemented: 2026-07-10 ($backend-engineer, branch `feat/adopt-provenance`) — A `69b24b89`, B (same commit), C `a6891320`, P2 fix `818985f2`, D `5adfd0c4`, E (this change). Post-impl review: architecture PASS on A+B+C; security REVISE→PASS (one P2, present-at-Build capture must fail closed, fixed in `818985f2`).
Blocks: 2026-07-09-deadopt-hub-to-native (its `Depends-on:` target)
Owner: internal/api/adopt.go + new internal/api/adopted_entries.go + adopt_provenance_events.go (+ tests); CLAUDE.md "Adopt provenance (v0.7)" section (Phase E)

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
4. [DONE 2026-07-10] $planner broke design.md into delivery phases (`plan.md`, PASS),
   respecting the F7 scope boundary (no stub bodies for de-adopt-owned mutators;
   `ReadAdoptProvenance` IS in-scope). Phases A (storage) → B (capture/lifecycle) →
   C (fail-closed seam) → D (orphan GC) → E (docs), serialized. Two flagged-optional
   phases (F1 managed-entries tuple-recording, F2 supervise-startup GC hook) DEFER by
   default — orchestrator decision required before implementing.
5. [DONE 2026-07-10] $backend-engineer implemented Phases A-E in the isolated
   worktree `feat/adopt-provenance` (TDD, one reviewable commit per phase).
   $security-reviewer gated A+B+C (snapshot DACL + fail-closed ordering + the P2
   present-at-Build fix); $architecture-reviewer gated the hot-path seam (protected
   surfaces untouched). All AC-ids (A1-A8, B1-B8, C1-C7, D1-D4, E1-E3) covered by
   named hermetic tests; build + vet (win + linux) + scoped `go test` green.
6. [NEXT] $qa-engineer full gate against all AC-ids, then the repo PR workflow
   (pre-push `go build && go vet && go test`; push; Codex bot PASS loop; deep-security
   pass; merge). See CLAUDE.md "PR review + merge workflow (MANDATORY)".

## Notes
- Not on the critical path of the in-flight reaper v1 (PR #527); queued behind it.
- The de-adopt design + review already exist and pin the consumer contract, so the
  architect step is schema + capture-seam, not a from-scratch design.

## Lead decisions on planner-flagged ambiguities (2026-07-10)
- Ambiguity 1 (three abort sites): resolved by planner — Phase C asserts all 3 (`persistAdoptRoutedSecrets` err-path `:218-220` becomes the 3rd). No further action.
- Ambiguity 2 (F1 managed-entries tuple-recording): **DEFERRED** — optional, not required for de-adopt correctness (arch-reviewer + planner concur). NOT in v1 scope.
- Ambiguity 3 (F2 `mcphub supervise` startup GC hook): **DEFERRED to follow-up** — touches `runSupervise`, outside the admitted adopt-Execute blast radius; per-adopt GC (Phase D) already bounds the residual. Follow-up work-item if a fleet-wide sweep is later wanted.
- Ambiguity 4 (capture atomicity + GC guard): implementer confirms; adopt-v1 disallows concurrent same-manifest (`adopt.go:148-152`), escalate-not-invent if a real window appears. **UPDATE (bot r2, 2026-07-10):** the real window DID appear — a GUI/CLI double-submit builds two plans before either commits, so capture-time concurrency is reachable despite the manifest-exists gate. Resolved by the r2 crash-consistency model (per-manifest lease + hub-binding committed signal + row-first anchor), design.md addendum + the new decision file.
