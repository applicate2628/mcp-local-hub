# Canonical brief

Primary task: finish the current six-PR bot-review wave end to end.

User goal: working behavior first, no regressions, no endless fix-of-fix loops, clean Git and worktrees afterward.

Scope:

- PR #590 current P1 resolver snapshot race.
- PR #591 six current correctness/functionality findings.
- PR #592 current bare-launch routing coverage gap.
- Current-head bot re-review for #583, #588, and #589 after their master integration merges.
- Publication, merge, and cleanup once functional gates pass.

Out of scope: old-head/outdated review threads, opportunistic refactors, unrelated backlog items, and broad worktree deletion without byte/process/reachability proof.

Integration owner: main conversation (Lead).

Acceptance criteria:

- Every current finding has verified disposition.
- Confirmed defects have a failing-before/passing-after regression.
- Focused normal and race tests plus relevant vet pass.
- Exact publication delta is clean.
- GitHub reports current heads mergeable and bot-clean before merge.
- Root and candidate worktrees are reconciled after delivery.
