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
