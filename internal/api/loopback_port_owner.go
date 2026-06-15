package api

// LoopbackPortOwnerPID resolves the PID that owns the LISTENING socket on
// 127.0.0.1:<port>. Windows uses the existing netstat-backed owner lookup;
// Linux maps /proc/net/tcp socket inodes back to /proc/<pid>/fd owners; other
// platforms fail closed until an OS-level owner proof is implemented.
func LoopbackPortOwnerPID(port int) (int, bool, error) {
	return loopbackPortOwnerPID(port)
}

// LoopbackPortOwnersSnapshot returns a single map of every IPv4-loopback
// LISTENING port to its owning PID, taken in ONE OS query (Windows: one
// `netstat -ano` spawn; Linux: one /proc/net/tcp read). It is the batch
// equivalent of calling LoopbackPortOwnerPID per port — a status refresh that
// must resolve N daemons takes ONE snapshot instead of N per-port spawns (the
// 15-netstat-spawns-per-cold-/api/status hot path). The per-port and snapshot
// paths share the same low-level netstatLineLoopbackPortPID line parser, so
// the LISTENING-state + exact-v4-loopback-address + non-zero-PID gate is
// byte-identical between them.
//
// Returns:
//   - (map, nil)  the snapshot succeeded (the map may be empty if nothing is
//     listening on the loopback).
//   - (nil, err)  the underlying OS query could not run. Callers MUST treat
//     this exactly like a per-port LoopbackPortOwnerPID error (fail closed /
//     port_owner_unverified) — a snapshot error is not proof of a dead daemon.
//
// On unsupported platforms (macOS / other POSIX) it returns the same
// errPortOwnerUnsupported sentinel the per-port path returns.
func LoopbackPortOwnersSnapshot() (map[int]int, error) {
	return loopbackPortOwnersSnapshot()
}
