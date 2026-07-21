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
// Chosen = NORMAL. This is a DELIBERATE REVERSAL of PR #577's original
// BELOW_NORMAL floor, made on new live evidence, NOT a design drift.
//
// PR #577 picked BELOW_NORMAL to "honor the polite background service"
// intent of the autostart task's <Priority>7</Priority>. A live A/B on the
// operator's deployed v0.4.26 fleet then showed that BELOW_NORMAL is not
// enough under real host load:
//   - BELOW_NORMAL, heavy host load: /api/status succeeded only 4/6, and
//     earlier as low as 2/6 (13s replies against a 30s timeout).
//   - BELOW_NORMAL, moderate load: 9/10.
//   - The SAME fleet raised to NORMAL: 10/10, immediately.
//   - A goroutine dump taken DURING a stall showed the supervisor
//     INTERNALLY IDLE (zero runnable goroutines, no internal stall). The
//     red tail is therefore pure OS scheduling latency under host load, not
//     mcphub burning CPU.
//
// Because the supervisor is idle-until-used, NORMAL costs ~nothing in CPU
// (an idle process at NORMAL is not scheduled any more often than one at
// BELOW_NORMAL until it actually has work) — it only buys scheduling
// latency for the status responder when the host is busy. The operator
// explicitly accepted the tradeoff (mcphub now competes with foreground
// work at NORMAL) precisely because that CPU cost is near-zero while the
// responsiveness win is decisive.
//
// The floor stays RAISE-ONLY / NEVER-LOWER / NEVER-IDLE: a process already
// at NORMAL or above (ABOVE_NORMAL / HIGH / REALTIME, or a future launcher
// at NORMAL) is left untouched — the floor only lifts a process launched
// BELOW it (IDLE or BELOW_NORMAL, i.e. the liveness/autostart launch class)
// up to NORMAL, and never higher. We do not jump to ABOVE_NORMAL/HIGH: the
// goal is "not starved and promptly scheduled", not "prioritised over the
// operator's foreground work beyond parity".
const supervisorPriorityFloorRank = rankNormal

// decideSupervisorPriorityRaise is the pure, OS-agnostic core of the
// priority-floor policy. Given the process's CURRENT rank it returns the
// rank to raise TO and whether a raise is warranted.
//
// Invariants (mutation-tested in supervisor_priority_test.go):
//   - RAISE-ONLY / NEVER-LOWER: it never returns raise=true with a target
//     BELOW the current rank. A process already at or above the floor
//     (NORMAL / ABOVE_NORMAL / HIGH / REALTIME) is left alone (raise=false).
//   - NEVER-IDLE: when raise=true the target is the floor (NORMAL), never
//     rankIdle. The supervisor must never lower ITSELF to IDLE.
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
		// Already at-floor-or-higher (NORMAL / ABOVE_NORMAL / HIGH /
		// REALTIME); never lower.
		return current, false
	}
	// Below the floor (IDLE or BELOW_NORMAL) → raise to the floor.
	return supervisorPriorityFloorRank, true
}
