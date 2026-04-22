package config

import (
	"strings"
	"testing"
)

func TestParsePortRegistry(t *testing.T) {
	yaml := `
global:
  - server: serena
    daemon: claude
    port: 9121
  - server: serena
    daemon: codex
    port: 9122
  - server: memory
    daemon: shared
    port: 9140
workspace_scoped:
  - server: mcp-language-server
    pool_start: 9200
    pool_end: 9299
`
	r, err := ParsePortRegistry(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("ParsePortRegistry: %v", err)
	}
	if len(r.Global) != 3 {
		t.Fatalf("len(Global) = %d, want 3", len(r.Global))
	}
	if r.Global[0].Port != 9121 {
		t.Errorf("Global[0].Port = %d, want 9121", r.Global[0].Port)
	}
	if len(r.WorkspaceScoped) != 1 {
		t.Fatalf("len(WorkspaceScoped) = %d, want 1", len(r.WorkspaceScoped))
	}
}

func TestPortRegistry_DetectConflictGlobal(t *testing.T) {
	yaml := `
global:
  - server: a
    daemon: x
    port: 9121
  - server: b
    daemon: y
    port: 9121
`
	_, err := ParsePortRegistry(strings.NewReader(yaml))
	if err == nil {
		t.Fatal("expected conflict error, got nil")
	}
	if !strings.Contains(err.Error(), "9121") {
		t.Errorf("error should mention conflicting port 9121, got: %v", err)
	}
}

func TestPortRegistry_DetectPoolOverlap(t *testing.T) {
	yaml := `
global:
  - server: a
    daemon: x
    port: 9250
workspace_scoped:
  - server: b
    pool_start: 9200
    pool_end: 9299
`
	_, err := ParsePortRegistry(strings.NewReader(yaml))
	if err == nil {
		t.Fatal("expected overlap error between global port 9250 and workspace pool 9200-9299")
	}
}

func TestPortRegistry_DetectWorkspacePoolOverlap(t *testing.T) {
	yaml := `
workspace_scoped:
  - server: a
    pool_start: 9200
    pool_end: 9250
  - server: b
    pool_start: 9240
    pool_end: 9299
`
	_, err := ParsePortRegistry(strings.NewReader(yaml))
	if err == nil {
		t.Fatal("expected overlap error between workspace pools 9200-9250 and 9240-9299")
	}
}
