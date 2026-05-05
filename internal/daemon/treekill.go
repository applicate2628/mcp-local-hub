package daemon

import (
	"fmt"
	"os/exec"
	"runtime"
	"strconv"

	"mcp-local-hub/internal/process"
)

// killProcessTree terminates the process rooted at pid along with its
// descendants. Mirror of taskkill /F /T on Windows and pkill -KILL on
// Unix. Returns an error only when the underlying command itself
// cannot be invoked; "pid does not exist" is silently tolerated so
// callers can use this in Stop() paths where the process may already
// have exited.
//
// Why tree-kill rather than plain cmd.Process.Kill:
//
// Many MCP servers are started via wrapper launchers — uvx (serena),
// npx (memory, sequential-thinking), uv (gdb), node (wolfram). The
// wrapper spawns the real server as a child and exits or lingers.
// os.Process.Kill only kills the immediate child (the wrapper). The
// real server keeps running, keeps its port bound, and confuses every
// subsequent Stop() or port-free check. Tree-kill terminates the
// whole subtree so the port is genuinely free when Stop returns.
//
// Why SIGKILL on POSIX rather than SIGTERM:
//
// Codex bot P2 on 34b1a30: SIGTERM is ignorable. TERM-ignoring or
// slow-shutdown MCP servers would survive Stop's 1s wait, and Stop
// would return success while the child + watcher are still alive,
// causing leaked daemons and port-collision restart failures. The
// caller (Stop) already closed stdin first so any well-behaved child
// already got its graceful-shutdown signal; killProcessTree is the
// force path and must actually force.
func killProcessTree(pid int) error {
	if pid <= 0 {
		return nil
	}
	if runtime.GOOS == "windows" {
		// /F = force, /T = tree (kill children too).
		cmd := exec.Command("taskkill", "/F", "/T", "/PID", strconv.Itoa(pid))
		process.NoConsole(cmd) // suppress per-child console pop on windowsgui parents
		// Ignore cmd output — Windows taskkill prints "SUCCESS: ..." /
		// "ERROR: The process <pid> not found" and we treat both the same.
		_ = cmd.Run()
		return nil
	}

	// POSIX: SIGKILL descendants then the root. pkill -P targets direct
	// children only; follow with kill -KILL so the root is signaled too.
	childKill := exec.Command("pkill", "-KILL", "-P", strconv.Itoa(pid))
	process.NoConsole(childKill)
	_ = childKill.Run()

	rootKill := exec.Command("kill", "-KILL", fmt.Sprintf("%d", pid))
	process.NoConsole(rootKill)
	_ = rootKill.Run()
	return nil
}
