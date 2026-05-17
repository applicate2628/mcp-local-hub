package scheduler

import (
	"bytes"
	"encoding/xml"
	"fmt"
)

// BuildWatchdogXML serializes the canonical watchdog scheduled-task XML
// per watchdog plan v13 Task 8 + §7 (Hidden=false) + §27 (no XML cache).
//
// The watchdog task drives `mcphub watchdog --once` every 5 minutes via a
// CalendarTrigger+Repetition pair, plus a LogonTrigger so the loop resumes
// immediately after a cold boot. Structurally it differs from
// buildCreateXML (sibling in scheduler_windows.go for daemon tasks) in
// three load-bearing ways:
//
//  1. <CalendarTrigger> with <Repetition><Interval>PT5M</Interval> +
//     <StopAtDurationEnd>false</StopAtDurationEnd> — recurrence semantics
//     that the daemon-task XML does not need.
//  2. <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy> — the
//     watchdog driver guards against concurrent ticks via a singleton
//     flock; IgnoreNew at the scheduler level is the second layer of
//     defense. This is DELIBERATELY DIFFERENT from daemon tasks (Task 5
//     bug #2 fix) which use StopExisting so RestartOnFailure can revive
//     killed daemons.
//  3. <Priority>9</Priority> + <ExecutionTimeLimit>PT5M</ExecutionTimeLimit>
//     — background priority + hard 5-min cap as the safety net behind the
//     driver-side ctx-deadline of 4 min.
//
// The Description text mentions cadence + uninstall command so operators
// inspecting the task in the Task Scheduler MMC console see the canonical
// recovery path without external docs.
//
// Inputs are XML-escaped at element-text level. The function is pure:
// repeated calls with the same inputs return identical bytes (no
// time.Now() in the body, no ambient input). A package-private alias
// `buildWatchdogXML` exists for symmetry with the lowercase
// `buildCreateXML` in scheduler_windows.go and is the form referenced
// by the in-package tests in scheduler_windows_test.go.
//
// Cross-platform: the function body is pure string composition with no
// Windows-specific dependencies, so it lives in a non-tagged file.
// Callers (api.InstallWatchdogTask) are themselves cross-platform; the
// scheduled-task install side fails through scheduler.New() returning
// "not implemented" on Linux/macOS.
func BuildWatchdogXML(canonicalExe, workingDir, userName string) string {
	var buf bytes.Buffer
	buf.WriteString(`<?xml version="1.0" encoding="UTF-16"?>`)
	buf.WriteString("\n")
	buf.WriteString(`<Task version="1.4" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">`)
	buf.WriteString("\n  <RegistrationInfo>\n")
	buf.WriteString("    <Description>mcp-local-hub watchdog: auto-recovery for daemons. Cadence 5 min. Disable: mcphub watchdog uninstall.</Description>\n")
	buf.WriteString("  </RegistrationInfo>\n")

	// Triggers — CalendarTrigger with 5-min repetition + LogonTrigger.
	// The StartBoundary is a fixed past date so the schedule is active
	// immediately after install regardless of install time-of-day. Per
	// plan §7: the watchdog must be visible (Hidden=false), but its
	// schedule does not depend on a specific calendar day.
	buf.WriteString("  <Triggers>\n")
	buf.WriteString("    <CalendarTrigger>\n")
	buf.WriteString("      <StartBoundary>2026-05-07T00:00:00</StartBoundary>\n")
	buf.WriteString("      <ScheduleByDay><DaysInterval>1</DaysInterval></ScheduleByDay>\n")
	buf.WriteString("      <Repetition>\n")
	buf.WriteString("        <Interval>PT5M</Interval>\n")
	buf.WriteString("        <StopAtDurationEnd>false</StopAtDurationEnd>\n")
	buf.WriteString("      </Repetition>\n")
	buf.WriteString("    </CalendarTrigger>\n")
	// LogonTrigger MUST scope to the specific user. Without <UserId>,
	// the trigger means "any user logon" which requires elevation —
	// schtasks /Create rejects the install with ERROR: Access is denied
	// when the current shell is non-elevated. Daemon tasks
	// (scheduler_windows.go buildCreateXML:84) correctly scope their
	// LogonTrigger to the configured userName; this missing-UserId
	// was a v0.4.x bug that surfaced post-v0.5.0 cherry-pick chain
	// when the user re-ran `mcphub setup` on a non-elevated shell.
	buf.WriteString("    <LogonTrigger>\n")
	buf.WriteString(fmt.Sprintf("      <UserId>%s</UserId>\n", watchdogXMLEscape(userName)))
	buf.WriteString("      <Enabled>true</Enabled>\n")
	buf.WriteString("    </LogonTrigger>\n")
	buf.WriteString("  </Triggers>\n")

	// Principal — same per-user model as daemon tasks. The XML validator
	// (Task 6) enforces RunLevel=LeastPrivilege and LogonType=
	// InteractiveToken at every restart-time recheck; drift here would
	// cause the watchdog to refuse-to-restart its own scheduled task.
	buf.WriteString("  <Principals>\n")
	buf.WriteString("    <Principal id=\"Author\">\n")
	buf.WriteString(fmt.Sprintf("      <UserId>%s</UserId>\n", watchdogXMLEscape(userName)))
	buf.WriteString("      <LogonType>InteractiveToken</LogonType>\n")
	buf.WriteString("      <RunLevel>LeastPrivilege</RunLevel>\n")
	buf.WriteString("    </Principal>\n")
	buf.WriteString("  </Principals>\n")

	// Settings — see function-level doc for the canonical contract pins.
	buf.WriteString("  <Settings>\n")
	buf.WriteString("    <Hidden>false</Hidden>\n")
	buf.WriteString("    <Priority>9</Priority>\n")
	buf.WriteString("    <ExecutionTimeLimit>PT5M</ExecutionTimeLimit>\n")
	buf.WriteString("    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>\n")
	buf.WriteString("    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>\n")
	buf.WriteString("    <RunOnlyIfIdle>false</RunOnlyIfIdle>\n")
	buf.WriteString("    <AllowStartOnDemand>true</AllowStartOnDemand>\n")
	buf.WriteString("    <Enabled>true</Enabled>\n")
	buf.WriteString("  </Settings>\n")

	// Actions — bound to the per-user "Author" principal. The validator
	// (Task 6) checks tokens[0] == "watchdog"; passing "watchdog --once"
	// satisfies that gate.
	buf.WriteString("  <Actions Context=\"Author\">\n    <Exec>\n")
	buf.WriteString(fmt.Sprintf("      <Command>%s</Command>\n", watchdogXMLEscape(canonicalExe)))
	buf.WriteString("      <Arguments>watchdog --once</Arguments>\n")
	buf.WriteString(fmt.Sprintf("      <WorkingDirectory>%s</WorkingDirectory>\n", watchdogXMLEscape(workingDir)))
	buf.WriteString("    </Exec>\n  </Actions>\n")
	buf.WriteString("</Task>\n")

	return buf.String()
}

// buildWatchdogXML is the package-private alias used by the in-package
// tests in scheduler_windows_test.go. Mirrors the naming pattern of
// buildCreateXML (lowercase, sibling to BuildWatchdogXML's exported form).
// Future scheduler-package callers should use this lowercase form; the
// exported BuildWatchdogXML exists only for the api package's
// InstallWatchdogTask which lives in a different package.
func buildWatchdogXML(canonicalExe, workingDir, userName string) string {
	return BuildWatchdogXML(canonicalExe, workingDir, userName)
}

// watchdogXMLEscape XML-escapes element text. Local to this file so the
// builder remains free of windows-tagged-file dependencies (the existing
// xmlEscape lives in scheduler_windows.go). encoding/xml.EscapeText
// handles `<`, `>`, `&`, `'`, and `"`.
func watchdogXMLEscape(s string) string {
	var out bytes.Buffer
	_ = xml.EscapeText(&out, []byte(s))
	return out.String()
}
