package api

import (
	"runtime"
	"strings"
	"testing"

	"mcp-local-hub/internal/process"
)

// The CSV fixtures below mirror the wmic/CIM snapshot shape that
// runProcessSnapshot emits and parseProcessRows consumes:
//   Node,CommandLine,CreationDate,ExecutablePath,ParentProcessId,ProcessId,WorkingSetSize
// The ExecutablePath column feeds the identity proof the reaper kills through;
// these parser-only candidate-selection tests do not exercise the kill path, so
// each row carries a plausible image path purely to keep the column aligned.
// Ages are set far in the past so the snapshot's CreationDate is old;
// These parser-only fixtures inspect nomination, not retirement authority or
// permission to preview/apply through AggressiveCleanup.

func candidatePIDs(out []OrphanProcess) map[int]OrphanProcess {
	m := map[int]OrphanProcess{}
	for _, o := range out {
		m[o.PID] = o
	}
	return m
}

// TestParseAggressiveCandidates_FindsLiveRootedUnderClient is the core
// inversion: a node.exe stdio child whose ancestor chain contains a live
// codex.exe IS an aggressive candidate (the default safe sweep would
// SPARE it; aggressive mode reclaims it under the explicit --client
// scope).
func TestParseAggressiveCandidates_FindsLiveRootedUnderClient(t *testing.T) {
	csv := `Node,CommandLine,CreationDate,ExecutablePath,ParentProcessId,ProcessId,WorkingSetSize
host,"C:\Windows\explorer.exe",20250101090000.000000+000,C:\Windows\explorer.exe,1000,2000,200000000
host,"C:\synthetic\profile\AppData\Local\Programs\codex\codex.exe",20250101090000.000000+000,C:\synthetic\profile\AppData\Local\Programs\codex\codex.exe,2000,3000,150000000
host,"node.exe c:\path\to\mcp-server.js",20250101090000.000000+000,C:\Program Files\nodejs\node.exe,3000,4000,80000000
`
	swapOrphanParentState(t, deadParent) // deterministic: absent ancestor = dead (no host PID probe)
	out, _ := parseAggressiveCandidates(strings.NewReader(csv), "codex", 0, nil)
	got := candidatePIDs(out)
	c, ok := got[4000]
	if !ok {
		t.Fatalf("PID 4000 (node stdio child of live codex) should be an aggressive candidate; got %d candidates", len(out))
	}
	if c.MatchSource != "codex" {
		t.Errorf("MatchSource = %q, want %q", c.MatchSource, "codex")
	}
	if _, ok := got[3000]; ok {
		t.Errorf("PID 3000 (codex.exe itself) must NOT be a candidate")
	}
}

// TestParseAggressiveCandidates_SparesHubDaemonDescendant verifies the
// no-bypass guard: a child whose chain reaches an mcphub.exe daemon
// BEFORE the scoped client is spared even under --client.
func TestParseAggressiveCandidates_SparesHubDaemonDescendant(t *testing.T) {
	// codex.exe(3000) -> mcphub.exe daemon(4000) -> node.exe(5000).
	// The hub daemon is the nearer ancestor, so PID 5000 is hub-managed.
	csv := `Node,CommandLine,CreationDate,ExecutablePath,ParentProcessId,ProcessId,WorkingSetSize
host,"C:\synthetic\profile\codex.exe",20250101090000.000000+000,C:\synthetic\profile\codex.exe,1000,3000,150000000
host,"C:\Program Files\mcphub\mcphub.exe daemon --server serena",20250101090000.000000+000,C:\Program Files\mcphub\mcphub.exe,3000,4000,90000000
host,"node.exe c:\path\to\mcp-server.js",20250101090000.000000+000,C:\Program Files\nodejs\node.exe,4000,5000,80000000
`
	swapOrphanParentState(t, deadParent) // deterministic: absent ancestor = dead (no host PID probe)
	out, _ := parseAggressiveCandidates(strings.NewReader(csv), "codex", 0, nil)
	if _, ok := candidatePIDs(out)[5000]; ok {
		t.Errorf("PID 5000 (descendant of mcphub.exe daemon) must be SPARED even under --client codex")
	}
}

// TestParseAggressiveCandidates_SkipsOwnBinaries verifies our own
// binaries are never aggressive targets.
func TestParseAggressiveCandidates_SkipsOwnBinaries(t *testing.T) {
	csv := `Node,CommandLine,CreationDate,ExecutablePath,ParentProcessId,ProcessId,WorkingSetSize
host,"C:\synthetic\profile\codex.exe",20250101090000.000000+000,C:\synthetic\profile\codex.exe,1000,3000,150000000
host,"C:\Program Files\mcphub\godbolt.exe",20250101090000.000000+000,C:\Program Files\mcphub\godbolt.exe,3000,4000,80000000
`
	swapOrphanParentState(t, deadParent) // deterministic: absent ancestor = dead (no host PID probe)
	out, _ := parseAggressiveCandidates(strings.NewReader(csv), "codex", 0, nil)
	if _, ok := candidatePIDs(out)[4000]; ok {
		t.Errorf("PID 4000 (our own godbolt.exe) must NOT be an aggressive candidate")
	}
}

// TestParseAggressiveCandidates_RootPIDScope verifies the --root-pid
// scope: descendants of an arbitrary PID are candidates regardless of
// whether the root is a recognized client.
func TestParseAggressiveCandidates_RootPIDScope(t *testing.T) {
	csv := `Node,CommandLine,CreationDate,ExecutablePath,ParentProcessId,ProcessId,WorkingSetSize
host,"C:\some\custom-agent.exe",20250101090000.000000+000,C:\some\custom-agent.exe,1000,7777,150000000
host,"node.exe c:\path\to\mcp-server.js",20250101090000.000000+000,C:\Program Files\nodejs\node.exe,7777,8888,80000000
host,"node.exe c:\unrelated\other.js",20250101090000.000000+000,C:\Program Files\nodejs\node.exe,1000,9999,80000000
`
	swapOrphanParentState(t, deadParent) // deterministic: absent ancestor = dead (no host PID probe)
	out, _ := parseAggressiveCandidates(strings.NewReader(csv), "", 7777, nil)
	got := candidatePIDs(out)
	c, ok := got[8888]
	if !ok {
		t.Fatalf("PID 8888 (descendant of root-pid 7777) should be a candidate")
	}
	if c.MatchSource != "root-pid 7777" {
		t.Errorf("MatchSource = %q, want %q", c.MatchSource, "root-pid 7777")
	}
	if _, ok := got[9999]; ok {
		t.Errorf("PID 9999 (NOT under root-pid 7777) must not be a candidate")
	}
}

// TestParseAggressiveCandidates_RootPIDScopeSparesHubDaemonAncestor
// verifies the no-bypass guard still wins when the requested --root-pid
// is itself below an mcphub daemon. The root match must not stop the
// ancestor walk before the daemon guard can see the higher ancestor.
func TestParseAggressiveCandidates_RootPIDScopeSparesHubDaemonAncestor(t *testing.T) {
	csv := `Node,CommandLine,CreationDate,ExecutablePath,ParentProcessId,ProcessId,WorkingSetSize
host,"C:\Program Files\mcphub\mcphub.exe daemon --server serena",20250101090000.000000+000,C:\Program Files\mcphub\mcphub.exe,1000,4000,90000000
host,"uv.exe run server",20250101090000.000000+000,C:\synthetic\profile\.local\bin\uv.exe,4000,7777,150000000
host,"node.exe c:\path\to\mcp-server.js",20250101090000.000000+000,C:\Program Files\nodejs\node.exe,7777,8888,80000000
`
	swapOrphanParentState(t, deadParent) // deterministic: absent ancestor = dead (no host PID probe)
	out, _ := parseAggressiveCandidates(strings.NewReader(csv), "", 7777, nil)
	if _, ok := candidatePIDs(out)[8888]; ok {
		t.Errorf("PID 8888 (descendant of mcphub.exe daemon above root-pid 7777) must be SPARED")
	}
}

// TestParseAggressiveCandidates_DenyListExcludesDangerousClassesByDefault
// verifies a chrome.exe under the scoped client is excluded by default.
func TestParseAggressiveCandidates_DenyListExcludesDangerousClassesByDefault(t *testing.T) {
	csv := `Node,CommandLine,CreationDate,ExecutablePath,ParentProcessId,ProcessId,WorkingSetSize
host,"C:\synthetic\profile\codex.exe",20250101090000.000000+000,C:\synthetic\profile\codex.exe,1000,3000,150000000
host,"C:\Program Files\Google\Chrome\chrome.exe --type=renderer",20250101090000.000000+000,C:\Program Files\Google\Chrome\chrome.exe,3000,4000,300000000
host,"C:\Windows\System32\cmd.exe /c something",20250101090000.000000+000,C:\Windows\System32\cmd.exe,3000,4100,5000000
`
	swapOrphanParentState(t, deadParent) // deterministic: absent ancestor = dead (no host PID probe)
	out, _ := parseAggressiveCandidates(strings.NewReader(csv), "codex", 0, nil)
	got := candidatePIDs(out)
	if _, ok := got[4000]; ok {
		t.Errorf("PID 4000 (chrome.exe) must be excluded by the default deny-list")
	}
	if _, ok := got[4100]; ok {
		t.Errorf("PID 4100 (cmd.exe) must be excluded by the default deny-list")
	}
}

// TestParseAggressiveCandidates_IncludeClassOverridesWithWarning verifies
// --include-class chrome opts that class back in.
func TestParseAggressiveCandidates_IncludeClassOverrides(t *testing.T) {
	csv := `Node,CommandLine,CreationDate,ExecutablePath,ParentProcessId,ProcessId,WorkingSetSize
host,"C:\synthetic\profile\codex.exe",20250101090000.000000+000,C:\synthetic\profile\codex.exe,1000,3000,150000000
host,"C:\Program Files\Google\Chrome\chrome.exe --type=renderer",20250101090000.000000+000,C:\Program Files\Google\Chrome\chrome.exe,3000,4000,300000000
host,"C:\Windows\System32\cmd.exe /c something",20250101090000.000000+000,C:\Windows\System32\cmd.exe,3000,4100,5000000
`
	swapOrphanParentState(t, deadParent) // deterministic: absent ancestor = dead (no host PID probe)
	out, _ := parseAggressiveCandidates(strings.NewReader(csv), "codex", 0, []string{"chrome"})
	got := candidatePIDs(out)
	if _, ok := got[4000]; !ok {
		t.Errorf("PID 4000 (chrome.exe) should be INCLUDED when --include-class chrome is set")
	}
	// cmd.exe was NOT opted in, so it stays excluded.
	if _, ok := got[4100]; ok {
		t.Errorf("PID 4100 (cmd.exe) must stay excluded when only chrome is opted in")
	}
}

func TestParseAggressiveCandidates_DepthCapExhaustionSparesMatchedScope(t *testing.T) {
	var b strings.Builder
	b.WriteString(walkCSVHeader + "\n")
	b.WriteString(walkRow("node.exe mcp-server.js", 5001, 5000) + "\n")
	for i := 1; i <= 17; i++ {
		cmdline, parentPID := "scope-intermediate.exe", 5000+i+1
		if i == 17 {
			cmdline, parentPID = "mcphub.exe daemon --server fixture", 0
		}
		b.WriteString(walkRow(cmdline, parentPID, 5000+i) + "\n")
	}
	swapOrphanParentState(t, deadParent)
	out, err := parseAggressiveCandidates(strings.NewReader(b.String()), "", 5001, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 0 {
		t.Fatalf("matched scope with a Hub protector beyond the ancestry cap must be spared; got %d candidates", len(out))
	}
}

func TestParseAggressiveCandidates_NonSelfCycleSparesMatchedScope(t *testing.T) {
	csv := walkCSVHeader + "\n" +
		walkRow("node.exe mcp-server.js", 5001, 5000) + "\n" +
		walkRow("codex.exe", 5002, 5001) + "\n" +
		walkRow("cycle-intermediate.exe", 5001, 5002) + "\n"
	swapOrphanParentState(t, deadParent)
	out, err := parseAggressiveCandidates(strings.NewReader(csv), "codex", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 0 {
		t.Fatalf("matched scope with unresolved non-self-cycle ancestry must be spared; got %d candidates", len(out))
	}
}

func TestParseAggressiveCandidates_ResolvesBoundaryAtLastAllowedHop(t *testing.T) {
	for _, tc := range []struct {
		name             string
		presentAncestors int
		genuineRoot      bool
	}{
		{"genuine root", 16, true},
		{"proven-dead missing parent", 15, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var b strings.Builder
			b.WriteString(walkCSVHeader + "\n")
			b.WriteString(walkRow("node.exe mcp-server.js", 5001, 5000) + "\n")
			for i := 1; i <= tc.presentAncestors; i++ {
				parentPID := 5000 + i + 1
				if tc.genuineRoot && i == tc.presentAncestors {
					parentPID = 0
				}
				b.WriteString(walkRow("scope-intermediate.exe", parentPID, 5000+i) + "\n")
			}
			swapOrphanParentState(t, deadParent)
			out, err := parseAggressiveCandidates(strings.NewReader(b.String()), "", 5001, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(out) != 1 || out[0].PID != 5000 || out[0].MatchSource != "root-pid 5001" {
				t.Fatalf("resolved last-hop boundary must keep the scoped candidate; got %+v", out)
			}
		})
	}
}

// TestAggressiveCleanup_RejectsWithoutScope verifies the scope-required
// gate fires before any platform-specific work (so the test is portable).
func TestAggressiveCleanup_RejectsWithoutScope(t *testing.T) {
	a := NewAPI()
	_, err := a.AggressiveCleanup(CleanupOpts{Aggressive: true})
	if err == nil {
		t.Fatal("expected error when neither --client nor --root-pid is set")
	}
	if err != errAggressiveScopeRequired {
		t.Fatalf("got %v, want errAggressiveScopeRequired", err)
	}
}

// TestAggressiveCleanup_RejectsBothScopes verifies setting both scopes is
// rejected (exactly one required).
func TestAggressiveCleanup_RejectsBothScopes(t *testing.T) {
	a := NewAPI()
	_, err := a.AggressiveCleanup(CleanupOpts{Aggressive: true, Client: "codex", RootPID: 1234})
	if err != errAggressiveScopeRequired {
		t.Fatalf("got %v, want errAggressiveScopeRequired", err)
	}
}

// TestAggressiveCleanup_RejectsUnknownClient verifies an unrecognized
// --client name is rejected loudly rather than sweeping nothing.
func TestAggressiveCleanup_RejectsUnknownClient(t *testing.T) {
	a := NewAPI()
	_, err := a.AggressiveCleanup(CleanupOpts{Aggressive: true, Client: "not-a-real-client"})
	if err != errAggressiveUnknownClient {
		t.Fatalf("got %v, want errAggressiveUnknownClient", err)
	}
}

func TestAggressiveCleanup_RetirementAuthorityUnavailable(t *testing.T) {
	oldParent := orphanParentStateFn
	t.Cleanup(func() {
		orphanParentStateFn = oldParent
	})
	parentCalls, terminateCalls := 0, 0
	orphanParentStateFn = func(int) (process.PIDState, error) {
		parentCalls++
		return process.PIDStateUnknown, nil
	}
	swapOrphanTerminator(t, func(process.PIDIdentityProof) error {
		terminateCalls++
		return nil
	})
	const cause = "aggressive cleanup unavailable: creator retirement authority is not established; preview and apply disabled"
	a := NewAPI()
	for _, tc := range []struct {
		name string
		opts CleanupOpts
	}{
		{"default apply", CleanupOpts{RootPID: 1234}},
		{"client preview", CleanupOpts{Client: "codex", Aggressive: true, DryRun: true}},
		{"empty expected set", CleanupOpts{RootPID: 1234, Expect: []ProcIdentity{}}},
		{"confirmed identity and class override", CleanupOpts{Client: "codex", MinAgeSec: 1, IncludeClasses: []string{"chrome"}, Expect: []ProcIdentity{{PID: 4321, StartedAt: "2026-01-01T00:00:00Z"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Re-entry cannot turn the refusal into a successful empty sweep.
			for attempt := 0; attempt < 2; attempt++ {
				got, err := a.AggressiveCleanup(tc.opts)
				if got != nil {
					t.Fatalf("candidates = %+v, want nil", got)
				}
				if runtime.GOOS == "windows" {
					if err != errAggressiveRetirementAuthorityUnavailable || err.Error() != cause {
						t.Fatalf("error = %v, want fixed retirement-authority refusal", err)
					}
				} else if err != nil {
					t.Fatalf("non-Windows empty-success behavior changed: %v", err)
				}
			}
		})
	}
	if parentCalls != 0 || terminateCalls != 0 {
		t.Fatalf("forbidden operations: parent=%d terminate=%d", parentCalls, terminateCalls)
	}
}

// TestAggressiveDenyClasses_ReturnsCopy verifies the exported accessor
// returns a defensive copy (mutating the result must not corrupt the
// package-level deny-list).
func TestAggressiveDenyClasses_ReturnsCopy(t *testing.T) {
	got := AggressiveDenyClasses()
	if len(got) == 0 {
		t.Fatal("AggressiveDenyClasses returned empty slice")
	}
	got[0] = "MUTATED"
	again := AggressiveDenyClasses()
	if again[0] == "MUTATED" {
		t.Error("AggressiveDenyClasses must return a copy; caller mutation leaked into the package state")
	}
}

// The aggressive kill-binding contract (bot #373 R5) is now identity-keyed and covered
// by TestFilterToExpectedIdentities_ExcludesRecycledPID (cleanup_config_absence_test.go);
// the PID-only filterToExpectedPIDs was retired when both reaper paths converged onto
// {PID, StartedAt} binding (bug 2026-07-08 aggressive-cleanup-token-omits-started-at).

// TestParseAggressiveCandidates_TruncatedSnapshotErrors pins the PR #520 all-return-paths
// fail-closed fix: a census row longer than the scanner buffer ends the scan early and
// drops every later row — possibly a live `mcphub.exe daemon` ancestor whose absence would
// mis-scope a hub-managed child as an aggressive candidate. parseAggressiveCandidates must
// surface that error to its callers; a parser fixture is not retirement authority.
func TestParseAggressiveCandidates_TruncatedSnapshotErrors(t *testing.T) {
	header := "Node,CommandLine,CreationDate,ExecutablePath,ParentProcessId,ProcessId,WorkingSetSize"
	normal := `host,"C:\synthetic\profile\codex.exe",20250101090000.000000+000,C:\synthetic\profile\codex.exe,2000,3000,150000000`
	// A CommandLine field past the 1 MiB scanner cap trips bufio.ErrTooLong.
	overlong := `host,"` + strings.Repeat("x", 1100000) + `",20250101090000.000000+000,C:\x.exe,3000,4000,80000000`
	csv := header + "\n" + normal + "\n" + overlong + "\n"

	swapOrphanParentState(t, deadParent) // deterministic: absent ancestor = dead (no host PID probe)
	_, err := parseAggressiveCandidates(strings.NewReader(csv), "codex", 0, nil)
	if err == nil {
		t.Fatal("a truncated census must surface a parser error; got nil")
	}
}

// TestAggressiveConfirmToken_StartedAtDrift pins bug 2026-07-08: the confirm token includes
// StartedAt, so a PID recycled onto a DIFFERENT process (same basename + match-source, new
// start time) between preview and confirm produces a DIFFERENT token → the recompute-and-
// compare refuses the kill instead of killing the replacement.
func TestAggressiveConfirmToken_StartedAtDrift(t *testing.T) {
	base := []OrphanProcess{{PID: 4000, CmdlineDisplay: "node.exe", MatchSource: "codex", StartedAt: "2026-01-01T00:00:00Z"}}
	reused := []OrphanProcess{{PID: 4000, CmdlineDisplay: "node.exe", MatchSource: "codex", StartedAt: "2026-01-01T00:11:00Z"}}

	if AggressiveConfirmToken(base) == AggressiveConfirmToken(reused) {
		t.Error("a PID recycled onto a different-start-time process must change the confirm token (started_at drift)")
	}
	// Same identity → stable token (determinism).
	if AggressiveConfirmToken(base) != AggressiveConfirmToken([]OrphanProcess{{PID: 4000, CmdlineDisplay: "node.exe", MatchSource: "codex", StartedAt: "2026-01-01T00:00:00Z"}}) {
		t.Error("the same candidate identity must produce a stable token")
	}
}

// TestIdentitiesOf projects a candidate set onto {PID, StartedAt} identities for the
// identity-keyed kill binding.
func TestIdentitiesOf(t *testing.T) {
	got := IdentitiesOf([]OrphanProcess{
		{PID: 100, StartedAt: "2026-01-01T00:00:00Z"},
		{PID: 200, StartedAt: "2026-01-01T00:05:00Z"},
	})
	if len(got) != 2 || got[0] != (ProcIdentity{PID: 100, StartedAt: "2026-01-01T00:00:00Z"}) || got[1] != (ProcIdentity{PID: 200, StartedAt: "2026-01-01T00:05:00Z"}) {
		t.Errorf("IdentitiesOf = %+v, want [{100 ...} {200 ...}]", got)
	}
}
