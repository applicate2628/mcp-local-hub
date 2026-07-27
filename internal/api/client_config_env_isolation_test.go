// internal/api/client_config_env_isolation_test.go
//
// One owner for "neutralize every client-config path environment variable" in
// package api tests. The internal/cli counterpart is
// internal/cli/client_config_env_isolation_test.go; the two are separate only
// because a _test.go file cannot be shared across packages.
//
// WHY THIS EXISTS. Redirecting HOME / USERPROFILE / LOCALAPPDATA covers just 4 of
// the 47 adapters in clients.AllClients(). The rest resolve from %APPDATA%,
// $XDG_CONFIG_HOME, %ProgramData%, $COPILOT_HOME, $KIMI_CODE_HOME or the
// $MIMOCODE_* set, and any api test that reaches a scan / install / cleanup /
// adopt / gate-detect path enumerates the whole registry. Two internal/cli
// fixtures already reached the operator's real configs this way — one of them
// write-capable — so this is not a hypothetical.
//
// The executable backstop is the client-config sandbox audit
// (internal/clients/config_path_sandbox_audit.go), installed for this package
// from TestMain (main_test.go). This helper is how a fixture SATISFIES the audit;
// the audit is what catches a fixture that forgot.
//
// Adding a client adapter that reads a new path env var means adding it HERE,
// once — and the audit will name it for you if you forget.
package api

import (
	"path/filepath"
	"testing"
)

// neutralizeClientConfigPathEnv points every client-config path environment
// variable this repo's adapters consult at sandboxHome, so a test that
// enumerates clients.AllClients() can only ever see sandbox configs.
//
// It deliberately does NOT set HOME / USERPROFILE / LOCALAPPDATA: those carry
// per-fixture meaning (state-dir redirect, seeded client files, canonical binary
// path) and each caller sets them itself. Callers MUST point them at sandboxHome
// too — hermeticHome below is the ready-made form for a fixture with no special
// layout needs.
func neutralizeClientConfigPathEnv(t *testing.T, sandboxHome string) {
	t.Helper()
	// Roaming/XDG roots: redirect (not unset). An unset APPDATA falls back to
	// <home>\AppData\Roaming in several adapters, which is only isolated by
	// accident — and only when HOME is redirected too.
	t.Setenv("APPDATA", filepath.Join(sandboxHome, "AppData", "Roaming"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(sandboxHome, ".config"))
	t.Setenv("ProgramData", filepath.Join(sandboxHome, "ProgramData"))
	// mimocode's machine-wide managed layer reads %ProgramData%\opencode unless
	// this override points elsewhere; redirect it so the walk reads an empty dir
	// rather than a real MDM-deployed config.
	t.Setenv("MIMOCODE_TEST_MANAGED_CONFIG_DIR", filepath.Join(sandboxHome, "ProgramData", "opencode"))
	// Explicit-profile overrides resolve with ABSOLUTE precedence and no home
	// fallback. Any value the developer has exported points OUTSIDE the sandbox by
	// definition, so clear them.
	for _, key := range []string{
		"COPILOT_HOME", "KIMI_CODE_HOME",
		"MIMOCODE_HOME", "MIMOCODE_CONFIG", "MIMOCODE_CONFIG_DIR", "MIMOCODE_CONFIG_CONTENT",
	} {
		t.Setenv(key, "")
	}
}

// sandboxClientConfigHome is the form for a fixture that already owns its temp
// dir: it points HOME/USERPROFILE at sandboxHome and neutralizes the rest of the
// set. It replaces the `t.Setenv("HOME", tmp); t.Setenv("USERPROFILE", tmp)` pair
// that was copy-pasted into ~44 tests across migrate_test.go, demigrate_test.go
// and demigrate_legacy_test.go — every one of which then admitted the other 43
// adapters against the operator's real roots.
//
// It deliberately does NOT touch LOCALAPPDATA: several of those fixtures point it
// at a separate state root on purpose.
func sandboxClientConfigHome(t *testing.T, sandboxHome string) {
	t.Helper()
	t.Setenv("HOME", sandboxHome)
	t.Setenv("USERPROFILE", sandboxHome)
	neutralizeClientConfigPathEnv(t, sandboxHome)
}

// hermeticHome is the whole-sandbox form: a fresh temp dir installed as the home
// root plus the full non-home neutralization above. Use it unless the fixture
// needs to own its own directory layout, in which case set HOME/USERPROFILE/
// LOCALAPPDATA yourself and call neutralizeClientConfigPathEnv with that root.
//
// LOCALAPPDATA is set belt-and-suspenders per the repo STATE SAFETY rule even
// though the api TestMain already fences DaemonStateDir.
func hermeticHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("LOCALAPPDATA", filepath.Join(home, "AppData", "Local"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	neutralizeClientConfigPathEnv(t, home)
	return home
}
