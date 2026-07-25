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
	for _, g := range reg.Global {
		if g.Port == DefaultRouteDaemonPort {
			t.Fatalf("DefaultRouteDaemonPort (%d) collides with configs/ports.yaml entry %s/%s",
				DefaultRouteDaemonPort, g.Server, g.Daemon)
		}
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
