// internal/gui/client_config_env_isolation_test.go
//
// One owner for "neutralize every client-config path environment variable" in
// package gui tests. Counterparts:
// internal/cli/client_config_env_isolation_test.go and
// internal/api/client_config_env_isolation_test.go — three copies only because a
// _test.go file cannot be shared across packages.
//
// WHY THIS EXISTS. HOME / USERPROFILE / LOCALAPPDATA cover 4 of the 47 adapters
// in clients.AllClients(). Three gui-reachable production surfaces fan out over
// the WHOLE registry and READ each resolved file:
//
//	api.ProbeHubGate              GetEntry on every constructed adapter
//	                              (internal/api/hub_gate_detect.go:56)
//	api.BuildAdoptPlan            extractStdioEntryFromClient over the 9
//	                              adopt-supported clients, paths from
//	                              DefaultScanConfigPaths (internal/api/adopt.go:478)
//	api.ScanFrom(DefaultScanConfigPaths())
//
// So a gui handler test that drives /api/adopt, /api/deadopt or a global project
// scan reads the operator's live client configs unless the full set below is
// redirected. The gui TestMain (main_test.go) installs no home barrier at all —
// it fences the STATE dir, not the client-config roots.
//
// The executable backstop is the client-config sandbox audit
// (internal/clients/config_path_sandbox_audit.go), installed for this package
// from TestMain. This helper is how a fixture SATISFIES the audit; the audit is
// what catches a fixture that forgot.
package gui

import (
	"path/filepath"
	"testing"
)

// neutralizeClientConfigPathEnv points every client-config path environment
// variable this repo's adapters consult at sandboxHome.
//
// It deliberately does NOT set HOME / USERPROFILE / LOCALAPPDATA: those carry
// per-fixture meaning and each caller sets them itself. Callers MUST point them
// at sandboxHome too.
func neutralizeClientConfigPathEnv(t *testing.T, sandboxHome string) {
	t.Helper()
	// Roaming/XDG roots: redirect (not unset) — an unset APPDATA falls back to
	// <home>\AppData\Roaming in several adapters, which is only isolated by
	// accident. XDG_CONFIG_HOME is NOT Linux-only here: crush, goose and opencode
	// consult it on every OS.
	t.Setenv("APPDATA", filepath.Join(sandboxHome, "AppData", "Roaming"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(sandboxHome, ".config"))
	t.Setenv("ProgramData", filepath.Join(sandboxHome, "ProgramData"))
	// mimocode's machine-wide managed layer reads %ProgramData%\opencode unless
	// this override points elsewhere. ProgramData is ALWAYS set on Windows, so
	// this read is unconditional without the redirect.
	t.Setenv("MIMOCODE_TEST_MANAGED_CONFIG_DIR", filepath.Join(sandboxHome, "ProgramData", "opencode"))
	// Explicit-profile overrides resolve with ABSOLUTE precedence and no home
	// fallback. Any exported value points OUTSIDE the sandbox by definition.
	for _, key := range []string{
		"COPILOT_HOME", "KIMI_CODE_HOME",
		"MIMOCODE_HOME", "MIMOCODE_CONFIG", "MIMOCODE_CONFIG_DIR", "MIMOCODE_CONFIG_CONTENT",
	} {
		t.Setenv(key, "")
	}
}

// sandboxClientConfigHome is the whole-sandbox form for a test that owns its own
// state dir: a fresh temp home plus the full neutralization above. It leaves
// LOCALAPPDATA alone, because gui tests routinely point it at a separate state
// root and TestMain already fences that.
func sandboxClientConfigHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	neutralizeClientConfigPathEnv(t, home)
	return home
}
