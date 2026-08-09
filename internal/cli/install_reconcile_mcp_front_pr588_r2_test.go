// internal/cli/install_reconcile_mcp_front_pr588_r2_test.go
//
// Round-2 regression coverage for `mcphub install --reconcile-mcp-front`:
// the codex bot PR #588 findings about the forward pass acting on state it
// never established, plus the cross-family findings about the recovery
// record's trustworthiness.
//
// SAFETY: identical posture to the other files in this family — HOME /
// USERPROFILE / LOCALAPPDATA and the daemon state dir are redirected to a
// throwaway t.TempDir(), and the report path seam keeps record persistence
// inside it. Nothing here can reach the operator's real client configs or
// state dir.
package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mcp-local-hub/internal/api"

	"github.com/gofrs/flock"
)

// startFirstLSPOnlyRouteServer serves a route daemon that answers /serena/mcp
// and the manifest's first LSP route, but has no later language routes.
//
// This is not a contrived shape: internal/cli/route.go's buildRouteServer
// wires the two routers INDEPENDENTLY, and the LSP one is only wired when the
// mcp-language-server manifest both loads and parses. Either failure is logged
// to stderr and the daemon keeps serving — producing exactly this process.
func startFirstLSPOnlyRouteServer(t *testing.T) (port int, cleanup func()) {
	port, _, cleanup = startMCPFrontReadinessServerWithFilter(t, int(^uint(0)>>1), "/lsp/clangd/mcp")
	return port, cleanup
}

func TestMCPFrontGuardPinPreparationZeroesBuffer(t *testing.T) {
	raw := []byte("token-bearing backup bytes")
	writeErr := errors.New("injected pin write failure")
	journal := &mcpFrontReconcileJournal{
		reportPath: t.TempDir(),
		readClientConfigBackup: func(context.Context, string, []string, string) ([]byte, error) {
			return raw, nil
		},
		writeClientConfigPin: func(string, []byte) error { return writeErr },
	}
	_, err := journal.pinBackup(context.Background(), "claude-code", filepath.Join(t.TempDir(), "claude-code.backup"))
	if !errors.Is(err, writeErr) {
		t.Fatalf("pin write failure lost its cause: %v", err)
	}
	for i, got := range raw {
		if got != 0 {
			t.Fatalf("backup byte %d=%d after failed pin write, want zero", i, got)
		}
	}
}

// startMCPFrontReadinessServer serves a real loopback HTTP listener whose
// gopls-mcp route has a controllable number of successful requests while every
// sibling stays live. A finite value models only that backend route dropping
// between preflight and final verification.
func startMCPFrontReadinessServer(t *testing.T, lspLiveRequests int) (port int, lspRequests *atomic.Int32, cleanup func()) {
	return startMCPFrontReadinessServerWithFilter(t, lspLiveRequests, "")
}

func startMCPFrontReadinessServerWithFilter(t *testing.T, lspLiveRequests int, onlyLSPPath string) (port int, lspRequests *atomic.Int32, cleanup func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc(api.SerenaRouterURLPath, func(w http.ResponseWriter, r *http.Request) {
		serveMCPFrontReadinessResponse(w, r)
	})
	lspRequests = new(atomic.Int32)
	var perPath sync.Map
	mux.HandleFunc("/lsp/", func(w http.ResponseWriter, r *http.Request) {
		lspRequests.Add(1)
		counterAny, _ := perPath.LoadOrStore(r.URL.Path, new(atomic.Int32))
		pathRequests := counterAny.(*atomic.Int32).Add(1)
		limitedPath := onlyLSPPath
		if limitedPath == "" {
			limitedPath = "/lsp/go/mcp"
		}
		if (onlyLSPPath != "" && r.URL.Path != onlyLSPPath) || (r.URL.Path == limitedPath && int(pathRequests) > lspLiveRequests) {
			http.NotFound(w, r)
			return
		}
		serveMCPFrontReadinessResponse(w, r)
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	return ln.Addr().(*net.TCPAddr).Port, lspRequests, func() { _ = srv.Close() }
}

func serveMCPFrontReadinessResponse(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodDelete:
		w.WriteHeader(http.StatusNoContent)
		return
	case http.MethodPost:
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Mcp-Session-Id", "mcp-front-readiness-test-session")
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2024-11-05","capabilities":{},"serverInfo":{"name":"mcp-front-readiness","version":"test"}}}`)
		return
	default:
		w.Header().Set("Allow", "POST, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
}

func TestMCPFrontR2_ZeroRowGenerationRoundTripsAndRetriesCleanly(t *testing.T) {
	tmp := mcpFrontPR588Env(t)
	assertRedirectedStateDir(t, tmp)
	reportPath := withMCPFrontReportPathSeam(t)

	port, cleanup := startTestRouteServer(t)
	defer cleanup()
	seedSupervisorOwnedRoutePort(t, port)

	a := api.NewAPI()
	if err := a.SettingsSet(api.MCPFrontPortSettingKey, strconv.Itoa(port)); err != nil {
		t.Fatalf("SettingsSet: %v", err)
	}

	for cycle := 1; cycle <= 2; cycle++ {
		if err := runReconcileMCPFront(newMCPFrontTestCmd(), false); err != nil {
			t.Fatalf("cycle %d empty forward: %v", cycle, err)
		}
		durable, err := readMCPFrontReconcileReport(reportPath)
		if err != nil {
			t.Fatalf("cycle %d read empty journal: %v", cycle, err)
		}
		if durable == nil || len(durable.Rows) != 0 || durable.ActivePlan == nil || len(durable.ActivePlan.Rows) != 0 || len(durable.ActivePlan.Operations) != 0 {
			t.Fatalf("cycle %d journal is not an explicit zero-row generation: %+v", cycle, durable)
		}
		state, err := a.MCPFrontRoutingTargetSnapshot()
		if err != nil || state.State != api.MCPFrontRoutingTargetFront || state.Generation != 1 {
			t.Fatalf("cycle %d forward routing state=%+v err=%v, want stable front generation 1", cycle, state, err)
		}

		clientRows := 0
		ops := newMCPFrontRollbackOps(a)
		restoreSerena := ops.restoreSerena
		restoreLSP := ops.restoreLSP
		ops.restoreSerena = func(requests []api.SerenaOwnedRestoreRequest) ([]api.SerenaOwnedRestoreResult, error) {
			clientRows += len(requests)
			return restoreSerena(requests)
		}
		ops.restoreLSP = func(rows []api.LSPRouterRecoveryRow, opts api.LSPClientRouterOpts, callbacks api.LSPRouterRestoreCallbacks) (*api.LSPClientRouterReport, []api.LSPRouterRestoreRowResult, error) {
			clientRows += len(rows)
			return restoreLSP(rows, opts, callbacks)
		}
		if err := runRollbackMCPFrontWithOps(newMCPFrontTestCmd(), reportPath, ops); err != nil {
			t.Fatalf("cycle %d empty rollback: %v", cycle, err)
		}
		if clientRows != 0 {
			t.Fatalf("cycle %d empty rollback admitted %d client mutation rows", cycle, clientRows)
		}
		state, err = a.MCPFrontRoutingTargetSnapshot()
		if err != nil || state.State != api.MCPFrontRoutingTargetGUI || state.Generation != 0 {
			t.Fatalf("cycle %d rollback routing state=%+v err=%v, want stable gui", cycle, state, err)
		}
		if _, err := os.Stat(reportPath); !os.IsNotExist(err) {
			t.Fatalf("cycle %d active empty journal was not retired: %v", cycle, err)
		}
	}
}

// TestMCPFrontR2_ForwardRefusesWhenOnlyTheSerenaRouteIsLive is the P1 guard
// for "the forward pass rewrites LSP clients without ever probing an LSP
// route".
//
// Before the fix the serena reconcile's own internal liveness probe was
// treated as the whole command's gate, so a route daemon serving only
// /serena/mcp satisfied it and the LSP leg then rewrote every LSP client onto
// a route that answers nothing.
func TestMCPFrontR2_ForwardRefusesWhenALaterLSPRouteIsBroken(t *testing.T) {
	redirectMCPFrontTestEnv(t)
	reportPath := withMCPFrontReportPathSeam(t)

	port, cleanup := startFirstLSPOnlyRouteServer(t)
	defer cleanup()
	seedSupervisorOwnedRoutePort(t, port)

	a := api.NewAPI()
	if err := a.SettingsSet(api.MCPFrontPortSettingKey, strconv.Itoa(port)); err != nil {
		t.Fatalf("SettingsSet: %v", err)
	}

	// Precondition: the serena route really IS live, so this test is failing
	// for the LSP reason and not because nothing was listening at all.
	if serr := api.AssertSerenaRouterRouteLive(context.Background(), port); serr != nil {
		t.Fatalf("test precondition broken: the serena route must be live for this test to isolate the LSP gap: %v", serr)
	}

	err := runReconcileMCPFront(newMCPFrontTestCmd(), false)
	if err == nil {
		t.Fatalf("forward reconcile must REFUSE when only the first LSP route is live: it is about to point later clients at broken /lsp/<language>/mcp routes")
	}
	if !strings.Contains(err.Error(), "lsp") && !strings.Contains(err.Error(), "LSP") {
		t.Fatalf("the refusal must name the LSP route as the cause, so the operator knows what to fix; got: %v", err)
	}
	var readinessErr *api.MCPFrontRoutesLiveError
	if !errors.As(err, &readinessErr) || readinessErr.Stage != api.MCPFrontRouteStageLSP {
		t.Fatalf("the preflight refusal must preserve a typed LSP readiness stage; err=%T %v", err, err)
	}
	if readinessErr.Code != api.MCPFrontRouteNotReadyCode || readinessErr.Language != "fortran" || readinessErr.Backend != "mcp-language-server" || readinessErr.ProbeStage != api.MCPFrontProbeStageShapeResponse {
		t.Fatalf("preflight must identify the first broken later manifest route and exact substage; got %+v", readinessErr)
	}
	if _, statErr := os.Stat(reportPath); !os.IsNotExist(statErr) {
		t.Fatalf("a refused pre-flight must leave ZERO side effects, including no recovery record; stat err = %v", statErr)
	}
}

// TestMCPFrontR2_ForwardFinalVerificationRefusesLostLSPRoute proves readiness
// is re-established after client writes, not only before the recovery journal
// is created. The server permits exactly preflight's HEAD+initialize LSP pair,
// then drops the LSP route; no timing or host scheduling is involved.
func TestMCPFrontR2_ForwardFinalVerificationRefusesLostLSPRoute(t *testing.T) {
	tmp := redirectMCPFrontTestEnv(t)
	reportPath := withMCPFrontReportPathSeam(t)

	// One complete staged pre-write lifecycle probe is mandatory now
	// (HEAD+initialize+DELETE). Drop the route only for the final post-write
	// verification.
	port, lspRequests, cleanup := startMCPFrontReadinessServer(t, 3)
	defer cleanup()
	seedSupervisorOwnedRoutePort(t, port)

	a := api.NewAPI()
	if err := a.SettingsSet(api.MCPFrontPortSettingKey, strconv.Itoa(port)); err != nil {
		t.Fatalf("SettingsSet: %v", err)
	}
	configPath := seedClaudeCodeConfig(t, tmp, map[string]any{
		"serena": map[string]any{"url": "http://127.0.0.1:9125/serena/mcp"},
	})

	err := runReconcileMCPFront(newMCPFrontTestCmd(), false)
	if err == nil {
		t.Fatal("forward reconcile reported success after the LSP route dropped during its client writes")
	}
	var readinessErr *api.MCPFrontRoutesLiveError
	if !errors.As(err, &readinessErr) || readinessErr.Stage != api.MCPFrontRouteStageLSP {
		t.Fatalf("final verification must return the typed LSP readiness failure; err=%T %v", err, err)
	}
	if readinessErr.Code != api.MCPFrontRouteNotReadyCode || readinessErr.Language != "go" || readinessErr.Backend != "gopls-mcp" || readinessErr.ProbeStage != api.MCPFrontProbeStageShapeResponse {
		t.Fatalf("final verification must identify the dropped gopls backend route and exact substage; got %+v", readinessErr)
	}
	if got := lspRequests.Load(); got <= 3 {
		t.Fatalf("LSP requests=%d, want more than one route's preflight pair so final all-route verification is proven to have run", got)
	}
	if got, ok := claudeCodeEntryURL(t, configPath, "serena"); !ok || got != api.SerenaRouterClientURL(port) {
		t.Fatalf("the final verification test must reach the post-write stage; serena URL=%q present=%v, want %q", got, ok, api.SerenaRouterClientURL(port))
	}
	if _, statErr := os.Stat(reportPath); statErr != nil {
		t.Fatalf("post-write readiness refusal must retain the durable recovery journal for retry or rollback: %v", statErr)
	}
	if state, stateErr := api.NewAPI().MCPFrontRoutingTargetSnapshot(); stateErr != nil || state.State != api.MCPFrontRoutingTargetFrontPreparing || state.Generation != 1 {
		t.Fatalf("post-write failure routing state=%+v err=%v, want front-preparing generation 1", state, stateErr)
	}
}

// TestMCPFrontR2_CheckWithReconcileMutatesNothing is the P1 guard for
// `--check` falling through to the real, mutating reconcile.
//
// The assertion is deliberately NOT "an error was returned" — the finding is
// about MUTATION, and a test that only checked the error could pass against a
// build that errored after writing. It seeds a real client config, runs the
// combination, and proves the bytes on disk are untouched.
func TestMCPFrontR2_CheckWithReconcileMutatesNothing(t *testing.T) {
	tmp := mcpFrontPR588Env(t)
	assertRedirectedStateDir(t, tmp)
	reportPath := withMCPFrontReportPathSeam(t)

	port, cleanup := startTestRouteServer(t)
	defer cleanup()
	seedSupervisorOwnedRoutePort(t, port)

	a := api.NewAPI()
	if err := a.SettingsSet(api.MCPFrontPortSettingKey, strconv.Itoa(port)); err != nil {
		t.Fatalf("SettingsSet: %v", err)
	}
	configPath := seedClaudeCodeConfig(t, tmp, map[string]any{
		"serena": map[string]any{"url": "http://127.0.0.1:9125/serena/mcp"},
	})
	before, rerr := os.ReadFile(configPath)
	if rerr != nil {
		t.Fatalf("read seeded config: %v", rerr)
	}

	c := newInstallCmdReal()
	c.SetArgs([]string{"--check", "--reconcile-mcp-front"})
	c.SetOut(new(testWriter))
	c.SetErr(new(testWriter))
	err := c.Execute()

	after, rerr2 := os.ReadFile(configPath)
	if rerr2 != nil {
		t.Fatalf("read config after: %v", rerr2)
	}
	if string(before) != string(after) {
		t.Fatalf("--check promises a read-only readiness report and MUTATED the client config instead.\nbefore: %s\nafter:  %s", string(before), string(after))
	}
	if _, statErr := os.Stat(reportPath); !os.IsNotExist(statErr) {
		t.Fatalf("--check wrote a reconcile recovery record, so it ran the real forward reconcile; stat err = %v", statErr)
	}
	if err == nil {
		t.Fatalf("--check combined with a mutating mode must be rejected explicitly, not silently ignored")
	}
}

// TestMCPFrontR2_RerunAtANewPortRecordsTheLatestPort guards the explicit N+1
// admission path after a terminal predecessor. The rollback judges each row
// against its exact receipt, while the active plan names the newly admitted port.
func TestMCPFrontR2_RerunAtANewPortRecordsTheLatestPort(t *testing.T) {
	tmp := mcpFrontPR588Env(t)
	assertRedirectedStateDir(t, tmp)
	reportPath := withMCPFrontReportPathSeam(t)
	seedClaudeCodeConfig(t, tmp, map[string]any{
		"serena": map[string]any{"url": "http://127.0.0.1:9125/serena/mcp"},
	})

	portA, cleanupA := startTestRouteServer(t)
	seedSupervisorOwnedRoutePort(t, portA)
	a := api.NewAPI()
	if err := a.SettingsSet(api.MCPFrontPortSettingKey, strconv.Itoa(portA)); err != nil {
		t.Fatalf("SettingsSet A: %v", err)
	}
	if err := runReconcileMCPFront(newMCPFrontTestCmd(), false); err != nil {
		t.Fatalf("forward reconcile at port A: %v", err)
	}
	if state, stateErr := a.MCPFrontRoutingTargetSnapshot(); stateErr != nil || state.State != api.MCPFrontRoutingTargetFront || state.Generation != 1 {
		t.Fatalf("successful forward routing state=%+v err=%v, want front generation 1", state, stateErr)
	}
	if got := readPersistedMCPFrontReport(t, reportPath).ActivePlan.Port; got != portA {
		t.Fatalf("generation 1 recorded port %d, want %d", got, portA)
	}
	cleanupA()

	// The operator changes mcp_front.port and the supervisor moves the route
	// daemon, then re-runs the command (the documented retry). Re-seeding takes
	// the supervisor flock again, so the first seed's hold must be dropped —
	// which is also what really happens here: the supervisor restarts onto the
	// new port.
	releaseSeededSupervisorLock(t)
	portB, cleanupB := startTestRouteServer(t)
	defer cleanupB()
	seedSupervisorOwnedRoutePort(t, portB)
	if err := a.SettingsSet(api.MCPFrontPortSettingKey, strconv.Itoa(portB)); err != nil {
		t.Fatalf("SettingsSet B: %v", err)
	}
	if err := runReconcileMCPFront(newMCPFrontTestCmd(), false); err != nil {
		t.Fatalf("terminal generation must admit an exact N+1 port move: %v", err)
	}

	rec := readPersistedMCPFrontReport(t, reportPath)
	if rec.Version != mcpFrontReconcileReportVersion || rec.Generation != 2 || rec.ActivePlan.Generation != 2 || rec.ActivePlan.Port != portB || !rec.Settled || rec.GenerationAdmission != nil {
		t.Fatalf("admitted generation is incomplete: %+v", rec)
	}
	stateDir, err := api.DaemonStateDir()
	if err != nil {
		t.Fatalf("state dir: %v", err)
	}
	intent, err := api.ReadSupervisorIntent(filepath.Join(stateDir, "supervisor-intent.json"))
	if err != nil {
		t.Fatalf("read staged route descriptor: %v", err)
	}
	wantTask := api.BuiltinRouteTaskName
	foundPort := 0
	for _, daemon := range intent.Daemons {
		if daemon.TaskName == wantTask {
			foundPort = daemon.Port
			break
		}
	}
	if foundPort != portB {
		t.Fatalf("supervisor route descriptor port=%d, want admitted port B=%d", foundPort, portB)
	}
	// The pre-state must NOT have moved with it: opposite polarity, same record.
	serenaKey := mcpFrontReconcileRowKey(mcpFrontSurfaceSerena, "claude-code", "", "serena")
	if rec.Rows[serenaKey].Pin == nil {
		t.Fatalf("expected the first generation's row-owned pin to survive the re-run; row=%+v", rec.Rows[serenaKey])
	}
}

func TestMCPFrontRollbackPartialLeavesGUIRestoring(t *testing.T) {
	tmp := mcpFrontPR588Env(t)
	assertRedirectedStateDir(t, tmp)
	reportPath := withMCPFrontReportPathSeam(t)
	configPath := seedClaudeCodeConfig(t, tmp, map[string]any{
		"serena": map[string]any{"url": "http://127.0.0.1:9125/serena/mcp"},
	})
	port, cleanup := startTestRouteServer(t)
	defer cleanup()
	seedSupervisorOwnedRoutePort(t, port)
	a := api.NewAPI()
	if err := a.SettingsSet(api.MCPFrontPortSettingKey, strconv.Itoa(port)); err != nil {
		t.Fatal(err)
	}
	if err := runReconcileMCPFront(newMCPFrontTestCmd(), false); err != nil {
		t.Fatalf("forward reconcile: %v", err)
	}
	if err := os.Remove(configPath); err != nil {
		t.Fatalf("make recorded client unavailable: %v", err)
	}
	err := runReconcileMCPFront(newMCPFrontTestCmd(), true)
	if err == nil || !strings.Contains(err.Error(), "recovery remains pending") {
		t.Fatalf("partial rollback err=%v, want durable pending recovery", err)
	}
	if state, stateErr := a.MCPFrontRoutingTargetSnapshot(); stateErr != nil || state.State != api.MCPFrontRoutingTargetGUIRestoring || state.Generation != 1 {
		t.Fatalf("partial rollback routing state=%+v err=%v, want gui-restoring generation 1", state, stateErr)
	}
	if _, statErr := os.Stat(reportPath); statErr != nil {
		t.Fatalf("partial rollback retired its recovery journal: %v", statErr)
	}
}

// TestMCPFrontR2_RollbackRefusesAnOperatorEditedSerenaEntry is the P2 guard
// for the rollback silently overwriting an entry the operator changed between
// the forward run and the rollback.
//
// VERSION-3 CONTRACT (the name predates it and is kept so the guard stays
// traceable; "refuses" now means "refuses to WRITE this row", not "refuses the
// whole command"). The version-2 rollback was all-or-nothing: one conflicting
// entry aborted the run and the record was kept for a retry. Version 3 makes
// the inverse a per-row compare-and-swap (I7) and lets independent rows finish
// (Decision E), so the outcome here is:
//
//   - the conflicting serena row is NOT written — the operator's edit stands;
//   - its disposition is `skipped-conflict`, which is TERMINAL: there is
//     nothing left for a retry to do, because the hub will never overwrite that
//     edit on its own;
//   - every other row still restores, and because all rows are then terminal
//     the record is RETIRED (I10 permits retirement only on an all-terminal
//     durable re-read — so keeping it here would be the bug, not the safety
//     net);
//   - the command still exits non-zero and names the row, so the operator
//     learns which entry was left alone and why.
func TestMCPFrontR2_RollbackRefusesAnOperatorEditedSerenaEntry(t *testing.T) {
	tmp := mcpFrontPR588Env(t)
	assertRedirectedStateDir(t, tmp)
	reportPath := withMCPFrontReportPathSeam(t)

	port, cleanup := startTestRouteServer(t)
	defer cleanup()
	seedSupervisorOwnedRoutePort(t, port)

	a := api.NewAPI()
	if err := a.SettingsSet(api.MCPFrontPortSettingKey, strconv.Itoa(port)); err != nil {
		t.Fatalf("SettingsSet: %v", err)
	}
	configPath := seedClaudeCodeConfig(t, tmp, map[string]any{
		"serena": map[string]any{"url": "http://127.0.0.1:9125/serena/mcp"},
	})
	if err := runReconcileMCPFront(newMCPFrontTestCmd(), false); err != nil {
		t.Fatalf("forward reconcile: %v", err)
	}

	// The operator repoints serena at their own endpoint, long after the
	// cutover. This is THEIR content now.
	const operatorURL = "http://127.0.0.1:19999/serena/mcp"
	seedClaudeCodeConfig(t, tmp, map[string]any{
		"serena": map[string]any{"url": operatorURL},
	})

	err := runReconcileMCPFront(newMCPFrontTestCmd(), true)
	if err == nil {
		t.Fatalf("rollback must report a non-zero outcome when the serena entry no longer matches what the forward run wrote — restoring would discard the operator's edit, and skipping it silently would leave the operator believing the rollback was complete")
	}
	if !strings.Contains(err.Error(), "edited") {
		t.Fatalf("the message must tell the operator their entry was edited since the forward run, in those words — a bare row key makes them reverse-engineer the cause; got: %v", err)
	}
	serenaRowKey := mcpFrontReconcileRowKey(mcpFrontSurfaceSerena, "claude-code", "", "serena")
	if !strings.Contains(err.Error(), strings.ReplaceAll(serenaRowKey, "\x00", "/")) {
		t.Fatalf("the message must name the exact row that was left alone; got: %v", err)
	}
	if got, _ := claudeCodeEntryURL(t, configPath, "serena"); got != operatorURL {
		t.Fatalf("the conflicting row must be left EXACTLY as the operator set it: serena is now %q, want %q", got, operatorURL)
	}
	// `skipped-conflict` is terminal, so with no other row outstanding the
	// record has done its whole job and must NOT stay in the active namespace:
	// a surviving record is the generation a later forward run would merge into,
	// and its rollback would then restore a pre-state two cutovers old.
	if _, statErr := os.Stat(reportPath); !os.IsNotExist(statErr) {
		t.Fatalf("every row reached a terminal disposition, so the record must be retired out of the active namespace; stat err = %v", statErr)
	}
}

// TestMCPFrontR2_SecondInvocationRefusesWhileTheTransactionLockIsHeld is the
// P2 guard for two concurrent invocations interleaving on one recovery record.
func TestMCPFrontR2_SecondInvocationRefusesWhileTheTransactionLockIsHeld(t *testing.T) {
	redirectMCPFrontTestEnv(t)
	reportPath := withMCPFrontReportPathSeam(t)

	port, cleanup := startTestRouteServer(t)
	defer cleanup()
	seedSupervisorOwnedRoutePort(t, port)

	a := api.NewAPI()
	if err := a.SettingsSet(api.MCPFrontPortSettingKey, strconv.Itoa(port)); err != nil {
		t.Fatalf("SettingsSet: %v", err)
	}

	// Stand in for a concurrent invocation by holding the same flock it would.
	// gofrs/flock refuses a second handle's TryLock even within one process on
	// both Windows and Linux, so this is the real contention path.
	lockPath := filepath.Join(filepath.Dir(reportPath), mcpFrontReconcileLockLeaf+".lock")
	held := flock.New(lockPath)
	got, lerr := held.TryLock()
	if lerr != nil || !got {
		t.Fatalf("could not hold the transaction lock for the test: got=%v err=%v", got, lerr)
	}
	defer func() { _ = held.Unlock() }()

	err := runReconcileMCPFront(newMCPFrontTestCmd(), false)
	if err == nil {
		t.Fatalf("a second concurrent invocation must REFUSE while the recovery transaction is held; two interleaved runs can leave a client rewritten with its recovery row in a retired record")
	}
	if _, statErr := os.Stat(reportPath); !os.IsNotExist(statErr) {
		t.Fatalf("the refused invocation must not have written a record; stat err = %v", statErr)
	}
}

// TestMCPFrontR2_ForwardRefusesAReusedPIDImpostor is the P1 guard for the
// supervisor-ownership proof binding to a PID NUMBER rather than to a process.
//
// Scenario: the supervised route child exited, its PID was reused by a
// standalone `mcphub route` that bound the port, and the supervisor has not
// reconciled its state file yet. Every PID-NUMBER check still passes — the
// state file names that PID, the PID is alive, the kernel says it owns the
// socket, and its image is the mcphub binary (the impostor is the same
// binary, so image identity proves nothing here).
//
// Simulated exactly as it would appear on disk: the state file carries the
// start time of the process the supervisor ACTUALLY spawned, which is not the
// start time of the process now holding the port.
func TestMCPFrontR2_ForwardRefusesAReusedPIDImpostor(t *testing.T) {
	tmp := mcpFrontPR588Env(t)
	assertRedirectedStateDir(t, tmp)
	reportPath := withMCPFrontReportPathSeam(t)

	port, cleanup := startTestRouteServer(t)
	defer cleanup()
	seedSupervisorOwnedRoutePort(t, port)

	configPath := seedClaudeCodeConfig(t, tmp, map[string]any{
		"serena": map[string]any{"url": "http://127.0.0.1:9125/serena/mcp"},
	})
	before, _ := os.ReadFile(configPath)

	a := api.NewAPI()
	if err := a.SettingsSet(api.MCPFrontPortSettingKey, strconv.Itoa(port)); err != nil {
		t.Fatalf("SettingsSet: %v", err)
	}

	// Rewrite ONLY the recorded start time: the exited child started an hour
	// before the process that now holds its PID.
	stateDir, derr := api.DaemonStateDir()
	if derr != nil {
		t.Fatalf("state dir: %v", derr)
	}
	statePath := filepath.Join(stateDir, "supervisor-state.json")
	if werr := api.WriteSupervisorState(statePath, &api.SupervisorStateFile{
		Version: 1,
		Daemons: map[string]api.SupervisorDaemonState{
			api.BuiltinRouteTaskName: {
				State:         "running",
				CurrentPID:    os.Getpid(),
				PIDGeneration: 1,
				StartedAt:     time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano),
			},
		},
	}); werr != nil {
		t.Fatalf("re-seed supervisor-state: %v", werr)
	}

	err := runReconcileMCPFront(newMCPFrontTestCmd(), false)
	if err == nil {
		t.Fatalf("the ownership gate must REFUSE a port owner whose PID number matches but whose process generation does not — that is a recycled PID, i.e. an unsupervised listener nothing will restart")
	}
	after, _ := os.ReadFile(configPath)
	if string(before) != string(after) {
		t.Fatalf("a refused ownership gate must write NO client config.\nbefore: %s\nafter:  %s", string(before), string(after))
	}
	if _, statErr := os.Stat(reportPath); !os.IsNotExist(statErr) {
		t.Fatalf("a refused ownership gate must leave no recovery record; stat err = %v", statErr)
	}
}

// TestMCPFrontR2_PinIsReclaimedWhenItsRecordCannotBePublished is the P2 guard
// for a token-bearing pin surviving a refused generation.
//
// The write-ahead ordering publishes the pin BEFORE the record that references
// it, so a record-write failure correctly prevents the client mutation but
// leaves the pin on disk with nothing pointing at it. A pin is a byte copy of
// a WHOLE client config — it can carry header tokens and stdio `env` secrets —
// and the retry this failure invites would publish another one every time.
//
// The window is driven directly on the journal, which is the only way to make
// the PIN write succeed while the RECORD write fails: a directory occupying
// the record's path defeats the atomic publish while leaving the sibling pin
// directory perfectly writable.
func TestMCPFrontR2_PinIsReclaimedWhenItsRecordCannotBePublished(t *testing.T) {
	tmp := t.TempDir()
	reportPath := filepath.Join(tmp, "mcp-front-reconcile-serena-report.json")
	if err := os.Mkdir(reportPath, 0o700); err != nil {
		t.Fatalf("seed a directory at the report path: %v", err)
	}
	backupPath := filepath.Join(tmp, "claude-code.bak-mcp-local-hub-20260725-120000")
	if err := os.WriteFile(backupPath, []byte(`{"mcpServers":{"serena":{"url":"http://127.0.0.1:9125/serena/mcp"}}}`), 0o600); err != nil {
		t.Fatalf("seed backup: %v", err)
	}

	journal, journalErr := newMCPFrontV3Journal(reportPath, nil, mcpFrontReconcileReportVersion, 1, 9137, &api.LSPRouterClientPlan{Port: 9137})
	if journalErr != nil {
		t.Fatal(journalErr)
	}
	err := journal.prepareSerenaAttempt(api.SerenaReconcileAttemptResult{
		Client: "claude-code", BackupPath: backupPath,
		PreFingerprint: "pre", IntendedFingerprint: "post",
	})
	if err == nil {
		t.Fatalf("the journal must refuse when its recovery row cannot be made durable; it returned nil, which would let the client be mutated with no way back")
	}

	pinDir := mcpFrontReconcilePinDir(reportPath)
	var survivors []string
	_ = filepath.Walk(pinDir, func(path string, info os.FileInfo, werr error) error {
		if werr != nil || info == nil || info.IsDir() {
			return nil //nolint:nilerr // absent tree is the passing case
		}
		survivors = append(survivors, path)
		return nil
	})
	if len(survivors) > 0 {
		t.Fatalf("a pinned copy of the client config survived a generation that published NO record referencing it: %v. Nothing will ever reference or clean it, and it holds a verbatim copy of %s (which may contain tokens or env secrets)", survivors, backupPath)
	}
}
