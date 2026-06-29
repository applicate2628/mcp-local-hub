# Plan: Test Infra Fleet Isolation

Created: 2026-06-29 18:39

Execution role
Assigned / replaced internal role: none
Requested provider: none
Resolved provider: none
Actual execution path: main Codex session
Model / profile used: unspecified by runtime
Deviation reason: none

## Scope

Make the named pre-existing Go test failures independent of host Task Scheduler state, live mcphub fleet state, Windows TCP port exclusions, and unsafe temporary profile roots. Do not run `go test ./...`, do not sweep mcphub processes, and do not address the CLI table-truncation or LSP port-pool product findings.

## Steps

- Completed: fetched origin and inspected the backlog item plus target tests.
- Completed: investigated restart sibling failures against the restart path, fake scheduler setup, and host port exclusion evidence.
- Completed: investigated mixed canonical/legacy workspace-key fixture behavior and confirmed the compatibility path remains live.
- Completed: investigated E2E lazy-register state-dir resolution and identified the existing test-only state-root seam.
- Completed: implemented scoped test-infra fixes.
- Completed: ran targeted tests, build, vet, cross-builds, and publication-safety scan.

## Acceptance Checks

- `go test -count=1 -run 'TestRestart_Server|TestUnregister_Mixed|TestWorkspaceUnregister_Backend|TestE2E_LazyRegister' ./internal/api/ ./internal/cli/ ./internal/e2e/`
- `go build ./...`
- `go vet ./...`
- `GOOS=linux go build ./...`
- `GOOS=darwin go build ./...`
- Git Bash publication-safety wrapper
