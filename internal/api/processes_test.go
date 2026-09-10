package api

import (
	"strings"
	"testing"

	"mcp-local-hub/internal/config"
	"mcp-local-hub/internal/process"
)

func TestCountProcessesFromSnapshotAttributesMcphubRootsByCompleteIdentityAndKeepsDescendants(t *testing.T) {
	raw := `Node,CommandLine,CreationDate,ExecutablePath,ParentProcessId,ProcessId,WorkingSetSize
HOST,"mcphub.exe lldb",20260417180000.000000+000,mcphub.exe,1,100,10
HOST,"lldb.exe --stdio",20260417180000.000000+000,lldb.exe,100,101,10
HOST,"mcphub.exe daemon --server vcpkg --daemon default",20260417180000.000000+000,mcphub.exe,1,200,10
HOST,"vcpkg.exe serve",20260417180000.000000+000,vcpkg.exe,200,201,10
HOST,"mcphub.exe daemon --server vcpkg --daemon default",20260417180000.000000+000,mcphub.exe,1,210,10
HOST,"vcpkg.exe serve",20260417180000.000000+000,vcpkg.exe,210,211,10
HOST,"node.exe node_modules/mcp-local-hub/bin/mcphub.js daemon --server vcpkg --daemon default",20260417180000.000000+000,node.exe,1,220,10
HOST,"vcpkg.exe serve",20260417180000.000000+000,vcpkg.exe,220,221,10
HOST,"mcphub.exe godbolt",20260417180000.000000+000,mcphub.exe,1,300,10
HOST,"node godbolt-child.js",20260417180000.000000+000,node.exe,300,301,10
HOST,"mcphub.exe unrelated",20260417180000.000000+000,mcphub.exe,1,400,10
HOST,"mcphub.exe scan --processes",20260417180000.000000+000,mcphub.exe,1,500,10
`
	snap := processSnapshot{raw: raw, lines: splitSnapshotLines(raw)}
	for _, tc := range []struct {
		name     string
		patterns []string
		want     int
	}{
		{name: "lldb", patterns: []string{"mcphub", "lldb"}, want: 2},
		{name: "vcpkg canonical duplicate trees plus npm shim", patterns: []string{"mcphub", "vcpkg"}, want: 6},
		{name: "godbolt excludes scan command", patterns: []string{"mcphub", "godbolt"}, want: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := NewAPI().CountProcessesFromSnapshot(snap, tc.patterns); got != tc.want {
				t.Fatalf("CountProcessesFromSnapshot(%v)=%d, want %d", tc.patterns, got, tc.want)
			}
		})
	}
}

func TestStructuredProcessAttributionRequiresManagedDaemonRootIdentity(t *testing.T) {
	raw := `Node,CommandLine,CreationDate,ExecutablePath,ParentProcessId,ProcessId,WorkingSetSize
HOST,"mcphub.exe vcpkg",20260417180000.000000+000,mcphub.exe,1,50,10
HOST,"mcphub.exe daemon --server vcpkg --daemon default",20260417180000.000000+000,mcphub.exe,1,200,10
HOST,"vcpkg.exe serve",20260417180000.000000+000,vcpkg.exe,200,201,10
HOST,"mcphub.exe daemon --server vcpkg --daemon default",20260417180000.000000+000,mcphub.exe,1,210,10
HOST,"vcpkg.exe serve",20260417180000.000000+000,vcpkg.exe,210,211,10
HOST,"node.exe node_modules/mcp-local-hub/bin/mcphub.js daemon --server vcpkg --daemon default",20260417180000.000000+000,node.exe,1,220,10
HOST,"vcpkg.exe serve",20260417180000.000000+000,vcpkg.exe,220,221,10
HOST,"mcphub.exe daemon --server vcpkg-shadow --daemon default",20260417180000.000000+000,mcphub.exe,1,230,10
HOST,"mcphub.exe daemon --server vcpkg --daemon default-old",20260417180000.000000+000,mcphub.exe,1,240,10
HOST,"vcpkg/mcphub.exe daemon --server nope --daemon default",20260417180000.000000+000,mcphub.exe,1,250,10
HOST,"""MCPHUB.EXE"" DaEmOn --SeRvEr VCPKG --DaEmOn DEFAULT",20260417180000.000000+000,mcphub.exe,1,260,10
HOST,"vcpkg.exe serve",20260417180000.000000+000,vcpkg.exe,260,261,10
HOST,"other.exe ""C:\\tools\\mcphub.exe"" daemon --server vcpkg --daemon default",20260417180000.000000+000,other.exe,1,270,10
HOST,"vcpkg.exe serve",20260417180000.000000+000,vcpkg.exe,270,271,10
HOST,"""C:\\vcpkg root\\mcphub.exe"" daemon --server vcpkg --daemon default",20260417180000.000000+000,mcphub.exe,1,280,10
HOST,"vcpkg.exe serve",20260417180000.000000+000,vcpkg.exe,280,281,10
`
	spec := processAttributionForManifest("vcpkg", &config.ServerManifest{Command: "mcphub", BaseArgs: []string{"vcpkg"}, Daemons: []config.DaemonSpec{{Name: "default", Port: 9138}}}, nil)
	if got := countProcessesFromSnapshotAttribution(processSnapshot{raw: raw, lines: splitSnapshotLines(raw)}, spec); got != 10 {
		t.Fatalf("structured Vcpkg attribution=%d, want canonical duplicate roots, npm shim, and quoted canonical root without argument/path collisions", got)
	}
}

func TestProcessAttributionForManifest_UsesManagedLauncherTreeInsteadOfBackendTokenRoots(t *testing.T) {
	raw := `Node,CommandLine,CreationDate,ExecutablePath,ParentProcessId,ProcessId,WorkingSetSize
HOST,"C:\managed\mcphub.exe daemon --server codegraph --daemon default",20260417180000.000000+000,mcphub.exe,1,100,10
HOST,"conhost.exe 0x4",20260417180000.000000+000,conhost.exe,100,101,10
HOST,"cmd.exe /c codegraph serve --mcp",20260417180000.000000+000,cmd.exe,100,102,10
HOST,"conhost.exe 0x4",20260417180000.000000+000,conhost.exe,102,103,10
HOST,"node.exe npm-shim.js serve --mcp",20260417180000.000000+000,node.exe,102,104,10
HOST,"node.exe codegraph.js serve --mcp",20260417180000.000000+000,node.exe,104,105,10
HOST,"node.exe -e watchdog",20260417180000.000000+000,node.exe,105,106,10
HOST,"C:\foreign\mcphub.exe daemon --server codegraph --daemon default",20260417180000.000000+000,mcphub.exe,1,150,10
HOST,"node.exe codegraph.js serve --mcp",20260417180000.000000+000,node.exe,150,151,10
HOST,"cmd.exe /c codegraph serve --mcp",20260417180000.000000+000,cmd.exe,900,200,10
HOST,"node.exe codegraph.js serve --mcp",20260417180000.000000+000,node.exe,200,201,10
HOST,"mcphub.exe daemon --server foreign --daemon default",20260417180000.000000+000,mcphub.exe,1,300,10
HOST,"cmd.exe /c codegraph serve --mcp",20260417180000.000000+000,cmd.exe,300,301,10
HOST,"node.exe codegraph.js serve --mcp",20260417180000.000000+000,node.exe,301,302,10
HOST,"mcphub.exe daemon --server codegraph --daemon other",20260417180000.000000+000,mcphub.exe,1,400,10
HOST,"cmd.exe /c codegraph serve --mcp",20260417180000.000000+000,cmd.exe,400,401,10
HOST,"node.exe codegraph.js serve --mcp",20260417180000.000000+000,node.exe,401,402,10
`
	manifest := &config.ServerManifest{
		Command:  "codegraph",
		BaseArgs: []string{"serve", "--mcp"},
		Daemons:  []config.DaemonSpec{{Name: "default", Port: 9303}},
	}
	snap := processSnapshot{raw: raw, lines: splitSnapshotLines(raw)}
	descriptors := []SupervisorDaemon{{
		Server: "codegraph", Daemon: "default", Command: `C:\managed\mcphub.exe`,
		Args: []string{"daemon", "--server", "codegraph", "--daemon", "default"},
	}}
	if got := countProcessesFromSnapshotAttribution(snap, processAttributionForManifest("codegraph", manifest, descriptors)); got != 7 {
		t.Fatalf("CodeGraph managed launcher tree count=%d, want 7; foreign same-command and wrong launcher identities must be excluded", got)
	}
}

func TestProcessAttributionForManifest_SeparatesMultipleManagedDaemons(t *testing.T) {
	raw := `Node,CommandLine,CreationDate,ExecutablePath,ParentProcessId,ProcessId,WorkingSetSize
HOST,"C:\managed\mcphub.exe daemon --server codegraph --daemon alpha",20260417180000.000000+000,mcphub.exe,1,100,10
HOST,"node.exe alpha-child",20260417180000.000000+000,node.exe,100,101,10
HOST,"C:\managed\mcphub.exe daemon --server codegraph --daemon beta",20260417180000.000000+000,mcphub.exe,1,200,10
HOST,"node.exe beta-child",20260417180000.000000+000,node.exe,200,201,10
HOST,"C:\managed\mcphub.exe daemon --server foreign --daemon alpha",20260417180000.000000+000,mcphub.exe,1,300,10
HOST,"node.exe foreign-child",20260417180000.000000+000,node.exe,300,301,10
`
	descriptors := []SupervisorDaemon{
		{Server: "codegraph", Daemon: "alpha", Command: `C:\managed\mcphub.exe`, Args: []string{"daemon", "--server", "codegraph", "--daemon", "alpha"}},
		{Server: "codegraph", Daemon: "beta", Command: `C:\managed\mcphub.exe`, Args: []string{"daemon", "--server", "codegraph", "--daemon", "beta"}},
	}
	result := processAttributionFromSnapshot(
		processSnapshot{raw: raw, lines: splitSnapshotLines(raw)},
		processAttributionForManifest("codegraph", &config.ServerManifest{Command: "codegraph", Daemons: []config.DaemonSpec{{Name: "alpha"}, {Name: "beta"}}}, descriptors),
	)
	if result.count != 4 || len(result.diagnostic.Contributors) != 4 {
		t.Fatalf("managed multi-daemon count/contributors=%d/%d, want 4/4", result.count, len(result.diagnostic.Contributors))
	}
	wantDaemon := map[int]string{100: "alpha", 101: "alpha", 200: "beta", 201: "beta"}
	for _, contributor := range result.diagnostic.Contributors {
		if got, ok := wantDaemon[contributor.PID]; !ok || contributor.Daemon != got {
			t.Fatalf("unexpected contributor %+v; want only alpha/beta managed trees", contributor)
		}
	}
}

func TestProcessAttributionForManifest_PreservesLegacyGlobalCommandMatch(t *testing.T) {
	raw := `Node,CommandLine,CreationDate,ExecutablePath,ParentProcessId,ProcessId,WorkingSetSize
HOST,"cmd.exe /c codegraph serve --mcp",20260417180000.000000+000,cmd.exe,1,100,10
HOST,"node.exe codegraph.js serve --mcp",20260417180000.000000+000,node.exe,100,101,10
`
	result := processAttributionFromSnapshot(
		processSnapshot{raw: raw, lines: splitSnapshotLines(raw)},
		processAttributionForManifest("codegraph", &config.ServerManifest{Command: "codegraph", BaseArgs: []string{"serve", "--mcp"}}, nil),
	)
	if result.count != 2 || result.diagnostic.Scope != processAttributionScopeGlobal || result.diagnostic.State != processAttributionStateComplete {
		t.Fatalf("legacy attribution=%+v count=%d, want complete global-command-match count 2", result.diagnostic, result.count)
	}
	if result.diagnostic.Contributors[0].Relation != "match-root" || result.diagnostic.Contributors[0].Daemon != "" {
		t.Fatalf("legacy root contributor=%+v, want match-root with no daemon", result.diagnostic.Contributors[0])
	}
}

func TestProcessAttributionFromSnapshot_MalformedSnapshotIsUnavailable(t *testing.T) {
	attribution := processAttributionForManifest("codegraph", &config.ServerManifest{Command: "codegraph", Daemons: []config.DaemonSpec{{Name: "default"}}}, []SupervisorDaemon{{
		Server: "codegraph", Daemon: "default", Command: `C:\managed\mcphub.exe`, Args: []string{"daemon", "--server", "codegraph", "--daemon", "default"},
	}})
	result := processAttributionFromSnapshot(processSnapshot{raw: "not-a-process-snapshot", lines: []string{"not-a-process-snapshot"}}, attribution)
	if result.count != 0 || result.diagnostic.State != processAttributionStateUnavailable || result.diagnostic.ReasonID != "process-snapshot-unavailable" || len(result.diagnostic.Contributors) != 0 {
		t.Fatalf("malformed snapshot result=%+v count=%d, want unavailable process-snapshot-unavailable with no contributors", result.diagnostic, result.count)
	}
}

func TestManagedLauncherIdentity_PreservesArgumentCaseWhileNormalizingWindowsCommandPath(t *testing.T) {
	commandLine := `c:\MANAGED\MCPHUB.EXE daemon --server codegraph --daemon default -B OpaqueValue`
	argv := process.TokenizeWindowsCommandLine(commandLine)
	if len(argv) != 8 || argv[6] != "-B" || argv[7] != "OpaqueValue" {
		t.Fatalf("tokenizer changed argument case: %#v", argv)
	}
	base := processRootIdentity{
		command:      `C:\managed\mcphub.exe`,
		argvSequence: []string{"daemon", "--server", "codegraph", "--daemon", "default", "-B", "OpaqueValue"},
	}
	if !processArgvMatchesIdentity(argv, base) {
		t.Fatal("Windows command-path case variation must match exact ordered arguments")
	}
	for _, tc := range []struct {
		name string
		args []string
	}{
		{name: "flag case differs", args: []string{"daemon", "--server", "codegraph", "--daemon", "default", "-b", "OpaqueValue"}},
		{name: "opaque value case differs", args: []string{"daemon", "--server", "codegraph", "--daemon", "default", "-B", "opaquevalue"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			identity := base
			identity.argvSequence = tc.args
			if processArgvMatchesIdentity(argv, identity) {
				t.Fatalf("managed launcher matched different argument semantics: argv=%#v identity=%#v", argv, identity.argvSequence)
			}
		})
	}
}

func TestCountProcessesFromSnapshotFailsClosedForCompleteIdentityWithoutAncestry(t *testing.T) {
	for _, raw := range []string{
		`Node,CommandLine,ProcessId,WorkingSetSize
HOST,"mcphub.exe daemon --server vcpkg --daemon default",200,10
HOST,"mcphub.exe daemon --server godbolt --daemon default",300,10
`,
		`Node,CommandLine,CreationDate,ExecutablePath,ParentProcessId,ProcessId,WorkingSetSize
HOST,"mcphub.exe daemon --server vcpkg --daemon default",20260417180000.000000+000,mcphub.exe,not-a-parent,200,10
`,
	} {
		snap := processSnapshot{raw: raw, lines: splitSnapshotLines(raw)}
		if got := NewAPI().CountProcessesFromSnapshot(snap, []string{"mcphub", "vcpkg"}); got != 0 {
			t.Fatalf("malformed complete identity snapshot counted %d process(es), want fail-closed zero", got)
		}
	}
}

func TestProcessAttributionWithoutManifestDoesNotClaimTokenMentions(t *testing.T) {
	raw := `Node,CommandLine,CreationDate,ExecutablePath,ParentProcessId,ProcessId,WorkingSetSize
HOST,"powershell.exe -NoProfile -Command \"Write-Output graphify\"",20260417180000.000000+000,powershell.exe,1,100,10
HOST,"cmd.exe /c echo graphify",20260417180000.000000+000,cmd.exe,1,101,10
HOST,"python.exe helper.py --log-message graphify",20260417180000.000000+000,python.exe,1,102,10
HOST,"node.exe tools/graphify-wrapper.js",20260417180000.000000+000,node.exe,1,103,10
`
	snap := processSnapshot{raw: raw, lines: splitSnapshotLines(raw)}
	if got := countProcessesFromSnapshotAttribution(snap, processAttributionForManifest("graphify", nil, nil)); got != 0 {
		t.Fatalf("manifest-less graphify attribution=%d, want no claimed ownership from command-token mentions", got)
	}
}

// TestCountProcessesHandlesEmptyInput verifies the parser returns (0, nil)
// on blank wmic output — zero processes matching, no error.
func TestCountProcessesHandlesEmptyInput(t *testing.T) {
	got, err := parseWmicCount(strings.NewReader(""), []string{"memory", "server-memory"})
	if err != nil {
		t.Fatal(err)
	}
	if got != 0 {
		t.Errorf("got %d, want 0", got)
	}
}

// TestCountProcessesMatchesSubstrings verifies a line containing any of the
// patterns counts once; a line containing multiple patterns still counts once.
func TestCountProcessesMatchesSubstrings(t *testing.T) {
	wmicCsv := `Node,CommandLine,ProcessId,WorkingSetSize
HOST,"npx -y @modelcontextprotocol/server-memory",1234,41000000
HOST,"node server-memory/dist/index.js",5678,40000000
HOST,"some-other-process",9999,1000000
`
	got, err := parseWmicCount(strings.NewReader(wmicCsv), []string{"server-memory"})
	if err != nil {
		t.Fatal(err)
	}
	if got != 2 {
		t.Errorf("got %d, want 2 (both lines mention server-memory)", got)
	}
}

func TestCountProcessesMatchesCommandLineOnly(t *testing.T) {
	wmicCsv := `Node,CommandLine,CreationDate,ExecutablePath,ParentProcessId,ProcessId,WorkingSetSize
HOST,"node unrelated.js",20260417180000.000000+180,node.exe,555,1001,40000000
HOST,"node server-memory/dist/index.js",20260417180000.000000+180,node.exe,555,1002,41000000
`
	got, err := parseWmicCount(strings.NewReader(wmicCsv), []string{"server-memory"})
	if err != nil {
		t.Fatal(err)
	}
	if got != 1 {
		t.Errorf("got %d, want 1 (only CommandLine should be matched)", got)
	}
}

func TestNetstatLinePIDForLoopbackPort_ExactPortMatch(t *testing.T) {
	line := "  TCP    127.0.0.1:9121         0.0.0.0:0              LISTENING       1234"
	pid, ok := netstatLinePIDForLoopbackPort(line, 9121)
	if !ok {
		t.Fatalf("expected exact match, got no match")
	}
	if pid != 1234 {
		t.Fatalf("expected pid 1234, got %d", pid)
	}
}

func TestNetstatLinePIDForLoopbackPort_DoesNotMatchPortPrefix(t *testing.T) {
	line := "  TCP    127.0.0.1:91210        0.0.0.0:0              LISTENING       4242"
	if pid, ok := netstatLinePIDForLoopbackPort(line, 9121); ok {
		t.Fatalf("expected no match for prefix port, got pid %d", pid)
	}
}

func TestNetstatLinePIDForLoopbackPort_DoesNotMatchNonLoopback(t *testing.T) {
	line := "  TCP    0.0.0.0:9121           0.0.0.0:0              LISTENING       777"
	if pid, ok := netstatLinePIDForLoopbackPort(line, 9121); ok {
		t.Fatalf("expected no match for non-loopback, got pid %d", pid)
	}
}

// TestNetstatLineLoopbackPortPID_BlobToMap drives the SHARED parser behind
// LoopbackPortOwnersSnapshot's Windows path over a representative `netstat -ano`
// blob and asserts only the IPv4-loopback LISTENING rows land in the
// port -> pid map. ESTABLISHED rows, non-loopback (0.0.0.0 / LAN) rows, the
// IPv6 [::1] loopback form, UDP rows, the header lines, and a zero-PID row are
// all excluded — exactly the gate per-port loopbackPortOwnerPID applies. The
// blob is parsed in-process; no real netstat is spawned. (This mirrors the
// line loop inside loopbackPortOwnersSnapshot on Windows so the cross-platform
// test still covers the Windows parsing contract.)
func TestNetstatLineLoopbackPortPID_BlobToMap(t *testing.T) {
	blob := `
Active Connections

  Proto  Local Address          Foreign Address        State           PID
  TCP    127.0.0.1:9121         0.0.0.0:0              LISTENING       1234
  TCP    127.0.0.1:9123         0.0.0.0:0              LISTENING       5678
  TCP    127.0.0.1:9200         127.0.0.1:54321       ESTABLISHED     9999
  TCP    0.0.0.0:9300           0.0.0.0:0              LISTENING       4242
  TCP    192.168.1.10:9400      0.0.0.0:0              LISTENING       4343
  TCP    [::1]:9500             [::]:0                 LISTENING       4444
  TCP    127.0.0.1:9600         0.0.0.0:0              LISTENING       0
  UDP    127.0.0.1:9700         *:*                                    4545
  TCP    127.0.0.1:91210        0.0.0.0:0              LISTENING       7777
`
	owners := map[int]int{}
	for line := range strings.SplitSeq(blob, "\n") {
		if port, pid, ok := netstatLineLoopbackPortPID(line); ok {
			if _, seen := owners[port]; !seen {
				owners[port] = pid
			}
		}
	}

	want := map[int]int{
		9121:  1234, // v4 loopback LISTENING
		9123:  5678, // v4 loopback LISTENING
		91210: 7777, // a different port that merely shares a prefix with 9121
	}
	if len(owners) != len(want) {
		t.Fatalf("map has %d entries, want %d: %v", len(owners), len(want), owners)
	}
	for port, wantPID := range want {
		if got, ok := owners[port]; !ok || got != wantPID {
			t.Fatalf("owners[%d] = (%d, %v), want (%d, true)", port, got, ok, wantPID)
		}
	}
	// Spot-check the explicit exclusions so a future parser regression that
	// admits any of them fails here loudly.
	for _, excluded := range []int{9200, 9300, 9400, 9500, 9600, 9700} {
		if _, ok := owners[excluded]; ok {
			t.Fatalf("port %d must be excluded but is present: %v", excluded, owners)
		}
	}
}

// TestNetstatLineLoopbackPortPID_FirstRowWins asserts that when two LISTENING
// rows report the same loopback port (netstat can surface duplicates), the
// FIRST row's PID wins — matching loopbackPortOwnerPID, which returns the first
// matching line, and matching the snapshot builder's first-write-wins guard.
func TestNetstatLineLoopbackPortPID_FirstRowWins(t *testing.T) {
	blob := "  TCP    127.0.0.1:9121  0.0.0.0:0  LISTENING  1111\n  TCP    127.0.0.1:9121  0.0.0.0:0  LISTENING  2222\n"
	owners := map[int]int{}
	for line := range strings.SplitSeq(blob, "\n") {
		if port, pid, ok := netstatLineLoopbackPortPID(line); ok {
			if _, seen := owners[port]; !seen {
				owners[port] = pid
			}
		}
	}
	if owners[9121] != 1111 {
		t.Fatalf("owners[9121] = %d, want first-row 1111", owners[9121])
	}
}
