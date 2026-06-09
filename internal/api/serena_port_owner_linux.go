//go:build linux

package api

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// loopbackPortOwnerPID resolves the PID that owns a LISTENING socket on
// 127.0.0.1:<port> using Linux's kernel socket tables. This avoids trusting a
// plain TCP connect: any local process can bind a stale daemon port, but it
// cannot make /proc attribute that listener to the tracked daemon PID.
func loopbackPortOwnerPID(port int) (int, bool, error) {
	if port <= 0 || port > 65535 {
		return 0, false, fmt.Errorf("loopbackPortOwnerPID: port %d out of range", port)
	}
	inode, ok, err := loopbackTCPListenInode(port)
	if err != nil || !ok {
		return 0, ok, err
	}
	pid, ok, err := pidForSocketInode(inode)
	if err != nil || !ok {
		return 0, ok, err
	}
	return pid, true, nil
}

func loopbackTCPListenInode(port int) (string, bool, error) {
	// Liveness is tied to the IPv4 127.0.0.1 listener ONLY: this repo writes
	// client URLs as http://127.0.0.1:<port> and the proxy bind path uses
	// 127.0.0.1, so a daemon that is alive but listening only on ::1 (IPv6
	// loopback) is unreachable by clients and must be treated as down. Do NOT
	// fall back to /proc/net/tcp6 — an IPv6-only listener returning the tracked
	// PID would make supervisorDaemonEntryLive report healthy for a daemon whose
	// 127.0.0.1 socket is dead, suppressing the restart clients need (Codex bot
	// #271 r2 P2).
	return loopbackTCPListenInodeFromProcNet("/proc/net/tcp", port)
}

func loopbackTCPListenInodeFromProcNet(path string, port int) (string, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		// /proc/net/tcp always exists on Linux (IPv4 is always present), so a
		// read failure here is a genuine probe error — not a benign missing table.
		// (The previous tcp6 fallback that could legitimately be absent was
		// removed in favor of IPv4-only liveness; see loopbackTCPListenInode.)
		return "", false, fmt.Errorf("loopbackPortOwnerPID: read %s: %w", path, err)
	}
	wantPort := strings.ToUpper(fmt.Sprintf("%04X", port))
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 10 || fields[0] == "sl" || fields[3] != "0A" {
			continue
		}
		addr, gotPort, ok := strings.Cut(fields[1], ":")
		if !ok || strings.ToUpper(gotPort) != wantPort || !isProcNetLoopbackAddress(addr) {
			continue
		}
		if fields[9] == "" || fields[9] == "0" {
			return "", false, fmt.Errorf("loopbackPortOwnerPID: listening socket on port %d has no inode", port)
		}
		return fields[9], true, nil
	}
	return "", false, nil
}

func isProcNetLoopbackAddress(addr string) bool {
	// 127.0.0.1 in /proc/net/tcp little-endian hex. Only the IPv4 loopback is
	// accepted: liveness reads /proc/net/tcp exclusively (no tcp6 fallback), so
	// the ::1 form never reaches here (Codex bot #271 r2 P2).
	return strings.ToUpper(addr) == "0100007F"
}

func pidForSocketInode(inode string) (int, bool, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0, false, fmt.Errorf("loopbackPortOwnerPID: read /proc: %w", err)
	}
	want := "socket:[" + inode + "]"
	var sawPermissionError bool
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}
		fds, err := os.ReadDir(filepath.Join("/proc", entry.Name(), "fd"))
		if err != nil {
			if os.IsPermission(err) {
				sawPermissionError = true
			}
			continue
		}
		for _, fd := range fds {
			target, err := os.Readlink(filepath.Join("/proc", entry.Name(), "fd", fd.Name()))
			if err != nil {
				if os.IsPermission(err) {
					sawPermissionError = true
				}
				continue
			}
			if target == want {
				return pid, true, nil
			}
		}
	}
	if sawPermissionError {
		// The socket inode exists in /proc/net/tcp but its owning /proc/<pid>/fd
		// is unreadable — a DIFFERENT UID owns the port. The supervisor can always
		// read its OWN daemon's fds (same UID), so an unreadable owner is provably
		// NOT the tracked daemon. Report a found-but-foreign owner (pid 0, ok=true)
		// so liveness classifies it port_owner_mismatch and restarts the daemon
		// whose port is squatted, instead of port_owner_unverified which leaves
		// the cross-user impersonation in place (Codex bot #271 P2). pid 0 != the
		// daemon's tracked PID and != the supervisor self PID → mismatch.
		return 0, true, nil
	}
	return 0, false, nil
}

// guiImageForPID is the POSIX counterpart to the Windows image lookup. The
// serena reconcile still fails closed on POSIX at this second proof step until
// image validation is implemented for those platforms.
func guiImageForPID(pid int) (string, bool) {
	return "", false
}
