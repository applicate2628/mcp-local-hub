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
	"sync"

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
func secureWriteWithOperatorOpt(path string, contents []byte) error {
	// MCPHUB_ALLOW_CLIENT_CONFIG_SYMLINK opt-in (post-PR #209
	// reintroduction under explicit operator consent): when set,
	// resolve symlinks at the destination before calling secure-
	// write. The hardened pipeline refuses pre-existing reparse
	// points outright (symlink-attack defense), but a real-world
	// solo-dev pattern is to symlink dotfiles to a separate repo
	// (~/.codex/config.toml -> /e/env/Agents/.codex/config.toml).
	// Without the opt-in, every matrix Apply on such hosts fails
	// with "pre-existing reparse point refused".
	//
	// Strict mode (MCPHUB_REQUIRE_SINGLE_USER_HOME=1) overrides
	// the opt-in inside OperatorAllowsClientConfigSymlink, so
	// multi-tenant / corp-managed hosts get the symlink-attack
	// protection without compromise.
	//
	// Resolution writes through to the symlink's TARGET path.
	// The original symlink is left intact (mcphub does not
	// rewrite it as a regular file). The target's DACL after the
	// write is owner-only via the secure-write pipeline.
	//
	// Audit log: emit a warn event on each opt-in resolve so the
	// security-boundary downgrade is visible to log monitoring
	// (mirrors the unhardened-fallback warn at line 271+).
	if OperatorAllowsClientConfigSymlink() {
		if resolved, was := resolveSymlinkForSecureWrite(path); was {
			if logErr := LogHubMcpEvent("warn", "client-write-symlink-resolved-via-optin", map[string]any{
				"path":     path,
				"resolved": resolved,
				"reason":   "MCPHUB_ALLOW_CLIENT_CONFIG_SYMLINK=1 (operator opt-in; write follows symlink target)",
			}); logErr != nil {
				_ = logErr
			}
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
// Safe-default-on-error: the posture depends on WHY the intent read
// failed (#301-2 refinement, extended by pr301 r4 + r5) —
//
//   - UNRESOLVABLE state dir (DefaultSupervisorIntentPath errors) →
//     cached value TRUE (fail-closed-to-STRICT; pr301 r4 Finding 1). A
//     resolver error may be hiding an existing strict_mode=true intent on
//     a broadened state dir; with no path to os.Lstat, strict is the only
//     fail-secure verdict. (Finding 4: this line previously claimed FALSE,
//     contradicting the r4 code — the comment is now correct.)
//   - ABSENT intent (genuinely missing file) → CONDITIONAL on the state
//     dir's delete-capability (pr301 r5 Finding 1):
//     · state dir NOT delete-capable by a non-allowlisted principal
//     (the read-side write-bits gate PASSES, e.g. a hardened or
//     merely read-broadened solo-dev dir) → cached value FALSE
//     (env-only relax). A fresh install with no enforcing intent yet.
//     · state dir delete-capable by a non-allowlisted principal (the
//     write-bits gate FAILS — FILE_DELETE_CHILD / write / DAC bits)
//     → cached value TRUE (fail-closed-to-STRICT). An absent intent
//     there is indistinguishable from an attacker-DELETED one, so
//     relaxing would turn a deletion into a strict-mode bypass.
//     See absentIntentStrictVerdict for the discriminator.
//   - READ FAILURE on an EXISTING intent (decode error, permission
//     denied, read-side parent-dir gate refusal on a present file) →
//     cached value TRUE (fail-closed-to-STRICT). A read failure on the
//     gate-controlling file must NOT silently disable the security gate;
//     strict is the safe-secure failure mode.
//
// Before #301-2, ANY read error relaxed (fail-open-to-relax), which
// silently disabled the strict gate if the EXISTING intent became
// unreadable. pr301 r4 closed the unresolvable-path hole; pr301 r5 closed
// the deletion-on-broadened-dir hole. Strict is the failure mode for every
// case EXCEPT a genuine absence on a dir an attacker cannot tamper with.
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
// use. The resolution rule is fail-closed-secure: an ABSENT intent on a
// NON-delete-capable state dir caches FALSE (env-only behavior, the
// pre-SEC-F2 posture); an ABSENT intent on a delete-capable broadened dir
// (pr301 r5 Finding 1), an unresolvable state dir (pr301 r4 Finding 1), or
// a READ FAILURE on an EXISTING intent (decode/permission/parent-gate
// refusal) all cache TRUE — see readStrictModeFromIntentBestEffort for the
// full case split.
func strictModeFromIntentCached() bool {
	strictModeIntentCache.mu.Lock()
	defer strictModeIntentCache.mu.Unlock()
	if !strictModeIntentCache.resolved {
		strictModeIntentCache.strict = readStrictModeFromIntentBestEffort()
		strictModeIntentCache.resolved = true
	}
	return strictModeIntentCache.strict
}

// readStrictModeFromIntentBestEffort reads supervisor-intent.json once
// and returns its `strict_mode` bit. It distinguishes a GENUINELY ABSENT
// intent on a tamper-safe dir (fresh install — relax is correct) from an
// absent intent on a delete-capable dir (indistinguishable from an
// attacker deletion — strict) and from a READ FAILURE on an existing
// intent (decode/permission/parent-gate refusal — strict is the safe-
// secure failure mode). #301-2 P2 + pr301 r4/r5:
//
//   - DefaultSupervisorIntentPath() error (state dir UNRESOLVABLE) →
//     TRUE (fail-closed strict; pr301 r4 Finding 1). A path-resolution
//     error is NOT proof of a fresh/absent intent: the resolver can refuse
//     an EXISTING, already-broadened state dir (POSIX rejects a state dir
//     whose parent is insecure / non-owner; Windows rejects an
//     unresolvable known-folder), and that same dir may hold a
//     supervisor-intent.json with strict_mode=true that this gate exists to
//     honor. Relaxing on a resolver error would silently DISABLE the gate
//     whenever the error happens to coincide with a strict intent we can no
//     longer reach — a fail-OPEN hole. We cannot os.Lstat to disambiguate
//     (we have no path), so the only safe verdict is strict. This is the
//     env-only DISABLE path's concern only by name: strict-mode DISABLE
//     does NOT consult this read (it gates on the env var, not the intent),
//     so failing closed here cannot deadlock a disable.
//   - ReadSupervisorIntent returns os.ErrNotExist (file absent) →
//     absentIntentStrictVerdict(path) (pr301 r5 Finding 1): relax (false)
//     when the state dir is NOT delete-capable by a non-allowlisted
//     principal (genuine fresh install), strict (true) when it IS (an
//     absent intent there is indistinguishable from an attacker deletion,
//     which would otherwise be a strict-mode bypass). This branch is also
//     reachable for a TRUE deletion when MCPHUB_ALLOW_UNHARDENED_STATE_READ
//     skips the read-side gate, so gating it (not only the os.Lstat branch)
//     is required.
//   - ReadSupervisorIntent returns ANY OTHER error (decode failure,
//     permission denied, read-side parent-dir gate refusal) → first
//     DISAMBIGUATE absent-vs-unreadable with a gate-free os.Lstat probe
//     (pr301 r3 Finding 2). ReadSupervisorIntent runs the read-side
//     parent-DACL gate BEFORE os.ReadFile, so on a Windows host whose
//     state dir inherited a non-allowlisted write ACE a genuinely ABSENT
//     intent surfaces as a parent-gate error, NOT os.ErrNotExist. os.Lstat
//     does NOT run the read-side parent gate, so it disambiguates: a path
//     os.Lstat reports as not-existing is genuinely ABSENT →
//     absentIntentStrictVerdict(path) (NOT an unconditional relax — pr301
//     r5 Finding 1: a read-only-broadened dir relaxes, a delete-capable one
//     fails closed); a path os.Lstat reports as EXISTING (regular file,
//     symlink, anything) is present-but-unreadable → fail CLOSED to TRUE,
//     because silently relaxing when an attacker or a corp ACL change made
//     the gate-controlling file unreadable would be a fail-OPEN hole.
//     os.Lstat (not os.Stat) is deliberate: an attacker-planted symlink
//     whose target is missing must read as PRESENT (the entry exists), not
//     be laundered into the absent branch.
//   - intent == nil (no error, no file content) → false.
//
// Caching note: this is called once per process behind
// strictModeFromIntentCached, so a transient read error caches STRICT
// for the process lifetime. That is the intended fail-closed-secure
// posture — the next process retries the read.
//
// Calls api.DefaultSupervisorIntentPath + api.ReadSupervisorIntent
// (same package). Honors the daemonStateRootOverride test seam through
// DefaultSupervisorIntentPath, so tests redirect the read into a temp
// state dir.
func readStrictModeFromIntentBestEffort() bool {
	path, err := DefaultSupervisorIntentPath()
	if err != nil {
		// State dir UNRESOLVABLE. pr301 r4 Finding 1: fail CLOSED to strict.
		// The resolver can REFUSE an EXISTING (already-broadened) state dir
		// that holds a gate-controlling supervisor-intent.json with
		// strict_mode=true; relaxing on the resolver error would silently
		// disable the gate. With no path we cannot os.Lstat to disambiguate
		// absent-vs-present, so strict is the only fail-secure verdict.
		return true
	}
	intent, err := ReadSupervisorIntent(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Missing file. A MISSING intent is EITHER a fresh install before
			// any strict-mode enable (relax is correct) OR an attacker DELETED
			// the gate-controlling intent on a broadened, delete-capable state
			// dir (pr301 r5 Finding 1: relaxing on that absence turns the
			// deletion into a strict-mode BYPASS). The verdict is decided by the
			// state dir's own delete-capability — see absentIntentStrictVerdict.
			//
			// This branch is also reachable on a delete-capable broadened dir
			// when MCPHUB_ALLOW_UNHARDENED_STATE_READ=1 skips the read-side gate
			// in ReadSupervisorIntent: os.ReadFile then surfaces a TRUE
			// os.ErrNotExist for a genuinely-deleted file, so gating this branch
			// (not only the os.Lstat branch below) is required (advisory REFINE).
			return absentIntentStrictVerdict(path)
		}
		// Non-ENOENT read error. ReadSupervisorIntent runs the read-side
		// parent-DACL gate BEFORE os.ReadFile, so on a broadened-parent host
		// a genuinely absent intent surfaces here as a parent-gate error
		// rather than os.ErrNotExist (pr301 r3 Finding 2). Disambiguate with
		// a gate-free os.Lstat probe: if the path truly does not exist, the
		// intent is ABSENT — but ABSENT does NOT unconditionally relax (pr301
		// r5 Finding 1): on a delete-capable broadened dir an absent file is
		// indistinguishable from an attacker-deleted one, so the verdict is
		// decided by absentIntentStrictVerdict. os.Lstat (not os.Stat) keeps an
		// attacker-planted dangling symlink classified as PRESENT.
		if _, statErr := os.Lstat(path); errors.Is(statErr, os.ErrNotExist) {
			return absentIntentStrictVerdict(path)
		}
		// The intent file EXISTS but could not be read (decode/permission/
		// parent-gate refusal on a present file): fail CLOSED to strict. A
		// read failure on an existing gate-controlling file must not silently
		// disable the gate.
		return true
	}
	if intent == nil {
		return false
	}
	return intent.StrictMode
}

// absentIntentStrictVerdict decides the strict-mode verdict for a GENUINELY
// ABSENT supervisor-intent.json (pr301 r5 Finding 1). It returns true (strict)
// when the state dir grants delete/write capability to a non-allowlisted
// principal, and false (relax) otherwise.
//
// Rationale: strict mode exists to protect a BROADENED / write-capable state
// dir. On such a dir, a co-resident non-allowlisted principal holding
// FILE_DELETE_CHILD (Windows) or group/world write (POSIX) can DELETE the
// gate-controlling supervisor-intent.json. If an absent intent always relaxed,
// that deletion would silently flip strict→relax — a strict-mode BYPASS the bot
// flagged. We cannot distinguish "fresh install, never wrote an intent" from
// "attacker deleted the intent" by the absence alone, so on a delete-capable
// dir the only fail-secure verdict is strict.
//
// Conversely, a dir that is merely READ-broadened (e.g. Authenticated Users
// GENERIC_READ — the common solo-dev %LOCALAPPDATA% case inheriting Codex /
// AppContainer ACEs) does NOT permit a non-allowlisted principal to delete the
// intent, so an absent intent there is a genuine fresh install → relax. This
// preserves the documented missing-intent → default-relax polarity for the
// benign read-broadened case (pr301 r3 Finding 2) while closing the
// deletion-bypass hole for the delete-capable case.
//
// The discriminator is checkStateDirParentWriteSafe(filepath.Dir(path)) — the
// SAME write-capability predicate ReadSupervisorIntent already runs as its
// read-side gate (it fails only on FILE_DELETE_CHILD / DELETE / WRITE_DAC /
// WRITE_OWNER / write bits, per windowsDACLWriteOrAdminBits; POSIX rejects
// 0o022). A gate ERROR (predicate non-nil) is treated as delete-capable → fail
// closed to strict, because an unverifiable parent-dir posture is itself a
// reason not to trust an absence on it.
//
// Operators who legitimately run on a delete-capable broadened dir and want
// relax must tighten the parent DACL; the documented corp-policy posture
// (CLAUDE.md "Hardened state-file writes") already says such hosts SHOULD be in
// strict mode (and MCPHUB_REQUIRE_SINGLE_USER_HOME=1 forces it regardless).
func absentIntentStrictVerdict(path string) bool {
	if err := checkStateDirParentWriteSafe(filepath.Dir(path)); err != nil {
		// Delete-capable (or unverifiable) state dir: an absent intent cannot be
		// distinguished from an attacker-deleted one → fail closed to strict.
		return true
	}
	// State dir is NOT delete-capable by a non-allowlisted principal: the absent
	// intent is a genuine fresh install → relax (env-only behavior).
	return false
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
