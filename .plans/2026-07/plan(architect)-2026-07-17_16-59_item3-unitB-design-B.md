# Plan: Item 3 Unit B design-B recovery simplification

Date: 2026-07-17
Role: `$architect`
Scope: `work-items/active/2026-07-16-productization-gui-solidify/item3-restart-design.md`

1. Read the accepted recovery-simplification decision, current design, work-item status, and current seam evidence.
2. Treat the unanimous accepted decision as the completed alternatives and approval gate; no new design choice is opened.
3. Rewrite the design around the minimal coarse marker, reservation/Held mapping, one ensure-alive relaunch predicate, immediate child activation, and pre-release-only parent rollback.
4. Remove all stale post-release arbiter, claim, self-advance, activation-signal, and 13-phase/43-edge recovery references.
5. Self-review the resulting design, verify a documentation-only diff, and record the session outcome.

No implementation, test execution, commit, external reviewer, or subagent is in scope.
