package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
	"mcp-local-hub/internal/process"
)

// --- shared fixtures ---------------------------------------------------------

func globalDaemonDescriptor() api.SupervisorDaemon {
	return api.SupervisorDaemon{
		TaskName: `\mcp-local-hub-memory-default`,
		Server:   "memory",
		Daemon:   "default",
		Command:  `C:\mcphub.exe`,
		Args:     []string{"daemon", "--server", "memory", "--daemon", "default"},
		Port:     9123,
	}
}

func serenaProxyDescriptor() api.SupervisorDaemon {
	task := `\mcp-local-hub-serena-b133f336`
	return api.SupervisorDaemon{
		TaskName: task,
		Server:   "serena",
		Daemon:   "wskey",
		Command:  `C:\mcphub.exe`,
		Args: []string{
			"daemon", "serena-proxy",
			"--server", "serena",
			"--workspace", `C:\proj`,
			"--port", "9151",
			"--task-name", task,
		},
		Port: 9151,
	}
}

func lspWorkspaceProxyDescriptor() api.SupervisorDaemon {
	return api.SupervisorDaemon{
		TaskName: `\mcp-local-hub-mcp-language-server-go-abc`,
		Server:   "mcp-language-server",
		Daemon:   "go-abc",
		Command:  `C:\mcphub.exe`,
		Args: []string{
			"daemon", "workspace-proxy",
			"--port", "9401",
			"--workspace", `C:\ws`,
			"--language", "go",
		},
		Port: 9401,
	}
}

// squatterIdentityFor builds a ProcessIdentity whose CommandLine is what the
// supervisor would have spawned for descriptor d (exe + verbatim args), so a
// disowned own-child of d passes the argv gate.
func squatterIdentityFor(pid int, d api.SupervisorDaemon) process.ProcessIdentity {
	parts := []string{quoteCmdArg(d.Command)}
	parts = append(parts, d.Args...)
	return process.ProcessIdentity{
		PID:              pid,
		Basename:         "mcphub.exe",
		CommandLine:      joinCmdLine(parts),
		ExecutablePath:   d.Command,
		CreationDateUnix: time.Now().Add(-time.Minute).Unix(),
	}
}

func quoteCmdArg(s string) string {
	if strings.ContainsAny(s, " \t") {
		return `"` + s + `"`
	}
	return s
}

func joinCmdLine(parts []string) string {
	quoted := make([]string, len(parts))
	for i, p := range parts {
		quoted[i] = quoteCmdArg(p)
	}
	return strings.Join(quoted, " ")
}

// setSquatterLookupForTest swaps the identity lookup + exe-match seams.
func setSquatterLookupForTest(t *testing.T, lookup func(int) (process.ProcessIdentity, error), exeMatch func(int, string) bool) {
	t.Helper()
	pl, pe := squatterLookupIdentityFn, squatterExeMatchesFn
	squatterLookupIdentityFn = lookup
	if exeMatch != nil {
		squatterExeMatchesFn = exeMatch
	}
	t.Cleanup(func() { squatterLookupIdentityFn, squatterExeMatchesFn = pl, pe })
}

func alwaysExeMatch(int, string) bool { return true }

// --- classifier gates --------------------------------------------------------

func TestClassifyPortSquatter_OwnGlobalDaemon(t *testing.T) {
	d := globalDaemonDescriptor()
	const owner = 44000
	setSquatterLookupForTest(t, func(pid int) (process.ProcessIdentity, error) {
		if pid != owner {
			t.Fatalf("lookup pid = %d, want %d", pid, owner)
		}
		return squatterIdentityFor(owner, d), nil
	}, alwaysExeMatch)

	tracked := map[string]DaemonRuntimeEntry{
		canonicalSupervisorTaskName(d.TaskName): {CurrentPID: 22036},
	}
	verdict, id := classifyPortSquatter(d, owner, 1, tracked)
	if verdict != squatterOwnTask {
		t.Fatalf("verdict = %v, want squatterOwnTask", verdict)
	}
	if id.PID != owner {
		t.Fatalf("returned id.PID = %d, want %d (proof must come from THIS pass's lookup)", id.PID, owner)
	}
}

func TestClassifyPortSquatter_OwnSerenaProxyByTaskName(t *testing.T) {
	d := serenaProxyDescriptor()
	const owner = 55001
	setSquatterLookupForTest(t, func(int) (process.ProcessIdentity, error) {
		return squatterIdentityFor(owner, d), nil
	}, alwaysExeMatch)

	verdict, _ := classifyPortSquatter(d, owner, 1, map[string]DaemonRuntimeEntry{
		canonicalSupervisorTaskName(d.TaskName): {CurrentPID: 22036},
	})
	if verdict != squatterOwnTask {
		t.Fatalf("verdict = %v, want squatterOwnTask (serena-proxy by --task-name)", verdict)
	}
}

func TestClassifyPortSquatter_RejectsForeignExe(t *testing.T) {
	d := globalDaemonDescriptor()
	const owner = 44000
	// Lookup succeeds (process exists) but the handle-truth exe gate says NO.
	setSquatterLookupForTest(t, func(int) (process.ProcessIdentity, error) {
		return squatterIdentityFor(owner, d), nil
	}, func(int, string) bool { return false })

	verdict, _ := classifyPortSquatter(d, owner, 1, nil)
	if verdict != squatterForeign {
		t.Fatalf("verdict = %v, want squatterForeign (exe mismatch → Foreign, distinct from Unverified)", verdict)
	}
}

func TestClassifyPortSquatter_RejectsOtherTasksArgv(t *testing.T) {
	d := serenaProxyDescriptor() // discriminator: --task-name \mcp-local-hub-serena-b133f336

	cases := []struct {
		name        string
		commandLine string
	}{
		{"prefix-overlap task-name", `"C:\mcphub.exe" daemon serena-proxy --server serena --workspace "C:\proj" --port 9151 --task-name \mcp-local-hub-serena-b1`},
		{"sibling gui", `"C:\mcphub.exe" gui`},
		{"sibling supervise", `"C:\mcphub.exe" supervise`},
		{"sibling status", `"C:\mcphub.exe" status`},
		{"sibling daemon recover positional task", `"C:\mcphub.exe" daemon recover \mcp-local-hub-serena-b133f336`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			const owner = 60000
			setSquatterLookupForTest(t, func(int) (process.ProcessIdentity, error) {
				return process.ProcessIdentity{PID: owner, Basename: "mcphub.exe", CommandLine: tc.commandLine, ExecutablePath: d.Command, CreationDateUnix: time.Now().Unix()}, nil
			}, alwaysExeMatch)
			verdict, _ := classifyPortSquatter(d, owner, 1, nil)
			if verdict != squatterForeign {
				t.Fatalf("verdict = %v, want squatterForeign for %q (argv must name THIS task exactly)", verdict, tc.commandLine)
			}
		})
	}
}

func TestClassifyPortSquatter_RejectsTrackedSibling(t *testing.T) {
	d := globalDaemonDescriptor()
	const owner = 44000
	// If the lookup/exe/argv gates ran they would say OwnTask — prove gate 2
	// (tracked sibling) short-circuits BEFORE any kill can be authorized.
	setSquatterLookupForTest(t, func(int) (process.ProcessIdentity, error) {
		return squatterIdentityFor(owner, d), nil
	}, alwaysExeMatch)

	t.Run("sibling CurrentPID", func(t *testing.T) {
		tracked := map[string]DaemonRuntimeEntry{
			canonicalSupervisorTaskName(d.TaskName): {CurrentPID: 22036},
			`\mcp-local-hub-time-default`:           {CurrentPID: owner}, // another task owns this PID
		}
		if v, _ := classifyPortSquatter(d, owner, 1, tracked); v != squatterForeign {
			t.Fatalf("verdict = %v, want squatterForeign (must not kill a sibling's CurrentPID)", v)
		}
	})
	t.Run("sibling OrphanPID", func(t *testing.T) {
		tracked := map[string]DaemonRuntimeEntry{
			canonicalSupervisorTaskName(d.TaskName): {CurrentPID: 22036},
			`\mcp-local-hub-time-default`:           {CurrentPID: 999, OrphanPID: owner},
		}
		if v, _ := classifyPortSquatter(d, owner, 1, tracked); v != squatterForeign {
			t.Fatalf("verdict = %v, want squatterForeign (must not kill a sibling's OrphanPID)", v)
		}
	})
}

func TestClassifyPortSquatter_LookupErrorUnverified(t *testing.T) {
	d := globalDaemonDescriptor()
	setSquatterLookupForTest(t, func(int) (process.ProcessIdentity, error) {
		return process.ProcessIdentity{}, fmt.Errorf("simulated OpenProcess ACCESS_DENIED")
	}, alwaysExeMatch)
	if v, _ := classifyPortSquatter(d, 44000, 1, nil); v != squatterUnverified {
		t.Fatalf("verdict = %v, want squatterUnverified (lookup failure fails closed)", v)
	}
}

func TestClassifyPortSquatter_NonWindowsUnverified(t *testing.T) {
	d := globalDaemonDescriptor()
	prev := squatterLookupIdentityFn
	squatterLookupIdentityFn = nil // simulate non-Windows (no start-time-proof lookup)
	t.Cleanup(func() { squatterLookupIdentityFn = prev })
	if v, _ := classifyPortSquatter(d, 44000, 1, nil); v != squatterUnverified {
		t.Fatalf("verdict = %v, want squatterUnverified (non-Windows is observe-only, no kill)", v)
	}
}

// TestClassifyPortSquatter_GlobalDaemonSubcommandAnchor is F2: a sibling
// subcommand that ALSO registers --server/--daemon flags (relay, restart, stop,
// install) must NOT pass the global-daemon gate — the observed argv must anchor
// the `daemon` subcommand in command position. The real `daemon --server X
// --daemon Y` still matches.
func TestClassifyPortSquatter_GlobalDaemonSubcommandAnchor(t *testing.T) {
	d := globalDaemonDescriptor() // Server=memory, Daemon=default
	const owner = 44000
	set := func(cl string) {
		setSquatterLookupForTest(t, func(int) (process.ProcessIdentity, error) {
			return process.ProcessIdentity{PID: owner, Basename: "mcphub.exe", CommandLine: cl, ExecutablePath: d.Command, CreationDateUnix: time.Now().Unix()}, nil
		}, alwaysExeMatch)
	}

	foreign := []string{
		`"C:\mcphub.exe" relay --server memory --daemon default`,
		`"C:\mcphub.exe" restart --server memory --daemon default`,
		`"C:\mcphub.exe" stop --server memory --daemon default`,
		`"C:\mcphub.exe" install --server memory --daemon default`,
		`"C:\mcphub.exe" daemon serena-proxy --server memory --daemon default`, // proxy subcommand, not a global daemon
	}
	for _, cl := range foreign {
		set(cl)
		if v, _ := classifyPortSquatter(d, owner, 1, nil); v != squatterForeign {
			t.Fatalf("verdict = %v for %q, want squatterForeign (subcommand anchor must reject siblings)", v, cl)
		}
	}

	set(`"C:\mcphub.exe" daemon --server memory --daemon default`)
	if v, _ := classifyPortSquatter(d, owner, 1, nil); v != squatterOwnTask {
		t.Fatalf("verdict = %v for a real global daemon argv, want squatterOwnTask", v)
	}
}

// TestClassifyPortSquatter_LSPWorkspaceProxy is F4: the LSP workspace-proxy argv
// shape (--workspace + --language, anchored on `daemon workspace-proxy`).
func TestClassifyPortSquatter_LSPWorkspaceProxy(t *testing.T) {
	d := lspWorkspaceProxyDescriptor()
	const owner = 47001
	set := func(cl string) {
		setSquatterLookupForTest(t, func(int) (process.ProcessIdentity, error) {
			return process.ProcessIdentity{PID: owner, Basename: "mcphub.exe", CommandLine: cl, ExecutablePath: d.Command, CreationDateUnix: time.Now().Unix()}, nil
		}, alwaysExeMatch)
	}

	t.Run("own task", func(t *testing.T) {
		set(joinCmdLine(append([]string{d.Command}, d.Args...)))
		if v, _ := classifyPortSquatter(d, owner, 1, nil); v != squatterOwnTask {
			t.Fatalf("verdict = %v, want squatterOwnTask", v)
		}
	})
	t.Run("different workspace foreign", func(t *testing.T) {
		set(`"C:\mcphub.exe" daemon workspace-proxy --port 9401 --workspace C:\other --language go`)
		if v, _ := classifyPortSquatter(d, owner, 1, nil); v != squatterForeign {
			t.Fatalf("verdict = %v, want squatterForeign (different workspace)", v)
		}
	})
	t.Run("different language foreign", func(t *testing.T) {
		set(`"C:\mcphub.exe" daemon workspace-proxy --port 9401 --workspace C:\ws --language python`)
		if v, _ := classifyPortSquatter(d, owner, 1, nil); v != squatterForeign {
			t.Fatalf("verdict = %v, want squatterForeign (different language)", v)
		}
	})
	t.Run("unknown descriptor shape foreign", func(t *testing.T) {
		unknown := api.SupervisorDaemon{TaskName: `\mcp-local-hub-weird`, Command: `C:\mcphub.exe`, Args: []string{"restart", "--server", "x"}, Port: 9401}
		set(`"C:\mcphub.exe" restart --server x`)
		if v, _ := classifyPortSquatter(unknown, owner, 1, nil); v != squatterForeign {
			t.Fatalf("verdict = %v, want squatterForeign (unknown descriptor shape fails closed)", v)
		}
	})
}

func TestClassifyPortSquatter_Gate1SelfAndOwnChild(t *testing.T) {
	d := globalDaemonDescriptor()
	setSquatterLookupForTest(t, func(int) (process.ProcessIdentity, error) {
		t.Fatal("lookup must not run when gate 1 rejects")
		return process.ProcessIdentity{}, nil
	}, alwaysExeMatch)
	// owner == self
	if v, _ := classifyPortSquatter(d, 4242, 4242, nil); v != squatterUnverified {
		t.Fatalf("owner==self verdict = %v, want squatterUnverified", v)
	}
	// owner == this task's own current child
	tracked := map[string]DaemonRuntimeEntry{canonicalSupervisorTaskName(d.TaskName): {CurrentPID: 22036}}
	if v, _ := classifyPortSquatter(d, 22036, 1, tracked); v != squatterUnverified {
		t.Fatalf("owner==own-child verdict = %v, want squatterUnverified", v)
	}
}

// --- tokenizer + proof -------------------------------------------------------

func TestTokenizeWindowsCommandLine_QuotedSpaces(t *testing.T) {
	// A workspace path with a space must survive as ONE token so the argv gate
	// compares the whole path, not a split fragment.
	cl := `"C:\mcphub.exe" daemon workspace-proxy --port 9401 --workspace "C:\My Proj\ws" --language go`
	tokens := tokenizeWindowsCommandLine(cl)
	if !commandLineHasAdjacentTokenPair(tokens, "--workspace", `C:\My Proj\ws`) {
		t.Fatalf("quoted workspace path not tokenized as one token; tokens=%q", tokens)
	}
	if !commandLineHasAdjacentTokenPair(tokens, "--language", "go") {
		t.Fatalf("--language go pair missing; tokens=%q", tokens)
	}
}

func TestCommandLineHasAdjacentTokenPair_ExactMatchNoPrefix(t *testing.T) {
	tokens := tokenizeWindowsCommandLine(`x --server memory-cache --daemon default`)
	// Prefix must NOT match: "memory" is a prefix of "memory-cache".
	if commandLineHasAdjacentTokenPair(tokens, "--server", "memory") {
		t.Fatal("prefix matched — exact per-token equality required (P0 friendly-fire hole)")
	}
	if !commandLineHasAdjacentTokenPair(tokens, "--server", "memory-cache") {
		t.Fatal("exact token pair should match")
	}
	// Non-adjacent must NOT match.
	if commandLineHasAdjacentTokenPair(tokens, "--server", "default") {
		t.Fatal("non-adjacent flag/value matched")
	}
}

func TestSquatterProof_SecondPrecisionStartTimeWithinTolerance(t *testing.T) {
	created := time.Now().Add(-90 * time.Second).Unix()
	id := process.ProcessIdentity{PID: 44000, ExecutablePath: `C:\mcphub.exe`, CreationDateUnix: created}
	proof := squatterKillProof(id)
	if proof.PID != 44000 || proof.ExecutablePath != `C:\mcphub.exe` {
		t.Fatalf("proof = %+v, want PID/exe sourced from the fresh identity read (MUST-FIX #3)", proof)
	}
	parsed, err := time.Parse(time.RFC3339Nano, proof.StartedAt)
	if err != nil {
		t.Fatalf("StartedAt %q not RFC3339Nano: %v", proof.StartedAt, err)
	}
	if got := parsed.UTC().Unix(); got != created {
		t.Fatalf("StartedAt unix = %d, want %d (second precision within the 2s identity tolerance)", got, created)
	}
	// An empty identity yields a proof that fails closed (no exe, zero PID).
	empty := squatterKillProof(process.ProcessIdentity{})
	if empty.ExecutablePath != "" || empty.PID != 0 || empty.StartedAt != "" {
		t.Fatalf("empty-identity proof = %+v, want zero-value fail-closed proof", empty)
	}
}

// --- MUST-FIX #1 / H1: field pre-bounding ------------------------------------

func TestSquatterEventBody_OversizedCommandLinePreservesIdentity(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	eventsPath := filepath.Join(stateDir, "supervisor-events.log")
	log, err := api.OpenSupervisorEventLog(eventsPath)
	if err != nil {
		t.Fatalf("open event log: %v", err)
	}
	defer log.Close()

	d := globalDaemonDescriptor()
	const owner = 44000
	// A 100 KB hostile CommandLine — unbounded it would blow past the event
	// log's 16 KB WHOLE-BODY cap and evict the identity into a sentinel.
	huge := "daemon --server memory --daemon default " + strings.Repeat("A", 100*1024)
	id := process.ProcessIdentity{PID: owner, Basename: "mcphub.exe", CommandLine: huge, ExecutablePath: `C:\mcphub.exe`, CreationDateUnix: time.Now().Unix()}

	emitSquatterEvent(log, "daemon-port-squatter-reaped", "sweep", squatterOwnTask, d, owner, id, map[string]any{"note": "test"})

	data, err := os.ReadFile(eventsPath)
	if err != nil {
		t.Fatalf("read event log: %v", err)
	}
	line := strings.TrimSpace(string(data))
	if strings.Contains(line, "_truncated_note") {
		t.Fatalf("whole-body truncation fired — identity evicted (MUST-FIX #1 violated); line:\n%.200s", line)
	}
	if !strings.Contains(line, `"squatter_pid":44000`) {
		t.Fatalf("squatter_pid missing from bounded event; line:\n%.400s", line)
	}
	if !strings.Contains(line, `"executable_path":"C:\\mcphub.exe"`) {
		t.Fatalf("executable_path missing from bounded event; line:\n%.400s", line)
	}
	if !strings.Contains(line, `"verdict":"own_task"`) {
		t.Fatalf("verdict missing from bounded event; line:\n%.400s", line)
	}
	if !strings.Contains(line, `"source":"sweep"`) {
		t.Fatalf("source missing from bounded event; line:\n%.400s", line)
	}
	if !strings.Contains(line, `"actor":`) {
		t.Fatalf("actor missing from bounded event; line:\n%.400s", line)
	}
	if len(line) > 12*1024 {
		t.Fatalf("event line %d bytes — expected well under the 16 KB cap after field bounding", len(line))
	}
}

// --- sweep integration -------------------------------------------------------

// mismatchProbe builds a liveness probe whose PID is alive and whose port is
// owned by ownerPID (a mismatch when ownerPID != the tracked CurrentPID).
func mismatchProbe(ownerPID int) supervisorLivenessProbe {
	return supervisorLivenessProbe{
		PIDAlive:     func(int) bool { return true },
		PortOwnerPID: func(int) (int, bool, error) { return ownerPID, true, nil },
	}
}

func runSweepOnce(t *testing.T, d api.SupervisorDaemon, tracker *DaemonRuntimeTracker, events *api.SupervisorEventLog, reap *squatterSweepReaper) []api.LoopEvent {
	t.Helper()
	stateDir := apitest.HardenedTempDir(t)
	intent := &api.SupervisorIntentFile{Version: 1, Daemons: []api.SupervisorDaemon{d}}
	loop := api.NewEventLoop(16)
	got := make(chan api.LoopEvent, 8)
	loop.RegisterHandler(func(e api.LoopEvent) { got <- e })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go loop.Run(ctx)

	prevSelf := supervisorSelfPIDFn
	supervisorSelfPIDFn = func() int { return 1 }
	t.Cleanup(func() { supervisorSelfPIDFn = prevSelf })

	sweepSupervisorLivenessOnce(stateDir, intent, tracker, loop, events, map[string]int{}, reap)

	var out []api.LoopEvent
	for {
		select {
		case e := <-got:
			out = append(out, e)
		case <-time.After(200 * time.Millisecond):
			return out
		}
	}
}

func TestSweepMismatch_OwnSquatterReapedThenRestart(t *testing.T) {
	d := globalDaemonDescriptor()
	const owner = 44000
	setSquatterLookupForTest(t, func(int) (process.ProcessIdentity, error) {
		return squatterIdentityFor(owner, d), nil
	}, alwaysExeMatch)

	restore := setSupervisorLivenessProbeForTest(mismatchProbe(owner))
	defer restore()

	tracker := NewDaemonRuntimeTracker()
	tracker.MarkSpawned(d.TaskName, 22036, time.Now().UTC().Add(-time.Minute))

	var reapedProof process.PIDIdentityProof
	reapCalled := 0
	reap := &squatterSweepReaper{
		reapFn: func(_ api.SupervisorDaemon, proof process.PIDIdentityProof) error {
			reapCalled++
			reapedProof = proof
			return nil
		},
		limiter: newSquatterReapLimiter(),
	}

	events := runSweepOnce(t, d, tracker, nil, reap)

	if reapCalled != 1 {
		t.Fatalf("reap called %d times, want 1", reapCalled)
	}
	if reapedProof.PID != owner {
		t.Fatalf("reaped proof PID = %d, want %d (the squatter owner)", reapedProof.PID, owner)
	}
	if len(events) != 1 || events[0].Kind != api.EvManualRestart {
		t.Fatalf("events = %+v, want exactly one EvManualRestart (reap then restart)", events)
	}
}

func TestSweepMismatch_ForeignObserveOnlyNoRestart(t *testing.T) {
	d := globalDaemonDescriptor()
	const owner = 44000
	// Exe gate says foreign.
	setSquatterLookupForTest(t, func(int) (process.ProcessIdentity, error) {
		return squatterIdentityFor(owner, d), nil
	}, func(int, string) bool { return false })

	restore := setSupervisorLivenessProbeForTest(mismatchProbe(owner))
	defer restore()

	tracker := NewDaemonRuntimeTracker()
	tracker.MarkSpawned(d.TaskName, 22036, time.Now().UTC().Add(-time.Minute))

	reapCalled := 0
	reap := &squatterSweepReaper{
		reapFn:  func(api.SupervisorDaemon, process.PIDIdentityProof) error { reapCalled++; return nil },
		limiter: newSquatterReapLimiter(),
	}
	events := runSweepOnce(t, d, tracker, nil, reap)

	if reapCalled != 0 {
		t.Fatalf("reap called %d times, want 0 (foreign owner must never be killed)", reapCalled)
	}
	if len(events) != 0 {
		t.Fatalf("events = %+v, want 0 (foreign mismatch is observe-only, no restart)", events)
	}
}

func TestSweepMismatch_ReapAccessDeniedNoRestart(t *testing.T) {
	d := globalDaemonDescriptor()
	const owner = 44000
	setSquatterLookupForTest(t, func(int) (process.ProcessIdentity, error) {
		return squatterIdentityFor(owner, d), nil
	}, alwaysExeMatch)

	restore := setSupervisorLivenessProbeForTest(mismatchProbe(owner))
	defer restore()

	tracker := NewDaemonRuntimeTracker()
	tracker.MarkSpawned(d.TaskName, 22036, time.Now().UTC().Add(-time.Minute))

	reap := &squatterSweepReaper{
		reapFn: func(api.SupervisorDaemon, process.PIDIdentityProof) error {
			return fmt.Errorf("simulated terminate ACCESS_DENIED")
		},
		limiter: newSquatterReapLimiter(),
	}
	events := runSweepOnce(t, d, tracker, nil, reap)

	if len(events) != 0 {
		t.Fatalf("events = %+v, want 0 (a failed reap must NOT restart while the port is held)", events)
	}
}

func TestSweepMismatch_RateLimitDowngrades(t *testing.T) {
	d := globalDaemonDescriptor()
	const owner = 44000
	lookups := 0
	setSquatterLookupForTest(t, func(int) (process.ProcessIdentity, error) {
		lookups++
		return squatterIdentityFor(owner, d), nil
	}, alwaysExeMatch)

	restore := setSupervisorLivenessProbeForTest(mismatchProbe(owner))
	defer restore()

	tracker := NewDaemonRuntimeTracker()
	tracker.MarkSpawned(d.TaskName, 22036, time.Now().UTC().Add(-time.Minute))

	// Shared limiter across two sweeps within 30s: the SECOND sweep's identity
	// lookup is rate-limited (no second lookup, no second reap).
	reapCalled := 0
	limiter := newSquatterReapLimiter()
	reap := &squatterSweepReaper{
		reapFn:  func(api.SupervisorDaemon, process.PIDIdentityProof) error { reapCalled++; return nil },
		limiter: limiter,
	}

	_ = runSweepOnceSharedReaper(t, d, tracker, reap)
	_ = runSweepOnceSharedReaper(t, d, tracker, reap)

	if lookups != 1 {
		t.Fatalf("identity lookups = %d, want 1 (<=1 lookup/30s per task)", lookups)
	}
	if reapCalled != 1 {
		t.Fatalf("reap called %d times, want 1 (second sweep is lookup-rate-limited)", reapCalled)
	}
}

// runSweepOnceSharedReaper runs one sweep reusing a caller-owned reaper (so the
// rate-limit state persists across sweeps).
func runSweepOnceSharedReaper(t *testing.T, d api.SupervisorDaemon, tracker *DaemonRuntimeTracker, reap *squatterSweepReaper) []api.LoopEvent {
	t.Helper()
	return runSweepOnce(t, d, tracker, nil, reap)
}
