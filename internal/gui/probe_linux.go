// internal/gui/probe_linux.go
//go:build linux

package gui

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// processIDImpl is the Linux implementation. Uses Kill(0) for
// liveness; reads /proc/<pid>/exe + /proc/<pid>/cmdline +
// /proc/<pid>/stat for image, argv, and start-time. macOS is split
// out into probe_darwin.go (Codex PR #23 P2 #3 iter-2) — macOS lacks
// /proc and the previous //go:build !windows tag let darwin compile
// against this Linux-only code path, where every read returned empty
// fields and the identity gate refused every kill with mysterious
// exit 7. Until a libproc/sysctl-based macOS probe lands, macOS
// returns an explicit "not supported" error from probe_darwin.go.
//
// EPERM (we're not allowed to signal the target) is treated as
// alive=true,denied=true to mirror Windows ACCESS_DENIED handling.
func processIDImpl(pid int) (ProcessIdentity, error) {
	if err := syscall.Kill(pid, 0); err != nil {
		if errors.Is(err, syscall.EPERM) {
			return ProcessIdentity{Alive: true, Denied: true}, nil
		}
		// ESRCH or other: not alive.
		return ProcessIdentity{Alive: false}, nil
	}

	// /proc/<pid>/exe
	imagePath, _ := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))

	// /proc/<pid>/cmdline (NUL-delimited args)
	var cmdline []string
	if data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid)); err == nil {
		raw := strings.Split(strings.TrimRight(string(data), "\x00"), "\x00")
		for _, a := range raw {
			if a != "" {
				cmdline = append(cmdline, a)
			}
		}
	}

	// /proc/<pid>/stat field 22 = starttime in jiffies-since-boot.
	// Convert to wall-clock approximation via /proc/uptime: the
	// design memo's identity-gate compares against pidport mtime
	// with a 1s tolerance, so jitter from this conversion is
	// acceptable. (memo §"PID identity")
	startTime := readStartTimeLinux(pid)

	return ProcessIdentity{
		Alive:     true,
		Denied:    false,
		ImagePath: imagePath,
		Cmdline:   cmdline,
		StartTime: startTime,
	}, nil
}

func killProcessImpl(pid int) error {
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		return fmt.Errorf("kill(%d, SIGKILL): %w", pid, err)
	}
	return nil
}

// readStartTimeLinux returns the process's wall-clock start time by
// combining /proc/<pid>/stat's starttime field with the system boot
// time. Returns time.Time{} on any read error.
func readStartTimeLinux(pid int) time.Time {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return time.Time{}
	}
	// Format: <pid> (<comm>) <state> <ppid> <pgrp> <session>
	//   <tty> <tpgid> <flags> <minflt> <cminflt> <majflt> <cmajflt>
	//   <utime> <stime> <cutime> <cstime> <priority> <nice>
	//   <num_threads> <itrealvalue> <starttime> ...
	// (comm) can contain spaces/parens — find the trailing ) first.
	rp := strings.LastIndexByte(string(data), ')')
	if rp == -1 || rp+2 >= len(data) {
		return time.Time{}
	}
	fields := strings.Fields(string(data[rp+2:]))
	// After ')' field 3 is state; index 19 in fields == starttime
	// (because /proc/<pid>/stat fields are 1-indexed in docs and we
	// dropped fields 1+2 by parsing post-')').
	const startTimeFieldIndex = 19
	if len(fields) <= startTimeFieldIndex {
		return time.Time{}
	}
	startJiffies, err := strconv.ParseInt(fields[startTimeFieldIndex], 10, 64)
	if err != nil {
		return time.Time{}
	}
	hz := int64(100) // CLK_TCK on most Linux configs; sysconf(_SC_CLK_TCK) would be more correct
	startSecsSinceBoot := startJiffies / hz

	uptimeData, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return time.Time{}
	}
	upFields := strings.Fields(string(uptimeData))
	if len(upFields) < 1 {
		return time.Time{}
	}
	uptimeSec, err := strconv.ParseFloat(upFields[0], 64)
	if err != nil {
		return time.Time{}
	}
	bootTime := time.Now().Add(-time.Duration(uptimeSec * float64(time.Second)))
	return bootTime.Add(time.Duration(startSecsSinceBoot) * time.Second)
}

// matchBasename returns true iff filepath.Base(path) equals "mcphub"
// (POSIX exact, no .exe — Codex r6 #6).
func matchBasename(path string) bool {
	return filepath.Base(path) == "mcphub"
}
