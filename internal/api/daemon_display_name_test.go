package api

import (
	"encoding/json"
	"testing"
)

// TestComputeDaemonDisplayName covers the three row classes the
// hash→name display feature targets: workspace serena, workspace LSP, and
// global daemons. Pure function — no scheduler, registry, or supervisor I/O.
func TestComputeDaemonDisplayName(t *testing.T) {
	cases := []struct {
		name      string
		taskName  string
		server    string
		daemon    string
		workspace string
		want      string
	}{
		{
			name:      "workspace serena → serena · project",
			taskName:  `\mcp-local-hub-serena-6935d24c`,
			server:    "serena",
			daemon:    "6935d24c",
			workspace: `d:\dev\VFEM`,
			want:      "serena · VFEM",
		},
		{
			name:      "workspace serena nested path uses basename",
			taskName:  `\mcp-local-hub-serena-b133f336`,
			server:    "serena",
			daemon:    "b133f336",
			workspace: `d:\dev\mcp-local-hub`,
			want:      "serena · mcp-local-hub",
		},
		{
			name:      "workspace serena forward-slash path",
			taskName:  "mcp-local-hub-serena-c3865a97",
			server:    "serena",
			daemon:    "c3865a97",
			workspace: "/home/dev/Orchestrarium",
			want:      "serena · Orchestrarium",
		},
		{
			name:      "workspace LSP → lang @ workspace",
			taskName:  `\mcp-local-hub-lsp-b133f336-go`,
			server:    "mcp-language-server",
			daemon:    "lsp-b133f336-go",
			workspace: `d:\dev\mcp-local-hub`,
			want:      "go @ mcp-local-hub",
		},
		{
			name:      "workspace LSP typescript basename",
			taskName:  "mcp-local-hub-lsp-8ce6b069-typescript",
			server:    "mcp-language-server",
			daemon:    "lsp-8ce6b069-typescript",
			workspace: `d:\dev\PaperPane\renderer`,
			want:      "typescript @ renderer",
		},
		{
			name:      "workspace LSP multi-segment language (vscode-css)",
			taskName:  "mcp-local-hub-lsp-5f2a28ff-vscode-css",
			server:    "mcp-language-server",
			daemon:    "lsp-5f2a28ff-vscode-css",
			workspace: `d:\dev\PaperPane`,
			want:      "vscode-css @ PaperPane",
		},
		{
			name:      "global daemon memory → empty (plain name)",
			taskName:  `\mcp-local-hub-memory-default`,
			server:    "memory",
			daemon:    "default",
			workspace: "",
			want:      "",
		},
		{
			name:      "global daemon time → empty",
			taskName:  `\mcp-local-hub-time-default`,
			server:    "time",
			daemon:    "default",
			workspace: "",
			want:      "",
		},
		{
			name:      "legacy global serena (no workspace) → empty",
			taskName:  `\mcp-local-hub-serena-claude`,
			server:    "serena",
			daemon:    "claude",
			workspace: "",
			want:      "",
		},
		{
			name:      "serena weekly-refresh (no workspace) → empty",
			taskName:  `\mcp-local-hub-serena-weekly-refresh`,
			server:    "serena",
			daemon:    "weekly-refresh",
			workspace: "",
			want:      "",
		},
		{
			name:      "whitespace-only workspace → empty",
			taskName:  `\mcp-local-hub-serena-abc12345`,
			server:    "serena",
			daemon:    "abc12345",
			workspace: "   ",
			want:      "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ComputeDaemonDisplayName(tc.taskName, tc.server, tc.daemon, tc.workspace)
			if got != tc.want {
				t.Errorf("ComputeDaemonDisplayName(%q, %q, %q, %q) = %q, want %q",
					tc.taskName, tc.server, tc.daemon, tc.workspace, got, tc.want)
			}
		})
	}
}

// TestComputeDaemonDisplayName_IPCRowMapping asserts the supervisor-IPC
// row-builder produces the friendly names end-to-end through
// decodeSupervisorIPCStatusResult — the production default path for bare
// `mcphub status` and the GUI Dashboard. Uses a synthetic status payload;
// no live supervisor is contacted.
func TestComputeDaemonDisplayName_IPCRowMapping(t *testing.T) {
	result := supervisorIPCStatusResult{
		State: "running",
		Daemons: []supervisorIPCStatusDaemon{
			{
				TaskName:  `\mcp-local-hub-serena-6935d24c`,
				Server:    "serena",
				Daemon:    "6935d24c",
				Workspace: `d:\dev\VFEM`,
				State:     "running",
			},
			{
				TaskName:  `\mcp-local-hub-lsp-b133f336-go`,
				Server:    "mcp-language-server",
				Daemon:    "lsp-b133f336-go",
				Workspace: `d:\dev\mcp-local-hub`,
				State:     "running",
			},
			{
				TaskName: `\mcp-local-hub-memory-default`,
				Server:   "memory",
				Daemon:   "default",
				State:    "running",
			},
		},
	}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal synthetic result: %v", err)
	}
	rows, err := decodeSupervisorIPCStatusResult(raw)
	if err != nil {
		t.Fatalf("decodeSupervisorIPCStatusResult: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("want 3 rows, got %d", len(rows))
	}
	want := map[string]string{
		`\mcp-local-hub-serena-6935d24c`: "serena · VFEM",
		`\mcp-local-hub-lsp-b133f336-go`: "go @ mcp-local-hub",
		`\mcp-local-hub-memory-default`:  "", // global: no display name
	}
	for _, r := range rows {
		w, ok := want[r.TaskName]
		if !ok {
			t.Errorf("unexpected row task=%q", r.TaskName)
			continue
		}
		if r.DisplayName != w {
			t.Errorf("task %q: DisplayName=%q, want %q", r.TaskName, r.DisplayName, w)
		}
	}
}
