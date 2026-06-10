package scheduler

import (
	"bytes"
	"encoding/xml"
	"fmt"
)

// livenessXMLEscape XML-escapes element text for the liveness task XML.
// Migrated here from scheduler_watchdog_xml.go when the v0.6 redesign
// (spec §5 Phase D) deleted the watchdog XML builder — this file is the
// surviving consumer. Local to the file so the builder remains free of
// windows-tagged-file dependencies (the broader xmlEscape lives in
// scheduler_windows.go). encoding/xml.EscapeText handles `<`, `>`, `&`,
// `'`, and `"`.
func livenessXMLEscape(s string) string {
	var out bytes.Buffer
	_ = xml.EscapeText(&out, []byte(s))
	return out.String()
}

// BuildLivenessXML serializes the canonical supervisor-liveness scheduled-task
// XML (v0.6 redesign spec §15 P1-b / §5.x Phase 3a). The liveness task is a
// minimal owner-death recovery: it relaunches the supervisor/GUI OWNER if it
// dies mid-session. It is the v0.6 supervisor-era replacement for the deleted
// v0.4.x watchdog (which revived scheduler-task DAEMONS, a job the supervisor's
// own Job-Object reaper + reconcile loop now owns).
//
// The task drives `mcphub supervise --ensure-alive` every ~1 minute via a
// CalendarTrigger+Repetition pair, plus a LogonTrigger so the loop resumes
// immediately after a cold boot. Load-bearing settings:
//
//  1. <Interval>PT1M</Interval> — the ~1-min cadence the Phase-3a done-gate
//     specifies ("back within ≈1 min").
//  2. <ExecutionTimeLimit>PT1M</ExecutionTimeLimit> — a single ensure-alive
//     tick is a flock probe + (rarely) one schtasks /Run; a 1-min cap is the
//     OS-level safety net behind the action's own fast return.
//  3. <Arguments>supervise --ensure-alive</Arguments> — the action is the
//     minimal liveness probe.
//
// MultipleInstancesPolicy=IgnoreNew matches the watchdog: the ensure-alive
// action is idempotent (the supervisor/GUI singleton locks make a relaunch a
// no-op when one is already coming up), and IgnoreNew at the scheduler level
// is the second layer of defense against a tick stacking on a slow prior tick.
//
// Inputs are XML-escaped at element-text level (livenessXMLEscape, local to
// this file). The function is pure: repeated calls with the same inputs return
// identical bytes (no time.Now() in the body, no ambient input).
//
// Cross-platform: the body is pure string composition with no Windows-specific
// dependencies, so it lives in a non-tagged file.
// The scheduled-task install side fails through scheduler.New() returning
// "not implemented" on Linux/macOS.
func BuildLivenessXML(canonicalExe, workingDir, userName string) string {
	var buf bytes.Buffer
	buf.WriteString(`<?xml version="1.0" encoding="UTF-16"?>`)
	buf.WriteString("\n")
	buf.WriteString(`<Task version="1.4" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">`)
	buf.WriteString("\n  <RegistrationInfo>\n")
	buf.WriteString("    <Description>mcp-local-hub supervisor-liveness: relaunch the supervisor/GUI owner if it dies mid-session. Cadence 1 min. Additive to the watchdog.</Description>\n")
	buf.WriteString("  </RegistrationInfo>\n")

	// Triggers — CalendarTrigger with 1-min repetition + LogonTrigger.
	// The StartBoundary is a fixed past date so the schedule is active
	// immediately after install regardless of install time-of-day.
	buf.WriteString("  <Triggers>\n")
	buf.WriteString("    <CalendarTrigger>\n")
	buf.WriteString("      <StartBoundary>2026-06-10T00:00:00</StartBoundary>\n")
	buf.WriteString("      <ScheduleByDay><DaysInterval>1</DaysInterval></ScheduleByDay>\n")
	buf.WriteString("      <Repetition>\n")
	buf.WriteString("        <Interval>PT1M</Interval>\n")
	buf.WriteString("        <StopAtDurationEnd>false</StopAtDurationEnd>\n")
	buf.WriteString("      </Repetition>\n")
	buf.WriteString("    </CalendarTrigger>\n")
	// LogonTrigger MUST scope to the specific user. Without <UserId>, the
	// trigger means "any user logon" which requires elevation — schtasks
	// /Create rejects the install with ERROR: Access is denied on a
	// non-elevated shell (same trap the watchdog XML documents).
	buf.WriteString("    <LogonTrigger>\n")
	buf.WriteString(fmt.Sprintf("      <UserId>%s</UserId>\n", livenessXMLEscape(userName)))
	buf.WriteString("      <Enabled>true</Enabled>\n")
	buf.WriteString("    </LogonTrigger>\n")
	buf.WriteString("  </Triggers>\n")

	// Principal — same per-user, least-privilege model as the watchdog +
	// daemon tasks.
	buf.WriteString("  <Principals>\n")
	buf.WriteString("    <Principal id=\"Author\">\n")
	buf.WriteString(fmt.Sprintf("      <UserId>%s</UserId>\n", livenessXMLEscape(userName)))
	buf.WriteString("      <LogonType>InteractiveToken</LogonType>\n")
	buf.WriteString("      <RunLevel>LeastPrivilege</RunLevel>\n")
	buf.WriteString("    </Principal>\n")
	buf.WriteString("  </Principals>\n")

	// Settings — background priority + hard 1-min cap.
	buf.WriteString("  <Settings>\n")
	buf.WriteString("    <Hidden>false</Hidden>\n")
	buf.WriteString("    <Priority>9</Priority>\n")
	buf.WriteString("    <ExecutionTimeLimit>PT1M</ExecutionTimeLimit>\n")
	buf.WriteString("    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>\n")
	buf.WriteString("    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>\n")
	buf.WriteString("    <RunOnlyIfIdle>false</RunOnlyIfIdle>\n")
	buf.WriteString("    <AllowStartOnDemand>true</AllowStartOnDemand>\n")
	buf.WriteString("    <Enabled>true</Enabled>\n")
	buf.WriteString("  </Settings>\n")

	// Actions — bound to the per-user "Author" principal. The action is
	// `supervise --ensure-alive` (the new minimal liveness probe).
	buf.WriteString("  <Actions Context=\"Author\">\n    <Exec>\n")
	buf.WriteString(fmt.Sprintf("      <Command>%s</Command>\n", livenessXMLEscape(canonicalExe)))
	buf.WriteString("      <Arguments>supervise --ensure-alive</Arguments>\n")
	buf.WriteString(fmt.Sprintf("      <WorkingDirectory>%s</WorkingDirectory>\n", livenessXMLEscape(workingDir)))
	buf.WriteString("    </Exec>\n  </Actions>\n")
	buf.WriteString("</Task>\n")

	return buf.String()
}
