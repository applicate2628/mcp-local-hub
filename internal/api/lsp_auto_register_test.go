package api

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofrs/flock"

	"mcp-local-hub/internal/api/apitest"
	"mcp-local-hub/internal/scheduler"
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

func TestEnsureLSPRegistered_ExistingLegacySymlinkKeyReturnsPriorRow(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	restoreState := SetDaemonStateRootForTest(apitest.HardenedTempDir(t))
	defer restoreState()

	origReadiness := proxyReadinessFn
	proxyReadinessFn = func(port int, timeout time.Duration) error { return nil }
	defer func() { proxyReadinessFn = origReadiness }()

	root := t.TempDir()
	realProject := filepath.Join(root, "real-project")
	if err := os.MkdirAll(realProject, 0o755); err != nil {
		t.Fatalf("mkdir real project: %v", err)
	}
	aliasProject := filepath.Join(root, "alias-project")
	if err := os.Symlink(realProject, aliasProject); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	legacyPath, err := CanonicalWorkspacePathLegacyCompat(aliasProject)
	if err != nil {
		t.Fatalf("CanonicalWorkspacePathLegacyCompat: %v", err)
	}
	legacyKey := WorkspaceKey(legacyPath)
	canonical, err := CanonicalWorkspacePath(aliasProject)
	if err != nil {
		t.Fatalf("CanonicalWorkspacePath: %v", err)
	}
	if legacyKey == WorkspaceKey(canonical) {
		t.Skip("legacy and symlink-resolved workspace keys are identical")
	}

	prior := WorkspaceEntry{
		WorkspaceKey:  legacyKey,
		WorkspacePath: legacyPath,
		Language:      "python",
		Backend:       "mcp-language-server",
		Port:          9243,
		TaskName:      LSPTaskNameForWorkspaceLanguage(legacyKey, "python"),
		Lifecycle:     LifecycleActive,
	}
	reg := NewRegistry(h.regPath)
	if err := reg.PutLSP(prior); err != nil {
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
			BuildSupervisorDaemonForLSP(prior, testCanonicalMcphubPathOverride),
		},
	})

	got, err := NewAPI().EnsureLSPRegistered(context.Background(), legacyKey, aliasProject, "python")
	if err != nil {
		t.Fatalf("EnsureLSPRegistered legacy row: %v", err)
	}
	if got.WorkspaceKey != legacyKey || got.Port != prior.Port {
		t.Fatalf("got %+v, want legacy key %s port %d", got, legacyKey, prior.Port)
	}
	if err := reg.Load(); err != nil {
		t.Fatalf("reload registry: %v", err)
	}
	if rows := reg.LSPEntries(); len(rows) != 1 {
		t.Fatalf("registry rows = %d, want 1 existing legacy row only: %+v", len(rows), rows)
	}
}

func TestEnsureLSPRegistered_ExistingDeadSupervisorOwnedRowSkipsScheduler(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	restoreState := SetDaemonStateRootForTest(apitest.HardenedTempDir(t))
	defer restoreState()

	origScheduler := schedulerFactoryFn
	var schedulerCalls atomic.Int32
	schedulerFactoryFn = func() (scheduler.Scheduler, error) {
		schedulerCalls.Add(1)
		return nil, errors.New("scheduler unavailable on this platform")
	}
	defer func() { schedulerFactoryFn = origScheduler }()

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
		t.Fatalf("EnsureLSPRegistered existing dead owned row: %v", err)
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
	if calls := schedulerCalls.Load(); calls != 0 {
		t.Fatalf("scheduler constructor calls = %d, want 0 for already supervisor-owned row", calls)
	}
}

func TestEnsureLSPRegistered_ExistingReadyUnownedRowPromotesThroughSupervisor(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	restoreState := SetDaemonStateRootForTest(apitest.HardenedTempDir(t))
	defer restoreState()

	origScheduler := schedulerFactoryFn
	schedulerFactoryFn = func() (scheduler.Scheduler, error) { return h.fakeSch, nil }
	defer func() { schedulerFactoryFn = origScheduler }()

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
	if slices.Contains(h.fakeSch.deleteNames, want.TaskName) {
		t.Fatalf("legacy task delete should be skipped when ExportXML reports not found; deleteNames=%v", h.fakeSch.deleteNames)
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

func TestEnsureLSPRegistered_ExistingReadyUnownedRowPromotesWhenSchedulerNotImplemented(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	restoreState := SetDaemonStateRootForTest(apitest.HardenedTempDir(t))
	defer restoreState()

	origScheduler := schedulerFactoryFn
	var schedulerCalls atomic.Int32
	schedulerFactoryFn = func() (scheduler.Scheduler, error) {
		schedulerCalls.Add(1)
		return nil, fmt.Errorf("scheduler.New: %w", scheduler.ErrNotImplemented)
	}
	defer func() { schedulerFactoryFn = origScheduler }()

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
		t.Fatalf("EnsureLSPRegistered existing ready unowned row with not-implemented scheduler: %v", err)
	}
	if got.Port != want.Port || got.Lifecycle != want.Lifecycle {
		t.Fatalf("existing row mismatch: got %+v want %+v", got, want)
	}
	if calls := schedulerCalls.Load(); calls != 1 {
		t.Fatalf("scheduler constructor calls = %d, want 1", calls)
	}
	if calls := killCalls.Load(); calls != 1 {
		t.Fatalf("kill calls = %d, want 1 to free the unowned ready port before reconcile", calls)
	}
	if calls := readinessCalls.Load(); calls != 2 {
		t.Fatalf("readiness calls = %d, want ownership probe + post-reconcile wait", calls)
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

func TestEnsureLSPRegistered_ExistingReadyUnownedRowKillFailureAbortsPromotion(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	restoreState := SetDaemonStateRootForTest(apitest.HardenedTempDir(t))
	defer restoreState()

	origScheduler := schedulerFactoryFn
	schedulerFactoryFn = func() (scheduler.Scheduler, error) { return h.fakeSch, nil }
	defer func() { schedulerFactoryFn = origScheduler }()

	var reconcileCalls atomic.Int32
	origReconcile := registerSupervisorReconcileFn
	registerSupervisorReconcileFn = func(ctx context.Context, apply bool) (ReconcileResponse, error) {
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
		return fmt.Errorf("access denied killing port %d", port)
	}
	defer func() { killByPortFn = origKill }()

	ws := t.TempDir()
	canonical, err := CanonicalWorkspacePath(ws)
	if err != nil {
		t.Fatalf("CanonicalWorkspacePath: %v", err)
	}
	wsKey := WorkspaceKey(canonical)
	prior := WorkspaceEntry{
		WorkspaceKey:  wsKey,
		WorkspacePath: canonical,
		Language:      "python",
		Backend:       "mcp-language-server",
		Port:          9242,
		TaskName:      LSPTaskNameForWorkspaceLanguage(wsKey, "python"),
		Lifecycle:     LifecycleActive,
	}
	reg := NewRegistry(h.regPath)
	if err := reg.PutLSP(prior); err != nil {
		t.Fatalf("PutLSP: %v", err)
	}
	if err := reg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	_, err = NewAPI().EnsureLSPRegistered(context.Background(), wsKey, canonical, "python")
	if err == nil {
		t.Fatal("EnsureLSPRegistered returned nil error after live unowned port kill failed")
	}
	if !strings.Contains(err.Error(), "kill legacy LSP proxy") {
		t.Fatalf("error = %v, want kill legacy LSP proxy context", err)
	}
	if calls := killCalls.Load(); calls != 1 {
		t.Fatalf("kill calls = %d, want 1", calls)
	}
	if calls := readinessCalls.Load(); calls != 1 {
		t.Fatalf("readiness calls = %d, want only ownership probe before abort", calls)
	}
	if calls := reconcileCalls.Load(); calls != 0 {
		t.Fatalf("reconcile calls = %d, want 0 after kill failure", calls)
	}
	intentPath, err := DefaultSupervisorIntentPath()
	if err != nil {
		t.Fatalf("DefaultSupervisorIntentPath: %v", err)
	}
	intent, err := ReadSupervisorIntent(intentPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ReadSupervisorIntent: %v", err)
	}
	if intent != nil {
		taskName := LSPIntentTaskNameForWorkspaceLanguage(wsKey, "python")
		if row := intent.FindSupervisorDaemonByTaskName(taskName); row != nil {
			t.Fatalf("supervisor intent row %s written despite kill failure: %+v", taskName, row)
		}
	}
}

func TestEnsureLSPRegistered_ExistingReadyUnownedRowSchedulerRealFailureFailsLoud(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	restoreState := SetDaemonStateRootForTest(apitest.HardenedTempDir(t))
	defer restoreState()

	origScheduler := schedulerFactoryFn
	var schedulerCalls atomic.Int32
	schedulerFactoryFn = func() (scheduler.Scheduler, error) {
		schedulerCalls.Add(1)
		return nil, errors.New("Task Scheduler user lookup failed")
	}
	defer func() { schedulerFactoryFn = origScheduler }()

	var reconcileCalls atomic.Int32
	origReconcile := registerSupervisorReconcileFn
	registerSupervisorReconcileFn = func(ctx context.Context, apply bool) (ReconcileResponse, error) {
		reconcileCalls.Add(1)
		return ReconcileResponse{DryRun: false}, nil
	}
	defer func() { registerSupervisorReconcileFn = origReconcile }()

	origReadiness := proxyReadinessFn
	var readinessCalls atomic.Int32
	proxyReadinessFn = func(port int, timeout time.Duration) error {
		readinessCalls.Add(1)
		return nil
	}
	defer func() { proxyReadinessFn = origReadiness }()

	origKill := killByPortFn
	var killCalls atomic.Int32
	killByPortFn = func(port int, timeout time.Duration) error {
		killCalls.Add(1)
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

	_, err = NewAPI().EnsureLSPRegistered(context.Background(), wsKey, canonical, "python")
	if err == nil {
		t.Fatal("EnsureLSPRegistered returned nil error for real scheduler constructor failure")
	}
	if !strings.Contains(err.Error(), "Task Scheduler user lookup failed") {
		t.Fatalf("error = %v, want scheduler constructor failure surfaced", err)
	}
	if calls := schedulerCalls.Load(); calls != 1 {
		t.Fatalf("scheduler constructor calls = %d, want 1", calls)
	}
	if calls := killCalls.Load(); calls != 0 {
		t.Fatalf("kill calls = %d, want 0 when legacy task ownership cannot be checked", calls)
	}
	if calls := readinessCalls.Load(); calls != 1 {
		t.Fatalf("readiness calls = %d, want 1 ownership probe only", calls)
	}
	if calls := reconcileCalls.Load(); calls != 0 {
		t.Fatalf("reconcile calls = %d, want 0 after scheduler constructor failure", calls)
	}
}

func TestEnsureLSPRegistered_ExistingReadyUnownedRowDeletesLegacyTaskBeforePromote(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	restoreState := SetDaemonStateRootForTest(apitest.HardenedTempDir(t))
	defer restoreState()

	origScheduler := schedulerFactoryFn
	schedulerFactoryFn = func() (scheduler.Scheduler, error) { return h.fakeSch, nil }
	defer func() { schedulerFactoryFn = origScheduler }()

	origReconcile := registerSupervisorReconcileFn
	registerSupervisorReconcileFn = func(ctx context.Context, apply bool) (ReconcileResponse, error) {
		if !apply {
			t.Fatalf("EnsureLSPRegistered called reconcile with apply=false")
		}
		return ReconcileResponse{DryRun: false}, nil
	}
	defer func() { registerSupervisorReconcileFn = origReconcile }()

	origReadiness := proxyReadinessFn
	proxyReadinessFn = func(port int, timeout time.Duration) error { return nil }
	defer func() { proxyReadinessFn = origReadiness }()

	origKill := killByPortFn
	killByPortFn = func(port int, timeout time.Duration) error { return nil }
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
	h.fakeSch.tasks[want.TaskName] = true
	h.fakeSch.xml[want.TaskName] = []byte(`<Task name="` + want.TaskName + `"/>`)

	if _, err := NewAPI().EnsureLSPRegistered(context.Background(), wsKey, canonical, "python"); err != nil {
		t.Fatalf("EnsureLSPRegistered existing ready unowned row: %v", err)
	}
	if !slices.Contains(h.fakeSch.deleteNames, want.TaskName) {
		t.Fatalf("legacy task %s was not deleted before promote; deleteNames=%v", want.TaskName, h.fakeSch.deleteNames)
	}
}

func TestEnsureLSPRegistered_ExistingPromotionRollbackMatrix(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	restoreState := SetDaemonStateRootForTest(apitest.HardenedTempDir(t))
	defer restoreState()

	origScheduler := schedulerFactoryFn
	schedulerFactoryFn = func() (scheduler.Scheduler, error) { return h.fakeSch, nil }
	defer func() { schedulerFactoryFn = origScheduler }()

	origReconcile := registerSupervisorReconcileFn
	registerSupervisorReconcileFn = func(ctx context.Context, apply bool) (ReconcileResponse, error) {
		if !apply {
			t.Fatalf("EnsureLSPRegistered called reconcile with apply=false")
		}
		return ReconcileResponse{}, errors.New("induced reconcile failure")
	}
	defer func() { registerSupervisorReconcileFn = origReconcile }()

	origReadiness := proxyReadinessFn
	proxyReadinessFn = func(port int, timeout time.Duration) error { return nil }
	defer func() { proxyReadinessFn = origReadiness }()

	origKill := killByPortFn
	killByPortFn = func(port int, timeout time.Duration) error { return nil }
	defer func() { killByPortFn = origKill }()

	ws := t.TempDir()
	canonical, err := CanonicalWorkspacePath(ws)
	if err != nil {
		t.Fatalf("CanonicalWorkspacePath: %v", err)
	}
	wsKey := WorkspaceKey(canonical)
	prior := WorkspaceEntry{
		WorkspaceKey:  wsKey,
		WorkspacePath: canonical,
		Language:      "python",
		Backend:       "mcp-language-server",
		Port:          9242,
		TaskName:      LSPTaskNameForWorkspaceLanguage(wsKey, "python"),
		Lifecycle:     LifecycleActive,
	}
	reg := NewRegistry(h.regPath)
	if err := reg.PutLSP(prior); err != nil {
		t.Fatalf("PutLSP: %v", err)
	}
	if err := reg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	h.fakeSch.tasks[prior.TaskName] = true
	h.fakeSch.xml[prior.TaskName] = []byte(`<Task name="` + prior.TaskName + `"/>`)

	_, err = NewAPI().EnsureLSPRegistered(context.Background(), wsKey, canonical, "python")
	if err == nil {
		t.Fatal("expected reconcile failure")
	}
	if !slices.Contains(h.fakeSch.deleteNames, prior.TaskName) {
		t.Fatalf("legacy task %s was not deleted before promote; deleteNames=%v", prior.TaskName, h.fakeSch.deleteNames)
	}
	if !slices.Contains(h.fakeSch.importNames, prior.TaskName) {
		t.Fatalf("legacy task %s was not restored with ImportXML; importNames=%v", prior.TaskName, h.fakeSch.importNames)
	}
	if !slices.Contains(h.fakeSch.runNames, prior.TaskName) {
		t.Fatalf("legacy task %s was not restarted after restore; runNames=%v", prior.TaskName, h.fakeSch.runNames)
	}
}

func TestEnsureLSPRegistered_KillFailureRunsPrearmedLegacyRestore(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	restoreState := SetDaemonStateRootForTest(apitest.HardenedTempDir(t))
	defer restoreState()

	origScheduler := schedulerFactoryFn
	schedulerFactoryFn = func() (scheduler.Scheduler, error) { return h.fakeSch, nil }
	defer func() { schedulerFactoryFn = origScheduler }()

	origReconcile := registerSupervisorReconcileFn
	registerSupervisorReconcileFn = func(ctx context.Context, apply bool) (ReconcileResponse, error) {
		t.Fatal("EnsureLSPRegistered must fail before supervisor reconcile")
		return ReconcileResponse{}, nil
	}
	defer func() { registerSupervisorReconcileFn = origReconcile }()

	origReadiness := proxyReadinessFn
	proxyReadinessFn = func(port int, timeout time.Duration) error { return nil }
	defer func() { proxyReadinessFn = origReadiness }()

	origKill := killByPortFn
	killByPortFn = func(port int, timeout time.Duration) error {
		return fmt.Errorf("access denied killing port %d", port)
	}
	defer func() { killByPortFn = origKill }()

	ws := t.TempDir()
	canonical, err := CanonicalWorkspacePath(ws)
	if err != nil {
		t.Fatalf("CanonicalWorkspacePath: %v", err)
	}
	wsKey := WorkspaceKey(canonical)
	prior := WorkspaceEntry{
		WorkspaceKey:  wsKey,
		WorkspacePath: canonical,
		Language:      "python",
		Backend:       "mcp-language-server",
		Port:          9242,
		TaskName:      LSPTaskNameForWorkspaceLanguage(wsKey, "python"),
		Lifecycle:     LifecycleActive,
	}
	reg := NewRegistry(h.regPath)
	if err := reg.PutLSP(prior); err != nil {
		t.Fatalf("PutLSP: %v", err)
	}
	if err := reg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	h.fakeSch.tasks[prior.TaskName] = true
	h.fakeSch.xml[prior.TaskName] = []byte(`<Task name="` + prior.TaskName + `"/>`)

	_, err = NewAPI().EnsureLSPRegistered(context.Background(), wsKey, canonical, "python")
	if err == nil {
		t.Fatal("expected kill failure")
	}
	if !strings.Contains(err.Error(), "kill legacy LSP proxy") {
		t.Fatalf("error = %v, want kill legacy LSP proxy context", err)
	}
	if len(h.fakeSch.deleteNames) != 0 ||
		!slices.Contains(h.fakeSch.importNames, prior.TaskName) ||
		!slices.Contains(h.fakeSch.runNames, prior.TaskName) {
		t.Fatalf("prearmed legacy restore did not run after possibly-partial kill failure: delete=%v import=%v run=%v",
			h.fakeSch.deleteNames, h.fakeSch.importNames, h.fakeSch.runNames)
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

func TestEnsureLSPRegistered_NewRowRollbackMatrix(t *testing.T) {
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
	if got := killed.Load(); got != 1 {
		t.Fatalf("rollback possible-proxy kill calls=%d, want 1 after reconcile attempt", got)
	}
	reg := NewRegistry(h.regPath)
	if err := reg.Load(); err != nil {
		t.Fatalf("load registry: %v", err)
	}
	if rows := reg.ListByWorkspaceLSP(WorkspaceKey(canonical)); len(rows) != 0 {
		t.Fatalf("registry rows leaked after rollback: %+v", rows)
	}
}

func TestEnsureLSPRegistered_AppliedReleaseCompletesForwardHandoff(t *testing.T) {
	tests := []struct {
		name            string
		seedPrior       bool
		registryRelease bool
		reconcileFails  bool
		failOnRelease   int32
	}{
		{name: "existing-row-promotion", seedPrior: true, failOnRelease: 2},
		{name: "new-row-registration", failOnRelease: 1},
		{name: "new-row-registry-release", registryRelease: true, failOnRelease: 1},
		{name: "new-row-registry-release-reconcile-fails", registryRelease: true, reconcileFails: true, failOnRelease: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newRegisterHarness(t)
			defer h.restore()
			restoreState := SetDaemonStateRootForTest(apitest.HardenedTempDir(t))
			defer restoreState()

			origScheduler := schedulerFactoryFn
			schedulerFactoryFn = func() (scheduler.Scheduler, error) { return h.fakeSch, nil }
			defer func() { schedulerFactoryFn = origScheduler }()
			origReadiness := proxyReadinessFn
			var readinessCalls atomic.Int32
			proxyReadinessFn = func(int, time.Duration) error {
				readinessCalls.Add(1)
				return nil
			}
			defer func() { proxyReadinessFn = origReadiness }()
			origKill := killByPortFn
			killByPortFn = func(int, time.Duration) error { return nil }
			defer func() { killByPortFn = origKill }()
			origReconcile := registerSupervisorReconcileFn
			var reconcileCalls atomic.Int32
			reconcileCause := errors.New("injected mandatory reconcile failure")
			registerSupervisorReconcileFn = func(context.Context, bool) (ReconcileResponse, error) {
				reconcileCalls.Add(1)
				if tc.reconcileFails {
					return ReconcileResponse{}, reconcileCause
				}
				return ReconcileResponse{}, nil
			}
			defer func() { registerSupervisorReconcileFn = origReconcile }()

			ws := t.TempDir()
			if err := os.WriteFile(filepath.Join(ws, "pyproject.toml"), []byte("[project]\n"), 0o600); err != nil {
				t.Fatalf("touch marker: %v", err)
			}
			canonical, err := CanonicalWorkspacePath(ws)
			if err != nil {
				t.Fatalf("CanonicalWorkspacePath: %v", err)
			}
			wsKey := WorkspaceKey(canonical)
			if tc.seedPrior {
				prior := WorkspaceEntry{
					WorkspaceKey: wsKey, WorkspacePath: canonical, Language: "python",
					Backend: "mcp-language-server", Port: 9242,
					TaskName:      LSPTaskNameForWorkspaceLanguage(wsKey, "python"),
					ClientEntries: map[string]string{"codex": "lsp-python-" + wsKey[:4]},
					Lifecycle:     LifecycleActive,
				}
				reg := NewRegistry(h.regPath)
				if err := reg.PutLSP(prior); err != nil {
					t.Fatalf("PutLSP: %v", err)
				}
				if err := reg.Save(); err != nil {
					t.Fatalf("Save: %v", err)
				}
			}

			intentPath, err := DefaultSupervisorIntentPath()
			if err != nil {
				t.Fatalf("DefaultSupervisorIntentPath: %v", err)
			}
			lockPath := supervisorIntentLockPath(intentPath)
			if tc.registryRelease {
				lockPath = NewRegistry(h.regPath).LockPath()
			}
			releaseCause := errors.New("injected LSP durable-state release failure")
			previousUnlock := flockUnlockFn
			var targetReleases atomic.Int32
			var stranded []*flock.Flock
			flockUnlockFn = func(fl *flock.Flock) error {
				if fl.Path() == lockPath && targetReleases.Add(1) == tc.failOnRelease {
					stranded = append(stranded, fl)
					return releaseCause
				}
				return previousUnlock(fl)
			}
			defer func() {
				flockUnlockFn = previousUnlock
				for _, fl := range stranded {
					_ = fl.Unlock()
				}
				unconfirmedLockReleasesMu.Lock()
				delete(unconfirmedLockReleases, lockPath)
				unconfirmedLockReleasesMu.Unlock()
			}()

			got, err := NewAPI().EnsureLSPRegistered(context.Background(), wsKey, canonical, "python")
			if !errors.Is(err, ErrLockReleaseUnconfirmed) || !errors.Is(err, releaseCause) {
				t.Fatalf("error = %v, want applied release class and injected cause", err)
			}
			if tc.reconcileFails && !errors.Is(err, reconcileCause) {
				t.Fatalf("error = %v, want mandatory reconcile cause", err)
			}
			if got.WorkspaceKey != wsKey || got.Language != "python" || got.Port == 0 {
				t.Fatalf("truthful committed entry = %+v", got)
			}
			wantReadiness := int32(1)
			if tc.seedPrior {
				wantReadiness++ // the existing-row liveness probe precedes promotion
			}
			if tc.reconcileFails {
				wantReadiness = 0
			}
			if reconcileCalls.Load() != 1 || readinessCalls.Load() != wantReadiness {
				t.Fatalf("reconcile=%d readiness=%d, want mandatory handoff 1/%d after applied release", reconcileCalls.Load(), readinessCalls.Load(), wantReadiness)
			}
			reg := NewRegistry(h.regPath)
			if err := reg.Load(); err != nil {
				t.Fatalf("Load registry: %v", err)
			}
			persisted, ok := reg.Get(wsKey, "python")
			if !ok || persisted.Port != got.Port || !reflect.DeepEqual(persisted.ClientEntries, got.ClientEntries) {
				t.Fatalf("persisted row = %+v ok=%t, want committed entry %+v", persisted, ok, got)
			}
			intent, _, readErr := readSupervisorIntentForMerge(intentPath)
			if readErr != nil {
				t.Fatalf("read committed supervisor intent: %v", readErr)
			}
			if row := intent.FindSupervisorDaemonByTaskName(LSPIntentTaskNameForWorkspaceLanguage(wsKey, "python")); row == nil {
				t.Fatalf("committed descriptor missing after applied release: %+v", intent.Daemons)
			}
		})
	}
}

func TestEnsureLSPRegistered_AuditContractUnchanged(t *testing.T) {
	assertNoWorkspaceRegistered := func(t *testing.T, recorder *recordingAuditWriter) {
		t.Helper()
		for _, entry := range recorder.entries {
			if entry.Action == AuditActionWorkspaceRegistered {
				t.Fatalf("EnsureLSPRegistered emitted workspace-registered: %+v", recorder.entries)
			}
		}
	}

	t.Run("fast-return", func(t *testing.T) {
		h := newRegisterHarness(t)
		defer h.restore()
		restoreState := SetDaemonStateRootForTest(apitest.HardenedTempDir(t))
		defer restoreState()
		recorder := &recordingAuditWriter{}
		installRecordingAudit(t, recorder)
		origReadiness := proxyReadinessFn
		proxyReadinessFn = func(int, time.Duration) error { return nil }
		defer func() { proxyReadinessFn = origReadiness }()

		canonical, err := CanonicalWorkspacePath(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		wsKey := WorkspaceKey(canonical)
		entry := WorkspaceEntry{
			WorkspaceKey: wsKey, WorkspacePath: canonical, Language: "python",
			Backend: "mcp-language-server", Port: 9242,
			TaskName: LSPTaskNameForWorkspaceLanguage(wsKey, "python"),
		}
		reg := NewRegistry(h.regPath)
		if err := reg.PutLSP(entry); err != nil {
			t.Fatal(err)
		}
		if err := reg.Save(); err != nil {
			t.Fatal(err)
		}
		executable, err := canonicalMcphubPath()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := NewAPI().upsertLSPSupervisorIntent(entry, executable); err != nil {
			t.Fatal(err)
		}
		if _, err := NewAPI().EnsureLSPRegistered(context.Background(), wsKey, canonical, "python"); err != nil {
			t.Fatal(err)
		}
		assertNoWorkspaceRegistered(t, recorder)
	})

	t.Run("promotion", func(t *testing.T) {
		h := newRegisterHarness(t)
		defer h.restore()
		restoreState := SetDaemonStateRootForTest(apitest.HardenedTempDir(t))
		defer restoreState()
		recorder := &recordingAuditWriter{}
		installRecordingAudit(t, recorder)
		origScheduler := schedulerFactoryFn
		schedulerFactoryFn = func() (scheduler.Scheduler, error) { return h.fakeSch, nil }
		defer func() { schedulerFactoryFn = origScheduler }()
		origReconcile := registerSupervisorReconcileFn
		registerSupervisorReconcileFn = func(context.Context, bool) (ReconcileResponse, error) {
			return ReconcileResponse{}, nil
		}
		defer func() { registerSupervisorReconcileFn = origReconcile }()
		origReadiness := proxyReadinessFn
		readinessCalls := 0
		proxyReadinessFn = func(int, time.Duration) error {
			readinessCalls++
			if readinessCalls == 1 {
				return errors.New("existing proxy not ready")
			}
			return nil
		}
		defer func() { proxyReadinessFn = origReadiness }()

		canonical, err := CanonicalWorkspacePath(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		wsKey := WorkspaceKey(canonical)
		entry := WorkspaceEntry{
			WorkspaceKey: wsKey, WorkspacePath: canonical, Language: "python",
			Backend: "mcp-language-server", Port: 9242,
			TaskName: LSPTaskNameForWorkspaceLanguage(wsKey, "python"),
		}
		reg := NewRegistry(h.regPath)
		if err := reg.PutLSP(entry); err != nil {
			t.Fatal(err)
		}
		if err := reg.Save(); err != nil {
			t.Fatal(err)
		}
		if _, err := NewAPI().EnsureLSPRegistered(context.Background(), wsKey, canonical, "python"); err != nil {
			t.Fatal(err)
		}
		assertNoWorkspaceRegistered(t, recorder)
	})

	t.Run("new-row", func(t *testing.T) {
		h := newRegisterHarness(t)
		defer h.restore()
		restoreState := SetDaemonStateRootForTest(apitest.HardenedTempDir(t))
		defer restoreState()
		recorder := &recordingAuditWriter{}
		installRecordingAudit(t, recorder)
		origReconcile := registerSupervisorReconcileFn
		registerSupervisorReconcileFn = func(context.Context, bool) (ReconcileResponse, error) {
			return ReconcileResponse{}, nil
		}
		defer func() { registerSupervisorReconcileFn = origReconcile }()
		origReadiness := proxyReadinessFn
		proxyReadinessFn = func(int, time.Duration) error { return nil }
		defer func() { proxyReadinessFn = origReadiness }()

		canonical, err := CanonicalWorkspacePath(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := NewAPI().EnsureLSPRegistered(
			context.Background(),
			WorkspaceKey(canonical),
			canonical,
			"python",
		); err != nil {
			t.Fatal(err)
		}
		assertNoWorkspaceRegistered(t, recorder)
	})
}
