# status - test-leftover reaper

Template: security-sensitive-adjacent preview implementation. Orchestrator: lead.
State: V1 SCOPED TO PREVIEW-ONLY (design accepted).

## Active agents / lanes

- None. Documentation re-scope is complete; implementation has not started.

## Completed agents / lanes

- Three adversarial security rounds converged the destructive live-kill findings from `9 (3×P1)` to `1×P1` to `0×P1`. Round 3 also confirmed the P2 respawn-loop ordering defect and the P3 recyclable-PPID ancestry defect. The complete record is `security-review.md`.
- `design.md` is now authoritative for a preview/diagnostics-only v1. It lists the observed standalone `supervise` class, emits per-candidate evidence and hypothetical refusal labels, and contains no process-action path.
- `work-items/decisions/2026-07-10-test-leftover-reaper-preview-only-v1.md` records the accepted long-lived decision.
- Destructive apply is deferred to v2, blocked on round-3 P2/P3 plus a demonstrated value case for the safely automatable subset. The PEB reader, `envProofGate` kill authorization, confirm token, tree-reap, `{PID, StartedAt}` binding, apply age floor, audit, and kill-owner contracts remain preserved under the deferred design section.
- Existing default and aggressive cleanup behavior remains protected and must not be weakened.

## Next action

Implement the preview/diagnostics lane in an isolated worktree. Treat it as security-sensitive-adjacent but non-destructive: read process metadata and on-disk buildinfo; use strict path canonicalization for classification display; do not require cross-process memory reads in baseline v1 (a cheap, bounded, read-only diagnostic provider requires separate acceptance); and never terminate a process. Keep existing default and aggressive cleanup unchanged.

## Terms and Abbreviations

- **PEB:** Process Environment Block, deferred to v2.
- **PID / PPID:** process identifier / parent process identifier.
- **P1 / P2 / P3:** security-review severity levels.
- **V1 / V2:** preview/diagnostics-only version 1 / deferred destructive version 2.
