//go:build linux

package process

import "testing"

// TestClockTicksPerSecond_ReturnsPositive verifies the public entry point
// never returns a non-positive value — every tier (auxv, estimate, 100
// fallback) yields a usable HZ. processStartTimeFromBootAndJiffies rejects
// hz <= 0, so a zero here would silently disable start-time proofs.
func TestClockTicksPerSecond_ReturnsPositive(t *testing.T) {
	if hz := ClockTicksPerSecond(); hz <= 0 {
		t.Fatalf("ClockTicksPerSecond() = %d, want > 0", hz)
	}
}

// TestClkTckFromUptimeEstimate_IsTheSecondaryFallback pins bot PR #474 P2: the
// unix.Times/unix.Sysinfo estimate that the CLK_TCK unification dropped is
// restored as the tier-2 fallback. On a real Linux test host both syscalls
// succeed, so the estimate resolves to a positive, plausibly-near-real HZ
// (kernels ship 100/250/300/1000). This proves the estimate is reachable and
// sane, not a dead branch behind a blind 100.
func TestClkTckFromUptimeEstimate_IsTheSecondaryFallback(t *testing.T) {
	hz, ok := clkTckFromUptimeEstimate()
	if !ok {
		// Times/Sysinfo can legitimately fail in some sandboxes; in that case
		// the tier-3 100 default still applies. Skip rather than fail so the
		// test stays meaningful only where the estimate is actually exercised.
		t.Skip("unix.Times/unix.Sysinfo unavailable in this sandbox; tier-2 not exercisable here")
	}
	if hz <= 0 {
		t.Fatalf("clkTckFromUptimeEstimate() = %d, want > 0", hz)
	}
	// Sanity bound: a real kernel HZ is small (commonly <= 1000). A wildly
	// large value would indicate the uptime/ticks ratio is being computed
	// inverted.
	if hz > 100000 {
		t.Fatalf("clkTckFromUptimeEstimate() = %d, implausibly large for a kernel CLK_TCK", hz)
	}
}

// TestClkTckFromAuxv_PrimaryReadsKernelValue verifies the primary auxv reader
// resolves on a normal Linux host (where /proc/self/auxv is exposed). A miss
// here on a standard runner would mean every process silently falls through to
// the estimate; the test makes the primary path's health observable.
func TestClkTckFromAuxv_PrimaryReadsKernelValue(t *testing.T) {
	hz, ok := clkTckFromAuxv()
	if !ok {
		t.Skip("/proc/self/auxv hidden or lacks AT_CLKTCK in this sandbox; primary tier not exercisable here")
	}
	if hz <= 0 {
		t.Fatalf("clkTckFromAuxv() = %d, want > 0", hz)
	}
}
