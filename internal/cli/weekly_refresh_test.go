package cli

import (
	"bytes"
	"strings"
	"testing"
)

// TestWeeklyRefreshCmd_WiresIntoRoot verifies the hidden subcommand is
// attached to the root command tree — the shared scheduler task that M4
// creates invokes exactly this name.
func TestWeeklyRefreshCmd_WiresIntoRoot(t *testing.T) {
	root := NewRootCmd()
	found := false
	for _, c := range root.Commands() {
		if c.Name() == "workspace-weekly-refresh" {
			found = true
			if !c.Hidden {
				t.Errorf("workspace-weekly-refresh should be Hidden=true (internal command)")
			}
			break
		}
	}
	if !found {
		t.Error("workspace-weekly-refresh subcommand not wired into root")
	}
}

// TestWeeklyRefreshCmd_HasJSONFlag confirms --json is wired for the
// machine-readable surface callers may use.
func TestWeeklyRefreshCmd_HasJSONFlag(t *testing.T) {
	c := newWeeklyRefreshCmdReal()
	flag := c.Flags().Lookup("json")
	if flag == nil {
		t.Fatal("--json flag missing")
	}
	if flag.DefValue != "false" {
		t.Errorf("--json default = %q, want false", flag.DefValue)
	}
}

// TestWeeklyRefreshCmd_InvokesWeeklyRefreshAll exercises the end-to-end
// CLI execution against an empty registry (no entries). Since
// WeeklyRefreshAll reads from the default registry path, we point it at a
// fresh temp dir so the call produces a clean zero-restart report.
func TestWeeklyRefreshCmd_InvokesWeeklyRefreshAll(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	buf := &bytes.Buffer{}
	c := newWeeklyRefreshCmdReal()
	c.SetOut(buf)
	c.SetErr(buf)
	c.SilenceUsage = true
	c.SetArgs([]string{})
	if err := c.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "Weekly refresh: restarted 0 task(s)") {
		t.Errorf("expected restart-count line; got:\n%s", out)
	}
}

// TestWeeklyRefreshCmd_JSONOutput confirms --json produces a valid JSON
// report shape with the fields callers need.
func TestWeeklyRefreshCmd_JSONOutput(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	buf := &bytes.Buffer{}
	c := newWeeklyRefreshCmdReal()
	c.SetOut(buf)
	c.SetErr(buf)
	c.SilenceUsage = true
	c.SetArgs([]string{"--json"})
	if err := c.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	got := strings.TrimSpace(buf.String())
	if !strings.HasPrefix(got, "{") || !strings.HasSuffix(got, "}") {
		t.Errorf("expected JSON object; got:\n%s", got)
	}
	// Key shape — "restarted" is always present in the JSON report.
	if !strings.Contains(got, "\"restarted\"") {
		t.Errorf("JSON output missing 'restarted' key:\n%s", got)
	}
}
