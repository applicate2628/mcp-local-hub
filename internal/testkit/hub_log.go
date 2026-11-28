package testkit

import (
	"testing"
	"time"
)

// WaitForHubLogEvent waits for the application log reader to observe want.
func WaitForHubLogEvent(t testing.TB, want string, readEvents func(int) ([]map[string]any, error)) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		events, err := readEvents(16)
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
