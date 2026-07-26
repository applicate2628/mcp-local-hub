// internal/cli/route_session_expiry_test.go
//
// Regression coverage for the codex bot PR #588 P2 finding that MCP sessions
// created by the STANDALONE `mcphub route` front daemon were never expired.
//
// The daemon builds its own serena and LSP SessionRouters (buildRouteServer),
// but the sweeps that expire sessions live in the GUI's lifecycle and drive
// the GUI's own routers. Nothing expired these, so a supervisor-managed,
// always-on route daemon accumulated one binding per MCP session for its
// entire uptime — unbounded growth in exactly the process designed to run
// forever.
package cli

import (
	"context"
	"testing"
	"time"

	"mcp-local-hub/internal/api"

	"github.com/spf13/cobra"
)

// TestRouteDaemon_SessionStoresAreReachableForExpiry pins the structural
// precondition: the route daemon's session routers must be handed back by its
// construction path at all. Before the fix they were local variables that fell
// out of scope, so no caller could ever sweep them.
func TestRouteDaemon_SessionStoresAreReachableForExpiry(t *testing.T) {
	redirectMCPFrontTestEnv(t)

	_, stores, err := buildRouteServer(&cobra.Command{}, 0)
	if err != nil {
		t.Fatalf("buildRouteServer: %v", err)
	}
	if stores == nil {
		t.Fatalf("buildRouteServer returned no session stores; the route daemon's session maps are unreachable and therefore un-expirable")
	}
	if stores.serena == nil {
		t.Fatalf("the route daemon's serena session router was not handed back, so nothing can expire it")
	}
	// The LSP router is only wired when the mcp-language-server manifest loads
	// and parses. It is embedded, so it should be wired here — and if it ever
	// is not, the sweep wiring must not be what silently breaks.
	if stores.lsp == nil {
		t.Fatalf("the route daemon's LSP session router was not handed back; the embedded mcp-language-server manifest should always wire it")
	}
}

// TestRouteDaemon_SessionExpiryActuallyReclaimsBoundSessions drives the real
// wiring (runRouteSessionExpiry, the same function runRoute calls) and proves
// a bound session is actually reclaimed.
//
// interval and ttl are compressed so the sweep completes inside the test; the
// production call passes sessionCleanupInterval and the 24h idle TTL.
func TestRouteDaemon_SessionExpiryActuallyReclaimsBoundSessions(t *testing.T) {
	redirectMCPFrontTestEnv(t)

	s, stores, err := buildRouteServer(&cobra.Command{}, 0)
	if err != nil {
		t.Fatalf("buildRouteServer: %v", err)
	}

	// Bind one serena session and one LSP session, the way a client handshake
	// does.
	stores.serena.BindSession("serena-session-1", &api.WorkspaceEntry{
		WorkspaceKey: "abcd1234", WorkspacePath: "/tmp/project", Language: "go",
	})
	stores.lsp.EnsureSession("lsp-session-1")
	if stores.serena.Len() == 0 {
		t.Fatalf("test precondition broken: no serena session was bound, so expiry cannot be observed")
	}
	if stores.lsp.Len() == 0 {
		t.Fatalf("test precondition broken: no LSP session was bound, so expiry cannot be observed")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// ttl=0 makes every existing binding immediately idle-expired, so the very
	// next tick must reclaim it.
	runRouteSessionExpiry(ctx, s, stores, 5*time.Millisecond, 0)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if stores.serena.Len() == 0 && stores.lsp.Len() == 0 {
			return // both reclaimed
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("the route daemon's sessions were never expired (serena=%d lsp=%d after 5s of sweeps); a long-lived route daemon grows these maps for its entire uptime",
		stores.serena.Len(), stores.lsp.Len())
}

// TestRouteDaemon_SessionExpiryStopsWithContext proves the sweeps cannot
// outlive the daemon: they are started as bare goroutines, so ctx is the only
// thing that stops them.
func TestRouteDaemon_SessionExpiryStopsWithContext(t *testing.T) {
	redirectMCPFrontTestEnv(t)

	s, stores, err := buildRouteServer(&cobra.Command{}, 0)
	if err != nil {
		t.Fatalf("buildRouteServer: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	runRouteSessionExpiry(ctx, s, stores, 5*time.Millisecond, 0)
	cancel()

	// After cancellation a newly-bound session must survive: no sweeper is
	// running any more.
	time.Sleep(50 * time.Millisecond)
	stores.serena.BindSession("post-cancel", &api.WorkspaceEntry{
		WorkspaceKey: "abcd1234", WorkspacePath: "/tmp/project", Language: "go",
	})
	time.Sleep(50 * time.Millisecond)
	if stores.serena.Len() == 0 {
		t.Fatalf("a sweep ran after its context was cancelled; the route daemon's expiry goroutines must stop with the daemon")
	}
}
