package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"mcp-local-hub/internal/clients"
)

func TestCodexCollisionDryRunUsesPortableSettlementSummary(t *testing.T) {
	const entry = "codex-h4-dry-run"
	_, _, _ = setupAdoptTestEnv(t, entry, `[mcp_servers.codex-h4-dry-run]
command = "go"
args = ["version"]
`)
	projectRoot := t.TempDir()
	projectConfig := filepath.Join(projectRoot, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(projectConfig), 0o700); err != nil {
		t.Fatalf("mkdir project Codex config: %v", err)
	}
	if err := os.WriteFile(projectConfig, []byte(`[mcp_servers.codex-h4-dry-run]
url = "http://127.0.0.1:9999/mcp"
`), 0o600); err != nil {
		t.Fatalf("write project Codex config: %v", err)
	}

	plan, err := NewAPI().BuildAdoptPlan(AdoptOpts{
		EntryName: entry, Client: "codex-cli", ManifestName: entry, Port: 9381,
		CodexProjectRoot: projectRoot, CodexWorkingDir: projectRoot,
	})
	if err != nil {
		t.Fatalf("BuildAdoptPlan: %v", err)
	}
	var out bytes.Buffer
	PrintAdoptPlan(&out, plan)
	got := out.String()
	for _, want := range []string{
		"logical source: " + entry,
		"target alias: " + entry + "-mcphub",
		"global write target: codex global config",
		"collision reason: cross-layer opposite transport",
		"action: relocate global HTTP/add alias; project read-only",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("dry-run output missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, projectRoot) {
		t.Fatalf("dry-run leaked absolute project path %q:\n%s", projectRoot, got)
	}
}

func TestCodexCollisionEmitsCommittedThenExplicitAlreadyConfigured(t *testing.T) {
	const entry = "codex-h4-settlement"
	codexPath, _, stateRoot := setupAdoptTestEnv(t, entry, `[mcp_servers.codex-h4-settlement]
command = "go"
args = ["version"]
`)
	projectRoot := t.TempDir()
	projectConfig := filepath.Join(projectRoot, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(projectConfig), 0o700); err != nil {
		t.Fatalf("mkdir project Codex config: %v", err)
	}
	projectBytes := []byte(`[mcp_servers.codex-h4-settlement]
url = "http://127.0.0.1:9999/mcp"
`)
	if err := os.WriteFile(projectConfig, projectBytes, 0o600); err != nil {
		t.Fatalf("write project Codex config: %v", err)
	}
	api := NewAPI()
	plan, err := api.BuildAdoptPlan(AdoptOpts{
		EntryName: entry, Client: "codex-cli", ManifestName: entry, Port: 9382,
		CodexProjectRoot: projectRoot, CodexWorkingDir: projectRoot,
	})
	if err != nil {
		t.Fatalf("BuildAdoptPlan: %v", err)
	}
	var first bytes.Buffer
	firstResult, err := api.ExecuteAdoptResultWithOpts(plan, &first, ExecuteAdoptOpts{})
	if err != nil {
		t.Fatalf("ExecuteAdopt: %v", err)
	}
	if len(firstResult.ClientConfigSettlements) != 1 || !firstResult.ClientConfigSettlements[0].IsCommittedWrite() {
		t.Fatalf("first adopt result = %#v, want one committed typed settlement", firstResult)
	}
	if strings.Contains(first.String(), projectRoot) || strings.Contains(first.String(), stateRoot) {
		t.Fatalf("first adopt output leaked an absolute path:\n%s", first.String())
	}
	eventBytes, err := os.ReadFile(filepath.Join(stateRoot, SupervisorEventLogFileLeaf))
	if err != nil {
		t.Fatalf("read supervisor event log: %v", err)
	}
	if got := strings.Count(string(eventBytes), `"event":"client-config-settled"`); got != 1 {
		t.Fatalf("generic settlement event count = %d, want 1:\n%s", got, eventBytes)
	}
	if strings.Contains(string(eventBytes), projectRoot) || strings.Contains(string(eventBytes), stateRoot) {
		t.Fatalf("Codex settlement event leaked an absolute path:\n%s", eventBytes)
	}

	globalBeforeRepeat, err := os.ReadFile(codexPath)
	if err != nil {
		t.Fatalf("read global Codex config before repeat: %v", err)
	}
	repeat, err := api.BuildAdoptPlan(AdoptOpts{
		EntryName: entry, Client: "codex-cli", ManifestName: entry, Port: 9382,
		CodexProjectRoot: projectRoot, CodexWorkingDir: projectRoot,
	})
	if err != nil {
		t.Fatalf("repeat BuildAdoptPlan: %v", err)
	}
	var repeatOut bytes.Buffer
	repeatResult, err := api.ExecuteAdoptResultWithOpts(repeat, &repeatOut, ExecuteAdoptOpts{})
	if err != nil {
		t.Fatalf("repeat ExecuteAdopt: %v", err)
	}
	if len(repeatResult.ClientConfigSettlements) != 0 {
		t.Fatalf("repeat result manufactured settlement without a writer result: %#v", repeatResult)
	}
	globalAfterRepeat, err := os.ReadFile(codexPath)
	if err != nil {
		t.Fatalf("read global Codex config after repeat: %v", err)
	}
	if !bytes.Equal(globalBeforeRepeat, globalAfterRepeat) {
		t.Fatal("repeat changed the global Codex config")
	}
	projectAfterRepeat, err := os.ReadFile(projectConfig)
	if err != nil {
		t.Fatalf("read project Codex config after repeat: %v", err)
	}
	if !bytes.Equal(projectBytes, projectAfterRepeat) {
		t.Fatal("repeat changed the project Codex config")
	}
	eventBytes, err = os.ReadFile(filepath.Join(stateRoot, SupervisorEventLogFileLeaf))
	if err != nil {
		t.Fatalf("read supervisor event log after repeat: %v", err)
	}
	if got := strings.Count(string(eventBytes), `"event":"client-config-settled"`); got != 1 {
		t.Fatalf("repeat emitted a settlement without a writer result: count=%d\n%s", got, eventBytes)
	}
}

func TestCodexCollisionRelocationFailureEmitsNoCommittedSignal(t *testing.T) {
	const entry = "codex-h4-relocation-failure"
	_, _, stateRoot := setupAdoptTestEnv(t, entry, `[mcp_servers.codex-h4-relocation-failure]
command = "go"
args = ["version"]
`)
	projectRoot := t.TempDir()
	projectConfig := filepath.Join(projectRoot, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(projectConfig), 0o700); err != nil {
		t.Fatalf("mkdir project Codex config: %v", err)
	}
	if err := os.WriteFile(projectConfig, []byte(`[mcp_servers.codex-h4-relocation-failure]
url = "http://127.0.0.1:9999/mcp"
`), 0o600); err != nil {
		t.Fatalf("write project Codex config: %v", err)
	}
	api := NewAPI()
	plan, err := api.BuildAdoptPlan(AdoptOpts{
		EntryName: entry, Client: "codex-cli", ManifestName: entry, Port: 9383,
		CodexProjectRoot: projectRoot, CodexWorkingDir: projectRoot,
	})
	if err != nil {
		t.Fatalf("BuildAdoptPlan: %v", err)
	}
	if err := clients.AllClients()["codex-cli"].AddEntry(clients.MCPEntry{
		Name: entry,
		URL:  "http://127.0.0.1:9911/mcp",
	}); err != nil {
		t.Fatalf("replace frozen source transport: %v", err)
	}

	var out bytes.Buffer
	if err := api.ExecuteAdopt(plan, &out); err == nil {
		t.Fatal("ExecuteAdopt succeeded after the frozen Codex source transport drifted")
	}
	if strings.Contains(out.String(), "client-config-settled") {
		t.Fatalf("failed relocation printed a settlement signal:\n%s", out.String())
	}
	eventBytes, err := os.ReadFile(filepath.Join(stateRoot, SupervisorEventLogFileLeaf))
	if err != nil {
		t.Fatalf("read supervisor event log: %v", err)
	}
	if strings.Contains(string(eventBytes), `"event":"client-config-settled"`) {
		t.Fatalf("failed relocation emitted a settlement event:\n%s", eventBytes)
	}
}

func TestAdoptCodexSettlementSurvivesOuterFailureWithoutAdoptSuccess(t *testing.T) {
	const entry = "codex-h4-outer-failure"
	_, _, stateRoot := setupAdoptTestEnv(t, entry, `[mcp_servers.codex-h4-outer-failure]
command = "go"
args = ["version"]
`)
	projectRoot := t.TempDir()
	projectConfig := filepath.Join(projectRoot, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(projectConfig), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(projectConfig, []byte(`[mcp_servers.codex-h4-outer-failure]
url = "http://127.0.0.1:9999/mcp"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	plan, err := NewAPI().BuildAdoptPlan(AdoptOpts{EntryName: entry, Client: "codex-cli", ManifestName: entry, Port: 9384, CodexProjectRoot: projectRoot, CodexWorkingDir: projectRoot})
	if err != nil {
		t.Fatal(err)
	}
	result, err := NewAPI().ExecuteAdoptResultWithOpts(plan, io.Discard, ExecuteAdoptOpts{
		ReceivingVerifier: func(*API, *AdoptPlan, *AdoptProvenanceRecord) error {
			return errors.New("injected outer receiver failure")
		},
	})
	if err == nil {
		t.Fatal("adopt unexpectedly succeeded after outer receiver failure")
	}
	if len(result.ClientConfigSettlements) != 1 || !result.ClientConfigSettlements[0].IsCommittedWrite() {
		t.Fatalf("outer failure lost committed lower settlement: %#v", result)
	}
	eventBytes, readErr := os.ReadFile(filepath.Join(stateRoot, SupervisorEventLogFileLeaf))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !strings.Contains(string(eventBytes), `"event":"client-config-settled"`) {
		t.Fatalf("missing generic committed settlement event: %s", eventBytes)
	}
	if strings.Contains(string(eventBytes), `"event":"adopt-executed"`) {
		t.Fatalf("outer failure emitted adopt success: %s", eventBytes)
	}
}

func TestAdoptAPI_HSettlementEventFailureCarriesCommittedRow(t *testing.T) {
	const entry = "codex-h4-event-failure"
	codexPath, _, stateRoot := setupAdoptTestEnv(t, entry, `[mcp_servers.codex-h4-event-failure]
command = "go"
args = ["version"]
`)
	projectRoot := t.TempDir()
	projectConfig := filepath.Join(projectRoot, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(projectConfig), 0o700); err != nil {
		t.Fatal(err)
	}
	projectBytes := []byte(`[mcp_servers.codex-h4-event-failure]
url = "http://127.0.0.1:9999/mcp"
`)
	if err := os.WriteFile(projectConfig, projectBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	plan, err := NewAPI().BuildAdoptPlan(AdoptOpts{
		EntryName: entry, Client: "codex-cli", ManifestName: entry, Port: 9385,
		CodexProjectRoot: projectRoot, CodexWorkingDir: projectRoot,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(stateRoot, SupervisorEventLogFileLeaf), 0o700); err != nil {
		t.Fatalf("make isolated event-log leaf a directory: %v", err)
	}
	result, err := NewAPI().ExecuteAdoptResultWithOpts(plan, io.Discard, ExecuteAdoptOpts{})
	if !errors.Is(err, ErrClientConfigSettlementEventFailed) {
		t.Fatalf("error = %v, want CLIENT_CONFIG_SETTLEMENT_EVENT_FAILED", err)
	}
	if len(result.ClientConfigSettlements) != 1 || !result.ClientConfigSettlements[0].IsCommittedWrite() {
		t.Fatalf("result = %#v, want one committed settlement row", result)
	}
	alias, readErr := clients.AllClients()["codex-cli"].GetEntry(entry + "-mcphub")
	if readErr != nil || alias == nil || alias.URL == "" {
		t.Fatalf("committed global alias = %#v, err = %v", alias, readErr)
	}
	projectAfter, readErr := os.ReadFile(projectConfig)
	if readErr != nil || !bytes.Equal(projectAfter, projectBytes) {
		t.Fatalf("project config changed or unreadable: err=%v before=%q after=%q", readErr, projectBytes, projectAfter)
	}
	globalAfter, readErr := os.ReadFile(codexPath)
	if readErr != nil || !bytes.Contains(globalAfter, []byte(entry+"-mcphub")) {
		t.Fatalf("global alias bytes missing: err=%v body=%s", readErr, globalAfter)
	}
	if _, statErr := os.Stat(filepath.Join(stateRoot, "adopt-executed")); statErr == nil {
		t.Fatal("unexpected adopt success artifact")
	}
}

func TestAdoptNonCodexCompatibilityMatrixDoesNotEmitHSettlement(t *testing.T) {
	for index, clientName := range []string{"claude-code", "cursor"} {
		t.Run(clientName, func(t *testing.T) {
			entry := "noncodex-h4-" + strings.ReplaceAll(clientName, "-", "")
			_, _, stateRoot := setupAdoptTestEnv(t, entry, `[mcp_servers.unrelated]
command = "go"
args = ["version"]
`)
			client := clients.AllClients()[clientName]
			if client == nil {
				t.Fatalf("%s adapter unavailable", clientName)
			}
			if err := os.MkdirAll(filepath.Dir(client.ConfigPath()), 0o700); err != nil {
				t.Fatal(err)
			}
			seed := []byte(`{"mcpServers":{"` + entry + `":{"type":"stdio","command":"go","args":["version"]}},"keep":true}`)
			if err := os.WriteFile(client.ConfigPath(), seed, 0o600); err != nil {
				t.Fatal(err)
			}
			port := 9391 + index
			plan, err := NewAPI().BuildAdoptPlan(AdoptOpts{
				EntryName: entry, Client: clientName, ManifestName: entry, Port: port, Clients: []string{clientName},
			})
			if err != nil {
				t.Fatalf("BuildAdoptPlan: %v", err)
			}
			var out bytes.Buffer
			result, err := NewAPI().ExecuteAdoptResultWithOpts(plan, &out, ExecuteAdoptOpts{})
			if err != nil {
				t.Fatalf("ExecuteAdoptResultWithOpts: %v", err)
			}
			if len(result.ClientConfigSettlements) != 0 {
				t.Fatalf("non-Codex result fabricated H settlement: %#v", result)
			}
			got, err := client.GetEntry(entry)
			if err != nil || got == nil || got.URL != "http://127.0.0.1:"+strconv.Itoa(port)+"/mcp" {
				t.Fatalf("non-Codex config result=%#v err=%v", got, err)
			}
			raw, err := os.ReadFile(client.ConfigPath())
			var document map[string]any
			if err != nil || json.Unmarshal(raw, &document) != nil || document["keep"] != true || bytes.Contains(raw, []byte(`"command"`)) {
				t.Fatalf("non-Codex config lost existing wire content: err=%v bytes=%s", err, raw)
			}
			if strings.Contains(out.String(), "client-config-settled") || strings.Contains(out.String(), "client-config-settlement-v1") {
				t.Fatalf("non-Codex output fabricated H signal: %s", out.String())
			}
			events, err := os.ReadFile(filepath.Join(stateRoot, SupervisorEventLogFileLeaf))
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(events, []byte(`"event":"client-config-settled"`)) || !bytes.Contains(events, []byte(`"event":"adopt-executed"`)) {
				t.Fatalf("non-Codex event behavior changed: %s", events)
			}
		})
	}
}
