//go:build windows

package cli

import (
	"golang.org/x/sys/windows"

	"mcp-local-hub/internal/api"
)

// getCurrentPriorityClassFn / setCurrentPriorityClassFn are the injectable
// syscall SEAMS. Production reads/writes THIS process's priority class via
// the GetCurrentProcess pseudo-handle; the unit test swaps in fakes so the
// orchestration is exercised WITHOUT mutating any real process priority
// (including the `go test` runner's). Production callers MUST NOT reassign
// these directly — the *ForTest setters below are the only allowed write
// path.
var (
	getCurrentPriorityClassFn = getCurrentProcessPriorityClass
	setCurrentPriorityClassFn = setCurrentProcessPriorityClass
)

// getCurrentProcessPriorityClass reads this process's current priority
// class. windows.CurrentProcess() is the (-1) pseudo-handle: it never
// fails and needs no Close.
func getCurrentProcessPriorityClass() (uint32, error) {
	return windows.GetPriorityClass(windows.CurrentProcess())
}

// setCurrentProcessPriorityClass sets this process's priority class.
func setCurrentProcessPriorityClass(class uint32) error {
	return windows.SetPriorityClass(windows.CurrentProcess(), class)
}

// priorityClassToRank maps a Win32 priority-class constant to its ordinal
// scheduling rank. An unrecognized value maps to rankUnknown so the
// decision leaves it untouched.
func priorityClassToRank(class uint32) priorityRank {
	switch class {
	case windows.IDLE_PRIORITY_CLASS:
		return rankIdle
	case windows.BELOW_NORMAL_PRIORITY_CLASS:
		return rankBelowNormal
	case windows.NORMAL_PRIORITY_CLASS:
		return rankNormal
	case windows.ABOVE_NORMAL_PRIORITY_CLASS:
		return rankAboveNormal
	case windows.HIGH_PRIORITY_CLASS:
		return rankHigh
	case windows.REALTIME_PRIORITY_CLASS:
		return rankRealtime
	default:
		return rankUnknown
	}
}

// rankToPriorityClass maps an ordinal rank back to its Win32 class
// constant. Only the raise targets the decision can return (BELOW_NORMAL
// and above) need a real mapping; rankUnknown/rankIdle map to 0, which the
// caller treats as "no class to set" — a defensive backstop, since
// decideSupervisorPriorityRaise never returns either with raise=true.
func rankToPriorityClass(rank priorityRank) uint32 {
	switch rank {
	case rankBelowNormal:
		return windows.BELOW_NORMAL_PRIORITY_CLASS
	case rankNormal:
		return windows.NORMAL_PRIORITY_CLASS
	case rankAboveNormal:
		return windows.ABOVE_NORMAL_PRIORITY_CLASS
	case rankHigh:
		return windows.HIGH_PRIORITY_CLASS
	case rankRealtime:
		return windows.REALTIME_PRIORITY_CLASS
	default:
		return 0
	}
}

// ensureSupervisorPriorityFloor raises THIS supervisor process to at least
// NORMAL_PRIORITY_CLASS when it was launched at a lower class (IDLE or
// BELOW_NORMAL), and is a no-op otherwise. It is the durable, in-binary fix
// for the fleet being scheduled too rarely under host load. Both scheduled
// launch classes are below the floor and are lifted: the liveness recovery
// task (<Priority>9</Priority> = IDLE) and the autostart / liveness tasks'
// <Priority>7</Priority> = BELOW_NORMAL.
//
// FLOOR = NORMAL (raised from BELOW_NORMAL; see supervisor_priority.go for
// the const and the live A/B evidence). A live A/B on the operator's fleet
// showed BELOW_NORMAL still left a red tail on /api/status under host load
// (as low as 2/6), while NORMAL was 10/10; a goroutine dump during a stall
// showed the supervisor internally IDLE, so the red is pure OS scheduling
// latency, not mcphub CPU. Because the supervisor is idle-until-used, NORMAL
// costs ~nothing in CPU — it only buys scheduling latency when the host is
// busy.
//
// Correctness is INDEPENDENT of the launcher: the autostart task, the
// liveness task, or a manual `mcphub supervise` run all converge on the
// floor. Existing hosts (whose installed tasks launch the supervisor at
// IDLE or BELOW_NORMAL) are corrected WITHOUT a reinstall, because the
// supervisor sets its OWN class at startup rather than trusting what
// launched it.
//
// FLEET PROPAGATION (no per-spawn code needed). The caller invokes this
// BEFORE the reconcile loop spawns any daemon, and the daemon spawn path
// (process.StartWithJob) passes NO priority-class creation flag. Per the
// documented Win32 rule — "If the calling process is IDLE_PRIORITY_CLASS or
// BELOW_NORMAL_PRIORITY_CLASS, the new process will inherit this class"
// (learn.microsoft.com/windows/win32/procthread/scheduling-priorities) —
// a daemon spawned by the raised (NORMAL) supervisor is NOT covered by that
// inheritance clause, so it takes the CreateProcess DEFAULT of NORMAL —
// which equals the floor. The mechanism differs from the old BELOW_NORMAL
// floor (which relied on the clause FIRING to propagate BELOW_NORMAL) but
// the outcome is the same-or-better: the fleet is always at least NORMAL. A
// future launcher at ABOVE_NORMAL+ likewise yields NORMAL-default children,
// still >= the floor.
//
// RAISE-ONLY / NEVER-LOWER / NEVER-IDLE: if the process is already at or
// above the floor (a launcher at NORMAL+, e.g. a future task authored at
// NORMAL, or a process already raised on a prior tick) it is left
// untouched — an ABOVE_NORMAL/HIGH/REALTIME process is NEVER lowered to
// NORMAL. Every outcome is audited to supervisor-events.log; a raise
// failure (e.g. SetPriorityClass denied by policy) is NON-FATAL — a
// supervisor that could not raise its own priority still supervises, just
// under the launcher's class (BELOW_NORMAL for the Priority-7 tasks, which
// the live A/B showed is degraded-but-functional rather than dead).
func ensureSupervisorPriorityFloor(events *api.SupervisorEventLog) {
	current, err := getCurrentPriorityClassFn()
	if err != nil {
		emitSupervisorPriorityEvent(events, api.SupervisorEventSeverityWarn,
			"supervisor-priority-probe-failed",
			map[string]any{"error": err.Error()})
		return
	}

	currentRank := priorityClassToRank(current)
	targetRank, raise := decideSupervisorPriorityRaise(currentRank)
	if !raise {
		emitSupervisorPriorityEvent(events, api.SupervisorEventSeverityInfo,
			"supervisor-priority-ok",
			map[string]any{
				"current_class": current,
				"current_rank":  int(currentRank),
			})
		return
	}

	targetClass := rankToPriorityClass(targetRank)
	if err := setCurrentPriorityClassFn(targetClass); err != nil {
		emitSupervisorPriorityEvent(events, api.SupervisorEventSeverityWarn,
			"supervisor-priority-raise-failed",
			map[string]any{
				"from_class": current,
				"to_class":   targetClass,
				"error":      err.Error(),
			})
		return
	}

	emitSupervisorPriorityEvent(events, api.SupervisorEventSeverityInfo,
		"supervisor-priority-raised",
		map[string]any{
			"from_class": current,
			"to_class":   targetClass,
		})
}

// emitSupervisorPriorityEvent is a best-effort audit helper. A nil log
// (early-startup / test paths) is tolerated; emit failure is ignored
// (observability, not a gate).
func emitSupervisorPriorityEvent(events *api.SupervisorEventLog, severity, event string, body map[string]any) {
	if events == nil {
		return
	}
	_ = events.Emit(api.SupervisorEvent{
		Severity: severity,
		Source:   api.SupervisorEventSourceLifecycle,
		Event:    event,
		Body:     body,
	})
}

// setPriorityClassSeamsForTest swaps the syscall seams for the orchestration
// test and returns a restore func. Only supervisor_priority_windows_test.go
// invokes it.
func setPriorityClassSeamsForTest(get func() (uint32, error), set func(uint32) error) func() {
	prevGet, prevSet := getCurrentPriorityClassFn, setCurrentPriorityClassFn
	getCurrentPriorityClassFn = get
	setCurrentPriorityClassFn = set
	return func() {
		getCurrentPriorityClassFn = prevGet
		setCurrentPriorityClassFn = prevSet
	}
}
