//go:build windows

package api

import (
	"fmt"
	"os/exec"
	"strings"

	"mcp-local-hub/internal/process"
)

// loopbackPortOwnerPID resolves the PID that ACTUALLY owns the LISTENING
// socket on 127.0.0.1:<port> at the OS level, by scanning `netstat -ano`
// (the authoritative kernel view of who holds the socket — NOT anything the
// listener self-reports over HTTP).
//
// This is the load-bearing primitive of the serena reconcile identity proof:
// the GUI pidport file is intentionally left on disk after exit, so a local
// attacker can bind the stale loopback port and serve a fake /serena/mcp.
// Comparing the recorded pidport PID against the OS-reported socket OWNER is
// the unforgeable binding the bot's PR #252 P1 demanded — an attacker that
// reuses the port cannot make the kernel attribute the socket to the GUI's
// PID, and cannot forge a PID over HTTP that the OS would corroborate.
//
// Returns:
//   - (pid, true, nil)  a 127.0.0.1:<port> LISTENING row was found and its
//     owning PID parsed to a non-zero value.
//   - (0, false, nil)   no LISTENING row owns that loopback port (dead/stale
//     port, or nothing is listening). The caller fails closed.
//   - (0, false, err)   netstat itself could not be run, or the port is out
//     of range. The caller fails closed; the error is surfaced for
//     diagnostics.
//
// Strictness note: only the v4 loopback 127.0.0.1:<port> form is matched
// (mcphub daemons + the GUI bind v4 loopback only — the IPv6 [::1]:<port>
// form is deliberately ignored, matching the rest of the package's netstat
// scanners). The per-line match reuses netstatLinePIDForLoopbackPort
// (processes.go:254) so the LISTENING-state + exact-address gate is identical
// to status enrichment.
func loopbackPortOwnerPID(port int) (int, bool, error) {
	if port <= 0 || port > 65535 {
		return 0, false, fmt.Errorf("loopbackPortOwnerPID: port %d out of range", port)
	}
	cmd := exec.Command("netstat", "-ano")
	process.NoConsole(cmd)
	out, err := cmd.Output()
	if err != nil {
		return 0, false, fmt.Errorf("loopbackPortOwnerPID: netstat -ano failed: %w", err)
	}
	for line := range strings.SplitSeq(string(out), "\n") {
		if pid, ok := netstatLinePIDForLoopbackPort(line, port); ok {
			return pid, true, nil
		}
	}
	return 0, false, nil
}

// guiImageForPID returns the image basename of a process. It is the second
// half of the OS-level identity proof: after loopbackPortOwnerPID confirms
// the recorded PID owns the loopback socket, this confirms that PID's image
// is the mcphub binary, so a foreign image owning the port (or a foreign
// process that somehow holds the recorded PID number) still fails closed.
//
// Delegates to procNameAndParent (processes.go:525: wmic `Name` with a
// PowerShell Get-CimInstance fallback for Windows 11 24H2+ where wmic is
// removed). Returns ("", false) on any lookup failure → the caller fails
// closed.
func guiImageForPID(pid int) (string, bool) {
	image, _, ok := procNameAndParent(pid)
	if !ok {
		return "", false
	}
	return image, true
}
