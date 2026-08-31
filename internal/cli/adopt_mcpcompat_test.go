package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
	"mcp-local-hub/internal/config"
	"mcp-local-hub/internal/daemon"
)

func TestAdoptCLIProfilePersistsAndBuildsLegacyHost(t *testing.T) {
	root, home := adoptTestHome(t)
	const entry = "codegraph"
	manifestRoot := cliAdoptDefaultManifestDir(t)
	t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", manifestRoot)
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Join(manifestRoot, entry)) })
	stateRoot := apitest.HardenedTempDir(t)
	t.Cleanup(api.SetDaemonStateRootForTest(stateRoot))
	canonical := filepath.Join(root, "bin", api.MCPHubBinaryName())
	if err := os.MkdirAll(filepath.Dir(canonical), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(canonical, []byte("test mcphub"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(api.SetTestCanonicalMcphubPath(canonical))
	codexPath := filepath.Join(home, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(codexPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(codexPath, []byte("[mcp_servers.codegraph]\ncommand = \"go\"\nargs = [\"version\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := newAdoptCmdWithDeps(api.NewAPI, nil, func(*api.API, *api.AdoptPlan, *api.AdoptProvenanceRecord) error { return nil })
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{entry, "--client", "codex-cli", "--port", "9371", "--mcp-protocol-compatibility-profile", "stdio-http-legacy-2024-11-05", "--yes"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("adopt: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "stdio-http-legacy-2024-11-05") {
		t.Fatalf("adopt result does not surface profile:\n%s", out.String())
	}
	record, found, err := api.ReadAdoptProvenance(entry)
	if err != nil || !found || record.MCPProtocolCompatibilityProfile != "stdio-http-legacy-2024-11-05" {
		t.Fatalf("persisted record=%#v found=%v err=%v", record, found, err)
	}
	b, err := os.ReadFile(filepath.Join(manifestRoot, entry, "manifest.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	m, err := config.ParseManifest(strings.NewReader(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := daemon.NewStdioHost(daemon.HostConfig{Command: "go", MCPProtocolCompatibilityProfile: m.Daemons[0].MCPProtocolCompatibilityProfile}); err != nil {
		t.Fatalf("generated daemon profile did not reach HostConfig: %v", err)
	}
}
