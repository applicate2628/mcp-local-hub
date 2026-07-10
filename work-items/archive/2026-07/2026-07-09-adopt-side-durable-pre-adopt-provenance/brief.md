# Brief — adopt-side durable pre-adopt provenance

Role: $product-manager (roadmap decision package). Admitted 2026-07-10.
Gate decision: **PASS (ADMIT for delivery).**

## Admission decision

**ADMIT into delivery.** Priority: **medium**. Milestone: **v0.7 Adoption**
(same milestone family as `2026-07-05-adopt-npx-orphans` / ROADMAP §9 Adoption).
No-epic (rationale below).

Single next stage: **$architect** — design the durable provenance schema + the
fail-closed capture seam. This item is a prerequisite, not a from-scratch design;
research + the downstream consumer contract are already pinned.

## Why now (rationale — evidence)

- **Sole blocker of an admitted downstream lane.** De-adopt
  (`2026-07-09-deadopt-hub-to-native`) is verdict REVISE/BLOCKED
  (`deadopt/status.md:4`) and declares `Depends-on:
  2026-07-09-adopt-side-durable-pre-adopt-provenance` (`deadopt/status.md:5`).
  The de-adopt review names this exact item as the one to admit next: "Future
  prerequisite item to admit: adopt-side durable pre-adopt provenance … has not
  been admitted yet" (`deadopt/review.md:93-97`), and lists it as prerequisites 1
  and 2 (`deadopt/review.md:86-87`). Nothing in the de-adopt lane can proceed
  until this lands.
- **Research is PASS-complete and low-ambiguity.** `research.md` (PASS,
  `research.md:216`) identifies the owner (`internal/api/adopt.go`), the exact
  overwrite loss point (`internal/api/install.go:2689`), the fail-closed capture
  insertion point (`ExecuteAdoptWithOpts`, before `internal/api/adopt.go:218`),
  the reuse template (`managed_entries.go` hardened-state storage + the backup
  lane's restore mechanics), and the already-written consumer contract
  (`deadopt/design.md:69-77`, `deadopt/review.md:86-91`). The architect stage is
  schema + capture-seam, not open-ended.
- **Roadmap fit / no competing priority.** Phase-1 adopt is SHIPPED + DEPLOYED
  (`adopt-npx-orphans/status.md` — CLI/API + GUI + reaper all live). De-adopt is
  the remaining reversibility half of that initiative: adopt-without-de-adopt is a
  one-way door in a shipped, live feature — a real product-completeness/trust gap.
  The ROADMAP §B backlog is at its floor ("ZERO agent-actionable items remaining",
  ROADMAP.md final-accounting block), so there is no higher-priority actionable
  coding work this displaces.
- **Prior gate cleared.** The item's own `status.md` queued it behind reaper v1
  (PR #527); that PR is now merged + archived (`index.md:56`,
  `archive/2026-07/2026-07-09-test-leftover-reaper/`), so the hold is released.

**Priority = medium (scheduling urgency, not impact).** Not high — no production
defect; adopt itself works; de-adopt is a completeness feature, not an outage fix.
Not low — it gates an entire admitted downstream lane whose adversarial review is
explicitly waiting on it, with no competing higher-priority work.

## Admitted scope (bounded)

Capture durable, adopt-scoped, per-entry provenance for every adopt operation
**before** the client-config rewrite commits at `install.go:2689`, sufficient for a
later process to reconstruct the pre-adopt native client state. Per the consumer
contract (`deadopt/design.md:69-77`, `deadopt/review.md:86`), one row per
adopt-created manifest carrying:

- `manifest_name`, source entry name, source client, selected clients, selected port;
- adopt-generated manifest hash + current expected hash (hash-gated manifest edit/delete);
- per-client original state: `present` with a **pinned / non-prunable** restore
  artifact (or serialized adapter snapshot), or `absent` for explicit-fanout
  clients with no pre-adopt entry;
- per-client original config-shape hash + expected hub-managed live shape;
- adopt-created routed secret keys;
- operation state `adopting → adopted → de_adopting → closed`, the `adopting`/pending
  row written **before the first irreversible adopt mutation** (before
  `adopt.go:218`) and flipped to `adopted` only after `Install` returns success,
  folded into the existing adopt failure-cleanup (`adopt.go:218-248`).

## Non-goals (explicitly out of this item)

- The de-adopt reverse operation itself (that is `2026-07-09-deadopt-hub-to-native`
  — this item only *produces* the provenance it consumes).
- De-adopt `/g/` group-route semantics, the de-adopt lock graph, and manifest
  delete-race protection — those are de-adopt prerequisites 3-5
  (`deadopt/review.md:88-90`), not adopt-side capture.
- Any change to the adopt user-facing behavior beyond the additive pre-`Install`
  capture step (capture is additive; a capture-step failure must fail closed and
  reuse the existing adopt cleanup — see acceptance below).

## Dependency edge (explicit)

`2026-07-09-deadopt-hub-to-native` **Depends-on** this item
(`deadopt/status.md:5`). De-adopt implementation must not start until this
prerequisite is delivered (`deadopt/status.md:27`). This item does not depend on
de-adopt; it is upstream.

**No-epic rationale.** The lane is exactly two work-items (this prereq + de-adopt)
joined by a single hard `Depends-on` edge, which already models the sequencing and
blocking relationship derivably (`/agents-status` computes blocked-by from it). An
epic groups several items toward a milestone and would add a `work-items/epics/*`
file + `Epic:` lines on each child; for a two-item chain already bound by one
dependency edge that is ceremony without added coordination value, and it would
touch the de-adopt item beyond this admission's sanctioned change surface. If the
lane grows (e.g. the de-adopt D P2a/P2b GUI surfaces become their own items),
admit an epic then.

## Next stage assignment — $architect

1. **Decision-registry entry first.** New `<state-dir>/adopted-entries.json` vs a
   schema-compatible extension of `managed-entries.json`. `deadopt/review.md:77-82`
   requires an *accepted decision* before coding; record it in
   `work-items/decisions/`.
2. **Provenance schema** covering every field in Admitted scope above, on the
   hardened state-file pattern (`state_file_helper.go` `WriteStateFileAtomic` +
   the flock + schema-version model of `managed_entries.go:99-167`). Cross-file
   consistency (provenance write + manifest create + install + secret persist span
   multiple files) must serialize at the adopt-owner level
   (`state_file_helper.go:68-71`).
3. **Fail-closed capture seam** in `ExecuteAdoptWithOpts` before `adopt.go:218`,
   with the pinned/non-prunable restore artifact and present/absent classification,
   folded into the existing failure-cleanup at `adopt.go:218-248`.

Design around these research-flagged known limits (not blocking; design must
account for them — `research.md:193-198`): (a) per-adapter byte-equivalent restore
is ASSUMPTION (UNVERIFIED) (`deadopt/design.md:79`) → schema needs a
`functional-equivalent` restore mode + a per-adapter probe before any user-facing
byte-equivalence promise; (b) sensitive-env literal spelling is unrecoverable after
`secret:` routing; (c) the generic backup lane is prunable and its `-original`
sentinel is not adopt-scoped → the restore artifact must be provenance-owned and
pinned, not an ordinary timestamped backup (`deadopt/review.md:87`).

## Acceptance framing (for the eventual QA gate — not this stage)

- **Facts (verified in research):** the pre-adopt entry's only durable trace today
  is an unlabelled, prunable, whole-file backup that adopt neither pins nor records
  (`research.md:80-83`); adopt's Install path does not record `managed-entries.json`
  (`research.md:75-78`).
- **Assumptions to resolve in design/QA:** per-adapter byte-equivalence
  (`design.md:79`); whether adopt should also write `managed-entries.json` tuples
  (`research.md:210`).
- **Judgment (PM):** medium priority + v0.7 milestone + no-epic, as argued above.
- **Falsification target (research Q, carried to QA):** seed one stdio entry →
  adopt → drop in-memory state (new process/reload) → de-adopt using **only** the
  persisted provenance file → assert the restored client entry equals the pre-adopt
  snapshot (`deadopt/review.md:103-104`, T1); plus T5 (churn backups past a low
  `keep_n`, then restore) to falsify the prunability limit (`research.md:202-203`).
  A capture-step failure must abort the adopt fail-closed without regressing a
  currently-successful adopt (`research.md:187-189`).

## Artifacts

- `research.md` — $analyst read-only memo (PASS).
- `brief.md` — this admission / roadmap decision package.
- `status.md` — lifecycle state (ADMITTED).
