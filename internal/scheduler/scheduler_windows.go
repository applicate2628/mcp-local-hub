//go:build windows

package scheduler

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"mcp-local-hub/internal/process"
)

// windowsScheduler shells out to `schtasks.exe` for all operations.
// We build a Task Scheduler XML document per spec, pipe it to `schtasks /Create /XML`,
// and parse the output of `/Query` for Status/List.
type windowsScheduler struct {
	username     string // e.g., "dima_"
	schtasksPath string
}

func newPlatformScheduler() (Scheduler, error) {
	u, err := user.Current()
	if err != nil {
		return nil, fmt.Errorf("user.Current: %w", err)
	}
	schtasksPath, err := resolveSchtasksPath()
	if err != nil {
		return nil, err
	}
	// u.Username is typically "MACHINE\\user" on Windows — strip the domain.
	name := u.Username
	if i := strings.LastIndex(name, "\\"); i >= 0 {
		name = name[i+1:]
	}
	return &windowsScheduler{username: name, schtasksPath: schtasksPath}, nil
}

func resolveSchtasksPath() (string, error) {
	systemRoot := os.Getenv("SystemRoot")
	if systemRoot == "" {
		systemRoot = os.Getenv("WINDIR")
	}
	if systemRoot == "" {
		return "", fmt.Errorf("resolve schtasks path: SystemRoot/WINDIR is empty")
	}
	return filepath.Join(systemRoot, "System32", "schtasks.exe"), nil
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
		// Weekly recurrence lives inside <CalendarTrigger>, not as a standalone
		// <WeeklyTrigger> child of <Triggers>. Task Scheduler rejects the flat
		// form with:
		//     ERROR: The task XML contains an unexpected node. (N,M):WeeklyTrigger:
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

	// Principal — run as current user, interactive (needs session)
	buf.WriteString("  <Principals>\n")
	buf.WriteString("    <Principal id=\"Author\">\n")
	buf.WriteString(fmt.Sprintf("      <UserId>%s</UserId>\n", xmlEscape(userName)))
	buf.WriteString("      <LogonType>InteractiveToken</LogonType>\n")
	buf.WriteString("      <RunLevel>LeastPrivilege</RunLevel>\n")
	buf.WriteString("    </Principal>\n")
	buf.WriteString("  </Principals>\n")

	// Settings — restart policy + sane defaults
	//
	// Per Task Scheduler XML schema, the retry policy is a single <RestartOnFailure>
	// container holding <Interval> + <Count> — NOT flat <RestartInterval>/<RestartCount>
	// siblings. Task Scheduler rejects the flat form with
	//     ERROR: The task XML contains an unexpected node. (N,M):RestartInterval:
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
	buf.WriteString("    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>\n")
	buf.WriteString("    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>\n")
	buf.WriteString("    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>\n")
	buf.WriteString("    <IdleSettings>\n      <StopOnIdleEnd>false</StopOnIdleEnd>\n      <RestartOnIdle>false</RestartOnIdle>\n    </IdleSettings>\n")
	buf.WriteString("    <AllowStartOnDemand>true</AllowStartOnDemand>\n")
	buf.WriteString("    <Enabled>true</Enabled>\n")
	buf.WriteString("    <Hidden>true</Hidden>\n")
	buf.WriteString("    <RunOnlyIfIdle>false</RunOnlyIfIdle>\n")
	buf.WriteString("    <WakeToRun>false</WakeToRun>\n")
	buf.WriteString("    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>\n") // no timeout
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

// joinTaskArgs concatenates args into a Windows-compatible command line.
// Each arg is escaped via syscall.EscapeArg (stdlib — implements the same
// CommandLineToArgvW-compatible rules Go's own os/exec uses on Windows),
// then joined with single spaces. Without this, arguments containing
// spaces or embedded quotes were fed unescaped into the Task Scheduler
// XML `<Arguments>` element and split into multiple argv tokens at child
// launch — breaking any workspace path like
// `C:\Users\Test User\workspace`.
//
// syscall.EscapeArg is a no-op for args that contain none of the shell
// metacharacters it guards against (space, tab, `"`, `\`), so previously
// simple flag values like `--port` / `9200` / `--language` / `go` render
// identically to the pre-fix output.
func joinTaskArgs(args []string) string {
	if len(args) == 0 {
		return ""
	}
	escaped := make([]string, len(args))
	for i, a := range args {
		escaped[i] = syscall.EscapeArg(a)
	}
	return strings.Join(escaped, " ")
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

	cmd := exec.Command(w.schtasksPath, "/Create", "/TN", spec.Name, "/XML", tmp.Name(), "/F")
	process.NoConsole(cmd)
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
	cmd := exec.Command(w.schtasksPath, "/Delete", "/TN", name, "/F")
	process.NoConsole(cmd)
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

// ExportXML dumps the full Task Scheduler XML for a task via
// `schtasks /Query /XML`. Returns a "not found" error (captured as
// ErrTaskNotFound so callers can distinguish a missing task from other
// failures) when the task does not exist.
func (w *windowsScheduler) ExportXML(name string) ([]byte, error) {
	cmd := exec.Command(w.schtasksPath, "/Query", "/TN", name, "/XML")
	process.NoConsole(cmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if strings.Contains(string(out), "cannot find") || strings.Contains(string(out), "does not exist") {
			return nil, ErrTaskNotFound
		}
		return nil, fmt.Errorf("schtasks /Query: %w: %s", err, string(out))
	}
	return out, nil
}

// ImportXML re-creates a task from raw Task Scheduler XML via
// `schtasks /Create /XML`. Used by install's rollback path to restore
// a task that was deleted in preparation for a re-install that then
// failed mid-sequence. Idempotent: if the task already exists, /F
// overwrites it.
func (w *windowsScheduler) ImportXML(name string, xml []byte) error {
	tmp, err := os.CreateTemp("", "mcp-task-restore-*.xml")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(xml); err != nil {
		tmp.Close()
		return fmt.Errorf("write xml: %w", err)
	}
	tmp.Close()
	cmd := exec.Command(w.schtasksPath, "/Create", "/TN", name, "/XML", tmp.Name(), "/F")
	process.NoConsole(cmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("schtasks /Create /XML: %w: %s", err, string(out))
	}
	return nil
}

func (w *windowsScheduler) Run(name string) error {
	cmd := exec.Command(w.schtasksPath, "/Run", "/TN", name)
	process.NoConsole(cmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("schtasks /Run: %w: %s", err, string(out))
	}
	return nil
}

func (w *windowsScheduler) Stop(name string) error {
	cmd := exec.Command(w.schtasksPath, "/End", "/TN", name)
	process.NoConsole(cmd)
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
	cmd := exec.Command(w.schtasksPath, "/Query", "/TN", name, "/V", "/FO", "LIST")
	process.NoConsole(cmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return TaskStatus{}, fmt.Errorf("schtasks /Query: %w: %s", err, string(out))
	}
	return parseTaskQueryOutput(string(out), name), nil
}

func (w *windowsScheduler) List(prefix string) ([]TaskStatus, error) {
	cmd := exec.Command(w.schtasksPath, "/Query", "/V", "/FO", "LIST")
	process.NoConsole(cmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("schtasks /Query: %w: %s", err, string(out))
	}
	// Split into records separated by blank lines; each record has "TaskName:" line.
	records := strings.Split(string(out), "\r\n\r\n")
	var results []TaskStatus
	for _, r := range records {
		status := parseTaskQueryOutput(r, "")
		if status.Name != "" &&
			strings.HasPrefix(strings.TrimPrefix(status.Name, "\\"), prefix) &&
			sameWindowsUser(status.Owner, w.username) {
			results = append(results, status)
		}
	}
	return results, nil
}

// parseTaskQueryOutput extracts key fields from schtasks /Query /V /FO LIST output.
func parseTaskQueryOutput(out string, nameHint string) TaskStatus {
	status := TaskStatus{Name: nameHint, LastResult: -1}
	for line := range strings.SplitSeq(out, "\r\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "TaskName:") {
			status.Name = strings.TrimSpace(strings.TrimPrefix(line, "TaskName:"))
		} else if strings.HasPrefix(line, "Status:") {
			status.State = strings.TrimSpace(strings.TrimPrefix(line, "Status:"))
		} else if strings.HasPrefix(line, "Last Result:") {
			fmt.Sscanf(strings.TrimPrefix(line, "Last Result:"), " %d", &status.LastResult)
		} else if strings.HasPrefix(line, "Next Run Time:") {
			status.NextRun = strings.TrimSpace(strings.TrimPrefix(line, "Next Run Time:"))
		} else if strings.HasPrefix(line, "Run As User:") {
			status.Owner = strings.TrimSpace(strings.TrimPrefix(line, "Run As User:"))
		}
	}
	return status
}

// sameWindowsUser compares a Task Scheduler "Run As User" value against the
// current user's short username (no DOMAIN\ prefix), case-insensitively.
func sameWindowsUser(owner, currentShortName string) bool {
	owner = strings.TrimSpace(owner)
	if owner == "" || currentShortName == "" {
		return false
	}
	owner = strings.ToLower(owner)
	currentShortName = strings.ToLower(currentShortName)
	if owner == currentShortName {
		return true
	}
	if i := strings.LastIndex(owner, `\`); i >= 0 {
		return owner[i+1:] == currentShortName
	}
	return false
}
