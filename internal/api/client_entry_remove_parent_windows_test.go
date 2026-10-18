//go:build windows

package api

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"mcp-local-hub/internal/clients"
)

func TestClientEntryRemovalInspectionRedirectedParentBeforeFlock(t *testing.T) {
	for _, kind := range []string{"final", "intermediate"} {
		t.Run(kind, func(t *testing.T) {
			root := hardenedTempDir(t)
			foreign := hardenedTempDir(t)
			link := filepath.Join(root, "redirected")
			if err := createJunctionForTest(link, foreign); err != nil {
				t.Fatalf("required real junction fixture unavailable: %v", err)
			}
			t.Cleanup(func() {
				if err := os.Remove(link); err != nil && !os.IsNotExist(err) {
					t.Errorf("owned junction cleanup: %v", err)
				}
			})
			actualParent := foreign
			selectedParent := link
			if kind == "intermediate" {
				actualParent = filepath.Join(foreign, "child")
				selectedParent = filepath.Join(link, "child")
				if err := os.Mkdir(actualParent, 0700); err != nil {
					t.Fatal(err)
				}
			}
			actualPath := filepath.Join(actualParent, "mcp.json")
			selectedPath := filepath.Join(selectedParent, "mcp.json")
			before := []byte(`{"mcpServers":{"one":{"url":"https://example.invalid/mcp"}}}`)
			if err := os.WriteFile(actualPath, before, 0600); err != nil {
				t.Fatal(err)
			}
			prod, err := clients.NewKimiCodeCLI()
			if err != nil {
				t.Fatal(err)
			}
			adapter, ok := clients.EntryRemovalClientForScan(prod, selectedPath)
			if !ok {
				t.Fatal("fixture adapter binding")
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			started := time.Now()
			result, err := NewAPI().removeClientEntry(ctx, ClientEntryRemovalRequest{Server: "one", Client: "kimi-code-cli"}, map[string]clients.Client{"kimi-code-cli": adapter})
			if err != nil || result.Outcome != "failed" || result.Code != "REMOVAL_INSPECTION_FAILED" || time.Since(started) > 2*time.Second {
				t.Fatalf("redirected inspection: %+v %v", result, err)
			}
			if _, err := os.Stat(actualPath + ".lock"); !os.IsNotExist(err) {
				t.Fatalf("redirected source gained lock before refusal: %v", err)
			}
			got, err := os.ReadFile(actualPath)
			if err != nil || string(got) != string(before) {
				t.Fatal("redirected read changed config")
			}
		})
	}
}

func TestClientEntryRemovalInspectionOrdinaryAndMissingParent(t *testing.T) {
	root := hardenedTempDir(t)
	for _, present := range []bool{true, false} {
		dir := filepath.Join(root, "ordinary")
		if !present {
			dir = filepath.Join(root, "missing", "nested")
		} else if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "mcp.json")
		if present {
			if err := os.WriteFile(path, []byte(`{"mcpServers":{"one":{"url":"https://example.invalid/mcp"}}}`), 0600); err != nil {
				t.Fatal(err)
			}
		}
		prod, _ := clients.NewKimiCodeCLI()
		adapter, ok := clients.EntryRemovalClientForScan(prod, path)
		if !ok {
			t.Fatal("fixture bind")
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		cap, err := clients.DescribeEntryRemovalContext(ctx, adapter, "one")
		cancel()
		if present {
			if err != nil || !cap.Supported {
				t.Fatalf("ordinary parent refused: %+v %v", cap, err)
			}
		} else {
			if cap.Supported {
				t.Fatal("missing source supported")
			}
			if _, err := os.Stat(filepath.Join(root, "missing")); !os.IsNotExist(err) {
				t.Fatal("inspection created parent")
			}
			if _, err := os.Stat(path + ".lock"); !os.IsNotExist(err) {
				t.Fatal("missing inspection created lock")
			}
		}
	}
}
