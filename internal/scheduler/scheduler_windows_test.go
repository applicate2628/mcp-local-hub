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
		Command:          `C:\path\mcp.exe`,
		Args:             []string{"daemon", "--server", "serena"},
		WorkingDir:       `C:\repo`,
		LogonTrigger:     true,
		RestartOnFailure: true,
	}
	xml := buildCreateXML(spec, "dima_")

	if !strings.Contains(xml, "<LogonTrigger>") {
		t.Error("expected <LogonTrigger> in XML")
	}
	if !strings.Contains(xml, `<Command>C:\path\mcp.exe</Command>`) {
		t.Errorf("Command path not found in XML: %s", xml)
	}
	if !strings.Contains(xml, "<Arguments>daemon --server serena</Arguments>") {
		t.Errorf("Arguments not properly joined: %s", xml)
	}
	if !strings.Contains(xml, "<RestartInterval>PT60S</RestartInterval>") {
		t.Errorf("Restart policy not set: %s", xml)
	}
	if !strings.Contains(xml, "<RestartCount>3</RestartCount>") {
		t.Errorf("Restart count not set: %s", xml)
	}
}

func TestBuildCreateXML_Weekly(t *testing.T) {
	spec := TaskSpec{
		Name:        "mcp-local-hub-refresh",
		Description: "weekly",
		Command:     `C:\path\mcp.exe`,
		Args:        []string{"restart", "--all"},
		WeeklyTrigger: &WeeklyTrigger{
			DayOfWeek:   0, // Sunday
			HourLocal:   3,
			MinuteLocal: 0,
		},
	}
	xml := buildCreateXML(spec, "dima_")
	if !strings.Contains(xml, "<WeeklyTrigger>") {
		t.Error("expected <WeeklyTrigger>")
	}
	if !strings.Contains(xml, "<DaysOfWeek><Sunday /></DaysOfWeek>") {
		t.Errorf("Sunday not set: %s", xml)
	}
	if !strings.Contains(xml, "T03:00:00") {
		t.Errorf("03:00 time not set: %s", xml)
	}
}
