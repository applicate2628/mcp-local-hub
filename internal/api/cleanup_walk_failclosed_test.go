package api

import (
	"fmt"
	"strings"
	"testing"
)

// swapOrphanParentAlive injects a deterministic parent-liveness probe for the parseOrphans
// ancestor walk (bug 2026-07-08 walk-fail-closed): tests control whether a PID absent from
// the census fixture reads as alive (a dropped live-parent row → spare) or dead (a real
// orphan → reap), instead of probing whatever real PID the fixture happens to name.
func swapOrphanParentAlive(t *testing.T, fn func(int) bool) {
	t.Helper()
	prev := orphanParentAliveFn
	orphanParentAliveFn = fn
	t.Cleanup(func() { orphanParentAliveFn = prev })
}

const walkCSVHeader = "Node,CommandLine,CreationDate,ExecutablePath,ParentProcessId,ProcessId,WorkingSetSize"

func walkRow(cmdline string, ppid, pid int) string {
	return fmt.Sprintf(`host,"%s",20250101090000.000000+000,C:\x.exe,%d,%d,80000000`, cmdline, ppid, pid)
}

// TestParseOrphans_ByPIDMiss_LiveParentSpared is the Case A falsifying probe: a candidate
// whose parent row is ABSENT from the census is spared ONLY when that parent is still alive
// (a dropped/malformed snapshot row of a live protector) and reaped when the parent is
// genuinely dead (a real orphan). A blunt "byPID-miss → spare" would inert the reaper; a
// blunt "byPID-miss → reap" is the pre-fix fail-OPEN that could kill a live-rooted child.
func TestParseOrphans_ByPIDMiss_LiveParentSpared(t *testing.T) {
	// candidate pid=5000, ppid=4000 (4000 is NOT in the census → byPID-miss).
	csv := walkCSVHeader + "\n" + walkRow("uvx -y @mui/mcp", 4000, 5000) + "\n"
	patterns := []string{"@mui/mcp"}

	// Parent 4000 still ALIVE (its census row was dropped) → cannot prove orphan → SPARED.
	swapOrphanParentAlive(t, func(pid int) bool { return pid == 4000 })
	got, _ := parseOrphans(strings.NewReader(csv), patterns)
	if len(got) != 0 {
		t.Errorf("a candidate whose ABSENT parent is still ALIVE must be spared (dropped-row fail-closed); got %d orphan(s)", len(got))
	}

	// Parent 4000 genuinely DEAD → real orphan → REAPED.
	swapOrphanParentAlive(t, func(int) bool { return false })
	got, _ = parseOrphans(strings.NewReader(csv), patterns)
	if len(got) != 1 {
		t.Errorf("a candidate whose ABSENT parent is genuinely DEAD is a real orphan and must be detected; got %d", len(got))
	}
}

// TestParseOrphans_DepthCapExhaustion_Spared is the Case B probe: a candidate under an
// abnormally deep chain of PRESENT, non-protected ancestors that never reaches a real root
// within the 16-level cap is spared (fail closed) rather than falling through to "orphan".
func TestParseOrphans_DepthCapExhaustion_Spared(t *testing.T) {
	// Build a 20-deep chain: candidate 5000 → 5001 → ... → 5020, all present, none a
	// protected launcher, and none with ppid==0 within the first 16 hops. The walk
	// exhausts the depth cap unresolved → spareUncertain.
	var b strings.Builder
	b.WriteString(walkCSVHeader + "\n")
	b.WriteString(walkRow("uvx -y @mui/mcp", 5001, 5000) + "\n") // candidate
	for i := 1; i < 20; i++ {
		b.WriteString(walkRow("some-intermediate-proc", 5000+i+1, 5000+i) + "\n")
	}
	// The chain never hits ppid==0 within the cap; every ancestor is present + generic.
	// Parent-alive probe is irrelevant here (no byPID-miss within the cap) but pin it dead.
	swapOrphanParentAlive(t, func(int) bool { return false })

	got, _ := parseOrphans(strings.NewReader(b.String()), []string{"@mui/mcp"})
	if len(got) != 0 {
		t.Errorf("a candidate under a >16-deep unresolved live chain must be spared (depth-cap fail-closed); got %d orphan(s)", len(got))
	}
}

// TestParseOrphans_ResolvedToRoot_StillReaped guards the POSITIVE path: a candidate whose
// walk resolves to a genuine root (ppid==0) with no protected ancestor is a real orphan and
// is still reaped — the fail-closed changes must not neuter legitimate reaping.
func TestParseOrphans_ResolvedToRoot_StillReaped(t *testing.T) {
	// candidate 5000 → 5001 (present, generic) → ppid 0 (genuine root).
	csv := walkCSVHeader + "\n" +
		walkRow("uvx -y @mui/mcp", 5001, 5000) + "\n" +
		walkRow("some-root-proc", 0, 5001) + "\n"
	swapOrphanParentAlive(t, func(int) bool { return false })

	got, _ := parseOrphans(strings.NewReader(csv), []string{"@mui/mcp"})
	if len(got) != 1 {
		t.Errorf("a candidate resolving to a genuine root with no protected ancestor is a real orphan; got %d", len(got))
	}
}
