// internal/api/secure_create_client_config.go
//
// SecureCreateClientConfigIfMissing is the production-grade
// init-time atomic create-new helper used by the v0.4.5 Servers
// matrix "Initialize" affordance (PR #208). It mirrors the security
// posture of SecureWriteClientConfig (handle-relative, allowlist-
// only DACL at create time, parent-dir gate, post-create re-verify)
// but uses **no-replace** rename semantics so a regular file that
// arrived at the destination in the publish-race window — whether
// from a legitimate concurrent writer or an attacker-planted
// symlink — is honored as the winner rather than clobbered.
//
// Returns (created, err):
//
//   - created=true, err=nil: this call wrote the stub bytes via the
//     hardened handle-relative pipeline.
//   - created=false, err=nil: a regular file was already present at
//     the destination (race-won by another writer, or pre-existing
//     for a second Initialize click). Idempotent success.
//   - created=false, err!=nil: refusal (symlink, junction, non-
//     regular entry) or a hard pipeline failure. The caller maps
//     this to 500 INIT_FAILED at the /api/init-client-config layer
//     OR to errParentMissing if the failure is `os.IsNotExist`.
//
// Cross-package wiring: `internal/api.init()` swaps
// `clients.CreateConfigFileIfMissing` to this function so adapter
// `InitEmpty()` calls inherit the hardened pipeline in production
// while in-package adapter tests keep using the fallback temp+
// hardlink pattern (which is safe on POSIX where mode 0o600 is the
// security boundary, and acceptable for tests on Windows where the
// tmpfs ACL isn't a concern).
//
// Strict mode interaction (PR #208 deep-sec Lane C #2 closure):
// when MCPHUB_REQUIRE_SINGLE_USER_HOME (or the persisted
// supervisor-intent.json strict_mode bit) enables strict mode, the
// parent-dir DACL/mode gate is enforced; a broadened parent returns
// ErrSecureWriteParentInsecure.
//
// The strict gate for the GUI Init affordance is enforced by the
// CROSS-PACKAGE WRAPPER, NOT by an endpoint short-circuit. The
// /api/init-client-config endpoint does NOT pre-check strict mode (the
// short-circuit was REMOVED in the PR #208 codex-r1 F1 closure — see
// internal/gui/init_client_config.go). Instead this function's
// ErrSecureWriteParentInsecure flows up through the
// secureCreateClientConfigIfMissingWithOperatorOpt wrapper in
// client_write_init.go, which routes it through the shared
// clientConfigParentGateRefusalOrRelax classifier: a WRONG-OWNER
// (foreign-owned) parent is REFUSED regardless of strict mode (bug
// 2026-07-08 F1 — the wrong-owner error is wrapped inside
// ErrSecureWriteParentInsecure and must not relax); a broadened-but-
// owner-correct parent returns the strict error in strict mode (mapped
// to INIT_FAILED) and relaxes (parent gate skipped) otherwise. So this
// function IS reached in strict mode, and it refuses on
// `ErrSecureWriteParentInsecure` regardless — that refusal is the
// load-bearing strict enforcement (and also blocks any future caller
// from using it as a strict-mode bypass), not a redundant defense
// behind a non-existent short-circuit.
package api

// SecureCreateClientConfigIfMissing is the exported entry point.
// The OS-specific implementation lives in secure_create_client_config_{windows,posix}.go.
// Runs with the parent-dir DACL/mode gate ENFORCED.
func SecureCreateClientConfigIfMissing(path string, contents []byte) (created bool, err error) {
	return secureCreateClientConfigIfMissingImpl(path, contents, false /*skipParentGate*/)
}

// SecureCreateOwnerOnlyFile is the generic owner-only secure-create
// primitive: it creates a NEW file at `path` with a PROTECTED
// allowlist-only DACL installed on the handle at NtCreateFile time
// (Windows) / O_CREAT|O_EXCL|O_NOFOLLOW mode 0600 (POSIX), writes
// `contents` into that owner-only handle (the bytes never touch an
// inheriting file), then publishes it via a no-replace atomic rename.
//
// The parent-dir DACL/mode gate is BYPASSED (this is the relax-lane
// sibling of SecureCreateClientConfigIfMissing). The new file is
// therefore owner-only REGARDLESS of how broad the parent directory's
// ACL is, and a broadened parent does NOT block the create — the same
// posture the vault READ-hardening path uses (relax the parent, keep
// the file owner-only), so a caller on a sandbox-broadened
// %LOCALAPPDATA% still writes an owner-only file.
//
// Semantics are create-if-missing:
//
//   - created=true,  err=nil: this call wrote `contents` via the
//     hardened owner-only pipeline.
//   - created=false, err=nil: a regular file already existed at `path`
//     (idempotent success; `contents` were NOT written). A caller that
//     needs a guaranteed-fresh file (e.g. a random-named temp) must
//     treat this as "name taken — retry with a new name", never as a
//     write to an owner-only file.
//   - created=false, err!=nil: refusal (symlink / non-regular entry at
//     the destination) or a hard pipeline failure.
//
// This primitive is NOT client-config-specific: it does no JSON/schema
// validation of `contents`. Both the client-config init-stub relax
// lane (secureCreateClientConfigIfMissingWithOperatorOpt) and the
// `mcphub secrets edit` decrypted-vault temp create call it, so the
// owner-only-create logic has a single owner.
func SecureCreateOwnerOnlyFile(path string, contents []byte) (created bool, err error) {
	return secureCreateClientConfigIfMissingImpl(path, contents, true /*skipParentGate*/)
}
