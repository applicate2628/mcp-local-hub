// hub_listener_groups_phase4b_test.go — groups/namespaces Phase 4b.
//
// The /g/<group>/mcp route is mounted on the gate-ON hub listener mux
// alongside /clients/ (startHubMcpListener, hub_listener.go). This test
// pins the gate-OFF requirement: with the hub endpoint gate OFF the
// listener never starts, so NO route — neither /clients/ nor the new /g/ —
// is served. The api-package tests
// (hub_mcp_groups_phase4b_test.go) cover the gate-ON request path through
// the shared handler; this gui-package test guards the listener-lifecycle
// half: a gate-OFF process must remain inert (no socket, no /g/ route).
//
// Spec: groups/namespaces decision §"Endpoint / URL shape" +
// §"Migration / compatibility" (additive-by-omission).

package gui

import (
	"context"
	"testing"

	"mcp-local-hub/internal/api"
)

// TestGroupsPhase4b_GateOffServesNoGroupRoute pins that with the gate OFF,
// startHubMcpListener short-circuits BEFORE building the mux, so the /g/
// route (and every other route) is unserved. A nil bundle is the proof
// that no listener — hence no /g/ handler — exists.
func TestGroupsPhase4b_GateOffServesNoGroupRoute(t *testing.T) {
	seedManifestDir(t)
	resetResolverSnapshot(t)

	a := api.NewAPI()
	bundle, err := startHubMcpListener(context.Background(), false, a)
	if err != nil {
		t.Fatalf("startHubMcpListener (gate OFF) err: %v", err)
	}
	if bundle != nil {
		ShutdownHubListener(context.Background(), bundle)
		t.Fatal("gate OFF produced a non-nil listener bundle — the /g/ route would be served despite gate OFF")
	}
}
