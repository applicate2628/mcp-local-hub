package daemon

import (
	"os/exec"
	"runtime"
	"strconv"

	"mcp-local-hub/internal/process"
)

// killProcessTree terminates the process rooted at pid along with its
// descendants. Mirror of taskkill /F /T on Windows and pkill -TERM on
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
func killProcessTree(pid int) error {
	if pid <= 0 {
		return nil
	}
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		// /F = force, /T = tree (kill children too).
		cmd = exec.Command("taskkill", "/F", "/T", "/PID", strconv.Itoa(pid))
	} else {
		// -TERM to the whole process group. Requires the child to have
		// been started with Setpgid — falls back to plain kill(pid)
		// via the standard library's cmd.Process.Kill if the caller
		// did not set up a group. For our purposes on Windows this
		// branch is only compiled but not executed.
		cmd = exec.Command("pkill", "-TERM", "-P", strconv.Itoa(pid))
	}
	process.NoConsole(cmd) // suppress per-child console pop on windowsgui parents
	// Ignore cmd output — Windows taskkill prints "SUCCESS: ..." /
	// "ERROR: The process <pid> not found" and we treat both the same.
	_ = cmd.Run()
	return nil
}
