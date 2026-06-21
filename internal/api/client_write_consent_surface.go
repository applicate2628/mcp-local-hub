// client_write_consent_surface.go — the cross-package EXPOSURE surface for
// A3's scoped symlink-consent write. It does NOT add a new write path: it
// EXPORTS the seam over the existing unexported choke point
// (secureWriteWithOperatorOptConsent, client_write_init.go) so the GUI
// (internal/gui) and CLI (internal/cli) — separate packages — can drive the
// guided symlink-consent UX (A3 PR-2). Three things live here:
//
//   - ResolveClientConfigSymlink: the RESOLVE half (resolve + pin the parent
//     the operator will be shown), a thin wrapper over the unexported
//     resolveSymlinkForSecureWrite.
//   - SecureWriteClientConfigWithConsent: the WRITE half (perform the
//     consent write through the EXISTING consent-aware entry, so the
//     write-time re-resolve + pinned-parent guard
//     (secureWriteFollowingSymlink) and the strict-mode override apply
//     unchanged).
//   - InteractiveSymlinkConsent: a nil-by-default, injected-from-above
//     consent port (same shape as SupervisorIPCStatusFn in health.go). The
//     CLI sets it for interactive install/reconcile so a symlinked
//     destination triggers a [y/N] prompt at the single write choke point;
//     production default nil = no prompt = existing refusal stands
//     (automation never gets silently redirected).
//
// SINGLE-OWNER of the pin derivation: the pinned PARENT the operator
// consents to is computed here as filepath.Clean(filepath.Dir(resolvedTarget)),
// which MUST byte-match the write-time pin the existing guard recomputes at
// secureWriteFollowingSymlink (filepath.Clean(filepath.Dir(resolved))). The
// RESOLVE facade returns that pinned parent directly so the GUI/CLI consumers
// never re-implement the Dir+Clean derivation — keeping "the pin the operator
// SAW == the pin VERIFIED at write time" structurally true.
//
// Architect SEAM-C decision (A3 PR-2), recorded in the work-item design.

package api

import "path/filepath"

// ResolveClientConfigSymlink reports whether path is a follow-able symlink at
// the moment of the call and, if so, the resolved TARGET file path plus the
// pinned PARENT directory the operator should be shown and will consent to.
//
//   - isSymlink == false: path is a regular file, does not exist, or could
//     not be resolved to a follow-able target. resolvedTarget == path and
//     pinnedParent == "" — there is nothing to consent to; the caller should
//     take its ordinary non-symlink write path.
//   - isSymlink == true: resolvedTarget is the real file the symlink points
//     at; pinnedParent is filepath.Clean(filepath.Dir(resolvedTarget)) — the
//     value to surface to the operator AND to carry into
//     ResolvedSymlinkConsent.PinnedResolvedPath for the write.
//
// This is a thin exported wrapper over the unexported
// resolveSymlinkForSecureWrite (client_write_init.go); the write pipeline
// re-resolves independently at write time, so a swap between this resolve and
// the write is caught by the pin guard, NOT silently followed.
func ResolveClientConfigSymlink(path string) (resolvedTarget, pinnedParent string, isSymlink bool) {
	resolved, was := resolveSymlinkForSecureWrite(path)
	if !was {
		return path, "", false
	}
	return resolved, filepath.Clean(filepath.Dir(resolved)), true
}

// SecureWriteClientConfigWithConsent performs the scoped-consent client-config
// write through the EXISTING consent-aware choke point
// (secureWriteWithOperatorOptConsent). The write-time re-resolve + pinned-parent
// guard and the strict-mode override (followViaConsent gate) apply unchanged:
//
//   - If the symlink was repointed between the operator's confirm and this
//     write, the freshly-resolved parent will not equal
//     consent.PinnedResolvedPath and the write is REFUSED.
//   - If strict mode (MCPHUB_REQUIRE_SINGLE_USER_HOME=1, or the persisted
//     supervisor-intent.json strict_mode bit) is active, the consent is
//     ignored and the symlink-follow is refused unconditionally.
//
// consent.OriginalPath is the symlink path the operator pointed at (the write
// destination passed to the pipeline). consent.PinnedResolvedPath is the value
// returned as pinnedParent by ResolveClientConfigSymlink at confirm time.
func SecureWriteClientConfigWithConsent(consent ResolvedSymlinkConsent, contents []byte) error {
	return secureWriteWithOperatorOptConsent(consent.OriginalPath, contents, &consent)
}

// InteractiveSymlinkConsent is a process-level, nil-by-default consent port
// injected FROM ABOVE (the CLI command), not read ambiently by lower layers.
// It mirrors the SupervisorIPCStatusFn seam in health.go: a package-api
// function var, nil in production, set+restored by the owner via defer, and
// consulted at a single coarse boundary.
//
// The consent-aware writer (secureWriteWithOperatorOptConsent) consults it for
// a client-config path that:
//
//   - IS a symlink, AND
//   - has NO env opt-in (MCPHUB_ALLOW_CLIENT_CONFIG_SYMLINK unset), AND
//   - carries NO scoped ResolvedSymlinkConsent already, AND
//   - is NOT under strict mode (the SAME !operatorRequiresSingleUserHome()
//     gate as followViaConsent — a scoped-consent-via-hook can NEVER bypass
//     strict), AND
//   - the hook is non-nil.
//
// On true the writer builds a fresh ResolvedSymlinkConsent pinned to the
// just-resolved parent and follows the symlink via the same handle-pinned,
// pin-verified path as an explicit scoped consent. On false (or nil hook) the
// write falls through to the ordinary path-based pipeline, where the
// pre-existing-symlink refusal stands.
//
// Ownership: the CLI install/reconcile command SETS this (printing the [y/N]
// prompt with the pinned real target) only when stdin is an interactive
// terminal, and CLEARS it via defer. Automation (non-TTY) leaves it nil — the
// existing refusal is preserved so a non-interactive run is never silently
// redirected to a symlink target.
//
// Arguments: client (diagnostic), originalPath (the symlink the operator
// pointed at), pinnedParent (filepath.Clean(filepath.Dir(resolvedTarget)), the
// real target's parent the operator is being asked to approve).
var InteractiveSymlinkConsent func(client, originalPath, pinnedParent string) bool
