//go:build linux

// intent_audit_caller_linux.go — per-OS CallerStartTime helper for
// Linux. Per plan §25 v9:
//
//   - /proc/self/stat field 22 = starttime (jiffies since boot).
//   - /proc/stat line "btime <n>" = boot time (Unix epoch seconds).
//   - HZ default 100 (POSIX-required minimum on x86; golang.org/x/sys
//     does not expose sysconf(_SC_CLK_TCK) so we use the documented
//     plan-§25 fallback).
//
// Final = time.Unix(btime + starttime/HZ, 0).UTC().
//
// Falls back to time.Now().UTC() on any read/parse failure so the
// audit-line path never crashes on a /proc oddity (containerized
// runtimes that mask /proc, namespace migrations, etc.). Best-effort.

package api

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// CallerStartTime returns the running process's start time in UTC.
// Reads /proc/self/stat for the per-process starttime jiffies and
// /proc/stat for the system boot time, then converts via the system
// clock-tick rate.
func CallerStartTime() time.Time {
	bootSec, err := readBootTimeSec()
	if err != nil {
		return time.Now().UTC()
	}
	jiffies, err := readSelfStartJiffies()
	if err != nil {
		return time.Now().UTC()
	}
	hz := readClkTck()
	if hz <= 0 {
		hz = 100 // documented fallback
	}
	startSec := bootSec + jiffies/hz
	return time.Unix(startSec, 0).UTC()
}

// readBootTimeSec parses /proc/stat for the "btime <n>" line and
// returns n (Unix epoch seconds).
func readBootTimeSec() (int64, error) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, fmt.Errorf("read /proc/stat: %w", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "btime ") {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) < 2 {
			return 0, errors.New("/proc/stat btime line malformed")
		}
		n, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("parse btime: %w", err)
		}
		return n, nil
	}
	return 0, errors.New("/proc/stat missing btime line")
}

// readSelfStartJiffies reads field 22 of /proc/self/stat (starttime).
// Field indexing per proc(5): field 1 = pid, field 2 = comm in
// parens (may contain spaces), field 3 = state, fields 4-22 are
// per-process counters. We split on the LAST ')' to skip past comm
// reliably.
func readSelfStartJiffies() (int64, error) {
	data, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		return 0, fmt.Errorf("read /proc/self/stat: %w", err)
	}
	rp := bytes.LastIndexByte(data, ')')
	if rp < 0 {
		return 0, errors.New("/proc/self/stat missing ')' (comm field)")
	}
	rest := strings.TrimSpace(string(data[rp+1:]))
	fields := strings.Fields(rest)
	// field 3 (state) is fields[0]; field 22 (starttime) is fields[19].
	if len(fields) < 20 {
		return 0, fmt.Errorf("/proc/self/stat too few fields: %d", len(fields))
	}
	n, err := strconv.ParseInt(fields[19], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse starttime jiffies: %w", err)
	}
	return n, nil
}

// readClkTck returns the kernel jiffy rate. golang.org/x/sys/unix does
// not expose sysconf(_SC_CLK_TCK) on Linux without cgo; the plan §25
// v9 fallback is 100, which matches every modern x86 build (CONFIG_HZ
// is 100/250/300/1000 in mainline; userspace sees 100 unless built
// with cgo to call sysconf). Production accuracy is acceptable: the
// audit-line caller_start_time field is forensic, not load-bearing
// for any decision; ±10ms across a multi-month process lifetime is
// inconsequential.
func readClkTck() int64 {
	return 100
}

