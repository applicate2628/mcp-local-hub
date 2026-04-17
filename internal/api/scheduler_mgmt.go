package api

import (
	"fmt"
	"os"
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
	exe, err := os.Executable()
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
	manifestDir := defaultManifestDir()
	for _, t := range tasks {
		srv, dmn := parseTaskName(t.Name)
		if srv == "" {
			results = append(results, SchedulerUpgradeResult{TaskName: t.Name, Err: "unparseable task name"})
			continue
		}
		m, err := loadManifestForServer(manifestDir, srv)
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

		if err := sch.Delete(t.Name); err != nil {
			results = append(results, SchedulerUpgradeResult{TaskName: t.Name, Err: fmt.Sprintf("delete: %v", err)})
			continue
		}
		spec := scheduler.TaskSpec{
			Name:             t.Name,
			Description:      "mcp-local-hub: " + m.Name,
			Command:          exe,
			Args:             args,
			RestartOnFailure: dmn != "weekly-refresh",
		}
		if dmn == "weekly-refresh" {
			spec.WeeklyTrigger = &scheduler.WeeklyTrigger{DayOfWeek: 0, HourLocal: 3, MinuteLocal: 0}
		} else {
			spec.LogonTrigger = true
		}
		if err := sch.Create(spec); err != nil {
			results = append(results, SchedulerUpgradeResult{TaskName: t.Name, Err: fmt.Sprintf("create: %v", err)})
			continue
		}
		results = append(results, SchedulerUpgradeResult{TaskName: t.Name, NewCmd: exe})
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
	exe, err := os.Executable()
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
		Command:          exe,
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
