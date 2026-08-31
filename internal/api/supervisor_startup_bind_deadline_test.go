package api

import (
	"testing"

	"mcp-local-hub/internal/config"
)

// TestSupervisorDaemonsFromPlan_CopiesStartupBindDeadline is the P1b plumb guard:
// the frozen effective start deadline must flow into the derived
// SupervisorDaemon descriptor the supervisor's liveness sweep reads.
func TestSupervisorDaemonsFromPlan_CopiesStartupBindDeadline(t *testing.T) {
	m := &config.ServerManifest{
		Name:      "slowsvc",
		Kind:      config.KindGlobal,
		Transport: config.TransportNativeHTTP,
		Command:   "go", // on PATH under `go test`
		Daemons: []config.DaemonSpec{
			{Name: "default", Port: 9140, StartupBindDeadlineSeconds: 240},
			{Name: "fast", Port: 9141}, // zero → default resolution downstream
		},
	}
	rows, err := supervisorDaemonsFromPlan(m, testSupervisorIntentPlan(m, ""), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("supervisorDaemonsFromPlan = %d rows, want 2", len(rows))
	}
	byDaemon := map[string]SupervisorDaemon{}
	for _, r := range rows {
		byDaemon[r.Daemon] = r
	}
	if got := byDaemon["default"].StartupBindDeadlineSeconds; got != 240 {
		t.Fatalf("default daemon StartupBindDeadlineSeconds = %d, want 240 (copied from DaemonSpec)", got)
	}
	if got := byDaemon["fast"].StartupBindDeadlineSeconds; got != 60 {
		t.Fatalf("fast daemon StartupBindDeadlineSeconds = %d, want frozen effective default 60", got)
	}
}

// TestSerenaProxyDescriptor_StampsStartupBindDeadline120 verifies the serena
// dynamic-pool builder stamps the explicit 120s first-bind deadline on every
// per-workspace proxy descriptor (serena's language-server subprocess is slow to
// bind).
func TestSerenaProxyDescriptor_StampsStartupBindDeadline120(t *testing.T) {
	wsPath := t.TempDir()
	ws := WorkspaceEntry{
		WorkspacePath: wsPath,
		WorkspaceKey:  WorkspaceKey(wsPath),
		Language:      SerenaLanguageSentinel,
		Port:          9151,
	}
	m := &config.ServerManifest{
		Name:      "serena",
		Kind:      config.KindWorkspaceScoped,
		Transport: config.TransportNativeHTTP,
		Command:   "uvx",
		BaseArgs:  []string{"serena"},
		DaemonTemplate: &config.DaemonTemplate{
			Context:           "ide-assistant",
			PortPool:          &config.PortPool{Start: 9151, End: 9199},
			ExtraArgsTemplate: []string{"--project", "${workspace.path}"},
		},
	}
	rows := BuildSupervisorDaemonsForSerena(m, []WorkspaceEntry{ws}, "hash", "mcphub")
	if len(rows) != 1 {
		t.Fatalf("BuildSupervisorDaemonsForSerena = %d rows, want 1", len(rows))
	}
	if got := rows[0].StartupBindDeadlineSeconds; got != 120 {
		t.Fatalf("serena-proxy StartupBindDeadlineSeconds = %d, want 120", got)
	}
}

// TestBuildSupervisorDaemonForLSP_StampsStartupBindDeadline120 is the P2c
// adjacent fix (mirrors the serena builder): the workspace LSP proxy descriptor
// must carry the same explicit 120s first-bind deadline (field exists on
// SupervisorDaemon since #488) so a slow-to-bind LSP proxy is not
// terminate-first-then-respawned mid-startup by the liveness sweep.
func TestBuildSupervisorDaemonForLSP_StampsStartupBindDeadline120(t *testing.T) {
	entry := WorkspaceEntry{
		WorkspaceKey:  "abcd1234",
		WorkspacePath: t.TempDir(),
		Language:      "python",
		Port:          9210,
	}
	d := BuildSupervisorDaemonForLSP(entry, "mcphub")
	if got := d.StartupBindDeadlineSeconds; got != 120 {
		t.Fatalf("LSP workspace-proxy StartupBindDeadlineSeconds = %d, want 120", got)
	}
	// Sanity: the descriptor is still the workspace-proxy launcher on its port.
	if d.Port != 9210 {
		t.Fatalf("Port = %d, want 9210", d.Port)
	}
	if len(d.Args) == 0 || d.Args[0] != "daemon" {
		t.Fatalf("Args = %v, want a `daemon workspace-proxy ...` launcher argv", d.Args)
	}
}
