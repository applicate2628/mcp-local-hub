# Daemon Recovery Round 10 Plan Snapshot

Date: 2026-07-17
Roles: backend-engineer, security-engineer
Status: completed

## Objective

Apply only the eleven user-approved final-round corrections to daemon recovery, preserve the existing
held-generation classifier and commit-to-respawn design, regenerate the embedded GUI bundle, and leave
the worktree uncommitted.

## Scope and constraints

- Recovery operation and audit durability: `internal/daemonrecovery/` and the existing
  `internal/api/supervisor_events.go` timeout primitive.
- Thin CLI/GUI adapters, focused tests, Dashboard copy, canonical `CLAUDE.md`, and the requested backlog
  follow-ups.
- No external reviewer, model process, graph tooling, helper process, commit, or untagged CLI/API test.
- Preserve all unrelated pre-existing worktree changes.

## Completed steps

1. Confirmed branch, head, live repository status, owning code paths, and existing test seams.
2. Added the bounded pre-respawn mutation audit plus queued fallback without consuming the respawn
   reservation; queued the already-exited audit; corrected result/error honesty and bounded fields.
3. Updated CLI, GUI, TypeScript copy, canonical documentation, and deferred follow-ups.
4. Added focused regressions for bounded/fast audit ordering, remaining blocking return path, raw error
   classification, zero-budget observation honesty, partial reaped result, and GUI messages.
5. Ran the full requested build, vet, Go, tagged CLI, frontend, and generation commands successfully.
6. Rebuilt `internal/gui/assets/` and rechecked the final build, diff whitespace, branch, and unchanged
   head.

## Acceptance evidence

- `go build ./...`
- `go vet ./...`
- `go test -count=1 ./internal/process/ ./internal/daemonrecovery/ ./internal/gui/`
- `go test -tags=test_state_path_env -count=1 -timeout 12m ./internal/cli/`
- `npm run typecheck && npm run test` in `internal/gui/frontend`
- `go generate ./internal/gui/...`

## Terms and Abbreviations

- `CLI`: Command-Line Interface.
- `GUI`: Graphical User Interface.
- `IPC`: Inter-Process Communication.
