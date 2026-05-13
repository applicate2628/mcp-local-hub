// internal/cli/gui_resetport_test.go — Phase 5 Task 5.3 tests.

package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
)

// TestGuiResetPortClearsPortKeepsInstanceID pins the spec contract:
// `mcphub gui --reset-port --yes` clears `hub-mcp.endpoint.json` Port
// to 0 while preserving instance_id + StartedAt, exits 0 without
// binding any listener, and emits the credential-rotation warning.
func TestGuiResetPortClearsPortKeepsInstanceID(t *testing.T) {
	root := t.TempDir()
	restore := api.SetDaemonStateRootForTest(root)
	t.Cleanup(restore)

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
