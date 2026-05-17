//go:build linux

// supervise_reaper_starttime_linux.go — Linux implementation of the
// per-PID process-start-time probe used by the cold-start reaper's
// StartedAt gate (Lane F P0 #4).
//
// Method (proc(5)):
//
//   - /proc/<pid>/stat field 22 = starttime (jiffies since boot).
//   - /proc/stat "btime <n>" line = boot time (Unix epoch seconds).
//   - HZ defaults to 100 — sysconf(_SC_CLK_TCK) is not exposed by
//     golang.org/x/sys/unix without cgo and 100 matches every modern
//     x86 build's userspace-visible jiffy rate (Linux's CONFIG_HZ in
//     mainline is one of 100/250/300/1000, but userspace ALWAYS sees
//     100 via clock_gettime / proc, independent of CONFIG_HZ — see
//     `man 7 time`).
//
// Final = time.Unix(btime + starttime/HZ, 0).UTC().
//
// On any read/parse error returns (time.Time{}, false) so the reaper
// fails closed: the gate treats "cannot determine start time" the
// same as "start-time mismatch" → skip kill, log warn. That is the
// safe failure mode (no kill is strictly safer than a wrong kill).
//
// Mirrors the canonical approach in internal/api/intent_audit_caller_linux.go
// (CallerStartTime + readBootTimeSec + readSelfStartJiffies) but reads
// an arbitrary <pid> instead of self. The duplication is deliberate —
// the audit-caller path is a per-process self-read forensic helper,
// while this is a per-target reaper helper with a different failure
// contract (returns ok=false rather than time.Now() fallback). Sharing
// a single multi-pid helper across the two surfaces is left as a
// follow-up refactor; the v0.5.0 codex post-review scope is narrow.

package cli

import (
	"bytes"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// processStartTime returns the wall-clock start time of <pid>. Returns
// (zero, false) on any failure so callers can branch on ok.
func processStartTime(pid int) (time.Time, bool) {
	if pid <= 0 {
		return time.Time{}, false
	}
	jiffies, ok := readStartJiffies(pid)
	if !ok {
		return time.Time{}, false
	}
	bootSec, ok := readBootTimeSec()
	if !ok {
		return time.Time{}, false
	}
	hz := int64(100) // userspace-visible clock-tick rate; see file header
	return time.Unix(bootSec+jiffies/hz, 0).UTC(), true
}

// readStartJiffies parses /proc/<pid>/stat field 22 (starttime in
// jiffies since boot). The comm field at index 2 is wrapped in parens
// and may contain spaces/parens itself, so the parse anchors on the
// LAST ')' to skip past comm reliably (proc(5) convention).
func readStartJiffies(pid int) (int64, bool) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, false
	}
	rp := bytes.LastIndexByte(data, ')')
	if rp < 0 {
		return 0, false
	}
	rest := strings.TrimSpace(string(data[rp+1:]))
	fields := strings.Fields(rest)
	// After ')' field 3 (state) is fields[0]; field 22 (starttime) is
	// fields[19]. proc(5) describes per-process counters as 1-indexed
	// starting with pid; the parens-trim drops fields 1+2, so the
	// 22nd documented field is at fields[19] here.
	const startTimeFieldIndex = 19
	if len(fields) <= startTimeFieldIndex {
		return 0, false
	}
	n, err := strconv.ParseInt(fields[startTimeFieldIndex], 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// readBootTimeSec parses /proc/stat "btime <n>" and returns n (Unix
// epoch seconds since boot).
func readBootTimeSec() (int64, bool) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "btime ") {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) < 2 {
			return 0, false
		}
		n, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			return 0, false
		}
		return n, true
	}
	return 0, false
}
