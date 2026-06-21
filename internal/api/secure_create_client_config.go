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
// client_write_init.go, which returns the strict error in strict mode
// (mapped to INIT_FAILED) and relaxes (parent gate skipped) otherwise.
// So this function IS reached in strict mode, and it refuses on
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

// secureCreateClientConfigIfMissingSkipParentGate is the relax-lane
// sibling — runs the same hardened pipeline with the parent-dir gate
// BYPASSED. Used by secureCreateClientConfigIfMissingWithOperatorOpt
// when the operator (implicitly via default-relax, or explicitly via
// MCPHUB_ALLOW_UNHARDENED_CLIENT_WRITE) opted out of the strict
// parent gate. Per-file allowlist-DACL hardening at create time
// still applies, so the new stub is owner-only regardless of
// parent broadening.
func secureCreateClientConfigIfMissingSkipParentGate(path string, contents []byte) (created bool, err error) {
	return secureCreateClientConfigIfMissingImpl(path, contents, true /*skipParentGate*/)
}
