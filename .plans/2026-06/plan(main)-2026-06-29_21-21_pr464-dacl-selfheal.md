# PR 464 cold-read DACL self-heal fixes

## Scope

Fix four security findings in the Windows cold-read DACL self-heal path:

1. Refuse secret-bearing state files before any DACL mutation.
2. Self-heal only on a proven non-allowlisted write/admin allowlist violation.
3. Do not emit the read-relax fallback audit after a successful self-heal.
4. Always emit the DACL self-heal mutation audit, including no-audit read callers.

## Plan

1. Fetch and verify branch/worktree state.
2. Inspect `internal/api/hub_mcp_state_read_inode_windows.go`, the DACL verifier, and existing Windows DACL tests.
3. Add RED regression tests for secret no-heal/retry refusal, unproven verifier failure no-heal, no-audit mutation audit, and no fallback audit after heal.
4. Patch the Windows file-DACL branch with the smallest owner-boundary change.
5. Run focused tests, build/vet, Linux/Darwin builds, publication-safety scan, and scoped security review.
6. Commit and push `feat/cold-read-dacl-selfheal`.

## Acceptance Checks

- `go test -count=1 -run 'InodeAnchored|StateRead|DACL|SelfHeal|Registry|Secret' ./internal/api/`
- `go build ./...`
- `go vet ./...`
- `GOOS=linux go build ./...`
- `GOOS=darwin go build ./...`
- `~/.codex/skills/lead/scripts/check-publication-safety.sh`
