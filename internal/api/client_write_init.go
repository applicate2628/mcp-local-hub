// client_write_init.go — wire the production secure-write pipeline
// into the client-adapter writer hook.
//
// `internal/clients/write.go` declares `WriteConfigFile` defaulted to a
// plain os.WriteFile wrapper so in-package adapter tests continue to
// work against `t.TempDir()` (which on Windows lives under %TEMP%'s
// Authenticated Users-readable DACL and would fail the
// SecureWriteClientConfig parent-dir gate).
//
// Production wires it to `secureWriteWithOperatorOpt` so every
// token-bearing rewrite — including the Phase 5 install reconciler's
// `mcphub-hub` aggregate entry — flows through the handle-relative,
// DACL-bound pipeline. The swap is a one-way override: package `api`
// is in the import graph of every production entry point (cmd/mcphub,
// internal/cli, internal/gui), so this init() always runs before any
// adapter call.
//
// Issue #161 P1 closure: the wrapper adds an operator-explicit
// fallback for corp-policy machines where the hardened gate would
// otherwise refuse ordinary install/migrate. The fallback never
// fires silently — operators must set the
// MCPHUB_ALLOW_UNHARDENED_CLIENT_WRITE env var to "1" / "true" first,
// and every fallback write logs a structured warn event via the
// hub-mcp event log.
//
// Spec: docs/superpowers/specs/2026-05-12-g4-unified-hub-mcp-design-v3.md
// §"SecureWriteClientConfig sequence" + §"Bidirectional install
// reconciler".
// Plan: docs/superpowers/plans/2026-05-12-g4-unified-hub-mcp.md Task 5.1
// step 6 ("Route ALL adapter writes through SecureWriteClientConfig").

package api

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"mcp-local-hub/internal/clients"
)

func init() {
	clients.WriteConfigFile = secureWriteWithOperatorOpt
}

// AllowUnhardenedClientWriteEnv is a legacy operator-explicit opt-in
// for the unhardened client-config write path. Pre-v0.4.0 the
// parent-dir DACL gate was STRICT by default, and this env var was
// the only way to bypass it. v0.4.0 flips the default to RELAX (see
// secureWriteWithOperatorOpt below), so this env var is now
// effectively a no-op vs the default — kept for backward
// compatibility with operators who already have it set in their
// shell profile or scheduler scripts.
const AllowUnhardenedClientWriteEnv = "MCPHUB_ALLOW_UNHARDENED_CLIENT_WRITE"

// RequireSingleUserHomeEnv is the v0.4.0+ operator opt-in for the
// STRICT parent-dir DACL/mode gate. Set to "1" or "true" (case-
// insensitive) on corp-managed machines, shared hosts, or other
// multi-tenant contexts where the parent-dir DACL check is the
// authoritative security boundary. When unset, mcphub treats the
// solo-developer Windows case as the common path and proceeds with
// the symlink-refusing fallback when parent-dir is not single-user-
// safe — see secureWriteWithOperatorOpt for rationale.
const RequireSingleUserHomeEnv = "MCPHUB_REQUIRE_SINGLE_USER_HOME"

// secureWriteWithOperatorOpt is the cross-package writer that
// `clients.WriteConfigFile` resolves to in production. It first
// attempts the hardened secure-write; on
// ErrSecureWriteParentInsecure (Windows DACL or POSIX mode/owner
// rejection at the parent dir gate) it either:
//
//   - returns the strict error when MCPHUB_REQUIRE_SINGLE_USER_HOME is
//     set (multi-tenant / corp-managed posture), or
//   - falls back to the symlink-refusing temp+atomic-rename writer
//     and logs an info event (default v0.4.0+ posture for solo-dev
//     Windows hosts).
//
// Default-relax rationale (v0.4.0):
//
// On solo-developer Windows hosts, the parent-dir DACL of
// %USERPROFILE% routinely contains ACEs the operator did not place
// there deliberately: Codex Sandbox local groups (CodexSandboxUsers),
// AppContainer SIDs for UWP apps, orphan SIDs from old AD accounts
// or prior hostnames, dev sandbox IDE installers, etc. The user
// already has full write access via their own account, and ordinary
// notepad/IDE writes to ~/.claude.json succeed — having mcphub
// refuse on the same parent makes the tool look broken in real-world
// conditions. The security guarantee for token-bearing client config
// files comes from THREE layers, in priority order:
//
//  1. New file mode 0600 (Chmod after temp create, before rename).
//     On Windows this translates to a DACL granting only the owner
//     Read+Write — no inheritance from parent. Other principals on
//     the parent's ACL do NOT propagate to the new file's ACL.
//  2. Symlink refusal at the destination (Lstat before open, reject
//     pre-existing symlink/reparse-point).
//  3. Temp+atomic-rename (no TOCTOU window between create and
//     publish).
//
// Layers 1+2+3 deliver "the file we just wrote is not readable by
// other principals on this host" regardless of the parent dir's
// historical ACE list. The parent-dir DACL gate is belt-and-
// suspenders that only matters when the operator does NOT trust
// their own admin of the parent dir's ACL — which is the
// multi-tenant / corp-managed posture explicitly gated by
// MCPHUB_REQUIRE_SINGLE_USER_HOME.
//
// Failure classes that propagate unchanged (the relax is scoped
// narrowly to the parent-dir gate):
//
//   - open temp, write, rename, post-rename verify
//   - pre-existing symlink/reparse-point at destination
//   - all non-gate hardened-write errors
//
// codex bot r1 P1 closure (PR #165): the original opt-in path used
// raw os.WriteFile, which silently follows symlinks. Even on the
// relax lane we MUST refuse to write through a pre-existing
// symlink/junction. The fallbackWriteRefusingSymlink helper Lstats
// first and rejects before opening.
//
// Backward compatibility: operators who already had
// MCPHUB_ALLOW_UNHARDENED_CLIENT_WRITE=1 set get identical behavior
// (the relax fires either way — old-style explicit opt-in OR new-
// style default). Operators on corp-managed hosts who want the
// strict gate must now opt IN via MCPHUB_REQUIRE_SINGLE_USER_HOME=1.
func secureWriteWithOperatorOpt(path string, contents []byte) error {
	err := SecureWriteClientConfig(path, contents)
	if err == nil {
		return nil
	}
	if !errors.Is(err, ErrSecureWriteParentInsecure) {
		return err
	}
	if operatorRequiresSingleUserHome() {
		return fmt.Errorf("%w; %s=1 is set, so the strict parent-dir gate is enforced (unset that env var, or tighten the parent's DACL to remove the offending principal, to proceed)",
			err, RequireSingleUserHomeEnv)
	}
	reason := "default-relax-on-solo-host"
	if operatorAllowedUnhardenedClientWrite() {
		// Legacy explicit opt-in produces the same fallback as
		// default; distinguish in the audit log so operators can
		// grep their shell profile after upgrade and remove the
		// now-redundant env var.
		reason = "legacy opt-in via " + AllowUnhardenedClientWriteEnv + " (now redundant — same as default)"
	}
	if logErr := LogHubMcpEvent("info", "client-write-unhardened-fallback", map[string]any{
		"path":   path,
		"reason": reason,
		"err":    err.Error(),
	}); logErr != nil {
		// Best-effort: never swallow the original failure path; the
		// fallback write still proceeds.
		_ = logErr
	}
	return fallbackWriteRefusingSymlink(path, contents)
}

// fallbackWriteRefusingSymlink writes contents to path with hardened
// per-file permissions via a temp file + atomic rename. The relax
// lane's residual protections beyond the parent-dir gate:
//
//  1. Lstat the destination first (does NOT follow symlinks). If
//     a symlink is present at `path`, refuse outright. Otherwise
//     a hostile pre-existing symlink would redirect the write
//     after the rename.
//  2. Create a fresh temp file in the same dir.
//  3. Harden the temp file's permissions PLATFORM-CORRECTLY before
//     writing contents:
//       - POSIX: Chmod(0o600) — owner Read+Write only.
//       - Windows: setRestrictiveDACL on the handle — DACL granting
//         GENERIC_ALL to {current-user, LocalSystem, Builtin
//         Administrators} only, with PROTECTED_DACL_SECURITY_INFORMATION
//         to block inheritance of parent ACEs. Bot r1 P1 closure on
//         PR #185: Go's os.Chmod on Windows only toggles the
//         FILE_ATTRIBUTE_READONLY bit and does NOT touch the ACL,
//         so without explicit DACL hardening the new file would
//         inherit parent's permissive ACEs (CodexSandboxUsers,
//         AppContainer SIDs, orphan AD SIDs) — exactly the
//         principals the parent-dir gate was trying to keep out.
//  4. Write contents to the now-hardened temp file.
//  5. Close + atomically rename over `path`.
//
// codex bot r1 P1 closure (PR #165): the original implementation
// used raw os.WriteFile, which silently followed symlinks.
// codex bot r2 P1 closure (PR #165): subsequent fix using
// os.WriteFile(path, ..., 0o600) preserved the pre-existing file's
// mode bits — temp+rename closes that channel.
// codex bot r1 P1 closure (PR #185): the previous Chmod-only step
// was a no-op for Windows ACL hardening; replaced with the
// hardenTempFileForUnhardenedFallback helper that wires
// setRestrictiveDACL on Windows and stays Chmod on POSIX.
//
// Residual gap vs the hardened SecureWriteClientConfig path: a
// small TOCTOU window between Lstat and the temp+rename. Handle-
// relative ops would close it; the relax lane documents the
// trade-off (operator must trust the host for symlink-swap races).
func fallbackWriteRefusingSymlink(path string, contents []byte) error {
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing to write to %s: destination is a symlink (the unhardened-client-write fallback does NOT downgrade symlink refusal — remove or replace the symlink and retry)", path)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("lstat %s before unhardened write: %w", path, err)
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".mcphub-unhardened-write.*.tmp")
	if err != nil {
		return fmt.Errorf("create temp in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}
	// Step 3 — platform-correct hardening BEFORE writing token
	// content. On Windows this is the load-bearing call that gives
	// the new file owner-only ACL; without it Go's Chmod is a no-op
	// for ACLs and the file inherits parent's permissive ACEs.
	if err := hardenTempFileForUnhardenedFallback(tmp); err != nil {
		cleanup()
		return fmt.Errorf("harden temp %s permissions: %w", tmpPath, err)
	}
	if _, err := tmp.Write(contents); err != nil {
		cleanup()
		return fmt.Errorf("write temp %s: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close temp %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename temp to %s: %w", path, err)
	}
	return nil
}

// operatorAllowedUnhardenedClientWrite reports whether the operator
// has explicitly opted into the unhardened-write fallback via the
// AllowUnhardenedClientWriteEnv env var. Accepts "1" and "true"
// case-insensitively; everything else (including unset, "0",
// "false", "no", garbage) returns false.
//
// Post-v0.4.0 the default is already "fall back to unhardened on
// parent-dir gate failure", so this helper exists only to log the
// "legacy opt-in" reason in audit events — operators who had the
// env var set pre-v0.4.0 get the same behavior, distinguished in the
// log so they can clean up their shell profile.
func operatorAllowedUnhardenedClientWrite() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(AllowUnhardenedClientWriteEnv))) {
	case "1", "true":
		return true
	}
	return false
}

// operatorRequiresSingleUserHome reports whether the operator has
// explicitly opted INTO the strict parent-dir DACL/mode gate via the
// RequireSingleUserHomeEnv env var. Accepts "1" and "true" case-
// insensitively; everything else (including unset) returns false,
// which means v0.4.0+ default (relax-on-gate-failure) applies.
//
// Operators set this on corp-managed machines, shared hosts, build
// servers, CI runners, or anywhere multi-tenant trust boundaries
// require the strict gate. Solo-developer Windows hosts should leave
// it unset — see secureWriteWithOperatorOpt above for rationale.
func operatorRequiresSingleUserHome() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(RequireSingleUserHomeEnv))) {
	case "1", "true":
		return true
	}
	return false
}
