# Plan: G17 Windows Secure Parent Create

Date: 2026-06-18
Owner: main
Status: implemented

## Scope

Fix the Windows G17 secure client-config parent-directory creator so each descendant component is created or opened relative to the currently held verified parent handle, not by an absolute path re-walk. Preserve home containment, outside-home refusal, default relax behavior, POSIX behavior, and the GUI server surface.

## Steps

1. Inspect existing Windows secure-create helpers and tests.
2. Add Windows regression coverage for a verified-component junction swap and strict existing-prefix DACL refusal.
3. Replace the Windows absolute `CreateDirectoryW` descent with `NtCreateFile` relative to the held parent handle.
4. Verify each existing-or-created Windows prefix handle for reparse/non-directory status, and verify DACLs in strict mode before that handle becomes the next root.
5. Run focused security tests, cross-OS builds, `go vet`, gofmt, and language-server diagnostics when available.

## Validation

- Red test before fix: `go test -tags=test_state_path_env ./internal/api -run 'SecureCreateClientConfigParentDir' -count=1` failed on the new junction-swap and strict-prefix DACL tests.
- Final focused test with isolated state: passed.
- `go build ./...`: passed on Windows.
- `GOOS=linux go build ./...`: passed.
- `GOOS=darwin go build ./...`: passed.
- `go vet ./internal/api/`: passed.
- Go language-server MCP diagnostics: unavailable; proxy on `127.0.0.1:9200` refused connection.

Execution role: main
Assigned / replaced internal role: none
Requested provider: none
Resolved provider: none
Actual execution path: direct Codex implementation
Model / profile used: unspecified by runtime
Deviation reason: none
