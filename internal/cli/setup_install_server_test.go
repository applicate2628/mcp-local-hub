package cli

// Tests for the `mcphub setup --server <name>` headless single-server
// install flag (ROADMAP "setup --server headless flag").
//
// STATE SAFETY: these tests NEVER reach api.Install. They seed a hermetic
// manifest catalog via MCPHUB_MANIFEST_DIR_OVERRIDE (so validateSetupServerArg
// sees EXACTLY the seeded names), stub bootstrap/watchdog/router via the
// existing setup seams, and intercept the install step with the
// setupInstallServerFn seam. No binary copy, no Task Scheduler, no client
// configs, no supervisor — the real install path is never exercised, so the
// operator's live state files are untouched.
//
// NOTE TO RUNNER: do NOT run this in internal/cli on the live host without
// the seams below being active — they are what keeps it state-safe. The file
// is written but intentionally not executed in the worktree (state-wipe risk
// per the lane guardrails); run it centrally on a clean host.

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
)

// seedSetupInstallManifests seeds a hermetic installed-server catalog via
// MCPHUB_MANIFEST_DIR_OVERRIDE so ManifestList() (consulted by
// validateSetupServerArg) sees EXACTLY the named servers — no leakage from the
// binary's embedded shipped set. Mirrors seedSetupHyphenInstalledManifests.
func seedSetupInstallManifests(t *testing.T, names ...string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", dir)
	for _, name := range names {
		if err := os.MkdirAll(filepath.Join(dir, name), 0o700); err != nil {
			t.Fatalf("mkdir manifest %q: %v", name, err)
		}
		body := "name: " + name + "\nkind: global\ntransport: stdio-bridge\ncommand: go\n"
		if err := os.WriteFile(filepath.Join(dir, name, "manifest.yaml"), []byte(body), 0o600); err != nil {
			t.Fatalf("write manifest %q: %v", name, err)
		}
	}
}

// stubSetupSideEffects replaces the bootstrap / LSP-router / watchdog seams
// with no-ops so `setup` neither copies a binary nor touches Task Scheduler /
// client configs, and intercepts the install step. It returns a pointer to a
// string recording which server the (intercepted) install step was asked to
// install ("<not-called>" until the seam fires).
func stubSetupSideEffects(t *testing.T) *string {
	t.Helper()
	origBootstrap := setupBootstrapFn
	origRouter := setupLSPClientRouterFn
	origWatchdog := setupWatchdogFn
	origInstall := setupInstallServerFn
	t.Cleanup(func() {
		setupBootstrapFn = origBootstrap
		setupLSPClientRouterFn = origRouter
		setupWatchdogFn = origWatchdog
		setupInstallServerFn = origInstall
	})
	setupBootstrapFn = func(io.Writer) error { return nil }
	setupWatchdogFn = func(io.Writer, bool) error { return nil }
	setupLSPClientRouterFn = func(bool) (*api.LSPClientRouterReport, error) {
		return &api.LSPClientRouterReport{}, nil
	}

	installed := new(string)
	*installed = "<not-called>"
	setupInstallServerFn = func(server string, w io.Writer) error {
		*installed = server
		_, _ = io.WriteString(w, "installed "+server+"\n")
		return nil
	}
	return installed
}

// TestSetupCommand_ServerInstallsKnownManifest verifies that
// `mcphub setup --server <known>` validates and routes the name to the
// install seam (the same code path the install command uses).
func TestSetupCommand_ServerInstallsKnownManifest(t *testing.T) {
	seedSetupInstallManifests(t, "serena", "memory")
	installed := stubSetupSideEffects(t)

	stdout, _, err := runSetupCmd(t, "--server", "serena")
	if err != nil {
		t.Fatalf("setup --server serena: %v", err)
	}
	if *installed != "serena" {
		t.Fatalf("install seam got %q, want serena", *installed)
	}
	if !strings.Contains(stdout, "installed serena") {
		t.Fatalf("stdout missing install confirmation; got %q", stdout)
	}
}

// TestSetupCommand_ServerUnknownFailsLoudBeforeMutation verifies that an
// unknown --server value fails the whole command up front, BEFORE bootstrap /
// watchdog / install side effects run, and that the error names the unknown
// server.
func TestSetupCommand_ServerUnknownFailsLoudBeforeMutation(t *testing.T) {
	seedSetupInstallManifests(t, "serena", "memory")

	origBootstrap := setupBootstrapFn
	origRouter := setupLSPClientRouterFn
	origWatchdog := setupWatchdogFn
	origInstall := setupInstallServerFn
	t.Cleanup(func() {
		setupBootstrapFn = origBootstrap
		setupLSPClientRouterFn = origRouter
		setupWatchdogFn = origWatchdog
		setupInstallServerFn = origInstall
	})
	var bootstrapCalls, installCalls int
	setupBootstrapFn = func(io.Writer) error { bootstrapCalls++; return nil }
	setupWatchdogFn = func(io.Writer, bool) error { return nil }
	setupLSPClientRouterFn = func(bool) (*api.LSPClientRouterReport, error) {
		return &api.LSPClientRouterReport{}, nil
	}
	setupInstallServerFn = func(string, io.Writer) error { installCalls++; return nil }

	_, _, err := runSetupCmd(t, "--server", "does-not-exist")
	if err == nil {
		t.Fatal("setup --server with an unknown name must error")
	}
	if !strings.Contains(err.Error(), "does-not-exist") || !strings.Contains(err.Error(), "unknown server") {
		t.Fatalf("error should name the unknown server; got %v", err)
	}
	if bootstrapCalls != 0 {
		t.Fatalf("bootstrap ran %d time(s) before validation rejected the name; want 0 (fail-loud-before-mutation)", bootstrapCalls)
	}
	if installCalls != 0 {
		t.Fatalf("install ran %d time(s) for an unknown server; want 0", installCalls)
	}
}

// TestSetupCommand_NoServerFlagSkipsInstall verifies that omitting --server
// leaves the default flow unchanged: the install seam is never called.
func TestSetupCommand_NoServerFlagSkipsInstall(t *testing.T) {
	seedSetupInstallManifests(t, "serena")
	installed := stubSetupSideEffects(t)

	if _, _, err := runSetupCmd(t); err != nil {
		t.Fatalf("setup (no --server): %v", err)
	}
	if *installed != "<not-called>" {
		t.Fatalf("install seam ran for %q with no --server flag; want no call", *installed)
	}
}

// TestValidateSetupServerArg covers the pure validation helper directly.
func TestValidateSetupServerArg(t *testing.T) {
	seedSetupInstallManifests(t, "serena", "memory")

	if err := validateSetupServerArg(""); err != nil {
		t.Fatalf("empty server name should be a no-op success; got %v", err)
	}
	if err := validateSetupServerArg("  "); err != nil {
		t.Fatalf("whitespace-only server name should be a no-op success; got %v", err)
	}
	if err := validateSetupServerArg("serena"); err != nil {
		t.Fatalf("known server should validate; got %v", err)
	}
	err := validateSetupServerArg("nope")
	if err == nil {
		t.Fatal("unknown server must error")
	}
	if !strings.Contains(err.Error(), "available:") {
		t.Fatalf("error should list available servers; got %v", err)
	}
}
