//go:build windows

package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"

	"mcp-local-hub/internal/api"
)

// TestPriorityClassRankMapping guards the class <-> rank mapping against
// drift from the authoritative golang.org/x/sys/windows constants, and
// against a mutant that maps a class to the wrong (e.g. IDLE) rank.
func TestPriorityClassRankMapping(t *testing.T) {
	cases := []struct {
		class uint32
		rank  priorityRank
	}{
		{windows.IDLE_PRIORITY_CLASS, rankIdle},
		{windows.BELOW_NORMAL_PRIORITY_CLASS, rankBelowNormal},
		{windows.NORMAL_PRIORITY_CLASS, rankNormal},
		{windows.ABOVE_NORMAL_PRIORITY_CLASS, rankAboveNormal},
		{windows.HIGH_PRIORITY_CLASS, rankHigh},
		{windows.REALTIME_PRIORITY_CLASS, rankRealtime},
	}
	for _, tc := range cases {
		if got := priorityClassToRank(tc.class); got != tc.rank {
			t.Errorf("priorityClassToRank(0x%x) = %d, want %d", tc.class, got, tc.rank)
		}
		// Round-trip for the ranks that map to a real class (BELOW_NORMAL+).
		if tc.rank >= rankBelowNormal {
			if got := rankToPriorityClass(tc.rank); got != tc.class {
				t.Errorf("rankToPriorityClass(%d) = 0x%x, want 0x%x", tc.rank, got, tc.class)
			}
		}
	}

	// An unrecognized class must map to rankUnknown (fail-safe: leaves it
	// untouched rather than lowering it).
	if got := priorityClassToRank(0xDEADBEEF); got != rankUnknown {
		t.Errorf("priorityClassToRank(unknown) = %d, want rankUnknown (%d)", got, rankUnknown)
	}

	// The floor must map to NORMAL and NEVER to IDLE — a mapping-layer guard
	// complementing the decision-layer never-IDLE invariant. (Floor raised
	// from BELOW_NORMAL to NORMAL on live A/B evidence; see supervisor_priority.go.)
	if got := rankToPriorityClass(supervisorPriorityFloorRank); got != windows.NORMAL_PRIORITY_CLASS {
		t.Errorf("rankToPriorityClass(floor) = 0x%x, want NORMAL 0x%x", got, uint32(windows.NORMAL_PRIORITY_CLASS))
	}
	if rankToPriorityClass(supervisorPriorityFloorRank) == windows.IDLE_PRIORITY_CLASS {
		t.Fatalf("floor must never map to IDLE_PRIORITY_CLASS")
	}
}

// TestEnsureSupervisorPriorityFloorOrchestration exercises the full
// probe -> decide -> set -> audit flow with INJECTED syscall seams, so it
// never mutates any real process priority (host-safety: the go test runner
// is untouched). Each case opens a fresh real event log against a temp dir
// and asserts both the SetPriorityClass behaviour and the audited outcome.
func TestEnsureSupervisorPriorityFloorOrchestration(t *testing.T) {
	probeErr := errors.New("simulated GetPriorityClass failure")
	setErr := errors.New("simulated SetPriorityClass failure")

	cases := []struct {
		name      string
		getClass  uint32
		getErr    error
		setErr    error
		wantSetTo uint32 // 0 => Set must NOT be called
		wantEvent string
	}{
		{
			name:      "idle is raised to normal",
			getClass:  windows.IDLE_PRIORITY_CLASS,
			wantSetTo: windows.NORMAL_PRIORITY_CLASS,
			wantEvent: "supervisor-priority-raised",
		},
		{
			// BELOW_NORMAL is now below the floor → raised to NORMAL (the
			// semantic flip vs #577, where this was a no-op at-floor case).
			name:      "below-normal is raised to normal",
			getClass:  windows.BELOW_NORMAL_PRIORITY_CLASS,
			wantSetTo: windows.NORMAL_PRIORITY_CLASS,
			wantEvent: "supervisor-priority-raised",
		},
		{
			name:      "normal is at floor, a no-op",
			getClass:  windows.NORMAL_PRIORITY_CLASS,
			wantSetTo: 0,
			wantEvent: "supervisor-priority-ok",
		},
		{
			name:      "high is not lowered",
			getClass:  windows.HIGH_PRIORITY_CLASS,
			wantSetTo: 0,
			wantEvent: "supervisor-priority-ok",
		},
		{
			name:      "probe failure is audited and does not set",
			getErr:    probeErr,
			wantSetTo: 0,
			wantEvent: "supervisor-priority-probe-failed",
		},
		{
			name:      "raise failure is audited",
			getClass:  windows.IDLE_PRIORITY_CLASS,
			setErr:    setErr,
			wantSetTo: windows.NORMAL_PRIORITY_CLASS, // Set IS attempted
			wantEvent: "supervisor-priority-raise-failed",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var setCalled bool
			var setGot uint32
			restore := setPriorityClassSeamsForTest(
				func() (uint32, error) { return tc.getClass, tc.getErr },
				func(class uint32) error {
					setCalled = true
					setGot = class
					return tc.setErr
				},
			)
			defer restore()

			logPath := filepath.Join(t.TempDir(), api.SupervisorEventLogFileLeaf)
			logger, err := api.OpenSupervisorEventLog(logPath)
			if err != nil {
				t.Fatalf("OpenSupervisorEventLog: %v", err)
			}

			ensureSupervisorPriorityFloor(logger)
			_ = logger.Close()

			if tc.wantSetTo == 0 {
				if setCalled {
					t.Errorf("SetPriorityClass was called (0x%x) but the current class should have been left untouched", setGot)
				}
			} else {
				if !setCalled {
					t.Fatalf("SetPriorityClass was NOT called; expected a raise to 0x%x", tc.wantSetTo)
				}
				if setGot != tc.wantSetTo {
					t.Errorf("SetPriorityClass(0x%x), want 0x%x", setGot, tc.wantSetTo)
				}
				if setGot == windows.IDLE_PRIORITY_CLASS {
					t.Fatalf("SetPriorityClass must NEVER set IDLE")
				}
			}

			data, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatalf("read event log: %v", err)
			}
			if !strings.Contains(string(data), tc.wantEvent) {
				t.Errorf("event log missing %q; got:\n%s", tc.wantEvent, string(data))
			}
		})
	}
}
