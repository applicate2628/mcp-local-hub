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

func TestSessionLocalReconcileSignalDoesNotPromoteProjectToGlobal(t *testing.T) {
	hubMcpStateTestHelper(t)
	home := hardenedTempDir(t)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	client, err := clients.NewCodexCLI()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(client.ConfigPath()), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(client.ConfigPath(), []byte("[mcp_servers.private-fixture]\nurl = 'http://127.0.0.1:9191/clients/codex-cli/mcp'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	owner, ok := clients.AsManagedRegistrationClient(client)
	if !ok {
		t.Fatal("global config owner unavailable")
	}
	snapshot, err := owner.SnapshotManagedRegistration(context.Background(), "private-fixture")
	if err != nil {
		t.Fatal(err)
	}
	entry := ManagedEntry{Client: "codex-cli", Server: "private-fixture", Owner: "hub-supervisor", Exists: true,
		Transport: clients.ManagedRegistrationTransportHTTP, IdentityState: clients.ManagedRegistrationIdentityManagedHTTP,
		ConfigPathSHA256: snapshot.ConfigPathSHA256}
	write := func() {
		t.Helper()
		if err := writeManagedEntries(&ManagedEntries{Entries: []ManagedEntry{entry}}); err != nil {
			t.Fatal(err)
		}
	}
	write()
	global, err := SessionLocalInstalledServers(context.Background(), true)
	if err != nil || !global[entry.Server] {
		t.Fatalf("global signal lost: %v", err)
	}
	// Reconcile removes the individual table in favor of mcphub-hub. The
	// existing positive install marker must survive that expected rewrite.
	if err := os.WriteFile(client.ConfigPath(), []byte("[mcp_servers.mcphub-hub]\nurl = 'http://127.0.0.1:9191/clients/codex-cli/mcp'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	global, err = SessionLocalInstalledServers(context.Background(), true)
	if err != nil || !global[entry.Server] {
		t.Fatalf("aggregate rewrite erased install signal: %v", err)
	}
	// A different physical project path remains a valid project attachment,
	// but its projection cannot authorize creation of a global entry.
	if err := json.Unmarshal([]byte(`"`+strings.Repeat("b", 64)+`"`), &entry.ConfigPathSHA256); err != nil {
		t.Fatal(err)
	}
	write()
	global, err = SessionLocalInstalledServers(context.Background(), true)
	if err != nil || global[entry.Server] {
		t.Fatalf("project-only marker promoted to global: %v", err)
	}
	all, err := SessionLocalInstalledServers(context.Background(), false)
	if err != nil || !all[entry.Server] {
		t.Fatalf("gate-OFF forgot project private dependency: %v", err)
	}
	entry.Exists = false
	write()
	all, err = SessionLocalInstalledServers(context.Background(), false)
	if err != nil || all[entry.Server] {
		t.Fatalf("removed projection remained installed: %v", err)
	}
}
