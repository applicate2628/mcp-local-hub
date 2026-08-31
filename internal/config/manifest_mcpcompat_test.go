package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDaemonProtocolCompatibilityProfileValidation(t *testing.T) {
	base := func(transport string, profile string) *ServerManifest {
		return &ServerManifest{
			Name:      "compat-test",
			Kind:      KindGlobal,
			Transport: transport,
			Command:   "compat-server",
			Daemons: []DaemonSpec{{
				Name:                            "default",
				Port:                            19130,
				MCPProtocolCompatibilityProfile: profile,
			}},
		}
	}

	for _, tt := range []struct {
		name      string
		transport string
		profile   string
		wantErr   string
	}{
		{name: "absent remains valid", transport: TransportStdioBridge},
		{name: "legacy profile is stdio only", transport: TransportStdioBridge, profile: "stdio-http-legacy-2024-11-05"},
		{name: "unknown profile", transport: TransportStdioBridge, profile: "unknown", wantErr: "mcp_protocol_compatibility_profile"},
		{name: "native http rejects profile", transport: TransportNativeHTTP, profile: "stdio-http-legacy-2024-11-05", wantErr: "stdio-bridge"},
		{name: "remote http rejects profile", transport: TransportRemoteHTTP, profile: "stdio-http-legacy-2024-11-05", wantErr: "stdio-bridge"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := base(tt.transport, tt.profile).Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestCodeGraphDiskManifestFixtureUsesLegacyStdioProfile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "codegraph", "manifest.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	const fixture = `name: codegraph
kind: global
transport: stdio-bridge
command: codegraph-mcp
daemons:
  - name: default
    port: 19131
    mcp_protocol_compatibility_profile: stdio-http-legacy-2024-11-05
`
	if err := os.WriteFile(path, []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	m, err := ParseManifest(strings.NewReader(string(b)))
	if err != nil {
		t.Fatalf("ParseManifest disk fixture: %v", err)
	}
	if got := m.Daemons[0].MCPProtocolCompatibilityProfile; got != "stdio-http-legacy-2024-11-05" {
		t.Fatalf("daemon profile = %q", got)
	}
}

func TestLLDBManifestDefaultUsesLegacyStdioProfile(t *testing.T) {
	b, err := os.ReadFile("../../servers/lldb/manifest.yaml")
	if err != nil {
		t.Fatal(err)
	}
	m, err := ParseManifest(strings.NewReader(string(b)))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if len(m.Daemons) != 1 {
		t.Fatalf("LLDB daemon count = %d, want 1", len(m.Daemons))
	}
	if got := m.Daemons[0].MCPProtocolCompatibilityProfile; got != "stdio-http-legacy-2024-11-05" {
		t.Fatalf("LLDB default compatibility profile = %q", got)
	}
}
