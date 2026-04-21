package api

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"mcp-local-hub/internal/config"
	"mcp-local-hub/internal/scheduler"
)

// SchedulerUpgradeResult is one row in the per-task upgrade report.
type SchedulerUpgradeResult struct {
	TaskName string
	OldCmd   string
	NewCmd   string
	Err      string
}

// SchedulerUpgrade regenerates every mcp-local-hub scheduler task using the
// current executable path. Useful after:
//   - moving the binary to a new location
//   - renaming the binary (e.g. mcp.exe → mcphub.exe)
//   - bin/ reorganization
//
// Preserves scheduler task names and trigger configurations; only the
// <Command> and <WorkingDirectory> fields are updated.
func (a *API) SchedulerUpgrade() ([]SchedulerUpgradeResult, error) {
	canonicalPath, err := canonicalMcphubPath()
	if err != nil {
		return nil, err
	}
	sch, err := scheduler.New()
	if err != nil {
		return nil, err
	}
	tasks, err := sch.List("mcp-local-hub-")
	if err != nil {
		return nil, err
	}
	var results []SchedulerUpgradeResult
	for _, t := range tasks {
		normalized := strings.TrimPrefix(t.Name, "\\")
		// Workspace-scoped shared weekly-refresh task:
		// "mcp-local-hub-workspace-weekly-refresh". parseTaskName would
		// see server="workspace" and try to load a nonexistent manifest.
		// Skip — the task's Command already points at canonical mcphub
		// running the hidden `workspace-weekly-refresh` subcommand,
		// same as the global weekly-refresh case below.
		if normalized == WeeklyRefreshTaskName {
			continue
		}
		// Workspace-scoped lazy-proxy tasks: "mcp-local-hub-lsp-<key>-<lang>".
		// parseTaskName reports server="lsp" which also lacks a manifest.
		// Skip — these tasks are rebuilt via `mcphub register` when needed,
		// not via scheduler upgrade. The upgrade flow is for per-server
		// daemon tasks whose Command needs rewiring after the binary moves;
		// workspace-proxy tasks' Command is the same mcphub binary and
		// already correct.
		if IsLazyProxyTaskName(normalized) {
			continue
		}
		srv, dmn := parseTaskName(t.Name)
		// Hub-wide weekly-refresh ("mcp-local-hub-weekly-refresh") parses
		// as ("", "weekly-refresh") — no per-server manifest to re-read,
		// no Command rewrite needed (it already points at canonical mcphub
		// and runs `restart --all`). Leave it untouched; the scheduler
		// upgrade flow is specifically about per-server daemon tasks
		// getting their Command rewired after the binary moves.
		if srv == "" && dmn == "weekly-refresh" {
			continue
		}
		if srv == "" {
			results = append(results, SchedulerUpgradeResult{TaskName: t.Name, Err: "unparseable task name"})
			continue
		}
		// Empty dir → loadManifestForServer uses embed-first resolution.
		m, err := loadManifestForServer("", srv)
		if err != nil {
			results = append(results, SchedulerUpgradeResult{TaskName: t.Name, Err: fmt.Sprintf("manifest %s: %v", srv, err)})
			continue
		}
		// Re-build the task spec with current exe path.
		var args []string
		if dmn == "weekly-refresh" {
			args = []string{"restart", "--server", m.Name}
		} else {
			args = []string{"daemon", "--server", m.Name, "--daemon", dmn}
		}
		_ = m // referenced for future expansion (env, triggers)

		// Snapshot the existing XML so a failed create can restore the
		// prior task instead of leaving the user with nothing.
		var priorXML []byte
		if xml, err := sch.ExportXML(t.Name); err == nil {
			priorXML = xml
		}
		if err := sch.Delete(t.Name); err != nil {
			results = append(results, SchedulerUpgradeResult{TaskName: t.Name, Err: fmt.Sprintf("delete: %v", err)})
			continue
		}
		// Anchor WorkingDir at the canonical install dir. The install
		// flow (executeInstallTo) does the same — scheduler upgrade
		// rewrites Command + Args + WorkingDir together so tasks built
		// by a throwaway 'mcphub install' in %TEMP' don't keep pointing
		// at a deleted cwd after the upgrade.
		spec := scheduler.TaskSpec{
			Name:             t.Name,
			Description:      "mcp-local-hub: " + m.Name,
			Command:          canonicalPath,
			Args:             args,
			WorkingDir:       filepath.Dir(canonicalPath),
			RestartOnFailure: dmn != "weekly-refresh",
		}
		if dmn == "weekly-refresh" {
			spec.WeeklyTrigger = &scheduler.WeeklyTrigger{DayOfWeek: 0, HourLocal: 3, MinuteLocal: 0}
		} else {
			spec.LogonTrigger = true
		}
		if err := sch.Create(spec); err != nil {
			// Restore prior task on failure; don't leave user with nothing.
			if len(priorXML) > 0 {
				_ = sch.ImportXML(t.Name, priorXML)
			}
			results = append(results, SchedulerUpgradeResult{TaskName: t.Name, Err: fmt.Sprintf("create: %v", err)})
			continue
		}
		results = append(results, SchedulerUpgradeResult{TaskName: t.Name, NewCmd: canonicalPath})
	}
	return results, nil
}

// WeeklyRefreshSet creates or replaces the hub-wide weekly-refresh
// scheduler task. schedule format is "<DAY> <HH:MM>" where DAY is a
// 3-letter abbreviation (SUN|MON|...|SAT, case-insensitive).
func (a *API) WeeklyRefreshSet(schedule string) error {
	day, hr, min, err := parseWeeklyRefreshSchedule(schedule)
	if err != nil {
		return err
	}
	canonicalPath, err := canonicalMcphubPath()
	if err != nil {
		return err
	}
	sch, err := scheduler.New()
	if err != nil {
		return err
	}
	const taskName = "mcp-local-hub-weekly-refresh"
	_ = sch.Delete(taskName) // idempotent
	return sch.Create(scheduler.TaskSpec{
		Name:             taskName,
		Description:      "mcp-local-hub: weekly refresh (restart --all)",
		Command:          canonicalPath,
		Args:             []string{"restart", "--all"},
		WeeklyTrigger:    &scheduler.WeeklyTrigger{DayOfWeek: day, HourLocal: hr, MinuteLocal: min},
		RestartOnFailure: false,
	})
}

// WeeklyRefreshDisable removes the hub-wide weekly-refresh task.
// Per-manifest weekly_refresh: true entries are not affected.
func (a *API) WeeklyRefreshDisable() error {
	sch, err := scheduler.New()
	if err != nil {
		return err
	}
	return sch.Delete("mcp-local-hub-weekly-refresh")
}

// parseWeeklyRefreshSchedule parses "<DAY> <HH:MM>" into numeric parts.
// DAY: SUN=0, MON=1, TUE=2, WED=3, THU=4, FRI=5, SAT=6 (matches Go's Weekday).
func parseWeeklyRefreshSchedule(s string) (day, hour, min int, err error) {
	parts := strings.SplitN(strings.TrimSpace(s), " ", 2)
	if len(parts) != 2 {
		return 0, 0, 0, fmt.Errorf("expected '<DAY> <HH:MM>', got %q", s)
	}
	dayMap := map[string]int{"SUN": 0, "MON": 1, "TUE": 2, "WED": 3, "THU": 4, "FRI": 5, "SAT": 6}
	day, ok := dayMap[strings.ToUpper(parts[0])]
	if !ok {
		return 0, 0, 0, fmt.Errorf("unknown day %q (use SUN..SAT)", parts[0])
	}
	hm := strings.SplitN(parts[1], ":", 2)
	if len(hm) != 2 {
		return 0, 0, 0, fmt.Errorf("expected HH:MM, got %q", parts[1])
	}
	hour, err = strconv.Atoi(hm[0])
	if err != nil || hour < 0 || hour > 23 {
		return 0, 0, 0, fmt.Errorf("invalid hour %q", hm[0])
	}
	min, err = strconv.Atoi(hm[1])
	if err != nil || min < 0 || min > 59 {
		return 0, 0, 0, fmt.Errorf("invalid minute %q", hm[1])
	}
	return day, hour, min, nil
}

// _ keeps config import alive for future use in this file.
var _ = config.KindGlobal
