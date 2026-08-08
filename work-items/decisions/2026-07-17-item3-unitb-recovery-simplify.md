---
status: accepted
---

# Decision: Item 3 Unit B (GUI self-restart) uses a SIMPLE recovery model — auto-recover the common cases, degrade the rare crash-mid-handoff to an operator-visible state. Drop the fully-automatic CAS recovery graph.

Date: 2026-07-17
Decided by: a 3-voice consilium (codex-Sol + opus + fable, diverse model families) convened by $lead at
operator request ("взять рекомендованный консилиум-синтез вариант"). **Unanimous: B (simplify).**
Relates: `active/2026-07-16-productization-gui-solidify/item3-restart-design.md` (v2.2, the model being
simplified), `item3-restart-recon.md`, `2026-07-17-gui-server-port-authority-model.md`.

## Why this decision was forced
The v2.x design protected a CRASH during the sub-second same-port handoff window with a fully-automatic
recovery model: a 13-phase CAS record store, 43 legal transitions + 43 crash-probe rows, a split
pre/post-flock rollback, a child watchdog, and a parent recovery-claim. Across FIVE revisions the SAME
"no-owner permanent freeze" defect relocated THREE times (parent-death → parent-wedge →
recovery-claimant-death-mid-rollback), and the consilium named a FOURTH (a claim-expiry × in-flight-
`Terminate` OS race). Each fix closed one crash-mid-handoff link and the freeze moved to an adjacent one.

## The root cause the consilium named (the decisive finding)
The defect class is **record-write × OS-side-effect races, and a record CAS cannot serialize kills,
binds, and flocks** (fable). A `Terminate` is not a CAS; a sequence check cannot revoke an in-flight
kill. So the model was structurally trying to control OS effects with a record store — impossible — and
"one more rule closes the record and mints the next record↔OS boundary race" (fable, withdrawing her own
earlier "one more rule closes it"). The relocation history is the class itself telling us the model is
wrong, not the spec.

Two more converging arguments:
- **Fails harder than the event it protects** (opus + Sol): buggy auto-recovery ships as wrong kills,
  double owners, or a flock-holding freeze that blocks even the 10-second manual `mcphub gui` relaunch —
  strictly worse than the dead tab it recovers. A single-user, operator-present local dev tool with a
  one-command fallback does not warrant distributed-consensus-grade machinery for a rare window.
- **Defends a NON-invariant** (fable): the entire post-lock arbiter exists to sequence hub-listener
  release before the child's hub bind — but the design itself concedes (v2.2 lines 342-344) an
  unsequenced hub bind lands in the EXISTING degraded-hub-health state and recovers. Hub contention
  already has an owner. The subsystem where all three relocations lived defends something already owned;
  delete it, don't patch it. (Consistency: the hub listener's own hang is already manual-recovery by
  runbook — CLAUDE.md B1 — so giving the rarer crash-mid-restart STRONGER machinery is inverted
  protection.)
- The full model's own deep tail is manual ANYWAY (`recovery-failed`, 3-min absolute expiry,
  `expired`-suppression, autostart never triggering recovery, liveness no-ops while the supervisor
  lives). The debate is only how many fragile automatic layers sit above the same manual floor.

## The decision (the $lead synthesis of the three B-shapes)
Unit B keeps the architectural WINS and collapses the recovery apparatus.

**KEEP (real wins, each cheaply human-verifiable):**
1. The restartable **`GUIListenerOwner`** seam (fixes the monolithic `Server.Start`; §16-A's
   monolith-rejection stands).
2. **Authenticated confirm-then-release standby** — nonce + retained OS process handle. Defeats the v1
   circular-wait and PID-reuse spoofing. The child, on flock acquisition, activates IMMEDIATELY (no
   hub-release sequencing — lean on the existing hub degraded-health owner).
3. **Parent PRE-release rollback** — while the parent still holds the flock, on child bind/auth failure
   it retains the lease and rebinds P. Parent-owned, no durable protocol; covers the overwhelmingly
   likely real failures (child bind failure, port theft, bad binary).
4. The **reservation / Held mapping** — the ONE load-bearing CAS piece: a raw `reserved` (the healthy
   sub-second release→acquire gap) maps to Held so a third entrant or ensure-alive never fires into it.
5. **ONE** ensure-alive relaunch predicate — fresh valid record + provably-free flock (owned probe
   lease) + qualifying phase → relaunch once. Covers both-processes-dead, the only catastrophic
   interleaving.
6. **Unit A** (the fail-closed hub-port dependency guard) — already separate, planned, and independent.

**CUT (the machinery that mints the relocation class):**
- The post-lock parent/child arbiter, `ClaimRecovery`, the two-tier 10s/30s cutoff, the phase-suffix
  self-advance, the `hub-released` phase + activation-signal sequencing, and most of the 13-phase enum /
  43-transition graph.
- Rationale: once the parent has released the flock its job is done — no heroic post-release recovery —
  so the claimant-death relocation class STRUCTURALLY cannot exist (there is no claimant).

**DEGRADE (the rare crash-DURING-handoff):**
- A coarse durable marker `{in-progress, committed, interrupted}` + a §12 discriminator + an
  operator-visible message: **"GUI restart interrupted; run `mcphub gui`."** One recovery path a human
  verifies end-to-end. It fails LOUD, with the operator already at the keyboard.

## The one deciding tradeoff (all three voices)
Recovery machinery must fail SOFTER than the failure it recovers. The 43-edge graph inverts that: its
empirically non-trivial defect surface (three spec-level relocations under intense review, a named
fourth) fails as wrong kills and flock-holding freezes that block even manual restart, to protect a rare
sub-second-window crash whose deep tail is manual anyway. Degrade-to-visible collapses that to one
reviewable path and one failure mode the relocating defect has no surface to hide in.

## Consequence
`item3-restart-design.md` is superseded by a v3 (design-B) that rewrites Unit B's recovery per the
KEEP/CUT/DEGRADE above. The v2.2 CAS graph is retained in history for the reasoning trail but is NOT the
implementation target. Unit A (guard) is unaffected and ships first.

## C6 supersession note (2026-07-18)

Design v3.1 is the current contract where this accepted synthesis differs from
its earlier sketch. KEEP #5's "relaunch once" is superseded: ensure-alive is
degrade-only and never spawns a GUI. After the phase deadline it may classify
the exact nonterminal marker and publish either the plain `mcphub gui` guidance
for an owned Free flock or the identity-gated `mcphub gui --force --kill`
guidance for a Held flock; Unknown selects neither.

The three-phase marker `{in-progress, committed, interrupted}` is likewise
superseded by the v3.1 four-phase marker
`{in-progress, reserved, committed, interrupted}`. `reserved` is the
load-bearing healthy release-to-acquire gap and maps to Held for third entrants.
The live design and implementation must not be read as authorizing the earlier
ensure-alive relaunch path.
