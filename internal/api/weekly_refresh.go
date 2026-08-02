// Package api — shared weekly-refresh task + WeeklyRefreshAll (M4 Task 13).
//
// Lazy-mode contract: the weekly refresh restarts the PROXY only (each
// per-(workspace, language) scheduler task launched by Register). The
// proxy's startup already writes Lifecycle=Configured to the registry, and
// re-materialization of the heavy backend happens lazily on the next
// tools/call. No explicit backend restart logic is needed in this file —
// that behavior falls out of the proxy's normal startup path.
//
// Shape:
//   - ONE shared scheduler task named WeeklyRefreshTaskName, created by
//     EnsureWeeklyRefreshTask (idempotent). Fires Sunday 03:00 local and
//     invokes `mcphub workspace-weekly-refresh` (the CLI subcommand M5
//     Task 17 will wire). Until M5 wires that subcommand, a manual trigger
//     of the scheduler task will error cleanly — acceptable because the
//     weekly trigger fires on a schedule, not at registration time.
//   - WeeklyRefreshAll reads the registry and restarts every entry whose
//     WeeklyRefresh flag is true. Best-effort: per-entry Run failures are
//     recorded in Warnings without aborting the run.
package api

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"mcp-local-hub/internal/scheduler"
)

// WeeklyRefreshTaskName is the single shared scheduler task name that invokes
// `mcphub workspace-weekly-refresh` on a weekly trigger. One task, regardless
// of how many workspaces are registered — per Phase 3 decision recorded in
// the plan's M4 section.
const WeeklyRefreshTaskName = "mcp-local-hub-workspace-weekly-refresh"

// ErrWeeklyRefreshConflict classifies a failed weekly-task replacement that
// observed a foreign current generation and therefore preserved it.
var (
	ErrWeeklyRefreshConflict            = errors.New("weekly refresh task changed concurrently; foreign task preserved")
	ErrWeeklyRefreshSnapshotUnavailable = errors.New("weekly refresh task snapshot is unavailable")
	ErrWeeklyScheduleSettingsWrite      = errors.New("weekly schedule settings update failed")
)

const weeklyRefreshLockSuffix = ".scheduler.lock"

type weeklyRefreshTaskScheduler interface {
	ExportXML(name string) ([]byte, error)
	Delete(name string) error
	Create(spec scheduler.TaskSpec) error
	ImportXML(name string, xml []byte) error
}

type weeklyRefreshTransactionState string

const (
	weeklyRefreshTaskCommitted                 weeklyRefreshTransactionState = "weekly-task-committed"
	weeklyRefreshTaskAppliedReleaseUnconfirmed weeklyRefreshTransactionState = "weekly-task-applied-release-unconfirmed"
	weeklyRefreshTaskRestored                  weeklyRefreshTransactionState = "weekly-task-restored"
)

type weeklyRefreshMutation struct {
	taskName    string
	expectedXML []byte
	expectedSet bool
	desired     func(priorXML []byte, exists bool) (scheduler.TaskSpec, error)
}

type weeklyRefreshTransactionResult struct {
	state         weeklyRefreshTransactionState
	restoreStatus string
	finalXML      []byte
}

// WeeklyRefreshReport lists the task names that were (re)started by this
// run. Per-entry failures go in Warnings; the overall call still returns
// nil unless registry/scheduler construction fails up-front.
type WeeklyRefreshReport struct {
	Restarted []string `json:"restarted"`
	Warnings  []string `json:"warnings,omitempty"`
}

// EnsureWeeklyRefreshTask creates the shared weekly refresh task if it does
// not already exist. Idempotent — replaces any prior task with the same
// name. Fires Sunday 03:00 local time and invokes
// `mcphub workspace-weekly-refresh`, the CLI counterpart of
// WeeklyRefreshAll. The CLI subcommand itself is wired in M5 Task 17; until
// then a manual run of this task will error cleanly, which is acceptable
// because the schedule only fires weekly.
func (a *API) EnsureWeeklyRefreshTask() error {
	sch, err := schedulerNewForRegister()
	if err != nil {
		return err
	}
	canonical, err := canonicalMcphubPath()
	if err != nil {
		return err
	}
	return a.withWeeklyScheduleSettings(weeklyScheduleSettingsRead, "", func(current string, _ func() error) error {
		schedule, parseErr := ParseSchedule(current)
		if parseErr != nil {
			return fmt.Errorf("parse persisted weekly schedule: %w", parseErr)
		}
		_, transactionErr := runWeeklyRefreshTaskTransaction(sch, weeklyRefreshMutation{
			taskName: WeeklyRefreshTaskName,
			desired: func([]byte, bool) (scheduler.TaskSpec, error) {
				return weeklyRefreshTaskSpec(canonical, schedule), nil
			},
		})
		return transactionErr
	})
}

// ApplyWeeklyRefreshSchedule updates the persisted weekly schedule and its
// singleton scheduler task as one settings-then-scheduler transaction. Its
// restore status is intentionally the existing GUI route vocabulary.
func (a *API) ApplyWeeklyRefreshSchedule(schedule *ScheduleSpec) (restoreStatus string, err error) {
	if schedule == nil || schedule.Kind != ScheduleWeekly {
		return "n/a", errors.New("weekly schedule is required")
	}
	sch, err := schedulerNewForRegister()
	if err != nil {
		return "n/a", err
	}
	canonicalPath, err := canonicalMcphubPath()
	if err != nil {
		return "n/a", err
	}
	restoreStatus = "n/a"
	callbackEntered := false
	err = a.withWeeklyScheduleSettings(weeklyScheduleSettingsUpdate, schedule.Canonical(), func(_ string, rollback func() error) error {
		callbackEntered = true
		result, transactionErr := runWeeklyRefreshTaskTransaction(sch, weeklyRefreshMutation{
			taskName: WeeklyRefreshTaskName,
			desired: func([]byte, bool) (scheduler.TaskSpec, error) {
				return weeklyRefreshTaskSpec(canonicalPath, schedule), nil
			},
		})
		restoreStatus = result.restoreStatus
		if result.state == weeklyRefreshTaskCommitted && transactionErr == nil {
			return nil
		}
		if result.state == weeklyRefreshTaskAppliedReleaseUnconfirmed {
			return transactionErr
		}
		if rollbackErr := rollback(); rollbackErr != nil {
			restoreStatus = "failed"
			return errors.Join(transactionErr, fmt.Errorf("restore prior weekly schedule: %w", rollbackErr))
		}
		return transactionErr
	})
	if err != nil && !callbackEntered {
		err = fmt.Errorf("%w: %w", ErrWeeklyScheduleSettingsWrite, err)
	}
	return restoreStatus, err
}

func weeklyRefreshTaskSpec(canonicalPath string, schedule *ScheduleSpec) scheduler.TaskSpec {
	trigger := scheduler.WeeklyTrigger{DayOfWeek: 0, HourLocal: 3, MinuteLocal: 0}
	if schedule != nil {
		trigger = scheduler.WeeklyTrigger{
			DayOfWeek:   schedule.DayOfWeek,
			HourLocal:   schedule.Hour,
			MinuteLocal: schedule.Minute,
		}
	}
	return scheduler.TaskSpec{
		Name:          WeeklyRefreshTaskName,
		Description:   "mcp-local-hub: weekly refresh of workspace-scoped lazy proxies",
		Command:       canonicalPath,
		Args:          []string{"workspace-weekly-refresh"},
		WorkingDir:    filepath.Dir(canonicalPath),
		WeeklyTrigger: &trigger,
	}
}

// runWeeklyRefreshTaskTransaction is the sole owner of the weekly task's
// export/delete/create/import lifecycle. Every production writer uses its
// existing singleton lock and returns only after the final XML is verified.
func runWeeklyRefreshTaskTransaction(sch weeklyRefreshTaskScheduler, mutation weeklyRefreshMutation) (result weeklyRefreshTransactionResult, err error) {
	result.restoreStatus = "n/a"
	if sch == nil || mutation.desired == nil {
		return result, errors.New("weekly refresh transaction is missing a scheduler or desired task")
	}
	taskName := strings.TrimSpace(mutation.taskName)
	if taskName == "" {
		taskName = WeeklyRefreshTaskName
	}
	stateDir, err := DaemonStateDir()
	if err != nil {
		return result, fmt.Errorf("resolve weekly refresh lock directory: %w", err)
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return result, fmt.Errorf("create weekly refresh lock directory: %w", err)
	}
	lockPath := filepath.Join(stateDir, WeeklyRefreshTaskName+weeklyRefreshLockSuffix)
	release, err := lockLeafLedgered(lockPath)
	if err != nil {
		return result, fmt.Errorf("lock weekly refresh singleton %s: %w", lockPath, err)
	}
	candidate, candidateErr := func() (weeklyRefreshTransactionResult, error) {
		result := weeklyRefreshTransactionResult{restoreStatus: "n/a"}
		priorXML, exportErr := sch.ExportXML(taskName)
		exists := exportErr == nil
		if exportErr != nil && !errors.Is(exportErr, scheduler.ErrTaskNotFound) {
			return result, fmt.Errorf("%w: export prior %s: %w", ErrWeeklyRefreshSnapshotUnavailable, taskName, exportErr)
		}
		if mutation.expectedSet {
			if exists != (mutation.expectedXML != nil) || (exists && !bytes.Equal(priorXML, mutation.expectedXML)) {
				return result, fmt.Errorf("%w: expected weekly task generation is stale", ErrWeeklyRefreshConflict)
			}
		}
		spec, desiredErr := mutation.desired(priorXML, exists)
		if desiredErr != nil {
			return result, desiredErr
		}
		spec.Name = taskName
		if deleteErr := sch.Delete(taskName); deleteErr != nil {
			return result, fmt.Errorf("delete prior %s: %w", taskName, deleteErr)
		}
		if createErr := sch.Create(spec); createErr != nil {
			currentXML, currentErr := sch.ExportXML(taskName)
			switch {
			case currentErr == nil:
				matches, classifyErr := weeklyTaskXMLMatchesSpec(currentXML, spec)
				if classifyErr == nil && matches {
					result.state = weeklyRefreshTaskCommitted
					result.finalXML = append([]byte(nil), currentXML...)
					return result, nil
				}
				if exists && bytes.Equal(currentXML, priorXML) {
					result.state = weeklyRefreshTaskRestored
					result.restoreStatus = "ok"
					return result, fmt.Errorf("create %s: %w", taskName, createErr)
				}
				return result, errors.Join(
					fmt.Errorf("create %s: %w", taskName, createErr),
					fmt.Errorf("%w: classify current task after create failure: %v", ErrWeeklyRefreshConflict, classifyErr),
				)
			case errors.Is(currentErr, scheduler.ErrTaskNotFound):
				if !exists {
					return result, fmt.Errorf("create %s: %w", taskName, createErr)
				}
				if restoreErr := sch.ImportXML(taskName, priorXML); restoreErr != nil {
					result.restoreStatus = "degraded"
					return result, errors.Join(
						fmt.Errorf("create %s: %w", taskName, createErr),
						fmt.Errorf("restore prior %s: %w", taskName, restoreErr),
					)
				}
				restoredXML, verifyErr := sch.ExportXML(taskName)
				if verifyErr != nil || !bytes.Equal(restoredXML, priorXML) {
					result.restoreStatus = "degraded"
					return result, errors.Join(
						fmt.Errorf("create %s: %w", taskName, createErr),
						fmt.Errorf("%w: restored prior task did not verify: %v", ErrWeeklyRefreshConflict, verifyErr),
					)
				}
				result.state = weeklyRefreshTaskRestored
				result.restoreStatus = "ok"
				return result, fmt.Errorf("create %s: %w", taskName, createErr)
			default:
				return result, errors.Join(
					fmt.Errorf("create %s: %w", taskName, createErr),
					fmt.Errorf("classify current %s: %w", taskName, currentErr),
				)
			}
		}
		settledXML, verifyErr := sch.ExportXML(taskName)
		if verifyErr != nil {
			return result, fmt.Errorf("verify created %s: %w", taskName, verifyErr)
		}
		matches, classifyErr := weeklyTaskXMLMatchesSpec(settledXML, spec)
		if classifyErr != nil {
			return result, fmt.Errorf("%w: verify created %s: %v", ErrWeeklyRefreshConflict, taskName, classifyErr)
		}
		if !matches {
			return result, fmt.Errorf("%w: created %s does not match the requested generation", ErrWeeklyRefreshConflict, taskName)
		}
		result.state = weeklyRefreshTaskCommitted
		result.finalXML = append([]byte(nil), settledXML...)
		return result, nil
	}()
	if releaseErr := release(); releaseErr != nil {
		switch candidate.state {
		case weeklyRefreshTaskCommitted:
			candidate.state = weeklyRefreshTaskAppliedReleaseUnconfirmed
			candidate.restoreStatus = "n/a"
		case weeklyRefreshTaskRestored:
			candidate.state = ""
		}
		return candidate, errors.Join(candidateErr, fmt.Errorf("release weekly refresh singleton lock: %w", releaseErr))
	}
	return candidate, candidateErr
}

type weeklyTaskXMLDocument struct {
	RegistrationInfo struct {
		Description string `xml:"Description"`
	} `xml:"RegistrationInfo"`
	Triggers struct {
		CalendarTrigger struct {
			StartBoundary  string `xml:"StartBoundary"`
			Enabled        string `xml:"Enabled"`
			ScheduleByWeek struct {
				WeeksInterval int `xml:"WeeksInterval"`
				DaysOfWeek    struct {
					Sunday    *struct{} `xml:"Sunday"`
					Monday    *struct{} `xml:"Monday"`
					Tuesday   *struct{} `xml:"Tuesday"`
					Wednesday *struct{} `xml:"Wednesday"`
					Thursday  *struct{} `xml:"Thursday"`
					Friday    *struct{} `xml:"Friday"`
					Saturday  *struct{} `xml:"Saturday"`
				} `xml:"DaysOfWeek"`
			} `xml:"ScheduleByWeek"`
		} `xml:"CalendarTrigger"`
	} `xml:"Triggers"`
	Principals struct {
		Principal struct {
			ID        string `xml:"id,attr"`
			LogonType string `xml:"LogonType"`
			RunLevel  string `xml:"RunLevel"`
		} `xml:"Principal"`
	} `xml:"Principals"`
	Settings struct {
		RestartOnFailure *struct {
			Interval string `xml:"Interval"`
			Count    int    `xml:"Count"`
		} `xml:"RestartOnFailure"`
		AllowHardTerminate         string `xml:"AllowHardTerminate"`
		StartWhenAvailable         string `xml:"StartWhenAvailable"`
		RunOnlyIfNetworkAvailable  string `xml:"RunOnlyIfNetworkAvailable"`
		MultipleInstancesPolicy    string `xml:"MultipleInstancesPolicy"`
		DisallowStartIfOnBatteries string `xml:"DisallowStartIfOnBatteries"`
		StopIfGoingOnBatteries     string `xml:"StopIfGoingOnBatteries"`
		IdleSettings               struct {
			StopOnIdleEnd string `xml:"StopOnIdleEnd"`
			RestartOnIdle string `xml:"RestartOnIdle"`
		} `xml:"IdleSettings"`
		AllowStartOnDemand string `xml:"AllowStartOnDemand"`
		Enabled            string `xml:"Enabled"`
		Hidden             string `xml:"Hidden"`
		RunOnlyIfIdle      string `xml:"RunOnlyIfIdle"`
		WakeToRun          string `xml:"WakeToRun"`
		ExecutionTimeLimit string `xml:"ExecutionTimeLimit"`
		Priority           int    `xml:"Priority"`
	} `xml:"Settings"`
	Actions struct {
		Context string `xml:"Context,attr"`
		Exec    struct {
			Command          string `xml:"Command"`
			Arguments        string `xml:"Arguments"`
			WorkingDirectory string `xml:"WorkingDirectory"`
		} `xml:"Exec"`
	} `xml:"Actions"`
}

func weeklyTaskXMLMatchesSpec(raw []byte, spec scheduler.TaskSpec) (bool, error) {
	if spec.WeeklyTrigger == nil {
		return false, errors.New("weekly task spec has no weekly trigger")
	}
	body := stripTaskXMLDeclaration(raw)
	decoder := xml.NewDecoder(bytes.NewReader(body))
	decoder.Strict = true
	decoder.Entity = nil
	decoder.CharsetReader = nil
	var document weeklyTaskXMLDocument
	if err := decoder.Decode(&document); err != nil {
		return false, err
	}
	start, err := time.Parse("2006-01-02T15:04:05", strings.TrimSpace(document.Triggers.CalendarTrigger.StartBoundary))
	if err != nil {
		return false, fmt.Errorf("parse StartBoundary: %w", err)
	}
	dayMatches := weeklyDaysMatch(document.Triggers.CalendarTrigger.ScheduleByWeek.DaysOfWeek, spec.WeeklyTrigger.DayOfWeek)
	restartMatches := document.Settings.RestartOnFailure == nil
	if spec.RestartOnFailure {
		restartMatches = document.Settings.RestartOnFailure != nil &&
			strings.TrimSpace(document.Settings.RestartOnFailure.Interval) == "PT1M" &&
			document.Settings.RestartOnFailure.Count == 3
	}
	return strings.TrimSpace(document.RegistrationInfo.Description) == spec.Description &&
		sameTaskPath(document.Actions.Exec.Command, spec.Command) &&
		sameTaskPath(document.Actions.Exec.WorkingDirectory, spec.WorkingDir) &&
		strings.TrimSpace(document.Actions.Exec.Arguments) == strings.Join(spec.Args, " ") &&
		dayMatches && document.Triggers.CalendarTrigger.ScheduleByWeek.WeeksInterval == 1 &&
		xmlBool(document.Triggers.CalendarTrigger.Enabled, true) &&
		start.Hour() == spec.WeeklyTrigger.HourLocal && start.Minute() == spec.WeeklyTrigger.MinuteLocal &&
		document.Principals.Principal.ID == "Author" &&
		strings.TrimSpace(document.Principals.Principal.LogonType) == "InteractiveToken" &&
		strings.TrimSpace(document.Principals.Principal.RunLevel) == "LeastPrivilege" &&
		document.Actions.Context == "Author" && restartMatches &&
		xmlBool(document.Settings.AllowHardTerminate, true) &&
		xmlBool(document.Settings.StartWhenAvailable, false) &&
		xmlBool(document.Settings.RunOnlyIfNetworkAvailable, false) &&
		strings.TrimSpace(document.Settings.MultipleInstancesPolicy) == "StopExisting" &&
		xmlBool(document.Settings.DisallowStartIfOnBatteries, false) &&
		xmlBool(document.Settings.StopIfGoingOnBatteries, false) &&
		xmlBool(document.Settings.IdleSettings.StopOnIdleEnd, false) &&
		xmlBool(document.Settings.IdleSettings.RestartOnIdle, false) &&
		xmlBool(document.Settings.AllowStartOnDemand, true) && xmlBool(document.Settings.Enabled, true) &&
		xmlBool(document.Settings.Hidden, false) && xmlBool(document.Settings.RunOnlyIfIdle, false) &&
		xmlBool(document.Settings.WakeToRun, false) && strings.TrimSpace(document.Settings.ExecutionTimeLimit) == "PT0S" &&
		document.Settings.Priority == 7, nil
}

func weeklyTaskTriggerFromXML(raw []byte) (*scheduler.WeeklyTrigger, error) {
	body := stripTaskXMLDeclaration(raw)
	decoder := xml.NewDecoder(bytes.NewReader(body))
	decoder.Strict = true
	decoder.Entity = nil
	decoder.CharsetReader = nil
	var document weeklyTaskXMLDocument
	if err := decoder.Decode(&document); err != nil {
		return nil, err
	}
	start, err := time.Parse("2006-01-02T15:04:05", strings.TrimSpace(document.Triggers.CalendarTrigger.StartBoundary))
	if err != nil {
		return nil, fmt.Errorf("parse StartBoundary: %w", err)
	}
	days := []*struct{}{
		document.Triggers.CalendarTrigger.ScheduleByWeek.DaysOfWeek.Sunday,
		document.Triggers.CalendarTrigger.ScheduleByWeek.DaysOfWeek.Monday,
		document.Triggers.CalendarTrigger.ScheduleByWeek.DaysOfWeek.Tuesday,
		document.Triggers.CalendarTrigger.ScheduleByWeek.DaysOfWeek.Wednesday,
		document.Triggers.CalendarTrigger.ScheduleByWeek.DaysOfWeek.Thursday,
		document.Triggers.CalendarTrigger.ScheduleByWeek.DaysOfWeek.Friday,
		document.Triggers.CalendarTrigger.ScheduleByWeek.DaysOfWeek.Saturday,
	}
	day := -1
	for index, present := range days {
		if present == nil {
			continue
		}
		if day >= 0 {
			return nil, errors.New("weekly task has multiple day-of-week triggers")
		}
		day = index
	}
	if day < 0 || document.Triggers.CalendarTrigger.ScheduleByWeek.WeeksInterval != 1 {
		return nil, errors.New("weekly task has no single weekly trigger")
	}
	return &scheduler.WeeklyTrigger{DayOfWeek: day, HourLocal: start.Hour(), MinuteLocal: start.Minute()}, nil
}

func xmlBool(raw string, want bool) bool {
	return strings.EqualFold(strings.TrimSpace(raw), fmt.Sprintf("%t", want))
}

func weeklyDaysMatch(days struct {
	Sunday    *struct{} `xml:"Sunday"`
	Monday    *struct{} `xml:"Monday"`
	Tuesday   *struct{} `xml:"Tuesday"`
	Wednesday *struct{} `xml:"Wednesday"`
	Thursday  *struct{} `xml:"Thursday"`
	Friday    *struct{} `xml:"Friday"`
	Saturday  *struct{} `xml:"Saturday"`
}, day int) bool {
	present := []*struct{}{days.Sunday, days.Monday, days.Tuesday, days.Wednesday, days.Thursday, days.Friday, days.Saturday}
	if day < 0 || day >= len(present) || present[day] == nil {
		return false
	}
	for index, value := range present {
		if index != day && value != nil {
			return false
		}
	}
	return true
}

func sameTaskPath(left, right string) bool {
	left = filepath.Clean(strings.Trim(strings.TrimSpace(left), `"`))
	right = filepath.Clean(strings.Trim(strings.TrimSpace(right), `"`))
	if runtime.GOOS == "windows" {
		return strings.EqualFold(left, right)
	}
	return left == right
}

func stripTaskXMLDeclaration(raw []byte) []byte {
	raw = bytes.TrimPrefix(raw, []byte{0xef, 0xbb, 0xbf})
	trimmed := bytes.TrimSpace(raw)
	if !bytes.HasPrefix(trimmed, []byte("<?xml")) {
		return trimmed
	}
	if end := bytes.Index(trimmed, []byte("?>")); end >= 0 {
		return bytes.TrimSpace(trimmed[end+2:])
	}
	return trimmed
}

// WeeklyRefreshAll reads the registry and restarts every per-(workspace,
// language) scheduler task whose WeeklyRefresh flag is true. Best-effort:
// per-entry failures are recorded in Warnings without aborting the run.
//
// Refresh is kill-then-run: the live proxy (if any) is terminated by port
// so the replacement one launched by sch.Run binds cleanly. Task
// Scheduler's Run semantics on an already-running task are unreliable
// (MultipleInstancesPolicy=IgnoreNew makes it a no-op) and the Phase 2
// install/restart paths established killDaemonByPort as the correct
// pattern. kill-on-absent is expected and produces no warning.
func (a *API) WeeklyRefreshAll() (*WeeklyRefreshReport, error) {
	regPath, err := registryPathForRegister()
	if err != nil {
		return nil, err
	}
	reg := NewRegistry(regPath)
	if err := reg.Load(); err != nil {
		return nil, err
	}
	sch, err := schedulerNewForRegister()
	if err != nil {
		return nil, err
	}
	report := &WeeklyRefreshReport{}
	for _, e := range reg.Workspaces {
		if !e.WeeklyRefresh {
			continue
		}
		// Kill the stale proxy first. Failure to kill is non-fatal —
		// the subsequent Run may still succeed if the old process is
		// already gone or was never running. Errors go to Warnings so
		// operators see them in the report.
		if killByPortFn != nil && e.Port != 0 {
			if err := killByPortFn(e.Port, 5*time.Second); err != nil {
				report.Warnings = append(report.Warnings,
					fmt.Sprintf("kill proxy on port %d (task %s): %v",
						e.Port, e.TaskName, err))
			}
		}
		if err := sch.Run(e.TaskName); err != nil {
			report.Warnings = append(report.Warnings,
				fmt.Sprintf("run %s: %v", e.TaskName, err))
			continue
		}
		report.Restarted = append(report.Restarted, e.TaskName)
	}
	return report, nil
}
