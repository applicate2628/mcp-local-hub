package gui

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
)

type hubRestartTestEvent struct {
	level  string
	event  string
	fields map[string]any
}

func liveRestartTestComp(port int) *HubListenerComponents {
	comp := &HubListenerComponents{port: port}
	comp.alive.Store(true)
	return comp
}

func waitRestartTestEvent(t *testing.T, ch <-chan hubRestartTestEvent, want string) hubRestartTestEvent {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev := <-ch:
			if ev.event == want {
				return ev
			}
		case <-deadline:
			t.Fatalf("timed out waiting for event %q", want)
		}
	}
}

func waitRestartDriverDone(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("restart driver did not stop")
	}
}

func waitHubRestartTestLogEvent(t *testing.T, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		events, err := api.RecentHubMcpEvents(16)
		if err == nil {
			for _, ev := range events {
				if ev["event"] == want {
					return
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for hub-mcp log event %q", want)
}

func TestHubListenerRestartDriverRestartsAndPublishesNewBundle(t *testing.T) {
	stateDir := t.TempDir()
	restoreState := api.SetDaemonStateRootForTest(stateDir)
	t.Cleanup(restoreState)
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", stateDir)
	if _, err := api.EnsureHubEndpoint(3439, 111); err != nil {
		t.Fatalf("EnsureHubEndpoint before restart: %v", err)
	}

	s := NewServer(Config{Port: 0})
	oldComp := liveRestartTestComp(3439)
	newComp := liveRestartTestComp(3439)
	s.hubMcpComp.Store(oldComp)

	var shutdowns []*HubListenerComponents
	var starts int
	var sleeps []time.Duration
	events := make(chan hubRestartTestEvent, 8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})

	go func() {
		defer close(done)
		runHubListenerRestartDriver(ctx, s, hubListenerRestartDriverOptions{
			startFn: func(context.Context) (*HubListenerComponents, error) {
				starts++
				return newComp, nil
			},
			shutdownFn: func(_ context.Context, comp *HubListenerComponents) {
				shutdowns = append(shutdowns, comp)
			},
			emitFn: func(level, event string, fields map[string]any) error {
				events <- hubRestartTestEvent{level: level, event: event, fields: fields}
				return nil
			},
			sleepFn: func(_ context.Context, d time.Duration) bool {
				sleeps = append(sleeps, d)
				return true
			},
			nowFn: func() time.Time { return time.Unix(100, 0) },
		})
	}()

	s.signalHubListenerRestart()
	ev := waitRestartTestEvent(t, events, "hub-listener-restarted")
	cancel()
	waitRestartDriverDone(t, done)

	if starts != 1 {
		t.Fatalf("start calls = %d, want 1", starts)
	}
	if !reflect.DeepEqual(shutdowns, []*HubListenerComponents{oldComp}) {
		t.Fatalf("shutdowns = %#v, want old component once", shutdowns)
	}
	if got := s.hubMcpComp.Load(); got != newComp {
		t.Fatalf("published component = %#v, want new component %#v", got, newComp)
	}
	if got, want := newComp.port, oldComp.port; got != want {
		t.Fatalf("new port = %d, want preserved old port %d", got, want)
	}
	if ev.level != "info" {
		t.Fatalf("restart event level = %q, want info", ev.level)
	}
	if ev.fields["port"] != 3439 {
		t.Fatalf("restart event port = %v, want 3439", ev.fields["port"])
	}
	if ev.fields["attempt"] != 1 {
		t.Fatalf("restart event attempt = %v, want 1", ev.fields["attempt"])
	}
	if ev.fields["instance_id_preserved"] != true {
		t.Fatalf("restart event instance_id_preserved = %v, want true", ev.fields["instance_id_preserved"])
	}
	if len(sleeps) != 0 {
		t.Fatalf("backoff sleeps before first recovery = %d, want 0", len(sleeps))
	}
}

func TestHubListenerRestartDriverBackoffAndExhaustion(t *testing.T) {
	s := NewServer(Config{Port: 0})
	s.hubMcpComp.Store(liveRestartTestComp(3439))

	var starts int
	var shutdowns int
	var sleeps []time.Duration
	var failed int
	events := make(chan hubRestartTestEvent, 16)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		defer close(done)
		runHubListenerRestartDriver(ctx, s, hubListenerRestartDriverOptions{
			startFn: func(context.Context) (*HubListenerComponents, error) {
				starts++
				return nil, errors.New("bind failed")
			},
			shutdownFn: func(context.Context, *HubListenerComponents) {
				shutdowns++
			},
			emitFn: func(level, event string, fields map[string]any) error {
				if event == "hub-listener-restart-failed" {
					failed++
				}
				events <- hubRestartTestEvent{level: level, event: event, fields: fields}
				return nil
			},
			sleepFn: func(_ context.Context, d time.Duration) bool {
				sleeps = append(sleeps, d)
				return true
			},
			nowFn: func() time.Time { return time.Unix(100, 0) },
		})
	}()

	s.signalHubListenerRestart()
	ev := waitRestartTestEvent(t, events, "hub-listener-restart-exhausted")
	cancel()
	waitRestartDriverDone(t, done)

	if starts != hubListenerRestartMaxConsecutiveRestarts {
		t.Fatalf("start calls = %d, want %d", starts, hubListenerRestartMaxConsecutiveRestarts)
	}
	if shutdowns != 1 {
		t.Fatalf("shutdown calls = %d, want 1", shutdowns)
	}
	if failed != hubListenerRestartMaxConsecutiveRestarts {
		t.Fatalf("failed events = %d, want %d", failed, hubListenerRestartMaxConsecutiveRestarts)
	}
	wantSleeps := []time.Duration{
		5 * time.Second,
		10 * time.Second,
		20 * time.Second,
		40 * time.Second,
	}
	if !reflect.DeepEqual(sleeps, wantSleeps) {
		t.Fatalf("backoff sleeps = %v, want %v", sleeps, wantSleeps)
	}
	if ev.level != "error" {
		t.Fatalf("exhausted level = %q, want error", ev.level)
	}
	if ev.fields["attempts"] != hubListenerRestartMaxConsecutiveRestarts {
		t.Fatalf("exhausted attempts = %v, want %d", ev.fields["attempts"], hubListenerRestartMaxConsecutiveRestarts)
	}
	if got := s.hubMcpComp.Load(); got != nil {
		t.Fatalf("component after exhausted failed restarts = %#v, want nil", got)
	}
}

func TestHubListenerRestartDriverStableHealthyWindowResetsCounter(t *testing.T) {
	s := NewServer(Config{Port: 0})
	oldComp := liveRestartTestComp(3439)
	firstComp := liveRestartTestComp(3439)
	secondComp := liveRestartTestComp(3439)
	s.hubMcpComp.Store(oldComp)

	now := time.Unix(100, 0)
	startComps := []*HubListenerComponents{firstComp, secondComp}
	unexpectedStart := make(chan struct{}, 1)
	events := make(chan hubRestartTestEvent, 8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})

	go func() {
		defer close(done)
		runHubListenerRestartDriver(ctx, s, hubListenerRestartDriverOptions{
			startFn: func(context.Context) (*HubListenerComponents, error) {
				if len(startComps) == 0 {
					select {
					case unexpectedStart <- struct{}{}:
					default:
					}
					return nil, errors.New("unexpected extra start")
				}
				next := startComps[0]
				startComps = startComps[1:]
				return next, nil
			},
			shutdownFn: func(context.Context, *HubListenerComponents) {},
			emitFn: func(level, event string, fields map[string]any) error {
				events <- hubRestartTestEvent{level: level, event: event, fields: fields}
				return nil
			},
			sleepFn: func(context.Context, time.Duration) bool { return true },
			nowFn:   func() time.Time { return now },
		})
	}()

	s.signalHubListenerRestart()
	first := waitRestartTestEvent(t, events, "hub-listener-restarted")
	if first.fields["attempt"] != 1 {
		t.Fatalf("first restart attempt = %v, want 1", first.fields["attempt"])
	}

	now = now.Add(hubListenerRestartStableWindow + time.Second)
	s.signalHubListenerRestart()
	second := waitRestartTestEvent(t, events, "hub-listener-restarted")
	cancel()
	waitRestartDriverDone(t, done)

	select {
	case <-unexpectedStart:
		t.Fatal("restart driver performed an unexpected extra start")
	default:
	}
	if second.fields["attempt"] != 1 {
		t.Fatalf("restart after stable window attempt = %v, want reset to 1", second.fields["attempt"])
	}
	if got := s.hubMcpComp.Load(); got != secondComp {
		t.Fatalf("published component = %#v, want second component", got)
	}
}

func TestHubListenerRestartStableWindowExceedsWatcherDetectionLatency(t *testing.T) {
	detectionLatency := api.DefaultHubHealthProbeInterval * time.Duration(api.DefaultHubHealthUnresponsiveThreshold)
	if hubListenerRestartStableWindow <= detectionLatency {
		t.Fatalf("stable window = %s, want strictly greater than watcher detection latency %s", hubListenerRestartStableWindow, detectionLatency)
	}
}

func TestHubListenerRestartDriverImmediateRefailAfterWatcherLatencyExhausts(t *testing.T) {
	s := NewServer(Config{Port: 0})
	s.hubMcpComp.Store(liveRestartTestComp(3439))

	var nowMu sync.Mutex
	now := time.Unix(1000, 0)
	advanceNow := func(d time.Duration) {
		nowMu.Lock()
		defer nowMu.Unlock()
		now = now.Add(d)
	}
	readNow := func() time.Time {
		nowMu.Lock()
		defer nowMu.Unlock()
		return now
	}

	detectionLatency := api.DefaultHubHealthProbeInterval * time.Duration(api.DefaultHubHealthUnresponsiveThreshold)
	var starts int
	events := make(chan hubRestartTestEvent, 16)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	defer func() {
		cancel()
		waitRestartDriverDone(t, done)
	}()

	go func() {
		defer close(done)
		runHubListenerRestartDriver(ctx, s, hubListenerRestartDriverOptions{
			startFn: func(context.Context) (*HubListenerComponents, error) {
				starts++
				return liveRestartTestComp(3439), nil
			},
			shutdownFn: func(context.Context, *HubListenerComponents) {},
			emitFn: func(level, event string, fields map[string]any) error {
				events <- hubRestartTestEvent{level: level, event: event, fields: fields}
				return nil
			},
			sleepFn: func(context.Context, time.Duration) bool { return true },
			nowFn:   readNow,
		})
	}()

	for wantAttempt := 1; wantAttempt <= hubListenerRestartMaxConsecutiveRestarts; wantAttempt++ {
		s.signalHubListenerRestart()
		ev := waitRestartTestEvent(t, events, "hub-listener-restarted")
		if ev.fields["attempt"] != wantAttempt {
			t.Fatalf("restart attempt after watcher detection latency = %v, want %d", ev.fields["attempt"], wantAttempt)
		}
		advanceNow(detectionLatency)
	}

	s.signalHubListenerRestart()
	ev := waitRestartTestEvent(t, events, "hub-listener-restart-exhausted")
	if ev.fields["attempts"] != hubListenerRestartMaxConsecutiveRestarts {
		t.Fatalf("exhausted attempts = %v, want %d", ev.fields["attempts"], hubListenerRestartMaxConsecutiveRestarts)
	}
	if starts != hubListenerRestartMaxConsecutiveRestarts {
		t.Fatalf("start calls = %d, want %d", starts, hubListenerRestartMaxConsecutiveRestarts)
	}
}

func TestHubListenerRestartDriverContinuesAfterExhaustionForFreshOutage(t *testing.T) {
	s := NewServer(Config{Port: 0})
	s.hubMcpComp.Store(liveRestartTestComp(3439))

	var nowMu sync.Mutex
	now := time.Unix(2000, 0)
	advanceNow := func(d time.Duration) {
		nowMu.Lock()
		defer nowMu.Unlock()
		now = now.Add(d)
	}
	readNow := func() time.Time {
		nowMu.Lock()
		defer nowMu.Unlock()
		return now
	}

	var starts int
	events := make(chan hubRestartTestEvent, 16)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	defer func() {
		cancel()
		waitRestartDriverDone(t, done)
	}()

	go func() {
		defer close(done)
		runHubListenerRestartDriver(ctx, s, hubListenerRestartDriverOptions{
			startFn: func(context.Context) (*HubListenerComponents, error) {
				starts++
				return liveRestartTestComp(3439), nil
			},
			shutdownFn: func(context.Context, *HubListenerComponents) {},
			emitFn: func(level, event string, fields map[string]any) error {
				events <- hubRestartTestEvent{level: level, event: event, fields: fields}
				return nil
			},
			sleepFn: func(context.Context, time.Duration) bool { return true },
			nowFn:   readNow,
		})
	}()

	detectionLatency := api.DefaultHubHealthProbeInterval * time.Duration(api.DefaultHubHealthUnresponsiveThreshold)
	for wantAttempt := 1; wantAttempt <= hubListenerRestartMaxConsecutiveRestarts; wantAttempt++ {
		s.signalHubListenerRestart()
		ev := waitRestartTestEvent(t, events, "hub-listener-restarted")
		if ev.fields["attempt"] != wantAttempt {
			t.Fatalf("restart attempt before exhaustion = %v, want %d", ev.fields["attempt"], wantAttempt)
		}
		advanceNow(detectionLatency)
	}

	s.signalHubListenerRestart()
	ev := waitRestartTestEvent(t, events, "hub-listener-restart-exhausted")
	if ev.fields["attempts"] != hubListenerRestartMaxConsecutiveRestarts {
		t.Fatalf("exhausted attempts = %v, want %d", ev.fields["attempts"], hubListenerRestartMaxConsecutiveRestarts)
	}
	select {
	case <-done:
		t.Fatal("restart driver exited after one exhausted outage; want it to stay armed for future signals")
	default:
	}

	advanceNow(hubListenerRestartStableWindow)
	s.signalHubListenerRestart()
	fresh := waitRestartTestEvent(t, events, "hub-listener-restarted")
	if fresh.fields["attempt"] != 1 {
		t.Fatalf("fresh outage restart attempt = %v, want reset to 1", fresh.fields["attempt"])
	}
	if starts != hubListenerRestartMaxConsecutiveRestarts+1 {
		t.Fatalf("start calls after fresh outage = %d, want %d", starts, hubListenerRestartMaxConsecutiveRestarts+1)
	}
}

func TestHubListenerRestartDriverStableResetDoesNotDisableFailureExhaustion(t *testing.T) {
	s := NewServer(Config{Port: 0})
	s.hubMcpComp.Store(liveRestartTestComp(3439))
	now := time.Unix(1000, 0)
	s.hubRestartConsecutive = 3
	s.hubRestartLastSuccess = now.Add(-hubListenerRestartStableWindow - time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	starts := 0
	exhausted := false

	restartHubListener(ctx, s, hubListenerRestartDriverOptions{
		startFn: func(context.Context) (*HubListenerComponents, error) {
			starts++
			if starts > hubListenerRestartMaxConsecutiveRestarts {
				cancel()
			}
			return nil, errors.New("bind failed")
		},
		shutdownFn: func(context.Context, *HubListenerComponents) {},
		emitFn: func(_ string, event string, _ map[string]any) error {
			if event == "hub-listener-restart-exhausted" {
				exhausted = true
			}
			return nil
		},
		sleepFn: func(context.Context, time.Duration) bool { return true },
		nowFn:   func() time.Time { return now },
	})

	if starts != hubListenerRestartMaxConsecutiveRestarts {
		t.Fatalf("start calls after stable reset failure = %d, want %d", starts, hubListenerRestartMaxConsecutiveRestarts)
	}
	if !exhausted {
		t.Fatal("restart failures after a stable reset did not emit hub-listener-restart-exhausted")
	}
}

func TestHubListenerRestartDriverCancelDuringBackoffDoesNotRestart(t *testing.T) {
	s := NewServer(Config{Port: 0})
	oldComp := liveRestartTestComp(3439)
	s.hubMcpComp.Store(oldComp)

	var starts int
	var shutdowns int
	enteredSleep := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		defer close(done)
		runHubListenerRestartDriver(ctx, s, hubListenerRestartDriverOptions{
			startFn: func(context.Context) (*HubListenerComponents, error) {
				starts++
				return nil, errors.New("bind failed")
			},
			shutdownFn: func(context.Context, *HubListenerComponents) {
				shutdowns++
			},
			emitFn: func(string, string, map[string]any) error { return nil },
			sleepFn: func(ctx context.Context, _ time.Duration) bool {
				close(enteredSleep)
				<-ctx.Done()
				return false
			},
			nowFn: func() time.Time { return time.Unix(100, 0) },
		})
	}()

	s.signalHubListenerRestart()
	select {
	case <-enteredSleep:
	case <-time.After(2 * time.Second):
		t.Fatal("restart driver did not enter backoff")
	}
	cancel()
	waitRestartDriverDone(t, done)

	if starts != 1 {
		t.Fatalf("start calls before canceled retry backoff = %d, want 1", starts)
	}
	if shutdowns != 1 {
		t.Fatalf("shutdown calls before canceled retry backoff = %d, want 1", shutdowns)
	}
	if got := s.hubMcpComp.Load(); got != nil {
		t.Fatalf("component after canceled retry backoff = %#v, want nil", got)
	}
}

func TestHubListenerRestartDriverCanceledBeforeSignalDoesNotRestart(t *testing.T) {
	s := NewServer(Config{Port: 0})
	oldComp := liveRestartTestComp(3439)
	s.hubMcpComp.Store(oldComp)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.signalHubListenerRestart()

	var starts int
	var shutdowns int
	runHubListenerRestartDriver(ctx, s, hubListenerRestartDriverOptions{
		startFn: func(context.Context) (*HubListenerComponents, error) {
			starts++
			return liveRestartTestComp(3439), nil
		},
		shutdownFn: func(context.Context, *HubListenerComponents) {
			shutdowns++
		},
		emitFn:  func(string, string, map[string]any) error { return nil },
		sleepFn: func(context.Context, time.Duration) bool { return true },
		nowFn:   func() time.Time { return time.Unix(100, 0) },
	})

	if starts != 0 {
		t.Fatalf("start calls after pre-signal cancel = %d, want 0", starts)
	}
	if shutdowns != 0 {
		t.Fatalf("shutdown calls after pre-signal cancel = %d, want 0", shutdowns)
	}
	if got := s.hubMcpComp.Load(); got != oldComp {
		t.Fatalf("component after pre-signal cancel = %#v, want original", got)
	}
}

func TestHubListenerRestartDriverCancelAfterStartBeforePublishTearsDownNewBundle(t *testing.T) {
	s := NewServer(Config{Port: 0})
	oldComp := liveRestartTestComp(3439)
	newComp := liveRestartTestComp(3439)
	s.hubMcpComp.Store(oldComp)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var shutdowns []*HubListenerComponents

	restarted := restartHubListener(ctx, s, hubListenerRestartDriverOptions{
		startFn: func(context.Context) (*HubListenerComponents, error) {
			cancel()
			return newComp, nil
		},
		shutdownFn: func(_ context.Context, comp *HubListenerComponents) {
			shutdowns = append(shutdowns, comp)
		},
		emitFn:  func(string, string, map[string]any) error { return nil },
		sleepFn: func(context.Context, time.Duration) bool { return true },
		nowFn:   func() time.Time { return time.Unix(100, 0) },
	})

	if restarted {
		t.Fatal("restart reported success after context cancellation in the publish window")
	}
	if !reflect.DeepEqual(shutdowns, []*HubListenerComponents{oldComp, newComp}) {
		t.Fatalf("shutdowns = %#v, want old then new component exactly once", shutdowns)
	}
	if got := s.hubMcpComp.Load(); got != nil {
		t.Fatalf("component after canceled publish window = %#v, want nil", got)
	}
}

func TestHubListenerRestartDriverShutdownCancelsOldListenerWatchers(t *testing.T) {
	s := NewServer(Config{Port: 0})
	oldComp := liveRestartTestComp(3439)
	oldCtx, oldCancel := context.WithCancel(context.Background())
	oldComp.listenerCancel = oldCancel
	oldWatcherDone := make(chan struct{})
	go func() {
		defer close(oldWatcherDone)
		<-oldCtx.Done()
	}()
	s.hubMcpComp.Store(oldComp)

	newComp := liveRestartTestComp(3439)
	events := make(chan hubRestartTestEvent, 4)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	defer func() {
		cancel()
		waitRestartDriverDone(t, done)
	}()

	go func() {
		defer close(done)
		runHubListenerRestartDriver(ctx, s, hubListenerRestartDriverOptions{
			startFn: func(context.Context) (*HubListenerComponents, error) {
				return newComp, nil
			},
			emitFn: func(level, event string, fields map[string]any) error {
				events <- hubRestartTestEvent{level: level, event: event, fields: fields}
				return nil
			},
			sleepFn: func(context.Context, time.Duration) bool { return true },
			nowFn:   func() time.Time { return time.Unix(100, 0) },
		})
	}()

	s.signalHubListenerRestart()
	waitRestartTestEvent(t, events, "hub-listener-restarted")
	select {
	case <-oldWatcherDone:
	case <-time.After(2 * time.Second):
		t.Fatal("old listener watcher context was not cancelled during restart shutdown")
	}
}

func TestHubListenerRestartDriverConcurrentCancelAndSignalTearsDownOnceNoOrphan(t *testing.T) {
	s := NewServer(Config{Port: 0})
	oldComp := liveRestartTestComp(3439)
	newComp := liveRestartTestComp(3439)
	s.hubMcpComp.Store(oldComp)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	var starts int
	var mu sync.Mutex
	shutdowns := map[*HubListenerComponents]int{}

	go func() {
		defer close(done)
		runHubListenerRestartDriver(ctx, s, hubListenerRestartDriverOptions{
			startFn: func(context.Context) (*HubListenerComponents, error) {
				starts++
				ready := make(chan struct{})
				var wg sync.WaitGroup
				wg.Add(2)
				go func() {
					defer wg.Done()
					<-ready
					cancel()
				}()
				go func() {
					defer wg.Done()
					<-ready
					s.signalHubListenerRestart()
				}()
				close(ready)
				wg.Wait()
				return newComp, nil
			},
			shutdownFn: func(_ context.Context, comp *HubListenerComponents) {
				mu.Lock()
				defer mu.Unlock()
				shutdowns[comp]++
			},
			emitFn:  func(string, string, map[string]any) error { return nil },
			sleepFn: func(context.Context, time.Duration) bool { return true },
			nowFn:   func() time.Time { return time.Unix(100, 0) },
		})
	}()

	s.signalHubListenerRestart()
	waitRestartDriverDone(t, done)

	if starts != 1 {
		t.Fatalf("start calls = %d, want 1", starts)
	}
	mu.Lock()
	oldShutdowns := shutdowns[oldComp]
	newShutdowns := shutdowns[newComp]
	mu.Unlock()
	if oldShutdowns != 1 {
		t.Fatalf("old component shutdowns = %d, want 1", oldShutdowns)
	}
	if newShutdowns != 1 {
		t.Fatalf("new component shutdowns = %d, want 1", newShutdowns)
	}
	if got := s.hubMcpComp.Load(); got != nil {
		t.Fatalf("component after concurrent cancel/signal = %#v, want nil", got)
	}
}

func TestHubListenerRestartDriverNilBatonStopsWithoutDoubleShutdown(t *testing.T) {
	s := NewServer(Config{Port: 0})

	var starts int
	var shutdowns int
	ctx := context.Background()
	done := make(chan struct{})

	go func() {
		defer close(done)
		runHubListenerRestartDriver(ctx, s, hubListenerRestartDriverOptions{
			startFn: func(context.Context) (*HubListenerComponents, error) {
				starts++
				return liveRestartTestComp(3439), nil
			},
			shutdownFn: func(context.Context, *HubListenerComponents) {
				shutdowns++
			},
			emitFn:  func(string, string, map[string]any) error { return nil },
			sleepFn: func(context.Context, time.Duration) bool { return true },
			nowFn:   func() time.Time { return time.Unix(100, 0) },
		})
	}()

	s.signalHubListenerRestart()
	waitRestartDriverDone(t, done)

	if starts != 0 {
		t.Fatalf("start calls with nil baton = %d, want 0", starts)
	}
	if shutdowns != 0 {
		t.Fatalf("shutdown calls with nil baton = %d, want 0", shutdowns)
	}
}

func TestHubListenerRestartStartPreservesPortOnReloadHandlerFailure(t *testing.T) {
	seedManifestDir(t)
	resetResolverSnapshot(t)
	t.Cleanup(func() { api.PublishResolverSnapshot(nil) })
	stateDir := t.TempDir()
	restoreState := api.SetDaemonStateRootForTest(stateDir)
	t.Cleanup(restoreState)
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", stateDir)

	a := api.NewAPI()
	firstCtx, firstCancel := context.WithCancel(context.Background())
	firstComp, err := startHubMcpListenerWithOptions(firstCtx, true, a, startHubMcpListenerOptions{})
	if err != nil {
		firstCancel()
		t.Fatalf("initial startHubMcpListenerWithOptions: %v", err)
	}
	persistedPort := firstComp.port
	firstCancel()
	ShutdownHubListener(context.Background(), firstComp)

	failCtx, failCancel := context.WithCancel(context.Background())
	defer failCancel()
	var reloadAttempts int
	failedComp, err := startHubMcpListenerWithOptions(failCtx, true, a, startHubMcpListenerOptions{
		preservePortOnReloadHandlerFailure: true,
		reloadHandlerFn: func(context.Context) (*api.InternalReloadHandler, error) {
			reloadAttempts++
			return nil, errors.New("reload handler build failed")
		},
	})
	if err == nil {
		failCancel()
		ShutdownHubListener(context.Background(), failedComp)
		t.Fatal("startHubMcpListenerWithOptions succeeded despite reload handler failure")
	}
	if reloadAttempts != 1 {
		t.Fatalf("reload handler attempts = %d, want 1", reloadAttempts)
	}
	afterFailure, err := api.LoadHubEndpoint()
	if err != nil {
		t.Fatalf("LoadHubEndpoint after reload failure: %v", err)
	}
	if afterFailure.Port != persistedPort {
		t.Fatalf("persisted port after reload failure = %d, want preserved %d", afterFailure.Port, persistedPort)
	}

	retryCtx, retryCancel := context.WithCancel(context.Background())
	retryComp, err := startHubMcpListenerWithOptions(retryCtx, true, a, startHubMcpListenerOptions{
		preservePortOnReloadHandlerFailure: true,
	})
	if err != nil {
		retryCancel()
		t.Fatalf("retry startHubMcpListenerWithOptions: %v", err)
	}
	defer func() {
		retryCancel()
		ShutdownHubListener(context.Background(), retryComp)
	}()
	if retryComp.port != persistedPort {
		t.Fatalf("retry port = %d, want preserved %d", retryComp.port, persistedPort)
	}
	afterRetry, err := api.LoadHubEndpoint()
	if err != nil {
		t.Fatalf("LoadHubEndpoint after retry: %v", err)
	}
	if afterRetry.Port != persistedPort {
		t.Fatalf("persisted port after retry = %d, want preserved %d", afterRetry.Port, persistedPort)
	}
}

func TestHubListenerRestartDriverPreservesEndpointInstanceIDAcrossRealRestart(t *testing.T) {
	seedManifestDir(t)
	resetResolverSnapshot(t)
	t.Cleanup(func() { api.PublishResolverSnapshot(nil) })
	stateDir := t.TempDir()
	restoreState := api.SetDaemonStateRootForTest(stateDir)
	t.Cleanup(restoreState)
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", stateDir)

	a := api.NewAPI()
	firstCtx, firstCancel := context.WithCancel(context.Background())
	firstComp, err := startHubMcpListenerWithOptions(firstCtx, true, a, startHubMcpListenerOptions{})
	if err != nil {
		firstCancel()
		t.Fatalf("initial startHubMcpListenerWithOptions: %v", err)
	}
	before, err := api.LoadHubEndpoint()
	if err != nil {
		firstCancel()
		ShutdownHubListener(context.Background(), firstComp)
		t.Fatalf("LoadHubEndpoint before restart: %v", err)
	}

	s := NewServer(Config{Port: 0})
	s.hubMcpComp.Store(firstComp)
	events := make(chan hubRestartTestEvent, 4)
	restartCtx, restartCancel := context.WithCancel(context.Background())
	defer restartCancel()

	restarted := restartHubListener(restartCtx, s, hubListenerRestartDriverOptions{
		startFn: func(ctx context.Context) (*HubListenerComponents, error) {
			return startHubMcpListenerWithOptions(ctx, true, a, startHubMcpListenerOptions{
				preservePortOnReloadHandlerFailure: true,
			})
		},
		shutdownFn: ShutdownHubListener,
		emitFn: func(level, event string, fields map[string]any) error {
			events <- hubRestartTestEvent{level: level, event: event, fields: fields}
			return nil
		},
		sleepFn: func(context.Context, time.Duration) bool { return true },
		nowFn:   func() time.Time { return time.Unix(100, 0) },
	})
	firstCancel()
	if !restarted {
		t.Fatal("restartHubListener returned false")
	}
	t.Cleanup(func() {
		if comp := s.hubMcpComp.Load(); comp != nil {
			ShutdownHubListener(context.Background(), comp)
		}
	})

	after, err := api.LoadHubEndpoint()
	if err != nil {
		t.Fatalf("LoadHubEndpoint after restart: %v", err)
	}
	if after.InstanceID != before.InstanceID {
		t.Fatalf("InstanceID after restart = %q, want preserved %q", after.InstanceID, before.InstanceID)
	}
	ev := waitRestartTestEvent(t, events, "hub-listener-restarted")
	if ev.level != "info" {
		t.Fatalf("restart event level = %q, want info", ev.level)
	}
	if ev.fields["instance_id_preserved"] != true {
		t.Fatalf("restart event instance_id_preserved = %v, want true", ev.fields["instance_id_preserved"])
	}
}

func TestHubListenerRestartEventReportsInstanceIDMismatch(t *testing.T) {
	stateDir := t.TempDir()
	restoreState := api.SetDaemonStateRootForTest(stateDir)
	t.Cleanup(restoreState)
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", stateDir)

	before, err := api.EnsureHubEndpoint(3439, 111)
	if err != nil {
		t.Fatalf("EnsureHubEndpoint before restart: %v", err)
	}
	s := NewServer(Config{Port: 0})
	oldComp := liveRestartTestComp(before.Port)
	newComp := liveRestartTestComp(before.Port)
	s.hubMcpComp.Store(oldComp)
	events := make(chan hubRestartTestEvent, 4)

	restarted := restartHubListener(context.Background(), s, hubListenerRestartDriverOptions{
		startFn: func(context.Context) (*HubListenerComponents, error) {
			if _, err := api.RotateHubInstanceID(); err != nil {
				return nil, err
			}
			return newComp, nil
		},
		shutdownFn: func(context.Context, *HubListenerComponents) {},
		emitFn: func(level, event string, fields map[string]any) error {
			events <- hubRestartTestEvent{level: level, event: event, fields: fields}
			return nil
		},
		sleepFn: func(context.Context, time.Duration) bool { return true },
		nowFn:   func() time.Time { return time.Unix(100, 0) },
	})
	if !restarted {
		t.Fatal("restartHubListener returned false")
	}
	ev := waitRestartTestEvent(t, events, "hub-listener-restarted")
	if ev.level != "warn" {
		t.Fatalf("restart event level = %q, want warn on InstanceID mismatch", ev.level)
	}
	if ev.fields["instance_id_preserved"] != false {
		t.Fatalf("restart event instance_id_preserved = %v, want false", ev.fields["instance_id_preserved"])
	}
}

func TestHubListenerStartOptionsInstallWatcherCallback(t *testing.T) {
	seedManifestDir(t)
	resetResolverSnapshot(t)
	t.Cleanup(func() { api.PublishResolverSnapshot(nil) })
	stateDir := t.TempDir()
	restoreState := api.SetDaemonStateRootForTest(stateDir)
	t.Cleanup(restoreState)
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", stateDir)

	a := api.NewAPI()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	signaled := make(chan struct{}, 1)
	comp, err := startHubMcpListenerWithOptions(ctx, true, a, startHubMcpListenerOptions{
		onUnresponsive: func() {
			select {
			case signaled <- struct{}{}:
			default:
			}
		},
		healthProbeInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("startHubMcpListenerWithOptions: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		ShutdownHubListener(context.Background(), comp)
	})

	if err := comp.srv.Close(); err != nil {
		t.Fatalf("force-close hub listener server: %v", err)
	}
	select {
	case <-signaled:
	case <-time.After(2 * time.Second):
		t.Fatal("health watcher did not invoke explicit onUnresponsive option")
	}
	waitHubRestartTestLogEvent(t, "hub-listener-unresponsive")
}

func TestHubListenerRestartSignalIsNonBlockingAndCoalesced(t *testing.T) {
	s := NewServer(Config{Port: 0})
	s.signalHubListenerRestart()
	s.signalHubListenerRestart()

	select {
	case <-s.hubRestartCh:
	default:
		t.Fatal("restart signal channel is empty after signal")
	}
	select {
	case <-s.hubRestartCh:
		t.Fatal("restart signal channel accepted duplicate pending signal; want coalesced buffered-1")
	default:
	}
}
