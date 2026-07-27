// internal/cli/client_config_env_isolation_test.go
//
// One owner for "neutralize every client-config path environment variable" in
// package cli tests.
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
// So the rule is: neutralize the whole set in ONE place, and let each fixture
// pick only its own HOME/USERPROFILE/LOCALAPPDATA policy. Adding a client
// adapter that reads a new path env var means adding it HERE, once.
package cli

import (
	"path/filepath"
	"testing"
)

// neutralizeClientConfigPathEnv points every client-config path environment
// variable this repo's adapters consult at sandboxHome, so a test that
// enumerates clients.AllClients() can only ever see sandbox configs.
//
// It deliberately does NOT set HOME / USERPROFILE / LOCALAPPDATA: those carry
// per-fixture meaning (state-dir redirect, settings path, seeded client files)
// and each caller sets them itself. Callers must point them at sandboxHome too.
//
// It also deliberately does NOT set MIMOCODE_DISABLE_CLAUDE_CODE_MCP. That gate
// switches OFF mimocode's ~/.claude.json import layer, and a fixture that
// silences it stops exercising mimocode's multi-layer read/write split — the
// exact contract the mcp-front reconcile has to get right. A caller that wants
// the single-layer shape sets it explicitly after calling this.
func neutralizeClientConfigPathEnv(t *testing.T, sandboxHome string) {
	t.Helper()
	// Roaming/XDG roots: redirect (not unset) — an unset APPDATA falls back to
	// <home>\AppData\Roaming in several adapters, which is only isolated by
	// accident.
	t.Setenv("APPDATA", filepath.Join(sandboxHome, "AppData", "Roaming"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(sandboxHome, ".config"))
	t.Setenv("ProgramData", filepath.Join(sandboxHome, "ProgramData"))
	t.Setenv("MIMOCODE_TEST_MANAGED_CONFIG_DIR", filepath.Join(sandboxHome, "ProgramData", "opencode"))
	// Explicit-profile overrides: clear them. Any value the developer has
	// exported points OUTSIDE the sandbox by definition.
	for _, key := range []string{
		"COPILOT_HOME", "KIMI_CODE_HOME",
		"MIMOCODE_HOME", "MIMOCODE_CONFIG", "MIMOCODE_CONFIG_DIR", "MIMOCODE_CONFIG_CONTENT",
	} {
		t.Setenv(key, "")
	}
}
