# Daemon recovery round-4 repair plan

Roles: backend-engineer, security-engineer

1. Correct recovery outcomes and destructive-boundary sequencing in `internal/daemonrecovery`.
2. Add hermetic regressions for already-exited audit truth, the shared termination-attempt budget, the boundary state re-read, and Windows handle closure.
3. Reconcile the CLI/CLAUDE exit tables and the design/backlog ownership and lease reasoning.
4. Run the user-approved build, vet, package tests, and focused hermetic CLI exit-contract test.

Constraints: no commit; no external/model helpers; no graph/codegraph; no held-generation redesign; preserve unrelated worktree changes.
