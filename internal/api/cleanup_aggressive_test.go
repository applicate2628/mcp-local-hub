package api

import (
	"strings"
	"testing"
)

// The CSV fixtures below mirror the wmic/CIM snapshot shape that
// runProcessSnapshot emits and parseProcessRows consumes:
//   Node,CommandLine,CreationDate,ExecutablePath,ParentProcessId,ProcessId,WorkingSetSize
// The ExecutablePath column feeds the identity proof the reaper kills through;
// these parser-only candidate-selection tests do not exercise the kill path, so
// each row carries a plausible image path purely to keep the column aligned.
// Ages are set far in the past so the snapshot's CreationDate is old;
// parseAggressiveCandidates itself does not age-filter (AggressiveCleanup
// does), so these unit tests over the parser ignore age.

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
host,"C:\Users\u\AppData\Local\Programs\codex\codex.exe",20250101090000.000000+000,C:\Users\u\AppData\Local\Programs\codex\codex.exe,2000,3000,150000000
host,"node.exe c:\path\to\mcp-server.js",20250101090000.000000+000,C:\Program Files\nodejs\node.exe,3000,4000,80000000
`
	out := parseAggressiveCandidates(strings.NewReader(csv), "codex", 0, nil)
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
host,"C:\Users\u\codex.exe",20250101090000.000000+000,C:\Users\u\codex.exe,1000,3000,150000000
host,"C:\Program Files\mcphub\mcphub.exe daemon --server serena",20250101090000.000000+000,C:\Program Files\mcphub\mcphub.exe,3000,4000,90000000
host,"node.exe c:\path\to\mcp-server.js",20250101090000.000000+000,C:\Program Files\nodejs\node.exe,4000,5000,80000000
`
	out := parseAggressiveCandidates(strings.NewReader(csv), "codex", 0, nil)
	if _, ok := candidatePIDs(out)[5000]; ok {
		t.Errorf("PID 5000 (descendant of mcphub.exe daemon) must be SPARED even under --client codex")
	}
}

// TestParseAggressiveCandidates_SkipsOwnBinaries verifies our own
// binaries are never aggressive targets.
func TestParseAggressiveCandidates_SkipsOwnBinaries(t *testing.T) {
	csv := `Node,CommandLine,CreationDate,ExecutablePath,ParentProcessId,ProcessId,WorkingSetSize
host,"C:\Users\u\codex.exe",20250101090000.000000+000,C:\Users\u\codex.exe,1000,3000,150000000
host,"C:\Program Files\mcphub\godbolt.exe",20250101090000.000000+000,C:\Program Files\mcphub\godbolt.exe,3000,4000,80000000
`
	out := parseAggressiveCandidates(strings.NewReader(csv), "codex", 0, nil)
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
	out := parseAggressiveCandidates(strings.NewReader(csv), "", 7777, nil)
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
host,"uv.exe run server",20250101090000.000000+000,C:\Users\u\.local\bin\uv.exe,4000,7777,150000000
host,"node.exe c:\path\to\mcp-server.js",20250101090000.000000+000,C:\Program Files\nodejs\node.exe,7777,8888,80000000
`
	out := parseAggressiveCandidates(strings.NewReader(csv), "", 7777, nil)
	if _, ok := candidatePIDs(out)[8888]; ok {
		t.Errorf("PID 8888 (descendant of mcphub.exe daemon above root-pid 7777) must be SPARED")
	}
}

// TestParseAggressiveCandidates_DenyListExcludesDangerousClassesByDefault
// verifies a chrome.exe under the scoped client is excluded by default.
func TestParseAggressiveCandidates_DenyListExcludesDangerousClassesByDefault(t *testing.T) {
	csv := `Node,CommandLine,CreationDate,ExecutablePath,ParentProcessId,ProcessId,WorkingSetSize
host,"C:\Users\u\codex.exe",20250101090000.000000+000,C:\Users\u\codex.exe,1000,3000,150000000
host,"C:\Program Files\Google\Chrome\chrome.exe --type=renderer",20250101090000.000000+000,C:\Program Files\Google\Chrome\chrome.exe,3000,4000,300000000
host,"C:\Windows\System32\cmd.exe /c something",20250101090000.000000+000,C:\Windows\System32\cmd.exe,3000,4100,5000000
`
	out := parseAggressiveCandidates(strings.NewReader(csv), "codex", 0, nil)
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
host,"C:\Users\u\codex.exe",20250101090000.000000+000,C:\Users\u\codex.exe,1000,3000,150000000
host,"C:\Program Files\Google\Chrome\chrome.exe --type=renderer",20250101090000.000000+000,C:\Program Files\Google\Chrome\chrome.exe,3000,4000,300000000
host,"C:\Windows\System32\cmd.exe /c something",20250101090000.000000+000,C:\Windows\System32\cmd.exe,3000,4100,5000000
`
	out := parseAggressiveCandidates(strings.NewReader(csv), "codex", 0, []string{"chrome"})
	got := candidatePIDs(out)
	if _, ok := got[4000]; !ok {
		t.Errorf("PID 4000 (chrome.exe) should be INCLUDED when --include-class chrome is set")
	}
	// cmd.exe was NOT opted in, so it stays excluded.
	if _, ok := got[4100]; ok {
		t.Errorf("PID 4100 (cmd.exe) must stay excluded when only chrome is opted in")
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

// TestFilterToExpectedPIDs pins the kill-binding contract (bot #373 R5): a
// freshly-snapshotted candidate set is reduced to ONLY the previously
// token-validated PIDs, so a process spawned after validation is excluded and
// a validated PID that has since exited simply drops out.
func TestFilterToExpectedPIDs(t *testing.T) {
	cands := []OrphanProcess{
		{PID: 100, CmdlineDisplay: "a"},
		{PID: 200, CmdlineDisplay: "b"},
		{PID: 300, CmdlineDisplay: "c"}, // spawned AFTER validation — not in the allowlist
	}
	// Validated set {100, 200, 999}: 300 (new) is excluded; 999 (died) is absent.
	got := filterToExpectedPIDs(cands, []int{100, 200, 999})
	if len(got) != 2 || got[0].PID != 100 || got[1].PID != 200 {
		t.Fatalf("filterToExpectedPIDs = %+v, want only PIDs [100 200] (300 excluded, 999 absent)", got)
	}
	if g := filterToExpectedPIDs(cands, []int{}); len(g) != 0 {
		t.Errorf("empty allowlist → %d kept, want 0 (validated-empty kills nothing)", len(g))
	}
}
