// internal/cli/client_config_env_isolation_test.go
//
// Package-local facade for the clients-owned client-config sandbox descriptor.
//
// WHY THIS EXISTS. A test that drives a REAL client-config reconcile enumerates
// clients.AllClients(), and each adapter resolves its config path from the
// environment. Redirecting HOME / USERPROFILE / LOCALAPPDATA covers only SOME of
// them. The rest resolve from env vars a fixture that lists them by hand
// inevitably misses — and a missed one is not a flake, it is a WRITE to the
// developer's live config:
//
//	%APPDATA%          vscode, cline, roo, kilocode, devin, amp, zed, ...
//	$XDG_CONFIG_HOME   opencode, devin, roo, mimocode (global dir)
//	$MIMOCODE_HOME     mimocode global dir (wins over XDG_CONFIG_HOME)
//	$MIMOCODE_CONFIG / _CONFIG_DIR / _CONFIG_CONTENT
//	                   mimocode extra read layers (a FILE, an overlay DIR, and an
//	                   inline JSONC string)
//	$MIMOCODE_TEST_MANAGED_CONFIG_DIR / %ProgramData%
//	                   mimocode's admin-deployed managed layer
//	$COPILOT_HOME, $KIMI_CODE_HOME
//	                   copilot-cli / kimi-code roots
//
// This was not hypothetical. `mcpFrontPR588Env` redirected HOME/USERPROFILE/
// LOCALAPPDATA but not %APPDATA%, so `TestMCPFront*` admitted the REAL vscode
// adapter at %APPDATA%\Code\User\mcp.json and the forward reconcile rewrote the
// developer's live VS Code MCP config to point at the test's ephemeral port
// (plus a .bak-mcp-local-hub-<ts> pair per run).
//
// The descriptor redirects every adapter-path input under the fixture root or
// unsets its explicit override; adding an adapter input changes that descriptor
// once for API, CLI, and GUI tests.
package cli

import (
	"testing"

	"mcp-local-hub/internal/clients"
)

// neutralizeClientConfigPathEnv points every client-config path environment
// variable this repo's adapters consult at sandboxHome, so a test that
// enumerates clients.AllClients() can only ever see sandbox configs.
//
// It does not set MIMOCODE_DISABLE_CLAUDE_CODE_MCP: that gate changes MiMoCode
// read-layer semantics rather than selecting a path. A fixture that needs the
// single-layer shape sets it explicitly after calling this helper.
func neutralizeClientConfigPathEnv(t *testing.T, sandboxHome string) {
	t.Helper()
	t.Cleanup(clients.ApplyClientConfigSandboxEnvironment(sandboxHome))
}
