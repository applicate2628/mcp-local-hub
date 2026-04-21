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
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"mcp-local-hub/internal/scheduler"
)

// WeeklyRefreshTaskName is the single shared scheduler task name that invokes
// `mcphub workspace-weekly-refresh` on a weekly trigger. One task, regardless
// of how many workspaces are registered — per Phase 3 decision recorded in
// the plan's M4 section.
const WeeklyRefreshTaskName = "mcp-local-hub-workspace-weekly-refresh"

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
	// Snapshot any prior task before the destructive Delete so a Create
	// failure can restore the previously working schedule. Register
	// treats EnsureWeeklyRefreshTask errors as non-fatal warnings — a
	// transient scheduler glitch must not silently disable weekly
	// refresh for every registered workspace until the next successful
	// register hits this path again.
	var priorXML []byte
	if xml, err := sch.ExportXML(WeeklyRefreshTaskName); err == nil {
		priorXML = xml
	} else if !errors.Is(err, scheduler.ErrTaskNotFound) {
		// ExportXML failed for a reason other than "not found" — abort
		// before the destructive Delete. Transient export errors should
		// not nuke the existing schedule.
		return fmt.Errorf("export prior %s: %w", WeeklyRefreshTaskName, err)
	}
	// Idempotent replace: Delete returns nil if the task is absent.
	_ = sch.Delete(WeeklyRefreshTaskName)
	spec := scheduler.TaskSpec{
		Name:        WeeklyRefreshTaskName,
		Description: "mcp-local-hub: weekly refresh of workspace-scoped lazy proxies",
		Command:     canonical,
		Args:        []string{"workspace-weekly-refresh"},
		WorkingDir:  filepath.Dir(canonical),
		WeeklyTrigger: &scheduler.WeeklyTrigger{
			DayOfWeek: 0, HourLocal: 3, MinuteLocal: 0,
		},
	}
	if err := sch.Create(spec); err != nil {
		// Create failed — restore the prior task if we had one so the
		// previously working schedule survives the transient glitch.
		if len(priorXML) > 0 {
			if rerr := sch.ImportXML(WeeklyRefreshTaskName, priorXML); rerr != nil {
				return fmt.Errorf("create %s failed: %w; additionally restore-prior failed: %v",
					WeeklyRefreshTaskName, err, rerr)
			}
		}
		return fmt.Errorf("create %s: %w", WeeklyRefreshTaskName, err)
	}
	return nil
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
