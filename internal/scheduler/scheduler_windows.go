//go:build windows

package scheduler

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"strings"
	"time"
)

// windowsScheduler shells out to `schtasks.exe` for all operations.
// We build a Task Scheduler XML document per spec, pipe it to `schtasks /Create /XML`,
// and parse the output of `/Query` for Status/List.
type windowsScheduler struct {
	username string // e.g., "dima_"
}

func newPlatformScheduler() (Scheduler, error) {
	u, err := user.Current()
	if err != nil {
		return nil, fmt.Errorf("user.Current: %w", err)
	}
	// u.Username is typically "MACHINE\\user" on Windows — strip the domain.
	name := u.Username
	if i := strings.LastIndex(name, "\\"); i >= 0 {
		name = name[i+1:]
	}
	return &windowsScheduler{username: name}, nil
}

// dayNames maps Go weekday ints to Task Scheduler XML element names.
var dayNames = map[int]string{
	0: "Sunday",
	1: "Monday",
	2: "Tuesday",
	3: "Wednesday",
	4: "Thursday",
	5: "Friday",
	6: "Saturday",
}

// buildCreateXML serializes a TaskSpec into a Task Scheduler XML document.
// Exposed (lowercase) for testing within the same package.
func buildCreateXML(spec TaskSpec, userName string) string {
	var buf bytes.Buffer
	buf.WriteString(`<?xml version="1.0" encoding="UTF-16"?>`)
	buf.WriteString("\n")
	buf.WriteString(`<Task version="1.4" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">`)
	buf.WriteString("\n  <RegistrationInfo>\n")
	buf.WriteString(fmt.Sprintf("    <Description>%s</Description>\n", xmlEscape(spec.Description)))
	buf.WriteString(fmt.Sprintf("    <Author>%s</Author>\n", xmlEscape(userName)))
	buf.WriteString(fmt.Sprintf("    <Date>%s</Date>\n", time.Now().Format("2006-01-02T15:04:05")))
	buf.WriteString("  </RegistrationInfo>\n")

	// Triggers
	buf.WriteString("  <Triggers>\n")
	if spec.LogonTrigger {
		buf.WriteString("    <LogonTrigger>\n")
		buf.WriteString(fmt.Sprintf("      <UserId>%s</UserId>\n", xmlEscape(userName)))
		buf.WriteString("      <Enabled>true</Enabled>\n")
		buf.WriteString("    </LogonTrigger>\n")
	}
	if spec.WeeklyTrigger != nil {
		wt := spec.WeeklyTrigger
		day := dayNames[wt.DayOfWeek]
		buf.WriteString("    <WeeklyTrigger>\n")
		buf.WriteString(fmt.Sprintf("      <StartBoundary>2026-01-04T%02d:%02d:00</StartBoundary>\n", wt.HourLocal, wt.MinuteLocal))
		buf.WriteString("      <Enabled>true</Enabled>\n")
		buf.WriteString("      <ScheduleByWeek>\n")
		buf.WriteString(fmt.Sprintf("        <DaysOfWeek><%s /></DaysOfWeek>\n", day))
		buf.WriteString("        <WeeksInterval>1</WeeksInterval>\n")
		buf.WriteString("      </ScheduleByWeek>\n")
		buf.WriteString("    </WeeklyTrigger>\n")
	}
	buf.WriteString("  </Triggers>\n")

	// Principal — run as current user, interactive (needs session)
	buf.WriteString("  <Principals>\n")
	buf.WriteString("    <Principal id=\"Author\">\n")
	buf.WriteString(fmt.Sprintf("      <UserId>%s</UserId>\n", xmlEscape(userName)))
	buf.WriteString("      <LogonType>InteractiveToken</LogonType>\n")
	buf.WriteString("      <RunLevel>LeastPrivilege</RunLevel>\n")
	buf.WriteString("    </Principal>\n")
	buf.WriteString("  </Principals>\n")

	// Settings — restart policy + sane defaults
	buf.WriteString("  <Settings>\n")
	if spec.RestartOnFailure {
		buf.WriteString("    <RestartInterval>PT60S</RestartInterval>\n")
		buf.WriteString("    <RestartCount>3</RestartCount>\n")
	}
	buf.WriteString("    <AllowHardTerminate>true</AllowHardTerminate>\n")
	buf.WriteString("    <StartWhenAvailable>false</StartWhenAvailable>\n")
	buf.WriteString("    <RunOnlyIfNetworkAvailable>false</RunOnlyIfNetworkAvailable>\n")
	buf.WriteString("    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>\n")
	buf.WriteString("    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>\n")
	buf.WriteString("    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>\n")
	buf.WriteString("    <IdleSettings>\n      <StopOnIdleEnd>false</StopOnIdleEnd>\n      <RestartOnIdle>false</RestartOnIdle>\n    </IdleSettings>\n")
	buf.WriteString("    <AllowStartOnDemand>true</AllowStartOnDemand>\n")
	buf.WriteString("    <Enabled>true</Enabled>\n")
	buf.WriteString("    <Hidden>false</Hidden>\n")
	buf.WriteString("    <RunOnlyIfIdle>false</RunOnlyIfIdle>\n")
	buf.WriteString("    <WakeToRun>false</WakeToRun>\n")
	buf.WriteString("    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>\n") // no timeout
	buf.WriteString("    <Priority>7</Priority>\n")
	buf.WriteString("  </Settings>\n")

	// Actions
	buf.WriteString("  <Actions Context=\"Author\">\n    <Exec>\n")
	buf.WriteString(fmt.Sprintf("      <Command>%s</Command>\n", xmlEscape(spec.Command)))
	if len(spec.Args) > 0 {
		buf.WriteString(fmt.Sprintf("      <Arguments>%s</Arguments>\n", xmlEscape(strings.Join(spec.Args, " "))))
	}
	if spec.WorkingDir != "" {
		buf.WriteString(fmt.Sprintf("      <WorkingDirectory>%s</WorkingDirectory>\n", xmlEscape(spec.WorkingDir)))
	}
	buf.WriteString("    </Exec>\n  </Actions>\n")
	buf.WriteString("</Task>\n")

	return buf.String()
}

func xmlEscape(s string) string {
	var out bytes.Buffer
	_ = xml.EscapeText(&out, []byte(s))
	return out.String()
}

// Create writes the XML to a temp file and invokes `schtasks /Create /XML`.
func (w *windowsScheduler) Create(spec TaskSpec) error {
	xmlDoc := buildCreateXML(spec, w.username)
	tmp, err := os.CreateTemp("", "mcp-local-hub-task-*.xml")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	defer os.Remove(tmp.Name())
	// Task Scheduler requires UTF-16 LE with BOM. Re-encode.
	utf16 := utf8ToUTF16WithBOM(xmlDoc)
	if _, err := tmp.Write(utf16); err != nil {
		tmp.Close()
		return fmt.Errorf("write xml: %w", err)
	}
	tmp.Close()

	cmd := exec.Command("schtasks", "/Create", "/TN", spec.Name, "/XML", tmp.Name(), "/F")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("schtasks /Create: %w: %s", err, string(out))
	}
	return nil
}

// utf8ToUTF16WithBOM converts a UTF-8 string to UTF-16 LE with a BOM, which
// is what Task Scheduler's /XML flag requires.
func utf8ToUTF16WithBOM(s string) []byte {
	var out bytes.Buffer
	out.WriteByte(0xFF)
	out.WriteByte(0xFE) // UTF-16 LE BOM
	for _, r := range s {
		if r <= 0xFFFF {
			out.WriteByte(byte(r))
			out.WriteByte(byte(r >> 8))
		} else {
			// surrogate pair
			r -= 0x10000
			hi := 0xD800 + (r >> 10)
			lo := 0xDC00 + (r & 0x3FF)
			out.WriteByte(byte(hi))
			out.WriteByte(byte(hi >> 8))
			out.WriteByte(byte(lo))
			out.WriteByte(byte(lo >> 8))
		}
	}
	return out.Bytes()
}

func (w *windowsScheduler) Delete(name string) error {
	cmd := exec.Command("schtasks", "/Delete", "/TN", name, "/F")
	out, err := cmd.CombinedOutput()
	if err != nil {
		// If the task does not exist, schtasks returns exit 1 with "ERROR: The system cannot find the file specified."
		// Treat that as success (idempotent delete).
		if strings.Contains(string(out), "cannot find") || strings.Contains(string(out), "does not exist") {
			return nil
		}
		return fmt.Errorf("schtasks /Delete: %w: %s", err, string(out))
	}
	return nil
}

func (w *windowsScheduler) Run(name string) error {
	cmd := exec.Command("schtasks", "/Run", "/TN", name)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("schtasks /Run: %w: %s", err, string(out))
	}
	return nil
}

func (w *windowsScheduler) Stop(name string) error {
	cmd := exec.Command("schtasks", "/End", "/TN", name)
	out, err := cmd.CombinedOutput()
	if err != nil {
		// "ERROR: There is no running instance of the task." → nil
		if strings.Contains(string(out), "no running instance") {
			return nil
		}
		return fmt.Errorf("schtasks /End: %w: %s", err, string(out))
	}
	return nil
}

func (w *windowsScheduler) Status(name string) (TaskStatus, error) {
	cmd := exec.Command("schtasks", "/Query", "/TN", name, "/V", "/FO", "LIST")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return TaskStatus{}, fmt.Errorf("schtasks /Query: %w: %s", err, string(out))
	}
	return parseTaskQueryOutput(string(out), name), nil
}

func (w *windowsScheduler) List(prefix string) ([]TaskStatus, error) {
	cmd := exec.Command("schtasks", "/Query", "/V", "/FO", "LIST")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("schtasks /Query: %w: %s", err, string(out))
	}
	// Split into records separated by blank lines; each record has "TaskName:" line.
	records := strings.Split(string(out), "\r\n\r\n")
	var results []TaskStatus
	for _, r := range records {
		status := parseTaskQueryOutput(r, "")
		if status.Name != "" && strings.HasPrefix(strings.TrimPrefix(status.Name, "\\"), prefix) {
			results = append(results, status)
		}
	}
	return results, nil
}

// parseTaskQueryOutput extracts key fields from schtasks /Query /V /FO LIST output.
func parseTaskQueryOutput(out string, nameHint string) TaskStatus {
	status := TaskStatus{Name: nameHint, LastResult: -1}
	for _, line := range strings.Split(out, "\r\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "TaskName:") {
			status.Name = strings.TrimSpace(strings.TrimPrefix(line, "TaskName:"))
		} else if strings.HasPrefix(line, "Status:") {
			status.State = strings.TrimSpace(strings.TrimPrefix(line, "Status:"))
		} else if strings.HasPrefix(line, "Last Result:") {
			fmt.Sscanf(strings.TrimPrefix(line, "Last Result:"), " %d", &status.LastResult)
		} else if strings.HasPrefix(line, "Next Run Time:") {
			status.NextRun = strings.TrimSpace(strings.TrimPrefix(line, "Next Run Time:"))
		}
	}
	return status
}
