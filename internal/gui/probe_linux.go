// internal/gui/probe_linux.go
//go:build linux

package gui

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math/bits"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// clkTck reads CLK_TCK (kernel clock-ticks-per-second) from the
// process's ELF auxiliary vector at /proc/self/auxv. The auxv is a
// kernel-emitted array of (type, value) pairs; AT_CLKTCK (type 17)
// carries what sysconf(_SC_CLK_TCK) would return — without needing
// CGo. Falls back to 100 (the most common kernel default) if the
// auxv is unreadable or the entry is missing; this keeps probe
// telemetry usable on minimal containers that hide /proc/self/auxv.
//
// Each auxv entry is two C unsigned longs — 16 bytes on 64-bit
// builds (amd64/arm64), 8 bytes on 32-bit builds (386/arm). The
// list terminates with type AT_NULL = 0. Word size is constant
// per Go build, so we pick the right decoder at compile time via
// math/bits.UintSize. Codex bot review on PR #23 P2 (auxv 32-bit
// parsing).
//
// Cached after first successful read because CLK_TCK is a kernel
// build-time constant — it never changes for the life of the process.
var (
	clkTckOnce  sync.Once
	clkTckValue int64
)

const (
	atClkTckType = 17                  // AT_CLKTCK in <elf.h>
	atNullType   = 0                   // AT_NULL terminator
	auxvWordSize = bits.UintSize / 8   // 4 on 32-bit, 8 on 64-bit Go builds
	auxvEntryLen = 2 * auxvWordSize    // 8 on 32-bit, 16 on 64-bit
)

// readAuxvWord decodes one auxv field (sized to the native pointer)
// at offset i in data as a uint64. Compile-time constant branching
// — the wrong-arch arm is dead-code-eliminated.
func readAuxvWord(data []byte, i int) uint64 {
	if auxvWordSize == 8 {
		return binary.LittleEndian.Uint64(data[i : i+8])
	}
	return uint64(binary.LittleEndian.Uint32(data[i : i+4]))
}

func clkTck() int64 {
	clkTckOnce.Do(func() {
		clkTckValue = 100 // safe default if auxv is unreadable
		data, err := os.ReadFile("/proc/self/auxv")
		if err != nil {
			return
		}
		for i := 0; i+auxvEntryLen <= len(data); i += auxvEntryLen {
			atype := readAuxvWord(data, i)
			avalue := readAuxvWord(data, i+auxvWordSize)
			if atype == atNullType {
				break
			}
			if atype == atClkTckType && avalue > 0 {
				clkTckValue = int64(avalue)
				return
			}
		}
	})
	return clkTckValue
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
