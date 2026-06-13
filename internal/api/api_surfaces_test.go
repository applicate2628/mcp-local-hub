package api

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mcp-local-hub/internal/scheduler"
)

// apiSurfacesFakeScheduler is a lightweight in-memory scheduler.Scheduler
// used by the liveness-task install/remove tests (liveness_task_test.go)
// and the codex_followup tests to drive Delete / ImportXML without
// touching the host's real Task Scheduler. Delete and ImportXML are
// exercised; the other methods return errNotImplementedForTest so
// accidental misuse is loud. (Distinct name from register_test.go's
// `fakeScheduler` to avoid in-package collision.)
type apiSurfacesFakeScheduler struct {
	mu             sync.Mutex
	deleteCalls    []string
	importXMLCalls []importXMLCall
	// deleteErr, when non-nil, is returned by Delete instead of nil.
	deleteErr error
	// importXMLErr, when non-nil, is returned by ImportXML instead of nil.
	importXMLErr error
}

// importXMLCall captures the (name, xml) tuple of a single ImportXML
// invocation so install tests can assert the rendered task name + XML body.
type importXMLCall struct {
	name string
	xml  []byte
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
func (f *apiSurfacesFakeScheduler) ImportXML(name string, xml []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Defensive copy of the xml slice — callers should not mutate.
	cp := make([]byte, len(xml))
	copy(cp, xml)
	f.importXMLCalls = append(f.importXMLCalls, importXMLCall{name: name, xml: cp})
	return f.importXMLErr
}

func (f *apiSurfacesFakeScheduler) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.deleteCalls))
	copy(out, f.deleteCalls)
	return out
}

// importCalls returns a defensive copy of the recorded ImportXML calls.
// Used by the liveness-task install tests to assert task name + XML body.
func (f *apiSurfacesFakeScheduler) importCalls() []importXMLCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]importXMLCall, len(f.importXMLCalls))
	copy(out, f.importXMLCalls)
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

// installTestIntentReader patches the legacy intent-read seam.
//
// Phase 4-E2: IntentStillRunning must NOT consult this seam. Tests that install
// it also redirect the state dir to an empty t.TempDir() so any accidental
// lookupSupervisorStop read stays hermetic and never reaches the developer's
// live supervisor-intent.json.
func installTestIntentReader(t *testing.T, fn func(taskName string) (DaemonIntent, bool, error)) {
	t.Helper()
	// pr301 r5 Finding 1: hardened state root so the absent-intent strict verdict
	// resolves relax=FALSE (mirrors production's owner-only %LOCALAPPDATA%); a
	// plain t.TempDir() on a broadened test host would resolve strict=TRUE.
	restoreRoot := SetDaemonStateRootForTest(hardenedTempDir(t))
	t.Cleanup(restoreRoot)
	orig := readDaemonIntentFn
	readDaemonIntentFn = fn
	t.Cleanup(func() { readDaemonIntentFn = orig })
}

func installTestSupervisorStops(t *testing.T, stops map[string]DaemonIntent) {
	t.Helper()
	// pr301 r5 Finding 1: hardened state root (this helper does a GATED
	// WriteSupervisorIntent seed below, which would refuse a broadened parent
	// once the absent-intent verdict resolves strict). See installTestIntentReader.
	restoreRoot := SetDaemonStateRootForTest(hardenedTempDir(t))
	t.Cleanup(restoreRoot)
	if stops == nil {
		return
	}
	path, err := DefaultSupervisorIntentPath()
	if err != nil {
		t.Fatalf("DefaultSupervisorIntentPath: %v", err)
	}
	if err := WriteSupervisorIntent(path, &SupervisorIntentFile{Version: 1, Stops: stops}); err != nil {
		t.Fatalf("seed supervisor-intent.json: %v", err)
	}
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
// IntentStillRunning
// ---------------------------------------------------------------------------

func TestIntentStillRunning_TrueWhenNoStopIntent(t *testing.T) {
	a := NewAPI()
	// Missing intent file (no entry for task) → not actively stopped → true.
	installTestSupervisorStops(t, nil)
	if !a.IntentStillRunning("\\mcp-local-hub-time-default", time.Now().UTC()) {
		t.Errorf("IntentStillRunning: want true when no intent recorded")
	}
}

func TestIntentStillRunning_AbsentSubBlockIgnoresStaleLegacyStop(t *testing.T) {
	a := NewAPI()
	now := time.Now().UTC()
	var legacyReaderCalled bool
	installTestIntentReader(t, func(taskName string) (DaemonIntent, bool, error) {
		legacyReaderCalled = true
		return DaemonIntent{
			Desired:   IntentDesiredStopped,
			Reason:    IntentReasonUserStop,
			UpdatedAt: now,
		}, true, nil
	})

	if !a.IntentStillRunning("\\mcp-local-hub-time-default", now) {
		t.Fatalf("IntentStillRunning: want true when the sole sub-block source has no stop entry")
	}
	if legacyReaderCalled {
		t.Fatalf("IntentStillRunning consulted stale daemon-intent fallback after absent sub-block entry")
	}
}

func TestIntentStillRunning_FalseWhenUserStop(t *testing.T) {
	a := NewAPI()
	now := time.Now().UTC()
	installTestSupervisorStops(t, map[string]DaemonIntent{
		`\mcp-local-hub-time-default`: {
			Desired:   IntentDesiredStopped,
			Reason:    IntentReasonUserStop,
			UpdatedAt: now.Add(-5 * time.Minute),
		},
	})
	if a.IntentStillRunning("\\mcp-local-hub-time-default", now) {
		t.Errorf("IntentStillRunning: want false during active user-stop")
	}
}

func TestIntentStillRunning_TrueWhenStopExpired(t *testing.T) {
	a := NewAPI()
	now := time.Now().UTC()
	// Stop intent older than TTL → IsActiveStop returns false → still running.
	installTestSupervisorStops(t, map[string]DaemonIntent{
		`\mcp-local-hub-time-default`: {
			Desired:   IntentDesiredStopped,
			Reason:    IntentReasonUserStop,
			UpdatedAt: now.Add(-48 * time.Hour),
		},
	})
	if !a.IntentStillRunning("\\mcp-local-hub-time-default", now) {
		t.Errorf("IntentStillRunning: want true when stop intent past TTL")
	}
}
