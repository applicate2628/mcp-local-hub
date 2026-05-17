//go:build !windows

package cli

import (
	"bytes"
	"os"
	"runtime"
	"syscall"
)

// pidAliveImpl probes liveness via os.FindProcess + Process.Signal(0),
// then on Linux refines via /proc/<pid>/stat to filter zombies.
//
// Per Go stdlib docs at os/exec.go:249-252, on Unix os.FindProcess
// ALWAYS succeeds — it just wraps the PID in a *Process without
// consulting the kernel. The actual liveness probe must come from a
// signal-0 delivery via kill(2). kill(2) returns:
//
//   - 0 (nil err)        — process exists AND we may signal it
//   - errno=ESRCH        — no such PID (dead OR never existed)
//   - errno=EPERM        — process exists but is owned by another
//                          user and we cannot signal it
//
// For the supervisor's drain we treat both ESRCH and EPERM as "not
// ours to keep waiting on" — the supervisor only manages PIDs it
// spawned, so an EPERM means the PID got recycled to a different
// user's process and the original transient is gone.
//
// **Zombie filter (codex r5 P2):** a transient child that exits before
// its parent reaps it stays in state 'Z' (zombie) — Signal(0) reports
// it as alive even though it has already terminated. That makes the
// quiesce drain loop think the transient is still running and either
// time out unnecessarily or escalate to force-kill. On Linux we read
// /proc/<pid>/stat and treat state 'Z' as not-alive so drain progresses
// normally. On Darwin/BSD there is no /proc; we keep the signal-0-only
// behavior (zombies on those platforms are rarer and the drain timeout
// remains the safety net).
//
// Mirrors internal/api/supervisor_lock.go:113-141 isOwnerLive()
// behavior, but works for ANY PID (not just the supervisor's own
// owner-sidecar PID).
func pidAliveImpl(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		// Unix: should never happen per docs, but be defensive.
		return false
	}
	if err := p.Signal(syscall.Signal(0)); err != nil {
		return false
	}
	if runtime.GOOS == "linux" && isLinuxZombie(pid) {
		return false
	}
	return true
}

// isLinuxZombie reads /proc/<pid>/stat and returns true iff the process
// state is 'Z' (zombie). On any read/parse failure returns false so the
// caller falls back to the Signal(0) verdict (we never want a probe
// failure to spuriously kill a live process).
//
// /proc/<pid>/stat format: "pid (comm) state ..." — comm CAN contain
// arbitrary bytes including spaces and parens. Per proc(5), the state
// character is the first field after the closing ')' of comm. We locate
// the LAST ')' to handle exec-renamed comm that contains its own ')'.
func isLinuxZombie(pid int) bool {
	const statPathPrefix = "/proc/"
	raw, err := os.ReadFile(statPathPrefix + itoaUint(pid) + "/stat")
	if err != nil {
		return false
	}
	idx := bytes.LastIndexByte(raw, ')')
	if idx < 0 || idx+2 >= len(raw) {
		return false
	}
	return raw[idx+2] == 'Z'
}

// itoaUint avoids the strconv import — pidAliveImpl is a hot-loop helper.
func itoaUint(n int) string {
	if n < 0 {
		return "0"
	}
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
