//go:build linux

package process

import (
	"bytes"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
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
	const hz = int64(100)
	return time.Unix(bootSec+jiffies/hz, 0).UTC(), true
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
