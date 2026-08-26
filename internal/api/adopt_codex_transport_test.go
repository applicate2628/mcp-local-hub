package api

import (
	"os"
	"path/filepath"
	"testing"

	"mcp-local-hub/internal/clients"
)

func TestBuildAdoptPlanCodexCollisionFreezesTargetEntryName(t *testing.T) {
	const entry = "codex-adopt-transport"
	codexPath, _, _ := setupAdoptTestEnv(t, entry, `[mcp_servers.codex-adopt-transport]
command = "go"
args = ["version"]
`)

	projectRoot := t.TempDir()
	projectConfig := filepath.Join(projectRoot, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(projectConfig), 0o700); err != nil {
		t.Fatalf("mkdir project Codex config: %v", err)
	}
	projectBytes := []byte(`[mcp_servers.codex-adopt-transport]
url = "http://127.0.0.1:9999/mcp"
`)
	if err := os.WriteFile(projectConfig, projectBytes, 0o600); err != nil {
		t.Fatalf("write project Codex config: %v", err)
	}
	beforeProject, err := os.ReadFile(projectConfig)
	if err != nil {
		t.Fatalf("read project Codex config before plan: %v", err)
	}

	plan, err := NewAPI().BuildAdoptPlan(AdoptOpts{
		EntryName:        entry,
		Client:           "codex-cli",
		ManifestName:     entry,
		Port:             9371,
		CodexProjectRoot: projectRoot,
		CodexWorkingDir:  projectRoot,
	})
	if err != nil {
		t.Fatalf("BuildAdoptPlan: %v", err)
	}
	if got := plan.TargetEntryNames["codex-cli"]; got != entry+"-mcphub" {
		t.Fatalf("Codex target entry = %q, want %q", got, entry+"-mcphub")
	}
	if plan.EntryName != entry {
		t.Fatalf("logical source entry = %q, want %q", plan.EntryName, entry)
	}
	afterProject, err := os.ReadFile(projectConfig)
	if err != nil {
		t.Fatalf("read project Codex config after plan: %v", err)
	}
	if string(afterProject) != string(beforeProject) {
		t.Fatalf("project Codex config changed during plan\nbefore:\n%s\nafter:\n%s", beforeProject, afterProject)
	}
	if _, err := os.Stat(codexPath); err != nil {
		t.Fatalf("global Codex config disappeared during plan: %v", err)
	}
}

func TestExecuteAndDeAdoptCodexCollisionUsesFrozenTargetEntry(t *testing.T) {
	const entry = "codex-adopt-roundtrip"
	codexPath, _, _ := setupAdoptTestEnv(t, entry, `[mcp_servers.codex-adopt-roundtrip]
command = "go"
args = ["version"]
`)
	projectRoot := t.TempDir()
	projectConfig := filepath.Join(projectRoot, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(projectConfig), 0o700); err != nil {
		t.Fatalf("mkdir project Codex config: %v", err)
	}
	projectBytes := []byte(`[mcp_servers.codex-adopt-roundtrip]
url = "http://127.0.0.1:9999/mcp"
`)
	if err := os.WriteFile(projectConfig, projectBytes, 0o600); err != nil {
		t.Fatalf("write project Codex config: %v", err)
	}

	api := NewAPI()
	plan, err := api.BuildAdoptPlan(AdoptOpts{
		EntryName:        entry,
		Client:           "codex-cli",
		ManifestName:     entry,
		Port:             9372,
		CodexProjectRoot: projectRoot,
		CodexWorkingDir:  projectRoot,
	})
	if err != nil {
		t.Fatalf("BuildAdoptPlan: %v", err)
	}
	if err := api.ExecuteAdopt(plan, nil); err != nil {
		t.Fatalf("ExecuteAdopt: %v", err)
	}
	alias := entry + "-mcphub"
	global, err := clients.AllClients()["codex-cli"].GetEntry(alias)
	if err != nil || global == nil {
		t.Fatalf("read relocated Codex target: entry=%#v err=%v", global, err)
	}
	if global.URL != "http://127.0.0.1:9372/mcp" {
		t.Fatalf("relocated URL = %q", global.URL)
	}
	if old, err := clients.AllClients()["codex-cli"].GetEntry(entry); err != nil || old != nil {
		t.Fatalf("logical source remains after relocation: entry=%#v err=%v", old, err)
	}
	rec, found, err := ReadAdoptProvenance(entry)
	if err != nil || !found {
		t.Fatalf("ReadAdoptProvenance: found=%t err=%v", found, err)
	}
	if got := rec.Clients[0].TargetEntryName; got != alias {
		t.Fatalf("persisted target_entry_name = %q, want %q", got, alias)
	}
	repeat, err := api.BuildAdoptPlan(AdoptOpts{
		EntryName:        entry,
		Client:           "codex-cli",
		ManifestName:     entry,
		Port:             9372,
		CodexProjectRoot: projectRoot,
		CodexWorkingDir:  projectRoot,
	})
	if err != nil {
		t.Fatalf("repeat BuildAdoptPlan: %v", err)
	}
	if err := api.ExecuteAdopt(repeat, nil); err != nil {
		t.Fatalf("repeat ExecuteAdopt: %v", err)
	}
	if _, err := api.ExecuteDeAdopt(entry, nil); err != nil {
		t.Fatalf("ExecuteDeAdopt: %v", err)
	}
	if restored, err := clients.AllClients()["codex-cli"].GetEntry(entry); err != nil || restored == nil {
		t.Fatalf("source was not restored: entry=%#v err=%v", restored, err)
	}
	if removed, err := clients.AllClients()["codex-cli"].GetEntry(alias); err != nil || removed != nil {
		t.Fatalf("target alias remains after de-adopt: entry=%#v err=%v", removed, err)
	}
	afterProject, err := os.ReadFile(projectConfig)
	if err != nil {
		t.Fatalf("read project Codex config after de-adopt: %v", err)
	}
	if string(afterProject) != string(projectBytes) {
		t.Fatalf("project Codex config changed\nbefore:\n%s\nafter:\n%s", projectBytes, afterProject)
	}
	if _, err := os.Stat(codexPath); err != nil {
		t.Fatalf("global Codex config disappeared: %v", err)
	}
}
