//go:build windows

package scheduler

import (
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
