package scheduler

import (
	"strings"
	"testing"
)

// TestBuildLivenessXML_Contract pins the load-bearing contract of the
// supervisor-liveness scheduled-task XML (v0.6 spec §15 P1-b / §5.x Phase 3a)
// so a future edit cannot silently drift the body. The function is pure +
// cross-platform (no build tag), so this test runs on every platform.
func TestBuildLivenessXML_Contract(t *testing.T) {
	const (
		exe  = `C:\Users\test\.local\bin\mcphub.exe`
		wdir = `C:\Users\test\.local\bin`
		user = "test-user"
	)
	xml := BuildLivenessXML(exe, wdir, user)

	wantContains := []string{
		// ~1-min cadence (the Phase-3a done-gate "back within ≈1 min").
		"<Interval>PT1M</Interval>",
		// Hard 1-min OS-level cap behind the action's own fast return.
		"<ExecutionTimeLimit>PT1M</ExecutionTimeLimit>",
		// The action is the new minimal liveness probe — NOT watchdog --once.
		"<Arguments>supervise --ensure-alive</Arguments>",
		// IgnoreNew (idempotent tick; second layer behind the singleton locks).
		"<MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>",
		// LogonTrigger scoped to the per-user principal (resumes after boot;
		// avoids the "any user logon needs elevation" install-refusal trap).
		"<LogonTrigger>",
		"<RunLevel>LeastPrivilege</RunLevel>",
		// CalendarTrigger + Repetition recurrence pair.
		"<CalendarTrigger>",
		"<Repetition>",
		// The canonical exe + working dir flow through verbatim.
		"<Command>" + exe + "</Command>",
		"<WorkingDirectory>" + wdir + "</WorkingDirectory>",
		// Per-user principal carries the supplied user id.
		"<UserId>" + user + "</UserId>",
	}
	for _, want := range wantContains {
		if !strings.Contains(xml, want) {
			t.Errorf("BuildLivenessXML output missing %q\n---\n%s", want, xml)
		}
	}

	// The liveness task must NOT carry the watchdog's 5-min cadence or its
	// `watchdog --once` action (proves it is a distinct task, not a copy).
	wantAbsent := []string{
		"<Interval>PT5M</Interval>",
		"watchdog --once",
	}
	for _, bad := range wantAbsent {
		if strings.Contains(xml, bad) {
			t.Errorf("BuildLivenessXML output unexpectedly contains %q (must not mirror the watchdog cadence/action)", bad)
		}
	}

	// Purity: repeated calls with the same inputs return identical bytes
	// (no time.Now(), no ambient input).
	if again := BuildLivenessXML(exe, wdir, user); again != xml {
		t.Errorf("BuildLivenessXML is not pure: two calls returned different bytes")
	}
}
