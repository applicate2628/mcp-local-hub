//go:build windows

package scheduler

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildCreateXML_Logon(t *testing.T) {
	spec := TaskSpec{
		Name:             "mcp-local-hub-test-logon",
		Description:      "test logon task",
		Command:          `C:\path\mcphub.exe`,
		Args:             []string{"daemon", "--server", "serena"},
		WorkingDir:       `C:\repo`,
		LogonTrigger:     true,
		RestartOnFailure: true,
	}
	xml := buildCreateXML(spec, "dima_")

	if !strings.Contains(xml, "<LogonTrigger>") {
		t.Error("expected <LogonTrigger> in XML")
	}
	if !strings.Contains(xml, `<Command>C:\path\mcphub.exe</Command>`) {
		t.Errorf("Command path not found in XML: %s", xml)
	}
	if !strings.Contains(xml, "<Arguments>daemon --server serena</Arguments>") {
		t.Errorf("Arguments not properly joined: %s", xml)
	}
	// Task Scheduler requires a nested <RestartOnFailure> container with
	// <Interval> and <Count> inside — flat <RestartInterval>/<RestartCount>
	// siblings are rejected at schtasks /Create /XML time.
	if !strings.Contains(xml, "<RestartOnFailure>") {
		t.Errorf("expected <RestartOnFailure> container: %s", xml)
	}
	if !strings.Contains(xml, "<Interval>PT1M</Interval>") {
		t.Errorf("expected <Interval>PT1M</Interval> inside RestartOnFailure: %s", xml)
	}
	if !strings.Contains(xml, "<Count>3</Count>") {
		t.Errorf("expected <Count>3</Count> inside RestartOnFailure: %s", xml)
	}
	// Also assert that the old flat form is NOT present (regression guard).
	if strings.Contains(xml, "<RestartInterval>") || strings.Contains(xml, "<RestartCount>") {
		t.Errorf("flat RestartInterval/RestartCount must not appear: %s", xml)
	}
	// v0.3.0-blockers bug #2 regression: MultipleInstancesPolicy must be
	// StopExisting, not IgnoreNew. With IgnoreNew, RestartOnFailure does
	// NOT fire after Task Manager kill — TS sees the just-killed instance
	// as still "running" in its internal state machine and ignores the
	// restart attempt. StopExisting tells TS "stop the lingering instance
	// (no-op when already dead) and start the new one", which is what
	// auto-recovery actually requires. D2.4 manual smoke 2026-05-07.
	if !strings.Contains(xml, "<MultipleInstancesPolicy>StopExisting</MultipleInstancesPolicy>") {
		t.Errorf("expected MultipleInstancesPolicy=StopExisting (not IgnoreNew); RestartOnFailure won't fire after kill otherwise: %s", xml)
	}
	if strings.Contains(xml, "<MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>") {
		t.Errorf("MultipleInstancesPolicy=IgnoreNew breaks RestartOnFailure auto-recovery (bug #2): %s", xml)
	}
}

func TestBuildCreateXML_Weekly(t *testing.T) {
	spec := TaskSpec{
		Name:        "mcp-local-hub-refresh",
		Description: "weekly",
		Command:     `C:\path\mcphub.exe`,
		Args:        []string{"restart", "--all"},
		WeeklyTrigger: &WeeklyTrigger{
			DayOfWeek:   0, // Sunday
			HourLocal:   3,
			MinuteLocal: 0,
		},
	}
	xml := buildCreateXML(spec, "dima_")
	// Weekly recurrence must be inside <CalendarTrigger>, not a top-level
	// <WeeklyTrigger> (Task Scheduler schema rule, rejected at schtasks /Create).
	if !strings.Contains(xml, "<CalendarTrigger>") {
		t.Errorf("expected <CalendarTrigger> container: %s", xml)
	}
	if !strings.Contains(xml, "<ScheduleByWeek>") {
		t.Errorf("expected <ScheduleByWeek>: %s", xml)
	}
	if !strings.Contains(xml, "<DaysOfWeek><Sunday /></DaysOfWeek>") {
		t.Errorf("Sunday not set: %s", xml)
	}
	if !strings.Contains(xml, "T03:00:00") {
		t.Errorf("03:00 time not set: %s", xml)
	}
	// Regression guard: bare <WeeklyTrigger> (as direct child of <Triggers>) is invalid.
	if strings.Contains(xml, "<WeeklyTrigger>") {
		t.Errorf("bare <WeeklyTrigger> must not appear (use <CalendarTrigger>): %s", xml)
	}
}

// TestBuildCreateXML_QuotesArgsWithSpaces guards the workspace-scoped
// register path that passes canonical absolute paths (e.g.
// `C:\Users\Test User\workspace`) as a TaskSpec argument. Without the
// syscall.EscapeArg pass in joinTaskArgs, Task Scheduler's XML
// `<Arguments>` element received the raw path and Windows' child-launcher
// split it into multiple argv tokens.
func TestBuildCreateXML_QuotesArgsWithSpaces(t *testing.T) {
	spec := TaskSpec{
		Name:    "mcp-local-hub-quote-test",
		Command: `C:\path\mcphub.exe`,
		Args: []string{
			"daemon", "workspace-proxy",
			"--port", "9200",
			"--workspace", `C:\Users\Test User\workspace`,
			"--language", "go",
		},
	}
	xml := buildCreateXML(spec, "dima_")
	// The workspace path MUST be wrapped in quotes; unquoted would split
	// into "C:\Users\Test" + "User\workspace" at argv time. xmlEscape will
	// then re-encode the surrounding quote as &#34; inside the XML.
	wantFragment := `&#34;C:\Users\Test User\workspace&#34;`
	if !strings.Contains(xml, wantFragment) {
		t.Errorf("expected workspace path to be quoted (%q) in XML, got:\n%s", wantFragment, xml)
	}
	// Simple args (no spaces) must still appear unquoted so operator-read
	// Task Scheduler panels stay readable.
	if !strings.Contains(xml, "daemon workspace-proxy --port 9200") {
		t.Errorf("simple args must not be gratuitously quoted; xml=%s", xml)
	}
}

// TestBuildCreateXML_HandlesInternalQuotes verifies the escaping applied
// to an argument that already contains a double quote. syscall.EscapeArg
// escapes the internal quote as \"; outer quotes are only added when the
// arg also contains whitespace. For an internal quote alone the expected
// rendering is `a\"b` which XML-escapes to `a\&#34;b`.
func TestBuildCreateXML_HandlesInternalQuotes(t *testing.T) {
	spec := TaskSpec{
		Name:    "mcp-local-hub-quote-internal-test",
		Command: `C:\path\mcphub.exe`,
		Args:    []string{"--label", `a"b`},
	}
	xml := buildCreateXML(spec, "dima_")
	if !strings.Contains(xml, `a\&#34;b`) {
		t.Errorf("expected escaped internal quote `a\\&#34;b` in XML, got:\n%s", xml)
	}
	// A quote-with-space arg must get the outer wrapping quotes too.
	spec2 := TaskSpec{
		Name:    "mcp-local-hub-quote-internal-space-test",
		Command: `C:\path\mcphub.exe`,
		Args:    []string{"--label", `has "quoted" space`},
	}
	xml2 := buildCreateXML(spec2, "dima_")
	if !strings.Contains(xml2, `&#34;has \&#34;quoted\&#34; space&#34;`) {
		t.Errorf("expected both outer and internal quotes escaped, got:\n%s", xml2)
	}
}

// TestBuildCreateXML_HandlesTrailingBackslash verifies
// CommandLineToArgvW's rule for runs of backslashes preceding a closing
// quote: every trailing backslash must be doubled. A naive
// quoting implementation would emit `"C:\path\"` which Windows parses as
// `C:\path"`. syscall.EscapeArg doubles the backslashes correctly.
func TestBuildCreateXML_HandlesTrailingBackslash(t *testing.T) {
	spec := TaskSpec{
		Name:    "mcp-local-hub-quote-trailbs-test",
		Command: `C:\path\mcphub.exe`,
		Args:    []string{"--workspace", `C:\Users\Test User\ws\`},
	}
	xml := buildCreateXML(spec, "dima_")
	// With a trailing backslash the arg should end in `\\"` inside the
	// command line, i.e. `&#34;...ws\\&#34;` in the XML-escaped form.
	if !strings.Contains(xml, `ws\\&#34;`) {
		t.Errorf("trailing backslash must be doubled before closing quote, got:\n%s", xml)
	}
}

func TestSameWindowsUser(t *testing.T) {
	tests := []struct {
		name    string
		owner   string
		current string
		want    bool
	}{
		{name: "exact", owner: "alice", current: "alice", want: true},
		{name: "domain prefixed", owner: `MACHINE\alice`, current: "alice", want: true},
		{name: "case-insensitive", owner: `domain\ALICE`, current: "alice", want: true},
		{name: "different user", owner: "bob", current: "alice", want: false},
		{name: "empty owner", owner: "", current: "alice", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := sameWindowsUser(tc.owner, tc.current)
			if got != tc.want {
				t.Fatalf("sameWindowsUser(%q, %q)=%v, want %v", tc.owner, tc.current, got, tc.want)
			}
		})
	}
}

func TestResolveSchtasksPath(t *testing.T) {
	t.Setenv("SystemRoot", `C:\Windows`)
	t.Setenv("WINDIR", "")
	got, err := resolveSchtasksPath()
	if err != nil {
		t.Fatalf("resolveSchtasksPath() error = %v", err)
	}
	want := filepath.Join(`C:\Windows`, "System32", "schtasks.exe")
	if got != want {
		t.Fatalf("resolveSchtasksPath() = %q, want %q", got, want)
	}
}

func TestResolveSchtasksPathFallbackAndMissing(t *testing.T) {
	t.Setenv("SystemRoot", "")
	t.Setenv("WINDIR", `D:\Win`)
	got, err := resolveSchtasksPath()
	if err != nil {
		t.Fatalf("resolveSchtasksPath() fallback error = %v", err)
	}
	want := filepath.Join(`D:\Win`, "System32", "schtasks.exe")
	if got != want {
		t.Fatalf("resolveSchtasksPath() fallback = %q, want %q", got, want)
	}

	t.Setenv("WINDIR", "")
	if _, err := resolveSchtasksPath(); err == nil {
		t.Fatal("resolveSchtasksPath() expected error when SystemRoot/WINDIR unset")
	}
}

// ---------------------------------------------------------------------------
// buildWatchdogXML — watchdog plan v13 Task 8 + §7 Hidden=false + §27 no cache.
//
// The watchdog scheduled task drives `mcphub watchdog --once` every 5 minutes.
// Its XML differs structurally from the daemon-task XML (CalendarTrigger with
// <Repetition>, IgnoreNew vs StopExisting, Priority=9, ExecutionTimeLimit=PT5M).
// These tests pin the canonical contract so future edits cannot drift the
// XML body silently.
// ---------------------------------------------------------------------------

const (
	testWatchdogExe        = `C:\Users\test\.local\bin\mcphub.exe`
	testWatchdogWorkingDir = `C:\Users\test\.local\bin`
	testWatchdogUser       = "test"
)

// TestBuildWatchdogXML_Hidden_False asserts the watchdog task is visible in
// Task Scheduler UI (Hidden=false). Per plan §7: operators must be able to
// inspect the watchdog from the standard Task Scheduler MMC console.
func TestBuildWatchdogXML_Hidden_False(t *testing.T) {
	xml := buildWatchdogXML(testWatchdogExe, testWatchdogWorkingDir, testWatchdogUser)
	if !strings.Contains(xml, "<Hidden>false</Hidden>") {
		t.Errorf("expected <Hidden>false</Hidden> in watchdog XML; got:\n%s", xml)
	}
	// Regression guard: the daemon-task XML also uses Hidden=false, but a
	// stray Hidden=true here would silently hide the watchdog from the
	// console. Loud-fail.
	if strings.Contains(xml, "<Hidden>true</Hidden>") {
		t.Errorf("watchdog must not set Hidden=true (plan §7); got:\n%s", xml)
	}
}

// TestBuildWatchdogXML_Priority_9 asserts the watchdog runs at the lowest
// (background) Task Scheduler priority. Per plan: a 5-minute heartbeat must
// not contend with foreground daemons or interactive workloads.
func TestBuildWatchdogXML_Priority_9(t *testing.T) {
	xml := buildWatchdogXML(testWatchdogExe, testWatchdogWorkingDir, testWatchdogUser)
	if !strings.Contains(xml, "<Priority>9</Priority>") {
		t.Errorf("expected <Priority>9</Priority> in watchdog XML; got:\n%s", xml)
	}
}

// TestBuildWatchdogXML_Interval_PT5M asserts the watchdog cadence is 5 min.
// Per plan: cadence advertised in <Description>; the canonical mechanism is
// <CalendarTrigger><Repetition><Interval>PT5M</Interval>.
func TestBuildWatchdogXML_Interval_PT5M(t *testing.T) {
	xml := buildWatchdogXML(testWatchdogExe, testWatchdogWorkingDir, testWatchdogUser)
	if !strings.Contains(xml, "<Interval>PT5M</Interval>") {
		t.Errorf("expected <Interval>PT5M</Interval> in watchdog XML; got:\n%s", xml)
	}
}

// TestBuildWatchdogXML_ExecutionTimeLimit_PT5M asserts the watchdog driver
// is bounded to 5 min per run. Watchdog driver uses ctx-deadline of 4 min;
// the schtasks-side hard cap of PT5M is the safety net.
func TestBuildWatchdogXML_ExecutionTimeLimit_PT5M(t *testing.T) {
	xml := buildWatchdogXML(testWatchdogExe, testWatchdogWorkingDir, testWatchdogUser)
	if !strings.Contains(xml, "<ExecutionTimeLimit>PT5M</ExecutionTimeLimit>") {
		t.Errorf("expected <ExecutionTimeLimit>PT5M</ExecutionTimeLimit> in watchdog XML; got:\n%s", xml)
	}
}

// TestBuildWatchdogXML_Description_Present asserts the operator-facing
// description text is present. The canonical text mentions "watchdog",
// "auto-recovery", "5 min", and the disable command.
func TestBuildWatchdogXML_Description_Present(t *testing.T) {
	xml := buildWatchdogXML(testWatchdogExe, testWatchdogWorkingDir, testWatchdogUser)
	want := []string{
		"mcp-local-hub watchdog",
		"auto-recovery",
		"5 min",
		"mcphub watchdog uninstall",
	}
	for _, w := range want {
		if !strings.Contains(xml, w) {
			t.Errorf("description should contain %q; full XML:\n%s", w, xml)
		}
	}
}

// TestBuildWatchdogXML_BothTriggers asserts BOTH a CalendarTrigger (5-min
// repetition) AND a LogonTrigger (resume-from-cold-boot) are present. Per
// plan: watchdog must fire on logon, then continue every 5 min.
func TestBuildWatchdogXML_BothTriggers(t *testing.T) {
	xml := buildWatchdogXML(testWatchdogExe, testWatchdogWorkingDir, testWatchdogUser)
	if !strings.Contains(xml, "<CalendarTrigger>") {
		t.Errorf("expected <CalendarTrigger> in watchdog XML; got:\n%s", xml)
	}
	if !strings.Contains(xml, "<LogonTrigger>") {
		t.Errorf("expected <LogonTrigger> in watchdog XML; got:\n%s", xml)
	}
	// Per the plan snippet the LogonTrigger is enabled.
	if !strings.Contains(xml, "<LogonTrigger><Enabled>true</Enabled></LogonTrigger>") &&
		!strings.Contains(xml, "<LogonTrigger>\n      <Enabled>true</Enabled>\n    </LogonTrigger>") {
		t.Errorf("LogonTrigger should be enabled; got:\n%s", xml)
	}
}

// TestBuildWatchdogXML_IgnoreNew asserts the watchdog itself uses
// MultipleInstancesPolicy=IgnoreNew (NOT StopExisting). Per plan §10.1:
// the watchdog driver guards against concurrent runs via singleton-flock;
// IgnoreNew at the scheduler level is the second layer of defense. This is
// DIFFERENT from daemon tasks which use StopExisting (bug #2 fix, Task 5).
func TestBuildWatchdogXML_IgnoreNew(t *testing.T) {
	xml := buildWatchdogXML(testWatchdogExe, testWatchdogWorkingDir, testWatchdogUser)
	if !strings.Contains(xml, "<MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>") {
		t.Errorf("watchdog must use MultipleInstancesPolicy=IgnoreNew (plan §10.1); got:\n%s", xml)
	}
	// Regression guard: StopExisting on the watchdog would cause overlapping
	// 5-min ticks to terminate one another, defeating the whole flock-based
	// cooperative concurrency story.
	if strings.Contains(xml, "<MultipleInstancesPolicy>StopExisting</MultipleInstancesPolicy>") {
		t.Errorf("watchdog must NOT use StopExisting (that is the daemon-task policy); got:\n%s", xml)
	}
}

// TestBuildWatchdogXML_Command asserts the <Command> element is the
// caller-supplied canonical exe path verbatim (XML-escaped).
func TestBuildWatchdogXML_Command(t *testing.T) {
	xml := buildWatchdogXML(testWatchdogExe, testWatchdogWorkingDir, testWatchdogUser)
	want := "<Command>" + testWatchdogExe + "</Command>"
	if !strings.Contains(xml, want) {
		t.Errorf("expected %q in watchdog XML; got:\n%s", want, xml)
	}
}

// TestBuildWatchdogXML_Arguments asserts the watchdog driver receives the
// `watchdog --once` argv exactly (no trailing or leading whitespace, no
// extra flags). The XML validator (Task 6) compares this verbatim against
// `tokens[0] == "watchdog"`.
func TestBuildWatchdogXML_Arguments(t *testing.T) {
	xml := buildWatchdogXML(testWatchdogExe, testWatchdogWorkingDir, testWatchdogUser)
	if !strings.Contains(xml, "<Arguments>watchdog --once</Arguments>") {
		t.Errorf("expected <Arguments>watchdog --once</Arguments>; got:\n%s", xml)
	}
}

// TestBuildWatchdogXML_WorkingDirectory asserts the <WorkingDirectory> is
// the caller-supplied path verbatim (XML-escaped). Watchdog cwd is set so
// that relative path lookups inside the driver behave deterministically.
func TestBuildWatchdogXML_WorkingDirectory(t *testing.T) {
	xml := buildWatchdogXML(testWatchdogExe, testWatchdogWorkingDir, testWatchdogUser)
	want := "<WorkingDirectory>" + testWatchdogWorkingDir + "</WorkingDirectory>"
	if !strings.Contains(xml, want) {
		t.Errorf("expected %q in watchdog XML; got:\n%s", want, xml)
	}
}

// TestBuildWatchdogXML_PrincipalCanonical asserts the watchdog runs as the
// caller-supplied per-user principal with the canonical RunLevel/LogonType
// the XML validator (Task 6) requires. A drift here would cause the
// watchdog to refuse-to-restart its own scheduled task at validation time.
func TestBuildWatchdogXML_PrincipalCanonical(t *testing.T) {
	xml := buildWatchdogXML(testWatchdogExe, testWatchdogWorkingDir, testWatchdogUser)
	if !strings.Contains(xml, "<UserId>"+testWatchdogUser+"</UserId>") {
		t.Errorf("expected <UserId>%s</UserId>; got:\n%s", testWatchdogUser, xml)
	}
	if !strings.Contains(xml, "<RunLevel>LeastPrivilege</RunLevel>") {
		t.Errorf("expected <RunLevel>LeastPrivilege</RunLevel>; got:\n%s", xml)
	}
	if !strings.Contains(xml, "<LogonType>InteractiveToken</LogonType>") {
		t.Errorf("expected <LogonType>InteractiveToken</LogonType>; got:\n%s", xml)
	}
}

// TestBuildWatchdogXML_AdditionalSettings asserts the remaining settings
// from the plan snippet:
//   - DisallowStartIfOnBatteries=false (must run on AC + battery)
//   - RunOnlyIfIdle=false (must run regardless of idle state)
//   - AllowStartOnDemand=true (operator can /Run from CLI)
//   - Enabled=true (initial state)
//   - StopAtDurationEnd=false (CalendarTrigger keeps repeating)
func TestBuildWatchdogXML_AdditionalSettings(t *testing.T) {
	xml := buildWatchdogXML(testWatchdogExe, testWatchdogWorkingDir, testWatchdogUser)
	want := []string{
		"<DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>",
		"<RunOnlyIfIdle>false</RunOnlyIfIdle>",
		"<AllowStartOnDemand>true</AllowStartOnDemand>",
		"<Enabled>true</Enabled>",
		"<StopAtDurationEnd>false</StopAtDurationEnd>",
	}
	for _, w := range want {
		if !strings.Contains(xml, w) {
			t.Errorf("expected %q in watchdog XML; got:\n%s", w, xml)
		}
	}
}

// TestBuildWatchdogXML_ActionsContextAuthor asserts the <Actions> element
// is bound to the "Author" principal (which carries the per-user UserId).
// Without Context="Author", schtasks defaults to SYSTEM, which would
// elevate the watchdog and break the per-user model.
func TestBuildWatchdogXML_ActionsContextAuthor(t *testing.T) {
	xml := buildWatchdogXML(testWatchdogExe, testWatchdogWorkingDir, testWatchdogUser)
	if !strings.Contains(xml, `<Actions Context="Author">`) {
		t.Errorf("expected <Actions Context=\"Author\">; got:\n%s", xml)
	}
}

// TestBuildWatchdogXML_XMLEscapesSpecialChars asserts that user names and
// paths containing XML metacharacters are escaped at element-text level
// (defense vs `<UserId>` injection if ever fed an attacker-controlled name).
func TestBuildWatchdogXML_XMLEscapesSpecialChars(t *testing.T) {
	exe := `C:\path\with&amp.exe`
	wd := `C:\working<dir>`
	user := "alice&bob"
	xml := buildWatchdogXML(exe, wd, user)
	// Raw `&` / `<` / `>` in element text would not be escaped if a
	// future edit dropped xmlEscape; assert the encoded forms are
	// present and the raw forms are not.
	if !strings.Contains(xml, "alice&amp;bob") {
		t.Errorf("user `&` not XML-escaped; got:\n%s", xml)
	}
	if !strings.Contains(xml, "C:\\working&lt;dir&gt;") {
		t.Errorf("workingDir `<` / `>` not XML-escaped; got:\n%s", xml)
	}
	if !strings.Contains(xml, "C:\\path\\with&amp;amp.exe") {
		t.Errorf("command-path `&` not XML-escaped; got:\n%s", xml)
	}
}

// TestBuildWatchdogXML_TaskHeader asserts the XML opens with the canonical
// declaration + Task root element. Without UTF-16 declaration + version
// attribute, schtasks /Create /XML fails with parse errors at install time.
func TestBuildWatchdogXML_TaskHeader(t *testing.T) {
	xml := buildWatchdogXML(testWatchdogExe, testWatchdogWorkingDir, testWatchdogUser)
	if !strings.Contains(xml, `<?xml version="1.0" encoding="UTF-16"?>`) {
		t.Errorf("expected XML declaration; got:\n%s", xml)
	}
	if !strings.Contains(xml, `<Task version="1.4" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">`) {
		t.Errorf("expected Task root element; got:\n%s", xml)
	}
}

// TestBuildWatchdogXML_Deterministic asserts repeated calls return
// identical XML for the same inputs. This guards against accidental
// time.Now() or other ambient-input drift inside the builder.
func TestBuildWatchdogXML_Deterministic(t *testing.T) {
	a := buildWatchdogXML(testWatchdogExe, testWatchdogWorkingDir, testWatchdogUser)
	b := buildWatchdogXML(testWatchdogExe, testWatchdogWorkingDir, testWatchdogUser)
	if a != b {
		t.Errorf("buildWatchdogXML must be deterministic for fixed inputs; diff:\nA=%s\nB=%s", a, b)
	}
}
