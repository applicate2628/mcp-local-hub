// Package api — weekly Task Scheduler compatibility adapter.
//
// swapWeeklyTriggerWith supplies the expected prior definition and delegates
// snapshot/Delete/Create/create-only recovery plus release settlement to
// runWeeklyRefreshTaskTransaction. It does not own settings.
package api

import (
	"fmt"

	"mcp-local-hub/internal/scheduler"
)

// schedulerSwap is the test seam: production path is the real scheduler
// returned by schedulerNewForRegister (which itself satisfies a wider
// interface that includes Delete/Create); tests inject a fake
// to drive deterministic mutation and create-only recovery outcomes without
// touching real Task Scheduler.
type schedulerSwap interface {
	ExportXML(name string) ([]byte, error)
	Delete(name string) error
	Create(spec scheduler.TaskSpec) error
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
//	("ok", err)         The failed mutation's prior definition is observed,
//	                    including after create-only restoration and readback.
//	("degraded", err)   Prior definition/absence could not be confirmed;
//	                    current task and recovery material are retained.
//	("n/a", err)        Mutation was refused, or fresh creation failed and
//	                    absence is confirmed. No rollback was needed.
//
// Definition readback does not establish native task-generation ownership.
// The caller's truth table at D8 step 8 maps these to response
// `restore_status` after combining with settings-YAML rollback.
func swapWeeklyTriggerWith(sch schedulerSwap, spec *ScheduleSpec, priorXML []byte) (restoreStatus string, err error) {
	if spec == nil || spec.Kind != ScheduleWeekly {
		return "n/a", fmt.Errorf("weekly schedule is required")
	}
	canonical, perr := canonicalMcphubPath()
	if perr != nil {
		return "n/a", fmt.Errorf("canonical path: %w", perr)
	}
	result, transactionErr := runWeeklyRefreshTaskTransaction(sch, weeklyRefreshMutation{
		taskName:    WeeklyRefreshTaskName,
		expectedXML: priorXML,
		expectedSet: true,
		desired: func([]byte, bool) (scheduler.TaskSpec, error) {
			return weeklyRefreshTaskSpec(canonical, spec), nil
		},
	})
	return result.restoreStatus, transactionErr
}
