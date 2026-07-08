package api

import (
	"fmt"
	"strings"
	"testing"

	"mcp-local-hub/internal/process"
)

// swapOrphanParentState injects a deterministic TRI-STATE parent-liveness probe for the
// reaper ancestor walks (bug 2026-07-08 walk-fail-closed + codex PR #521): tests control
// whether a PID absent from the census fixture reads as PIDStateDead (a real orphan → reap),
// PIDStateAlive (a dropped live-parent row → spare), or PIDStateUnknown / probe error (cannot
// prove → spare), instead of probing whatever real PID the fixture happens to name.
func swapOrphanParentState(t *testing.T, fn func(int) (process.PIDState, error)) {
	t.Helper()
	prev := orphanParentStateFn
	orphanParentStateFn = fn
	t.Cleanup(func() { orphanParentStateFn = prev })
}

// deadParent / aliveParent / unknownParent are the three deterministic seam values.
func deadParent(int) (process.PIDState, error)    { return process.PIDStateDead, nil }
func aliveParent(int) (process.PIDState, error)   { return process.PIDStateAlive, nil }
func unknownParent(int) (process.PIDState, error) { return process.PIDStateUnknown, nil }

const walkCSVHeader = "Node,CommandLine,CreationDate,ExecutablePath,ParentProcessId,ProcessId,WorkingSetSize"

func walkRow(cmdline string, ppid, pid int) string {
	return fmt.Sprintf(`host,"%s",20250101090000.000000+000,C:\x.exe,%d,%d,80000000`, cmdline, ppid, pid)
}

// TestParseOrphans_ByPIDMiss_TriState is the Case A falsifying probe: a candidate whose
// parent row is ABSENT is reaped ONLY when that parent is PROVABLY dead. Alive (a dropped
// live-protector row) AND Unknown (an unprobeable live protector — OpenProcess ACCESS_DENIED)
// both SPARE (fail closed). A boolean "alive?" probe would collapse Unknown into "dead" and
// force-kill a live-rooted child on the unattended ticker (codex PR #521 P0).
func TestParseOrphans_ByPIDMiss_TriState(t *testing.T) {
	csv := walkCSVHeader + "\n" + walkRow("uvx -y @mui/mcp", 4000, 5000) + "\n"
	patterns := []string{"@mui/mcp"}

	cases := []struct {
		name      string
		probe     func(int) (process.PIDState, error)
		wantReap  bool
	}{
		{"dead parent → real orphan → reap", deadParent, true},
		{"alive parent → dropped live row → spare", aliveParent, false},
		{"unknown parent → unprobeable live protector → spare", unknownParent, false},
		{"probe error → cannot prove → spare", func(int) (process.PIDState, error) {
			return process.PIDStateUnknown, fmt.Errorf("open denied")
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			swapOrphanParentState(t, tc.probe)
			got, _ := parseOrphans(strings.NewReader(csv), patterns)
			gotReap := len(got) == 1
			if gotReap != tc.wantReap {
				t.Errorf("reap=%v, want %v (got %d orphan(s))", gotReap, tc.wantReap, len(got))
			}
		})
	}
}

// TestParseOrphans_SelfLoop_Spared (codex PR #521 P0): a candidate whose ancestor chain hits a
// self-loop row (ppid == pid) — a MALFORMED row, not a real root — must be spared, because a
// protected daemon/client could be above the malformed row. ppid==0 (a genuine root) still reaps.
func TestParseOrphans_SelfLoop_Spared(t *testing.T) {
	// candidate 5000 → 5001, where 5001 is a self-loop (ppid==pid==5001).
	selfLoop := walkCSVHeader + "\n" +
		walkRow("uvx -y @mui/mcp", 5001, 5000) + "\n" +
		walkRow("malformed-self-loop", 5001, 5001) + "\n"
	swapOrphanParentState(t, deadParent)
	if got, _ := parseOrphans(strings.NewReader(selfLoop), []string{"@mui/mcp"}); len(got) != 0 {
		t.Errorf("a candidate whose chain hits a self-loop row must be spared (unresolved ancestry); got %d", len(got))
	}

	// Contrast: a genuine root (ppid==0) still reaps.
	genuineRoot := walkCSVHeader + "\n" +
		walkRow("uvx -y @mui/mcp", 5001, 5000) + "\n" +
		walkRow("real-root", 0, 5001) + "\n"
	if got, _ := parseOrphans(strings.NewReader(genuineRoot), []string{"@mui/mcp"}); len(got) != 1 {
		t.Errorf("a candidate resolving to a genuine root (ppid==0) with no protected ancestor is a real orphan; got %d", len(got))
	}
}

// TestParseOrphans_DepthCapExhaustion_Spared is the Case B probe: a candidate under an
// abnormally deep chain of PRESENT, non-protected ancestors that never reaches a real root
// within the 16-level cap is spared (fail closed) rather than falling through to "orphan".
func TestParseOrphans_DepthCapExhaustion_Spared(t *testing.T) {
	var b strings.Builder
	b.WriteString(walkCSVHeader + "\n")
	b.WriteString(walkRow("uvx -y @mui/mcp", 5001, 5000) + "\n") // candidate
	for i := 1; i < 20; i++ {
		b.WriteString(walkRow("some-intermediate-proc", 5000+i+1, 5000+i) + "\n")
	}
	swapOrphanParentState(t, deadParent) // irrelevant (no byPID-miss within the cap) but deterministic
	if got, _ := parseOrphans(strings.NewReader(b.String()), []string{"@mui/mcp"}); len(got) != 0 {
		t.Errorf("a candidate under a >16-deep unresolved live chain must be spared (depth-cap fail-closed); got %d", len(got))
	}
}
