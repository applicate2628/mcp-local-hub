package process

import (
	"os"
	"runtime"
	"testing"
)

// TestResidentSetSizeByPID_Self exercises the platform RSS helper against
// the current process. No state path, registry, scheduler, or supervisor is
// touched — it only reads this test process's own working set.
//
// On Windows the GetProcessMemoryInfo syscall must succeed and report a
// plausible (> 0) working set; this also validates the hand-laid-out
// PROCESS_MEMORY_COUNTERS struct (a wrong CB / field offset would make the
// syscall fail or return garbage). On non-Windows the helper is a documented
// no-op returning (0, false).
func TestResidentSetSizeByPID_Self(t *testing.T) {
	rss, ok := ResidentSetSizeByPID(os.Getpid())
	if runtime.GOOS == "windows" {
		if !ok {
			t.Fatal("ResidentSetSizeByPID(self): ok=false on Windows; GetProcessMemoryInfo should succeed for the current process")
		}
		if rss == 0 {
			t.Fatal("ResidentSetSizeByPID(self): RSS=0 on Windows; a live process always has a non-zero working set")
		}
	} else {
		if ok || rss != 0 {
			t.Fatalf("ResidentSetSizeByPID(self) on %s: got (%d, %v), want (0, false) — non-Windows is a no-op", runtime.GOOS, rss, ok)
		}
	}
}

// TestResidentSetSizeByPID_InvalidPID asserts the helper fails closed for a
// non-positive PID without attempting any syscall.
func TestResidentSetSizeByPID_InvalidPID(t *testing.T) {
	for _, pid := range []int{0, -1} {
		if rss, ok := ResidentSetSizeByPID(pid); ok || rss != 0 {
			t.Fatalf("ResidentSetSizeByPID(%d) = (%d, %v), want (0, false)", pid, rss, ok)
		}
	}
}
