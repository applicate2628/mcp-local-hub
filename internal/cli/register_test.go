package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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

func TestRegisterCmd_HasSupervisedFlag(t *testing.T) {
	c := newRegisterCmdReal()
	flag := c.Flags().Lookup("supervised")
	if flag == nil {
		t.Fatal("--supervised flag missing")
	}
	if flag.DefValue != "false" {
		t.Errorf("--supervised default = %q, want false", flag.DefValue)
	}
}

func TestRegisterManagedRouterAuthorizerDiscoveryDoesNotCreateState(t *testing.T) {
	base := t.TempDir()
	t.Setenv("LOCALAPPDATA", base)
	t.Setenv("XDG_STATE_HOME", base)
	stateDir := filepath.Join(base, "mcp-local-hub")
	if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
		t.Fatalf("precondition: state dir exists or stat failed unexpectedly: %v", err)
	}
	authorizer := registerManagedRouterAuthorizer()
	if authorizer == nil {
		t.Fatal("register managed-router authorizer is nil")
	}
	if got := authorizer(context.Background(), 0); got.Lease != nil || got.FailureClass != "port-invalid" {
		t.Fatalf("invalid-port authorization = %+v", got)
	}
	if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
		t.Fatalf("read-only authorizer discovery created state dir or stat failed unexpectedly: %v", err)
	}
}

func TestRegisterCmd_SchedulerlessRegisterEnsuresSupervisor(t *testing.T) {
	origScheduler := registerSchedulerUnavailableForHost
	origResolve := registerResolveMCPHubBinary
	origEnsure := registerEnsureSupervisorRunning
	t.Cleanup(func() {
		registerSchedulerUnavailableForHost = origScheduler
		registerResolveMCPHubBinary = origResolve
		registerEnsureSupervisorRunning = origEnsure
	})

	registerSchedulerUnavailableForHost = func() (bool, error) { return true, nil }
	registerResolveMCPHubBinary = func() (string, error) { return "testdata/mcphub.exe", nil }
	var gotBin string
	var gotStrict bool
	var gotWait time.Duration
	registerEnsureSupervisorRunning = func(ctx context.Context, mcphubBin string, strictMode bool, waitFor time.Duration) (*supervisorOwner, error) {
		gotBin = mcphubBin
		gotStrict = strictMode
		gotWait = waitFor
		return &supervisorOwner{spawned: false}, nil
	}

	c := newRegisterCmdReal()
	var stdout bytes.Buffer
	c.SetOut(&stdout)
	if err := ensureSupervisorForSchedulerlessRegister(c); err != nil {
		t.Fatalf("ensureSupervisorForSchedulerlessRegister: %v", err)
	}
	if gotBin != "testdata/mcphub.exe" || gotStrict {
		t.Fatalf("ensure args = bin %q strict %v, want resolved binary strict=false", gotBin, gotStrict)
	}
	if gotWait != 15*time.Second {
		t.Fatalf("wait = %s, want 15s", gotWait)
	}
	if !strings.Contains(stdout.String(), "supervisor: adopted for schedulerless LSP register") {
		t.Fatalf("stdout missing adopted message: %q", stdout.String())
	}
}

func TestRegisterCmd_SchedulerlessSupervisorFailureStopsBeforeRegister(t *testing.T) {
	origScheduler := registerSchedulerUnavailableForHost
	origResolve := registerResolveMCPHubBinary
	origEnsure := registerEnsureSupervisorRunning
	t.Cleanup(func() {
		registerSchedulerUnavailableForHost = origScheduler
		registerResolveMCPHubBinary = origResolve
		registerEnsureSupervisorRunning = origEnsure
	})

	registerSchedulerUnavailableForHost = func() (bool, error) { return true, nil }
	registerResolveMCPHubBinary = func() (string, error) { return "testdata/mcphub.exe", nil }
	registerEnsureSupervisorRunning = func(ctx context.Context, mcphubBin string, strictMode bool, waitFor time.Duration) (*supervisorOwner, error) {
		return nil, errors.New("IPC unavailable")
	}

	err := ensureSupervisorForSchedulerlessRegister(newRegisterCmdReal())
	if err == nil {
		t.Fatal("expected schedulerless supervisor failure")
	}
	if !strings.Contains(err.Error(), "schedulerless LSP register requires a running supervisor") ||
		!strings.Contains(err.Error(), "mcphub supervise") {
		t.Fatalf("error lacks schedulerless guidance: %v", err)
	}
}

func TestRegisterCmd_SchedulerCapableRegisterSkipsSupervisorEnsure(t *testing.T) {
	origScheduler := registerSchedulerUnavailableForHost
	origEnsure := registerEnsureSupervisorRunning
	t.Cleanup(func() {
		registerSchedulerUnavailableForHost = origScheduler
		registerEnsureSupervisorRunning = origEnsure
	})

	registerSchedulerUnavailableForHost = func() (bool, error) { return false, nil }
	registerEnsureSupervisorRunning = func(ctx context.Context, mcphubBin string, strictMode bool, waitFor time.Duration) (*supervisorOwner, error) {
		t.Fatal("scheduler-capable register must not ensure supervisor")
		return nil, nil
	}

	if err := ensureSupervisorForSchedulerlessRegister(newRegisterCmdReal()); err != nil {
		t.Fatalf("ensureSupervisorForSchedulerlessRegister: %v", err)
	}
}

func TestRegisterCLI_RetainsRawOperatorDiagnostic(t *testing.T) {
	c := newRegisterCmdReal()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	c.SetOut(&stdout)
	c.SetErr(&stderr)

	report := &api.RegisterReport{
		Workspace:    "/repo/alpha",
		WorkspaceKey: "abcd1234",
		Entries: []api.WorkspaceEntry{
			{Language: "go", Port: 9200, TaskName: "mcp-local-hub-lsp-abcd1234-go"},
		},
		Warnings: []string{"codex-cli remove direct LSP entry mcp-language-server-go failed: induced failure"},
	}
	printRegisterReport(c, report)

	if !strings.Contains(stdout.String(), "Registered 1 language(s) for workspace /repo/alpha (key abcd1234):") {
		t.Fatalf("stdout missing register summary:\n%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "warning: codex-cli remove direct LSP entry mcp-language-server-go failed: induced failure") {
		t.Fatalf("stderr missing warning:\n%s", stderr.String())
	}
}

func TestRegisterCLIPartialReportPrintsTypedCauseBeforeReturningError(t *testing.T) {
	const rawSentinel = `D:\local\operator --password=hunter2`
	operationErr := errors.New(rawSentinel)
	command := newRegisterCmdReal()
	var stderr bytes.Buffer
	command.SetErr(&stderr)

	report := &api.RegisterReport{Workspace: "project", WorkspaceKey: "key"}
	returned := finishRegisterCommand(command, report, operationErr)
	if !errors.Is(returned, operationErr) {
		t.Fatalf("returned error=%v, want original operation cause", returned)
	}
	output := stderr.String()
	if !strings.Contains(output, string(api.RegistrationCodeUnknown)) || !strings.Contains(output, rawSentinel) {
		t.Fatalf("local CLI diagnostic did not preserve code and cause: %q", output)
	}
}

func TestUnregisterCLIPartialReportPrintsTypedCauseBeforeReturningError(t *testing.T) {
	const rawSentinel = "unregister-local-cause"
	operationErr := errors.New(rawSentinel)
	command := newUnregisterCmdReal()
	var stderr bytes.Buffer
	command.SetErr(&stderr)

	report := &api.UnregisterReport{Workspace: "project", WorkspaceKey: "key"}
	returned := finishUnregisterCommand(command, report, []string{"go"}, operationErr)
	if !errors.Is(returned, operationErr) {
		t.Fatalf("returned error=%v, want original operation cause", returned)
	}
	output := stderr.String()
	if !strings.Contains(output, string(api.RegistrationCodeUnknown)) || !strings.Contains(output, rawSentinel) {
		t.Fatalf("local CLI diagnostic did not preserve code and cause: %q", output)
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
