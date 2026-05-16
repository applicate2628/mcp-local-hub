// Package migration renders the v0.4.x-pinned Task Scheduler XML and
// classifies arbitrary observed XML against that baseline. The renderer is
// a frozen snapshot of the v0.3.x..v0.4.x install template lineage; it does
// NOT track future v0.5.0 install template changes. See
// docs/superpowers/specs/2026-05-16-v0.5.0-supervisor-architecture.md
// §"XML deviation-only classification" for the binding contract.
package migration

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"strings"

	"mcp-local-hub/internal/scheduler"
)

// dayNames maps Go weekday ints to Task Scheduler XML element names.
// Mirrors internal/scheduler/scheduler_windows.go:57-65 — same lineage.
var dayNames = map[int]string{
	0: "Sunday",
	1: "Monday",
	2: "Tuesday",
	3: "Wednesday",
	4: "Thursday",
	5: "Friday",
	6: "Saturday",
}

// pinnedDefaults table: every scalar XML field whose v0.4.x default value is
// frozen. The classifier compares observed values against this table. Keys
// are dot-separated XPaths matching the way deviations are reported.
//
// User-specific anchors (Principals.Principal.UserId, Triggers.LogonTrigger.UserId)
// are NOT in this map — they are checked against the currentUser parameter
// because the value cannot be hardcoded.
var pinnedDefaults = map[string]string{
	"Principals.Principal.LogonType":      "InteractiveToken",
	"Principals.Principal.RunLevel":       "LeastPrivilege",
	"Settings.AllowHardTerminate":         "true",
	"Settings.StartWhenAvailable":         "false",
	"Settings.RunOnlyIfNetworkAvailable":  "false",
	"Settings.MultipleInstancesPolicy":    "StopExisting",
	"Settings.DisallowStartIfOnBatteries": "false",
	"Settings.StopIfGoingOnBatteries":     "false",
	"Settings.IdleSettings.StopOnIdleEnd": "false",
	"Settings.IdleSettings.RestartOnIdle": "false",
	"Settings.AllowStartOnDemand":         "true",
	"Settings.Enabled":                    "true",
	"Settings.Hidden":                     "false",
	"Settings.RunOnlyIfIdle":              "false",
	"Settings.WakeToRun":                  "false",
	"Settings.ExecutionTimeLimit":         "PT0S",
	"Settings.Priority":                   "7",
	"Settings.RestartOnFailure.Interval":  "PT1M",
	"Settings.RestartOnFailure.Count":     "3",
	"Triggers.LogonTrigger.Enabled":       "true",
}

// V04xTemplateXML renders the canonical v0.3.x..v0.4.x Task Scheduler XML
// for a given TaskSpec + current user.
//
// This is a PINNED SNAPSHOT — it intentionally does NOT track future v0.5.0
// install template changes. Production callers use this only to derive the
// baseline against which observed task XML is classified.
//
// The body is a faithful copy of internal/scheduler/scheduler_windows.go's
// buildCreateXML at the v0.4.x line of development, except for the dynamic
// <Date> element: the migration classifier ignores RegistrationInfo for
// deviation purposes, so we still emit a placeholder for byte-identical
// schema shape. The rendered XML is NOT installed as a scheduled task; it
// is reference text for comparisons inside this package and for any future
// caller that needs a v0.4.x-shaped pristine document.
func V04xTemplateXML(spec scheduler.TaskSpec, currentUser string) string {
	var buf bytes.Buffer
	buf.WriteString(`<?xml version="1.0" encoding="UTF-16"?>`)
	buf.WriteString("\n")
	buf.WriteString(`<Task version="1.4" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">`)
	buf.WriteString("\n  <RegistrationInfo>\n")
	buf.WriteString(fmt.Sprintf("    <Description>%s</Description>\n", xmlEscape(spec.Description)))
	buf.WriteString(fmt.Sprintf("    <Author>%s</Author>\n", xmlEscape(currentUser)))
	// <Date> is intentionally elided to a placeholder — the classifier
	// ignores RegistrationInfo (timestamps drift naturally; not part of the
	// deviation baseline).
	buf.WriteString("    <Date>2026-01-01T00:00:00</Date>\n")
	buf.WriteString("  </RegistrationInfo>\n")

	// Triggers
	buf.WriteString("  <Triggers>\n")
	if spec.LogonTrigger {
		buf.WriteString("    <LogonTrigger>\n")
		buf.WriteString(fmt.Sprintf("      <UserId>%s</UserId>\n", xmlEscape(currentUser)))
		buf.WriteString("      <Enabled>true</Enabled>\n")
		buf.WriteString("    </LogonTrigger>\n")
	}
	if spec.WeeklyTrigger != nil {
		wt := spec.WeeklyTrigger
		day := dayNames[wt.DayOfWeek]
		buf.WriteString("    <CalendarTrigger>\n")
		buf.WriteString(fmt.Sprintf("      <StartBoundary>2026-01-04T%02d:%02d:00</StartBoundary>\n", wt.HourLocal, wt.MinuteLocal))
		buf.WriteString("      <Enabled>true</Enabled>\n")
		buf.WriteString("      <ScheduleByWeek>\n")
		buf.WriteString(fmt.Sprintf("        <DaysOfWeek><%s /></DaysOfWeek>\n", day))
		buf.WriteString("        <WeeksInterval>1</WeeksInterval>\n")
		buf.WriteString("      </ScheduleByWeek>\n")
		buf.WriteString("    </CalendarTrigger>\n")
	}
	buf.WriteString("  </Triggers>\n")

	// Principal — pinned LogonType + RunLevel.
	buf.WriteString("  <Principals>\n")
	buf.WriteString("    <Principal id=\"Author\">\n")
	buf.WriteString(fmt.Sprintf("      <UserId>%s</UserId>\n", xmlEscape(currentUser)))
	buf.WriteString("      <LogonType>InteractiveToken</LogonType>\n")
	buf.WriteString("      <RunLevel>LeastPrivilege</RunLevel>\n")
	buf.WriteString("    </Principal>\n")
	buf.WriteString("  </Principals>\n")

	// Settings — pinned defaults across v0.3.x..v0.4.x.
	buf.WriteString("  <Settings>\n")
	if spec.RestartOnFailure {
		buf.WriteString("    <RestartOnFailure>\n")
		buf.WriteString("      <Interval>PT1M</Interval>\n")
		buf.WriteString("      <Count>3</Count>\n")
		buf.WriteString("    </RestartOnFailure>\n")
	}
	buf.WriteString("    <AllowHardTerminate>true</AllowHardTerminate>\n")
	buf.WriteString("    <StartWhenAvailable>false</StartWhenAvailable>\n")
	buf.WriteString("    <RunOnlyIfNetworkAvailable>false</RunOnlyIfNetworkAvailable>\n")
	buf.WriteString("    <MultipleInstancesPolicy>StopExisting</MultipleInstancesPolicy>\n")
	buf.WriteString("    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>\n")
	buf.WriteString("    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>\n")
	buf.WriteString("    <IdleSettings>\n      <StopOnIdleEnd>false</StopOnIdleEnd>\n      <RestartOnIdle>false</RestartOnIdle>\n    </IdleSettings>\n")
	buf.WriteString("    <AllowStartOnDemand>true</AllowStartOnDemand>\n")
	buf.WriteString("    <Enabled>true</Enabled>\n")
	buf.WriteString("    <Hidden>false</Hidden>\n")
	buf.WriteString("    <RunOnlyIfIdle>false</RunOnlyIfIdle>\n")
	buf.WriteString("    <WakeToRun>false</WakeToRun>\n")
	buf.WriteString("    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>\n")
	buf.WriteString("    <Priority>7</Priority>\n")
	buf.WriteString("  </Settings>\n")

	// Actions
	buf.WriteString("  <Actions Context=\"Author\">\n    <Exec>\n")
	buf.WriteString(fmt.Sprintf("      <Command>%s</Command>\n", xmlEscape(spec.Command)))
	if len(spec.Args) > 0 {
		buf.WriteString(fmt.Sprintf("      <Arguments>%s</Arguments>\n", xmlEscape(joinTaskArgs(spec.Args))))
	}
	if spec.WorkingDir != "" {
		buf.WriteString(fmt.Sprintf("      <WorkingDirectory>%s</WorkingDirectory>\n", xmlEscape(spec.WorkingDir)))
	}
	buf.WriteString("    </Exec>\n  </Actions>\n")
	buf.WriteString("</Task>\n")

	return buf.String()
}

// joinTaskArgs replicates scheduler_windows.go:joinTaskArgs but without
// importing the syscall package (which is build-tagged Windows-only on the
// scheduler side). Migration is cross-platform code — the migration package
// runs in tests on every OS, and the classifier reads observed XML from disk
// not from a syscall. For the renderer's purpose (reference-text baseline)
// the simple-space-joiner is sufficient because all real v0.4.x args are
// shell-metachar-free (`daemon`, `--server`, identifier names).
func joinTaskArgs(args []string) string {
	return strings.Join(args, " ")
}

func xmlEscape(s string) string {
	var out bytes.Buffer
	_ = xml.EscapeText(&out, []byte(s))
	return out.String()
}
