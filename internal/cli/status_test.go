package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
)

// TestStatusCLI_WorkspaceScopedFlagExists confirms the flag is wired and
// defaults to false (so regular `mcphub status` is unaffected).
func TestStatusCLI_WorkspaceScopedFlagExists(t *testing.T) {
	c := newStatusCmdReal()
	flag := c.Flags().Lookup("workspace-scoped")
	if flag == nil {
		t.Fatal("--workspace-scoped flag missing")
	}
	if flag.DefValue != "false" {
		t.Errorf("--workspace-scoped default = %q, want false", flag.DefValue)
	}
}

// TestStatusCLI_ForceMaterializeFlagExists confirms the flag is wired.
func TestStatusCLI_ForceMaterializeFlagExists(t *testing.T) {
	c := newStatusCmdReal()
	flag := c.Flags().Lookup("force-materialize")
	if flag == nil {
		t.Fatal("--force-materialize flag missing")
	}
	if flag.DefValue != "false" {
		t.Errorf("--force-materialize default = %q, want false", flag.DefValue)
	}
}

// TestFilterWorkspaceScoped asserts only rows with Language OR Lifecycle
// populated survive the filter.
func TestFilterWorkspaceScoped(t *testing.T) {
	rows := []api.DaemonStatus{
		{TaskName: "mcp-local-hub-serena-claude"},                          // global, filtered out
		{TaskName: "mcp-local-hub-lsp-abcd1234-python", Language: "python"}, // workspace-scoped, kept
		{TaskName: "mcp-local-hub-gdb-default"},                             // global, filtered out
		{TaskName: "mcp-local-hub-lsp-deadbeef-go", Lifecycle: api.LifecycleActive}, // workspace-scoped via lifecycle
	}
	got := filterWorkspaceScoped(rows)
	if len(got) != 2 {
		t.Fatalf("filter kept %d rows, want 2: %+v", len(got), got)
	}
	if got[0].Language != "python" {
		t.Errorf("row 0: Language = %q, want python", got[0].Language)
	}
	if got[1].Lifecycle != api.LifecycleActive {
		t.Errorf("row 1: Lifecycle = %q, want active", got[1].Lifecycle)
	}
}

// TestPrintWorkspaceScopedTable_HeaderContainsNewColumns verifies the
// workspace-scoped table header includes LIFECYCLE, LAST_USED, LAST_ERROR.
func TestPrintWorkspaceScopedTable_HeaderContainsNewColumns(t *testing.T) {
	buf := &bytes.Buffer{}
	c := newStatusCmdReal()
	c.SetOut(buf)
	rows := []api.DaemonStatus{
		{
			TaskName:        "mcp-local-hub-lsp-abcd1234-python",
			State:           "Running",
			Port:            9217,
			Language:        "python",
			Lifecycle:       api.LifecycleActive,
			LastToolsCallAt: time.Now().Add(-10 * time.Minute),
			LastError:       "",
		},
	}
	if err := printWorkspaceScopedTable(c, rows, false); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"LIFECYCLE", "LAST_USED", "LAST_ERROR", "python", "active", "10m ago"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in workspace-scoped output; got:\n%s", want, out)
		}
	}
}

// TestPrintDefaultStatusTable_StableHeader asserts the default (global)
// table header is unchanged — the invariant in the plan that global-daemon
// operators see stable output.
func TestPrintDefaultStatusTable_StableHeader(t *testing.T) {
	buf := &bytes.Buffer{}
	c := newStatusCmdReal()
	c.SetOut(buf)
	rows := []api.DaemonStatus{
		{TaskName: "mcp-local-hub-serena-claude", State: "Running", Port: 9121, NextRun: "N/A"},
	}
	if err := printDefaultStatusTable(c, rows, false); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	// Must have the pre-M5 header fields.
	for _, want := range []string{"NAME", "STATE", "PORT", "PID", "RAM(MB)", "UPTIME", "NEXT RUN"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in default status output; got:\n%s", want, out)
		}
	}
	// Must NOT have the workspace-scoped columns.
	for _, unwanted := range []string{"LIFECYCLE", "LAST_USED", "LAST_ERROR"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("global status output leaked %q column; got:\n%s", unwanted, out)
		}
	}
}

// TestRelativeStatusLastUsed_ZeroReturnsDash covers the zero-time path.
func TestRelativeStatusLastUsed_ZeroReturnsDash(t *testing.T) {
	if got := relativeStatusLastUsed(time.Time{}); got != "-" {
		t.Errorf("zero time: got %q, want %q", got, "-")
	}
}

// TestStatusCLI_ForceMaterializeRequiresHealth verifies the contract the CLI
// help promises: `--force-materialize` without `--health` errors out with a
// message that names both flags, so operators see the fix immediately.
func TestStatusCLI_ForceMaterializeRequiresHealth(t *testing.T) {
	// Invoke the validation directly via the API layer; this mirrors what
	// the CLI RunE does (api.StatusWithOpts propagates the error verbatim).
	a := api.NewAPI()
	_, err := a.StatusWithOpts(api.StatusOpts{ForceMaterialize: true, ProbeHealth: false})
	if err == nil {
		t.Fatal("StatusWithOpts must reject --force-materialize without --health")
	}
	if !strings.Contains(err.Error(), "--force-materialize") || !strings.Contains(err.Error(), "--health") {
		t.Errorf("error must name both flags; got: %v", err)
	}
}

// TestRenderHealthCell_DistinguishesProxyFromBackend asserts the cell
// rendering for workspace-scoped rows differentiates a synthetic-only
// probe from one that observed a materialized backend. Prevents an
// operator from mistaking `OK` on a lazy proxy for `the LSP binary is
// installed and responsive`.
func TestRenderHealthCell_DistinguishesProxyFromBackend(t *testing.T) {
	cases := []struct {
		name     string
		row      api.DaemonStatus
		wantCell string
	}{
		{
			name: "proxy_synth_no_materialize",
			row: api.DaemonStatus{
				Language:  "python",
				Lifecycle: api.LifecycleConfigured,
				Health:    &api.HealthProbe{OK: true, ToolCount: 7, Source: "proxy-synthetic"},
			},
			wantCell: "OK (synth)",
		},
		{
			name: "proxy_synth_backend_active",
			row: api.DaemonStatus{
				Language:  "python",
				Lifecycle: api.LifecycleActive,
				Health:    &api.HealthProbe{OK: true, ToolCount: 7, Source: "proxy-synthetic"},
			},
			wantCell: "OK (materialized)",
		},
		{
			name: "proxy_synth_backend_missing",
			row: api.DaemonStatus{
				Language:  "go",
				Lifecycle: api.LifecycleMissing,
				LastError: "exec: \"gopls\": not found",
				Health:    &api.HealthProbe{OK: true, ToolCount: 7, Source: "proxy-synthetic"},
			},
			wantCell: "OK (synth); backend missing",
		},
		{
			name: "proxy_synth_backend_failed",
			row: api.DaemonStatus{
				Language:  "go",
				Lifecycle: api.LifecycleFailed,
				LastError: "spawn failed",
				Health:    &api.HealthProbe{OK: true, ToolCount: 7, Source: "proxy-synthetic"},
			},
			wantCell: "OK (synth); backend ERR: spawn failed",
		},
		{
			name: "global_daemon_unchanged",
			row: api.DaemonStatus{
				Health: &api.HealthProbe{OK: true, ToolCount: 5}, // Source=""
			},
			wantCell: "OK (5)",
		},
		{
			name: "probe_error",
			row: api.DaemonStatus{
				Language: "python",
				Health:   &api.HealthProbe{OK: false, Err: "connection refused", Source: "proxy-synthetic"},
			},
			wantCell: "ERR: connection refused",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := renderHealthCell(tc.row)
			if got != tc.wantCell {
				t.Errorf("got %q, want %q", got, tc.wantCell)
			}
		})
	}
}
