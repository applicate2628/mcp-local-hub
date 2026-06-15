//go:build !windows

package process

// ResidentSetSizeByPID is a no-op on non-Windows targets. The supervisor
// IPC status producer (internal/cli/supervise_status.go) treats ok=false
// as "RAM unknown" and omits the per-daemon RAM metric, so Linux/macOS
// daemon cards simply render without a RAM row. A clean cross-platform RSS
// reader (parsing /proc/<pid>/statm on Linux, task_info on macOS) is a
// deliberate non-goal here — Windows is the GA target for the supervisor
// model, and hand-rolling a fragile POSIX RSS path was out of scope for
// this metric.
func ResidentSetSizeByPID(pid int) (uint64, bool) {
	_ = pid
	return 0, false
}
