package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
)

func TestInstallAllFailedLegacyResultRendersCarriedSettlementBeforeError(t *testing.T) {
	original := installAllWithOpts
	t.Cleanup(func() { installAllWithOpts = original })
	calls := 0
	installAllWithOpts = func(opts api.InstallAllOpts) []api.InstallResult {
		calls++
		if !opts.DryRun {
			t.Fatalf("legacy bulk opts dry-run = false, want true")
		}
		return []api.InstallResult{{
			Server: "serena", Err: errors.New("injected legacy bulk failure"),
			ClientConfigSettlements: []api.ClientConfigSettlementV1{{
				SchemaVersion: "client-config-settlement-v1", Operation: "install", Phase: "settled",
				Client: "codex-cli", LogicalSource: "serena", TargetEntry: "serena-mcphub",
				WriteTarget: "codex_global", DesiredTransport: "http", CollisionReason: "cross_layer_opposite_transport",
				Action: "relocate", Outcome: "committed", Readback: "exact",
			}},
		}}
	}

	cmd := newInstallCmdReal()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--all", "--dry-run"})
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	err := cmd.Execute()
	if err == nil || err.Error() != "1 of 1 install(s) failed" {
		t.Fatalf("Execute error = %v, want unchanged bulk failure", err)
	}
	if calls != 1 {
		t.Fatalf("legacy bulk calls = %d, want 1", calls)
	}
	if !strings.Contains(stdout.String(), `"schema_version":"client-config-settlement-v1"`) ||
		!strings.Contains(stdout.String(), `"target_entry":"serena-mcphub"`) {
		t.Fatalf("stdout omitted carried settlement row: %q", stdout.String())
	}
	if !strings.HasSuffix(stdout.String(), "✗ serena: injected legacy bulk failure\n") {
		t.Fatalf("stdout = %q, want unchanged per-server error after row", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty because Cobra routes the existing bulk row stream to stdout", stderr.String())
	}
}
