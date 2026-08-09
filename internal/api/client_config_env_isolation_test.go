// internal/api/client_config_env_isolation_test.go
//
// Package-local facade for the clients-owned client-config sandbox descriptor.
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
// Adding a client adapter path input means updating the clients descriptor once;
// this facade and its CLI/GUI counterparts consume it unchanged.
package api

import (
	"testing"

	"mcp-local-hub/internal/clients"
)

// neutralizeClientConfigPathEnv points every client-config path environment
// variable this repo's adapters consult at sandboxHome, so a test that
// enumerates clients.AllClients() can only ever see sandbox configs.
//
// It redirects every adapter path input, including HOME, USERPROFILE, and
// LOCALAPPDATA, below sandboxHome.
func neutralizeClientConfigPathEnv(t *testing.T, sandboxHome string) {
	t.Helper()
	t.Cleanup(clients.ApplyClientConfigSandboxEnvironment(sandboxHome))
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
	neutralizeClientConfigPathEnv(t, home)
	return home
}
