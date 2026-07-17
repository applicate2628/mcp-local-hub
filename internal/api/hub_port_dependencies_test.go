package api

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func setupHubPortDependenciesTest(t *testing.T) string {
	t.Helper()
	root := hardenedTempDir(t)
	restore := SetDaemonStateRootForTest(root)
	t.Cleanup(restore)
	return hermeticHome(t)
}

func seedGatedClaudeCode(t *testing.T, home string) {
	t.Helper()
	path := filepath.Join(home, ".claude.json")
	body := `{"mcpServers":{"mcphub-hub":{"url":"http://127.0.0.1:3439/clients/claude-code/mcp","type":"http"}}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("seed gate-ON claude-code config: %v", err)
	}
}

func seedMalformedClaudeCode(t *testing.T, home string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte("{"), 0o600); err != nil {
		t.Fatalf("seed malformed claude-code config: %v", err)
	}
}

func seedGroupForHubPortDependencies(t *testing.T, name string) {
	t.Helper()
	if err := WriteGroups(GroupsConfig{
		Version: 1,
		Groups:  []Group{{Name: name, Servers: []string{"memory"}}},
	}); err != nil {
		t.Fatalf("seed groups.yaml: %v", err)
	}
}

func seedMalformedGroupsForHubPortDependencies(t *testing.T) {
	t.Helper()
	if err := writeHubMcpStateFile(hubMcpGroupsFileLeaf, []byte("version: 2\n")); err != nil {
		t.Fatalf("seed malformed groups.yaml: %v", err)
	}
}

func hasHubPortDependencyError(deps HubPortDependencies, kind, name string) bool {
	return slices.ContainsFunc(deps.Errors, func(source HubPortDependencySource) bool {
		return source.Kind == kind && source.Name == name
	})
}

func TestProbeHubPortDependenciesClear(t *testing.T) {
	setupHubPortDependenciesTest(t)

	deps := ProbeHubPortDependencies()
	if deps.State != DependencyStateClear {
		t.Fatalf("State = %v, want clear", deps.State)
	}
	if len(deps.GatedClients) != 0 || len(deps.Groups) != 0 || len(deps.Errors) != 0 {
		t.Fatalf("dependencies = %+v, want all slices empty", deps)
	}
}

func TestProbeHubPortDependenciesDependentByClient(t *testing.T) {
	home := setupHubPortDependenciesTest(t)
	seedGatedClaudeCode(t, home)

	deps := ProbeHubPortDependencies()
	if deps.State != DependencyStateDependent {
		t.Fatalf("State = %v, want dependent", deps.State)
	}
	if !slices.Contains(deps.GatedClients, "claude-code") {
		t.Fatalf("GatedClients = %v, want claude-code", deps.GatedClients)
	}
	if len(deps.Errors) != 0 {
		t.Fatalf("Errors = %+v, want empty", deps.Errors)
	}
}

func TestProbeHubPortDependenciesDependentByGroup(t *testing.T) {
	setupHubPortDependenciesTest(t)
	seedGroupForHubPortDependencies(t, "frontend")

	deps := ProbeHubPortDependencies()
	if deps.State != DependencyStateDependent {
		t.Fatalf("State = %v, want dependent", deps.State)
	}
	if !slices.Contains(deps.Groups, "frontend") {
		t.Fatalf("Groups = %v, want frontend", deps.Groups)
	}
	if len(deps.Errors) != 0 {
		t.Fatalf("Errors = %+v, want empty", deps.Errors)
	}
}

func TestProbeHubPortDependenciesUnknownByClientError(t *testing.T) {
	home := setupHubPortDependenciesTest(t)
	seedMalformedClaudeCode(t, home)

	deps := ProbeHubPortDependencies()
	if deps.State != DependencyStateUnknown {
		t.Fatalf("State = %v, want unknown", deps.State)
	}
	if !hasHubPortDependencyError(deps, "client", "claude-code") {
		t.Fatalf("Errors = %+v, want unreadable claude-code source", deps.Errors)
	}
}

func TestProbeHubPortDependenciesUnknownByGroupError(t *testing.T) {
	setupHubPortDependenciesTest(t)
	seedMalformedGroupsForHubPortDependencies(t)

	deps := ProbeHubPortDependencies()
	if deps.State != DependencyStateUnknown {
		t.Fatalf("State = %v, want unknown", deps.State)
	}
	if !hasHubPortDependencyError(deps, "groups", "groups.yaml") {
		t.Fatalf("Errors = %+v, want unreadable groups.yaml source", deps.Errors)
	}
}

func TestProbeHubPortDependenciesUnknownPrecedesDependent(t *testing.T) {
	home := setupHubPortDependenciesTest(t)
	seedGatedClaudeCode(t, home)
	seedMalformedGroupsForHubPortDependencies(t)

	deps := ProbeHubPortDependencies()
	if deps.State != DependencyStateUnknown {
		t.Fatalf("State = %v, want unknown", deps.State)
	}
	if !slices.Contains(deps.GatedClients, "claude-code") {
		t.Fatalf("GatedClients = %v, want claude-code retained for the operator message", deps.GatedClients)
	}
	if !hasHubPortDependencyError(deps, "groups", "groups.yaml") {
		t.Fatalf("Errors = %+v, want unreadable groups.yaml source", deps.Errors)
	}
}
