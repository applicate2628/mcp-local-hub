package gui

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestHubInitialBindFailureEnqueuesTypedRestartAndKeepsRecovering(t *testing.T) {
	s := NewServer(Config{Port: 0})
	s.signalInitialHubBindFailure()

	select {
	case cause := <-s.hubRestartCh:
		if cause != hubListenerRestartCauseInitialBindFailed {
			t.Fatalf("restart cause = %v, want initial-bind-failed", cause)
		}
	default:
		t.Fatal("initial hub bind failure did not enqueue the restart driver")
	}
	if got, action := s.hubHealth.snapshot(); got != HubHealthRecovering || action != "" {
		t.Fatalf("health after initial bind failure = state %q action %q, want recovering", got, action)
	}
}

func TestHubListenerRestartDriverInitialBindFailureRetriesFromNil(t *testing.T) {
	s := NewServer(Config{Port: 0})
	newComp := liveRestartTestComp(3439)
	events := make(chan hubRestartTestEvent, 8)
	var starts, shutdowns int

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runHubListenerRestartDriver(ctx, s, hubListenerRestartDriverOptions{
			startFn: func(context.Context) (*HubListenerComponents, error) {
				starts++
				return newComp, nil
			},
			shutdownFn: func(context.Context, *HubListenerComponents) { shutdowns++ },
			emitFn: s.hubHealthEmitWrapper(func(level, event string, fields map[string]any) error {
				events <- hubRestartTestEvent{level: level, event: event, fields: fields}
				return nil
			}),
			sleepFn: func(context.Context, time.Duration) bool { return true },
			nowFn:   func() time.Time { return time.Unix(100, 0) },
		})
	}()

	s.signalInitialHubBindFailure()
	waitRestartTestEvent(t, events, "hub-listener-restarted")
	cancel()
	waitRestartDriverDone(t, done)

	if starts != 1 {
		t.Fatalf("start calls = %d, want 1", starts)
	}
	if shutdowns != 0 {
		t.Fatalf("shutdown calls from nil initial component = %d, want 0", shutdowns)
	}
	if got := s.hubMcpComp.Load(); got != newComp {
		t.Fatalf("published component = %#v, want %#v", got, newComp)
	}
	if got, action := s.hubHealth.snapshot(); got != HubHealthHealthy || action != "" {
		t.Fatalf("health after initial-bind retry = state %q action %q, want healthy", got, action)
	}
}

func TestHubListenerRestartDriverInitialBindFailureExhaustionEndsDown(t *testing.T) {
	s := NewServer(Config{Port: 0})
	events := make(chan hubRestartTestEvent, 64)
	var starts, shutdowns int

	done := make(chan struct{})
	go func() {
		defer close(done)
		runHubListenerRestartDriver(context.Background(), s, hubListenerRestartDriverOptions{
			startFn: func(context.Context) (*HubListenerComponents, error) {
				starts++
				return nil, errors.New("permanently unbindable")
			},
			shutdownFn: func(context.Context, *HubListenerComponents) { shutdowns++ },
			emitFn: s.hubHealthEmitWrapper(func(level, event string, fields map[string]any) error {
				events <- hubRestartTestEvent{level: level, event: event, fields: fields}
				return nil
			}),
			sleepFn: func(context.Context, time.Duration) bool { return true },
			nowFn:   func() time.Time { return time.Unix(100, 0) },
		})
	}()

	s.signalInitialHubBindFailure()
	waitRestartTestEvent(t, events, "hub-listener-restart-abandoned")
	waitRestartDriverDone(t, done)

	if starts != hubListenerRestartMaxAttemptsPerWindow {
		t.Fatalf("start calls = %d, want rolling-window cap %d", starts, hubListenerRestartMaxAttemptsPerWindow)
	}
	if shutdowns != 0 {
		t.Fatalf("shutdown calls from permanently nil component = %d, want 0", shutdowns)
	}
	if got, action := s.hubHealth.snapshot(); got != HubHealthDown || action != "" {
		t.Fatalf("health after bounded initial-bind exhaustion = state %q action %q, want down", got, action)
	}
}

func TestHubListenerRestartDriverNilComponentNonInitialCausesStop(t *testing.T) {
	for _, cause := range []hubListenerRestartCause{
		hubListenerRestartCauseUnresponsive,
		hubListenerRestartCause(255),
	} {
		t.Run(cause.String(), func(t *testing.T) {
			s := NewServer(Config{Port: 0})
			var starts, shutdowns int
			outcome := restartHubListenerWithOutcome(context.Background(), s, hubListenerRestartDriverOptions{
				cause: cause,
				startFn: func(context.Context) (*HubListenerComponents, error) {
					starts++
					return liveRestartTestComp(3439), nil
				},
				shutdownFn: func(context.Context, *HubListenerComponents) { shutdowns++ },
				emitFn:     func(string, string, map[string]any) error { return nil },
				sleepFn:    func(context.Context, time.Duration) bool { return true },
				nowFn:      func() time.Time { return time.Unix(100, 0) },
			})
			if outcome != hubListenerRestartStopDriver {
				t.Fatalf("outcome = %v, want stop-driver", outcome)
			}
			if starts != 0 || shutdowns != 0 {
				t.Fatalf("starts=%d shutdowns=%d, want zero for nil non-initial entry", starts, shutdowns)
			}
		})
	}
}
