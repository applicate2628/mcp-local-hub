package api

import "testing"

// TestIsMaintenanceTaskName_LivenessExactMatch is the bot PR #288 r38 P3
// regression pin: the `-liveness` classifier must be an EXACT match against
// the single hub-wide LivenessTaskName, NOT a suffix match.
//
// Falsifying control: a legitimate server daemon literally named `liveness`
// produces the task "\\mcp-local-hub-<server>-liveness", which also ends in
// `-liveness`. The pre-r38 form `strings.HasSuffix(name, "-liveness")` wrongly
// classified that real `<server>/liveness` daemon as hub maintenance, so
// callers skipped/rejected it. With the exact-name fix the `<server>-liveness`
// cases below assert false; under the pre-fix suffix match those assertions
// FAIL (the function returns true).
//
// The hub-wide LivenessTaskName itself (both canonical leading-backslash and
// bare forms) stays true. The `-watchdog` and `-weekly-refresh` suffix matches
// are deliberately broad and are asserted UNCHANGED here to guard against an
// over-narrowing regression: watchdog covers legacy/hand-edited rows, and
// weekly-refresh is legitimately both per-server and hub-wide.
func TestIsMaintenanceTaskName_LivenessExactMatch(t *testing.T) {
	cases := []struct {
		name string
		task string
		want bool
	}{
		// Hub-wide liveness — the ONE canonical name. Both shapes stay true
		// because callers arrive in both forms (isHubInfrastructureTaskName
		// strips the leading backslash; supervisor/GUI/status callers pass
		// the canonical form verbatim).
		{"hub-wide liveness canonical (unchanged true)", `\mcp-local-hub-liveness`, true},
		{"hub-wide liveness bare (unchanged true)", "mcp-local-hub-liveness", true},
		{"hub-wide liveness via constant", LivenessTaskName, true},

		// FALSIFYING controls: a real server daemon named `liveness`. Pre-fix
		// suffix match wrongly returns true; the exact-name fix returns false.
		{"server liveness daemon canonical (pre-fix WRONGLY true)", `\mcp-local-hub-demo-liveness`, false},
		{"server liveness daemon bare (pre-fix WRONGLY true)", "mcp-local-hub-demo-liveness", false},
		{"another server liveness daemon (pre-fix WRONGLY true)", `\mcp-local-hub-serena-liveness`, false},

		// Watchdog suffix — deliberately broad, asserted UNCHANGED.
		{"hub-wide watchdog (unchanged true)", `\mcp-local-hub-watchdog`, true},
		{"server watchdog suffix (unchanged true)", `\mcp-local-hub-demo-watchdog`, true},

		// Weekly-refresh suffix — deliberately broad (hub-wide AND per-server),
		// asserted UNCHANGED.
		{"hub-wide weekly-refresh (unchanged true)", `\mcp-local-hub-weekly-refresh`, true},
		{"per-server weekly-refresh (unchanged true)", `\mcp-local-hub-demo-weekly-refresh`, true},
		{"server weekly-refresh bare (unchanged true)", "mcp-local-hub-serena-weekly-refresh", true},

		// Genuine daemon names with no maintenance suffix — false (sanity).
		{"genuine daemon", `\mcp-local-hub-memory-default`, false},
		{"foreign task", "some-other-task", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isMaintenanceTaskName(tc.task); got != tc.want {
				t.Errorf("isMaintenanceTaskName(%q) = %v, want %v", tc.task, got, tc.want)
			}
			// The exported alias must agree with the unexported helper.
			if got := IsMaintenanceTaskName(tc.task); got != tc.want {
				t.Errorf("IsMaintenanceTaskName(%q) = %v, want %v", tc.task, got, tc.want)
			}
		})
	}
}
