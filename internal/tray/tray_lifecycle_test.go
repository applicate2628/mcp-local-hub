package tray

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type testLifecycleClock struct {
	mu     sync.Mutex
	now    time.Time
	waitFn func(context.Context, time.Duration) bool
}

func (c *testLifecycleClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testLifecycleClock) Wait(ctx context.Context, d time.Duration) bool {
	if c.waitFn != nil {
		return c.waitFn(ctx, d)
	}
	select {
	case <-ctx.Done():
		return false
	default:
		c.mu.Lock()
		c.now = c.now.Add(d)
		c.mu.Unlock()
		return true
	}
}

func (c *testLifecycleClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

type recordingWriteCloser struct {
	mu       sync.Mutex
	buf      bytes.Buffer
	lines    chan string
	closeOne sync.Once
	onClose  func()
}

func newRecordingWriteCloser(onClose func()) *recordingWriteCloser {
	return &recordingWriteCloser{lines: make(chan string, 16), onClose: onClose}
}

func (w *recordingWriteCloser) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.buf.Write(p)
	for {
		line, readErr := w.buf.ReadString('\n')
		if readErr != nil {
			_, _ = w.buf.WriteString(line)
			break
		}
		w.lines <- line
	}
	return n, err
}

func (w *recordingWriteCloser) Close() error {
	w.closeOne.Do(func() {
		if w.onClose != nil {
			w.onClose()
		}
	})
	return nil
}

type controlledAttempt struct {
	attempt *childAttempt
	stdin   *recordingWriteCloser
	stdoutW *io.PipeWriter
	exitCh  chan error
	done    chan struct{}
	one     sync.Once

	mu        sync.Mutex
	waitCalls int
	events    []string
}

func newControlledAttempt(exitOnStdinClose bool) *controlledAttempt {
	a := &controlledAttempt{exitCh: make(chan error, 1), done: make(chan struct{})}
	a.stdin = newRecordingWriteCloser(func() {
		a.record("close")
		if exitOnStdinClose {
			a.finish(nil)
		}
	})
	stdoutR, stdoutW := io.Pipe()
	a.stdoutW = stdoutW
	a.attempt = &childAttempt{
		stdin:  a.stdin,
		stdout: stdoutR,
		wait: func() error {
			a.mu.Lock()
			a.waitCalls++
			a.mu.Unlock()
			return <-a.exitCh
		},
		kill: func() error {
			a.record("kill")
			a.finish(errors.New("killed"))
			return nil
		},
	}
	return a
}

func (a *controlledAttempt) record(event string) {
	a.mu.Lock()
	a.events = append(a.events, event)
	a.mu.Unlock()
}

func (a *controlledAttempt) finish(err error) {
	a.one.Do(func() {
		_ = a.stdoutW.Close()
		a.exitCh <- err
		close(a.done)
	})
}

func (a *controlledAttempt) snapshot() (int, []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.waitCalls, append([]string(nil), a.events...)
}

func TestRunRetriesWithExactBackoffAndThirtySecondReset(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stateCh := make(chan TrayState)
	clock := &testLifecycleClock{now: time.Unix(1, 0)}
	var mu sync.Mutex
	var waits []time.Duration
	var stableChild *controlledAttempt
	var launchCount atomic.Int32
	clock.waitFn = func(ctx context.Context, d time.Duration) bool {
		if d == lifecycleStableAfter {
			clock.advance(d)
			stableChild.finish(nil)
			return true
		}
		currentLaunch := launchCount.Load()
		if currentLaunch <= 6 || (currentLaunch == 7 && d == 100*time.Millisecond) {
			mu.Lock()
			waits = append(waits, d)
			mu.Unlock()
		}
		clock.advance(d)
		return true
	}

	launches := 0
	launch := func() (*childAttempt, lifecyclePhase, error) {
		launches++
		launchCount.Store(int32(launches))
		if launches <= 6 {
			return nil, lifecyclePhaseSpawn, errors.New("not started")
		}
		if launches == 7 {
			stableChild = newControlledAttempt(false)
			return stableChild.attempt, "", nil
		}
		cancel()
		return newControlledAttempt(true).attempt, "", nil
	}

	if err := runLifecycle(ctx, Config{StateCh: stateCh}, lifecycleDeps{launch: launch, clock: clock}); err != nil {
		t.Fatalf("runLifecycle: %v", err)
	}
	mu.Lock()
	got := append([]time.Duration(nil), waits...)
	mu.Unlock()
	want := []time.Duration{100 * time.Millisecond, 250 * time.Millisecond, 500 * time.Millisecond, time.Second, 2 * time.Second, 2 * time.Second, 100 * time.Millisecond}
	if len(got) != len(want) {
		t.Fatalf("retry waits = %v, want exactly %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("retry wait[%d] = %v, want %v (all=%v)", i, got[i], want[i], got)
		}
	}
}

func TestRunShortLivedFailureDoesNotResetBackoffDuringSettlement(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clock := &testLifecycleClock{now: time.Unix(1, 0)}
	var launchCount atomic.Int32
	var settlementWaits atomic.Int32
	var retryWaits []time.Duration
	clock.waitFn = func(ctx context.Context, d time.Duration) bool {
		if d == lifecycleStableAfter {
			<-ctx.Done()
			return false
		}
		if launchCount.Load() == 6 && d == lifecycleShutdownTimeout && settlementWaits.Add(1) == 1 {
			clock.advance(d)
			return true
		}
		if launchCount.Load() <= 6 {
			retryWaits = append(retryWaits, d)
		}
		clock.advance(d)
		return true
	}

	shortLived := newControlledAttempt(false)
	launches := 0
	var reports []LifecycleReport
	launch := func() (*childAttempt, lifecyclePhase, error) {
		launches++
		launchCount.Store(int32(launches))
		if launches <= 5 {
			return nil, lifecyclePhaseSpawn, errors.New("not started")
		}
		if launches == 6 {
			clock.advance(29 * time.Second)
			_ = shortLived.stdoutW.Close()
			return shortLived.attempt, "", nil
		}
		cancel()
		return newControlledAttempt(true).attempt, "", nil
	}
	if err := runLifecycle(ctx, Config{LifecycleReport: func(report LifecycleReport) {
		reports = append(reports, report)
	}}, lifecycleDeps{launch: launch, clock: clock}); err != nil {
		t.Fatalf("runLifecycle: %v", err)
	}
	want := []time.Duration{100 * time.Millisecond, 250 * time.Millisecond, 500 * time.Millisecond, time.Second, 2 * time.Second, 2 * time.Second}
	if len(retryWaits) != len(want) {
		t.Fatalf("retry waits = %v, want %v", retryWaits, want)
	}
	for i := range want {
		if retryWaits[i] != want[i] {
			t.Fatalf("retry wait[%d] = %v, want %v", i, retryWaits[i], want[i])
		}
	}
	for _, report := range reports {
		if report.Type == lifecycleEventStable {
			t.Fatalf("29s child plus 2s settlement was misclassified stable: %#v", report)
		}
	}
}

func TestRunReplaysLatestSnapshotAfterChildAbsence(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stateCh := make(chan TrayState)
	relaxCh := make(chan bool)
	backoffEntered := make(chan struct{})
	releaseBackoff := make(chan struct{})
	var backoffEnteredOnce sync.Once
	clock := &testLifecycleClock{now: time.Unix(1, 0)}
	clock.waitFn = func(ctx context.Context, d time.Duration) bool {
		if d == lifecycleStableAfter {
			<-ctx.Done()
			return false
		}
		backoffEnteredOnce.Do(func() { close(backoffEntered) })
		select {
		case <-releaseBackoff:
			clock.advance(d)
			return true
		case <-ctx.Done():
			return false
		}
	}

	child := newControlledAttempt(true)
	launches := 0
	launch := func() (*childAttempt, lifecyclePhase, error) {
		launches++
		if launches == 1 {
			return nil, lifecyclePhaseSpawn, errors.New("offline")
		}
		return child.attempt, "", nil
	}
	done := make(chan error, 1)
	go func() {
		done <- runLifecycle(ctx, Config{StateCh: stateCh, StateReadRelaxCh: relaxCh}, lifecycleDeps{launch: launch, clock: clock})
	}()
	<-backoffEntered
	stateCh <- StateHealthy
	stateCh <- StateDown
	relaxCh <- false
	relaxCh <- true
	close(releaseBackoff)

	select {
	case line := <-child.stdin.lines:
		if line != "{\"state\":\"down\",\"state_read_relax\":true}\n" {
			t.Fatalf("first state line = %q", line)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("replacement child did not receive an initial snapshot")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("runLifecycle: %v", err)
	}
}

func TestRunRetriesZeroExitWhileContextAlive(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clock := &testLifecycleClock{now: time.Unix(1, 0)}
	var waits []time.Duration
	clock.waitFn = func(ctx context.Context, d time.Duration) bool {
		if d == lifecycleStableAfter {
			<-ctx.Done()
			return false
		}
		if d == lifecycleShutdownTimeout {
			return false
		}
		waits = append(waits, d)
		return true
	}
	first := newControlledAttempt(false)
	first.finish(nil)
	launches := 0
	launch := func() (*childAttempt, lifecyclePhase, error) {
		launches++
		if launches == 1 {
			return first.attempt, "", nil
		}
		cancel()
		return newControlledAttempt(true).attempt, "", nil
	}
	if err := runLifecycle(ctx, Config{}, lifecycleDeps{launch: launch, clock: clock}); err != nil {
		t.Fatalf("runLifecycle: %v", err)
	}
	if launches != 2 || len(waits) != 1 || waits[0] != 100*time.Millisecond {
		t.Fatalf("launches=%d waits=%v, want 2 launches and exact retries [100ms]", launches, waits)
	}
	waitCalls, _ := first.snapshot()
	if waitCalls != 1 {
		t.Fatalf("first child Wait calls = %d, want 1", waitCalls)
	}
}

type cancelEOFReader struct {
	cancel context.CancelFunc
	once   sync.Once
}

func (r *cancelEOFReader) Read([]byte) (int, error) {
	r.once.Do(r.cancel)
	return 0, io.EOF
}

func TestRunCancellationAlreadyWonDoesNotReportFailure(t *testing.T) {
	t.Run("launch failure", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		var reports []LifecycleReport
		clock := &testLifecycleClock{now: time.Unix(1, 0), waitFn: func(context.Context, time.Duration) bool {
			t.Fatal("canceled launch failure must not enter backoff")
			return false
		}}
		err := runLifecycle(ctx, Config{LifecycleReport: func(report LifecycleReport) {
			reports = append(reports, report)
		}}, lifecycleDeps{launch: func() (*childAttempt, lifecyclePhase, error) {
			cancel()
			return nil, lifecyclePhaseSpawn, errors.New("canceled launch")
		}, clock: clock})
		if err != nil {
			t.Fatalf("runLifecycle: %v", err)
		}
		if len(reports) != 0 {
			t.Fatalf("reports after cancellation won = %#v, want none", reports)
		}
	})

	t.Run("child exit", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		var reports []LifecycleReport
		stdin := newRecordingWriteCloser(nil)
		attempt := &childAttempt{
			stdin:  stdin,
			stdout: io.NopCloser(&cancelEOFReader{cancel: cancel}),
			wait:   func() error { return nil },
			kill:   func() error { return nil },
		}
		clock := &testLifecycleClock{now: time.Unix(1, 0), waitFn: func(ctx context.Context, _ time.Duration) bool {
			<-ctx.Done()
			return false
		}}
		err := runLifecycle(ctx, Config{LifecycleReport: func(report LifecycleReport) {
			reports = append(reports, report)
		}}, lifecycleDeps{launch: func() (*childAttempt, lifecyclePhase, error) {
			return attempt, "", nil
		}, clock: clock})
		if err != nil {
			t.Fatalf("runLifecycle: %v", err)
		}
		if len(reports) != 0 {
			t.Fatalf("reports after child-exit cancellation won = %#v, want none", reports)
		}
	})
}

func TestRunMalformedEventReportsRateLimitedProtocolWarning(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clock := &testLifecycleClock{now: time.Unix(1, 0), waitFn: func(ctx context.Context, _ time.Duration) bool {
		<-ctx.Done()
		return false
	}}
	child := newControlledAttempt(true)
	reportCh := make(chan LifecycleReport, 4)
	done := make(chan error, 1)
	go func() {
		done <- runLifecycle(ctx, Config{LifecycleReport: func(report LifecycleReport) {
			reportCh <- report
		}}, lifecycleDeps{launch: func() (*childAttempt, lifecyclePhase, error) {
			return child.attempt, "", nil
		}, clock: clock})
	}()

	writeMalformed := func(line string) {
		t.Helper()
		if _, err := io.WriteString(child.stdoutW, line+"\n"); err != nil {
			t.Fatalf("write malformed child event: %v", err)
		}
	}
	writeMalformed(`{"event":`)
	first := <-reportCh
	writeMalformed(`not-json-with-private-payload`)
	clock.advance(lifecycleSummaryInterval)
	writeMalformed(`{"event":]`)
	second := <-reportCh
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("runLifecycle: %v", err)
	}
	select {
	case extra := <-reportCh:
		t.Fatalf("extra protocol warning inside rate window: %#v", extra)
	default:
	}
	for i, report := range []LifecycleReport{first, second} {
		if report.Type != "tray-child-protocol-warning" || report.Attempt != 1 || report.Phase != "event-json-invalid" {
			t.Fatalf("report[%d] = %#v", i, report)
		}
		if report.ErrorDetail != "" {
			t.Fatalf("report[%d] disclosed child payload/detail: %q", i, report.ErrorDetail)
		}
	}
}

func TestRunCancellationSettlesAttemptAndJoins(t *testing.T) {
	t.Run("during backoff", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		clock := &testLifecycleClock{now: time.Unix(1, 0)}
		entered := make(chan struct{})
		clock.waitFn = func(ctx context.Context, d time.Duration) bool {
			close(entered)
			<-ctx.Done()
			return false
		}
		launches := 0
		done := make(chan error, 1)
		go func() {
			done <- runLifecycle(ctx, Config{}, lifecycleDeps{launch: func() (*childAttempt, lifecyclePhase, error) {
				launches++
				return nil, lifecyclePhaseSpawn, errors.New("offline")
			}, clock: clock})
		}()
		<-entered
		cancel()
		if err := <-done; err != nil {
			t.Fatalf("runLifecycle: %v", err)
		}
		if launches != 1 {
			t.Fatalf("launches after cancellation = %d, want 1", launches)
		}
	})

	t.Run("hung child closes gracefully before owned kill", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		clock := &testLifecycleClock{now: time.Unix(1, 0)}
		clock.waitFn = func(ctx context.Context, d time.Duration) bool {
			if d == lifecycleShutdownTimeout {
				return true
			}
			<-ctx.Done()
			return false
		}
		child := newControlledAttempt(false)
		launched := make(chan struct{})
		var launchedOnce sync.Once
		done := make(chan error, 1)
		go func() {
			done <- runLifecycle(ctx, Config{}, lifecycleDeps{launch: func() (*childAttempt, lifecyclePhase, error) {
				launchedOnce.Do(func() { close(launched) })
				return child.attempt, "", nil
			}, clock: clock})
		}()
		<-launched
		cancel()
		if err := <-done; err != nil {
			t.Fatalf("runLifecycle: %v", err)
		}
		waitCalls, events := child.snapshot()
		if waitCalls != 1 {
			t.Fatalf("Wait calls = %d, want 1", waitCalls)
		}
		if strings.Join(events, ",") != "close,kill" {
			t.Fatalf("settlement order = %v, want [close kill]", events)
		}
	})
}

func TestRunMainStateCloseIsTerminalButRelaxCloseIsNot(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stateCh := make(chan TrayState)
	relaxCh := make(chan bool)
	clock := &testLifecycleClock{now: time.Unix(1, 0)}
	clock.waitFn = func(ctx context.Context, d time.Duration) bool {
		if d == lifecycleStableAfter {
			<-ctx.Done()
			return false
		}
		return true
	}
	first := newControlledAttempt(false)
	second := newControlledAttempt(true)
	launches := 0
	launch := func() (*childAttempt, lifecyclePhase, error) {
		launches++
		if launches == 1 {
			return first.attempt, "", nil
		}
		return second.attempt, "", nil
	}
	done := make(chan error, 1)
	go func() {
		done <- runLifecycle(ctx, Config{StateCh: stateCh, StateReadRelaxCh: relaxCh}, lifecycleDeps{launch: launch, clock: clock})
	}()
	relaxCh <- true
	close(relaxCh)
	first.finish(nil)
	select {
	case line := <-second.stdin.lines:
		if line != "{\"state_read_relax\":true}\n" {
			t.Fatalf("retained relax snapshot = %q", line)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("relax channel closure stopped retries")
	}
	close(stateCh)
	if err := <-done; err != nil {
		t.Fatalf("runLifecycle: %v", err)
	}
	if launches != 2 {
		t.Fatalf("launches after main state closure = %d, want 2", launches)
	}
}

func TestRunLifecycleDiagnosticsAreRateLimitedAndClassified(t *testing.T) {
	fixtureRoot := t.TempDir()
	selfPath := filepath.Join(fixtureRoot, "secret", "mcphub.exe")
	spawnPath := filepath.Join(fixtureRoot, "private", "mcphub.exe")
	pipePath := filepath.Join(fixtureRoot, "hidden", "pipe")
	for _, path := range []string{selfPath, spawnPath, pipePath} {
		if !filepath.IsAbs(path) {
			t.Fatalf("diagnostic fixture path is not absolute: %q", path)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clock := &testLifecycleClock{now: time.Unix(1, 0)}
	clock.waitFn = func(ctx context.Context, d time.Duration) bool {
		if d == lifecycleStableAfter {
			clock.advance(d)
			return true
		}
		clock.advance(d)
		return true
	}
	var reports []LifecycleReport
	launches := 0
	launch := func() (*childAttempt, lifecyclePhase, error) {
		launches++
		switch launches {
		case 1:
			return nil, lifecyclePhaseSelfPath, fmt.Errorf("open %s: access denied", selfPath)
		case 2:
			clock.advance(30 * time.Second)
			return nil, lifecyclePhaseSpawn, fmt.Errorf("spawn %s failed", spawnPath)
		case 3:
			return nil, lifecyclePhaseStdoutPipe, fmt.Errorf("read %s failed", pipePath)
		default:
			return newControlledAttempt(true).attempt, "", nil
		}
	}
	if err := runLifecycle(ctx, Config{LifecycleReport: func(r LifecycleReport) {
		reports = append(reports, r)
		if r.Type == lifecycleEventStable {
			cancel()
		}
	}}, lifecycleDeps{launch: launch, clock: clock}); err != nil {
		t.Fatalf("runLifecycle: %v", err)
	}
	if len(reports) != 3 {
		t.Fatalf("reports = %#v, want unavailable + one summary + stable", reports)
	}
	if reports[0].Type != lifecycleEventUnavailable || reports[1].Type != lifecycleEventRetrySummary || reports[2].Type != lifecycleEventStable {
		t.Fatalf("report types = %q, %q, %q", reports[0].Type, reports[1].Type, reports[2].Type)
	}
	for _, report := range reports {
		if len(report.ErrorDetail) > 512 || strings.Contains(report.ErrorDetail, fixtureRoot) || strings.Contains(report.ErrorDetail, "secret") || strings.Contains(report.ErrorDetail, "private") || strings.Contains(report.ErrorDetail, "hidden") {
			t.Fatalf("unsafe diagnostic detail: %q", report.ErrorDetail)
		}
	}
}
