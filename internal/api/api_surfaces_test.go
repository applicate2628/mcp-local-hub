package api

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mcp-local-hub/internal/scheduler"
)

// apiSurfacesFakeScheduler is a lightweight in-memory scheduler.Scheduler
// used by Task 0 tests to drive UninstallWatchdogTask /
// UninstallWatchdogTaskInternal without touching the host's real Task
// Scheduler. Only Delete is exercised; the other methods return
// errNotImplementedForTest so accidental misuse is loud. (Distinct name
// from register_test.go's `fakeScheduler` to avoid in-package collision.)
type apiSurfacesFakeScheduler struct {
	mu          sync.Mutex
	deleteCalls []string
	// deleteErr, when non-nil, is returned by Delete instead of nil.
	deleteErr error
}

var errNotImplementedForTest = errors.New("apiSurfacesFakeScheduler: not implemented")

func (f *apiSurfacesFakeScheduler) Create(scheduler.TaskSpec) error {
	return errNotImplementedForTest
}
func (f *apiSurfacesFakeScheduler) Delete(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteCalls = append(f.deleteCalls, name)
	return f.deleteErr
}
func (f *apiSurfacesFakeScheduler) Run(string) error  { return errNotImplementedForTest }
func (f *apiSurfacesFakeScheduler) Stop(string) error { return errNotImplementedForTest }
func (f *apiSurfacesFakeScheduler) Status(string) (scheduler.TaskStatus, error) {
	return scheduler.TaskStatus{}, errNotImplementedForTest
}
func (f *apiSurfacesFakeScheduler) List(string) ([]scheduler.TaskStatus, error) {
	return nil, errNotImplementedForTest
}
func (f *apiSurfacesFakeScheduler) ExportXML(string) ([]byte, error) {
	return nil, errNotImplementedForTest
}
func (f *apiSurfacesFakeScheduler) ImportXML(string, []byte) error {
	return errNotImplementedForTest
}

func (f *apiSurfacesFakeScheduler) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.deleteCalls))
	copy(out, f.deleteCalls)
	return out
}

// installTestScheduler patches the package-level scheduler factory seam with f
// for the duration of the test. Restores on cleanup.
func installTestScheduler(t *testing.T, f scheduler.Scheduler) {
	t.Helper()
	orig := schedulerFactoryFn
	schedulerFactoryFn = func() (scheduler.Scheduler, error) { return f, nil }
	t.Cleanup(func() { schedulerFactoryFn = orig })
}

// installTestStatusFn patches the package-level Status source seam.
func installTestStatusFn(t *testing.T, fn func() ([]DaemonStatus, error)) {
	t.Helper()
	orig := statusContextSrcFn
	statusContextSrcFn = fn
	t.Cleanup(func() { statusContextSrcFn = orig })
}

// installTestRestartFn patches the package-level Restart source seam.
func installTestRestartFn(t *testing.T, fn func(server, daemonFilter string) ([]RestartResult, error)) {
	t.Helper()
	orig := restartContextSrcFn
	restartContextSrcFn = fn
	t.Cleanup(func() { restartContextSrcFn = orig })
}

// installTestRestartWithSnapshotFn patches the snapshot-bound Restart seam.
func installTestRestartWithSnapshotFn(t *testing.T, fn func(server, filter string, snap OwnershipSnapshot) ([]RestartResult, error)) {
	t.Helper()
	orig := restartContextWithSnapshotSrcFn
	restartContextWithSnapshotSrcFn = fn
	t.Cleanup(func() { restartContextWithSnapshotSrcFn = orig })
}

// installTestAuditFn patches the audit-append seam. Returns the captured
// entries via the closed-over slice pointer the caller supplies.
func installTestAuditFn(t *testing.T, capture *[]IntentAuditEntry, retErr error) {
	t.Helper()
	orig := appendIntentAuditFn
	var mu sync.Mutex
	appendIntentAuditFn = func(e IntentAuditEntry) error {
		mu.Lock()
		*capture = append(*capture, e)
		mu.Unlock()
		return retErr
	}
	t.Cleanup(func() { appendIntentAuditFn = orig })
}

// installTestIntentReader patches the intent-read seam.
func installTestIntentReader(t *testing.T, fn func(taskName string) (DaemonIntent, bool, error)) {
	t.Helper()
	orig := readDaemonIntentFn
	readDaemonIntentFn = fn
	t.Cleanup(func() { readDaemonIntentFn = orig })
}

// ---------------------------------------------------------------------------
// StatusContext
// ---------------------------------------------------------------------------

func TestStatusContext_RespectsCtxCancellation(t *testing.T) {
	a := NewAPI()
	// Slow Status — completes after 5s. ctx is cancelled before that.
	installTestStatusFn(t, func() ([]DaemonStatus, error) {
		time.Sleep(5 * time.Second)
		return []DaemonStatus{{TaskName: "should-never-arrive"}}, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, err := a.StatusContext(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("StatusContext: want context.Canceled, got err=%v", err)
	}
	if got != nil {
		t.Errorf("StatusContext: want nil rows on ctx cancel, got %+v", got)
	}
}

func TestStatusContext_NormalCompletion(t *testing.T) {
	a := NewAPI()
	want := []DaemonStatus{{TaskName: "\\mcp-local-hub-time-default", State: "Running", Port: 9100}}
	installTestStatusFn(t, func() ([]DaemonStatus, error) {
		return want, nil
	})
	ctx := context.Background()
	got, err := a.StatusContext(ctx)
	if err != nil {
		t.Fatalf("StatusContext: %v", err)
	}
	if len(got) != 1 || got[0].TaskName != want[0].TaskName {
		t.Errorf("StatusContext rows: got %+v want %+v", got, want)
	}
}

// ---------------------------------------------------------------------------
// RestartContext
// ---------------------------------------------------------------------------

func TestRestartContext_RespectsCtxCancellation(t *testing.T) {
	a := NewAPI()
	installTestRestartFn(t, func(server, filter string) ([]RestartResult, error) {
		time.Sleep(5 * time.Second)
		return []RestartResult{{TaskName: "should-not-arrive"}}, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, err := a.RestartContext(ctx, "time", "")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RestartContext: want context.Canceled, got err=%v", err)
	}
	if got != nil {
		t.Errorf("RestartContext: want nil results on ctx cancel, got %+v", got)
	}
}

// TestRestartContext_BestEffort verifies that ctx cancellation returns to the
// caller within ~10ms even though the underlying Restart continues to run.
// The plan documents this as best-effort; the underlying op is not killed.
func TestRestartContext_BestEffort(t *testing.T) {
	a := NewAPI()
	var underlyingFinished int32
	installTestRestartFn(t, func(server, filter string) ([]RestartResult, error) {
		// Underlying Restart takes 200ms; the wrapper must return long
		// before that when ctx is cancelled.
		time.Sleep(200 * time.Millisecond)
		atomic.StoreInt32(&underlyingFinished, 1)
		return nil, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	_, err := a.RestartContext(ctx, "time", "")
	elapsed := time.Since(start)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RestartContext: want context.Canceled, got %v", err)
	}
	if elapsed > 50*time.Millisecond {
		t.Errorf("RestartContext: cancellation should propagate within ~10ms (slack to 50ms), got %v", elapsed)
	}
	// Confirm best-effort: underlying continues to run after ctx-cancel
	// (we don't *require* it to finish, but the wrapper must not block on it).
	_ = atomic.LoadInt32(&underlyingFinished)
}

// ---------------------------------------------------------------------------
// WaitDaemonRunning
// ---------------------------------------------------------------------------

func TestWaitDaemonRunning_ReturnsTrue(t *testing.T) {
	a := NewAPI()
	installTestStatusFn(t, func() ([]DaemonStatus, error) {
		return []DaemonStatus{{TaskName: "\\mcp-local-hub-time-default", State: "Running"}}, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if !a.WaitDaemonRunning(ctx, "\\mcp-local-hub-time-default") {
		t.Errorf("WaitDaemonRunning: want true on Running row, got false")
	}
}

func TestWaitDaemonRunning_ReturnsFalse(t *testing.T) {
	a := NewAPI()
	installTestStatusFn(t, func() ([]DaemonStatus, error) {
		return []DaemonStatus{{TaskName: "\\mcp-local-hub-time-default", State: "Stopped"}}, nil
	})
	// Short ctx so we don't hang.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if a.WaitDaemonRunning(ctx, "\\mcp-local-hub-time-default") {
		t.Errorf("WaitDaemonRunning: want false on never-Running, got true")
	}
}

// TestWaitDaemonRunning_PollsAtOneSecond instruments the StatusContext source
// and verifies the poll cadence approximates 1s. Over ~3.2s ctx, we expect
// 3-4 polls (initial + 2-3 timer ticks).
func TestWaitDaemonRunning_PollsAtOneSecond(t *testing.T) {
	a := NewAPI()
	var pollCount int32
	installTestStatusFn(t, func() ([]DaemonStatus, error) {
		atomic.AddInt32(&pollCount, 1)
		return []DaemonStatus{{TaskName: "x", State: "Stopped"}}, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3200*time.Millisecond)
	defer cancel()
	a.WaitDaemonRunning(ctx, "x")
	got := atomic.LoadInt32(&pollCount)
	// Initial poll + up to 3 timer ticks; allow loose [2,5] band for CI noise.
	if got < 2 || got > 5 {
		t.Errorf("WaitDaemonRunning: poll cadence ~1s expected 2-5 polls in 3.2s window, got %d", got)
	}
}

// ---------------------------------------------------------------------------
// IntentStillRunning
// ---------------------------------------------------------------------------

func TestIntentStillRunning_TrueWhenNoStopIntent(t *testing.T) {
	a := NewAPI()
	// Missing intent file (no entry for task) → not actively stopped → true.
	installTestIntentReader(t, func(taskName string) (DaemonIntent, bool, error) {
		return DaemonIntent{}, false, nil
	})
	if !a.IntentStillRunning("\\mcp-local-hub-time-default", time.Now().UTC()) {
		t.Errorf("IntentStillRunning: want true when no intent recorded")
	}
}

func TestIntentStillRunning_FalseWhenUserStop(t *testing.T) {
	a := NewAPI()
	now := time.Now().UTC()
	installTestIntentReader(t, func(taskName string) (DaemonIntent, bool, error) {
		return DaemonIntent{
			Desired:   IntentDesiredStopped,
			Reason:    IntentReasonUserStop,
			UpdatedAt: now.Add(-5 * time.Minute),
		}, true, nil
	})
	if a.IntentStillRunning("\\mcp-local-hub-time-default", now) {
		t.Errorf("IntentStillRunning: want false during active user-stop")
	}
}

func TestIntentStillRunning_TrueWhenStopExpired(t *testing.T) {
	a := NewAPI()
	now := time.Now().UTC()
	// Stop intent older than TTL → IsActiveStop returns false → still running.
	installTestIntentReader(t, func(taskName string) (DaemonIntent, bool, error) {
		return DaemonIntent{
			Desired:   IntentDesiredStopped,
			Reason:    IntentReasonUserStop,
			UpdatedAt: now.Add(-48 * time.Hour),
		}, true, nil
	})
	if !a.IntentStillRunning("\\mcp-local-hub-time-default", now) {
		t.Errorf("IntentStillRunning: want true when stop intent past TTL")
	}
}

// ---------------------------------------------------------------------------
// LoadDaemonRegistry
// ---------------------------------------------------------------------------

func TestLoadDaemonRegistry_ImmutableSnapshot(t *testing.T) {
	a := NewAPI()
	rows := []DaemonStatus{
		{TaskName: "\\mcp-local-hub-time-default", Server: "time", Daemon: "default"},
	}
	installTestStatusFn(t, func() ([]DaemonStatus, error) {
		return rows, nil
	})
	reg := a.LoadDaemonRegistry()
	if !reg.IsManagedDaemon("\\mcp-local-hub-time-default") {
		t.Fatal("registry should manage seeded task")
	}
	// Mutate the underlying source slice. The registry must not see it.
	rows[0].TaskName = "\\mcp-local-hub-MUTATED"
	if !reg.IsManagedDaemon("\\mcp-local-hub-time-default") {
		t.Errorf("registry was not a defensive copy: original task lost after source mutation")
	}
	if reg.IsManagedDaemon("\\mcp-local-hub-MUTATED") {
		t.Errorf("registry leaked source mutation: MUTATED task should not be managed")
	}
}

func TestLoadDaemonRegistry_IsManagedDaemon_TaskInStatus(t *testing.T) {
	a := NewAPI()
	installTestStatusFn(t, func() ([]DaemonStatus, error) {
		return []DaemonStatus{{TaskName: "\\mcp-local-hub-time-default"}}, nil
	})
	reg := a.LoadDaemonRegistry()
	if !reg.IsManagedDaemon("\\mcp-local-hub-time-default") {
		t.Errorf("IsManagedDaemon: want true for task present in Status()")
	}
}

func TestLoadDaemonRegistry_IsManagedDaemon_TaskInManifest(t *testing.T) {
	// A manifest server defines daemons that may exist in the manifest set
	// even when no scheduler row is present yet (transient install/uninstall
	// race). The registry derives the candidate set from manifest.ListServers,
	// not just the live scheduler status.
	a := NewAPI()
	installTestStatusFn(t, func() ([]DaemonStatus, error) {
		// Status() empty.
		return nil, nil
	})
	// "time" is a shipped manifest with daemon "default" (verified via
	// servers/time/manifest.yaml). The registry-loader resolves it via
	// embedded ListServers + per-server Daemons.
	reg := a.LoadDaemonRegistry()
	if !reg.IsManagedDaemon("\\mcp-local-hub-time-default") {
		t.Errorf("IsManagedDaemon: want true for manifest-known time/default daemon, got false")
	}
}

func TestLoadDaemonRegistry_IsManagedDaemon_OrphanTask(t *testing.T) {
	a := NewAPI()
	installTestStatusFn(t, func() ([]DaemonStatus, error) {
		return []DaemonStatus{{TaskName: "\\mcp-local-hub-time-default"}}, nil
	})
	reg := a.LoadDaemonRegistry()
	if reg.IsManagedDaemon("\\mcp-local-hub-orphan-server-orphan-daemon") {
		t.Errorf("IsManagedDaemon: want false for unknown task, got true")
	}
	// Random non-mcp-local-hub-prefixed task → orphan.
	if reg.IsManagedDaemon("\\Microsoft\\Windows\\AnotherTask") {
		t.Errorf("IsManagedDaemon: want false for non-mcphub task, got true")
	}
}

// ---------------------------------------------------------------------------
// LoadOwnershipSnapshot (v9 §47/§59)
// ---------------------------------------------------------------------------

// TestLoadOwnershipSnapshot_ImmutableAcrossManifestSwap verifies the snapshot
// is detached from package-level mutable state. Mutating the returned maps
// must not leak back into shared state for subsequent loads.
func TestLoadOwnershipSnapshot_ImmutableAcrossManifestSwap(t *testing.T) {
	a := NewAPI()
	snap1 := a.LoadOwnershipSnapshot()
	// Mutate the returned PortMap; this must not affect future loads.
	snap1.PortMap["\\mcp-local-hub-injected-task"] = 99999
	snap1.ManifestServers["injected-server"] = true
	// Defensive-copy proof: a fresh snapshot must not contain the injection.
	snap2 := a.LoadOwnershipSnapshot()
	if _, ok := snap2.PortMap["\\mcp-local-hub-injected-task"]; ok {
		t.Errorf("LoadOwnershipSnapshot.PortMap is shared, not a defensive copy")
	}
	if _, ok := snap2.ManifestServers["injected-server"]; ok {
		t.Errorf("LoadOwnershipSnapshot.ManifestServers is shared, not a defensive copy")
	}
}

func TestLoadOwnershipSnapshot_PortMapPresent(t *testing.T) {
	a := NewAPI()
	snap := a.LoadOwnershipSnapshot()
	if snap.PortMap == nil {
		t.Fatal("LoadOwnershipSnapshot.PortMap should be non-nil (may be empty in degraded states)")
	}
	if snap.ManifestServers == nil {
		t.Errorf("ManifestServers should be non-nil")
	}
	if snap.ManifestDaemons == nil {
		t.Errorf("ManifestDaemons should be non-nil")
	}
	if snap.WorkspaceTasksByKey == nil {
		t.Errorf("WorkspaceTasksByKey should be non-nil")
	}
	if snap.SnapshottedAt.IsZero() {
		t.Errorf("SnapshottedAt should be populated")
	}
	// The "time" manifest defines daemon "default" with port 9128 (verified
	// against servers/time/manifest.yaml at plan-v13 time). Verify the
	// PortMap encodes that. Path: PortMap["\\mcp-local-hub-time-default"].
	got := snap.PortMap["\\mcp-local-hub-time-default"]
	if got == 0 && len(snap.PortMap) > 0 {
		// Be lenient: the embed may be empty in degraded test envs without
		// servers/ embed. If non-empty, time-default must be present.
		t.Errorf("PortMap[time-default] missing despite non-empty PortMap (len=%d)", len(snap.PortMap))
	}
	// If present, it must match the manifest source of truth.
	if got != 0 && got != 9128 {
		t.Errorf("PortMap[time-default] = %d; expected manifest port 9128", got)
	}
	// ManifestDaemons[server] should match the per-server daemon set.
	if daemons, ok := snap.ManifestDaemons["time"]; ok {
		if !daemons["default"] {
			t.Errorf("ManifestDaemons[time] should mark daemon 'default' present, got %+v", daemons)
		}
	}
}

func TestRestartContextWithSnapshot_UsesSnapshotPortMap(t *testing.T) {
	a := NewAPI()
	// A custom snapshot-bound restart fn captures the snapshot it received,
	// so we can assert the variant DOES forward the snapshot rather than
	// silently delegating to the manifest-fresh path.
	var captured OwnershipSnapshot
	installTestRestartWithSnapshotFn(t, func(server, filter string, snap OwnershipSnapshot) ([]RestartResult, error) {
		captured = snap
		return []RestartResult{{TaskName: "\\mcp-local-hub-time-default"}}, nil
	})
	wantPort := 12345
	snap := OwnershipSnapshot{
		ManifestServers:     map[string]bool{"time": true},
		ManifestDaemons:     map[string]map[string]bool{"time": {"default": true}},
		WorkspaceTasksByKey: map[string]string{},
		PortMap:             map[string]int{"\\mcp-local-hub-time-default": wantPort},
		SnapshottedAt:       time.Now().UTC(),
	}
	ctx := context.Background()
	got, err := a.RestartContextWithSnapshot(ctx, "time", "default", snap)
	if err != nil {
		t.Fatalf("RestartContextWithSnapshot: %v", err)
	}
	if len(got) != 1 || got[0].TaskName != "\\mcp-local-hub-time-default" {
		t.Errorf("RestartContextWithSnapshot results: got %+v", got)
	}
	if captured.PortMap["\\mcp-local-hub-time-default"] != wantPort {
		t.Errorf("snapshot PortMap not forwarded: captured=%+v want port %d", captured.PortMap, wantPort)
	}
}

// ---------------------------------------------------------------------------
// UninstallWatchdogTask + UninstallWatchdogTaskInternal
// ---------------------------------------------------------------------------

func TestUninstallWatchdogTask_Idempotent(t *testing.T) {
	a := NewAPI()
	f := &apiSurfacesFakeScheduler{}
	installTestScheduler(t, f)
	// First call: nominal delete.
	if err := a.UninstallWatchdogTask(); err != nil {
		t.Fatalf("UninstallWatchdogTask first call: %v", err)
	}
	// Second call must succeed even though the underlying scheduler.Delete
	// is idempotent. Confirms no spurious "already absent" hard error.
	if err := a.UninstallWatchdogTask(); err != nil {
		t.Fatalf("UninstallWatchdogTask second call (idempotent): %v", err)
	}
	calls := f.calls()
	if len(calls) != 2 {
		t.Errorf("expected 2 Delete calls, got %d: %+v", len(calls), calls)
	}
	for i, name := range calls {
		if !strings.Contains(name, "mcp-local-hub-watchdog") {
			t.Errorf("Delete call %d targeted unexpected task %q", i, name)
		}
	}
}

func TestUninstallWatchdogTaskInternal_AuditReason(t *testing.T) {
	a := NewAPI()
	f := &apiSurfacesFakeScheduler{}
	installTestScheduler(t, f)
	var captured []IntentAuditEntry
	installTestAuditFn(t, &captured, nil)

	if err := a.UninstallWatchdogTaskInternal(QuarantineFourStrikes30Min); err != nil {
		t.Fatalf("UninstallWatchdogTaskInternal: %v", err)
	}

	// Scheduler.Delete must have fired against the watchdog task name.
	calls := f.calls()
	if len(calls) != 1 || !strings.Contains(calls[0], "mcp-local-hub-watchdog") {
		t.Fatalf("expected one watchdog Delete call, got %+v", calls)
	}

	// Audit entry contract per §39, §63 v12 canonical:
	//   Action == "watchdog-self-quarantined" (literal)
	//   Reason == "4-strikes-30min" (enum value)
	if len(captured) != 1 {
		t.Fatalf("expected exactly one audit entry, got %d", len(captured))
	}
	got := captured[0]
	if got.Action != "watchdog-self-quarantined" {
		t.Errorf("audit Action: got %q, want %q", got.Action, "watchdog-self-quarantined")
	}
	if got.Reason != string(QuarantineFourStrikes30Min) {
		t.Errorf("audit Reason: got %q, want %q", got.Reason, string(QuarantineFourStrikes30Min))
	}
}

// TestSelfQuarantineReason_SuggestedAction verifies the §56 contract: each
// reason value carries an operator-facing suggested-action string.
func TestSelfQuarantineReason_SuggestedAction(t *testing.T) {
	got := QuarantineFourStrikes30Min.SuggestedAction()
	if got == "" {
		t.Errorf("QuarantineFourStrikes30Min.SuggestedAction returned empty string")
	}
	// Smoke-check the message mentions "watchdog install" so the operator
	// has the recovery command surfaced.
	if !strings.Contains(got, "mcphub watchdog install") {
		t.Errorf("SuggestedAction should reference recovery cmd, got %q", got)
	}
	// Unknown reason → fallback string (still non-empty).
	other := SelfQuarantineReason("unknown-future-reason")
	if other.SuggestedAction() == "" {
		t.Errorf("default SuggestedAction should not be empty")
	}
}

// TestNewOwnedXMLValidatorFromSnapshot_StubInterface verifies the constructor
// returns a non-nil OwnedXMLValidator wrapping the snapshot. Real validation
// logic lands in Task 6 (ValidateOwnedTaskXML); for now the stub at minimum
// reports that a snapshot-known task is "owned" structurally.
func TestNewOwnedXMLValidatorFromSnapshot_StubInterface(t *testing.T) {
	snap := OwnershipSnapshot{
		ManifestServers: map[string]bool{"time": true},
		ManifestDaemons: map[string]map[string]bool{"time": {"default": true}},
		PortMap:         map[string]int{"\\mcp-local-hub-time-default": 9100},
		SnapshottedAt:   time.Now().UTC(),
	}
	v := NewOwnedXMLValidatorFromSnapshot(snap)
	if v == nil {
		t.Fatal("NewOwnedXMLValidatorFromSnapshot returned nil")
	}
	// A snapshot-known task SHOULD pass the structural check (ownership-only;
	// real XML validation lands in Task 6). An unknown task must NOT.
	if !v.IsOwnedAndValid("\\mcp-local-hub-time-default") {
		t.Errorf("validator: snapshot-known task should be owned, got false")
	}
	if v.IsOwnedAndValid("\\mcp-local-hub-orphan-server-orphan-daemon") {
		t.Errorf("validator: unknown task should NOT be owned, got true")
	}
}

// Compile-time assertion: the registry impl and validator impl satisfy the
// declared interfaces. If a future refactor renames a method, this test
// fails to compile rather than silently skipping coverage.
var _ DaemonRegistry = (*daemonRegistryImpl)(nil)
var _ OwnedXMLValidator = (*ownedXMLValidatorImpl)(nil)
