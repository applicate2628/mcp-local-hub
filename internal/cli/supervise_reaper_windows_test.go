//go:build windows

package cli

import (
	"context"
	"slices"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
)

// TestReapStaleTransients_Windows covers PR #243 bot round-2 P2: the
// Windows cold-start reaper must clear claim slots + dead PIDs, tree-kill
// alive maintenance children whose recorded start time matches the live
// process (recycling-safe), skip-and-clear alive PIDs whose start time
// disagrees (PID recycled to an unrelated process), and retain entries it
// cannot verify (conservative). Fully injected deps — no real syscalls.
func TestReapStaleTransients_Windows(t *testing.T) {
	base := time.Date(2026, 5, 17, 4, 0, 0, 0, time.UTC)
	const (
		pidDead         = 111
		pidMatch        = 222
		pidRecycled     = 333
		pidUnverifiable = 444
	)
	seed := &api.SupervisorStateFile{
		Version: 1,
		Daemons: map[string]api.SupervisorDaemonState{},
		TransientPIDs: []api.TransientPID{
			{PID: 0, Kind: "workspace-weekly-refresh", StartedAt: base.Format(time.RFC3339Nano)},
			{PID: pidDead, Kind: "workspace-weekly-refresh", StartedAt: base.Format(time.RFC3339Nano)},
			{PID: pidMatch, Kind: "workspace-weekly-refresh", StartedAt: base.Format(time.RFC3339Nano)},
			{PID: pidRecycled, Kind: "workspace-weekly-refresh", StartedAt: base.Format(time.RFC3339Nano)},
			{PID: pidUnverifiable, Kind: "workspace-weekly-refresh", StartedAt: base.Format(time.RFC3339Nano)},
		},
	}

	var written *api.SupervisorStateFile
	var killed []int
	deps := ReaperDeps{
		StateDir:  `C:\fake\state`,
		ReadState: func(string) (*api.SupervisorStateFile, error) { return seed, nil },
		WriteState: func(_ string, s *api.SupervisorStateFile) error {
			written = s
			return nil
		},
		PIDAlive: func(pid int) bool { return pid != pidDead },
		ProcessStartTime: func(pid int) (time.Time, bool) {
			switch pid {
			case pidMatch:
				return base, true // within tolerance → our child
			case pidRecycled:
				return base.Add(time.Hour), true // far off → recycled
			default: // pidUnverifiable
				return time.Time{}, false
			}
		},
		KillProcess: func(pid int) error { killed = append(killed, pid); return nil },
	}

	res, err := ReapStaleTransients(context.Background(), deps)
	if err != nil {
		t.Fatalf("ReapStaleTransients: %v", err)
	}

	if len(killed) != 1 || killed[0] != pidMatch {
		t.Fatalf("only the start-time-matching child must be killed; killed=%v", killed)
	}
	if len(res.KilledPIDs) != 1 || res.KilledPIDs[0] != pidMatch {
		t.Fatalf("KilledPIDs=%v, want [%d]", res.KilledPIDs, pidMatch)
	}
	if !slices.Contains(res.DeadPIDs, 0) || !slices.Contains(res.DeadPIDs, pidDead) {
		t.Fatalf("DeadPIDs must contain the claim (0) and the dead PID (%d); got %v", pidDead, res.DeadPIDs)
	}
	if !slices.Contains(res.SkippedPIDs, pidRecycled) || !slices.Contains(res.SkippedPIDs, pidUnverifiable) {
		t.Fatalf("SkippedPIDs must contain recycled (%d) + unverifiable (%d); got %v", pidRecycled, pidUnverifiable, res.SkippedPIDs)
	}

	// Only the unverifiable entry is retained for the next cold start;
	// everything else (claim, dead, killed, recycled) is cleared.
	if written == nil {
		t.Fatal("reaper must write back reconciled state")
	}
	if len(written.TransientPIDs) != 1 || written.TransientPIDs[0].PID != pidUnverifiable {
		t.Fatalf("only the unverifiable entry must be retained; got %+v", written.TransientPIDs)
	}
}
