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
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"mcp-local-hub/internal/api"

	"github.com/gofrs/flock"
)

// startSerenaOnlyRouteServer serves a route daemon that answers /serena/mcp
// perfectly and has NO /lsp/<language>/mcp mount.
//
// This is not a contrived shape: internal/cli/route.go's buildRouteServer
// wires the two routers INDEPENDENTLY, and the LSP one is only wired when the
// mcp-language-server manifest both loads and parses. Either failure is logged
// to stderr and the daemon keeps serving — producing exactly this process.
func startSerenaOnlyRouteServer(t *testing.T) (port int, cleanup func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc(api.SerenaRouterURLPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2024-11-05","capabilities":{},"serverInfo":{"name":"serena-router","version":"0"}}}`)
	})
	// Everything else — crucially every /lsp/<language>/mcp path — 404s, the
	// way an unmounted route does.
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	return ln.Addr().(*net.TCPAddr).Port, func() { _ = srv.Close() }
}

// TestMCPFrontR2_ForwardRefusesWhenOnlyTheSerenaRouteIsLive is the P1 guard
// for "the forward pass rewrites LSP clients without ever probing an LSP
// route".
//
// Before the fix the serena reconcile's own internal liveness probe was
// treated as the whole command's gate, so a route daemon serving only
// /serena/mcp satisfied it and the LSP leg then rewrote every LSP client onto
// a route that answers nothing.
func TestMCPFrontR2_ForwardRefusesWhenOnlyTheSerenaRouteIsLive(t *testing.T) {
	redirectMCPFrontTestEnv(t)
	reportPath := withMCPFrontReportPathSeam(t)

	port, cleanup := startSerenaOnlyRouteServer(t)
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
		t.Fatalf("forward reconcile must REFUSE when the LSP route is not live: it is about to point every LSP client at /lsp/<language>/mcp on this port, and nothing answers there")
	}
	if !strings.Contains(err.Error(), "lsp") && !strings.Contains(err.Error(), "LSP") {
		t.Fatalf("the refusal must name the LSP route as the cause, so the operator knows what to fix; got: %v", err)
	}
	if _, statErr := os.Stat(reportPath); !os.IsNotExist(statErr) {
		t.Fatalf("a refused pre-flight must leave ZERO side effects, including no recovery record; stat err = %v", statErr)
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

// TestMCPFrontR2_RerunAtANewPortRecordsTheLatestPort is the P2 guard for the
// record naming the FIRST generation's port after a re-run moved the clients
// to a different one. The rollback judges live entries against this value, so
// a stale one makes it skip the very entries the cutover created.
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
		t.Fatalf("forward reconcile at port B: %v", err)
	}

	rec := readPersistedMCPFrontReport(t, reportPath)
	if rec.ActivePlan.Port != portB {
		t.Fatalf("active plan port=%d, want latest %d (first %d)", rec.ActivePlan.Port, portB, portA)
	}
	// The pre-state must NOT have moved with it: opposite polarity, same record.
	serenaKey := mcpFrontReconcileRowKey(mcpFrontSurfaceSerena, "claude-code", "", "serena")
	if rec.Rows[serenaKey].Pin == nil {
		t.Fatalf("expected the first generation's row-owned pin to survive the re-run; row=%+v", rec.Rows[serenaKey])
	}
}

// TestMCPFrontR2_RollbackRefusesAnOperatorEditedSerenaEntry is the P2 guard
// for the rollback silently overwriting an entry the operator changed between
// the forward run and the rollback.
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
		t.Fatalf("rollback must REFUSE when the serena entry no longer matches what the forward run wrote — restoring would discard the operator's edit with no warning")
	}
	if !strings.Contains(err.Error(), "edited") {
		t.Fatalf("the refusal must tell the operator their entry was edited since the forward run; got: %v", err)
	}
	if got, _ := claudeCodeEntryURL(t, configPath, "serena"); got != operatorURL {
		t.Fatalf("a refused rollback must touch NO client: the operator's serena entry is now %q, want %q", got, operatorURL)
	}
	if _, statErr := os.Stat(reportPath); statErr != nil {
		t.Fatalf("a refused rollback must KEEP the record so the operator can retry after resolving the conflict: %v", statErr)
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

	journal, journalErr := newMCPFrontV3Journal(reportPath, nil, 9137, &api.LSPRouterClientPlan{Port: 9137})
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
