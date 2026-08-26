package cli

import (
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
)

func TestRenderMCPReadinessCellUsesTypedStateOnly(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result api.MCPReadinessResult
		want   string
	}{
		{"not checked", api.NewMCPReadinessNotChecked(), "NOT CHECKED"},
		{"not applicable", api.NewMCPReadinessNotApplicable(), "N/A"},
		{"ready", api.MCPReadinessResult{SchemaVersion: "mcp-readiness-v1", State: api.MCPReadinessReady, ToolCount: 3}, "READY 3"},
		{"unready", api.MCPReadinessResult{SchemaVersion: "mcp-readiness-v1", State: api.MCPReadinessUnready, FailureID: "MCP_BACKING_PROTOCOL_UNSUPPORTED", Detail: "raw-secret-must-not-render"}, "UNREADY MCP_BACKING_PROTOCOL_UNSUPPORTED"},
		{"dual failure uses primary only", api.MCPReadinessResult{SchemaVersion: "mcp-readiness-v1", State: api.MCPReadinessUnready, FailureID: "MCP_COMPAT_CHILD_RESPONSE_INVALID", SecondaryStage: "session_cleanup", SecondaryFailureID: "MCP_HEALTH_SESSION_CLEANUP_FAILED", Detail: "raw-secret-must-not-render"}, "UNREADY MCP_COMPAT_CHILD_RESPONSE_INVALID"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := renderMCPReadinessCell(tc.result)
			if got != tc.want || strings.Contains(got, "raw-secret") {
				t.Fatalf("cell = %q, want %q", got, tc.want)
			}
		})
	}
}
