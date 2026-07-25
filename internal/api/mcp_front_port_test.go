// internal/api/mcp_front_port_test.go
//
// Sub-increment 2a: unit coverage for the mcp_front.port resolver. Both
// tests are mutation-proven — see each doc comment for the specific
// reverted line and the resulting failure.
package api

import (
	"os"
	"path/filepath"
	"testing"
)

// TestResolveMCPFrontPort_RoundTripsPersistedValue proves a persisted,
// valid setting round-trips through SettingsSet -> ResolveMCPFrontPort.
// Mutation-proven: hardcoding ResolveMCPFrontPort to always return
// DefaultMCPFrontPort (ignoring the persisted value) makes this test fail
// with "port = 9137, want 9201".
func TestResolveMCPFrontPort_RoundTripsPersistedValue(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("LOCALAPPDATA", tmp)

	a := NewAPI()
	if err := a.SettingsSet(MCPFrontPortSettingKey, "9201"); err != nil {
		t.Fatalf("SettingsSet(%s, 9201): %v", MCPFrontPortSettingKey, err)
	}
	port, err := a.ResolveMCPFrontPort()
	if err != nil {
		t.Fatalf("ResolveMCPFrontPort() error = %v", err)
	}
	if port != 9201 {
		t.Fatalf("port = %d, want 9201", port)
	}
	if got := a.MCPFrontPortOrDefault(); got != 9201 {
		t.Fatalf("MCPFrontPortOrDefault() = %d, want 9201", got)
	}
}

// TestResolveMCPFrontPort_FreshHostUsesRegistryDefault proves a genuinely
// fresh host (no gui-preferences.yaml on disk yet) resolves to
// DefaultMCPFrontPort via the registry-default path (SettingsGet returns
// the registry default without error when nothing is persisted — see
// readRawSettingsMap's os.IsNotExist branch), not an error.
func TestResolveMCPFrontPort_FreshHostUsesRegistryDefault(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("LOCALAPPDATA", tmp)

	a := NewAPI()
	port, err := a.ResolveMCPFrontPort()
	if err != nil {
		t.Fatalf("ResolveMCPFrontPort() on a fresh host: error = %v, want nil (registry default)", err)
	}
	if port != DefaultMCPFrontPort {
		t.Fatalf("port = %d, want DefaultMCPFrontPort (%d)", port, DefaultMCPFrontPort)
	}
}

// TestMCPFrontPortOrDefault_FallsBackOnCorruptSettingsFile proves the
// graceful-fallback accessor degrades to DefaultMCPFrontPort (never 0, never
// a panic) when gui-preferences.yaml exists but is not valid YAML.
// Mutation-proven: changing MCPFrontPortOrDefault's fallback branch to
// `return 0` makes this test fail with "got 0, want 9137"; changing
// ResolveMCPFrontPort to swallow the yaml.Unmarshal error (returning nil
// instead of propagating it) makes TestResolveMCPFrontPort_
// ErrorsOnCorruptSettingsFile below fail because err becomes nil.
func TestMCPFrontPortOrDefault_FallsBackOnCorruptSettingsFile(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("LOCALAPPDATA", tmp)
	corruptPath := filepath.Join(tmp, "mcp-local-hub", "gui-preferences.yaml")
	if err := os.MkdirAll(filepath.Dir(corruptPath), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Not valid YAML (unterminated flow sequence) — yaml.Unmarshal into
	// map[string]string must error.
	if err := os.WriteFile(corruptPath, []byte("mcp_front: [unterminated\n"), 0o600); err != nil {
		t.Fatalf("write corrupt settings file: %v", err)
	}

	a := NewAPI()
	if got := a.MCPFrontPortOrDefault(); got != DefaultMCPFrontPort {
		t.Fatalf("MCPFrontPortOrDefault() = %d, want DefaultMCPFrontPort (%d) on a corrupt settings file", got, DefaultMCPFrontPort)
	}
}

// TestResolveMCPFrontPort_ErrorsOnCorruptSettingsFile is the strict
// counterpart: ResolveMCPFrontPort (the WRITE-path accessor) must propagate
// the failure rather than silently defaulting, so a write-path caller
// (mcphub install --reconcile-mcp-front) refuses instead of reconciling
// against a port the operator never configured.
func TestResolveMCPFrontPort_ErrorsOnCorruptSettingsFile(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("LOCALAPPDATA", tmp)
	corruptPath := filepath.Join(tmp, "mcp-local-hub", "gui-preferences.yaml")
	if err := os.MkdirAll(filepath.Dir(corruptPath), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(corruptPath, []byte("mcp_front: [unterminated\n"), 0o600); err != nil {
		t.Fatalf("write corrupt settings file: %v", err)
	}

	a := NewAPI()
	if _, err := a.ResolveMCPFrontPort(); err == nil {
		t.Fatalf("ResolveMCPFrontPort() error = nil, want a propagated read error on a corrupt settings file")
	}
}
