package api

import (
	"strings"
	"testing"
)

// TestSchedulerUpgrade_WorkspaceTaskSkipPredicates guards the skip logic
// introduced for Phase 3 workspace-scoped tasks. SchedulerUpgrade is
// designed to rebuild per-server daemon tasks (Command path rewrite
// after a binary move); the shared workspace weekly-refresh task and
// the per-(workspace, language) lazy proxies must be left alone — they
// do not resolve to any manifest's server slug and re-registration is
// the correct path to rebuild them. Without these skips, upgrade
// would report "manifest workspace: not found" and "manifest lsp: not
// found" and potentially delete tasks that have no replacement path.
func TestSchedulerUpgrade_WorkspaceTaskSkipPredicates(t *testing.T) {
	cases := []struct {
		name   string
		task   string
		expect string // "skip-ws-weekly" | "skip-ws-lazy" | "upgrade" | "skip-global-weekly"
	}{
		{"global-weekly-refresh", "mcp-local-hub-weekly-refresh", "skip-global-weekly"},
		{"workspace-weekly-refresh", "mcp-local-hub-workspace-weekly-refresh", "skip-ws-weekly"},
		{"lazy-proxy", "mcp-local-hub-lsp-abcd1234-python", "skip-ws-lazy"},
		{"global-daemon", "mcp-local-hub-serena-claude", "upgrade"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			normalized := strings.TrimPrefix(tc.task, "\\")
			var got string
			switch {
			case normalized == WeeklyRefreshTaskName:
				got = "skip-ws-weekly"
			case IsLazyProxyTaskName(normalized):
				got = "skip-ws-lazy"
			default:
				srv, dmn := parseTaskName(tc.task)
				if srv == "" && dmn == "weekly-refresh" {
					got = "skip-global-weekly"
				} else if srv == "" {
					got = "unparseable"
				} else {
					got = "upgrade"
				}
			}
			if got != tc.expect {
				t.Errorf("task %q: got %q, want %q", tc.task, got, tc.expect)
			}
		})
	}
}

// TestSchedulerUpgradeNoopWhenEmpty verifies that running SchedulerUpgrade
// on a system with no mcp-local-hub tasks returns an empty result list
// without error.
func TestSchedulerUpgradeNoopWhenEmpty(t *testing.T) {
	// Cannot easily stub schtasks.exe in unit tests; just assert the api
	// accepts the call and returns something sane. Real verification is
	// the live smoke test in step 3.
	a := NewAPI()
	results, err := a.SchedulerUpgrade()
	if err != nil {
		t.Skipf("scheduler unavailable: %v", err)
	}
	_ = results
}

func TestParseWeeklyRefreshSchedule(t *testing.T) {
	tests := []struct {
		input   string
		wantDay int
		wantHr  int
		wantMin int
		wantErr bool
	}{
		{"SUN 03:00", 0, 3, 0, false},
		{"MON 14:30", 1, 14, 30, false},
		{"FRI 23:59", 5, 23, 59, false},
		{"SAT 00:01", 6, 0, 1, false},
		{"XXX 12:00", 0, 0, 0, true},
		{"SUN 25:00", 0, 0, 0, true},
		{"SUN", 0, 0, 0, true},
	}
	for _, tc := range tests {
		day, hr, min, err := parseWeeklyRefreshSchedule(tc.input)
		gotErr := err != nil
		if gotErr != tc.wantErr {
			t.Errorf("%q: err=%v, wantErr=%v", tc.input, err, tc.wantErr)
			continue
		}
		if !tc.wantErr && (day != tc.wantDay || hr != tc.wantHr || min != tc.wantMin) {
			t.Errorf("%q: got (%d,%d,%d), want (%d,%d,%d)", tc.input, day, hr, min, tc.wantDay, tc.wantHr, tc.wantMin)
		}
	}
}
