# Plan Snapshot: mcphub repair-state-dacl scope cut

1. Inspect repair command, API repair flow, docs, and existing tests. Status: completed.
2. Add/adjust failing tests for path-only, secure audit behavior, and Linux chmod fallback. Status: completed.
3. Implement scope cut, audit hardening, and chmod fallback. Status: completed.
4. Run requested targeted tests, builds, vet, cross-builds, and publication-safety scan. Status: completed for Windows/local build surfaces; Linux fallback test compiled cross-OS.
5. Commit, push branch, and report HEAD/evidence. Status: pending at snapshot time.

Verification evidence captured in session:

| Check | Result |
|---|---|
| `go test -count=1 -run 'RepairStateDACL|RepairStateFileDACL' ./internal/cli/ ./internal/api/` | PASS |
| `go build ./...` | PASS |
| `go vet ./...` | PASS |
| `GOOS=linux go build ./...` | PASS |
| `GOOS=darwin go build ./...` | PASS |
| `GOOS=linux go test -c -o .scratch/testbins/internal-api-linux.test ./internal/api/` | PASS |
| Git Bash `check-publication-safety.sh` | PASS |
