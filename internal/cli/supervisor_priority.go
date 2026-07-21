package cli

// Supervisor priority-floor policy — OS-agnostic decision core.
//
// ROOT CAUSE (see work-items/bugs/2026-07-21-status-ipc-reparses-every-manifest-per-poll.md,
// REVISION 4). The repo contains NO priority code; the supervisor's
// priority class is entirely INHERITED from the scheduled task that
// launched it. The Windows Task Scheduler <Priority> element (0-10) maps
// the liveness RECOVERY task to IDLE_PRIORITY_CLASS (<Priority>9</Priority>)
// — one class WORSE than the normal autostart task
// (<Priority>7</Priority> = BELOW_NORMAL). A liveness-relaunched
// supervisor (spawnStandaloneSupervisor) is a detached CHILD of that IDLE
// action process, so it inherits IDLE, and every daemon it spawns inherits
// IDLE in turn (the daemon spawn path passes NO priority-class creation
// flag; per the documented Win32 rule a child inherits an IDLE/BELOW_NORMAL
// parent's class). A fleet at IDLE on a busy host is scheduled so rarely
// that the status IPC and even the 60s heartbeat ticker stall for tens of
// seconds — a measured ~86x mean amplification of the identity workload vs
// NORMAL priority.
//
// This file is the pure, OS-agnostic core of the fix. The Windows syscall
// seam (class <-> rank mapping + Get/SetPriorityClass) lives in
// supervisor_priority_windows.go; the non-Windows no-op in
// supervisor_priority_other.go.

// priorityRank is an ordinal scheduling rank for a Windows process
// priority class, ordered by ACTUAL scheduling priority (higher rank =
// more CPU-favoured).
//
// It exists because the raw Win32 priority-class CONSTANTS are NOT ordered
// by scheduling priority: IDLE_PRIORITY_CLASS is 0x40 and
// NORMAL_PRIORITY_CLASS is 0x20, so IDLE is numerically GREATER than NORMAL
// yet schedules LOWER. Any "raise-only, never-lower" decision MUST compare
// ranks, never the raw constants — comparing the constants would invert the
// decision (e.g. treat NORMAL as below IDLE). Keeping the decision on this
// OS-agnostic type also makes it unit-testable on every platform.
type priorityRank int

const (
	// rankUnknown is the rank of an unrecognized priority-class value.
	// The decision treats it as "do not touch": we cannot prove a raise-to-
	// floor would not LOWER an unknown-but-higher class, so we leave it.
	rankUnknown     priorityRank = iota - 1 // -1
	rankIdle                                // 0  IDLE_PRIORITY_CLASS         (base 4)
	rankBelowNormal                         // 1  BELOW_NORMAL_PRIORITY_CLASS (base 6)
	rankNormal                              // 2  NORMAL_PRIORITY_CLASS       (base 8)
	rankAboveNormal                         // 3  ABOVE_NORMAL_PRIORITY_CLASS (base 10)
	rankHigh                                // 4  HIGH_PRIORITY_CLASS         (base 13)
	rankRealtime                            // 5  REALTIME_PRIORITY_CLASS     (base 24)
)

// supervisorPriorityFloorRank is the MINIMUM acceptable scheduling class
// for the supervisor and — by Windows CreateProcess inheritance — the
// daemon fleet it spawns.
//
// Chosen = BELOW_NORMAL, DELIBERATELY not NORMAL. The normal autostart
// task authors <Priority>7</Priority>, which Task Scheduler maps to
// BELOW_NORMAL_PRIORITY_CLASS; that encodes a considered "polite
// background service" intent. This floor HONORS that intent — it lifts a
// process launched at IDLE (the recovery/liveness path) up to the SAME
// class the normal path already uses, and no higher. A background MCP
// supervisor being "not starved" is the goal, NOT being "prioritised over
// the operator's foreground work", so we do not jump to NORMAL/ABOVE_NORMAL.
const supervisorPriorityFloorRank = rankBelowNormal

// decideSupervisorPriorityRaise is the pure, OS-agnostic core of the
// priority-floor policy. Given the process's CURRENT rank it returns the
// rank to raise TO and whether a raise is warranted.
//
// Invariants (mutation-tested in supervisor_priority_test.go):
//   - RAISE-ONLY / NEVER-LOWER: it never returns raise=true with a target
//     BELOW the current rank. A process already at or above the floor is
//     left alone (raise=false).
//   - NEVER-IDLE: when raise=true the target is the floor (BELOW_NORMAL),
//     never rankIdle. The supervisor must never lower ITSELF to IDLE.
//   - DON'T-TOUCH-UNKNOWN: an unrecognized current rank yields raise=false,
//     because lowering an unknown-but-possibly-higher class must never
//     happen.
func decideSupervisorPriorityRaise(current priorityRank) (target priorityRank, raise bool) {
	if current == rankUnknown {
		// Cannot rank the current class → cannot prove a change would not
		// lower it → leave it untouched.
		return rankUnknown, false
	}
	if current >= supervisorPriorityFloorRank {
		// Already polite-or-higher; never lower.
		return current, false
	}
	// Below the floor (i.e. IDLE) → raise to the floor.
	return supervisorPriorityFloorRank, true
}
