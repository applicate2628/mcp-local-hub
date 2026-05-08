//go:build darwin

// intent_audit_caller_darwin.go — per-OS CallerStartTime helper for
// macOS. Per plan §25 v9: golang.org/x/sys/unix.SysctlKinfoProc
// (kern.proc.pid) returns a KinfoProc with Proc.P_starttime as a
// Timeval (Sec/Usec). Convert via time.Unix(Sec, Usec*1000).UTC().
//
// Falls back to time.Now().UTC() on sysctl failure so the audit-line
// path never crashes on a sandbox or container weirdness.

package api

import (
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// CallerStartTime returns the running process's start time in UTC.
// Uses kern.proc.pid sysctl via golang.org/x/sys/unix.SysctlKinfoProc.
func CallerStartTime() time.Time {
	pid := os.Getpid()
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || kp == nil {
		return time.Now().UTC()
	}
	tv := kp.Proc.P_starttime
	sec, nsec := tv.Unix()
	if sec == 0 && nsec == 0 {
		return time.Now().UTC()
	}
	return time.Unix(sec, nsec).UTC()
}
