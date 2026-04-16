package cli

import (
	"strings"
	"testing"

	"mcp-local-hub/internal/config"
)

// serenaLikeManifest returns a manifest resembling the Serena manifest:
// 3 daemons, weekly refresh, 4 client bindings (one shared daemon).
func serenaLikeManifest() *config.ServerManifest {
	return &config.ServerManifest{
		Name:      "serena",
		Kind:      config.KindGlobal,
		Transport: config.TransportNativeHTTP,
		Command:   "uvx",
		Daemons: []config.DaemonSpec{
			{Name: "claude", Port: 9121},
			{Name: "codex", Port: 9122},
			{Name: "antigravity", Port: 9123},
		},
		ClientBindings: []config.ClientBinding{
			{Client: "claude-code", Daemon: "claude", URLPath: "/mcp"},
			{Client: "codex-cli", Daemon: "codex", URLPath: "/mcp"},
			{Client: "antigravity", Daemon: "antigravity", URLPath: "/mcp"},
			{Client: "gemini-cli", Daemon: "antigravity", URLPath: "/mcp"}, // shared daemon
		},
		WeeklyRefresh: true,
	}
}

func TestBuildPlan_NoFilter_FullInstall(t *testing.T) {
	m := serenaLikeManifest()
	p, err := BuildPlan(m, "")
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	// 3 daemon tasks + 1 weekly refresh = 4 scheduler tasks.
	if len(p.SchedulerTasks) != 4 {
		t.Errorf("len(SchedulerTasks) = %d, want 4", len(p.SchedulerTasks))
	}
	// 4 client bindings.
	if len(p.ClientUpdates) != 4 {
		t.Errorf("len(ClientUpdates) = %d, want 4", len(p.ClientUpdates))
	}
	// Weekly refresh present.
	var sawWeekly bool
	for _, s := range p.SchedulerTasks {
		if strings.Contains(s.Name, "weekly-refresh") {
			sawWeekly = true
		}
	}
	if !sawWeekly {
		t.Error("weekly-refresh task missing in full install")
	}
}

func TestBuildPlan_SingleDaemonFilter_SkipsOthersAndWeeklyRefresh(t *testing.T) {
	m := serenaLikeManifest()
	p, err := BuildPlan(m, "codex")
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	// Only the codex scheduler task; weekly refresh is skipped for partial installs.
	if len(p.SchedulerTasks) != 1 {
		t.Errorf("len(SchedulerTasks) = %d, want 1 (got: %+v)", len(p.SchedulerTasks), p.SchedulerTasks)
	}
	if len(p.SchedulerTasks) >= 1 && !strings.HasSuffix(p.SchedulerTasks[0].Name, "-codex") {
		t.Errorf("task name %q, want suffix -codex", p.SchedulerTasks[0].Name)
	}
	// Only codex-cli binding (it's the only binding referencing daemon codex).
	if len(p.ClientUpdates) != 1 {
		t.Errorf("len(ClientUpdates) = %d, want 1 (got: %+v)", len(p.ClientUpdates), p.ClientUpdates)
	}
	if len(p.ClientUpdates) >= 1 && p.ClientUpdates[0].Client != "codex-cli" {
		t.Errorf("client = %q, want codex-cli", p.ClientUpdates[0].Client)
	}
	if len(p.ClientUpdates) >= 1 && !strings.Contains(p.ClientUpdates[0].URL, ":9122/") {
		t.Errorf("url = %q, want port 9122", p.ClientUpdates[0].URL)
	}
}

func TestBuildPlan_SharedDaemonFilter_IncludesAllReferencingBindings(t *testing.T) {
	m := serenaLikeManifest()
	// antigravity daemon is referenced by TWO bindings: antigravity + gemini-cli.
	p, err := BuildPlan(m, "antigravity")
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if len(p.SchedulerTasks) != 1 {
		t.Errorf("len(SchedulerTasks) = %d, want 1", len(p.SchedulerTasks))
	}
	if len(p.ClientUpdates) != 2 {
		t.Errorf("len(ClientUpdates) = %d, want 2 (antigravity + gemini-cli share the daemon)", len(p.ClientUpdates))
	}
	sawAG, sawGemini := false, false
	for _, u := range p.ClientUpdates {
		if u.Client == "antigravity" {
			sawAG = true
		}
		if u.Client == "gemini-cli" {
			sawGemini = true
		}
	}
	if !sawAG || !sawGemini {
		t.Errorf("expected both antigravity and gemini-cli bindings; got: %+v", p.ClientUpdates)
	}
}

func TestBuildPlan_UnknownDaemonFilter_Errors(t *testing.T) {
	m := serenaLikeManifest()
	_, err := BuildPlan(m, "does-not-exist")
	if err == nil {
		t.Fatal("expected error for unknown daemon filter, got nil")
	}
	if !strings.Contains(err.Error(), "does-not-exist") {
		t.Errorf("error should mention the unknown daemon name, got: %v", err)
	}
}
