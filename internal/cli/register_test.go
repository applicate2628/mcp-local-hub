package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
)

// TestRegisterCmd_AcceptsOnlyWorkspaceArgDefaultsAll verifies `register <ws>`
// with no language positional args is accepted by cobra's Args validator.
// (The register implementation delegates default-all semantics to
// api.Register; we're only checking the CLI surface here.)
func TestRegisterCmd_AcceptsOnlyWorkspaceArgDefaultsAll(t *testing.T) {
	c := newRegisterCmdReal()
	// MinimumNArgs(1) should accept exactly 1 arg — the cobra layer itself
	// must not reject this shape. We don't execute RunE (that would try to
	// load manifests and write to a real registry); instead we validate
	// the cobra.Args function directly.
	if err := c.Args(c, []string{"/some/workspace"}); err != nil {
		t.Errorf("cobra Args rejected single-arg form: %v", err)
	}
}

func TestRegisterCmd_RequiresAtLeastOneArg(t *testing.T) {
	c := newRegisterCmdReal()
	if err := c.Args(c, []string{}); err == nil {
		t.Error("expected error for zero-arg invocation")
	}
}

func TestRegisterCmd_ExplicitLanguagesAccepted(t *testing.T) {
	c := newRegisterCmdReal()
	if err := c.Args(c, []string{"/ws", "python", "typescript"}); err != nil {
		t.Errorf("cobra Args rejected explicit languages: %v", err)
	}
}

func TestRegisterCmd_HasNoWeeklyRefreshFlag(t *testing.T) {
	c := newRegisterCmdReal()
	flag := c.Flags().Lookup("no-weekly-refresh")
	if flag == nil {
		t.Fatal("--no-weekly-refresh flag missing")
	}
	if flag.DefValue != "false" {
		t.Errorf("--no-weekly-refresh default = %q, want false", flag.DefValue)
	}
}

func TestUnregisterCmd_RequiresAtLeastOneArg(t *testing.T) {
	c := newUnregisterCmdReal()
	if err := c.Args(c, []string{}); err == nil {
		t.Error("expected error for zero-arg invocation")
	}
}

func TestUnregisterCmd_AcceptsWorkspaceOnly(t *testing.T) {
	c := newUnregisterCmdReal()
	if err := c.Args(c, []string{"/ws"}); err != nil {
		t.Errorf("cobra Args rejected single-arg form: %v", err)
	}
}

func TestWorkspacesCmd_EmptyRegistryPrintsHeader(t *testing.T) {
	// Point the registry at a fresh empty temp dir via env override.
	// This works because DefaultRegistryPath consults LOCALAPPDATA on
	// Windows and XDG_STATE_HOME otherwise; setting both to a fresh
	// temp dir guarantees the registry file does not exist.
	t.Setenv("LOCALAPPDATA", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	buf := &bytes.Buffer{}
	c := newWorkspacesCmdReal()
	c.SetOut(buf)
	c.SetErr(buf)
	c.SilenceUsage = true
	c.SetArgs([]string{})
	if err := c.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	out := buf.String()
	// Required columns in header.
	for _, want := range []string{"WORKSPACE", "LANG", "PORT", "BACKEND", "LIFECYCLE", "LAST_USED", "PATH"} {
		if !strings.Contains(out, want) {
			t.Errorf("header missing column %q; got:\n%s", want, out)
		}
	}
}

func TestWorkspacesCmd_JSONOutput(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	buf := &bytes.Buffer{}
	c := newWorkspacesCmdReal()
	c.SetOut(buf)
	c.SetErr(buf)
	c.SilenceUsage = true
	c.SetArgs([]string{"--json"})
	if err := c.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	got := strings.TrimSpace(buf.String())
	// Empty registry → JSON array "[]".
	if got != "[]" {
		// Accept "[]" with trailing newline and/or pretty formatting.
		if !strings.HasPrefix(got, "[") || !strings.HasSuffix(got, "]") {
			t.Errorf("expected JSON array, got:\n%s", got)
		}
	}
	// Parse to confirm valid JSON.
	var arr []api.WorkspaceEntry
	if err := json.Unmarshal([]byte(got), &arr); err != nil {
		t.Errorf("JSON invalid: %v\noutput: %s", err, got)
	}
}

func TestWorkspacesCmd_PopulatedPrintsLifecycleColumn(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LOCALAPPDATA", dir)
	t.Setenv("XDG_STATE_HOME", dir)
	// Seed the registry with entries in different lifecycle states so the
	// rendered table proves LIFECYCLE is surfaced per-row.
	regPath, err := api.DefaultRegistryPath()
	if err != nil {
		t.Fatal(err)
	}
	reg := api.NewRegistry(regPath)
	reg.Put(api.WorkspaceEntry{
		WorkspaceKey: "ws000001", WorkspacePath: "/ws/one", Language: "python",
		Backend: "mcp-language-server", Port: 9200, TaskName: "tP",
		Lifecycle: api.LifecycleConfigured,
	})
	reg.Put(api.WorkspaceEntry{
		WorkspaceKey: "ws000001", WorkspacePath: "/ws/one", Language: "typescript",
		Backend: "mcp-language-server", Port: 9201, TaskName: "tT",
		Lifecycle: api.LifecycleActive, LastToolsCallAt: time.Now().Add(-5 * time.Minute),
	})
	reg.Put(api.WorkspaceEntry{
		WorkspaceKey: "ws000002", WorkspacePath: "/ws/two", Language: "go",
		Backend: "gopls-mcp", Port: 9210, TaskName: "tG",
		Lifecycle: api.LifecycleMissing, LastError: "gopls not on PATH",
	})
	if err := reg.Save(); err != nil {
		t.Fatal(err)
	}

	buf := &bytes.Buffer{}
	c := newWorkspacesCmdReal()
	c.SetOut(buf)
	c.SetErr(buf)
	c.SilenceUsage = true
	c.SetArgs([]string{})
	if err := c.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		api.LifecycleConfigured,
		api.LifecycleActive,
		api.LifecycleMissing,
		"python",
		"typescript",
		"go",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in table output; got:\n%s", want, out)
		}
	}
}

func TestRelativeLastUsed_ZeroReturnsDash(t *testing.T) {
	if got := relativeLastUsed(time.Time{}); got != "-" {
		t.Errorf("zero time: got %q, want %q", got, "-")
	}
}

func TestRelativeLastUsed_RecentRendersSecondsMinutesHours(t *testing.T) {
	cases := []struct {
		elapsed time.Duration
		want    string
	}{
		{10 * time.Second, "10s ago"},
		{5 * time.Minute, "5m ago"},
		{3 * time.Hour, "3h ago"},
		{25 * time.Hour, "1d ago"},
	}
	for _, tc := range cases {
		got := relativeLastUsed(time.Now().Add(-tc.elapsed))
		if got != tc.want {
			t.Errorf("elapsed=%s: got %q, want %q", tc.elapsed, got, tc.want)
		}
	}
}

func TestStateOrDash(t *testing.T) {
	if got := stateOrDash(""); got != "-" {
		t.Errorf("empty: got %q, want %q", got, "-")
	}
	if got := stateOrDash(api.LifecycleActive); got != api.LifecycleActive {
		t.Errorf("active: got %q, want %q", got, api.LifecycleActive)
	}
}

func TestRegisterCmd_WiredIntoRoot(t *testing.T) {
	root := NewRootCmd()
	// Walk subcommands and confirm all three new commands exist.
	names := map[string]bool{}
	for _, c := range root.Commands() {
		names[c.Name()] = true
	}
	for _, want := range []string{"register", "unregister", "workspaces"} {
		if !names[want] {
			t.Errorf("subcommand %q not wired into root", want)
		}
	}
}
