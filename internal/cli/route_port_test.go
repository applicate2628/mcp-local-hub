// internal/cli/route_port_test.go
//
// Guard test for bot/architect review finding F2 (Increment-1, 2026-07-25):
// `mcphub route`'s default port must not collide with any hand-assigned
// global daemon (configs/ports.yaml), the GUI's own reserved port, or the
// serena dynamic pool's effective range — mechanically re-verified against
// the live sources of truth so a future edit to any of them cannot silently
// reintroduce the exact class of collision F2 caught (9126 colliding with
// godbolt).
package cli

import (
	"os"
	"testing"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/config"
)

func TestDefaultRouteDaemonPort_NotInPortsYAMLOrGUIOrSerenaPool(t *testing.T) {
	if DefaultRouteDaemonPort == config.ReservedGUIPort {
		t.Fatalf("DefaultRouteDaemonPort (%d) collides with the GUI's reserved port (config.ReservedGUIPort=%d)",
			DefaultRouteDaemonPort, config.ReservedGUIPort)
	}

	// configs/ports.yaml is the single-owner list of hand-assigned global
	// daemon ports (internal/api/global_port_alloc.go's band-convention
	// comment: "9121-9149 hand-assigned globals (configs/ports.yaml)").
	f, err := os.Open("../../configs/ports.yaml")
	if err != nil {
		t.Fatalf("open configs/ports.yaml: %v", err)
	}
	defer f.Close()
	reg, err := config.ParsePortRegistry(f)
	if err != nil {
		t.Fatalf("parse configs/ports.yaml: %v", err)
	}
	// FOREIGN rows only. The original guard rejected the port appearing in
	// configs/ports.yaml at all, which conflated two opposite facts: another
	// server holding this port (the F2 defect — 9126 vs godbolt) versus the
	// route daemon declaring its OWN port in the ledger (correct, and the whole
	// reason the ledger exists). The un-narrowed form made the two mutually
	// exclusive, so registering route/front:9137 turned this guard red even
	// though nothing collided.
	//
	// That registration is load-bearing, not bookkeeping: 9137 was claimed only
	// as a CONSTANT IN CODE (DefaultRouteDaemonPort here, DefaultMCPFrontPort in
	// internal/api/mcp_front_port.go), so a ledger scan could not see it and the
	// vcpkg daemon was nearly assigned the same port. The ledger row is what
	// makes the claim visible to the next assignment; keep it, and narrow the
	// guard to what it was always meant to catch.
	//
	// The `daemon` field is deliberately NOT matched — any daemon under the
	// route server is this server's own port to declare, and pinning the daemon
	// name would make a future rename look like a foreign collision.
	var ownRows int
	for _, g := range reg.Global {
		if g.Port != DefaultRouteDaemonPort {
			continue
		}
		if g.Server == api.BuiltinRouteServer {
			ownRows++
			continue
		}
		t.Fatalf("DefaultRouteDaemonPort (%d) collides with configs/ports.yaml entry %s/%s owned by a DIFFERENT server",
			DefaultRouteDaemonPort, g.Server, g.Daemon)
	}
	// Positive assertion, not merely the absence of a foreign row: the whole
	// point of the fix above is that the route daemon's port IS declared in the
	// ledger. Without this, silently deleting the row would leave the guard
	// green and re-open the invisible-claim gap that nearly cost vcpkg its port.
	if ownRows == 0 {
		t.Errorf("DefaultRouteDaemonPort (%d) is not declared in configs/ports.yaml under server %q; an unregistered port is invisible to the next hand-assignment (this is exactly how the vcpkg daemon nearly took 9137)",
			DefaultRouteDaemonPort, api.BuiltinRouteServer)
	}

	// The serena dynamic pool (internal/api/serena_dynamic_pool.go) reserves
	// its OWN range (built-in default 9150-9199) that configs/ports.yaml does
	// not list at all — a bare "not in configs/ports.yaml" check would miss
	// this class of collision entirely (the exact gap that made "9122" look
	// free from configs/ports.yaml alone during this review). Call the
	// EXPORTED accessor (nil embed => built-in default) instead of
	// duplicating the 9150/9199 literals, so a future change to the pool's
	// range is picked up automatically rather than silently drifting from
	// this guard.
	pool, err := api.EffectiveSerenaPortPool(nil)
	if err != nil {
		t.Fatalf("EffectiveSerenaPortPool(nil): %v", err)
	}
	if DefaultRouteDaemonPort >= pool.Start && DefaultRouteDaemonPort <= pool.End {
		t.Fatalf("DefaultRouteDaemonPort (%d) falls inside the serena dynamic pool's effective range [%d, %d]",
			DefaultRouteDaemonPort, pool.Start, pool.End)
	}

	// Sanity: the constant should also sit inside the documented
	// "hand-assigned globals, room to grow" band (9121-9149) rather than in
	// some unrelated, undocumented range — the intended, reviewed placement,
	// not an accident of the two checks above both happening to pass.
	const handAssignedGlobalsBandStart = 9121
	const handAssignedGlobalsBandEnd = 9149
	if DefaultRouteDaemonPort < handAssignedGlobalsBandStart || DefaultRouteDaemonPort > handAssignedGlobalsBandEnd {
		t.Errorf("DefaultRouteDaemonPort (%d) is outside the documented hand-assigned-globals band [%d, %d] (internal/api/serena_dynamic_pool.go / global_port_alloc.go convention); reconcile the port-map comment if this is an intentional new band",
			DefaultRouteDaemonPort, handAssignedGlobalsBandStart, handAssignedGlobalsBandEnd)
	}
}
