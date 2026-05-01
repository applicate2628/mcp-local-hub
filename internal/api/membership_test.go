package api

import (
	"os"
	"path/filepath"
	"testing"
)

func setupRegistryWithEntries(t *testing.T) *Registry {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "workspaces.yaml")
	reg := NewRegistry(path)
	reg.Workspaces = []WorkspaceEntry{
		{WorkspaceKey: "k1", Language: "python", TaskName: "tA", Port: 9100, WeeklyRefresh: true, Backend: "mcp-language-server"},
		{WorkspaceKey: "k1", Language: "rust", TaskName: "tB", Port: 9101, WeeklyRefresh: false, Backend: "mcp-language-server"},
		{WorkspaceKey: "k2", Language: "go", TaskName: "tC", Port: 9102, WeeklyRefresh: true, Backend: "mcp-language-server"},
	}
	if err := reg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	return reg
}

func TestUpdateMembership_HappyPartialUpdate(t *testing.T) {
	reg := setupRegistryWithEntries(t)
	deltas := []MembershipDelta{
		{WorkspaceKey: "k1", Language: "python", Enabled: false},
		{WorkspaceKey: "k2", Language: "go", Enabled: false},
	}
	n, err := UpdateWeeklyRefreshMembership(reg.path, deltas)
	if err != nil {
		t.Fatalf("UpdateWeeklyRefreshMembership: %v", err)
	}
	if n != 2 {
		t.Errorf("updated = %d, want 2", n)
	}

	reloaded := NewRegistry(reg.path)
	if err := reloaded.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := map[string]bool{"k1/python": false, "k1/rust": false, "k2/go": false}
	for _, e := range reloaded.Workspaces {
		key := e.WorkspaceKey + "/" + e.Language
		if got, ok := want[key]; ok && got != e.WeeklyRefresh {
			t.Errorf("entry %s WeeklyRefresh = %v, want %v", key, e.WeeklyRefresh, got)
		}
	}
}

func TestUpdateMembership_UnknownPair_Rejected(t *testing.T) {
	reg := setupRegistryWithEntries(t)
	deltas := []MembershipDelta{
		{WorkspaceKey: "kX", Language: "ruby", Enabled: true},
	}
	_, err := UpdateWeeklyRefreshMembership(reg.path, deltas)
	if err == nil {
		t.Fatal("expected error for unknown (workspace_key, language); got nil")
	}
	reloaded := NewRegistry(reg.path)
	if err := reloaded.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, e := range reloaded.Workspaces {
		if e.WorkspaceKey == "k1" && e.Language == "python" && !e.WeeklyRefresh {
			t.Error("registry mutated despite validation failure")
		}
	}
}

func TestUpdateMembership_EmptyBody_NoOp(t *testing.T) {
	reg := setupRegistryWithEntries(t)
	statBefore, err := os.Stat(reg.path)
	if err != nil {
		t.Fatal(err)
	}
	n, err := UpdateWeeklyRefreshMembership(reg.path, []MembershipDelta{})
	if err != nil {
		t.Fatalf("UpdateWeeklyRefreshMembership: %v", err)
	}
	if n != 0 {
		t.Errorf("updated = %d, want 0 for empty body", n)
	}
	statAfter, err := os.Stat(reg.path)
	if err != nil {
		t.Fatal(err)
	}
	if !statBefore.ModTime().Equal(statAfter.ModTime()) {
		t.Error("empty body should not rewrite registry file")
	}
}
