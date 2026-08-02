// internal/gui/client_config_env_isolation_test.go
//
// Package-local facade for the clients-owned client-config sandbox descriptor.
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
	"testing"

	"mcp-local-hub/internal/clients"
)

// neutralizeClientConfigPathEnv points every client-config path environment
// variable this repo's adapters consult at sandboxHome.
//
// It redirects every adapter path input, including HOME, USERPROFILE, and
// LOCALAPPDATA, below sandboxHome.
func neutralizeClientConfigPathEnv(t *testing.T, sandboxHome string) {
	t.Helper()
	t.Cleanup(clients.ApplyClientConfigSandboxEnvironment(sandboxHome))
}

// sandboxClientConfigHome is the whole-sandbox form for a test that owns its own
// state dir: a fresh temp home plus the full neutralization above. It leaves
// LOCALAPPDATA alone, because gui tests routinely point it at a separate state
// root and TestMain already fences that.
func sandboxClientConfigHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	neutralizeClientConfigPathEnv(t, home)
	return home
}
