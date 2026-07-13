package cli

import (
	"errors"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
	"mcp-local-hub/internal/config"
)

// isForceExitErr reports whether err carries the (ExitCode + IsMcphubForceExit)
// marker main.go routes to os.Exit(<code>) — i.e. a self-healing daemon-proxy exit
// code rather than cobra's default exit 1. Used to assert the exit-1 (plain error)
// classification without the fatal that the package-shared exitCodeOf enforces.
func isForceExitErr(err error) bool {
	var fe interface {
		ExitCode() int
		IsMcphubForceExit() bool
	}
	return errors.As(err, &fe)
}

// lspDescWithPort returns an lspWorkspaceProxyDescriptor with its --port argv (and
// Port field) rewritten to port — so an intent snapshot can name a specific port.
func lspDescWithPort(port int) api.SupervisorDaemon {
	d := lspWorkspaceProxyDescriptor()
	d.Args = append([]string(nil), d.Args...)
	for i := 0; i+1 < len(d.Args); i++ {
		if d.Args[i] == "--port" {
			d.Args[i+1] = strconv.Itoa(port)
		}
	}
	d.Port = port
	return d
}

// --- FIX-3: LSP registry↔argv port-mismatch classification (crash-window
// startability). The MUST-have blind-spot test: registry=newPort / intent=oldPort
// (and the inverse) must SELF-HEAL (exit-3 re-drive) instead of exit-1-forever.

func TestClassifyLSPPortMismatch_StaleRegistrySelfHeals(t *testing.T) {
	const oldPort, newPort = 9401, 9450

	// (a) crash window: registry reserves the new port, but the daemon is spawned
	// from intent (argv --port = old) which still names the old port. Intent AGREES
	// with argv; only the registry disagrees → the registry row is stale → exit-3.
	intentOld := &api.SupervisorIntentFile{Version: 1, Daemons: []api.SupervisorDaemon{lspDescWithPort(oldPort)}}
	err := classifyLSPPortMismatch(intentOld, `C:\ws`, "go", oldPort, newPort, "go-abc")
	if got := exitCodeOf(t, err); got != exitBindRefused {
		t.Fatalf("crash window (registry=new, intent=argv=old): exit = %d, want %d (self-heal, NOT exit-1-brick)", got, exitBindRefused)
	}

	// (b) fail-after-publish INVERSION: intent published the new port (argv --port =
	// new) but compensation reverted the registry row to the old port. Intent AGREES
	// with argv; only the registry disagrees → still a stale registry row → exit-3.
	intentNew := &api.SupervisorIntentFile{Version: 1, Daemons: []api.SupervisorDaemon{lspDescWithPort(newPort)}}
	err = classifyLSPPortMismatch(intentNew, `C:\ws`, "go", newPort, oldPort, "go-abc")
	if got := exitCodeOf(t, err); got != exitBindRefused {
		t.Fatalf("inversion (intent=argv=new, registry=old): exit = %d, want %d (self-heal)", got, exitBindRefused)
	}
}

func TestClassifyLSPPortMismatch_GenuineMisregistrationExit1(t *testing.T) {
	const argvPort, intentPort, registryPort = 9500, 9401, 9401

	// Our --port (9500) is STALE relative to intent (which says 9401) — a genuine
	// mis-registration (stale scheduler task XML), NOT a stale registry row. Keep the
	// original fail-closed exit-1 so it is never swept into a self-heal loop.
	intent := &api.SupervisorIntentFile{Version: 1, Daemons: []api.SupervisorDaemon{lspDescWithPort(intentPort)}}
	if err := classifyLSPPortMismatch(intent, `C:\ws`, "go", argvPort, registryPort, "go-abc"); isForceExitErr(err) {
		t.Fatalf("stale-argv mis-registration must be a plain exit-1 error, got a self-heal exit: %v", err)
	}

	// Intent has NO matching descriptor → exit-1 (not supervisor-managed by intent).
	if err := classifyLSPPortMismatch(&api.SupervisorIntentFile{Version: 1}, `C:\ws`, "go", 9401, 9450, "go-abc"); isForceExitErr(err) {
		t.Fatalf("intent without the descriptor must be exit-1, got a self-heal exit: %v", err)
	}

	// nil intent (read failed) → fail-closed exit-1.
	if err := classifyLSPPortMismatch(nil, `C:\ws`, "go", 9401, 9450, "go-abc"); isForceExitErr(err) {
		t.Fatalf("nil intent (read failed) must be exit-1, got a self-heal exit: %v", err)
	}

	// Wrong workspace/language → no match → exit-1 (a different proxy's row must not
	// be treated as ours).
	if err := classifyLSPPortMismatch(intent, `C:\other`, "go", 9401, 9450, "go-abc"); isForceExitErr(err) {
		t.Fatalf("workspace mismatch must be exit-1, got a self-heal exit: %v", err)
	}
}

// --- Blind spot 4: a stale-generation bind-refused exit-3 is dropped by the
// generation guard BEFORE the self-heal; it must NOT record a reallocation.

func TestRealloc_StaleGeneration_Exit3_NotSelfHealed(t *testing.T) {
	d := serenaProxyDescriptor()
	ctrl, eventsPath := reallocSyncController(t, d)
	task := canonicalSupervisorTaskName(d.TaskName)

	// Two spawns → current generation 2. A bind-refused exit-3 carrying generation 1
	// is a late exit of a superseded child.
	ctrl.tracker.MarkSpawned(task, 1000, time.Now().UTC())
	ctrl.tracker.MarkSpawned(task, 1001, time.Now().UTC())
	ctrl.smStates.Store(task, api.StRunning)
	ctrl.handleLoopEvent(api.LoopEvent{
		Kind:     api.EvChildExit,
		TaskName: d.TaskName,
		Body:     map[string]any{"exit_code": exitBindRefused, "pid_generation": 1, "pid": 1000},
	})

	if got := reallocCount(ctrl, task); got != 0 {
		t.Fatalf("stale-generation exit-3 recorded a reallocation (%d), want 0 (dropped by the generation guard before the self-heal)", got)
	}
	assertEventInLog(t, eventsPath, "daemon-stale-exit-ignored")
	assertEventNotInLog(t, eventsPath, "daemon-bind-access-denied")
}

// --- Blind spot 5: a full reallocation channel drops the dispatch (returns false,
// clears the in-flight marker, emits realloc-dispatch-dropped) so the caller's
// armed fallback timer re-drives instead of stranding.

func TestRealloc_DispatchDrop_FullChannel(t *testing.T) {
	tmp := apitest.HardenedTempDir(t)
	eventsPath := filepath.Join(tmp, "supervisor-events.log")
	events, err := api.OpenSupervisorEventLog(eventsPath)
	if err != nil {
		t.Fatalf("open event log: %v", err)
	}
	t.Cleanup(func() { _ = events.Close() })

	ctrl := &supervisorController{
		events:    events,
		reallocCh: make(chan reallocReq, 1), // tiny + undrained so #2 finds it full
	}

	d1 := lspWorkspaceProxyDescriptor()
	if !ctrl.tryDispatchRealloc(reallocReq{d: d1, attempt: 1}) {
		t.Fatalf("first dispatch (empty channel) should be delivered")
	}

	// A DIFFERENT task (so the per-task in-flight dedupe does not short-circuit)
	// finds the one-slot channel full → dropped.
	d2 := lspWorkspaceProxyDescriptor()
	d2.TaskName = `\mcp-local-hub-mcp-language-server-go-xyz`
	if ctrl.tryDispatchRealloc(reallocReq{d: d2, attempt: 1}) {
		t.Fatalf("dispatch into a full channel should be dropped (return false)")
	}
	assertEventInLog(t, eventsPath, "realloc-dispatch-dropped")
	if _, loaded := ctrl.reallocInFlight.Load(canonicalSupervisorTaskName(d2.TaskName)); loaded {
		t.Fatalf("reallocInFlight must be cleared after a dropped dispatch so a later retry can re-dispatch")
	}
}

// --- Blind spot 7: a malformed / absent outcome body decodes to reallocOutcomeFailed
// (the deliberate zero value) → the daemon stays HELD in backoff (re-armed fallback),
// never mistaken for a phantom "reallocated" respawn nor a pool-exhausted quarantine.

func TestRealloc_MalformedOutcomeBody_DecodesFailed(t *testing.T) {
	d := lspWorkspaceProxyDescriptor()
	ctrl, eventsPath := reallocSyncController(t, d)
	task := canonicalSupervisorTaskName(d.TaskName)

	ctrl.smStates.Store(task, api.StBackoffWaiting) // parked before the worker outcome
	// The outcome key carries the WRONG type: the .(reallocOutcome) assertion fails →
	// zero value = reallocOutcomeFailed.
	ctrl.handleReallocApplied(api.LoopEvent{
		Kind:     evReallocApplied,
		TaskName: d.TaskName,
		Body:     map[string]any{reallocResultOutcomeBodyKey: "garbage-not-an-outcome"},
	})

	if st, _ := ctrl.getSMStateCanonical(task); st != api.StBackoffWaiting {
		t.Fatalf("malformed outcome body: state = %v, want StBackoffWaiting (decoded to Failed → held, re-armed fallback)", st)
	}
	if got := crashCount(ctrl, task); got != 0 {
		t.Fatalf("malformed outcome body must not crash-count, got %d", got)
	}
	assertEventNotInLog(t, eventsPath, "daemon-quarantined")
	// FIX-9.2 strengthening: a malformed body decodes to Failed, which records NO cap
	// slot (FIX-6 records only on Reallocated). If a regression inverted the enum so
	// the zero value decoded to Reallocated, the reallocated branch would record a
	// phantom reallocation here — assert 0 so that inversion FAILS this test rather
	// than passing on the state check alone (Failed and Reallocated both leave the
	// daemon StBackoffWaiting).
	if got := reallocCount(ctrl, task); got != 0 {
		t.Fatalf("malformed outcome body recorded a reallocation (%d), want 0 (zero value must decode to Failed, not Reallocated)", got)
	}
}

// serenaProxyDescriptorWithSpec returns a serena-proxy descriptor at `port` with its
// Port field, --port argv, and RuntimeSpec External/Upstream ALL consistent — so a
// reworked realloc test can assert the full port-move consistency (Sol P2: a bare
// dd.Port check hides a stale argv / RuntimeSpec).
func serenaProxyDescriptorWithSpec(port int) api.SupervisorDaemon {
	d := serenaProxyDescriptor()
	d.Port = port
	d.Args = append([]string(nil), d.Args...)
	for i := 0; i+1 < len(d.Args); i++ {
		if d.Args[i] == "--port" {
			d.Args[i+1] = strconv.Itoa(port)
		}
	}
	d.RuntimeSpec = &api.DaemonRuntimeSpec{
		SpecVersion:   api.DaemonRuntimeSpecVersion,
		ChildCommand:  "uvx",
		ChildArgs:     []string{"--project", `C:\proj`, "--context", "codex"},
		ExternalPort:  port,
		UpstreamPort:  port + config.NativeHTTPInternalPortOffset,
		WorkspacePath: `C:\proj`,
	}
	return d
}

// argPortOf extracts the value following the FIRST --port argv token (0 if absent).
func argPortOf(d api.SupervisorDaemon) int {
	for i := 0; i+1 < len(d.Args); i++ {
		if d.Args[i] == "--port" {
			p, _ := strconv.Atoi(d.Args[i+1])
			return p
		}
	}
	return 0
}

// assertReallocConsistent asserts a descriptor was moved to wantPort CONSISTENTLY: the
// Port field, the --port argv token, and (for a serena proxy carrying a RuntimeSpec) the
// RuntimeSpec External/Upstream ports all agree — so a respawn can never bind a stale
// argv or a stale RuntimeSpec (Sol P2). Asserting only dd.Port would let a
// port-moved-but-argv-stale descriptor pass.
func assertReallocConsistent(t *testing.T, d api.SupervisorDaemon, wantPort int) {
	t.Helper()
	if d.Port != wantPort {
		t.Fatalf("descriptor Port = %d, want %d", d.Port, wantPort)
	}
	if got := argPortOf(d); got != wantPort {
		t.Fatalf("descriptor --port argv = %d, want %d (stale argv → respawn binds the wrong port)", got, wantPort)
	}
	if d.RuntimeSpec != nil {
		if d.RuntimeSpec.ExternalPort != wantPort {
			t.Fatalf("RuntimeSpec.ExternalPort = %d, want %d", d.RuntimeSpec.ExternalPort, wantPort)
		}
		if want := wantPort + config.NativeHTTPInternalPortOffset; d.RuntimeSpec.UpstreamPort != want {
			t.Fatalf("RuntimeSpec.UpstreamPort = %d, want %d (External+offset invariant)", d.RuntimeSpec.UpstreamPort, want)
		}
	}
}

// TestRealloc_EqualTimestampSnapshot_TreatedStale (FIX-4b + common-path fix): a carried
// whole-intent snapshot whose UpdatedAt EQUALS the cache's is treated as STALE, so the
// WHOLE snapshot is NOT wholesale-applied (flock serializes writes but does not make
// wall-clock timestamps strictly ordered — an equal timestamp must never clobber a
// possibly-newer cache). BUT the realloc's port move is authoritative for THIS
// descriptor, so the stale branch STILL targeted-patches this descriptor to newPort.
// This is the COMMON production timeline: ReallocateDynamicPoolPort's step-4 intent
// write does not stamp UpdatedAt, so the worker's carried snapshot shares the cache's
// timestamp. Asserts the full consistency (Port + --port argv + serena RuntimeSpec) so a
// skipped snapshot can't appear correct while a real respawn uses stale argv (Sol P2).
func TestRealloc_EqualTimestampSnapshot_TreatedStale(t *testing.T) {
	base := serenaProxyDescriptorWithSpec(9151) // Port + argv + RuntimeSpec all consistent
	ctrl, eventsPath := reallocSyncController(t, base)
	task := canonicalSupervisorTaskName(base.TaskName)

	const ts = "2026-07-13T10:00:00Z"
	const newPort = 9175 // the realloc's authoritative move (worker returned this)
	ctrl.intentCache.Refresh(&api.SupervisorIntentFile{Version: 1, UpdatedAt: ts, Daemons: []api.SupervisorDaemon{base}})

	// The worker's carried whole-intent snapshot shares the cache UpdatedAt (EQUAL — the
	// common timeline) and names a DIFFERENT, WRONG port we must NOT wholesale-apply.
	wrongSnap, ok := api.CloneIntentWithReallocatedPort(
		&api.SupervisorIntentFile{Version: 1, UpdatedAt: ts, Daemons: []api.SupervisorDaemon{base}}, base.TaskName, 1111)
	if !ok || wrongSnap == nil {
		t.Fatalf("build wrong snapshot fixture")
	}

	ctrl.smStates.Store(task, api.StBackoffWaiting)
	ctrl.handleReallocApplied(api.LoopEvent{
		Kind:     evReallocApplied,
		TaskName: base.TaskName,
		Body: map[string]any{
			reallocResultOutcomeBodyKey: reallocOutcomeReallocated,
			reallocResultNewPortBodyKey: newPort,
			reallocResultIntentBodyKey:  wrongSnap,
		},
	})

	// port 1111 would appear only if the whole snapshot were wrongly applied; 9151 only
	// if the stale branch skipped the patch (the pre-fix regression). The correct result
	// is the authoritative newPort, moved consistently across Port + argv + RuntimeSpec.
	dd, ok := ctrl.intentCache.Lookup(base.TaskName)
	if !ok || dd == nil {
		t.Fatalf("descriptor missing after equal-timestamp apply")
	}
	assertReallocConsistent(t, *dd, newPort)
	assertEventInLog(t, eventsPath, "realloc-stale-snapshot-skipped")
}

// TestRealloc_ParseFailSnapshot_TargetedPatch (FIX-4b): a carried snapshot with an
// UNPARSEABLE UpdatedAt cannot be chronologically ordered, so FIX-4b no longer
// fail-opens into a blind whole-snapshot apply (which would trust the snapshot's
// other contents). The loop instead applies the FIX-3b targeted port patch from the
// worker's newPort — the descriptor moves to newPort, NOT the untrusted snapshot's
// port, NOT the old port.
func TestRealloc_ParseFailSnapshot_TargetedPatch(t *testing.T) {
	d := lspWorkspaceProxyDescriptor() // oldPort 9401, has --port argv
	ctrl, _ := reallocSyncController(t, d)
	task := canonicalSupervisorTaskName(d.TaskName)

	const newPort = 9460
	// The cache currently holds a valid (parseable) intent at oldPort.
	ctrl.intentCache.Refresh(&api.SupervisorIntentFile{Version: 1, UpdatedAt: "2026-07-13T10:00:00Z", Daemons: []api.SupervisorDaemon{d}})

	// Carried snapshot has a GARBAGE UpdatedAt (unparseable) and a DIFFERENT, wrong
	// port (7777) we must NOT trust wholesale.
	badDesc := d
	badDesc.Port = 7777
	bad := &api.SupervisorIntentFile{Version: 1, UpdatedAt: "not-a-timestamp", Daemons: []api.SupervisorDaemon{badDesc}}

	ctrl.smStates.Store(task, api.StBackoffWaiting)
	ctrl.handleReallocApplied(api.LoopEvent{
		Kind:     evReallocApplied,
		TaskName: d.TaskName,
		Body: map[string]any{
			reallocResultOutcomeBodyKey: reallocOutcomeReallocated,
			reallocResultNewPortBodyKey: newPort,
			reallocResultIntentBodyKey:  bad,
		},
	})

	if dd, ok := ctrl.intentCache.Lookup(d.TaskName); !ok || dd == nil {
		t.Fatalf("descriptor missing after parse-fail apply")
	} else if dd.Port != newPort {
		t.Fatalf("parse-fail: descriptor port = %d, want the targeted newPort %d (NOT the untrusted snapshot's 7777, NOT the old 9401)", dd.Port, newPort)
	}
}

// --- FIX-4: the stale-snapshot guard rejects a worker snapshot older than the intent
// already applied to the cache (an operator/reconciler update the IntentWatcher
// swapped in during the worker's off-loop window), so the newer intent is not
// clobbered.

func TestRealloc_StaleSnapshot_NotClobbered(t *testing.T) {
	d := lspWorkspaceProxyDescriptor()
	ctrl, eventsPath := reallocSyncController(t, d)
	task := canonicalSupervisorTaskName(d.TaskName)

	// The cache already holds a NEWER intent (UpdatedAt T2) — an operator update the
	// 60s IntentWatcher swapped in. Its descriptor carries the reallocated port 9999.
	newerDesc := d
	newerDesc.Port = 9999
	newer := &api.SupervisorIntentFile{
		Version:   1,
		UpdatedAt: "2026-07-13T10:00:02Z",
		Daemons:   []api.SupervisorDaemon{newerDesc},
	}
	ctrl.intentCache.Refresh(newer)

	// The worker's evReallocApplied carries an OLDER snapshot (UpdatedAt T1, port 1111).
	olderDesc := d
	olderDesc.Port = 1111
	older := &api.SupervisorIntentFile{
		Version:   1,
		UpdatedAt: "2026-07-13T10:00:01Z",
		Daemons:   []api.SupervisorDaemon{olderDesc},
	}
	ctrl.smStates.Store(task, api.StBackoffWaiting)
	ctrl.handleReallocApplied(api.LoopEvent{
		Kind:     evReallocApplied,
		TaskName: d.TaskName,
		Body: map[string]any{
			reallocResultOutcomeBodyKey: reallocOutcomeReallocated,
			reallocResultIntentBodyKey:  older,
		},
	})

	if got := ctrl.intentCache.CurrentIntent(); got == nil || got.UpdatedAt != "2026-07-13T10:00:02Z" {
		t.Fatalf("stale snapshot clobbered the newer cache: CurrentIntent = %+v, want UpdatedAt 10:00:02Z", got)
	}
	if dd, ok := ctrl.intentCache.Lookup(d.TaskName); !ok || dd == nil {
		t.Fatalf("descriptor missing after the stale-snapshot apply")
	} else if dd.Port != 9999 {
		t.Fatalf("descriptor port = %d after stale apply, want the newer 9999 (stale 1111 must be rejected)", dd.Port)
	}
	assertEventInLog(t, eventsPath, "realloc-stale-snapshot-skipped")
}
