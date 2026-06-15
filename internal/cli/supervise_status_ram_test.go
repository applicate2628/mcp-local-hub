package cli

import (
	"path/filepath"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
)

// withFakeRSS swaps the package-level rssByPID lookup for the duration of a
// test so the producer's RAM path is exercised deterministically without a
// live process. Restores the real (platform) lookup on cleanup.
func withFakeRSS(t *testing.T, fn func(pid int) (uint64, bool)) {
	t.Helper()
	prev := rssByPID
	rssByPID = fn
	t.Cleanup(func() { rssByPID = prev })
}

// runningStatusRow seeds a single Running daemon (Port preset so the
// status producer never reaches ResolveManifestDaemonPort / the registry —
// keeps the test fully state-safe per the live-state safety rule) and
// returns the producer's row for it. The liveness probe is forced to
// "alive + owns port" so the row stays Running (not flipped to Restarting,
// which would zero current_pid and suppress the RAM lookup).
func runningStatusRow(t *testing.T) map[string]any {
	t.Helper()
	stateDir := apitest.HardenedTempDir(t)
	taskName := `\mcp-local-hub-memory-default`
	intent := &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{{
			TaskName: taskName,
			Server:   "memory",
			Daemon:   "default",
			Port:     9123,
		}},
	}
	if err := api.WriteSupervisorIntent(filepath.Join(stateDir, "supervisor-intent.json"), intent); err != nil {
		t.Fatalf("seed supervisor-intent.json: %v", err)
	}
	const livePID = 424242
	tracker := NewDaemonRuntimeTracker()
	tracker.MarkSpawned(taskName, livePID, time.Date(2026, 6, 4, 10, 0, 0, 0, time.UTC))

	// Force "alive + owns its port": PIDAlive true, no identity proof, and the
	// port-owner lookup returns the SAME pid so supervisorDaemonEntryLive
	// returns live (row stays Running, current_pid retained — the RAM lookup
	// only fires for a Running daemon with a non-zero current_pid).
	restore := setSupervisorLivenessProbeForTest(supervisorLivenessProbe{
		PIDAlive:    func(pid int) bool { return true },
		PortLive:    func(port int) bool { return true },
		PortOwnerPID: func(port int) (int, bool, error) { return livePID, true, nil },
	})
	defer restore()

	rows, err := supervisorStatusDaemons(stateDir, tracker)
	if err != nil {
		t.Fatalf("supervisorStatusDaemons: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows len = %d, want 1", len(rows))
	}
	if rows[0]["state"] != "Running" {
		t.Fatalf("precondition: row not Running: %+v", rows[0])
	}
	return rows[0]
}

// TestSupervisorStatusEmitsRAMForRunningDaemon asserts the producer looks
// up RSS by the live current_pid and emits ram_bytes for a Running daemon.
func TestSupervisorStatusEmitsRAMForRunningDaemon(t *testing.T) {
	var gotPID int
	withFakeRSS(t, func(pid int) (uint64, bool) {
		gotPID = pid
		return 48 * 1024 * 1024, true
	})
	row := runningStatusRow(t)
	if gotPID != 424242 {
		t.Fatalf("rssByPID called with pid=%d, want the live current_pid 424242", gotPID)
	}
	got, ok := row["ram_bytes"]
	if !ok {
		t.Fatalf("ram_bytes not emitted for Running daemon: %+v", row)
	}
	if got != uint64(48*1024*1024) {
		t.Fatalf("ram_bytes = %v, want %d", got, 48*1024*1024)
	}
}

// TestSupervisorStatusOmitsRAMWhenLookupFails asserts a failed RSS lookup
// (ok=false — non-Windows host, PID recycled, OpenProcess denied) omits the
// ram_bytes field entirely rather than emitting a misleading 0.
func TestSupervisorStatusOmitsRAMWhenLookupFails(t *testing.T) {
	withFakeRSS(t, func(pid int) (uint64, bool) {
		return 0, false
	})
	row := runningStatusRow(t)
	if _, ok := row["ram_bytes"]; ok {
		t.Fatalf("ram_bytes emitted despite lookup failure: %+v", row)
	}
}

// TestSupervisorStatusOmitsRAMWhenZero asserts a zero RSS value (ok=true but
// 0 bytes — degenerate) is treated as unknown and the field is omitted.
func TestSupervisorStatusOmitsRAMWhenZero(t *testing.T) {
	withFakeRSS(t, func(pid int) (uint64, bool) {
		return 0, true
	})
	row := runningStatusRow(t)
	if _, ok := row["ram_bytes"]; ok {
		t.Fatalf("ram_bytes emitted for zero RSS: %+v", row)
	}
}
