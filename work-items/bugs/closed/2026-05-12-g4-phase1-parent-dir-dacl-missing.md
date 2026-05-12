---
title: G4 Phase 1 SecureWriteClientConfig Windows leg + VerifyHubMcpStateDACL skip parent-dir DACL verify
severity: high
found-by: qa-engineer
found-in-phase: G4 Phase 1 — Pre-gate + Write Hardening
affected-surface: internal/api/secure_write_windows.go, internal/api/hub_mcp_state_dacl_windows.go
context: feat/g4-phase1-pre-gate (independent spec-compliance review of branch HEAD 0f0efb9)
status: closed
fixed-in: feat/g4-phase1-pre-gate (see Resolution section below)
---

## Reproduction

There is no failing test; the gap is non-coverage of a spec-required security step. Visible by inspection:

1. Read `internal/api/secure_write_windows.go` `secureWriteClientConfigImpl` (lines 80-191).
2. Confirm: after `openDirHandleNoReparse(parentDir)` the impl proceeds directly to `refusePreexistingReparsePoint`, `ntCreateRelative`, etc. — it does NOT call `verifyWindowsDACLFromHandle(dirHandle)`.
3. Read `internal/api/hub_mcp_state_dacl_windows.go` `verifyHubMcpStateDACLImpl` (lines 37-65).
4. Confirm: after `CreateFile(path, ...)` the impl calls `verifyWindowsDACLFromHandle(h)` for the file only — it never verifies the parent dir's DACL.

Compare against POSIX leg `internal/api/secure_write_posix.go` lines 56-58: POSIX correctly calls `verifyPosixParentDirFromFd(dirFd)` between dir-open and temp-file create. The Windows leg has no equivalent.

## Expected vs actual

**Expected (per spec):**
- `docs/superpowers/specs/2026-05-12-g4-unified-hub-mcp-design-v3.md` §"SecureWriteClientConfig sequence" step 3 (lines 323-326): "verify dirHandle DACL (handle-bound): owner == current-user, only {current-user, LocalSystem, BuiltinAdministrators} allowlist. On failure: reject with '<parent-dir>: DACL not single-user safe'."
- Spec lines 277-280 (state-file load-time): explicit `verifyWindowsParentDACL(filepath.Dir(path))` step alongside the file's own DACL verify.
- Plan Task 1.4 step pseudocode at `docs/superpowers/plans/2026-05-12-g4-unified-hub-mcp.md` lines 511-514: `verifyWindowsDACLFromHandle(dirHandle)` as comment "// 2. Handle-bound DACL verify on the parent dir."

**Actual:**
- `secureWriteClientConfigImpl` (Windows): elides step 3 entirely. The file header step list (lines 5-39) skips from step 1 directly to step 3 ("crypto/rand 8-byte hex tempName") with no parent DACL verify. The doc comment claims "Ancestor chain is covered by the per-user trust boundary" — but the spec at line 425 is explicit that the trust boundary covers ANCESTOR-OF-ANCESTOR chain, not the immediate parent. Spec line 432: "The per-target parent-dir DACL check at step 3 is the same allowlist-based gate."
- `verifyHubMcpStateDACLImpl` (Windows): never opens or verifies the parent dir.

**Impact:**
- A Group-Policy / MDM-pushed inherited DACL on the immediate parent dir (e.g., `%USERPROFILE%` for `~/.claude.json` writes) granting read to `Domain Users` / `Authenticated Users` / corporate management SID is not caught at write-time.
- The post-rename re-verify on the FILE catches the case where inheritance propagated to the file itself, but `PROTECTED_DACL_SECURITY_INFORMATION` set at step 4 prevents inheritance from re-broadening — so the file passes the re-verify even though the parent dir is broadly readable / writable by other SIDs.
- For `VerifyHubMcpStateDACL`: the spec explicitly requires both the file's DACL AND the parent dir's DACL to be allowlist-conforming. A state-dir whose parent (`%LOCALAPPDATA%\mcp-local-hub`) had its DACL broadened externally would pass the check.
- POSIX leg honors the parent-dir verify correctly; the gap is Windows-only — a security asymmetry between platforms in a spec-mandated step.

**Operator-visible failure mode:** an enterprise deployment with GPO-pushed `Authenticated Users:Read` on `%USERPROFILE%` would silently write hub-mode tokens to a parent dir whose contents (including the tokens) are listable / readable by every domain user. The spec's "Enterprise stance" section (lines 296-304) explicitly contemplates this scenario and requires the operator-facing diagnostic to fire.

## Files involved

- `internal/api/secure_write_windows.go:80-191` (secureWriteClientConfigImpl missing parent-dir DACL verify between line 99 `defer windows.CloseHandle(dirHandle)` and line 105 `refusePreexistingReparsePoint`)
- `internal/api/hub_mcp_state_dacl_windows.go:37-65` (verifyHubMcpStateDACLImpl never verifies the parent dir at all)
- `docs/superpowers/specs/2026-05-12-g4-unified-hub-mcp-design-v3.md:277-281, 310-326, 432` (spec source-of-truth)
- `docs/superpowers/plans/2026-05-12-g4-unified-hub-mcp.md:511-514` (plan pseudocode)
- `internal/api/secure_write_posix.go:56-58` (POSIX reference impl for parent-DACL verify pattern)

## Suggested fix

1. In `secureWriteClientConfigImpl` (Windows), insert between line 99 and line 105:
   ```go
   if err := verifyWindowsDACLFromHandle(dirHandle); err != nil {
       return fmt.Errorf("secure write: parent %s not single-user safe: %w", parentDir, err)
   }
   ```
2. In `verifyHubMcpStateDACLImpl` (Windows), after the file-DACL verify, open the parent dir with `FILE_FLAG_OPEN_REPARSE_POINT|FILE_FLAG_BACKUP_SEMANTICS` and call `verifyWindowsDACLFromHandle` on its handle too. Wrap the same error sentinel (`ErrDaclOutsideAllowlist`) with parent-dir context.
3. Add a Windows synthesis test in `secure_write_client_config_test.go` (or a new `_windows_test.go`) that builds a parent dir with `Authenticated Users:Read` via `SetNamedSecurityInfo`, then asserts `SecureWriteClientConfig` refuses with the parent-dir diagnostic. Symmetric for `VerifyHubMcpStateDACL`.
4. Update the file-header step list in `secure_write_windows.go` to keep parity with the spec's 14-step sequence; do not silently re-number.

## Resolution

Resolved on 2026-05-12 on branch `feat/g4-phase1-pre-gate` (commit landed in same branch following this entry).

Changes:

- `internal/api/secure_write_windows.go` — inserted `verifyWindowsDACLFromHandle(dirHandle)` immediately after `defer windows.CloseHandle(dirHandle)` in `secureWriteClientConfigImpl`. Error path returns `secure write: parent <dir> not single-user safe: ...`. File-header step list re-numbered to 10 steps (parent-DACL verify inserted as step 2; legacy crypto/rand / NtCreateFile / SetSecurityInfo / WriteFile / NtRename / re-open / re-verify shifted to 3-9). Also fixed misleading doc comment on `ntOpenRelative` re: `FILE_OPEN_REPARSE_POINT` semantics — clarified that the flag opens the reparse point metadata without following it, and the actual reject-on-reparse comes from the explicit `refusePreexistingReparsePoint` call earlier in the sequence (atomic rename with `FILE_RENAME_POSIX_SEMANTICS` guarantees the post-rename handle refers to the file we wrote).
- `internal/api/hub_mcp_state_dacl_windows.go` — added `verifyWindowsParentDACL(parentDir)` helper that opens the parent dir with `FILE_LIST_DIRECTORY | READ_CONTROL` and runs `verifyWindowsDACLFromHandle` against the resulting handle. `verifyHubMcpStateDACLImpl` now calls it after the file's own DACL verify succeeds. Errors wrap with parent-dir context.
- `openDirHandleNoReparse` and the new `verifyWindowsParentDACL` both request `READ_CONTROL` access on the dir handle — without it `GetSecurityInfo` fails with `ERROR_ACCESS_DENIED` regardless of who owns the directory.
- `internal/api/secure_write_windows_test.go` (new) — `TestSecureWriteClientConfigRejectsPermissiveParentDACL` builds a parent dir with an Authenticated Users:GenericRead ACE via `SetNamedSecurityInfo` (PROTECTED), then asserts `SecureWriteClientConfig` refuses with a parent-dir-context error AND leaves no half-written destination.
- `internal/api/hub_mcp_state_dacl_windows_test.go` — `TestVerifyHubMcpStateDACLRejectsPermissiveParentDACL` synthesizes the same permissive parent but locks the FILE's own DACL to allowlist-only, then asserts `VerifyHubMcpStateDACL` rejects with `ErrDaclOutsideAllowlist` wrapped in parent-dir context.
- `internal/api/hardened_tempdir_posix_test.go` + `internal/api/hardened_tempdir_windows_test.go` (new) — `hardenedTempDir(t)` shim. POSIX returns `t.TempDir()` as-is (the per-user 0700 trust boundary already covers it). Windows creates an intermediate dir and applies a PROTECTED allowlist-only DACL so existing round-trip tests don't get rejected by the new parent-dir gate when `%TEMP%` carries an inherited Authenticated Users ACE.
- Existing tests updated to call `hardenedTempDir(t)` instead of `t.TempDir()` where they exercise the writer/verifier happy path: `TestSecureWriteClientConfigBasicRoundTrip`, `TestSecureWriteClientConfigOverwritesExisting`, `TestSecureWriteClientConfigRefusesSymlinkTarget`, `TestSecureWriteClientConfigPosixMode0600`, `TestVerifyHubMcpStateDACLAcceptsFreshlyCreatedFile`, `TestVerifyHubMcpStateDACLRejectsAuthenticatedUsersAllow`, `TestVerifyHubMcpStateDACLAcceptsAllowlistOnly`.

Verification: `go build ./...` clean; `go vet ./internal/api/` clean; targeted test suite `go test -run "TestSecureWriteClientConfig|TestVerifyHubMcpStateDACL|TestManifest|TestValidateStateFileName" ./internal/api/` PASS (in both default and `-tags=test_state_path_env` builds).
