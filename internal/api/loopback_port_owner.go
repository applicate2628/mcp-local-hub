package api

import "context"

// LoopbackPortOwnerPID resolves the PID that owns the LISTENING socket on
// 127.0.0.1:<port>. Windows uses the existing netstat-backed owner lookup;
// Linux maps /proc/net/tcp socket inodes back to /proc/<pid>/fd owners; other
// platforms fail closed until an OS-level owner proof is implemented.
//
// It is now a context.Background() delegate of LoopbackPortOwnerPIDContext so
// every existing caller (serena reconcile, status enrichment, `daemon recover`)
// stays byte-identical — a background context never cancels, so the Windows
// exec.CommandContext behaves exactly like the prior exec.Command.
func LoopbackPortOwnerPID(port int) (int, bool, error) {
	return LoopbackPortOwnerPIDContext(context.Background(), port)
}

// LoopbackPortOwnerPIDContext is the context-bounded owner probe: on Windows the
// underlying `netstat -ano` runs under exec.CommandContext, so a canceled or
// deadline-exceeded ctx kills the shell-out instead of blocking the caller. It
// is the seam the supervisor's F1 pre-spawn gate uses so the netstat probe on
// the controller event-loop goroutine can never hang the loop: the gate wraps a
// short deadline around this and treats ctx cancellation as a probe error →
// fail-open (proceed to spawn), exactly like any other netstat failure. On
// Linux/other platforms the underlying lookup does not shell out, so ctx is
// honored as a pre-read cancellation check only.
func LoopbackPortOwnerPIDContext(ctx context.Context, port int) (int, bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	return loopbackPortOwnerPIDContext(ctx, port)
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
	return LoopbackPortOwnersSnapshotContext(context.Background())
}

// LoopbackPortOwnersSnapshotContext is the context-bounded batch snapshot: the
// deadline is enforced on the OS query (Windows: netstat -ano under
// exec.CommandContext; Linux/other: honored as a pre-read cancellation check).
// The seam the supervisor status coalescer uses to bound how long a status
// refresh can wait on netstat, so a pathologically slow network stack degrades
// to a snapshot error (-> per-daemon port_owner_unverified) instead of a hung
// status IPC that trips the GUI restart-watcher timeout. LoopbackPortOwnersSnapshot
// delegates here with context.Background(), so non-ctx callers are byte-identical.
func LoopbackPortOwnersSnapshotContext(ctx context.Context) (map[int]int, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	return loopbackPortOwnersSnapshotContext(ctx)
}
