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
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"mcp-local-hub/internal/clients"
)

func init() {
	clients.WriteConfigFile = secureWriteWithOperatorOpt
	clients.CreateConfigFileIfMissing = secureCreateClientConfigIfMissingWithOperatorOpt
	clients.SecureCreateParentDir = SecureCreateParentDirForConfigLock
}

// secureCreateClientConfigIfMissingWithOperatorOpt is the
// cross-package init-only writer that clients.CreateConfigFileIfMissing
// resolves to in production. Walks the same operator-opt-in policy
// as secureWriteWithOperatorOpt: try the hardened pipeline first;
// fall back to a parent-gate-skipped variant if strict mode is OFF
// and the gate rejected on a single-user-but-broadened parent dir.
//
// Strict mode (MCPHUB_REQUIRE_SINGLE_USER_HOME, or the persisted
// supervisor-intent.json strict_mode bit, enabled) keeps the
// parent-dir gate enforced. This wrapper IS the strict enforcement
// point for the GUI Init affordance: the /api/init-client-config
// endpoint does NOT short-circuit strict-mode operators (that
// pre-check was REMOVED in the PR #208 codex-r1 F1 closure — see
// internal/gui/init_client_config.go), so the strict refusal returned
// here is what surfaces as INIT_FAILED. It also covers any future
// non-GUI caller of clients.CreateConfigFileIfMissing.
//
// Deep-sec PR #208 Lane C #1 closure.
func secureCreateClientConfigIfMissingWithOperatorOpt(path string, stub []byte) (created bool, err error) {
	created, err = SecureCreateClientConfigIfMissing(path, stub)
	if err == nil {
		return created, nil
	}
	// Foreign-owned parent → refuse; non-parent-gate error → raw; only a
	// broadened-but-owner-correct parent falls through to strict + relax. Same
	// single-owner decision the write lanes use (bug 2026-07-08 F1 — this create
	// lane previously relaxed a wrong-owner parent because it hand-rolled a bare
	// ErrSecureWriteParentInsecure check).
	if refuse, relaxEligible := clientConfigParentGateRefusalOrRelax(err); !relaxEligible {
		return false, refuse
	}
	if operatorRequiresSingleUserHome() {
		return false, fmt.Errorf("%w; strict mode is active (via %s, or via persisted supervisor-intent.json strict_mode set by `mcphub strict-mode enable`), so the strict parent-dir gate is enforced for init-stub creation (unset that env var or run `mcphub strict-mode disable`, or tighten the parent's DACL to remove the offending principal, to proceed)",
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
	return SecureCreateOwnerOnlyFile(path, stub)
}

// SecureCreateClientConfigParentDirWithOperatorOpt securely creates the
// missing parent directory of `configPath` (G17) applying the SAME
// operator-opt-in policy as the file create: try the hardened pipeline
// (home-anchor DACL/mode gate ENFORCED) first; on
// ErrSecureWriteParentInsecure, route through clientConfigParentGateRefusalOrRelax
// — a WRONG-OWNER (foreign-owned) parent is REFUSED regardless of strict mode
// (bug 2026-07-08 F1), a broadened-but-owner-correct parent returns the strict
// error when strict mode is active, else re-runs with the anchor gate bypassed
// and logs a warn event. Created directories are owner-only and the symlink /
// home-containment refusals apply on BOTH lanes.
//
// Exported so the GUI /api/init-client-config endpoint
// (internal/gui/init_client_config.go) can secure-create the parent for
// the new "missing-init-creatable" state before calling adapter
// InitEmpty(). G17 (2026-06-18).
func SecureCreateClientConfigParentDirWithOperatorOpt(configPath string) error {
	err := SecureCreateClientConfigParentDir(configPath)
	if err == nil {
		return nil
	}
	// Same single-owner parent-gate decision as the write lanes (F1).
	if refuse, relaxEligible := clientConfigParentGateRefusalOrRelax(err); !relaxEligible {
		return refuse
	}
	if operatorRequiresSingleUserHome() {
		return fmt.Errorf("%w; strict mode is active (via %s, or via persisted supervisor-intent.json strict_mode set by `mcphub strict-mode enable`), so the strict parent-dir gate is enforced for init-parent creation (unset that env var or run `mcphub strict-mode disable`, or tighten the home directory's DACL to remove the offending principal, to proceed)",
			err, RequireSingleUserHomeEnv)
	}
	// Default-relax lane: home-anchor gate rejected but operator did not
	// opt into strict mode. Re-run with the anchor gate skipped; created
	// dirs stay owner-only (mode 0700 / allowlist DACL) and the symlink /
	// home-containment refusals still apply.
	if logErr := LogHubMcpEvent("warn", "client-write-unhardened-fallback", map[string]any{
		"path":   configPath,
		"reason": "default-relax-on-solo-host (init-parent-dir)",
		"origin": "SecureCreateClientConfigParentDir",
		"err":    err.Error(),
	}); logErr != nil {
		_ = logErr
	}
	return secureCreateClientConfigParentDirSkipParentGate(configPath)
}

// SecureCreateParentDirForConfigLock securely creates the missing parent
// directory of a client-config WRITE TARGET for the shared withConfigLock
// chokepoint (internal/clients/config_lock.go), applying the SAME
// operator-opt-in policy as the file create: try the hardened pipeline
// (the strict-mode DACL/mode gate ENFORCED on the deepest existing prefix)
// first; on ErrSecureWriteParentInsecure, route through
// clientConfigParentGateRefusalOrRelax — a WRONG-OWNER (foreign-owned) parent is
// REFUSED regardless of strict mode (bug 2026-07-08 F1), a broadened-but-owner-
// correct parent returns the strict error when strict mode is active, else
// re-runs with the parent-dir gate bypassed and logs a warn event. Created
// directories are owner-only and the symlink / reparse-point refusals apply on
// BOTH lanes.
//
// DIFFERENCE from SecureCreateClientConfigParentDirWithOperatorOpt (the G17
// Init-button parent creator): that creator REFUSES any path outside the user
// home (a blast-radius bound for the GUI affordance). This one does NOT —
// withConfigLock is SHARED across every client adapter, and a legitimate write
// target can live OUTSIDE the home: MiMoCode's global dir is
// $MIMOCODE_HOME/config (MIMOCODE_HOME may be any absolute path) or
// $XDG_CONFIG_HOME/mimocode (see internal/clients/mimocode.go
// resolveMimoCodeGlobalDir). A home-bounded creator wired here would convert
// the bot PR #420 finding 1 P1 into an install-breaking regression for
// outside-home targets. The LOAD-BEARING security property — refuse a
// symlink/reparse-point component on EVERY component, create each real component
// fresh owner-only, descend fd/handle-relative from the VOLUME ROOT (TOCTOU-safe)
// — is preserved; only the home-containment BLAST-RADIUS bound is dropped. The
// descent runs from the volume root (NOT the nearest existing ancestor — an
// absolute-path anchor re-open followed an intermediate symlink, the F1 residual),
// and the strict-mode DACL gate verifies only the DEEPEST EXISTING PREFIX (not
// every system-owned ancestor like C:\Users — bot PR #420 r17 finding B1), exactly
// as the POSIX leg does. (bot PR #420 finding 1 + F1 + r17 B1.)
//
// `dir` is the parent directory itself (filepath.Dir(configPath) at the call
// site), so this creates `dir` and any missing ancestors, not dir's parent.
func SecureCreateParentDirForConfigLock(dir string) error {
	err := secureCreateParentDirAnywhereImpl(dir, false /*skipParentGate*/)
	if err == nil {
		return nil
	}
	// Same single-owner parent-gate decision as the write lanes (F1).
	if refuse, relaxEligible := clientConfigParentGateRefusalOrRelax(err); !relaxEligible {
		return refuse
	}
	if operatorRequiresSingleUserHome() {
		return fmt.Errorf("%w; strict mode is active (via %s, or via persisted supervisor-intent.json strict_mode set by `mcphub strict-mode enable`), so the strict parent-dir gate is enforced for config-lock parent creation (unset that env var or run `mcphub strict-mode disable`, or tighten the parent's DACL to remove the offending principal, to proceed)",
			err, RequireSingleUserHomeEnv)
	}
	// Default-relax lane: the deepest-existing-prefix DACL gate rejected but the
	// operator did not opt into strict mode. Re-run with the parent-dir gate
	// skipped; created dirs stay owner-only (mode 0700 / allowlist DACL) and the
	// per-component symlink / reparse-point refusals still apply.
	if logErr := LogHubMcpEvent("warn", "client-write-unhardened-fallback", map[string]any{
		"path":   dir,
		"reason": "default-relax-on-solo-host (config-lock-parent-dir)",
		"origin": "SecureCreateParentDirForConfigLock",
		"err":    err.Error(),
	}); logErr != nil {
		_ = logErr
	}
	return secureCreateParentDirAnywhereImpl(dir, true /*skipParentGate*/)
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

// AllowUnhardenedStateWriteEnv is an operator-explicit opt-in for
// bypassing the state-file parent-dir write/delete TOCTOU check.
// The hardened per-file DACL/mode write path still applies; this
// only lets operators accept the state-file parent relax lane when
// parent ACL cleanup is not practical.
const AllowUnhardenedStateWriteEnv = "MCPHUB_ALLOW_UNHARDENED_STATE_WRITE"

// AllowUnhardenedStateReadEnv is the READ-side counterpart to
// AllowUnhardenedStateWriteEnv. Set to "1" / "true" to opt out of
// the parent-dir DACL/mode check on supervisor state-file reads
// (supervisor-intent.json, supervisor-state.json).
//
// Why a separate env var (not piggy-backing on the WRITE one): the
// READ-side gate was added in PR #223 specifically so that operators
// who set the WRITE relax env var for their write-side ACL needs
// cannot accidentally turn off READ-side TOCTOU defense too. Reads
// stay strict-by-default; the READ env var is the explicit consent.
//
// Real-world trigger that required this opt-in: corp-managed Windows
// hosts whose %LOCALAPPDATA% inherits a Domain Users / "Authenticated
// Users" ACE that the user cannot remove. With STRICT reads, the
// supervisor crashed at startup with "insecure parent directory"
// before it could even load supervisor-intent.json — Dashboard then
// showed Failed-to-load with no recovery path.
const AllowUnhardenedStateReadEnv = "MCPHUB_ALLOW_UNHARDENED_STATE_READ"

// operatorAllowsUnhardenedStateRead reports whether the operator has
// explicitly opted into the relax lane for supervisor state-file
// READS by setting MCPHUB_ALLOW_UNHARDENED_STATE_READ to "1" or
// "true" (case-insensitive). Default false: reads remain strict.
func operatorAllowsUnhardenedStateRead() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(AllowUnhardenedStateReadEnv)))
	return v == "1" || v == "true"
}

// RequireSingleUserHomeEnv is the v0.4.0+ operator opt-in for the
// STRICT parent-dir DACL/mode gate. Set to "1" or "true" (case-
// insensitive) on corp-managed machines, shared hosts, or other
// multi-tenant contexts where the parent-dir DACL check is the
// authoritative security boundary. When unset, mcphub treats the
// solo-developer Windows case as the common path and proceeds with
// the symlink-refusing fallback when parent-dir is not single-user-
// safe — see secureWriteWithOperatorOpt for rationale.
//
// SEC-F2: this env var is NO LONGER the only enforcement input. The
// persisted supervisor-intent.json `strict_mode` bit — mutated by the
// operator-facing `mcphub strict-mode {enable,disable}` command — is
// now ALSO honored by OperatorRequiresSingleUserHome(). Before SEC-F2
// the intent bit was inert for enforcement, so `strict-mode enable`
// was a security false-promise (it claimed to activate the strict gate
// but the gate read only this env var). The intent value is read once
// per process (lazy cache) — see strictModeIntentCache below.
const RequireSingleUserHomeEnv = "MCPHUB_REQUIRE_SINGLE_USER_HOME"

// AllowClientConfigSymlinkEnv is the operator opt-in for resolving
// pre-existing symlinks at client-config destination paths instead
// of refusing them. Set to "1" or "true" on solo-developer hosts
// where dotfile-management (chezmoi, yadm, GNU stow, plain
// `ln -s`) symlinks the canonical client config out to a separate
// repo (e.g. `~/.codex/config.toml -> /e/env/Agents/.codex/config.toml`).
//
// Background: PR #209 removed default-mode symlink resolution from
// the secure-write pipeline to close a confused-deputy regression
// where default-mode writes followed an attacker-planted symlink
// and overwrote attacker-chosen targets. That refusal is the right
// default for multi-tenant / corp-managed hosts but breaks
// dotfile-symlinked client configs on solo-developer machines
// (see work-items/bugs/2026-05-19-codex-config-symlink-blocked-by-pr209.md).
//
// When this env var is set, the WRITE pipeline resolves the symlink
// to its target BEFORE calling the hardened secure-write. The
// hardened pipeline then operates on the resolved target as if
// no symlink existed — `refusePreexistingReparsePoint` does not
// see a symlink (the path passed in is already the real target),
// the file's own DACL is installed at handle-create time on the
// target, and the original symlink is left intact. The SCAN
// pipeline similarly treats a symlink-to-regular-file as "ok"
// (writable) instead of "config-error".
//
// Security tradeoff under this opt-in: an attacker with write
// access to the operator's home directory could plant a symlink
// at a known client-config path BEFORE the operator sets this env
// var; once set, the next mcphub write would follow the attacker
// symlink to an attacker-chosen target. The opt-in is therefore
// scoped narrowly to operators who manage their own dotfile
// symlinks and trust the symlink target. Strict mode
// (MCPHUB_REQUIRE_SINGLE_USER_HOME=1) overrides this opt-in and
// refuses symlinks regardless — corp-managed hosts get the
// hardening unconditionally.
const AllowClientConfigSymlinkEnv = "MCPHUB_ALLOW_CLIENT_CONFIG_SYMLINK"

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
// ResolvedSymlinkConsent carries a SCOPED, per-write operator consent to
// follow a client-config symlink to its resolved target — the SEAM-B
// supplement to the AllowClientConfigSymlinkEnv env-var opt-in (NOT a
// replacement: the env var stays valid; a scoped consent is the second,
// per-path input to the SAME follow-symlink predicate).
//
// PinnedResolvedPath is the full resolved TARGET path (parent + basename) the
// operator approved at confirm time, not just the parent. At write time the
// resolve re-runs and the freshly-resolved full target MUST equal
// PinnedResolvedPath, else the write is REFUSED — this is the
// swap-between-confirm-and-write guard: a co-resident who repoints the symlink
// after the operator consented but before the write lands cannot redirect the
// privileged write to a different target, because the pin no longer matches.
// Pinning the FULL target (not just the parent) closes the same-parent-repoint
// bypass: repointing /cfg/claude.json -> /cfg/other.json (SAME parent) before
// the write is now caught, where a parent-only pin would have passed it.
//
// Client + OriginalPath are diagnostic/audit context only (which client's
// config, the symlink path the operator pointed at); they are NOT part of
// the pin-match decision.
//
// CLIENT-CONFIG-ONLY: a ResolvedSymlinkConsent is never constructed for a
// state_dir / supervisor-intent write. The state-file pipeline has its own
// strict-mode resolution and does not follow symlinks.
type ResolvedSymlinkConsent struct {
	Client             string
	OriginalPath       string
	PinnedResolvedPath string
}

// secureWriteWithOperatorOpt is the production clients.WriteConfigFile hook.
// It carries NO scoped consent (consent=nil) — the symlink-follow decision
// then reduces to the AllowClientConfigSymlinkEnv env-var opt-in alone
// (the F2 lane). PR-2 will add the scoped-consent caller surface; PR-1
// keeps the production hook's behavior identical via the nil-consent path.
func secureWriteWithOperatorOpt(path string, contents []byte) error {
	return secureWriteWithOperatorOptConsent(path, contents, nil)
}

// secureWriteWithOperatorOptConsent is the consent-aware writer. The inputs to
// the ONE follow-symlink predicate are:
//
//   - the AllowClientConfigSymlinkEnv env-var opt-in (F2),
//   - a present, matching ResolvedSymlinkConsent (SEAM-B), and
//   - an approving InteractiveSymlinkConsent injected port (A3 PR-2 design B;
//     nil in production, set by the CLI for interactive install/reconcile).
//
// Any one alone is sufficient to follow a symlink; strict mode
// (MCPHUB_REQUIRE_SINGLE_USER_HOME=1, checked inside
// OperatorAllowsClientConfigSymlink AND re-checked here for BOTH the scoped
// consent path and the interactive-port path) overrides ALL of them and
// refuses symlink-follow unconditionally.
//
// AF-1 closure: when a symlink is followed, the write goes through the
// HANDLE-PINNED secureWriteThroughResolvedParentHandle (resolve → open the
// resolved target's parent ONCE → write through the held handle), NOT the
// old resolve-to-STRING → SecureWriteClientConfig(resolvedString) path that
// re-walked the resolved string (filepath.Split + a second path-based
// parent open). The pinned parent handle/fd is frozen at open, so a
// symlink/component swap between resolve and write cannot redirect the
// privileged write.
func secureWriteWithOperatorOptConsent(path string, contents []byte, consent *ResolvedSymlinkConsent) error {
	// Follow-symlink decision: the env-var opt-in OR a present scoped
	// consent. Strict mode overrides both — OperatorAllowsClientConfigSymlink
	// already returns false under strict, and the consent branch re-checks
	// operatorRequiresSingleUserHome() so a scoped consent can NEVER bypass
	// strict mode (PROTECTED invariant: corp-managed hosts get symlink-attack
	// hardening regardless of any per-write consent).
	followViaEnv := OperatorAllowsClientConfigSymlink()
	followViaConsent := consent != nil && !operatorRequiresSingleUserHome()
	if followViaEnv || followViaConsent {
		if _, was := resolveSymlinkForSecureWrite(path); was {
			// Test-only injection seam (race-window-assertion discipline):
			// fires AFTER the confirm-time resolve confirmed a symlink but
			// BEFORE secureWriteFollowingSymlink re-resolves for the
			// pin-match. A test repoints the symlink here to engineer a
			// deterministic swap-between-confirm-and-write window (T3) so the
			// TOCTOU pin-mismatch refusal is exercised without relying on the
			// natural race staying open. nil in production.
			if afterResolveBeforePinHook != nil {
				afterResolveBeforePinHook()
			}
			return secureWriteFollowingSymlink(path, contents, consent, followViaEnv)
		}
	}

	// A3 PR-2 interactive-consent port (design B): when NO explicit consent
	// and NO env opt-in followed above, an injected InteractiveSymlinkConsent
	// port (set by the CLI for interactive install/reconcile, nil in
	// production) is consulted for a symlinked destination. The gate is the
	// SAME !operatorRequiresSingleUserHome() invariant as followViaConsent
	// above — a consent-via-hook can NEVER bypass strict mode (PROTECTED:
	// corp-managed hosts get symlink-attack hardening regardless of any
	// per-write consent). On approval the port produces a fresh scoped
	// ResolvedSymlinkConsent pinned to the JUST-resolved FULL target path, then
	// follows via the same handle-pinned, pin-verified secureWriteFollowingSymlink
	// path as an explicit consent — the prompt only PRODUCES the consent; the
	// existing write-time re-resolve + pin guard still VERIFIES it.
	if consent == nil && !followViaEnv && InteractiveSymlinkConsent != nil && !operatorRequiresSingleUserHome() {
		// Pin via the SINGLE owner (ResolveClientConfigSymlink) so this site
		// never independently re-derives the pin — the full resolved target
		// path it returns is the same value the write-time guard recomputes.
		if _, pinnedPath, was := ResolveClientConfigSymlink(path); was {
			// Attribute the write to the client whose config path this is, so
			// the prompt names the real client and the
			// client-write-symlink-resolved-via-scoped-consent audit event logs
			// a non-empty "client" (the GUI lane already sets it via
			// resolve_symlink_write.go; this is the CLI/production parity). The
			// hook receives only (path, contents) so the name is DERIVED from
			// the destination path against the adapter catalog — see
			// deriveClientNameForConfigPath for why the call path is too wide to
			// thread a client param through.
			client := deriveClientNameForConfigPath(path)
			if InteractiveSymlinkConsent(client, path, pinnedPath) {
				hookConsent := &ResolvedSymlinkConsent{
					Client:             client,
					OriginalPath:       path,
					PinnedResolvedPath: pinnedPath,
				}
				// Same test-only swap seam as the explicit-consent lane above
				// so a swap between the interactive resolve and the write-time
				// re-resolve is still caught by the pin guard.
				if afterResolveBeforePinHook != nil {
					afterResolveBeforePinHook()
				}
				return secureWriteFollowingSymlink(path, contents, hookConsent, false)
			}
			// Operator declined at the prompt: fall through to the standard
			// path-based pipeline, where the pre-existing-symlink refusal is
			// the documented default. No silent follow.
		}
	}

	// Non-symlink path (or symlink-follow not consented): the standard
	// path-based hardened pipeline with the env/legacy relax fallback.
	return secureWritePathBased(path, contents)
}

// afterResolveBeforePinHook is a TEST-ONLY seam (see
// secureWriteWithOperatorOptConsent). nil in production. Tests set it to
// repoint a symlink between the confirm-time resolve and the write-time
// re-resolve, engineering a deterministic TOCTOU window for the
// scoped-consent pin-mismatch guard (T3).
var afterResolveBeforePinHook func()

// clientCatalogForDerivation supplies the {name -> adapter} catalog
// deriveClientNameForConfigPath matches a write destination against. It
// defaults to clients.AllClients (the SINGLE owner of the name->adapter
// mapping); a test overrides it to inject a hermetic adapter whose
// ConfigPath() is the test's temp symlink, so the production derivation lane
// can be exercised without depending on the host's real client config paths.
// Production callers never set it.
var clientCatalogForDerivation = clients.AllClients

// deriveClientNameForConfigPath reverse-maps a client-config destination path
// to the client name that owns it, by matching against each adapter's
// ConfigPath(). Returns "" when no adapter claims the path (an unknown or
// non-client destination — the audit then records an empty client rather than
// a wrong one).
//
// Why DERIVE instead of THREAD the name down: the only input the
// clients.WriteConfigFile hook receives is (path, contents); the client name
// is not in scope there. Threading a client param would change the
// clients.WriteConfigFile function-pointer signature plus every adapter write
// call site (15+ across internal/clients), a wide shared-seam change for an
// audit-attribution fix. The adapter's ConfigPath() is already the single
// owner of the name->path mapping (clients.ConfigPathForName resolves the same
// way), so a reverse lookup here reuses that owner without widening the seam.
// Paths are compared after filepath.Clean so separator/relativity differences
// do not cause a spurious miss.
func deriveClientNameForConfigPath(path string) string {
	want := filepath.Clean(path)
	for name, adapter := range clientCatalogForDerivation() {
		if filepath.Clean(adapter.ConfigPath()) == want {
			return name
		}
	}
	return ""
}

// secureWriteFollowingSymlink performs the handle-pinned write to a
// resolved symlink target at `originalPath`. It RE-RESOLVES the symlink
// here (not trusting any earlier confirm-time resolve) so the pin-match
// reflects the symlink's state at write time — that is the load-bearing
// half of the swap-between-confirm-and-write guard. `consent` (when
// non-nil) pins the resolved FULL TARGET path (parent + basename) the
// operator approved, so a same-parent repoint to a sibling file is
// refused. It tries the
// hardened pipeline (parent gate ON) first; on ErrSecureWriteParentInsecure
// with strict mode OFF it retries with the parent gate skipped, mirroring
// secureWritePathBased's relax fallback.
func secureWriteFollowingSymlink(originalPath string, contents []byte, consent *ResolvedSymlinkConsent, followViaEnv bool) error {
	// Re-resolve at write time. If the symlink vanished or is no longer a
	// symlink, refuse — the operator consented to following a symlink, and
	// a target that changed shape is exactly the swap we must catch.
	resolved, was := resolveSymlinkForSecureWrite(originalPath)
	if !was {
		return fmt.Errorf("secure write: symlink %s no longer resolves to a follow-able target at write time; refusing the write — the symlink may have been removed or repointed after consent",
			originalPath)
	}

	// SEAM-B swap-between-confirm-and-write guard: when a scoped consent is
	// present, the write-time-resolved FULL TARGET path now MUST equal the full
	// target the operator consented to. Comparing the full path (parent +
	// basename), not just the parent, closes the same-parent-repoint bypass: a
	// symlink approved pointing at /cfg/claude.json and repointed to
	// /cfg/other.json (SAME parent) before the write no longer matches the pin.
	// Refuse on mismatch BEFORE opening any handle or writing any byte.
	if consent != nil {
		resolvedTarget := filepath.Clean(resolved)
		pinnedTarget := filepath.Clean(consent.PinnedResolvedPath)
		if resolvedTarget != pinnedTarget {
			return fmt.Errorf("secure write: symlink %s now resolves to a target (%s) that does not match the operator-consented target (%s); refusing the write — the symlink may have been repointed after consent",
				originalPath, resolvedTarget, pinnedTarget)
		}
	}

	// Audit: emit a distinct warn event per relax lane so the
	// security-boundary downgrade is visible to log monitoring. The env
	// lane keeps the established client-write-symlink-resolved-via-optin
	// event; the scoped-consent lane emits its own distinct event.
	if followViaEnv {
		if logErr := LogHubMcpEvent("warn", "client-write-symlink-resolved-via-optin", map[string]any{
			"path":     originalPath,
			"resolved": resolved,
			"reason":   "MCPHUB_ALLOW_CLIENT_CONFIG_SYMLINK=1 (operator opt-in; write follows symlink target via pinned parent handle)",
		}); logErr != nil {
			_ = logErr
		}
	} else if consent != nil {
		if logErr := LogHubMcpEvent("warn", "client-write-symlink-resolved-via-scoped-consent", map[string]any{
			"path":     originalPath,
			"resolved": resolved,
			"client":   consent.Client,
			"reason":   "scoped operator consent (per-write; pinned-full-target verified; write follows symlink target via pinned parent handle)",
		}); logErr != nil {
			_ = logErr
		}
	}

	// Handle-pinned write: resolve target's parent is opened ONCE and the
	// hardened owner runs against the held handle/fd. No string re-walk,
	// no second path-based parent open. (AF-1 closure.)
	_, err := secureWriteThroughResolvedParentHandle(resolved, contents, false)
	if err == nil {
		return nil
	}
	if refuse, relaxEligible := clientConfigParentGateRefusalOrRelax(err); !relaxEligible {
		return refuse
	}
	if operatorRequiresSingleUserHome() {
		return fmt.Errorf("%w; strict mode is active (via %s=1, or via persisted supervisor-intent.json strict_mode set by `mcphub strict-mode enable`), so the strict parent-dir gate is enforced (unset that env var or run `mcphub strict-mode disable`, or tighten the parent's DACL to remove the offending principal, to proceed)",
			err, RequireSingleUserHomeEnv)
	}
	if logErr := LogHubMcpEvent("warn", "client-write-unhardened-fallback", map[string]any{
		"path":     resolved,
		"reason":   "default-relax-on-solo-host (symlink-resolved target)",
		"original": originalPath,
		"err":      err.Error(),
	}); logErr != nil {
		_ = logErr
	}
	// Relax retry: same handle-pinned write with the parent-dir gate
	// skipped. The per-file restrictive DACL/mode is still installed on
	// the file handle at create time, so the resolved target stays
	// owner-only regardless of how broad its parent's ACL is.
	if _, rerr := secureWriteThroughResolvedParentHandle(resolved, contents, true); rerr != nil {
		return rerr
	}
	return nil
}

// clientConfigParentGateAllowsDefaultRelax reports whether a client-config
// secure-write parent-gate error is eligible for the default-relax lane. It
// mirrors the state-file lane's stateFileParentGateAllowsDefaultRelax: a
// broadened-but-owner-correct parent (ErrSecureWriteParentInsecure WITHOUT
// ErrWrongOwner) may relax on a solo host; a wrong-owner parent, or any
// non-parent-gate error, may NOT (bug 2026-07-03 — the two lanes are meant to
// be symmetric; the wrong-owner error is WRAPPED inside ErrSecureWriteParentInsecure
// by secure_write_windows.go, so a bare errors.Is(ErrSecureWriteParentInsecure)
// check would wrongly relax it).
func clientConfigParentGateAllowsDefaultRelax(err error) bool {
	return errors.Is(err, ErrSecureWriteParentInsecure) && !errors.Is(err, ErrWrongOwner)
}

// clientWriteRefuseWrongOwnerParent fails a client-config write CLOSED when the
// parent directory is owned by a DIFFERENT account. The default-relax lane
// exists only for a BROADENED-but-owner-correct parent on a solo host; a
// foreign-owned parent is a stronger signal — its owner holds implicit
// WRITE_DAC / WRITE_OWNER and could rewrite the DACL to grant itself
// FILE_DELETE_CHILD, then swap the token-bearing client-config entry the
// external app (Claude Desktop / Codex) loads via an ordinary path read, which
// nothing backstops (unlike mcphub's own inode-anchored state reads). Refused
// regardless of strict mode, mirroring the state-file lane's
// stateFileParentGateAllowsDefaultRelax ErrWrongOwner hard-fail — the two lanes
// are meant to be symmetric (bug 2026-07-03). On a solo-dev host the operator
// owns their config dirs, so this never fires; it only bites a genuinely
// foreign-owned parent, where failing closed is the correct behaviour.
func clientWriteRefuseWrongOwnerParent(err error) error {
	// Operation-neutral wording: this refusal now surfaces from BOTH the write
	// lanes AND the three create/parent-lock lanes (via clientConfigParentGate-
	// RefusalOrRelax), and SecureCreateParentDirForConfigLock can create a lock
	// parent that is not itself a "client config" file (e.g. a global config-lock
	// dir), so the message avoids write-specific / client-config-specific phrasing.
	return fmt.Errorf("%w; the parent directory is owned by a different account — refusing to operate on a foreign-owned directory (default-relax applies only to a broadened but owner-correct parent). Move the config under a directory you own, or correct the parent directory's ownership, to proceed", err)
}

// clientConfigParentGateRefusalOrRelax is the SINGLE owner of the decision every
// client-config lane makes on a non-nil error from a secure create/write
// pipeline. Both write lanes (secureWritePathBased, secureWriteFollowingSymlink)
// AND the three create lanes (secureCreateClientConfigIfMissingWithOperatorOpt,
// SecureCreateClientConfigParentDirWithOperatorOpt, SecureCreateParentDirForConfigLock)
// call it, so the classification cannot drift between them.
//
// Bug 2026-07-08 (F1): the create lanes had NOT adopted this decision — they
// hand-rolled a bare errors.Is(err, ErrSecureWriteParentInsecure) check, so a
// FOREIGN-OWNED parent relaxed (wrote anyway) in the create lanes while the write
// lanes refused it. A duplicated security decision drifted. Collapsing it here
// makes all five lanes provably identical and prevents the next drift.
//
// Returns (refuse, relaxEligible):
//
//   - relaxEligible=true, refuse=nil: a broadened-but-owner-correct parent on a
//     solo host. The caller may fall through to its strict-mode check + the
//     default-relax retry.
//   - relaxEligible=false, refuse!=nil: NOT relax-eligible. refuse is either the
//     wrong-owner-PARENT refusal (ErrWrongOwner wrapped INSIDE
//     ErrSecureWriteParentInsecure — a foreign-owned parent, failed closed) or
//     the raw error verbatim (a bare ErrWrongOwner from a post-rename FILE verify,
//     or any non-parent-gate error — returned as-is so its accurate diagnostic
//     survives instead of being overwritten with a misleading "parent directory"
//     wording, bot/fable review F2).
//
// Wrong-owner detectability depends on the pipelines wrapping their underlying
// owner/DACL-verify error with %w (not %v) via wrapParentGateRefusal below. The
// write pipelines already did; the F1 fix routed the create pipelines through the
// same wrap owner so ErrWrongOwner survives into this classifier on the create
// paths too.
//
// nil err is unreachable in production — every call site guards err != nil first
// (a nil pipeline error is success, handled at the call site). It is treated
// defensively as relax-eligible ("no gate error, nothing to refuse") so the
// documented (relaxEligible=false => refuse!=nil) invariant holds even on misuse.
func clientConfigParentGateRefusalOrRelax(err error) (refuse error, relaxEligible bool) {
	if err == nil {
		return nil, true
	}
	if clientConfigParentGateAllowsDefaultRelax(err) {
		return nil, true
	}
	if errors.Is(err, ErrWrongOwner) && errors.Is(err, ErrSecureWriteParentInsecure) {
		return clientWriteRefuseWrongOwnerParent(err), false
	}
	return err, false
}

// wrapParentGateRefusal is the SINGLE owner of the parent-dir gate refusal error
// shape produced by every secure create/write pipeline (Windows + POSIX legs). It
// wraps BOTH ErrSecureWriteParentInsecure AND the underlying owner/DACL verify
// error with %w, so errors.Is downstream matches either sentinel — critically
// ErrWrongOwner, which clientConfigParentGateRefusalOrRelax keys on to refuse a
// foreign-owned parent.
//
// Bug 2026-07-08 (F1) root cause 1: the create pipelines wrapped verr with %v
// here, FLATTENING ErrWrongOwner out of the chain so a foreign-owned parent
// relaxed. Collapsing all gate-site wraps (both platforms, write + create) into
// this one owner makes a %v regression a single-file review + unit-test target
// instead of scattered ones (see TestWrapParentGateRefusalPreservesSentinels).
func wrapParentGateRefusal(path string, verr error) error {
	return fmt.Errorf("%w (path %s): %w", ErrSecureWriteParentInsecure, path, verr)
}

// secureWritePathBased is the non-symlink standard pipeline: the hardened
// path-based secure-write WITH the parent-dir gate, falling back to the
// gate-skipped relax lane on ErrSecureWriteParentInsecure when strict mode
// is OFF. This is the pre-existing behavior preserved verbatim for the
// common (regular-file) path.
//
// PR #185 r3 (codex deep-sec P1 closure): the relax lane re-runs the SAME
// hardened pipeline with the parent-dir gate bypassed; per-file DACL/mode
// hardening still applies at temp-create time, closing the race window the
// previous os.CreateTemp + path-based SetNamedSecurityInfo path left open.
func secureWritePathBased(path string, contents []byte) error {
	err := SecureWriteClientConfig(path, contents)
	if err == nil {
		return nil
	}
	if refuse, relaxEligible := clientConfigParentGateRefusalOrRelax(err); !relaxEligible {
		return refuse
	}
	if operatorRequiresSingleUserHome() {
		return fmt.Errorf("%w; strict mode is active (via %s=1, or via persisted supervisor-intent.json strict_mode set by `mcphub strict-mode enable`), so the strict parent-dir gate is enforced (unset that env var or run `mcphub strict-mode disable`, or tighten the parent's DACL to remove the offending principal, to proceed)",
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

// operatorAllowsUnhardenedStateWrite reports whether the operator
// explicitly accepts the state-file parent TOCTOU relax lane via
// AllowUnhardenedStateWriteEnv. Accepts "1" and "true"
// case-insensitively; everything else returns false.
func operatorAllowsUnhardenedStateWrite() bool {
	v := strings.TrimSpace(os.Getenv(AllowUnhardenedStateWriteEnv))
	return v == "1" || strings.EqualFold(v, "true")
}

// OperatorAllowsClientConfigSymlink reports whether the operator
// has explicitly opted IN to symlink resolution at client-config
// destination paths via AllowClientConfigSymlinkEnv. Accepts "1"
// and "true" case-insensitively; everything else returns false.
//
// Strict-mode overrides: when MCPHUB_REQUIRE_SINGLE_USER_HOME=1 is
// also set, the strict gate takes precedence — the opt-in is
// ignored and symlinks are refused unconditionally. This keeps the
// strict posture's invariant (multi-tenant / corp-managed hosts
// get hardening regardless of any per-operator env vars). Callers
// MUST check this predicate AFTER strict-mode checks, not before.
//
// Exported so the scan path (internal/api/scan.go) and any GUI
// affordances that surface client-config status can honor the same
// opt-in as the write path. Mirrors the OperatorRequiresSingleUserHome
// export convention.
func OperatorAllowsClientConfigSymlink() bool {
	if operatorRequiresSingleUserHome() {
		return false
	}
	v := strings.TrimSpace(os.Getenv(AllowClientConfigSymlinkEnv))
	return v == "1" || strings.EqualFold(v, "true")
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
//
// SEC-F2 (security false-promise fix): strict mode is now active when
// EITHER input is true:
//
//   - the MCPHUB_REQUIRE_SINGLE_USER_HOME env var is "1"/"true" (read
//     fresh on every call — cheap, so an env override still takes
//     effect immediately within the same process), OR
//   - the persisted supervisor-intent.json `strict_mode` bit is true
//     (the operator ran `mcphub strict-mode enable`).
//
// Before this change the env var was the ONLY enforcement input, so an
// operator who ran `strict-mode enable` on a multi-tenant host believed
// the strict relax-on-rejection-DISABLED gate was active when it was
// NOT — the relax lane still fired. The intent bit was inert. This
// closes that gap so the operator-facing command actually enforces.
//
// Per-process / restart-propagation semantics: the intent value is
// read exactly ONCE per process (lazy cache — strictModeIntentCache).
// This is deliberate: the gate is called on EVERY secure state-file
// write, so re-reading (and flock-contending on) supervisor-intent.json
// per call would be a perf + lock-contention regression. A long-lived
// supervisor therefore picks up a `strict-mode enable` change on its
// NEXT restart — which is the documented propagation point anyway
// (`strict-mode enable` already rewrites the autostart shim args, so a
// supervisor restart is expected). Short-lived install/gui/cli
// invocations read fresh each run. No file-watcher, no per-write read.
//
// Fail-closed-on-anomalous-read, relax-on-legit-absent (pr304 hybrid — intent
// strict is read GATE-FREE): intent-derived strict is TRUE for a present
// strict_mode==true intent OR for an ANOMALOUS read (a read error may hide a
// strict intent); only a legit ABSENT intent and a parsed strict_mode=false
// relax —
//
//   - UNRESOLVABLE state dir (DaemonStateDirReadOnly errors) → TRUE (strict).
//     A path-resolution failure is anomalous (a normal host resolves fine);
//     failing closed keeps an unresolvable resolver from silently disabling a
//     persisted strict intent. The robust strict path remains the env var.
//   - ABSENT intent (missing file, ErrNotExist on a resolvable path) → FALSE
//     (relax). An absent intent declares NO strict_mode, so it must NOT make the
//     operator strict — the canonical CLAUDE.md posture (broadened state dir
//     defaults to RELAX; STRICT is opt-in via the env var). Keeps fresh-install
//     on a broadened host working.
//   - non-ENOENT READ error or JSON parse error on the gate-free read → TRUE
//     (strict). A read/parse fault is anomalous and may hide a strict intent;
//     failing closed prevents a silent strict downgrade.
//   - PRESENT + parseable + strict_mode==true → TRUE (honor the explicit bit);
//     strict_mode==false → FALSE (relax).
//
// The pre-r10 implementation read the bit through ReadSupervisorIntent, whose
// parent-dir WRITE-protection gate rejected on a broadened parent and forced
// strict=TRUE even when strict_mode was FALSE on disk — a live-fleet break on
// solo-dev/corp hosts with a broadened %LOCALAPPDATA% (the fleet's client/state
// writes would be refused). Reading the strict_mode bit gate-free fixes that:
// a broadened dir now correctly yields relax-via-intent unless strict_mode is
// explicitly true. A co-resident attacker on a broadened delete-capable dir who
// tampers/deletes the bit can only force RELAX — the documented best-effort
// limitation whose robust mitigation is MCPHUB_REQUIRE_SINGLE_USER_HOME=1.
func OperatorRequiresSingleUserHome() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(RequireSingleUserHomeEnv))) {
	case "1", "true":
		return true
	}
	// #301-3 strict-mode-mutation deadlock fix: when a strict-mode
	// enable/disable is mid-flight writing the gate-controlling
	// supervisor-intent.json itself, the cached intent.strict_mode value
	// must NOT govern that very write — otherwise an OLD strict_mode=true
	// would refuse the disabling write (and a broadened parent would
	// strand the operator in strict mode forever). During the bounded
	// mutation-write window the gate consults ENV ONLY, skipping the
	// intent cache. The bypass is scoped to the intent write (see
	// BeginStrictModeMutationGateBypass) and is always cleared via defer.
	if strictModeMutationGateBypassActive() {
		return false
	}
	return strictModeFromIntentCached()
}

// strictModeMutationGateBypass is a process-level reentrancy-counted guard.
// While its depth > 0, OperatorRequiresSingleUserHome consults ONLY the
// MCPHUB_REQUIRE_SINGLE_USER_HOME env var and ignores the cached
// supervisor-intent.json strict_mode bit.
//
// It exists for exactly one caller class: the `mcphub strict-mode
// {enable,disable}` mutation, which writes the authoritative
// gate-controlling strict_mode value. That write must not be governed by the
// value it is replacing (SEC-F2 made intent.strict_mode authoritative for ALL
// secure state-file writes, which would otherwise self-gate the disabling
// write on a broadened parent — #301-3). A reentrancy counter (not a bool)
// keeps nested Begin/End pairs correct.
var strictModeMutationGateBypass struct {
	mu    sync.Mutex
	depth int
}

// strictModeMutationGateBypassActive reports whether a strict-mode mutation
// write window is currently open in this process.
func strictModeMutationGateBypassActive() bool {
	strictModeMutationGateBypass.mu.Lock()
	defer strictModeMutationGateBypass.mu.Unlock()
	return strictModeMutationGateBypass.depth > 0
}

// BeginStrictModeMutationGateBypass opens a strict-mode-mutation write window:
// for its duration OperatorRequiresSingleUserHome consults the env var ONLY,
// ignoring the cached supervisor-intent.json strict_mode bit. EVERY call MUST
// be paired with exactly one EndStrictModeMutationGateBypass, always via defer
// so a panic or early return cannot leak the bypass. Reentrant: nested
// Begin/End pairs increment/decrement a depth counter, and the gate stays
// bypassed until the OUTERMOST End.
//
// Scope discipline (#301-3): wrap ONLY the supervisor-intent.json write
// step(s) of `mcphub strict-mode enable|disable`, never a whole command.
// A NON-mutation secure write that happens to run while the window is open
// would also see the bypass, so callers must keep the window as narrow as the
// intent write itself.
func BeginStrictModeMutationGateBypass() {
	strictModeMutationGateBypass.mu.Lock()
	defer strictModeMutationGateBypass.mu.Unlock()
	strictModeMutationGateBypass.depth++
}

// EndStrictModeMutationGateBypass closes one strict-mode-mutation write window
// opened by BeginStrictModeMutationGateBypass. Safe to call when depth is
// already 0 (clamps at 0) so a defensive double-End cannot drive the counter
// negative.
func EndStrictModeMutationGateBypass() {
	strictModeMutationGateBypass.mu.Lock()
	defer strictModeMutationGateBypass.mu.Unlock()
	if strictModeMutationGateBypass.depth > 0 {
		strictModeMutationGateBypass.depth--
	}
}

// WithStrictModeMutationGateBypass runs fn with the strict-mode-mutation gate
// bypass active, guaranteeing the window is closed even if fn panics. This is
// the preferred entry point for the strict-mode CLI's intent-write step.
func WithStrictModeMutationGateBypass(fn func() error) error {
	BeginStrictModeMutationGateBypass()
	defer EndStrictModeMutationGateBypass()
	return fn()
}

// resetStrictModeMutationGateBypassForTest force-clears the bypass depth so a
// leaked Begin (e.g. from a failed test) cannot bleed into later tests in the
// same process. Test-only.
func resetStrictModeMutationGateBypassForTest() {
	strictModeMutationGateBypass.mu.Lock()
	defer strictModeMutationGateBypass.mu.Unlock()
	strictModeMutationGateBypass.depth = 0
}

// strictModeIntentCache holds the lazily-resolved
// supervisor-intent.json `strict_mode` bit. It is resolved exactly once
// per process (the first OperatorRequiresSingleUserHome call whose env
// var was NOT already truthy), then served from the cache on every
// subsequent call. The mutex + `resolved` guard mirrors the established
// internal/api cache convention (cf. serenaStopReadCache /
// resetSerenaStopReadCache) rather than sync.Once specifically so the
// test seam can clear `resolved` and force a fresh read without a
// process restart.
var strictModeIntentCache struct {
	mu       sync.Mutex
	resolved bool
	strict   bool
}

// strictModeFromIntentCached returns the cached
// supervisor-intent.json `strict_mode` value, resolving it on first
// use. The resolution rule (pr304 hybrid): intent-derived strict is TRUE when
// the intent file is PRESENT, gate-free-readable, parses, AND strict_mode==true,
// OR when the read fails in an ANOMALOUS way (unresolvable state dir, non-ENOENT
// read error, parse error → fail closed to STRICT, since an error may hide a
// strict intent). Only a legitimately ABSENT intent (ErrNotExist) and a parsed
// strict_mode=false RELAX. See readStrictModeFromIntentBestEffort for the full
// case split and the gate-free-read rationale.
func strictModeFromIntentCached() bool {
	strictModeIntentCache.mu.Lock()
	defer strictModeIntentCache.mu.Unlock()
	if !strictModeIntentCache.resolved {
		strictModeIntentCache.strict = readStrictModeFromIntentBestEffort()
		strictModeIntentCache.resolved = true
	}
	return strictModeIntentCache.strict
}

// readStrictModeFromIntentBestEffort reads the supervisor-intent.json
// `strict_mode` bit GATE-FREE and returns it. The rule (pr304 hybrid):
// fail-closed-to-STRICT on an ANOMALOUS read, RELAX on a legitimately ABSENT
// intent or a parsed strict_mode=false:
//
//   - DaemonStateDirReadOnly() error (state dir UNRESOLVABLE) → TRUE (strict).
//     A path-resolution failure is anomalous (a normal host resolves the path
//     fine, then reads or ErrNotExist's the file); we cannot read the bit or
//     prove it absent/false, so failing closed keeps an unresolvable resolver
//     from silently disabling a persisted strict intent. (Legit absent strict
//     is the ErrNotExist branch below, on a RESOLVABLE path.)
//
//   - intent file ABSENT (os.ErrNotExist) → FALSE (relax). An ABSENT intent
//     declares NO strict_mode, so it must NOT make the operator strict — the
//     canonical CLAUDE.md "Hardened state-file writes" posture (a broadened
//     state dir defaults to RELAX; STRICT is opt-in via the env var). This keeps
//     fresh-install on a broadened host working; the deletion-of-a-strict-intent
//     bypass is a documented best-effort limitation mitigated by the env var.
//
//   - other read error (non-ENOENT: permission-denied on the file's OWN DACL, a
//     non-regular file, an I/O fault) → TRUE (strict). A read error is anomalous
//     and does not prove the intent absent-or-false; failing closed keeps a read
//     fault from silently disabling a persisted strict intent.
//
//   - JSON parse error → TRUE (strict). A present-but-corrupt intent may be a
//     strict-enabled intent whose body was clobbered; relaxing on corruption
//     would silently downgrade an operator-enabled posture. The posture read is
//     side-effect-free, so this branch does not emit diagnostics.
//
//   - parsed → return intent.StrictMode verbatim (true → strict, false → relax).
//
// (pr304 took pr301 r10's gate-free read and flipped the ANOMALOUS-read cases
// back to fail-closed-strict for the deletion/corruption threat, while KEEPING
// r10's absent→relax so fresh-install-on-broadened and the canon default-relax
// are preserved. The running fleet stays relaxed: a present strict_mode=false
// intent reads gate-free and returns false — verified on the real host.)
//
// WHY GATE-FREE (the pr301 r10 fix): the pre-r10 implementation read the bit
// via ReadSupervisorIntent, which runs the parent-dir WRITE-protection gate
// (checkStateDirParentWriteSafe) BEFORE os.ReadFile (unless
// MCPHUB_ALLOW_UNHARDENED_STATE_READ=1). On a host whose %LOCALAPPDATA% parent
// inherited a broadened write/delete ACE (the common corp-managed AND many
// solo-dev cases), that gate REJECTED, the present-but-unreadable branch fired,
// and the gate verdict became strict=TRUE — so a host with strict_mode=FALSE
// on disk and the env var unset was driven into the STRICT refusal path and
// ALL client/state/overlay writes were refused (a live-fleet break). Reading
// the strict_mode bit through a WRITE-protection gate meant a broadened dir
// could NEVER yield relax-via-intent — it was always strict, contradicting the
// documented default-relax-on-broadened posture. The strict_mode bit is NOT
// write-protected data: reading it gate-free exposes no secrets (secrets live
// in the vault, never the intent), and a tampered/deleted bit is the SAME
// best-effort limitation the env var already exists to cover. The WRITE path's
// parent gate is UNCHANGED — writes are still hardened/gated; only this
// strict_mode READ is gate-free.
//
// Security note: a co-resident attacker on a broadened, delete-capable state
// dir who tampers with or deletes the strict_mode bit can only force RELAX
// (never strict) — exactly the best-effort limitation the robust mitigation
// (MCPHUB_REQUIRE_SINGLE_USER_HOME=1, checked first in
// OperatorRequiresSingleUserHome before this read) is documented to cover in
// CLAUDE.md "Hardened state-file writes".
//
// Caching note: called once per process behind strictModeFromIntentCached.
//
// Honors the daemonStateRootOverride test seam through DaemonStateDirReadOnly
// (read-only resolver: no chmod-heal, no parent-dir sanity check), so tests
// redirect the read into a temp state dir and a posture read never mutates the
// state dir.
func readStrictModeFromIntentBestEffort() bool {
	stateDir, err := DaemonStateDirReadOnly()
	if err != nil {
		// State dir UNRESOLVABLE → fail closed to STRICT (pr304 hybrid). A
		// path-resolution failure is ANOMALOUS, not a legitimate "no strict
		// declared" signal: we cannot read the bit or prove it absent/false,
		// so failing closed keeps an unresolvable resolver from silently
		// disabling a persisted strict intent. (A normal host resolves the
		// path fine; the legit absent-intent case is the ErrNotExist branch
		// below, which still RELAXES.)
		return true
	}
	path := joinStateFilePath(stateDir, supervisorIntentFileLeaf)
	// Absent intent is the fresh-install "no strict_mode declared" case. Check
	// that side-effect-free before the anchored reader so a missing file under a
	// broadened state dir does not emit fallback audit logs or create log files
	// while merely probing operator posture.
	if _, err := os.Lstat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false
		}
		return true
	}
	// Inode-anchored read with ENV-only strict policy: deliberately NOT
	// ReadSupervisorIntent, and deliberately NOT the normal anchored reader's
	// persisted-strict policy. ReadSupervisorIntent runs the parent-dir
	// WRITE-protection gate before reading; the normal anchored reader asks
	// OperatorRequiresSingleUserHome, which would recurse into this very
	// persisted strict-mode resolution. This variant still rejects
	// symlink/reparse swaps and preserves the pr301 r10 fleet-safety behavior:
	// a broadened parent with strict_mode=false on disk relaxes unless the
	// MCPHUB_REQUIRE_SINGLE_USER_HOME env var itself is set.
	raw, err := readStateFileInodeAnchoredEnvStrictOnlyNoAudit(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// ABSENT intent → RELAX (canon-aligned; pr301 r9/r10). An absent
			// supervisor-intent.json declares NO strict_mode, so it must NOT
			// make the operator strict — the canonical CLAUDE.md "Hardened
			// state-file writes" posture (a broadened state dir defaults to
			// RELAX; STRICT is opt-in via MCPHUB_REQUIRE_SINGLE_USER_HOME=1).
			// This keeps fresh-install on a broadened host working and keeps
			// the deletion-of-a-strict-intent bypass a documented best-effort
			// limitation whose robust mitigation is that env var. (pr304
			// proposed absent→strict here; rejected to avoid a fresh-install
			// regression + the canon contradiction.)
			return false
		}
		// Other read error (permission-denied on the file's OWN DACL, a
		// non-regular file, an I/O fault) → fail closed to STRICT (pr304
		// hybrid). A read error is anomalous and does not prove the intent is
		// absent-or-false; failing closed keeps a read fault from silently
		// disabling a persisted strict intent.
		return true
	}
	var f SupervisorIntentFile
	if err := json.Unmarshal(raw, &f); err != nil {
		// Corrupt/unparseable intent → fail closed to STRICT (pr304 hybrid). A
		// present-but-corrupt supervisor-intent.json may be a strict-enabled
		// intent whose body was truncated/clobbered; relaxing on corruption would
		// silently downgrade an operator-enabled strict posture. This posture read
		// is deliberately side-effect-free, so it does not emit diagnostics here.
		return true
	}
	return f.StrictMode
}

// resetStrictModeIntentCacheForTest clears the lazy strict-mode-intent
// cache so a single test process can exercise both intent=true and
// intent=false without a process restart. Mirrors the
// resetSerenaStopReadCache convention. Test-only — callers in
// production never need it because the intent value is fixed for the
// life of the process (see OperatorRequiresSingleUserHome semantics).
func resetStrictModeIntentCacheForTest() {
	strictModeIntentCache.mu.Lock()
	defer strictModeIntentCache.mu.Unlock()
	strictModeIntentCache.resolved = false
	strictModeIntentCache.strict = false
}
