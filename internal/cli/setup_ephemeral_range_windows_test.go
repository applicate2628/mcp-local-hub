//go:build windows

package cli

import (
	"bytes"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/cobra"

	"mcp-local-hub/internal/config"
)

// resetEphemeralRangeCache clears the sync.Once-cached probe so each test drives
// a fresh queryEphemeralTCPRange seam.
func resetEphemeralRangeCache(t *testing.T) {
	t.Helper()
	ephemeralRangeOnce = sync.Once{}
	ephemeralRangeCache = ephemeralRange{}
	ephemeralRangeCacheOK = false
	t.Cleanup(func() {
		ephemeralRangeOnce = sync.Once{}
		ephemeralRangeCache = ephemeralRange{}
		ephemeralRangeCacheOK = false
	})
}

func setEphemeralProbeForTest(t *testing.T, output string, err error) {
	t.Helper()
	prev := queryEphemeralTCPRange
	queryEphemeralTCPRange = func() ([]byte, error) { return []byte(output), err }
	t.Cleanup(func() { queryEphemeralTCPRange = prev })
}

// Real `netsh int ipv4 show dynamicport tcp` output shape (widened range that
// swallows the pools — the reported bug).
const netshDynamicPortWidened = `
Protocol tcp Dynamic Port Range
---------------------------------
Start Port      : 1024
Number of Ports : 13977
`

// Windows default range (49152-65535) — clear of the pools.
const netshDynamicPortDefault = `
Protocol tcp Dynamic Port Range
---------------------------------
Start Port      : 49152
Number of Ports : 16384
`

func TestParseEphemeralTCPRange_Widened(t *testing.T) {
	r, ok := parseEphemeralTCPRange([]byte(netshDynamicPortWidened))
	if !ok {
		t.Fatal("parse failed on valid widened netsh output")
	}
	if r.start != 1024 || r.count != 13977 || r.end() != 1024+13977-1 {
		t.Errorf("parsed range = {start:%d count:%d end:%d}, want {1024,13977,%d}", r.start, r.count, r.end(), 1024+13977-1)
	}
	if !r.contains(9150) || !r.contains(9205) {
		t.Errorf("widened range must contain the pool ports 9150/9205")
	}
}

func TestParseEphemeralTCPRange_Garbage(t *testing.T) {
	if _, ok := parseEphemeralTCPRange([]byte("no numbers here\njust text")); ok {
		t.Error("expected ok=false on non-numeric output")
	}
}

func TestEphemeralRangeOverlap_WidenedVsDisjoint(t *testing.T) {
	widened := ephemeralRange{start: 1024, count: 13977}
	def := ephemeralRange{start: 49152, count: 16384}
	pool := tcpRange{start: 9121, end: 9149}
	if !pool.overlaps(widened) {
		t.Error("widened ephemeral range must overlap the global pool 9121-9149")
	}
	if pool.overlaps(def) {
		t.Error("default ephemeral range (49152-65535) must NOT overlap the pool")
	}
}

func TestComputeEphemeralRangeFix_MovesAbovePools(t *testing.T) {
	pools := []tcpRange{{9121, 9149}, {9150, 9199}, {9400, 9599}}
	start, num := computeEphemeralRangeFix(pools)
	if start != 9600 {
		t.Errorf("fix start = %d, want 9600 (just above the highest pool, rounded to 100)", start)
	}
	if start+num-1 != 65535 {
		t.Errorf("fix window ends at %d, want 65535 (maximal window above the pools)", start+num-1)
	}
	// The new window must not overlap any pool.
	newRange := ephemeralRange{start: start, count: num}
	for _, p := range pools {
		if p.overlaps(newRange) {
			t.Errorf("fixed range %d-%d still overlaps pool %d-%d", start, start+num-1, p.start, p.end)
		}
	}
}

// TestMcphubEffectivePools_IncludesSerenaUpstreamBand is the P2-2 guard: the pool
// set fed to computeEphemeralRangeFix MUST include the serena native-http UPSTREAM
// band (ExternalPort + NativeHTTPInternalPortOffset), so --fix-ephemeral-range
// moves the OS dynamic range ABOVE it instead of sitting the new range over it (a
// fresh unhealable theft class on the serena backend's internal port).
//
// NON-VACUITY: without the upstream band the `found` check FAILS, and the computed
// fix would land at ~9600 — inside the ~19150-19205 upstream band.
func TestMcphubEffectivePools_IncludesSerenaUpstreamBand(t *testing.T) {
	sp, err := serenaPortPool(nil)
	if err != nil {
		t.Fatalf("serenaPortPool: %v", err)
	}
	off := config.NativeHTTPInternalPortOffset
	wantStart := sp.Start + off
	wantEnd := sp.End + off

	pools := mcphubEffectivePools()
	found := false
	for _, p := range pools {
		if p.start == wantStart && p.end == wantEnd {
			found = true
		}
	}
	if !found {
		t.Fatalf("mcphubEffectivePools missing the serena upstream band %d-%d; got %+v", wantStart, wantEnd, pools)
	}

	// The computed range fix must clear the upstream band (start strictly above it).
	start, num := computeEphemeralRangeFix(pools)
	if start <= wantEnd {
		t.Fatalf("computeEphemeralRangeFix start = %d does not clear the serena upstream band end %d; the range fix would re-introduce the theft", start, wantEnd)
	}
	if start+num-1 != 65535 {
		t.Errorf("fix window ends at %d, want 65535", start+num-1)
	}
}

func runStepCapture(t *testing.T, fix bool) (stdout, stderr string) {
	t.Helper()
	var out, errBuf bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	runSetupEphemeralRangeStep(cmd, fix)
	return out.String(), errBuf.String()
}

func TestRunSetupEphemeralRangeStep_WarnsOnOverlap(t *testing.T) {
	resetEphemeralRangeCache(t)
	setEphemeralProbeForTest(t, netshDynamicPortWidened, nil)
	_, stderr := runStepCapture(t, false)
	if !strings.Contains(stderr, "overlaps mcphub daemon pools") {
		t.Errorf("expected overlap warning; stderr:\n%s", stderr)
	}
	if !strings.Contains(stderr, "--fix-ephemeral-range") {
		t.Errorf("warning must name the remedy flag; stderr:\n%s", stderr)
	}
}

func TestRunSetupEphemeralRangeStep_NoWarnWhenDisjoint(t *testing.T) {
	resetEphemeralRangeCache(t)
	setEphemeralProbeForTest(t, netshDynamicPortDefault, nil)
	_, stderr := runStepCapture(t, false)
	if strings.Contains(stderr, "overlaps") {
		t.Errorf("no warning expected for a disjoint default range; stderr:\n%s", stderr)
	}
}

func TestRunSetupEphemeralRangeStep_FixRequiresElevation(t *testing.T) {
	resetEphemeralRangeCache(t)
	setEphemeralProbeForTest(t, netshDynamicPortWidened, nil)
	// Not elevated → the fix must NOT mutate; a set-seam call fails the test.
	prevElev := setupIsElevatedFn
	setupIsElevatedFn = func() (bool, error) { return false, nil }
	t.Cleanup(func() { setupIsElevatedFn = prevElev })
	prevSet := setEphemeralTCPRange
	setEphemeralTCPRange = func(int, int) ([]byte, error) {
		t.Fatal("setEphemeralTCPRange must NOT be called when the shell is not elevated")
		return nil, nil
	}
	t.Cleanup(func() { setEphemeralTCPRange = prevSet })

	_, stderr := runStepCapture(t, true)
	if !strings.Contains(stderr, "requires an ELEVATED shell") {
		t.Errorf("expected an elevation-required message; stderr:\n%s", stderr)
	}
}

func TestRunSetupEphemeralRangeStep_FixMutatesWhenElevated(t *testing.T) {
	resetEphemeralRangeCache(t)
	setEphemeralProbeForTest(t, netshDynamicPortWidened, nil)
	prevElev := setupIsElevatedFn
	setupIsElevatedFn = func() (bool, error) { return true, nil }
	t.Cleanup(func() { setupIsElevatedFn = prevElev })

	var gotStart, gotNum int
	prevSet := setEphemeralTCPRange
	setEphemeralTCPRange = func(start, num int) ([]byte, error) {
		gotStart, gotNum = start, num
		return []byte("Ok."), nil
	}
	t.Cleanup(func() { setEphemeralTCPRange = prevSet })

	stdout, _ := runStepCapture(t, true)
	if gotStart < 9600 {
		t.Errorf("fix set start = %d, want a value above the pools (>=9600)", gotStart)
	}
	if gotStart+gotNum-1 != 65535 {
		t.Errorf("fix window ends at %d, want 65535", gotStart+gotNum-1)
	}
	if !strings.Contains(stdout, "AFTER") {
		t.Errorf("fix must print before/after; stdout:\n%s", stdout)
	}
}
