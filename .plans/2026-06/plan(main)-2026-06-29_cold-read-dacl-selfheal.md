# Plan Snapshot: cold-read DACL self-heal

1. Add failing Windows tests for owner-owned stale broad DACL self-heal and foreign-owner no-heal.
2. Implement handle-bound owner verification via `GetSecurityInfo(OWNER_SECURITY_INFORMATION)`.
3. Tighten only current-user-owned files with the hardened writer's protected owner-only DACL.
4. Re-verify with the existing DACL verifier and continue the handle-bound read.
5. Verify with targeted `internal/api` tests, `go build`, `go vet`, Linux/Darwin builds, and publication-safety scan.

Scope exclusions: POSIX read path, write path, symlink/reparse/irregular-file refusals, parent-DACL refusals, strict-mode relax.
