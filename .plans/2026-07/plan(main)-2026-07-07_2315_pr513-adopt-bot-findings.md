# Plan: PR #513 adopt bot findings

Execution role: main conversation
Assigned / replaced internal role: backend-engineer
Requested provider: none
Resolved provider: none
Actual execution path: main Codex session
Model / profile used: unspecified by runtime
Deviation reason: none

## Scope

Fix the six P2 adopt findings on branch `feat/mcphub-adopt`, leaving changes uncommitted.

## Steps

1. Inspect adopt, secret, port, and client-repoint seams; capture hypotheses and existing tests.
2. Add failing tests for F1-F6 and run targeted red checks.
3. Implement scoped fixes at the owning seams.
4. Run requested verification: Windows, Linux, and Darwin `go build ./...`; `go vet ./internal/api/... ./internal/cli/...`; targeted race tests; `git diff --check`.
5. Write the required session report and summarize uncommitted changes.
