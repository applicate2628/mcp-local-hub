//go:build linux

package api

import (
	"context"
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
	return loopbackPortOwnerPIDContext(context.Background(), port)
}

// loopbackPortOwnerPIDContext is the context-bounded form. The Linux owner lookup
// walks /proc/<pid>/fd to map the listening socket inode back to its owner — an
// O(processes × fds) scan that on a host with a large/slow /proc tree can take
// meaningfully long. P2-C: ctx is honored DURING that walk (pidForSocketInodeContext
// checks ctx.Err() per /proc entry) so the caller's deadline (the supervisor's 2s
// portGateProbeDeadline) is actually enforced and the controller loop cannot block
// past it. loopbackPortOwnerPID delegates here with context.Background(), which
// never trips, so the non-ctx path stays byte-identical.
func loopbackPortOwnerPIDContext(ctx context.Context, port int) (int, bool, error) {
	if err := ctx.Err(); err != nil {
		return 0, false, err
	}
	if port <= 0 || port > 65535 {
		return 0, false, fmt.Errorf("loopbackPortOwnerPID: port %d out of range", port)
	}
	inode, ok, err := loopbackTCPListenInode(port)
	if err != nil || !ok {
		return 0, ok, err
	}
	return pidForSocketInodeContext(ctx, inode)
}

// loopbackPortOwnersSnapshot maps every IPv4 loopback LISTENING port to its
// owning PID in ONE pass over /proc/net/tcp plus ONE walk of /proc — the batch
// form of loopbackPortOwnerPID. A status refresh resolving N daemons reads the
// socket tables once instead of N times.
//
// Per-port fidelity preserved:
//   - port present, owner pid resolved → map[port] = pid (same as the per-port
//     (pid, true, nil));
//   - port whose socket inode is owned by a DIFFERENT UID (its /proc/<pid>/fd
//     is permission-denied) → map[port] = 0, so the caller sees a found owner
//     that mismatches the tracked daemon → port_owner_mismatch, exactly as the
//     per-port pidForSocketInode returns (0, true, nil) for the squatted-port
//     cross-user case (Codex bot #271 P2);
//   - port with no resolvable owner and no permission signal → ABSENT from the
//     map → caller treats it as port_unbound, matching the per-port
//     (0, false, nil).
//
// A read failure of /proc/net/tcp or /proc is a genuine probe error and is
// returned as (nil, err) so every port the caller asks about fails closed to
// port_owner_unverified — never a restart.
func loopbackPortOwnersSnapshot() (map[int]int, error) {
	return loopbackPortOwnersSnapshotFromProcNet("/proc/net/tcp")
}

// loopbackPortOwnersSnapshotFromProcNet is the path-injectable core of
// loopbackPortOwnersSnapshot (the procNetPath seam mirrors
// loopbackTCPListenInodeFromProcNet so tests can feed a canned table).
func loopbackPortOwnersSnapshotFromProcNet(procNetPath string) (map[int]int, error) {
	portInodes, err := loopbackListenPortInodesFromProcNet(procNetPath)
	if err != nil {
		return nil, err
	}
	owners := map[int]int{}
	if len(portInodes) == 0 {
		return owners, nil
	}
	inodePIDs, sawPermissionError, err := pidsForSocketInodes(portInodes)
	if err != nil {
		return nil, err
	}
	for port, inode := range portInodes {
		if pid, ok := inodePIDs[inode]; ok {
			owners[port] = pid
			continue
		}
		// Inode present in /proc/net/tcp but no readable /proc/<pid>/fd owns it.
		// If we hit a permission wall while walking /proc, a DIFFERENT UID owns
		// the socket (the supervisor can always read its own daemon's fds), so
		// report a found-but-foreign owner (pid 0) → port_owner_mismatch, exactly
		// like the per-port pidForSocketInode path. Without a permission signal
		// the owner is simply unresolved → leave the port out of the map so the
		// caller treats it as port_unbound.
		if sawPermissionError {
			owners[port] = 0
		}
	}
	return owners, nil
}

// loopbackListenPortInodesFromProcNet reads /proc/net/tcp once and returns
// port -> socket inode for every IPv4 loopback LISTENING (st == 0A) row.
// Mirrors loopbackTCPListenInodeFromProcNet's per-row gate exactly. Rows whose
// inode is empty/"0" are skipped (a listening socket without an inode is the
// per-port error case; in batch form a skipped port reads as port_unbound,
// which is the safe fail-closed direction and never drives a restart on its
// own — see supervisorLivenessReasonNeedsRestart). A read failure is a probe
// error.
func loopbackListenPortInodesFromProcNet(path string) (map[int]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("loopbackPortOwnersSnapshot: read %s: %w", path, err)
	}
	out := map[int]string{}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 10 || fields[0] == "sl" || fields[3] != "0A" {
			continue
		}
		addr, gotPort, ok := strings.Cut(fields[1], ":")
		if !ok || !isProcNetLoopbackAddress(addr) {
			continue
		}
		port, err := strconv.ParseInt(gotPort, 16, 32)
		if err != nil || port <= 0 {
			continue
		}
		inode := fields[9]
		if inode == "" || inode == "0" {
			continue
		}
		if _, seen := out[int(port)]; !seen {
			out[int(port)] = inode
		}
	}
	return out, nil
}

// pidsForSocketInodes walks /proc ONCE and returns the subset of the requested
// inodes that map to a readable /proc/<pid>/fd owner, plus whether any
// permission error was seen while walking (so the caller can classify
// unresolved inodes as foreign-owned). Batch form of pidForSocketInode.
func pidsForSocketInodes(wantInodes map[int]string) (map[string]int, bool, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, false, fmt.Errorf("loopbackPortOwnersSnapshot: read /proc: %w", err)
	}
	want := map[string]struct{}{}
	for _, inode := range wantInodes {
		want["socket:["+inode+"]"] = struct{}{}
	}
	found := map[string]int{}
	var sawPermissionError bool
	for _, entry := range entries {
		if len(found) == len(want) {
			break
		}
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
			if _, ok := want[target]; ok {
				// target is "socket:[<inode>]"; strip back to the bare inode key.
				inode := strings.TrimSuffix(strings.TrimPrefix(target, "socket:["), "]")
				if _, already := found[inode]; !already {
					found[inode] = pid
				}
			}
		}
	}
	return found, sawPermissionError, nil
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
	return pidForSocketInodeContext(context.Background(), inode)
}

// pidForSocketInodeContext is the context-bounded /proc walk (P2-C). It checks
// ctx.Err() at the top of each /proc-entry iteration — BEFORE that entry's
// per-fd readlink scan — so a canceled/deadline-exceeded ctx aborts promptly with
// the ctx error instead of walking the whole (possibly large/slow) process tree.
// pidForSocketInode delegates here with context.Background() (never trips), so the
// non-ctx behavior is byte-identical.
func pidForSocketInodeContext(ctx context.Context, inode string) (int, bool, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0, false, fmt.Errorf("loopbackPortOwnerPID: read /proc: %w", err)
	}
	want := "socket:[" + inode + "]"
	var sawPermissionError bool
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return 0, false, err
		}
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
			// P2-iv (Codex PR-3): also honor the deadline INSIDE a single entry's fd
			// scan — one process before the socket owner with a huge /proc/<pid>/fd
			// dir would otherwise let this inner loop run past the deadline even
			// though the per-entry check passed. ctx.Err() is a cheap atomic load.
			if err := ctx.Err(); err != nil {
				return 0, false, err
			}
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
