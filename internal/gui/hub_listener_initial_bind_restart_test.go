package gui

import (
	"context"
	"testing"
	"time"
)

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
