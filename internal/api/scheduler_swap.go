// Package api — weekly Task Scheduler compatibility adapter.
//
// swapWeeklyTriggerWith supplies the expected prior generation and delegates
// Export/Delete/Create/Import/verify plus release settlement to
// runWeeklyRefreshTaskTransaction. It does not own settings.
package api

import (
	"fmt"

	"mcp-local-hub/internal/scheduler"
)

// schedulerSwap is the test seam: production path is the real scheduler
// returned by schedulerNewForRegister (which itself satisfies a wider
// interface that includes Delete/Create/ImportXML); tests inject a fake
// to drive deterministic Delete/Create/ImportXML outcomes without
// touching real Task Scheduler.
type schedulerSwap interface {
	ExportXML(name string) ([]byte, error)
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
