package api

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/internal/clients"
)

func TestClientEntryRemovalExplicitIntentAndWire(t *testing.T) {
	dir := hardenedTempDir(t)
	path := filepath.Join(dir, "mcp.json")
	before := []byte(`{"mcpServers":{"serena":{"url":"http://localhost:9121/mcp","headers":{"Authorization":"synthetic-secret-sentinel"}}},"extra":"preserved"}`)
	if err := os.WriteFile(path, before, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".bak-mcp-local-hub-original", []byte(`{"mcpServers":{"serena":{"command":"synthetic-old-direct"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	production, err := clients.NewKimiCodeCLI()
	if err != nil {
		t.Fatal(err)
	}
	adapter, ok := clients.EntryRemovalClientForScan(production, path)
	if !ok {
		t.Fatal("scan source unsupported")
	}
	fleet := map[string]clients.Client{"kimi-code-cli": adapter}
	cap := clients.DescribeEntryRemoval(adapter, "serena")
	if !cap.Supported {
		t.Fatalf("capability: %+v", cap)
	}
	request := ClientEntryRemovalRequest{Server: "serena", Client: "kimi-code-cli", SourceToken: cap.SourceToken}
	result, err := NewAPI().removeClientEntry(context.Background(), request, fleet)
	if err != nil || result.Outcome != "refused" || result.Code != ClientEntryRemovalApplyRequired {
		t.Fatalf("safe preview: %+v %v", result, err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != string(before) {
		t.Fatal("preview changed config")
	}
	request.Apply = true
	result, err = NewAPI().removeClientEntry(context.Background(), request, fleet)
	if err != nil || result.Outcome != "removed" || !result.EffectiveAbsent || result.Receipt == nil {
		t.Fatalf("remove: %+v %v", result, err)
	}
	got, _ = os.ReadFile(path)
	if strings.Contains(string(got), "serena") || !strings.Contains(string(got), "preserved") {
		t.Fatalf("physical result: %s", got)
	}
	wire, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(wire), "synthetic-secret-sentinel") || strings.Contains(string(wire), dir) || strings.Contains(string(wire), "synthetic-old-direct") {
		t.Fatal("wire leaked config preimage")
	}
	t.Logf("observed removal wire: %s", wire)
	result, err = NewAPI().removeClientEntry(context.Background(), request, fleet)
	if err != nil || result.Code != "REMOVAL_SOURCE_CONFLICT" {
		t.Fatalf("unexpected disappearance credited: %+v %v", result, err)
	}
}

func TestClientEntryRemovalMissingAuthority(t *testing.T) {
	for _, req := range []ClientEntryRemovalRequest{
		{Apply: true, Server: "one", Client: "kimi-code-cli"},
		{Apply: true, Server: "one", SourceToken: "token"},
		{Apply: true, Client: "kimi-code-cli", SourceToken: "token"},
		{Apply: true, Server: "one", Client: "unknown-client", SourceToken: "token"},
	} {
		if ValidateClientEntryRemovalRequest(req) == nil {
			t.Fatalf("invalid selection accepted: %+v", req)
		}
	}
}

func TestClientEntryRemovalScanMetadataMatchesCurrentSource(t *testing.T) {
	dir := hardenedTempDir(t)
	path := filepath.Join(dir, "mcp.json")
	if err := os.WriteFile(path, []byte(`{"mcpServers":{"one":{"url":"https://example.invalid/mcp"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	rows := map[string]*ScanEntry{"one": {Name: "one", ClientPresence: map[string]ClientEntry{"kimi-code-cli": {Transport: "http"}}}}
	applyEntryRemovalCapabilities(rows, map[string]string{"kimi-code-cli": path})
	cap := rows["one"].ClientPresence["kimi-code-cli"].Removal
	if cap == nil || !cap.Supported || cap.SourceToken == "" || cap.Version != 1 {
		t.Fatalf("metadata: %+v", cap)
	}
	production, err := clients.NewKimiCodeCLI()
	if err != nil {
		t.Fatal(err)
	}
	bound, ok := clients.EntryRemovalClientForScan(production, path)
	if !ok {
		t.Fatal("path binding failed")
	}
	if got := clients.DescribeEntryRemoval(bound, "one"); got.SourceToken != cap.SourceToken {
		t.Fatal("scan token producer drifted")
	}
}
