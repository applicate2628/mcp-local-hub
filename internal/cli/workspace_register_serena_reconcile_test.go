// Package cli — area-4 router-native fresh-install wiring-gap fix tests.
//
// Post-flip the serena catalog is the dynamic-pool shape (no client_bindings),
// so neither `mcphub install` nor the #400 binding-loop override writes a
// `/serena/mcp` client entry on a fresh host (the gap QA caught — see
// work-items/bugs/2026-06-28-serena-fresh-install-client-wiring-gap.md). The fix
// routes a fresh `mcphub workspace register <serena-ws>` through the EXISTING
// single-owner client-reconcile path (api.ReconcileSerenaClientsToRouter) so the
// in-scope client configs get pointed at the /serena/mcp router.
//
// These tests pin that contract:
//   - a fresh register with a LIVE GUI wires the in-scope client config to
//     SerenaRouterClientURL(<live port>) (the load-bearing falsifier for the gap);
//   - a fresh register with NO live GUI still SUCCEEDS (registry row written) and
//     writes NO client entry, emitting a warning (fail-closed, warn-only).
//
// Design ref: architect a80c69eb (accepted, option a — reuse the single owner).
package cli

import (
	"context"
	"io"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/clients"
)

// stubReconcileSerenaRegister overrides the fresh-register reconcile seam for
// the test scope and returns a pointer to a bool the test can assert was set
// (the hook fired). Returns a restore via t.Cleanup.
func stubReconcileSerenaRegister(t *testing.T, fn func(ctx context.Context, w io.Writer) (*api.MigrateReport, error)) *bool {
	t.Helper()
	invoked := new(bool)
	orig := reconcileSerenaRegisterClientsFn
	reconcileSerenaRegisterClientsFn = func(ctx context.Context, w io.Writer) (*api.MigrateReport, error) {
		*invoked = true
		return fn(ctx, w)
	}
	t.Cleanup(func() { reconcileSerenaRegisterClientsFn = orig })
	return invoked
}

// TestWorkspaceRegisterSerena_WiresClientsToRouter_OnLiveGUI is the LOAD-BEARING
// falsifier for the router-native fresh-install wiring gap. A fresh serena
// register against a LIVE GUI router must rewrite the in-scope client config to
// the constant /serena/mcp router URL on the live port.
//
// Non-vacuous: the seam drives the REAL api.ReconcileSerenaClientsToRouter owner
// against a real on-disk claude-code adapter (resolved under the redirected
// HOME). If the register hook were removed (the gap regressed), the seam would
// never be invoked, the claude-code config would keep ZERO serena entries, and
// the URL assertion below would fail.
func TestWorkspaceRegisterSerena_WiresClientsToRouter_OnLiveGUI(t *testing.T) {
	withSerenaManifest(t, 9121, 9123)
	stateDir := withStateDir(t)

	// Hermetic HOME so the real claude-code adapter resolves $HOME/.claude.json
	// under the temp tree. withStateDir already redirected LOCALAPPDATA/XDG;
	// the client adapters resolve via os.UserHomeDir (HOME/USERPROFILE).
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	// Seed an empty claude-code config so the adapter Exists() is true (the
	// reconcile skips clients that are not installed on the host).
	cc, err := clients.NewClaudeCode()
	if err != nil {
		t.Fatalf("construct claude-code adapter: %v", err)
	}
	if _, err := cc.InitEmpty(); err != nil {
		t.Fatalf("seed empty claude-code config: %v", err)
	}

	// Sanity: ZERO serena entries before the register (the falsifying probe for
	// the gap — the gap is precisely "no serena entry gets written").
	if e, gerr := cc.GetEntry("serena"); gerr != nil {
		t.Fatalf("pre-register GetEntry(serena): %v", gerr)
	} else if e != nil {
		t.Fatalf("pre-register claude-code already has a serena entry %q; the falsifier would be vacuous", e.URL)
	}

	const livePort = 9137
	wantURL := api.SerenaRouterClientURL(livePort) // http://127.0.0.1:9137/serena/mcp

	// Drive the REAL reconcile owner with injected live-GUI discovery seams
	// (no real GUI / netstat / pidport file needed) against the real claude-code
	// adapter, narrowed to claude-code so the test does not depend on which other
	// clients happen to be "installed" under the hermetic HOME.
	invoked := stubReconcileSerenaRegister(t, func(ctx context.Context, w io.Writer) (*api.MigrateReport, error) {
		return api.ReconcileSerenaClientsToRouter(ctx, api.SerenaReconcileOpts{
			PidportPath:    "ignored-by-injected-readpidport",
			ReadPidport:    func(string) (int, int, error) { return 4242, livePort, nil },
			VerifyIdentity: func(context.Context, int, int) error { return nil },
			Ping:           func(context.Context, int) error { return nil },
			Clients:        clients.AllClients(),
			ClientsInclude: []string{"claude-code"},
		})
	})

	out, err := runWorkspaceCmd(t, "register", makeWorkspaceDir(t, t.TempDir(), []string{"python"}))
	if err != nil {
		t.Fatalf("register: %v\noutput: %s", err, out)
	}
	if !*invoked {
		t.Fatalf("the fresh-register reconcile hook never fired; the wiring gap is unfixed")
	}

	// (i) The claude-code config now carries the /serena/mcp router URL on the
	//     live port — the gap is closed.
	got, gerr := cc.GetEntry("serena")
	if gerr != nil {
		t.Fatalf("post-register GetEntry(serena): %v", gerr)
	}
	if got == nil {
		t.Fatalf("post-register claude-code has NO serena entry — the fresh-install wiring gap is NOT fixed")
	}
	if got.URL != wantURL {
		t.Errorf("claude-code serena URL = %q, want %q (the live-port /serena/mcp router URL)", got.URL, wantURL)
	}

	// (ii) The registry row exists (registration itself succeeded).
	regPath, _ := api.DefaultRegistryPath()
	reg := api.NewRegistry(regPath)
	if err := reg.Load(); err != nil {
		t.Fatalf("load registry: %v", err)
	}
	if n := len(reg.SerenaEntries()); n != 1 {
		t.Fatalf("want exactly 1 serena registry row after register, got %d", n)
	}

	// The reconcile report (Applied row) is surfaced on stderr.
	if !strings.Contains(out, "serena/claude-code") {
		t.Errorf("expected the reconcile report to name the wired client; got %q", out)
	}
	_ = stateDir
}

// TestWorkspaceRegisterSerena_FailClosed_NoLiveGUI_RegisterStillSucceeds pins the
// fail-closed, warn-only contract: a fresh register with NO live GUI must STILL
// succeed (the registry row is written + durable) and write NO client entry, with
// a warning naming the re-wire command. ErrSerenaReconcileGUINotLive is warn-only,
// never fatal — register exits 0.
func TestWorkspaceRegisterSerena_FailClosed_NoLiveGUI_RegisterStillSucceeds(t *testing.T) {
	withSerenaManifest(t, 9121, 9123)
	withStateDir(t)

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	cc, err := clients.NewClaudeCode()
	if err != nil {
		t.Fatalf("construct claude-code adapter: %v", err)
	}
	if _, err := cc.InitEmpty(); err != nil {
		t.Fatalf("seed empty claude-code config: %v", err)
	}

	// Drive the REAL reconcile owner with a discovery path that fails closed
	// (an injected ReadPidport that reports the GUI is not live → the discovery
	// guard returns ErrSerenaReconcileGUINotLive with NO client write).
	invoked := stubReconcileSerenaRegister(t, func(ctx context.Context, w io.Writer) (*api.MigrateReport, error) {
		return api.ReconcileSerenaClientsToRouter(ctx, api.SerenaReconcileOpts{
			PidportPath:    "ignored-by-injected-readpidport",
			ReadPidport:    func(string) (int, int, error) { return 0, 0, io.ErrUnexpectedEOF },
			VerifyIdentity: func(context.Context, int, int) error { return nil },
			Ping:           func(context.Context, int) error { return nil },
			Clients:        clients.AllClients(),
			ClientsInclude: []string{"claude-code"},
		})
	})

	out, err := runWorkspaceCmd(t, "register", makeWorkspaceDir(t, t.TempDir(), []string{"python"}))
	if err != nil {
		t.Fatalf("register must SUCCEED even when the GUI is not live (warn-only); got %v\noutput: %s", err, out)
	}
	if !*invoked {
		t.Fatalf("the fresh-register reconcile hook never fired")
	}

	// The registry row IS persisted (registration is durable independent of the
	// client wiring).
	regPath, _ := api.DefaultRegistryPath()
	reg := api.NewRegistry(regPath)
	if err := reg.Load(); err != nil {
		t.Fatalf("load registry: %v", err)
	}
	if n := len(reg.SerenaEntries()); n != 1 {
		t.Fatalf("want 1 serena registry row after a GUI-not-live register, got %d", n)
	}

	// NO client entry was written (fail-closed: the discovery guard refused to
	// write a guessed URL).
	if e, gerr := cc.GetEntry("serena"); gerr != nil {
		t.Fatalf("GetEntry(serena): %v", gerr)
	} else if e != nil {
		t.Errorf("a GUI-not-live register must NOT write a client serena entry; got URL %q", e.URL)
	}

	// A warning naming the GUI-not-live cause + the re-wire path is emitted.
	if !strings.Contains(out, "warning:") {
		t.Errorf("expected a warning on the GUI-not-live path; got %q", out)
	}
	if !strings.Contains(out, "mcphub gui") {
		t.Errorf("the GUI-not-live warning should tell the operator to start `mcphub gui`; got %q", out)
	}
}
