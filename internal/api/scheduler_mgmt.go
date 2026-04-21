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
	// Load the registry once for workspace-scoped task upgrades.
	wsByTask := workspaceTasksByName()
	var results []SchedulerUpgradeResult
	for _, t := range tasks {
		normalized := strings.TrimPrefix(t.Name, "\\")
		// Workspace-scoped shared weekly-refresh task:
		// "mcp-local-hub-workspace-weekly-refresh". parseTaskName would
		// see server="workspace" and try to load a nonexistent manifest,
		// but the task's Command DOES need rewriting to the new canonical
		// mcphub path when the binary moves — otherwise weekly refreshes
		// silently stop working after the upgrade.
		if normalized == WeeklyRefreshTaskName {
			if r := upgradeWorkspaceWeeklyRefreshTask(sch, t.Name, canonicalPath); r != nil {
				results = append(results, *r)
			}
			continue
		}
		// Workspace-scoped lazy-proxy tasks: "mcp-local-hub-lsp-<key>-<lang>".
		// parseTaskName reports server="lsp" which also lacks a manifest.
		// Cannot reuse the per-server manifest path, but the Command still
		// needs rewriting so the new logon spawns the relocated mcphub.
		// Args are reconstructed from the registry entry (port, workspace,
		// language) which is already the source of truth for these tasks.
		if IsLazyProxyTaskName(normalized) {
			if r := upgradeLazyProxyTask(sch, t.Name, normalized, canonicalPath, wsByTask); r != nil {
				results = append(results, *r)
			}
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

// upgradeWorkspaceWeeklyRefreshTask rewrites the shared workspace
// weekly-refresh task's Command to canonicalPath. Called from
// SchedulerUpgrade for the `mcp-local-hub-workspace-weekly-refresh`
// task — that task has no corresponding manifest so the main upgrade
// loop can't use loadManifestForServer, but the Command path still
// needs refreshing after a binary move. Snapshot + restore on failure
// mirrors the rest of the upgrade loop.
func upgradeWorkspaceWeeklyRefreshTask(sch scheduler.Scheduler, taskName, canonicalPath string) *SchedulerUpgradeResult {
	priorXML, _ := sch.ExportXML(taskName)
	if err := sch.Delete(taskName); err != nil {
		return &SchedulerUpgradeResult{TaskName: taskName, Err: fmt.Sprintf("delete: %v", err)}
	}
	spec := scheduler.TaskSpec{
		Name:             taskName,
		Description:      "mcp-local-hub: weekly refresh of workspace-scoped lazy proxies",
		Command:          canonicalPath,
		Args:             []string{"workspace-weekly-refresh"},
		WorkingDir:       filepath.Dir(canonicalPath),
		WeeklyTrigger:    &scheduler.WeeklyTrigger{DayOfWeek: 0, HourLocal: 3, MinuteLocal: 0},
		RestartOnFailure: false,
	}
	if err := sch.Create(spec); err != nil {
		if len(priorXML) > 0 {
			_ = sch.ImportXML(taskName, priorXML)
		}
		return &SchedulerUpgradeResult{TaskName: taskName, Err: fmt.Sprintf("create: %v", err)}
	}
	return &SchedulerUpgradeResult{TaskName: taskName, NewCmd: canonicalPath}
}

// upgradeLazyProxyTask rewrites a `mcp-local-hub-lsp-<key>-<lang>`
// scheduler task's Command + Args to the new canonicalPath. Args
// (port, workspace path, language) come from the registry entry
// which is the source of truth for these tasks. Missing registry
// entry surfaces as an error — upgrading a task with no registry
// row would produce a broken config, better to let the operator
// re-register.
func upgradeLazyProxyTask(sch scheduler.Scheduler, taskName, normalizedName, canonicalPath string, wsByTask map[string]WorkspaceEntry) *SchedulerUpgradeResult {
	entry, ok := wsByTask[normalizedName]
	if !ok {
		return &SchedulerUpgradeResult{TaskName: taskName, Err: "no registry entry for workspace-proxy task; run mcphub register to rebuild"}
	}
	priorXML, _ := sch.ExportXML(taskName)
	if err := sch.Delete(taskName); err != nil {
		return &SchedulerUpgradeResult{TaskName: taskName, Err: fmt.Sprintf("delete: %v", err)}
	}
	spec := scheduler.TaskSpec{
		Name:        taskName,
		Description: fmt.Sprintf("mcp-local-hub: workspace %s lang %s", entry.WorkspacePath, entry.Language),
		Command:     canonicalPath,
		Args: []string{
			"daemon", "workspace-proxy",
			"--port", fmt.Sprintf("%d", entry.Port),
			"--workspace", entry.WorkspacePath,
			"--language", entry.Language,
		},
		WorkingDir:       filepath.Dir(canonicalPath),
		RestartOnFailure: true,
		LogonTrigger:     true,
	}
	if err := sch.Create(spec); err != nil {
		if len(priorXML) > 0 {
			_ = sch.ImportXML(taskName, priorXML)
		}
		return &SchedulerUpgradeResult{TaskName: taskName, Err: fmt.Sprintf("create: %v", err)}
	}
	return &SchedulerUpgradeResult{TaskName: taskName, NewCmd: canonicalPath}
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
