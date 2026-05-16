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
	"strings"

	"mcp-local-hub/internal/clients"
)

func init() {
	clients.WriteConfigFile = secureWriteWithOperatorOpt
	clients.CreateConfigFileIfMissing = secureCreateClientConfigIfMissingWithOperatorOpt
}

// secureCreateClientConfigIfMissingWithOperatorOpt is the
// cross-package init-only writer that clients.CreateConfigFileIfMissing
// resolves to in production. Walks the same operator-opt-in policy
// as secureWriteWithOperatorOpt: try the hardened pipeline first;
// fall back to a parent-gate-skipped variant if strict mode is OFF
// and the gate rejected on a single-user-but-broadened parent dir.
//
// Strict mode (MCPHUB_REQUIRE_SINGLE_USER_HOME enabled) keeps the
// parent-dir gate enforced — the /api/init-client-config endpoint
// short-circuits strict-mode operators BEFORE this function runs,
// so the strict refusal here is defense-in-depth for any future
// non-GUI caller of clients.CreateConfigFileIfMissing.
//
// Deep-sec PR #208 Lane C #1 closure.
func secureCreateClientConfigIfMissingWithOperatorOpt(path string, stub []byte) (created bool, err error) {
	created, err = SecureCreateClientConfigIfMissing(path, stub)
	if err == nil {
		return created, nil
	}
	if !errors.Is(err, ErrSecureWriteParentInsecure) {
		return false, err
	}
	if operatorRequiresSingleUserHome() {
		return false, fmt.Errorf("%w; %s is set, so the strict parent-dir gate is enforced for init-stub creation (unset that env var, or tighten the parent's DACL to remove the offending principal, to proceed)",
			err, RequireSingleUserHomeEnv)
	}
	// Default-relax lane: parent-dir gate rejected but operator did
	// not opt into strict mode. Re-run with the parent gate skipped;
	// the per-file allowlist DACL at create time still ensures the
	// new stub is owner-only regardless of parent broadening. Mirror
	// of secureWriteWithOperatorOpt's relax path.
	if logErr := LogHubMcpEvent("warn", "client-write-unhardened-fallback", map[string]any{
		"path":   path,
		"reason": "default-relax-on-solo-host (init-stub)",
		"origin": "SecureCreateClientConfigIfMissing",
		"err":    err.Error(),
	}); logErr != nil {
		_ = logErr
	}
	return secureCreateClientConfigIfMissingSkipParentGate(path, stub)
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
// attempts the hardened secure-write WITH the parent-dir gate; on
// ErrSecureWriteParentInsecure (Windows DACL or POSIX mode/owner
// rejection at the parent dir gate) it either:
//
//   - returns the strict error when MCPHUB_REQUIRE_SINGLE_USER_HOME is
//     set (multi-tenant / corp-managed posture), or
//   - re-runs the SAME hardened pipeline with the parent-dir gate
//     bypassed and logs a warn event (default v0.4.0+ posture for
//     solo-dev Windows hosts).
//
// PR #185 r3 (codex deep-sec P1 closure): the relax lane used to be
// a separate code path (fallbackWriteRefusingSymlink) using
// os.CreateTemp + path-based SetNamedSecurityInfo. That left a
// pre-hardening window between temp create and DACL apply during
// which a co-resident SID allowed by the parent dir's permissive
// DACL could race-open the temp file (Go's Windows runtime opens
// with FILE_SHARE_READ|FILE_SHARE_WRITE), retain the handle past
// the DACL tighten (ACL changes do not revoke existing handles),
// and read token bytes once the write committed. The fix routes
// the relax lane through the SAME handle-relative hardened pipeline
// as the strict path — the only difference is whether step 2
// (parent-dir DACL verify) runs. Per-file restrictive DACL is
// installed on the file HANDLE at step 5, before any bytes hit
// disk, closing the race window.
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
// conditions.
//
// The security guarantee for token-bearing client config files
// comes from THREE layers, in priority order:
//
//  1. New file DACL set on the file HANDLE at temp-create time, BEFORE
//     any bytes write. {current-user, LocalSystem,
//     BuiltinAdministrators} get GENERIC_ALL; PROTECTED_DACL prevents
//     inherited ACEs from re-broadening between rename and re-verify.
//     On POSIX: O_CREAT mode 0600 + defensive Fchmod(0600).
//  2. Symlink/reparse-point refusal at the destination
//     (refusePreexistingReparsePoint on Windows,
//     refusePreexistingSymlink on POSIX).
//  3. Handle-relative atomic rename (no TOCTOU window between create
//     and publish — the rename is anchored to the parent dirHandle,
//     not a path re-walk).
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
//   - open temp, write, rename, post-rename DACL verify
//   - pre-existing symlink/reparse-point at destination
//   - all non-gate hardened-write errors
//
// Backward compatibility: operators who already had
// MCPHUB_ALLOW_UNHARDENED_CLIENT_WRITE=1 set get identical behavior
// (the relax fires either way — old-style explicit opt-in OR new-
// style default). Operators on corp-managed hosts who want the
// strict gate must opt IN via MCPHUB_REQUIRE_SINGLE_USER_HOME=1.
func secureWriteWithOperatorOpt(path string, contents []byte) error {
	// v0.4.2: when relax mode is allowed (default), resolve
	// symlinks at the destination before calling secure-write. The
	// hardened pipeline refuses pre-existing reparse points
	// outright (symlink-attack defense), but a real-world solo-dev
	// pattern is to symlink dotfiles to a separate repo
	// (~/.codex/config.toml -> E:\env\Agents\.codex\config.toml).
	// Without resolution, every matrix Apply on such hosts fails
	// with "pre-existing reparse point refused" — making the GUI
	// unusable on standard dotfile-symlink setups.
	//
	// Strict mode (MCPHUB_REQUIRE_SINGLE_USER_HOME=1) keeps the
	// outright refusal so multi-tenant / corp-managed hosts get
	// the symlink-attack protection without compromise.
	//
	// Resolution writes through to the symlink's TARGET path.
	// The original symlink is left intact (mcphub does not
	// rewrite it as a regular file). The target's DACL after the
	// write is owner-only via the secure-write pipeline.
	if !operatorRequiresSingleUserHome() {
		if resolved, isSymlink := resolveSymlinkForSecureWrite(path); isSymlink && resolved != path {
			_ = LogHubMcpEvent("info", "client-write-symlink-followed", map[string]any{
				"symlink": path,
				"target":  resolved,
				"reason":  "default-relax-on-solo-host (dotfile pattern)",
			})
			path = resolved
		}
	}
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
	// Codex deep-sec PR #185 r2 P2: emit at WARN, not INFO. The
	// fallback is a security-boundary downgrade (operator-policy:
	// parent-dir gate skipped); warn-level keeps it visible to
	// log-monitoring conventions that filter info out of audit
	// dashboards.
	if logErr := LogHubMcpEvent("warn", "client-write-unhardened-fallback", map[string]any{
		"path":   path,
		"reason": reason,
		"err":    err.Error(),
	}); logErr != nil {
		// Best-effort: never swallow the original failure path; the
		// fallback write still proceeds.
		_ = logErr
	}
	// PR #185 r3: re-run the SAME hardened pipeline with parent-dir
	// gate bypassed. Per-file DACL/mode hardening still applies at
	// temp-create time, closing the race window that the previous
	// os.CreateTemp + path-based SetNamedSecurityInfo path left
	// open.
	return secureWriteClientConfigSkipParentGate(path, contents)
}

// resolveSymlinkForSecureWrite inspects path. If path is a symlink
// (Windows / POSIX symbolic link), returns (resolvedTargetPath,
// true). If path is a regular file or does not exist, returns
// (path, false).
//
// Best-effort: any error returns (path, false) and lets the
// standard secure-write path proceed (which will refuse the
// reparse point via refusePreexistingReparsePoint — that's the
// fail-safe default).
//
// Used by secureWriteWithOperatorOpt in relax mode to follow
// dotfile symlinks (e.g. ~/.codex/config.toml ->
// E:\env\Agents\.codex\config.toml). The hardened pipeline runs
// on the resolved target; the original symlink at `path` is
// untouched.
//
// Platform split: Windows uses GetFinalPathNameByHandle so the
// resolver walks through junction-mounted subst drives correctly
// (filepath.EvalSymlinks on Go 1.x fails on substed targets like
// E: -> %USERPROFILE%\OneDrive\... that the user routinely
// configures). POSIX uses filepath.EvalSymlinks which works
// uniformly there. Implementations live in
// client_write_resolve_{windows,posix}.go.
func resolveSymlinkForSecureWrite(path string) (string, bool) {
	info, err := os.Lstat(path)
	if err != nil {
		return path, false
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return path, false
	}
	resolved, err := resolveSymlinkFinalPath(path)
	if err != nil || resolved == "" || resolved == path {
		return path, false
	}
	return resolved, true
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
	return OperatorRequiresSingleUserHome()
}

// OperatorRequiresSingleUserHome is the exported canonical strict-mode
// predicate. The /api/init-client-config endpoint
// (internal/gui/init_client_config.go) uses it to refuse the Init
// affordance when strict mode is enabled — sharing the parser
// guarantees the endpoint accepts the same case-insensitive
// "1"/"true" forms as the secure-write pipeline rather than
// fail-opening on values the canonical reader treats as enabled.
// Deep-sec PR #208 Lane C #2 closure.
func OperatorRequiresSingleUserHome() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(RequireSingleUserHomeEnv))) {
	case "1", "true":
		return true
	}
	return false
}
