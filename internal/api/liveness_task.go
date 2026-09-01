// Package api — supervisor-liveness scheduled-task install surface
// (v0.6 redesign spec §15 P1-b / §5.x Phase 3a + 3b).
//
// The liveness task is the v0.6 owner-death recovery: it drives `mcphub
// supervise --ensure-alive` every ~1 minute; that action probes the
// flock-authoritative SupervisorRunningUnderStateDir and, when no supervisor
// holds the lock, relaunches the owner via the autostart task.
//
// Phase 3a added it ALONGSIDE the still-present v0.4.x watchdog; Phase 3b
// (spec §5 Phase C/D) then DELETED the watchdog engine, leaving the liveness
// task as the sole maintenance-task install surface. This file therefore also
// hosts the canonical-exe / current-user resolvers migrated out of the deleted
// watchdog-side files (it is their surviving consumer) plus the
// RemoveLegacyWatchdogTask cleanup for existing hosts.
package api

import (
	"bytes"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os/user"
	"strings"
	"unicode/utf16"

	"mcp-local-hub/internal/scheduler"
)

// livenessWorkingDir returns the directory portion of the canonical mcphub
// executable path for the liveness task's <WorkingDirectory> element.
//
// It must NOT use filepath.Dir: path/filepath is OS-specific, so on a
// non-Windows host filepath.Dir of a Windows-shaped path
// ("C:\Users\…\bin\mcphub.exe") finds no '/' separator and returns "." —
// which makes the rendered XML differ by host OS and fails the
// cross-platform liveness test. The liveness task is a Windows-GA surface
// (scheduler.New() returns "not implemented" on POSIX), but its XML must
// render identically on any host so the test asserts one expected output.
//
// Splitting on the LAST separator of either kind ('\' or '/') yields the
// correct parent dir for both the Windows canonical path (backslash) and
// the POSIX canonical path (forward slash), independent of the host OS's
// filepath separator. A path with no separator returns itself verbatim
// (the original filepath.Dir(".")-style fallback never applied to a real
// absolute exe path).
func livenessWorkingDir(exePath string) string {
	if i := strings.LastIndexAny(exePath, `\/`); i >= 0 {
		return exePath[:i]
	}
	return exePath
}

// canonicalMcphubPathFn is the canonical-mcphub-path resolver used by the
// scheduled-task install Command field. Production: thin adapter over the
// internal canonicalMcphubPath() (install.go). Tests: inject a stub via
// SetTestCanonicalMcphubPathFn (testhooks.go).
//
// Migrated here from watchdog_xml_validator.go when the v0.6 redesign
// (spec §5 Phase D) deleted the watchdog engine — InstallLivenessTask
// (below) is the surviving consumer of both resolvers, so they live with
// the task they install rather than in deleted watchdog-side files.
var canonicalMcphubPathFn = func() (string, error) { return canonicalMcphubPath() }

// currentWindowsUserFn is the current-user resolver used by the
// scheduled-task install principal field. Production:
// defaultCurrentWindowsUser() (strips DOMAIN\\ prefix from
// user.Current().Username). Tests: inject via SetTestCurrentWindowsUserFn.
var currentWindowsUserFn = defaultCurrentWindowsUser

// defaultCurrentWindowsUser returns the bare username of the running
// process, stripping the DOMAIN\\ prefix Windows attaches to
// user.Current().Username.
func defaultCurrentWindowsUser() (string, error) {
	u, err := user.Current()
	if err != nil {
		return "", fmt.Errorf("user.Current: %w", err)
	}
	name := u.Username
	if i := strings.LastIndex(name, "\\"); i >= 0 {
		name = name[i+1:]
	}
	return name, nil
}

// LivenessTaskName is the canonical scheduled-task name installed alongside
// the daemon tasks by `mcphub setup` (Phase 3a). Kept as a package-level
// constant so callers reference one literal.
const LivenessTaskName = "\\mcp-local-hub-liveness"

// LivenessTaskResult identifies the pre-state that an EnsureLivenessTask
// receipt can restore. It is an internal transaction result, not a public CLI
// state string.
type LivenessTaskResult string

const (
	LivenessTaskUnchanged LivenessTaskResult = "unchanged"
	LivenessTaskCreated   LivenessTaskResult = "created-from-absent"
	LivenessTaskReplaced  LivenessTaskResult = "replaced-with-prior-xml"
)

// LivenessTaskReceipt is valid only for the setup transaction that received
// it. The captured settled XML is a compare-before-restore fence: rollback
// refuses to delete or overwrite a liveness task changed after settlement.
type LivenessTaskReceipt struct {
	Result     LivenessTaskResult
	priorXML   []byte
	settledXML []byte
}

var ErrLivenessTaskRollbackConflict = errors.New("api: liveness task rollback conflict")

// LivenessTaskPostImportStage identifies a failure after Task Scheduler has
// accepted a replacement liveness-task definition.
type LivenessTaskPostImportStage string

const (
	LivenessTaskPostImportReadback      LivenessTaskPostImportStage = "readback"
	LivenessTaskPostImportSemanticDrift LivenessTaskPostImportStage = "semantic-drift"
)

// LivenessTaskPostImportError preserves the original post-import failure and,
// when necessary, the failure to restore the exact captured pre-state.
type LivenessTaskPostImportError struct {
	Stage    LivenessTaskPostImportStage
	Cause    error
	Rollback error
}

func (e *LivenessTaskPostImportError) Error() string {
	if e.Rollback != nil {
		return fmt.Sprintf("liveness task post-import %s failure: %v; rollback failed: %v", e.Stage, e.Cause, e.Rollback)
	}
	return fmt.Sprintf("liveness task post-import %s failure: %v", e.Stage, e.Cause)
}

func (e *LivenessTaskPostImportError) Unwrap() []error {
	if e.Rollback == nil {
		return []error{e.Cause}
	}
	return []error{e.Cause, e.Rollback}
}

func (e *LivenessTaskPostImportError) LivenessTaskPostImportStage() string {
	return string(e.Stage)
}

func (e *LivenessTaskPostImportError) LivenessTaskRollbackError() error {
	return e.Rollback
}

// InstallLivenessTask is the idempotent install of the supervisor-liveness
// scheduled task: resolve the canonical mcphub.exe path + the current Windows
// user, render the liveness XML via scheduler.BuildLivenessXML, encode it
// for Task Scheduler's /XML parser, and ImportXML it under LivenessTaskName.
//
// CLI-level concerns (admin-elevation refusal, audit entry, interactive
// confirm) live in the CLI layer that calls this (runSetupWatchdog rides the
// same state-dir-sanity + elevation gates); this API method is the
// unconditional execution path.
//
// On Linux/macOS scheduler.New() returns "not implemented" so this fails
// loud at the ImportXML call — the liveness task is a Windows-GA capability
// in v0.6.
func (a *API) InstallLivenessTask() error {
	_, err := a.EnsureLivenessTask()
	return err
}

// EnsureLivenessTask settles the API-owned liveness task and returns an exact
// rollback receipt. The caller is responsible for proving the supervisor
// target first; this owner never creates or mutates that separate resource.
func (a *API) EnsureLivenessTask() (LivenessTaskReceipt, error) {
	canonicalExe, err := canonicalMcphubPathFn()
	if err != nil {
		return LivenessTaskReceipt{}, err
	}
	userName, err := currentWindowsUserFn()
	if err != nil {
		return LivenessTaskReceipt{}, err
	}
	workingDir := livenessWorkingDir(canonicalExe)
	sch, err := newScheduler()
	if err != nil {
		return LivenessTaskReceipt{}, err
	}
	prior, exportErr := sch.ExportXML(LivenessTaskName)
	receipt := LivenessTaskReceipt{}
	if exportErr != nil {
		if !errors.Is(exportErr, scheduler.ErrTaskNotFound) {
			return receipt, fmt.Errorf("snapshot liveness task: %w", exportErr)
		}
		receipt.Result = LivenessTaskCreated
	} else {
		receipt.Result = LivenessTaskReplaced
		receipt.priorXML = append([]byte(nil), prior...)
		if !livenessTaskReadable(prior) {
			return receipt, fmt.Errorf("liveness task definition is corrupt")
		}
		if livenessTaskDifference(prior, canonicalExe, workingDir, userName) == nil {
			receipt.Result = LivenessTaskUnchanged
			receipt.settledXML = append([]byte(nil), prior...)
			return receipt, nil
		}
	}
	xmlBytes := scheduler.EncodeXMLUTF16LEBOM(scheduler.BuildLivenessXML(canonicalExe, workingDir, userName))
	if err := sch.ImportXML(LivenessTaskName, xmlBytes); err != nil {
		return receipt, fmt.Errorf("import liveness task: %w", err)
	}
	settled, err := sch.ExportXML(LivenessTaskName)
	if err != nil {
		return receipt, livenessTaskPostImportFailure(sch, receipt, LivenessTaskPostImportReadback, fmt.Errorf("read back liveness task: %w", err))
	}
	receipt.settledXML = append([]byte(nil), settled...)
	if difference := livenessTaskDifference(settled, canonicalExe, workingDir, userName); difference != nil {
		return receipt, livenessTaskPostImportFailure(sch, receipt, LivenessTaskPostImportSemanticDrift, fmt.Errorf("liveness task definition drifted after import: %w", difference))
	}
	return receipt, nil
}

func livenessTaskPostImportFailure(sch scheduler.Scheduler, receipt LivenessTaskReceipt, stage LivenessTaskPostImportStage, cause error) error {
	return &LivenessTaskPostImportError{
		Stage:    stage,
		Cause:    cause,
		Rollback: restoreLivenessTaskSnapshot(sch, receipt),
	}
}

// RestoreLivenessTask restores only the exact pre-state captured in receipt.
func (a *API) RestoreLivenessTask(receipt LivenessTaskReceipt) error {
	if receipt.Result == LivenessTaskUnchanged {
		return nil
	}
	if len(receipt.settledXML) == 0 {
		return fmt.Errorf("%w: receipt has no settled liveness XML", ErrLivenessTaskRollbackConflict)
	}
	sch, err := newScheduler()
	if err != nil {
		return err
	}
	current, err := sch.ExportXML(LivenessTaskName)
	if err != nil || !bytes.Equal(current, receipt.settledXML) {
		return fmt.Errorf("%w: liveness task changed after settlement", ErrLivenessTaskRollbackConflict)
	}
	return restoreLivenessTaskSnapshot(sch, receipt)
}

// restoreLivenessTaskSnapshot restores the exact state captured before a
// replacement. Callers that run after settlement must establish their own
// ownership fence first; the immediate post-import failure path owns the same
// scheduler transaction and must always attempt this recovery.
func restoreLivenessTaskSnapshot(sch scheduler.Scheduler, receipt LivenessTaskReceipt) error {
	switch receipt.Result {
	case LivenessTaskCreated:
		if err := sch.Delete(LivenessTaskName); err != nil {
			return fmt.Errorf("delete created liveness task: %w", err)
		}
		if _, err := sch.ExportXML(LivenessTaskName); !errors.Is(err, scheduler.ErrTaskNotFound) {
			return fmt.Errorf("%w: created liveness task remains after deletion: %v", ErrLivenessTaskRollbackConflict, err)
		}
		return nil
	case LivenessTaskReplaced:
		if err := sch.ImportXML(LivenessTaskName, receipt.priorXML); err != nil {
			return fmt.Errorf("restore liveness task XML: %w", err)
		}
		restored, err := sch.ExportXML(LivenessTaskName)
		if err != nil || !bytes.Equal(restored, receipt.priorXML) {
			return fmt.Errorf("%w: liveness XML readback differs from captured pre-state", ErrLivenessTaskRollbackConflict)
		}
		return nil
	default:
		return fmt.Errorf("%w: receipt has no replaceable liveness task state", ErrLivenessTaskRollbackConflict)
	}
}

type LivenessTaskDifferenceField string

const (
	livenessTaskFieldDefinition                      LivenessTaskDifferenceField = "definition"
	livenessTaskFieldPrincipalCount                  LivenessTaskDifferenceField = "principal.count"
	livenessTaskFieldPrincipalUser                   LivenessTaskDifferenceField = "principal.user"
	livenessTaskFieldPrincipalLogonType              LivenessTaskDifferenceField = "principal.logon_type"
	livenessTaskFieldPrincipalRunLevel               LivenessTaskDifferenceField = "principal.run_level"
	livenessTaskFieldCalendarTriggerCount            LivenessTaskDifferenceField = "trigger.calendar.count"
	livenessTaskFieldCalendarRepetitionInterval      LivenessTaskDifferenceField = "trigger.calendar.repetition.interval"
	livenessTaskFieldCalendarDaysInterval            LivenessTaskDifferenceField = "trigger.calendar.schedule_by_day.days_interval"
	livenessTaskFieldCalendarStopAtDurationEnd       LivenessTaskDifferenceField = "trigger.calendar.repetition.stop_at_duration_end"
	livenessTaskFieldLogonTriggerCount               LivenessTaskDifferenceField = "trigger.logon.count"
	livenessTaskFieldLogonTriggerUser                LivenessTaskDifferenceField = "trigger.logon.user"
	livenessTaskFieldLogonTriggerEnabled             LivenessTaskDifferenceField = "trigger.logon.enabled"
	livenessTaskFieldSettingsEnabled                 LivenessTaskDifferenceField = "settings.enabled"
	livenessTaskFieldSettingsExecutionTimeLimit      LivenessTaskDifferenceField = "settings.execution_time_limit"
	livenessTaskFieldSettingsMultipleInstancesPolicy LivenessTaskDifferenceField = "settings.multiple_instances_policy"
	livenessTaskFieldActionContext                   LivenessTaskDifferenceField = "action.context"
	livenessTaskFieldActionCount                     LivenessTaskDifferenceField = "action.count"
	livenessTaskFieldActionCommand                   LivenessTaskDifferenceField = "action.command"
	livenessTaskFieldActionArguments                 LivenessTaskDifferenceField = "action.arguments"
	livenessTaskFieldActionWorkingDirectory          LivenessTaskDifferenceField = "action.working_directory"
)

// LivenessTaskDefinitionDifference identifies the first semantic field whose
// Task Scheduler readback differs from the canonical liveness definition.
type LivenessTaskDefinitionDifference struct {
	Field LivenessTaskDifferenceField
}

func (e *LivenessTaskDefinitionDifference) Error() string {
	return fmt.Sprintf("liveness task definition differs at %s", e.Field)
}

func (e *LivenessTaskDefinitionDifference) LivenessTaskDifferenceField() string {
	return string(e.Field)
}

type livenessPrincipalXML struct {
	UserID    string `xml:"UserId"`
	LogonType string `xml:"LogonType"`
	RunLevel  string `xml:"RunLevel"`
}

type livenessCalendarTriggerXML struct {
	Repetition struct {
		Interval          string `xml:"Interval"`
		StopAtDurationEnd string `xml:"StopAtDurationEnd"`
	} `xml:"Repetition"`
	ScheduleByDay struct {
		DaysInterval string `xml:"DaysInterval"`
	} `xml:"ScheduleByDay"`
}

type livenessLogonTriggerXML struct {
	UserID  string `xml:"UserId"`
	Enabled string `xml:"Enabled"`
}

type livenessExecXML struct {
	Command          string `xml:"Command"`
	Arguments        string `xml:"Arguments"`
	WorkingDirectory string `xml:"WorkingDirectory"`
}

type livenessSettingsXML struct {
	Enabled                 string `xml:"Enabled"`
	ExecutionTimeLimit      string `xml:"ExecutionTimeLimit"`
	MultipleInstancesPolicy string `xml:"MultipleInstancesPolicy"`
}

type livenessTaskXML struct {
	Principals      []livenessPrincipalXML
	CalendarTrigger []livenessCalendarTriggerXML
	LogonTrigger    []livenessLogonTriggerXML
	ActionContext   string
	Exec            []livenessExecXML
	Settings        livenessSettingsXML
}

func livenessTaskDifference(xmlBlob []byte, canonicalExe, workingDir, userName string) *LivenessTaskDefinitionDifference {
	actual, ok := parseLivenessTaskXML(xmlBlob)
	if !ok {
		return &LivenessTaskDefinitionDifference{Field: livenessTaskFieldDefinition}
	}
	expected, ok := parseLivenessTaskXML([]byte(scheduler.BuildLivenessXML(canonicalExe, workingDir, userName)))
	if !ok {
		return &LivenessTaskDefinitionDifference{Field: livenessTaskFieldDefinition}
	}
	difference := func(field LivenessTaskDifferenceField) *LivenessTaskDefinitionDifference {
		return &LivenessTaskDefinitionDifference{Field: field}
	}
	if len(actual.Principals) != len(expected.Principals) {
		return difference(livenessTaskFieldPrincipalCount)
	}
	principal, expectedPrincipal := actual.Principals[0], expected.Principals[0]
	if !scheduler.WindowsUsersEquivalent(principal.UserID, expectedPrincipal.UserID) {
		return difference(livenessTaskFieldPrincipalUser)
	}
	if principal.LogonType != expectedPrincipal.LogonType {
		return difference(livenessTaskFieldPrincipalLogonType)
	}
	if principal.RunLevel != expectedPrincipal.RunLevel {
		return difference(livenessTaskFieldPrincipalRunLevel)
	}
	if len(actual.CalendarTrigger) != len(expected.CalendarTrigger) {
		return difference(livenessTaskFieldCalendarTriggerCount)
	}
	calendar, expectedCalendar := actual.CalendarTrigger[0], expected.CalendarTrigger[0]
	if calendar.Repetition.Interval != expectedCalendar.Repetition.Interval {
		return difference(livenessTaskFieldCalendarRepetitionInterval)
	}
	if calendar.ScheduleByDay.DaysInterval != expectedCalendar.ScheduleByDay.DaysInterval {
		return difference(livenessTaskFieldCalendarDaysInterval)
	}
	if !sameTaskXMLBoolean(calendar.Repetition.StopAtDurationEnd, expectedCalendar.Repetition.StopAtDurationEnd) {
		return difference(livenessTaskFieldCalendarStopAtDurationEnd)
	}
	if len(actual.LogonTrigger) != len(expected.LogonTrigger) {
		return difference(livenessTaskFieldLogonTriggerCount)
	}
	logon, expectedLogon := actual.LogonTrigger[0], expected.LogonTrigger[0]
	if !scheduler.WindowsUsersEquivalent(logon.UserID, expectedLogon.UserID) {
		return difference(livenessTaskFieldLogonTriggerUser)
	}
	if !sameTaskXMLBoolean(logon.Enabled, expectedLogon.Enabled) {
		return difference(livenessTaskFieldLogonTriggerEnabled)
	}
	if !sameTaskXMLBoolean(actual.Settings.Enabled, expected.Settings.Enabled) {
		return difference(livenessTaskFieldSettingsEnabled)
	}
	if actual.Settings.ExecutionTimeLimit != expected.Settings.ExecutionTimeLimit {
		return difference(livenessTaskFieldSettingsExecutionTimeLimit)
	}
	if actual.Settings.MultipleInstancesPolicy != expected.Settings.MultipleInstancesPolicy {
		return difference(livenessTaskFieldSettingsMultipleInstancesPolicy)
	}
	if actual.ActionContext != expected.ActionContext {
		return difference(livenessTaskFieldActionContext)
	}
	if len(actual.Exec) != len(expected.Exec) {
		return difference(livenessTaskFieldActionCount)
	}
	action, expectedAction := actual.Exec[0], expected.Exec[0]
	if action.Command != expectedAction.Command {
		return difference(livenessTaskFieldActionCommand)
	}
	if action.Arguments != expectedAction.Arguments {
		return difference(livenessTaskFieldActionArguments)
	}
	if action.WorkingDirectory != expectedAction.WorkingDirectory {
		return difference(livenessTaskFieldActionWorkingDirectory)
	}
	return nil
}

func sameTaskXMLBoolean(left, right string) bool {
	normalize := func(value string) (bool, bool) {
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "true", "1":
			return true, true
		case "false", "0":
			return false, true
		default:
			return false, false
		}
	}
	leftValue, leftOK := normalize(left)
	rightValue, rightOK := normalize(right)
	return leftOK && rightOK && leftValue == rightValue
}

func livenessTaskReadable(xmlBlob []byte) bool {
	_, ok := parseLivenessTaskXML(xmlBlob)
	return ok
}

func parseLivenessTaskXML(xmlBlob []byte) (livenessTaskXML, bool) {
	type taskRoot struct {
		Triggers struct {
			Calendar []livenessCalendarTriggerXML `xml:"CalendarTrigger"`
			Logon    []livenessLogonTriggerXML    `xml:"LogonTrigger"`
		} `xml:"Triggers"`
		Principals struct {
			Principal []livenessPrincipalXML `xml:"Principal"`
		} `xml:"Principals"`
		Settings livenessSettingsXML `xml:"Settings"`
		Actions  struct {
			Context string            `xml:"Context,attr"`
			Exec    []livenessExecXML `xml:"Exec"`
		} `xml:"Actions"`
	}
	if len(xmlBlob) >= 2 && xmlBlob[0] == 0xff && xmlBlob[1] == 0xfe && (len(xmlBlob)-2)%2 == 0 {
		units := make([]uint16, 0, (len(xmlBlob)-2)/2)
		for i := 2; i < len(xmlBlob); i += 2 {
			units = append(units, binary.LittleEndian.Uint16(xmlBlob[i:i+2]))
		}
		xmlBlob = []byte(string(utf16.Decode(units)))
	}
	decoder := xml.NewDecoder(bytes.NewReader(xmlBlob))
	decoder.CharsetReader = func(_ string, reader io.Reader) (io.Reader, error) { return reader, nil }
	var task taskRoot
	if err := decoder.Decode(&task); err != nil {
		return livenessTaskXML{}, false
	}
	if len(task.Principals.Principal) == 0 || len(task.Actions.Exec) == 0 ||
		task.Principals.Principal[0].UserID == "" || task.Actions.Exec[0].Command == "" || task.Actions.Exec[0].Arguments == "" {
		return livenessTaskXML{}, false
	}
	return livenessTaskXML{
		Principals:      task.Principals.Principal,
		CalendarTrigger: task.Triggers.Calendar,
		LogonTrigger:    task.Triggers.Logon,
		ActionContext:   task.Actions.Context,
		Exec:            task.Actions.Exec,
		Settings:        task.Settings,
	}, true
}

// UninstallLivenessTask is the idempotent removal of the supervisor-liveness
// scheduled task.
//
// Wired into the CLI uninstall path at internal/cli/setup.go's
// runUninstallWatchdog, INSIDE the same last-server partial-uninstall gate
// (shouldRemoveGlobalWatchdog) that authorizes the legacy-watchdog teardown:
// the liveness task is a hub-wide shared maintenance job, so it is removed only
// when the last managed server is uninstalled, never while peer servers
// remain. The call is non-fatal / idempotent there — scheduler.Delete
// returns nil for an absent task.
func (a *API) UninstallLivenessTask() error {
	sch, err := newScheduler()
	if err != nil {
		return err
	}
	return sch.Delete(LivenessTaskName)
}

// LegacyWatchdogTaskName is the canonical name of the v0.4.x watchdog
// scheduled task. The watchdog ENGINE was deleted in the v0.6 redesign
// (spec §5 Phase D); this literal survives ONLY so existing hosts can
// have the leftover task removed by `mcphub setup` / `mcphub uninstall`.
// Kept here (not in a deleted watchdog-side file) so the one place that
// still needs the literal owns it.
const LegacyWatchdogTaskName = "\\mcp-local-hub-watchdog"

// RemoveLegacyWatchdogTask deletes the leftover v0.4.x
// `\mcp-local-hub-watchdog` scheduled task on existing hosts. The v0.6
// supervisor owns daemon revival via its Job-Object reaper + reconcile
// loop, and owner-death recovery is the liveness task — so the legacy
// watchdog is a no-op vestige that actively fights the supervisor every
// 5 min (it writes "suspicious-xml" warnings against the v0.5.0 task XML
// its v0.4.x validator can no longer parse).
//
// Idempotent + best-effort by contract: scheduler.Delete returns nil for
// an absent task (clean hosts), so callers treat this as non-fatal. Routes
// through the same newScheduler() factory seam as UninstallLivenessTask so
// tests can drive it with a recording fake.
func (a *API) RemoveLegacyWatchdogTask() error {
	sch, err := newScheduler()
	if err != nil {
		return err
	}
	return sch.Delete(LegacyWatchdogTaskName)
}
