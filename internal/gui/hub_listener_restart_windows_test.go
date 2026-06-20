//go:build windows

package gui

import (
	"context"
	"fmt"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestHubListenerRestartWindowsSamePortRebindErrorsDoNotConsumeNormalBudget(t *testing.T) {
	s := NewServer(Config{Port: 0})
	oldComp := liveRestartTestComp(3439)
	newComp := liveRestartTestComp(3439)
	s.hubMcpComp.Store(oldComp)

	var starts int
	var sleeps []time.Duration
	events := make(chan hubRestartTestEvent, 16)
	restarted := restartHubListener(context.Background(), s, hubListenerRestartDriverOptions{
		startFn: func(context.Context) (*HubListenerComponents, error) {
			starts++
			if starts <= hubListenerRestartMaxConsecutiveRestarts+2 {
				return nil, fmt.Errorf("bind 127.0.0.1:%d: %w", oldComp.port, syscall.Errno(windows.WSAEADDRINUSE))
			}
			return newComp, nil
		},
		shutdownFn: func(context.Context, *HubListenerComponents) {},
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

	if !restarted {
		t.Fatal("restartHubListener exhausted normal attempts on temporary Windows same-port rebind errors")
	}
	if starts <= hubListenerRestartMaxConsecutiveRestarts {
		t.Fatalf("start calls = %d, want more than normal budget %d", starts, hubListenerRestartMaxConsecutiveRestarts)
	}
	if got := s.hubMcpComp.Load(); got != newComp {
		t.Fatalf("published component = %#v, want new component", got)
	}
	if len(sleeps) != hubListenerRestartMaxConsecutiveRestarts+2 {
		t.Fatalf("same-port retry sleeps = %d, want %d", len(sleeps), hubListenerRestartMaxConsecutiveRestarts+2)
	}
	for i, d := range sleeps {
		if d != hubListenerRestartSamePortRebindBackoff {
			t.Fatalf("same-port retry sleep[%d] = %s, want %s", i, d, hubListenerRestartSamePortRebindBackoff)
		}
	}
	ev := waitRestartTestEvent(t, events, "hub-listener-restarted")
	if ev.fields["attempt"] != 1 {
		t.Fatalf("restart attempt after same-port retries = %v, want 1", ev.fields["attempt"])
	}
}
