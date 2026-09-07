package api

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"mcp-local-hub/internal/config"
)

func TestManagedGraphifyAdoptPreservesExactRecipeAndDistinctNamedIdentities(t *testing.T) {
	const (
		firstEntry  = "graphify-project-a"
		secondEntry = "graphify-project-b"
	)
	_, _, _ = setupAdoptTestEnv(t, firstEntry, `[mcp_servers.graphify-project-a]
command = "graphify-mcp"
args = ["<graph-a-json>"]

[mcp_servers.graphify-project-a.env]
RECIPE_PROFILE_MARKER = "profile-a"

[mcp_servers.graphify-project-b]
command = "graphify-mcp"
args = ["<graph-b-json>"]

[mcp_servers.graphify-project-b.env]
RECIPE_PROFILE_MARKER = "profile-b"
`)

	used := collectUsedAdoptPorts()
	firstPort := nextBindableAdoptPortForTest(t, used)
	used[firstPort] = true
	secondPort := nextBindableAdoptPortForTest(t, used)
	api := NewAPI()
	first, err := api.BuildAdoptPlan(AdoptOpts{
		EntryName: firstEntry, Client: "codex-cli", ManifestName: firstEntry,
		Port: firstPort, Clients: []string{"codex-cli"},
	})
	if err != nil {
		t.Fatalf("BuildAdoptPlan(%s): %v", firstEntry, err)
	}
	second, err := api.BuildAdoptPlan(AdoptOpts{
		EntryName: secondEntry, Client: "codex-cli", ManifestName: secondEntry,
		Port: secondPort, Clients: []string{"codex-cli"},
	})
	if err != nil {
		t.Fatalf("BuildAdoptPlan(%s): %v", secondEntry, err)
	}

	assertManagedRecipeManifest(t, first.ManifestYAML, firstEntry, "graphify-mcp",
		[]string{"<graph-a-json>"}, map[string]string{"RECIPE_PROFILE_MARKER": "profile-a"})
	assertManagedRecipeManifest(t, second.ManifestYAML, secondEntry, "graphify-mcp",
		[]string{"<graph-b-json>"}, map[string]string{"RECIPE_PROFILE_MARKER": "profile-b"})
	if first.ManifestName == second.ManifestName || first.ManifestYAML == second.ManifestYAML {
		t.Fatalf("distinct graph identities collapsed: first=%q second=%q", first.ManifestName, second.ManifestName)
	}
}

func TestManagedScholarAdoptPreservesExactRecipeAndRepeatIsByteStable(t *testing.T) {
	const entry = "scholar-search-default"
	codexPath, manifestRoot, stateRoot := setupAdoptTestEnv(t, entry, `[mcp_servers.scholar-search-default]
command = "python"
args = ["-m", "scholar_search_mcp"]

[mcp_servers.scholar-search-default.env]
SCHOLAR_SEARCH_ENABLE_SEMANTIC_SCHOLAR = "true"
SCHOLAR_SEARCH_ENABLE_ARXIV = "true"
`)
	wantEnv := map[string]string{
		"SCHOLAR_SEARCH_ENABLE_SEMANTIC_SCHOLAR": "true",
		"SCHOLAR_SEARCH_ENABLE_ARXIV":            "true",
	}
	port := nextBindableAdoptPortForTest(t, collectUsedAdoptPorts())
	opts := AdoptOpts{
		EntryName: entry, Client: "codex-cli", ManifestName: entry,
		Port: port, Clients: []string{"codex-cli"},
	}
	api := NewAPI()
	plan, err := api.BuildAdoptPlan(opts)
	if err != nil {
		t.Fatalf("BuildAdoptPlan initial: %v", err)
	}
	assertManagedRecipeManifest(t, plan.ManifestYAML, entry, "python",
		[]string{"-m", "scholar_search_mcp"}, wantEnv)
	if err := api.ExecuteAdopt(plan, &bytes.Buffer{}); err != nil {
		t.Fatalf("ExecuteAdopt initial: %v", err)
	}

	repeat, err := api.BuildAdoptPlan(opts)
	if err != nil {
		t.Fatalf("BuildAdoptPlan repeat: %v", err)
	}
	before := snapshotAdoptA2(t, repeat, codexPath, manifestRoot, stateRoot)
	var out bytes.Buffer
	if err := api.ExecuteAdopt(repeat, &out); err != nil {
		t.Fatalf("ExecuteAdopt repeat: %v", err)
	}
	if !strings.Contains(out.String(), "Already adopted logical source") {
		t.Fatalf("repeat output = %q, want Already adopted", out.String())
	}
	after := snapshotAdoptA2(t, repeat, codexPath, manifestRoot, stateRoot)
	assertAdoptA2SnapshotEqual(t, before, after)
}

func TestManagedGraphifyAdoptRefusesDisabledSourceWithoutMutation(t *testing.T) {
	const entry = "graphify-disabled-source"
	codexPath, manifestRoot, stateRoot := setupAdoptTestEnv(t, entry, `[mcp_servers.graphify-disabled-source]
command = "graphify-mcp"
args = ["<graph-json>"]
disabled = true
`)
	before, err := os.ReadFile(codexPath)
	if err != nil {
		t.Fatalf("snapshot disabled source: %v", err)
	}
	_, err = NewAPI().BuildAdoptPlan(AdoptOpts{
		EntryName: entry, Client: "codex-cli", ManifestName: entry,
		Port: nextBindableAdoptPortForTest(t, collectUsedAdoptPorts()),
	})
	if err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("disabled source error = %v, want explicit refusal", err)
	}
	assertAdoptPlanMutationFree(t, codexPath, before, manifestRoot, stateRoot, entry)
}

func TestManagedScholarAdoptRollbackRestoresFirstClientAfterSecondWriteFailure(t *testing.T) {
	const entry = "scholar-search-rollback"
	codexPath, manifestRoot, _ := setupAdoptTestEnv(t, entry, `[mcp_servers.scholar-search-rollback]
command = "python"
args = ["-m", "scholar_search_mcp"]

[mcp_servers.scholar-search-rollback.env]
SCHOLAR_SEARCH_ENABLE_SEMANTIC_SCHOLAR = "true"
SCHOLAR_SEARCH_ENABLE_ARXIV = "true"
`)
	home := filepath.Dir(filepath.Dir(codexPath))
	claudePath := filepath.Join(home, ".claude.json")
	originalClaude := []byte(`{"mcpServers":{"scholar-search-rollback":{"type":"stdio","command":"python","args":["-m","scholar_search_mcp"],"env":{"SCHOLAR_SEARCH_ENABLE_SEMANTIC_SCHOLAR":"true","SCHOLAR_SEARCH_ENABLE_ARXIV":"true"},"disabled":false},"keep":{"type":"http","url":"https://example.invalid/mcp","disabled":true}}}`)
	if err := os.WriteFile(claudePath, originalClaude, 0o600); err != nil {
		t.Fatalf("seed claude config: %v", err)
	}
	failClientConfigWritesForAdoptTest(t, codexPath)

	plan, err := NewAPI().BuildAdoptPlan(AdoptOpts{
		EntryName: entry, Client: "codex-cli", ManifestName: entry,
		Port:    nextBindableAdoptPortForTest(t, collectUsedAdoptPorts()),
		Clients: []string{"claude-code", "codex-cli"},
	})
	if err != nil {
		t.Fatalf("BuildAdoptPlan: %v", err)
	}
	if err := NewAPI().ExecuteAdopt(plan, &bytes.Buffer{}); err == nil {
		t.Fatal("ExecuteAdopt succeeded; want induced second-client write failure")
	}
	after, err := os.ReadFile(claudePath)
	if err != nil {
		t.Fatalf("read claude config after rollback: %v", err)
	}
	var beforeState, afterState map[string]any
	if err := json.Unmarshal(originalClaude, &beforeState); err != nil {
		t.Fatalf("decode original claude config: %v", err)
	}
	if err := json.Unmarshal(after, &afterState); err != nil {
		t.Fatalf("decode claude config after rollback: %v", err)
	}
	if !reflect.DeepEqual(afterState, beforeState) {
		t.Fatalf("rollback did not restore the enabled recipe and unrelated entry:\nwant %#v\n got %#v", beforeState, afterState)
	}
	if _, err := os.Stat(filepath.Join(manifestRoot, entry, "manifest.yaml")); !os.IsNotExist(err) {
		t.Fatalf("rollback left generated manifest: %v", err)
	}
}

func assertManagedRecipeManifest(t *testing.T, raw, wantName, wantCommand string, wantArgs []string, wantEnv map[string]string) {
	t.Helper()
	manifest, err := config.ParseManifest(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("ParseManifest(%s): %v\n%s", wantName, err, raw)
	}
	if manifest.Name != wantName || manifest.Transport != config.TransportStdioBridge {
		t.Fatalf("manifest identity = (%q, %q), want (%q, %q)", manifest.Name, manifest.Transport, wantName, config.TransportStdioBridge)
	}
	if manifest.Command != wantCommand || !reflect.DeepEqual(manifest.BaseArgs, wantArgs) || !reflect.DeepEqual(manifest.Env, wantEnv) {
		t.Fatalf("manifest recipe = command %q args %#v env %#v, want command %q args %#v env %#v",
			manifest.Command, manifest.BaseArgs, manifest.Env, wantCommand, wantArgs, wantEnv)
	}
}
