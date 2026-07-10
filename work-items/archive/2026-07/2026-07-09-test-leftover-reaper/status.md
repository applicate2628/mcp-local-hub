# status - test-leftover reaper

Template: security-sensitive-adjacent preview implementation. Orchestrator: lead.
State: DELIVERED — v1 preview/diagnostics-only shipped. Destructive apply deferred to v2.

## Active agents / lanes

- None. V1 is delivered and this work-item is being archived.

## Completed agents / lanes

- Three adversarial security rounds converged the destructive live-kill findings from `9 (3×P1)` to `1×P1` to `0×P1`. Round 3 also confirmed the P2 respawn-loop ordering defect and the P3 recyclable-PPID ancestry defect. The complete record is `security-review.md`.
- `design.md` is authoritative for the preview/diagnostics-only v1. It lists the observed standalone `supervise` class, emits per-candidate evidence and hypothetical refusal labels, and contains no process-action path.
- `work-items/decisions/2026-07-10-test-leftover-reaper-preview-only-v1.md` records the accepted long-lived decision (`status: accepted`).
- **V1 shipped in PR #527, merged to master as `436e4f58` (Codex Cloud bot PASS, 2 rounds).** The implementation is `internal/api/test_leftover_preview.go` (evidence collection + classification, non-destructive) with `internal/api/test_leftover_preview_test.go` (vocabulary tests). No apply flag, confirm token, kill owner, tree-reap coordinator, or process-termination step exists in v1.
- Across implementation + two commissions + the two bot rounds the diagnostic/refusal vocabulary grew: new tokens `ambiguous-family-classification` and `protected-scope-unverified` (refusal reasons), `snapshot-unsupported-platform` (snapshot verdict), and `live-supervise` (pattern class). `design.md`'s enum/vocabulary tables were reconciled to the merged code const set on 2026-07-10 (this closure commit).
- Destructive apply is deferred to v2, blocked on round-3 P2 (respawn-loop ordering) + P3 (recyclable-PPID ancestry) plus a demonstrated value case for the safely automatable subset. The PEB reader, `envProofGate` kill authorization, confirm token, tree-reap, `{PID, StartedAt}` binding, apply age floor, audit, and kill-owner contracts remain preserved under the deferred design section.
- Existing default and aggressive cleanup behavior remains protected and unchanged.

## Next action

None for v1 — delivered and archived. The deferred v2 destructive apply remains an open follow-up (blocked, see closure.md and the decision record).

## Terms and Abbreviations

- **PEB:** Process Environment Block, deferred to v2.
- **PID / PPID:** process identifier / parent process identifier.
- **P1 / P2 / P3:** security-review severity levels.
- **V1 / V2:** preview/diagnostics-only version 1 / deferred destructive version 2.
