package api

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/internal/clients"
)

func TestClientEntryRemovalProductionPublicationGuard(t *testing.T) {
	for _, stage := range []string{"writer-entry", "pre-publication"} {
		for _, change := range []string{"member", "parent-identity"} {
			t.Run(stage+"-"+change, func(t *testing.T) {
				root := hardenedTempDir(t)
				dir := filepath.Join(root, "source")
				foreignDir := filepath.Join(root, "foreign")
				for _, p := range []string{dir, foreignDir} {
					if err := os.Mkdir(p, 0700); err != nil {
						t.Fatal(err)
					}
				}
				path := filepath.Join(dir, "mcp.json")
				foreignPath := filepath.Join(foreignDir, "mcp.json")
				before := []byte(`{"mcpServers":{"one":{"url":"https://example.invalid/mcp"}},"sibling":0}`)
				for _, p := range []string{path, foreignPath} {
					if err := os.WriteFile(p, before, 0600); err != nil {
						t.Fatal(err)
					}
				}
				prod, err := clients.NewKimiCodeCLI()
				if err != nil {
					t.Fatal(err)
				}
				adapter, ok := clients.EntryRemovalClientForScan(prod, path)
				if !ok {
					t.Fatal("source bind")
				}
				cap := clients.DescribeEntryRemoval(adapter, "one")
				if !cap.Supported {
					t.Fatalf("capability: %+v", cap)
				}
				foreignIdentity := ""
				if change == "parent-identity" {
					foreignAdapter, ok := clients.EntryRemovalClientForScan(prod, foreignPath)
					if !ok {
						t.Fatal("foreign bind")
					}
					foreignCap := clients.DescribeEntryRemoval(foreignAdapter, "one")
					port := clients.WriteConfigFileRequest
					clients.WriteConfigFileRequest = func(req clients.ConfigWriteRequest) error {
						foreignIdentity = req.ExpectedSnapshot.AncestorIdentityForTest()
						return clients.ErrConfigWriteSnapshotConflict
					}
					t.Cleanup(func() { clients.WriteConfigFileRequest = port })
					_, err := NewAPI().removeClientEntry(context.Background(), ClientEntryRemovalRequest{Apply: true, Server: "one", Client: "kimi-code-cli", SourceToken: foreignCap.SourceToken}, map[string]clients.Client{"kimi-code-cli": foreignAdapter})
					clients.WriteConfigFileRequest = port
					if err != nil || foreignIdentity == "" {
						t.Fatalf("real identity capture failed: %v", err)
					}
				}
				foreign := []byte(`{"mcpServers":{"one":{"url":"https://foreign.invalid/mcp"}},"sibling":1}`)
				oldEntry, oldPublish := expectedSnapshotWriterEntryHook, expectedSnapshotBeforePublishHook
				oldExpectation := clients.ConfigWriteExpectedAncestorTestHook
				t.Cleanup(func() {
					expectedSnapshotWriterEntryHook = oldEntry
					expectedSnapshotBeforePublishHook = oldPublish
					clients.ConfigWriteExpectedAncestorTestHook = oldExpectation
				})
				fired := false
				hook := func() {
					if fired {
						return
					}
					fired = true
					if change == "member" {
						if err := os.WriteFile(path, foreign, 0600); err != nil {
							t.Fatal(err)
						}
					} else {
						clients.ConfigWriteExpectedAncestorTestHook = func(*clients.ConfigWriteSnapshot) string { return foreignIdentity }
					}
				}
				if stage == "writer-entry" {
					expectedSnapshotWriterEntryHook = hook
				} else {
					expectedSnapshotBeforePublishHook = hook
				}
				result, err := NewAPI().removeClientEntry(context.Background(), ClientEntryRemovalRequest{Apply: true, Server: "one", Client: "kimi-code-cli", SourceToken: cap.SourceToken}, map[string]clients.Client{"kimi-code-cli": adapter})
				if err != nil || !fired || result.Outcome == "removed" || result.Code != "REMOVAL_SOURCE_CONFLICT" {
					t.Fatalf("guard: %+v %v fired=%v", result, err, fired)
				}
				got, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				want := foreign
				if change == "parent-identity" {
					want = before
				}
				if string(got) != string(want) {
					t.Fatal("publisher overwrote or admitted wrong expected source")
				}
				other, err := os.ReadFile(foreignPath)
				if err != nil || string(other) != string(before) {
					t.Fatal("foreign identity source changed")
				}
				left, err := filepath.Glob(filepath.Join(dir, ".*.tmp.*"))
				if err != nil || len(left) != 0 {
					t.Fatalf("publisher temp residue: %v %v", left, err)
				}
			})
		}
	}
}
func TestClientEntryRemovalProductionInversePublicationGuard(t *testing.T) {
	for _, stage := range []string{"writer-entry", "pre-publication"} {
		t.Run(stage, func(t *testing.T) {
			dir := hardenedTempDir(t)
			path := filepath.Join(dir, "mcp.json")
			before := []byte(`{"mcpServers":{"one":{"url":"https://example.invalid/mcp"}},"sibling":0}`)
			if err := os.WriteFile(path, before, 0600); err != nil {
				t.Fatal(err)
			}
			prod, _ := clients.NewKimiCodeCLI()
			adapter, _ := clients.EntryRemovalClientForScan(prod, path)
			cap := clients.DescribeEntryRemoval(adapter, "one")
			oldPort := clients.WriteConfigFileRequest
			calls := 0
			clients.WriteConfigFileRequest = func(req clients.ConfigWriteRequest) error {
				calls++
				err := oldPort(req)
				if calls == 1 && err == nil {
					return errors.New("synthetic post-publication failure")
				}
				return err
			}
			oldEntry, oldPublish := expectedSnapshotWriterEntryHook, expectedSnapshotBeforePublishHook
			t.Cleanup(func() {
				clients.WriteConfigFileRequest = oldPort
				expectedSnapshotWriterEntryHook = oldEntry
				expectedSnapshotBeforePublishHook = oldPublish
			})
			fired := false
			foreign := []byte(`{"mcpServers":{"one":{"url":"https://foreign.invalid/mcp"}},"sibling":1}`)
			hook := func() {
				if calls == 2 && !fired {
					fired = true
					if err := os.WriteFile(path, foreign, 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			if stage == "writer-entry" {
				expectedSnapshotWriterEntryHook = hook
			} else {
				expectedSnapshotBeforePublishHook = hook
			}
			result, err := NewAPI().removeClientEntry(context.Background(), ClientEntryRemovalRequest{Apply: true, Server: "one", Client: "kimi-code-cli", SourceToken: cap.SourceToken}, map[string]clients.Client{"kimi-code-cli": adapter})
			if err != nil || !fired || result.Outcome != "rollback-conflict" {
				t.Fatalf("inverse guard: %+v %v fired=%v", result, err, fired)
			}
			got, _ := os.ReadFile(path)
			if string(got) != string(foreign) {
				t.Fatal("inverse overwrote foreign generation")
			}
		})
	}
}

func TestClientEntryRemovalProductionSiblingAtomicBatch(t *testing.T) {
	dir := hardenedTempDir(t)
	path := filepath.Join(dir, "mcp.json")
	before := []byte(`{"mcpServers":{"one":{"url":"https://example.invalid/one"},"two":{"url":"https://example.invalid/two"}},"sibling":"preserved"}`)
	if err := os.WriteFile(path, before, 0600); err != nil {
		t.Fatal(err)
	}
	prod, _ := clients.NewKimiCodeCLI()
	adapter, _ := clients.EntryRemovalClientForScan(prod, path)
	tokens := map[string]string{}
	for _, name := range []string{"one", "two"} {
		tokens[name] = clients.DescribeEntryRemoval(adapter, name).SourceToken
	}
	for _, name := range []string{"one", "two"} {
		r, err := NewAPI().removeClientEntry(context.Background(), ClientEntryRemovalRequest{Apply: true, Server: name, Client: "kimi-code-cli", SourceToken: tokens[name]}, map[string]clients.Client{"kimi-code-cli": adapter})
		if err != nil || r.Outcome != "removed" {
			t.Fatalf("sibling %s: %+v %v", name, r, err)
		}
	}
	got, _ := os.ReadFile(path)
	if !strings.Contains(string(got), "preserved") {
		t.Fatal("foreign sibling removed")
	}
}
