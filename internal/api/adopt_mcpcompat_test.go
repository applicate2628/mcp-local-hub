package api

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/internal/config"
)

func TestAdoptCodeGraphLegacyProfilePersistsAndRenders(t *testing.T) {
	const entry = "codegraph"
	_, manifestRoot, _ := setupAdoptTestEnv(t, entry, `[mcp_servers.codegraph]
command = "go"
args = ["version"]
`)

	plan, err := NewAPI().BuildAdoptPlan(AdoptOpts{
		EntryName:                       entry,
		Client:                          "codex-cli",
		ManifestName:                    entry,
		Port:                            9368,
		MCPProtocolCompatibilityProfile: "stdio-http-legacy-2024-11-05",
	})
	if err != nil {
		t.Fatalf("BuildAdoptPlan: %v", err)
	}
	if got := plan.MCPProtocolCompatibilityProfile; got != "stdio-http-legacy-2024-11-05" {
		t.Fatalf("plan profile = %q", got)
	}
	var dryRun bytes.Buffer
	PrintAdoptPlan(&dryRun, plan)
	if !strings.Contains(dryRun.String(), "stdio-http-legacy-2024-11-05") {
		t.Fatalf("dry-run does not surface profile:\n%s", dryRun.String())
	}

	result, err := NewAPI().ExecuteAdoptResultWithOpts(plan, &bytes.Buffer{}, ExecuteAdoptOpts{
		ReceivingVerifier: func(*API, *AdoptPlan, *AdoptProvenanceRecord) error { return nil },
	})
	if err != nil {
		t.Fatalf("ExecuteAdoptWithOpts: %v", err)
	}
	if got := result.MCPProtocolCompatibilityProfile; got != "stdio-http-legacy-2024-11-05" {
		t.Fatalf("execution result profile = %q", got)
	}
	record, found, err := ReadAdoptProvenance(entry)
	if err != nil || !found {
		t.Fatalf("ReadAdoptProvenance: record=%#v found=%v err=%v", record, found, err)
	}
	if got := record.MCPProtocolCompatibilityProfile; got != "stdio-http-legacy-2024-11-05" {
		t.Fatalf("persisted profile = %q", got)
	}
	manifest, err := os.ReadFile(filepath.Join(manifestRoot, entry, "manifest.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := config.ParseManifest(strings.NewReader(string(manifest)))
	if err != nil {
		t.Fatal(err)
	}
	if got := parsed.Daemons[0].MCPProtocolCompatibilityProfile; got != "stdio-http-legacy-2024-11-05" {
		t.Fatalf("generated daemon profile = %q", got)
	}
}

func TestAdoptUnknownLegacyProfileFailsBeforeWrites(t *testing.T) {
	const entry = "unknown-profile"
	codexPath, manifestRoot, stateRoot := setupAdoptTestEnv(t, entry, `[mcp_servers.unknown-profile]
command = "go"
args = ["version"]
`)
	before, err := os.ReadFile(codexPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewAPI().BuildAdoptPlan(AdoptOpts{
		EntryName:                       entry,
		Client:                          "codex-cli",
		ManifestName:                    entry,
		Port:                            9369,
		MCPProtocolCompatibilityProfile: "unknown",
	})
	if err == nil || !strings.Contains(err.Error(), "unknown MCP protocol compatibility profile") {
		t.Fatalf("BuildAdoptPlan error = %v", err)
	}
	assertAdoptPlanMutationFree(t, codexPath, before, manifestRoot, stateRoot, entry)
}

func TestAdoptLegacyProfileDoesNotPermitHTTPSource(t *testing.T) {
	const entry = "http-profile"
	codexPath, manifestRoot, stateRoot := setupAdoptTestEnv(t, entry, `[mcp_servers.http-profile]
url = "http://127.0.0.1:9999/mcp"
`)
	before, err := os.ReadFile(codexPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewAPI().BuildAdoptPlan(AdoptOpts{
		EntryName:                       entry,
		Client:                          "codex-cli",
		ManifestName:                    entry,
		Port:                            9372,
		MCPProtocolCompatibilityProfile: "stdio-http-legacy-2024-11-05",
	})
	if !errors.Is(err, ErrClientEntryNotStdio) {
		t.Fatalf("BuildAdoptPlan error = %v, want ErrClientEntryNotStdio", err)
	}
	assertAdoptPlanMutationFree(t, codexPath, before, manifestRoot, stateRoot, entry)
}

func TestAdoptWithoutLegacyProfileRemainsStrict(t *testing.T) {
	const entry = "strict-profile"
	_, _, _ = setupAdoptTestEnv(t, entry, `[mcp_servers.strict-profile]
command = "go"
args = ["version"]
`)
	plan, err := NewAPI().BuildAdoptPlan(AdoptOpts{EntryName: entry, Client: "codex-cli", ManifestName: entry, Port: 9370})
	if err != nil {
		t.Fatal(err)
	}
	if plan.MCPProtocolCompatibilityProfile != "" || strings.Contains(plan.ManifestYAML, "mcp_protocol_compatibility_profile") {
		t.Fatalf("flagless adopt changed strict manifest:\n%s", plan.ManifestYAML)
	}
}
