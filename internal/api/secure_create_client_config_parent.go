// internal/api/secure_create_client_config_parent.go
//
// SecureCreateClientConfigParentDir is the G17 secure parent-directory
// creator for the Servers-matrix "Initialize" affordance on a clean
// install. It lets the operator pre-configure a client whose config
// PARENT directory does not exist yet (e.g. ~/.cursor/ on a host where
// Cursor is not installed) by creating the missing parent securely as
// part of init — NEVER a blind os.MkdirAll.
//
// Why this exists (history): PR #208 deliberately REMOVED MkdirAll from
// the init stub path (EnsureClientConfigStub / fallbackWriteConfigFile)
// to close a symlink-TOCTOU / parent-planting window — a blind MkdirAll
// could re-create a parent that an attacker had primed, or follow an
// attacker-planted symlink and create directories at an attacker-chosen
// location. This function re-opens that surface DELIBERATELY but safely:
// it creates the missing parent chain COMPONENT-BY-COMPONENT, refusing
// to follow any symlink / reparse-point, and refuses any path outside
// the user home (blast-radius bound). It mirrors the security posture of
// SecureCreateClientConfigIfMissing.
//
// Security contract (enforced by the OS-specific impls in
// secure_create_client_config_parent_{posix,windows}.go):
//
//   - HOME CONTAINMENT: every component created must lie at or below the
//     user home directory. If the target parent is not under the home,
//     the function REFUSES (returns an error) and creates nothing. The
//     home itself is the trusted anchor (it always exists); only missing
//     descendants of it are created.
//   - COMPONENT-BY-COMPONENT: walking down from the home anchor, each
//     component is either (a) created fresh via a handle/fd-relative
//     mkdir that fails if the name already exists, or (b) already present
//     as a REAL directory (re-opened with O_NOFOLLOW / reparse-refusal).
//     A component that exists as a symlink / reparse-point / non-dir is
//     REFUSED — the walk never follows it. There is no os.MkdirAll
//     anywhere in the path.
//   - HANDLE-RELATIVE descent: each child operation (mkdirat / re-open)
//     is anchored to the parent's open fd/handle, not a fresh path
//     re-walk from root, so a component swapped between the create and
//     the descend cannot redirect the next mkdir (TOCTOU-safe).
//   - OWNER-ONLY mode/DACL: created directories are owner-only (POSIX
//     mode 0700 via mkdirat; Windows allowlist DACL {current-user,
//     LocalSystem, BuiltinAdministrators} installed at create time).
//   - STRICT-MODE: when skipParentGate is false (strict gate enforced),
//     the home anchor's DACL/mode is verified before any creation, and
//     a broadened anchor returns ErrSecureWriteParentInsecure. The
//     cross-package wrapper (secureCreateClientConfigParentDirWithOperatorOpt
//     in client_write_init.go) relaxes this on solo-dev hosts exactly
//     like the file create, and refuses in strict mode.
//
// The function is IDEMPOTENT: if the target parent already exists as a
// real directory, it returns nil without creating anything (and without
// re-verifying every existing ancestor — the existing chain is the
// operator's, the same trust model as the file create's parent-dir gate
// which only checks the immediate parent).
package api

// SecureCreateClientConfigParentDir securely creates the immediate
// parent directory of `configPath` (and any missing ancestors UNDER the
// user home) if it does not already exist. Runs with the parent-dir
// DACL/mode gate ENFORCED on the home anchor.
//
// Returns nil when the parent already exists as a real directory
// (idempotent) or was successfully created. Returns a non-nil error on
// any refusal (path outside home, symlink/reparse-point in the chain,
// non-dir component, strict-mode broadened anchor) or hard failure. The
// caller (realClientInitializer.Init) maps the error to the appropriate
// HTTP status.
func SecureCreateClientConfigParentDir(configPath string) error {
	return secureCreateClientConfigParentDirImpl(configPath, false /*skipParentGate*/)
}

// secureCreateClientConfigParentDirSkipParentGate is the relax-lane
// sibling — same component-by-component, symlink-refusing,
// home-bounded create but with the home-anchor DACL/mode gate BYPASSED.
// Used by secureCreateClientConfigParentDirWithOperatorOpt when the
// operator (implicitly via default-relax, or explicitly via
// MCPHUB_ALLOW_UNHARDENED_CLIENT_WRITE) opted out of the strict anchor
// gate. Created directories are STILL owner-only and the symlink /
// home-containment refusals STILL apply, so a broadened home cannot
// make the new directories readable by other principals.
func secureCreateClientConfigParentDirSkipParentGate(configPath string) error {
	return secureCreateClientConfigParentDirImpl(configPath, true /*skipParentGate*/)
}
