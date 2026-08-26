package api

import (
	"runtime"
	"testing"

	"mcp-local-hub/internal/config"
)

// TestCompanionInstallAdmission is the #381 P1 guard: a kind=companion manifest
// whose single daemon has NO MCP port (Port=0, valid — the companion process
// binds its own port) must be ADMITTED by the install path, and its daemon must
// still register in supervisor-intent so the supervisor spawns it. Before the
// fix, manifestBlockingWarnings flagged port=0 and Preflight rejected the
// out-of-range port, making the kind uninstallable without a dummy port.
func TestCompanionInstallAdmission(t *testing.T) {
	absCwd := "/opt/excalidraw-canvas"
	if runtime.GOOS == "windows" {
		absCwd = "C:/opt/excalidraw-canvas"
	}
	m := &config.ServerManifest{
		Name:      "excalidraw-canvas",
		Kind:      config.KindCompanion,
		Transport: config.TransportProcess,
		Command:   "node",
		BaseArgs:  []string{"dist/server.js"},
		Daemons:   []config.DaemonSpec{{Name: "default", Cwd: absCwd}},
	}

	// P1: a Port=0 companion daemon must NOT be a blocking (write/install) warning.
	if w := manifestBlockingWarnings(m); len(w) != 0 {
		t.Errorf("companion manifestBlockingWarnings = %v, want none (Port=0 is valid for a non-MCP companion)", w)
	}

	// The companion daemon still materializes a supervisor-intent row (kind is not
	// workspace-scoped), so the supervisor spawns `mcphub daemon …` → RunProcess.
	rows, err := supervisorDaemonsFromPlan(m, testSupervisorIntentPlan(m, ""), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("supervisorDaemonsFromPlan = %d rows, want 1 (companion must register to spawn)", len(rows))
	}
}

// TestCompanionValidateRejectsNonZeroPort is the #381 liveness guard: a companion
// daemon must carry Port==0 (it binds its own listener directly; a non-zero value
// is mis-owned by the mcphub-daemon liveness probe → port_owner_mismatch → a
// healthy companion gets restarted). Validate must reject a non-zero port.
func TestCompanionValidateRejectsNonZeroPort(t *testing.T) {
	absCwd := "/opt/excalidraw-canvas"
	if runtime.GOOS == "windows" {
		absCwd = "C:/opt/excalidraw-canvas"
	}
	m := &config.ServerManifest{
		Name:      "excalidraw-canvas",
		Kind:      config.KindCompanion,
		Transport: config.TransportProcess,
		Command:   "node",
		BaseArgs:  []string{"dist/server.js"},
		Daemons:   []config.DaemonSpec{{Name: "default", Cwd: absCwd, Port: 3000}},
	}
	if err := m.Validate(); err == nil {
		t.Fatal("companion with a non-zero daemon port must be rejected (liveness port-owner mismatch)")
	}
}
