// Package api — Task Scheduler trigger swap helper. Memo D8.
//
// SwapWeeklyTrigger owns ONLY the scheduler XML lifecycle:
//
//	Delete → Create → optional ImportXML(priorXML) on Create failure.
//
// It does NOT call ExportXML (caller's preflight) and does NOT touch
// settings YAML (caller's responsibility). The four disjoint return
// tuples are documented at the swap function's docstring below.
package api

import (
	"fmt"
	"path/filepath"

	"mcp-local-hub/internal/scheduler"
)

// schedulerSwap is the test seam: production path is the real scheduler
// returned by schedulerNewForRegister (which itself satisfies a wider
// interface that includes Delete/Create/ImportXML); tests inject a fake
// to drive deterministic Delete/Create/ImportXML outcomes without
// touching real Task Scheduler.
type schedulerSwap interface {
	Delete(name string) error
	Create(spec scheduler.TaskSpec) error
	ImportXML(name string, xml []byte) error
}

// SwapWeeklyTrigger is the production entrypoint. It loads the real
// scheduler and delegates to swapWeeklyTriggerWith.
func SwapWeeklyTrigger(spec *ScheduleSpec, priorXML []byte) (restoreStatus string, err error) {
	sch, sErr := schedulerNewForRegister()
	if sErr != nil {
		return "n/a", fmt.Errorf("scheduler init: %w", sErr)
	}
	return swapWeeklyTriggerWith(sch, spec, priorXML)
}

// swapWeeklyTriggerWith is the test-seam variant. Returns disjoint
// (restoreStatus, err) tuples per memo D8:
//
//	("n/a", nil)        Create succeeded (both fresh-install and
//	                    had-prior-task paths). No rollback was needed.
//	("ok", err)         Create FAILED, priorXML != nil, ImportXML
//	                    succeeded — prior task restored.
//	("degraded", err)   Create FAILED, priorXML != nil, ImportXML also
//	                    FAILED — prior task lost.
//	("n/a", err)        Create FAILED, priorXML == nil (fresh-install).
//	                    No rollback was attempted (nothing to restore).
//
// All four cases are exhaustive over the helper's scheduler-XML domain.
// The caller's truth table at D8 step 8 maps these to response
// `restore_status` after combining with settings-YAML rollback.
func swapWeeklyTriggerWith(sch schedulerSwap, spec *ScheduleSpec, priorXML []byte) (restoreStatus string, err error) {
	canonical, perr := canonicalMcphubPath()
	if perr != nil {
		return "n/a", fmt.Errorf("canonical path: %w", perr)
	}

	_ = sch.Delete(WeeklyRefreshTaskName) // idempotent; nil if absent

	createErr := sch.Create(scheduler.TaskSpec{
		Name:        WeeklyRefreshTaskName,
		Description: "mcp-local-hub: weekly refresh of workspace-scoped lazy proxies",
		Command:     canonical,
		Args:        []string{"workspace-weekly-refresh"},
		WorkingDir:  filepath.Dir(canonical),
		WeeklyTrigger: &scheduler.WeeklyTrigger{
			DayOfWeek:   spec.DayOfWeek,
			HourLocal:   spec.Hour,
			MinuteLocal: spec.Minute,
		},
	})
	if createErr == nil {
		return "n/a", nil
	}

	if priorXML == nil {
		return "n/a", createErr // fresh-install case: nothing to restore
	}
	if importErr := sch.ImportXML(WeeklyRefreshTaskName, priorXML); importErr != nil {
		return "degraded", createErr // prior task lost; surface original error
	}
	return "ok", createErr
}
