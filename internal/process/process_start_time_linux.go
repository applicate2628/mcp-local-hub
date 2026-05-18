//go:build linux

package process

import (
	"bytes"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// ProcessStartTime returns the kernel-recorded wall-clock start time of pid.
// It fails closed with ok=false on any /proc read or parse failure.
func ProcessStartTime(pid int) (time.Time, bool) {
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
	hz, ok := clockTicksPerSecond()
	if !ok {
		return time.Time{}, false
	}
	return processStartTimeFromBootAndJiffies(bootSec, jiffies, hz)
}

func processStartTimeFromBootAndJiffies(bootSec, jiffies, hz int64) (time.Time, bool) {
	if hz <= 0 || jiffies < 0 {
		return time.Time{}, false
	}
	sec := jiffies / hz
	nsec := (jiffies % hz) * int64(time.Second) / hz
	return time.Unix(bootSec+sec, nsec).UTC(), true
}

func clockTicksPerSecond() (int64, bool) {
	var tms unix.Tms
	ticks, err := unix.Times(&tms)
	if err != nil || ticks == 0 {
		return 0, false
	}
	var info unix.Sysinfo_t
	if err := unix.Sysinfo(&info); err != nil || info.Uptime <= 0 {
		return 0, false
	}
	hz := int64((ticks + uintptr(info.Uptime/2)) / uintptr(info.Uptime))
	if hz <= 0 {
		return 0, false
	}
	return hz, true
}

func readStartJiffies(pid int) (int64, bool) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, false
	}
	rp := bytes.LastIndexByte(data, ')')
	if rp < 0 {
		return 0, false
	}
	fields := strings.Fields(strings.TrimSpace(string(data[rp+1:])))
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
