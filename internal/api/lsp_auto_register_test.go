package api

import (
	"context"
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
	if calls := readinessCalls.Load(); calls != 1 {
		t.Fatalf("readiness calls = %d, want 1", calls)
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
