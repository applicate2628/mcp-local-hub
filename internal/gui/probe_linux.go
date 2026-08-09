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
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"mcp-local-hub/internal/process"
)

// clkTck returns CLK_TCK (kernel clock-ticks-per-second), used to convert
// /proc/<pid>/stat jiffy fields to wall-clock durations. The derivation
// itself (an /proc/self/auxv AT_CLKTCK read, hardened across three rounds of
// bot review for 32-bit auxv layout, native endianness, and caching) is
// single-owned in internal/process.ClockTicksPerSecond — internal/cli's
// supervisor identity-gate start-time proof needs the exact same value, and
// internal/process is the lower-layer package both internal/gui and
// internal/cli already depend on. See process.ClockTicksPerSecond's doc for
// the full history of why the auxv reader (not a unix.Times/unix.Sysinfo
// estimate) is the correct shared implementation.
func clkTck() int64 {
	return process.ClockTicksPerSecond()
}

var (
	// bootTime is the wall-clock instant of system boot, computed
	// once from /proc/uptime + time.Now() at first use and cached for
	// the process's lifetime. Without caching, every call to
	// readStartTimeLinux recomputes bootTime from a fresh time.Now()
	// vs the same /proc/uptime, so two probes of the same PID
	// (Probe + KillRecordedHolder's internal re-probe) produce
	// drifting startTime estimates. The identity gate's 1s tolerance
	// bounds the practical risk, but the same physical event should
	// yield the same logical timestamp. Sonnet review on PR #23 F3.
	bootTimeOnce  sync.Once
	bootTimeValue time.Time
	bootTimeOK    bool
)

// systemBootTime returns the cached wall-clock instant of system boot,
// reading /proc/uptime once on first call. Returns (time.Time{}, false)
// if /proc/uptime can't be read or parsed.
func systemBootTime() (time.Time, bool) {
	bootTimeOnce.Do(func() {
		uptimeData, err := os.ReadFile("/proc/uptime")
		if err != nil {
			return
		}
		upFields := strings.Fields(string(uptimeData))
		if len(upFields) < 1 {
			return
		}
		uptimeSec, err := strconv.ParseFloat(upFields[0], 64)
		if err != nil {
			return
		}
		bootTimeValue = time.Now().Add(-time.Duration(uptimeSec * float64(time.Second)))
		bootTimeOK = true
	})
	return bootTimeValue, bootTimeOK
}

// classifyKillError maps a Kill(pid, 0) failure to the appropriate
// ProcessIdentity result. Extracted as a pure function (no syscalls) so its
// polarity is directly unit-testable with synthetic errors, without needing
// a real ambiguous kernel failure. Residual 1(a): ESRCH is kill(2)'s
// documented definitive "no such process" signal — the ONLY error this
// classifier may treat as proof of death. EPERM means the process exists but
// signaling it is denied (mirrors Windows ERROR_ACCESS_DENIED). kill(2)
// documents only EINVAL/EPERM/ESRCH as possible errors, and EINVAL cannot
// occur for signal 0 (always a valid signal number) — but a future
// kernel/libc surprise, or any errno this classifier does not recognize,
// must still fail safe: NOT proof of death, so it returns Indeterminate,
// never Alive:false. UNVERIFIED on a live Linux host this session (this
// codebase's implementer environment is Windows-only) — verification step:
// reproduce classifyKillError's ESRCH mapping against a real just-exited PID
// on a Linux runner before relying on it beyond this static/logical review.
func classifyKillError(err error) (ProcessIdentity, error) {
	if errors.Is(err, syscall.EPERM) {
		return ProcessIdentity{Alive: true, Denied: true}, nil
	}
	if errors.Is(err, syscall.ESRCH) {
		return ProcessIdentity{Alive: false}, nil
	}
	return ProcessIdentity{Indeterminate: true}, err
}

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
		return classifyKillError(err)
	}

	// /proc/<pid>/exe
	imagePath, _ := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))

	// /proc/<pid>/cmdline (NUL-delimited args). Preserve empty argv
	// tokens — they're valid in /proc/<pid>/cmdline and the identity
	// gate's `argv[1] == "gui" OR len(argv) == 1` check (the no-arg
	// launch: Explorer double-click OR a bare `mcphub` at a terminal) uses
	// positional semantics, so collapsing `mcphub ""` to a single-arg
	// argv would let a non-GUI invocation pass len(argv)==1 and
	// incorrectly authorize --force --kill.
	//
	// Subtlety: Linux always appends ONE extra trailing NUL after the
	// last argument as a record terminator. A round 1 fix used
	// strings.TrimRight which stripped that trailing NUL but ALSO
	// stripped the preceding NUL of a final-empty argv (e.g.
	// `argv=['mcphub','']` is encoded as "mcphub\x00\x00\x00" — TrimRight
	// to "" or "mcphub" loses the empty arg). Codex bot review on
	// PR #23 P1.
	//
	// Correct: strip exactly ONE trailing NUL (the kernel-emitted
	// terminator) before Split. That preserves trailing empty argv
	// while still avoiding the spurious tail-empty Split would otherwise
	// produce.
	var cmdline []string
	if data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid)); err == nil {
		s := string(data)
		if strings.HasSuffix(s, "\x00") {
			s = s[:len(s)-1]
		}
		if s != "" {
			cmdline = strings.Split(s, "\x00")
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

func retainedProcessIDImpl(pid int) (ProcessIdentity, error) {
	first, err := processIDImpl(pid)
	if err != nil {
		return ProcessIdentity{}, err
	}
	if !first.Alive || first.Denied {
		return ProcessIdentity{}, fmt.Errorf("retained process identity for pid %d is unavailable", pid)
	}
	pidfd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		return ProcessIdentity{}, fmt.Errorf("pidfd_open(%d): %w", pid, err)
	}
	closeFailure := func(cause error) (ProcessIdentity, error) {
		return ProcessIdentity{}, errors.Join(cause, unix.Close(pidfd))
	}
	if err := retainedPIDFDAlive(pidfd); err != nil {
		return closeFailure(fmt.Errorf("pidfd liveness before identity confirmation for pid %d: %w", pid, err))
	}
	second, err := processIDImpl(pid)
	if err != nil {
		return closeFailure(err)
	}
	if !sameLinuxProcessIdentity(first, second) {
		return closeFailure(fmt.Errorf("retained process identity changed for pid %d", pid))
	}
	if err := retainedPIDFDAlive(pidfd); err != nil {
		return closeFailure(fmt.Errorf("pidfd liveness after identity confirmation for pid %d: %w", pid, err))
	}
	second.Handle = uintptr(pidfd)
	return second, nil
}

func retainedPIDFDAlive(pidfd int) error {
	fds := []unix.PollFd{{Fd: int32(pidfd), Events: unix.POLLIN}}
	n, err := unix.Poll(fds, 0)
	if err != nil {
		return err
	}
	if n != 0 || fds[0].Revents != 0 {
		return fmt.Errorf("pidfd ready/revents=%#x", fds[0].Revents)
	}
	return nil
}

func sameLinuxProcessIdentity(first, second ProcessIdentity) bool {
	if first.Alive != second.Alive ||
		first.Denied != second.Denied ||
		first.ImagePath != second.ImagePath ||
		!first.StartTime.Equal(second.StartTime) ||
		len(first.Cmdline) != len(second.Cmdline) {
		return false
	}
	for i := range first.Cmdline {
		if first.Cmdline[i] != second.Cmdline[i] {
			return false
		}
	}
	return true
}

// killProcessImpl sends SIGKILL by PID. Residual TOCTOU: between
// gate-pass and Kill the kernel can in theory recycle the PID. A
// future hardening lane will switch to pidfd_send_signal on Linux
// 5.3+ (with PID-fallback on older kernels) to close the race;
// documented as residual risk for now.
func killProcessImpl(pid int) error {
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		return fmt.Errorf("kill(%d, SIGKILL): %w", pid, err)
	}
	return nil
}

func closeProcessHandle(handle uintptr) error {
	return unix.Close(int(handle))
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
	// CLK_TCK varies by kernel build (commonly 100/250/1000).
	// Hard-coding 100 was wrong: on hosts where the kernel ships
	// 250 or 1000, the computed PIDStart is off by 2.5x/10x and
	// the start-time identity gate (startTimeBeforeMtime)
	// misclassifies a legitimate mcphub gui holder as PID-recycled,
	// so --force --kill refuses with exit 7 against the correct
	// stuck incumbent. Codex bot review on PR #23.
	//
	// We read the kernel-published value via /proc/self/auxv
	// (AT_CLKTCK entry) — pure Go, no CGo — falling back to 100
	// only when /proc/self/auxv is unreadable.
	hz := clkTck()

	bootTime, ok := systemBootTime()
	if !ok {
		return time.Time{}
	}
	// Preserve sub-second jiffy precision: the prior
	// `startJiffies / hz` integer division truncated up to a full
	// jiffy ÷ Hz (≈10ms on Hz=100, but compounded across the int
	// boundary at the second-mark). The identity gate allows
	// start <= mtime + 1s, so even ~990ms of truncation could let
	// a recycled PID that started just past the threshold appear
	// valid and pass the kill gate against the wrong process.
	// Codex bot review on PR #23 P2 (jiffy precision). Compute the
	// start as nanoseconds since boot, then add to bootTime, so the
	// full jiffy resolution survives.
	startNanosSinceBoot := startJiffies * int64(time.Second) / hz
	return bootTime.Add(time.Duration(startNanosSinceBoot))
}

// matchBasename returns true iff filepath.Base(path) equals "mcphub"
// (POSIX exact, no .exe — Codex r6 #6).
func matchBasename(path string) bool {
	return filepath.Base(path) == "mcphub"
}
