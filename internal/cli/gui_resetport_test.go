// internal/cli/gui_resetport_test.go — Phase 5 Task 5.3 tests.

package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
	"mcp-local-hub/internal/gui"
)

// resetPortHermeticHome redirects HOME/USERPROFILE (and the
// XDG/LOCALAPPDATA companions) to a fresh temp dir so the B2 gate-ON
// guard's api.GatedOnClients() scan reads sandbox client configs, never
// the developer's real ones. Returns the temp home so seeders can write
// a client config into it.
func resetPortHermeticHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("LOCALAPPDATA", filepath.Join(home, "AppData", "Local"))
	// Every remaining client-config path env var (%APPDATA% —
	// defaultVSCodeConfigPath reads %APPDATA%\Code\User\mcp.json, so without it
	// the gate-ON scan reads the developer's REAL live vscode config —
	// $XDG_CONFIG_HOME, the $MIMOCODE_* profile set, %ProgramData%,
	// $COPILOT_HOME, $KIMI_CODE_HOME) is neutralized by the package's one owner.
	neutralizeClientConfigPathEnv(t, home)
	// This fixture additionally wants the SINGLE-layer mimocode shape: it only
	// needs an empty sandbox client set, not the multi-layer read/write split.
	t.Setenv("MIMOCODE_DISABLE_CLAUDE_CODE_MCP", "1")
	return home
}

// TestGuiResetPortClearsPortKeepsInstanceID pins the spec contract:
// `mcphub gui --reset-port --yes` clears `hub-mcp.endpoint.json` Port
// to 0 while preserving instance_id + StartedAt, exits 0 without
// binding any listener, and emits the credential-rotation warning.
func TestGuiResetPortClearsPortKeepsInstanceID(t *testing.T) {
	root := t.TempDir()
	restore := api.SetDaemonStateRootForTest(root)
	t.Cleanup(restore)

	// B2 guard hermeticity: --reset-port now calls api.GatedOnClients()
	// which reads every supported client's on-disk config under $HOME.
	// Redirect HOME/USERPROFILE to an empty temp dir so the guard sees a
	// gate-OFF host (no mcphub-hub entry) and the happy path proceeds —
	// without this the test would spuriously hit the exit-8 refusal if
	// the developer's real home has a gate-ON client.
	resetPortHermeticHome(t)

	// codex bot phase5 r7 P2 closure on PR #160 added a hub-running
	// check before --reset-port mutates endpoint state. Route the
	// single-instance lock through the test-only pidport dir so the
	// test doesn't lock the user's real %LOCALAPPDATA% pidport.
	pidportDir := apitest.HardenedTempDir(t)
	t.Setenv("MCPHUB_GUI_TEST_PIDPORT_DIR", pidportDir)

	// Seed endpoint state so reset has something to clear.
	// On Windows, the DACL allowlist gate (Phase 1 §"State-dir DACL
	// allowlist verifier") rejects t.TempDir() because %TEMP%
	// inherits a non-allowlist SID via Authenticated Users. The
	// hardenedTempDir helper in internal/api package handles this
	// case but is package-private. Skip when EnsureHubEndpoint
	// surfaces that DACL rejection — the full state-mutation flow
	// is exercised by api-package tests with hardenedTempDir; this
	// test covers the CLI surface specifically.
	originalEp, err := api.EnsureHubEndpoint(9120, 12345)
	if err != nil {
		if strings.Contains(err.Error(), "DACL") || strings.Contains(err.Error(), "not single-user safe") {
			t.Skipf("skipping state-mutation test on host where %%TEMP%% fails DACL allowlist gate: %v", err)
		}
		t.Fatalf("EnsureHubEndpoint: %v", err)
	}
	if originalEp.InstanceID == "" {
		t.Fatalf("seeded endpoint has empty instance_id (test setup broken)")
	}

	c := newGuiCmdRealForTest()
	var stdout, stderr bytes.Buffer
	c.SetOut(&stdout)
	c.SetErr(&stderr)
	c.SetArgs([]string{"--reset-port", "--yes"})
	if err := c.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("--reset-port --yes: %v\nstderr:\n%s", err, stderr.String())
	}

	// State assertion: Port=0, instance_id preserved, StartedAt preserved.
	reloaded, err := api.LoadHubEndpoint()
	if err != nil {
		t.Fatalf("LoadHubEndpoint after reset: %v", err)
	}
	if reloaded.Port != 0 {
		t.Errorf("Port after --reset-port = %d, want 0", reloaded.Port)
	}
	if reloaded.InstanceID != originalEp.InstanceID {
		t.Errorf("instance_id rotated by --reset-port: %q -> %q", originalEp.InstanceID, reloaded.InstanceID)
	}
	if reloaded.StartedAt != originalEp.StartedAt {
		t.Errorf("StartedAt rotated by --reset-port: %q -> %q", originalEp.StartedAt, reloaded.StartedAt)
	}

	// Credential-rotation guidance must appear on stdout.
	out := stdout.String()
	if !strings.Contains(out, "credentials may have leaked") {
		t.Errorf("stdout missing credential-rotation warning:\n%s", out)
	}
	if !strings.Contains(out, "regenerate-token") {
		t.Errorf("stdout missing regenerate-token guidance:\n%s", out)
	}
	if !strings.Contains(out, "regenerate-instance-id") {
		t.Errorf("stdout missing regenerate-instance-id guidance:\n%s", out)
	}
}

// TestGuiResetPortNonTTYRequiresYes pins the non-TTY guard: running
// --reset-port without --yes in a non-TTY context exits 6 (matches
// the existing forceExitError code 6 convention).
func TestGuiResetPortNonTTYRequiresYes(t *testing.T) {
	root := t.TempDir()
	restore := api.SetDaemonStateRootForTest(root)
	t.Cleanup(restore)

	pidportDir := apitest.HardenedTempDir(t)
	t.Setenv("MCPHUB_GUI_TEST_PIDPORT_DIR", pidportDir)

	c := newGuiCmdRealForTest()
	var stdout, stderr bytes.Buffer
	c.SetOut(&stdout)
	c.SetErr(&stderr)
	c.SetIn(bytes.NewReader(nil)) // non-terminal reader
	c.SetArgs([]string{"--reset-port"})
	err := c.ExecuteContext(context.Background())
	if err == nil {
		t.Fatalf("expected non-TTY without --yes to exit non-zero")
	}
	var fe *forceExitError
	if !errors.As(err, &fe) || fe.ExitCode() != 6 {
		t.Errorf("want forceExitError code 6, got %T %v", err, err)
	}
	// Endpoint must be untouched on the reject path — the persisted
	// state file should not even exist (we never wrote to it).
	if _, err := api.LoadHubEndpoint(); err == nil {
		t.Errorf("endpoint file written despite non-TTY rejection")
	}
}

// TestGuiResetPortRefusedWhenHubRunning pins the codex bot phase5
// r7 P2 closure on PR #160: --reset-port MUST refuse when another
// `mcphub gui` instance is currently holding the single-instance
// lock (and therefore its hub-mcp listener is still bound on the
// persisted port). Clearing endpoint.json Port to 0 in that state
// would leave the live listener on the old port but tell downstream
// commands (regenerate-token, install --reconcile-hub-mode) that
// the hub isn't running, silently dropping the token reload.
func TestGuiResetPortRefusedWhenHubRunning(t *testing.T) {
	root := t.TempDir()
	restore := api.SetDaemonStateRootForTest(root)
	t.Cleanup(restore)

	// The hub-running refusal is checked BEFORE the gate-ON guard
	// (the single-instance lock acquire precedes GatedOnClients), but
	// keep the home hermetic regardless so the test is independent of
	// the developer's real client configs.
	resetPortHermeticHome(t)

	pidportDir := apitest.HardenedTempDir(t)
	t.Setenv("MCPHUB_GUI_TEST_PIDPORT_DIR", pidportDir)
	pidportPath := filepath.Join(pidportDir, "gui.pidport")

	// Simulate a live GUI instance holding the single-instance lock.
	incumbent, err := gui.AcquireSingleInstanceAt(pidportPath, 9125)
	if err != nil {
		t.Fatalf("simulate incumbent lock: %v", err)
	}
	defer incumbent.Release()

	// Seed endpoint state. ResetHubPort should NOT touch it.
	originalEp, err := api.EnsureHubEndpoint(9120, 12345)
	if err != nil {
		if strings.Contains(err.Error(), "DACL") || strings.Contains(err.Error(), "not single-user safe") {
			t.Skipf("skipping state-mutation test on host where %%TEMP%% fails DACL allowlist gate: %v", err)
		}
		t.Fatalf("EnsureHubEndpoint: %v", err)
	}

	c := newGuiCmdRealForTest()
	var stdout, stderr bytes.Buffer
	c.SetOut(&stdout)
	c.SetErr(&stderr)
	c.SetArgs([]string{"--reset-port", "--yes"})
	err = c.ExecuteContext(context.Background())
	if err == nil {
		t.Fatalf("expected refusal when incumbent holds single-instance lock; got success")
	}
	var fe *forceExitError
	if !errors.As(err, &fe) || fe.ExitCode() != 3 {
		t.Errorf("want forceExitError code 3, got %T %v", err, err)
	}
	if !strings.Contains(stderr.String(), "another `mcphub gui` is running") {
		t.Errorf("stderr should explain busy state; got %q", stderr.String())
	}

	// Endpoint must be UNCHANGED — Port preserved at 9120, not 0.
	reloaded, err := api.LoadHubEndpoint()
	if err != nil {
		t.Fatalf("LoadHubEndpoint after refusal: %v", err)
	}
	if reloaded.Port != originalEp.Port {
		t.Errorf("endpoint.Port mutated despite refusal: %d -> %d", originalEp.Port, reloaded.Port)
	}
}

// TestGuiResetPortRefusedWhenClientGatedOn pins the B2 footgun guard
// (work-items/backlog/2026-06-16-hub-port-drift-on-reset-port.md):
// `--reset-port` MUST refuse (exit 8) while any client is gate-ON (its
// config carries the reserved `mcphub-hub` aggregate entry). Clearing
// the persisted port to 0 in that state would orphan every gated client
// URL (connection refused for ALL aggregated servers after the next
// ephemeral re-bind). The endpoint port must be left UNTOUCHED.
func TestGuiResetPortRefusedWhenClientGatedOn(t *testing.T) {
	root := t.TempDir()
	restore := api.SetDaemonStateRootForTest(root)
	t.Cleanup(restore)

	home := resetPortHermeticHome(t)

	pidportDir := apitest.HardenedTempDir(t)
	t.Setenv("MCPHUB_GUI_TEST_PIDPORT_DIR", pidportDir)

	// Seed claude-code gate-ON: a mcphub-hub aggregate entry, exactly
	// the shape the gate-ON reconciler writes.
	cfg := filepath.Join(home, ".claude.json")
	body := `{"mcpServers":{"mcphub-hub":{"url":"http://127.0.0.1:3439/clients/claude-code/mcp","type":"http"}}}`
	if err := os.WriteFile(cfg, []byte(body), 0600); err != nil {
		t.Fatalf("seed gate-ON claude-code config: %v", err)
	}

	// Seed endpoint state so we can prove the refusal left it untouched.
	originalEp, err := api.EnsureHubEndpoint(9120, 12345)
	if err != nil {
		if strings.Contains(err.Error(), "DACL") || strings.Contains(err.Error(), "not single-user safe") {
			t.Skipf("skipping state-mutation test on host where %%TEMP%% fails DACL allowlist gate: %v", err)
		}
		t.Fatalf("EnsureHubEndpoint: %v", err)
	}

	c := newGuiCmdRealForTest()
	var stdout, stderr bytes.Buffer
	c.SetOut(&stdout)
	c.SetErr(&stderr)
	c.SetArgs([]string{"--reset-port", "--yes"})
	err = c.ExecuteContext(context.Background())
	if err == nil {
		t.Fatalf("expected refusal when a client is gate-ON; got success")
	}
	var fe *forceExitError
	if !errors.As(err, &fe) || fe.ExitCode() != 8 {
		t.Fatalf("want forceExitError code 8, got %T %v\nstderr:\n%s", err, err, stderr.String())
	}
	if !strings.Contains(stderr.String(), "gate-ON") {
		t.Errorf("stderr should explain the gate-ON refusal; got %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "claude-code") {
		t.Errorf("stderr should name the gated client; got %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "reconcile-hub-mode") {
		t.Errorf("stderr should point to the reconcile recovery; got %q", stderr.String())
	}

	// Endpoint must be UNTOUCHED — Port preserved at 9120, not 0.
	reloaded, err := api.LoadHubEndpoint()
	if err != nil {
		t.Fatalf("LoadHubEndpoint after refusal: %v", err)
	}
	if reloaded.Port != originalEp.Port {
		t.Errorf("endpoint.Port mutated despite gate-ON refusal: %d -> %d", originalEp.Port, reloaded.Port)
	}
}

func setupResetPortDependencyTest(t *testing.T) string {
	t.Helper()
	root := apitest.HardenedTempDir(t)
	restore := api.SetDaemonStateRootForTest(root)
	t.Cleanup(restore)
	if _, err := api.EnsureHubEndpoint(9120, 12345); err != nil {
		t.Fatalf("seed hub endpoint: %v", err)
	}
	return resetPortHermeticHome(t)
}

func seedResetPortGroup(t *testing.T, name string) {
	t.Helper()
	if err := api.WriteGroups(api.GroupsConfig{
		Version: 1,
		Groups:  []api.Group{{Name: name, Servers: []string{"memory"}}},
	}); err != nil {
		t.Fatalf("seed groups.yaml: %v", err)
	}
}

func seedMalformedResetPortGroups(t *testing.T) {
	t.Helper()
	stateDir, err := api.DaemonStateDir()
	if err != nil {
		t.Fatalf("resolve daemon state dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "groups.yaml"), []byte("version: 2\n"), 0o600); err != nil {
		t.Fatalf("seed malformed groups.yaml: %v", err)
	}
}

func executeResetPortDependencyTest(t *testing.T) (error, string) {
	t.Helper()
	pidportDir := apitest.HardenedTempDir(t)
	t.Setenv("MCPHUB_GUI_TEST_PIDPORT_DIR", pidportDir)

	c := newGuiCmdRealForTest()
	var stderr bytes.Buffer
	c.SetErr(&stderr)
	c.SetArgs([]string{"--reset-port", "--yes"})
	return c.ExecuteContext(context.Background()), stderr.String()
}

func requireResetPortExit8(t *testing.T, err error, stderr string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected --reset-port refusal; stderr:\n%s", stderr)
	}
	var fe *forceExitError
	if !errors.As(err, &fe) || fe.ExitCode() != 8 {
		t.Fatalf("want forceExitError code 8, got %T %v\nstderr:\n%s", err, err, stderr)
	}
	if !strings.Contains(stderr, "--reset-port refused:") {
		t.Fatalf("stderr missing reset-port refusal header:\n%s", stderr)
	}
}

func TestGuiResetPortRefusedWhenGroupDependsOnHubPort(t *testing.T) {
	setupResetPortDependencyTest(t)
	seedResetPortGroup(t, "frontend")

	err, stderr := executeResetPortDependencyTest(t)
	requireResetPortExit8(t, err, stderr)
	if !strings.Contains(stderr, "frontend") || !strings.Contains(stderr, "/g/<group>/mcp") {
		t.Errorf("stderr should name the group and its pinned URL:\n%s", stderr)
	}
	if !strings.Contains(stderr, "no reconcile path rewrites group URLs") {
		t.Errorf("stderr should explain the group URL non-goal:\n%s", stderr)
	}
	if strings.Contains(stderr, "cannot be proven") {
		t.Errorf("dependent-only refusal must not include unreadable-source text:\n%s", stderr)
	}
}

func TestGuiResetPortRefusedWhenClientConfigUnreadable(t *testing.T) {
	home := setupResetPortDependencyTest(t)
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte("{"), 0o600); err != nil {
		t.Fatalf("seed malformed claude-code config: %v", err)
	}

	err, stderr := executeResetPortDependencyTest(t)
	requireResetPortExit8(t, err, stderr)
	if !strings.Contains(stderr, "client claude-code (config unreadable (parse/DACL))") {
		t.Errorf("stderr should name the unreadable client source:\n%s", stderr)
	}
	if !strings.Contains(stderr, "cannot be proven") {
		t.Errorf("stderr should explain the unknown safety state:\n%s", stderr)
	}
}

func TestGuiResetPortRefusedWhenGroupsFileUnreadable(t *testing.T) {
	setupResetPortDependencyTest(t)
	seedMalformedResetPortGroups(t)

	err, stderr := executeResetPortDependencyTest(t)
	requireResetPortExit8(t, err, stderr)
	if !strings.Contains(stderr, "groups groups.yaml (") {
		t.Errorf("stderr should name the unreadable groups source:\n%s", stderr)
	}
	if !strings.Contains(stderr, "cannot be proven") {
		t.Errorf("stderr should explain the unknown safety state:\n%s", stderr)
	}
}

func TestGuiResetPortRefusalNamesDependenciesAndUnreadableSources(t *testing.T) {
	home := setupResetPortDependencyTest(t)
	body := `{"mcpServers":{"mcphub-hub":{"url":"http://127.0.0.1:3439/clients/claude-code/mcp","type":"http"}}}`
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(body), 0o600); err != nil {
		t.Fatalf("seed gate-ON claude-code config: %v", err)
	}
	seedMalformedResetPortGroups(t)

	err, stderr := executeResetPortDependencyTest(t)
	requireResetPortExit8(t, err, stderr)
	if !strings.Contains(stderr, "claude-code") {
		t.Errorf("stderr should name the proved client dependency:\n%s", stderr)
	}
	if !strings.Contains(stderr, "groups groups.yaml (") || !strings.Contains(stderr, "cannot be proven") {
		t.Errorf("stderr should separately name the unreadable groups source:\n%s", stderr)
	}
}
