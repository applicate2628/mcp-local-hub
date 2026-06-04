package api

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mcp-local-hub/internal/api/apitest"
)

func TestEnsureLSPRegistered_ConcurrentFirstTouchWaitsForReadyPort(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	restoreState := SetDaemonStateRootForTest(apitest.HardenedTempDir(t))
	defer restoreState()

	var reconcileCalls atomic.Int32
	origReconcile := registerSupervisorReconcileFn
	registerSupervisorReconcileFn = func(ctx context.Context, apply bool) (ReconcileResponse, error) {
		if !apply {
			t.Fatalf("EnsureLSPRegistered called reconcile with apply=false")
		}
		reconcileCalls.Add(1)
		return ReconcileResponse{DryRun: false}, nil
	}
	defer func() { registerSupervisorReconcileFn = origReconcile }()

	readinessEntered := make(chan int, 1)
	releaseReadiness := make(chan struct{})
	origReadiness := proxyReadinessFn
	var readinessCalls atomic.Int32
	proxyReadinessFn = func(port int, timeout time.Duration) error {
		readinessCalls.Add(1)
		readinessEntered <- port
		<-releaseReadiness
		return nil
	}
	defer func() { proxyReadinessFn = origReadiness }()

	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "pyproject.toml"), []byte("[project]\n"), 0o600); err != nil {
		t.Fatalf("touch marker: %v", err)
	}
	canonical, err := CanonicalWorkspacePath(ws)
	if err != nil {
		t.Fatalf("CanonicalWorkspacePath: %v", err)
	}
	wsKey := WorkspaceKey(canonical)

	type result struct {
		entry WorkspaceEntry
		err   error
	}
	results := make(chan result, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			entry, err := NewAPI().EnsureLSPRegistered(context.Background(), wsKey, canonical, "python")
			results <- result{entry: entry, err: err}
		}()
	}
	close(start)

	var readyPort int
	select {
	case readyPort = <-readinessEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("first registration never reached proxy readiness")
	}

	select {
	case r := <-results:
		t.Fatalf("EnsureLSPRegistered returned before readiness completed: entry=%+v err=%v", r.entry, r.err)
	case <-time.After(100 * time.Millisecond):
	}

	close(releaseReadiness)
	wg.Wait()
	close(results)

	var got []WorkspaceEntry
	for r := range results {
		if r.err != nil {
			t.Fatalf("EnsureLSPRegistered returned error: %v", r.err)
		}
		got = append(got, r.entry)
	}
	if len(got) != 2 {
		t.Fatalf("got %d results, want 2", len(got))
	}
	for i, entry := range got {
		if entry.Port != readyPort {
			t.Fatalf("result[%d].Port = %d, want readiness port %d", i, entry.Port, readyPort)
		}
		if entry.WorkspaceKey != wsKey || entry.WorkspacePath != canonical || entry.Language != "python" {
			t.Fatalf("result[%d] entry mismatch: %+v", i, entry)
		}
	}
	if got[0].Port != got[1].Port {
		t.Fatalf("concurrent calls allocated different ports: %d vs %d", got[0].Port, got[1].Port)
	}
	if calls := reconcileCalls.Load(); calls != 1 {
		t.Fatalf("reconcile calls = %d, want 1", calls)
	}
	if calls := readinessCalls.Load(); calls != 2 {
		t.Fatalf("readiness calls = %d, want 2 (first spawn wait + existing-row probe)", calls)
	}
	if n := countEntries(h.fakeClients); n != 0 {
		t.Fatalf("EnsureLSPRegistered must not write client configs; fake client entries = %d", n)
	}

	reg := NewRegistry(h.regPath)
	if err := reg.Load(); err != nil {
		t.Fatalf("load registry: %v", err)
	}
	if rows := reg.ListByWorkspaceLSP(wsKey); len(rows) != 1 {
		t.Fatalf("registry rows for %s = %d, want 1: %+v", wsKey, len(rows), rows)
	}
}

func TestEnsureLSPRegistered_ExistingReadySupervisorOwnedRowReturnsWithoutReconcile(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	restoreState := SetDaemonStateRootForTest(apitest.HardenedTempDir(t))
	defer restoreState()

	origReconcile := registerSupervisorReconcileFn
	registerSupervisorReconcileFn = func(ctx context.Context, apply bool) (ReconcileResponse, error) {
		t.Fatal("existing ready LSP row must return before supervisor reconcile")
		return ReconcileResponse{}, nil
	}
	defer func() { registerSupervisorReconcileFn = origReconcile }()

	origReadiness := proxyReadinessFn
	var readinessCalls atomic.Int32
	proxyReadinessFn = func(port int, timeout time.Duration) error {
		readinessCalls.Add(1)
		return nil
	}
	defer func() { proxyReadinessFn = origReadiness }()

	ws := t.TempDir()
	canonical, err := CanonicalWorkspacePath(ws)
	if err != nil {
		t.Fatalf("CanonicalWorkspacePath: %v", err)
	}
	wsKey := WorkspaceKey(canonical)
	want := WorkspaceEntry{
		WorkspaceKey:  wsKey,
		WorkspacePath: canonical,
		Language:      "python",
		Backend:       "mcp-language-server",
		Port:          9242,
		TaskName:      LSPTaskNameForWorkspaceLanguage(wsKey, "python"),
		Lifecycle:     LifecycleActive,
	}
	reg := NewRegistry(h.regPath)
	if err := reg.PutLSP(want); err != nil {
		t.Fatalf("PutLSP: %v", err)
	}
	if err := reg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	intentPath, err := DefaultSupervisorIntentPath()
	if err != nil {
		t.Fatalf("DefaultSupervisorIntentPath: %v", err)
	}
	writeSupervisorIntentForRegisterTest(t, intentPath, &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{
			BuildSupervisorDaemonForLSP(want, testCanonicalMcphubPathOverride),
		},
	})

	got, err := NewAPI().EnsureLSPRegistered(context.Background(), wsKey, canonical, "python")
	if err != nil {
		t.Fatalf("EnsureLSPRegistered existing row: %v", err)
	}
	if got.Port != want.Port || got.Lifecycle != want.Lifecycle {
		t.Fatalf("existing row mismatch: got %+v want %+v", got, want)
	}
	if calls := readinessCalls.Load(); calls != 1 {
		t.Fatalf("existing ready row readiness probes = %d, want 1", calls)
	}
}

func TestEnsureLSPRegistered_ExistingReadyUnownedRowPromotesThroughSupervisor(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	restoreState := SetDaemonStateRootForTest(apitest.HardenedTempDir(t))
	defer restoreState()

	var reconcileCalls atomic.Int32
	origReconcile := registerSupervisorReconcileFn
	registerSupervisorReconcileFn = func(ctx context.Context, apply bool) (ReconcileResponse, error) {
		if !apply {
			t.Fatalf("EnsureLSPRegistered called reconcile with apply=false")
		}
		reconcileCalls.Add(1)
		return ReconcileResponse{DryRun: false}, nil
	}
	defer func() { registerSupervisorReconcileFn = origReconcile }()

	origReadiness := proxyReadinessFn
	var readinessCalls atomic.Int32
	proxyReadinessFn = func(port int, timeout time.Duration) error {
		readinessCalls.Add(1)
		if port != 9242 {
			t.Fatalf("readiness port = %d, want 9242", port)
		}
		return nil
	}
	defer func() { proxyReadinessFn = origReadiness }()

	origKill := killByPortFn
	var killCalls atomic.Int32
	killByPortFn = func(port int, timeout time.Duration) error {
		killCalls.Add(1)
		if port != 9242 {
			t.Fatalf("kill port = %d, want 9242", port)
		}
		if timeout != 5*time.Second {
			t.Fatalf("kill timeout = %v, want 5s", timeout)
		}
		return nil
	}
	defer func() { killByPortFn = origKill }()

	ws := t.TempDir()
	canonical, err := CanonicalWorkspacePath(ws)
	if err != nil {
		t.Fatalf("CanonicalWorkspacePath: %v", err)
	}
	wsKey := WorkspaceKey(canonical)
	want := WorkspaceEntry{
		WorkspaceKey:  wsKey,
		WorkspacePath: canonical,
		Language:      "python",
		Backend:       "mcp-language-server",
		Port:          9242,
		TaskName:      LSPTaskNameForWorkspaceLanguage(wsKey, "python"),
		Lifecycle:     LifecycleActive,
	}
	reg := NewRegistry(h.regPath)
	if err := reg.PutLSP(want); err != nil {
		t.Fatalf("PutLSP: %v", err)
	}
	if err := reg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := NewAPI().EnsureLSPRegistered(context.Background(), wsKey, canonical, "python")
	if err != nil {
		t.Fatalf("EnsureLSPRegistered existing ready unowned row: %v", err)
	}
	if got.Port != want.Port || got.Lifecycle != want.Lifecycle {
		t.Fatalf("existing row mismatch: got %+v want %+v", got, want)
	}
	if calls := readinessCalls.Load(); calls != 2 {
		t.Fatalf("readiness calls = %d, want ownership probe + post-reconcile wait", calls)
	}
	if calls := killCalls.Load(); calls != 1 {
		t.Fatalf("kill calls = %d, want 1 to free the unowned ready port before reconcile", calls)
	}
	if calls := reconcileCalls.Load(); calls != 1 {
		t.Fatalf("reconcile calls = %d, want 1", calls)
	}
	intentPath, err := DefaultSupervisorIntentPath()
	if err != nil {
		t.Fatalf("DefaultSupervisorIntentPath: %v", err)
	}
	intent, err := ReadSupervisorIntent(intentPath)
	if err != nil {
		t.Fatalf("ReadSupervisorIntent: %v", err)
	}
	taskName := LSPIntentTaskNameForWorkspaceLanguage(wsKey, "python")
	if row := intent.FindSupervisorDaemonByTaskName(taskName); row == nil {
		t.Fatalf("ready unowned row did not write supervisor intent %s; rows=%+v", taskName, intent.Daemons)
	}
}

func TestEnsureLSPRegistered_ExistingDeadRowReconcilesAndWaitsForReady(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	restoreState := SetDaemonStateRootForTest(apitest.HardenedTempDir(t))
	defer restoreState()

	var reconcileCalls atomic.Int32
	origReconcile := registerSupervisorReconcileFn
	registerSupervisorReconcileFn = func(ctx context.Context, apply bool) (ReconcileResponse, error) {
		if !apply {
			t.Fatalf("EnsureLSPRegistered called reconcile with apply=false")
		}
		reconcileCalls.Add(1)
		return ReconcileResponse{DryRun: false}, nil
	}
	defer func() { registerSupervisorReconcileFn = origReconcile }()

	origReadiness := proxyReadinessFn
	var readinessCalls atomic.Int32
	proxyReadinessFn = func(port int, timeout time.Duration) error {
		call := readinessCalls.Add(1)
		if port != 9242 {
			t.Fatalf("readiness port = %d, want 9242", port)
		}
		if call == 1 {
			return errors.New("existing proxy is dead")
		}
		return nil
	}
	defer func() { proxyReadinessFn = origReadiness }()

	ws := t.TempDir()
	canonical, err := CanonicalWorkspacePath(ws)
	if err != nil {
		t.Fatalf("CanonicalWorkspacePath: %v", err)
	}
	wsKey := WorkspaceKey(canonical)
	want := WorkspaceEntry{
		WorkspaceKey:  wsKey,
		WorkspacePath: canonical,
		Language:      "python",
		Backend:       "mcp-language-server",
		Port:          9242,
		TaskName:      LSPTaskNameForWorkspaceLanguage(wsKey, "python"),
		Lifecycle:     LifecycleActive,
	}
	reg := NewRegistry(h.regPath)
	if err := reg.PutLSP(want); err != nil {
		t.Fatalf("PutLSP: %v", err)
	}
	if err := reg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := NewAPI().EnsureLSPRegistered(context.Background(), wsKey, canonical, "python")
	if err != nil {
		t.Fatalf("EnsureLSPRegistered existing dead row: %v", err)
	}
	if got.Port != want.Port || got.Lifecycle != want.Lifecycle {
		t.Fatalf("existing row mismatch: got %+v want %+v", got, want)
	}
	if calls := readinessCalls.Load(); calls != 2 {
		t.Fatalf("readiness calls = %d, want dead probe + post-reconcile wait", calls)
	}
	if calls := reconcileCalls.Load(); calls != 1 {
		t.Fatalf("reconcile calls = %d, want 1", calls)
	}

	intentPath, err := DefaultSupervisorIntentPath()
	if err != nil {
		t.Fatalf("DefaultSupervisorIntentPath: %v", err)
	}
	intent, err := ReadSupervisorIntent(intentPath)
	if err != nil {
		t.Fatalf("ReadSupervisorIntent: %v", err)
	}
	taskName := LSPIntentTaskNameForWorkspaceLanguage(wsKey, "python")
	if row := intent.FindSupervisorDaemonByTaskName(taskName); row == nil {
		t.Fatalf("existing dead row did not write supervisor intent %s; rows=%+v", taskName, intent.Daemons)
	}
}

func TestEnsureLSPRegistered_RollbackDoesNotKillPortBeforeSupervisorSpawn(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	restoreState := SetDaemonStateRootForTest(apitest.HardenedTempDir(t))
	defer restoreState()

	origReconcile := registerSupervisorReconcileFn
	registerSupervisorReconcileFn = func(ctx context.Context, apply bool) (ReconcileResponse, error) {
		if !apply {
			t.Fatalf("EnsureLSPRegistered called reconcile with apply=false")
		}
		return ReconcileResponse{}, errors.New("induced reconcile failure before spawn")
	}
	defer func() { registerSupervisorReconcileFn = origReconcile }()

	origKill := killByPortFn
	var killed atomic.Int32
	killByPortFn = func(port int, timeout time.Duration) error {
		killed.Add(1)
		return nil
	}
	defer func() { killByPortFn = origKill }()

	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "pyproject.toml"), []byte("[project]\n"), 0o600); err != nil {
		t.Fatalf("touch marker: %v", err)
	}
	canonical, err := CanonicalWorkspacePath(ws)
	if err != nil {
		t.Fatalf("CanonicalWorkspacePath: %v", err)
	}
	_, err = NewAPI().EnsureLSPRegistered(context.Background(), WorkspaceKey(canonical), canonical, "python")
	if err == nil {
		t.Fatal("expected reconcile failure")
	}
	if got := killed.Load(); got != 0 {
		t.Fatalf("rollback killed port before supervisor spawn was requested; kill calls=%d", got)
	}
	reg := NewRegistry(h.regPath)
	if err := reg.Load(); err != nil {
		t.Fatalf("load registry: %v", err)
	}
	if rows := reg.ListByWorkspaceLSP(WorkspaceKey(canonical)); len(rows) != 0 {
		t.Fatalf("registry rows leaked after rollback: %+v", rows)
	}
}
