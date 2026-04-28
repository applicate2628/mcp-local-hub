package tray

import (
	"testing"

	"mcp-local-hub/internal/api"
)

func TestAggregate(t *testing.T) {
	cases := []struct {
		name string
		rows []api.DaemonStatus
		want TrayState
	}{
		{"empty → Healthy", nil, StateHealthy},
		{"all Running → Healthy",
			[]api.DaemonStatus{
				{Server: "memory", State: "Running"},
				{Server: "time", State: "Running"},
			},
			StateHealthy,
		},
		{"some Running → Partial",
			[]api.DaemonStatus{
				{Server: "memory", State: "Running"},
				{Server: "time", State: "Stopped"},
			},
			StatePartial,
		},
		{"none Running → Down",
			[]api.DaemonStatus{
				{Server: "memory", State: "Stopped"},
				{Server: "time", State: "Stopped"},
			},
			StateDown,
		},
		{"any LastResult != 0 → Error overrides Partial",
			[]api.DaemonStatus{
				{Server: "memory", State: "Running"},
				{Server: "time", State: "Running", LastResult: 1},
			},
			StateError,
		},
		{"any LastResult != 0 → Error overrides Healthy",
			[]api.DaemonStatus{
				{Server: "memory", State: "Running", LastResult: 1},
			},
			StateError,
		},
		{"state containing 'fail' → Error",
			[]api.DaemonStatus{
				{Server: "memory", State: "FailedToLaunch"},
			},
			StateError,
		},
		{"maintenance rows skipped (would otherwise look Down)",
			[]api.DaemonStatus{
				{Server: "memory", State: "Running"},
				{Daemon: "weekly-refresh", State: "Ready", IsMaintenance: true},
			},
			StateHealthy,
		},
		{"only maintenance rows → Healthy (no real daemons)",
			[]api.DaemonStatus{
				{Daemon: "weekly-refresh", State: "Scheduled", IsMaintenance: true},
			},
			StateHealthy,
		},
		{"maintenance with non-zero LastResult still triggers Error",
			[]api.DaemonStatus{
				{Server: "memory", State: "Running"},
				{Daemon: "weekly-refresh", State: "Ready", IsMaintenance: true, LastResult: 2},
			},
			StateError,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Aggregate(tc.rows)
			if got != tc.want {
				t.Errorf("Aggregate = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestTrayState_String(t *testing.T) {
	cases := []struct {
		s    TrayState
		want string
	}{
		{StateHealthy, "healthy"},
		{StatePartial, "partial"},
		{StateDown, "down"},
		{StateError, "error"},
		{TrayState(99), "unknown"},
	}
	for _, c := range cases {
		if got := c.s.String(); got != c.want {
			t.Errorf("State(%d).String() = %q, want %q", c.s, got, c.want)
		}
	}
}
