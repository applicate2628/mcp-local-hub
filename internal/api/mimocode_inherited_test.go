package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"mcp-local-hub/internal/clients"
)

// writeMimoManifest writes a minimal global manifest with one daemon at `port`
// so the scan's port-aware via-hub gate has a daemon port to match.
func writeMimoManifest(t *testing.T, dir, name string, port int) string {
	t.Helper()
	manifestDir := filepath.Join(dir, "servers")
	srvDir := filepath.Join(manifestDir, name)
	if err := os.MkdirAll(srvDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "name: " + name + "\nkind: global\ntransport: stdio-bridge\ncommand: npx\ndaemons:\n  - name: default\n    port: " + strconv.Itoa(port) + "\n"
	if err := os.WriteFile(filepath.Join(srvDir, "manifest.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return manifestDir
}

func mimoEntryFor(res *ScanResult, name string) *ScanEntry {
	for i := range res.Entries {
		if res.Entries[i].Name == name {
			return &res.Entries[i]
		}
	}
	return nil
}

// TestScanMimoCode_ClaudeImportHubURL_ClassifiedInherited is the load-bearing
// classification probe for the import-inherited fix: a mimocode `time` server
// whose hub-loopback URL is sourced ONLY from the ~/.claude.json mcpServers
// import (a layer the hub never wrote) must classify Status == "via-hub-inherited"
// with Managed == false and ClientPresence["mimocode"].Inherited == true — NOT
// "via-hub" (which would offer a demigrate switch that always fails closed).
func TestScanMimoCode_ClaudeImportHubURL_ClassifiedInherited(t *testing.T) {
	isolateMimoCodeScanEnv(t)
	// Re-enable the import and redirect HOME/USERPROFILE so the import reads a
	// FIXTURE ~/.claude.json, never the developer's real one.
	t.Setenv(clients.MimoCodeDisableClaudeImportEnv, "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	const port = 9120
	manifestDir := writeMimoManifest(t, home, "time", port)

	globalDir := filepath.Join(home, ".config", "mimocode")
	if err := os.MkdirAll(globalDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mimoPath := filepath.Join(globalDir, "mimocode.json")
	// Write target EXISTS but does NOT define `time`.
	if err := os.WriteFile(mimoPath, []byte(`{"mcp":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// `time` resolves ONLY from the ~/.claude.json import, pointing at the hub
	// loopback URL on the manifest's daemon port.
	claudeBody := `{"mcpServers":{"time":{"url":"http://localhost:9120/mcp"}}}`
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(claudeBody), 0o600); err != nil {
		t.Fatal(err)
	}

	a := NewAPI()
	res, err := a.ScanFrom(ScanOpts{MimoCodeConfigPath: mimoPath, ManifestDir: manifestDir})
	if err != nil {
		t.Fatalf("ScanFrom: %v", err)
	}
	ent := mimoEntryFor(res, "time")
	if ent == nil {
		t.Fatal("`time` row missing from the scan (import-only profile must still be scanned)")
	}
	if ent.Status != "via-hub-inherited" {
		t.Errorf("import-sourced hub URL must classify via-hub-inherited, got %q", ent.Status)
	}
	if ent.Managed {
		t.Errorf("via-hub-inherited must keep Managed == false, got true")
	}
	if !ent.ClientPresence["mimocode"].Inherited {
		t.Errorf("the mimocode cell must carry Inherited == true, got %+v", ent.ClientPresence["mimocode"])
	}
}

// TestScanMimoCode_OwnedHubURL_StaysViaHub is the regression guard against
// misclassifying a genuinely hub-OWNED entry: the SAME hub URL written into the
// write target (mimocode.json) must classify "via-hub", Managed == true, and
// Inherited == false (still demigratable).
func TestScanMimoCode_OwnedHubURL_StaysViaHub(t *testing.T) {
	isolateMimoCodeScanEnv(t) // import disabled by default
	tmp := t.TempDir()
	const port = 9120
	manifestDir := writeMimoManifest(t, tmp, "time", port)

	mimoPath := filepath.Join(tmp, "mimocode.json")
	// `time` is written DIRECTLY into the write target → at/above → hub-ownable.
	if err := os.WriteFile(mimoPath,
		[]byte(`{"mcp":{"time":{"type":"remote","url":"http://localhost:9120/mcp","enabled":true}}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	a := NewAPI()
	res, err := a.ScanFrom(ScanOpts{MimoCodeConfigPath: mimoPath, ManifestDir: manifestDir})
	if err != nil {
		t.Fatalf("ScanFrom: %v", err)
	}
	ent := mimoEntryFor(res, "time")
	if ent == nil {
		t.Fatal("`time` row missing from the scan")
	}
	if ent.Status != "via-hub" {
		t.Errorf("an owned write-target hub URL must classify via-hub, got %q", ent.Status)
	}
	if !ent.Managed {
		t.Errorf("an owned via-hub entry must have Managed == true, got false")
	}
	if ent.ClientPresence["mimocode"].Inherited {
		t.Errorf("an owned entry must have Inherited == false, got %+v", ent.ClientPresence["mimocode"])
	}
}

// TestScanMimoCode_ImportLSPHubURL_ClassifiedInherited is the end-to-end probe
// for bot finding #2 (PR #422): a mimocode `mcp-language-server-go` LSP entry
// whose hub-loopback URL is sourced ONLY from the ~/.claude.json mcpServers
// import (a layer the hub never wrote) must classify Status == "via-hub-inherited"
// with Managed == false — NOT "via-hub". This exercises the FULL path: the
// mimocode scan stamps ClientEntry.Inherited on the http hub-loopback LSP cell,
// classify() sets "via-hub-inherited", then classifyLSPEntries (which runs AFTER
// and re-classifies http LSP rows) MUST preserve it rather than forcing it back
// to the demigratable "via-hub" bucket.
func TestScanMimoCode_ImportLSPHubURL_ClassifiedInherited(t *testing.T) {
	isolateMimoCodeScanEnv(t)
	t.Setenv(clients.MimoCodeDisableClaudeImportEnv, "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	const (
		lspName = "mcp-language-server-go"
		port    = 9300
	)
	manifestDir := writeMimoManifest(t, home, lspName, port)

	globalDir := filepath.Join(home, ".config", "mimocode")
	if err := os.MkdirAll(globalDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mimoPath := filepath.Join(globalDir, "mimocode.json")
	// Write target EXISTS but does NOT define the LSP server.
	if err := os.WriteFile(mimoPath, []byte(`{"mcp":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// The LSP server resolves ONLY from the ~/.claude.json import, pointing at the
	// hub loopback URL on the manifest's daemon port.
	claudeBody := `{"mcpServers":{"` + lspName + `":{"url":"http://localhost:9300/mcp"}}}`
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(claudeBody), 0o600); err != nil {
		t.Fatal(err)
	}

	a := NewAPI()
	res, err := a.ScanFrom(ScanOpts{MimoCodeConfigPath: mimoPath, ManifestDir: manifestDir})
	if err != nil {
		t.Fatalf("ScanFrom: %v", err)
	}
	ent := mimoEntryFor(res, lspName)
	if ent == nil {
		t.Fatalf("%q row missing from the scan (import-only LSP profile must still be scanned)", lspName)
	}
	if !ent.ClientPresence["mimocode"].Inherited {
		t.Errorf("the mimocode LSP cell must carry Inherited == true, got %+v", ent.ClientPresence["mimocode"])
	}
	if ent.Status != "via-hub-inherited" {
		t.Errorf("import-sourced hub LSP URL must classify via-hub-inherited (classifyLSPEntries must not force via-hub), got %q", ent.Status)
	}
	if ent.Managed {
		t.Errorf("via-hub-inherited LSP row must keep Managed == false, got true")
	}
}

// TestClientEntry_InheritedJSON_OmitEmpty is the wire-shape verification (per
// AGENTS.md): the additive Inherited field is absent on the wire when false
// (so every non-mimocode client's bytes are byte-identical) and present+true
// when set.
func TestClientEntry_InheritedJSON_OmitEmpty(t *testing.T) {
	falseBytes, err := json.Marshal(ClientEntry{Transport: "http", Endpoint: "http://x/mcp"})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(falseBytes); strings.Contains(got, "inherited") {
		t.Errorf("Inherited:false must be OMITTED from the wire (omitempty), got %s", got)
	}
	trueBytes, err := json.Marshal(ClientEntry{Transport: "http", Endpoint: "http://x/mcp", Inherited: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(trueBytes); !strings.Contains(got, `"inherited":true`) {
		t.Errorf("Inherited:true must be PRESENT on the wire, got %s", got)
	}
}
