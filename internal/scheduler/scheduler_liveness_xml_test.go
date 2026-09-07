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
		exe        = `C:\Users\test\.local\bin\mcphub.exe`
		windowless = `C:\Users\test\.local\bin\mcphub-windowless.exe`
		wdir       = `C:\Users\test\.local\bin`
		user       = "test-user"
	)
	xml := BuildLivenessXML(exe, wdir, "S-1-5-21-test", user)

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
		// Owned liveness runs through the same-directory windowless adapter.
		"<Command>" + windowless + "</Command>",
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
	if again := BuildLivenessXML(exe, wdir, "S-1-5-21-test", user); again != xml {
		t.Errorf("BuildLivenessXML is not pure: two calls returned different bytes")
	}
}

func TestBuildLivenessXML_SeparatesEscapedPrincipalSIDAndLogonAccount(t *testing.T) {
	const (
		exe     = `C:\Users\test\.local\bin\mcphub.exe`
		wdir    = `C:\Users\test\.local\bin`
		sid     = `S-1-5-21<&>'"`
		account = `account<&>'"`
	)

	got := BuildLivenessXML(exe, wdir, sid, account)
	again := BuildLivenessXML(exe, wdir, sid, account)
	if again != got {
		t.Fatal("BuildLivenessXML output changed between identical calls")
	}

	const (
		escapedSID     = `S-1-5-21&lt;&amp;&gt;&#39;&#34;`
		escapedAccount = `account&lt;&amp;&gt;&#39;&#34;`
	)
	principalStart := strings.Index(got, "    <Principal id=\"Author\">\n")
	principalEnd := strings.Index(got, "    </Principal>\n")
	if principalStart < 0 || principalEnd < principalStart {
		t.Fatal("principal section is missing")
	}
	principal := got[principalStart : principalEnd+len("    </Principal>\n")]
	if !strings.Contains(principal, "<UserId>"+escapedSID+"</UserId>") || strings.Contains(principal, escapedAccount) {
		t.Fatalf("principal identity = %q; want only escaped SID %q", principal, escapedSID)
	}
	logonStart := strings.Index(got, "    <LogonTrigger>\n")
	logonEnd := strings.Index(got, "    </LogonTrigger>\n")
	if logonStart < 0 || logonEnd < logonStart {
		t.Fatal("logon trigger section is missing")
	}
	logon := got[logonStart : logonEnd+len("    </LogonTrigger>\n")]
	if !strings.Contains(logon, "<UserId>"+escapedAccount+"</UserId>") || strings.Contains(logon, escapedSID) {
		t.Fatalf("logon identity = %q; want only escaped account %q", logon, escapedAccount)
	}

	other := BuildLivenessXML(exe, wdir, "S-1-5-21-other", "other-account")
	scrub := func(xml, principal, logon string) string {
		xml = strings.Replace(xml, principal, "<principal-identity>", 1)
		return strings.Replace(xml, logon, "<logon-identity>", 1)
	}
	if got, want := scrub(got, escapedSID, escapedAccount), scrub(other, "S-1-5-21-other", "other-account"); got != want {
		t.Fatalf("identity inputs changed non-identity canonical XML\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestWindowsOwnedEntrypointPathUsesExactSiblingAndIsIdempotent(t *testing.T) {
	const cli = `C:\Users\test\.local\bin\mcphub.exe`
	const adapter = `C:\Users\test\.local\bin\mcphub-windowless.exe`
	if got := WindowsOwnedEntrypointPath(cli); got != adapter {
		t.Fatalf("got %q want %q", got, adapter)
	}
	if got := WindowsOwnedEntrypointPath(adapter); got != adapter {
		t.Fatalf("adapter changed to %q", got)
	}
}
