package api

import (
	"errors"
	"testing"
)

// TestAllocateSingleGlobalPort_FirstFreeInEmptyBand returns the band start
// when nothing is taken and every port binds free.
func TestAllocateSingleGlobalPort_FirstFreeInEmptyBand(t *testing.T) {
	origAvail := portAvailable
	defer func() { portAvailable = origAvail }()
	portAvailable = func(int) bool { return true }
	got, err := AllocateSingleGlobalPort(nil)
	if err != nil {
		t.Fatalf("AllocateSingleGlobalPort: %v", err)
	}
	if got != globalDaemonBandStart {
		t.Errorf("got %d, want %d (band start)", got, globalDaemonBandStart)
	}
}

// TestAllocateSingleGlobalPort_SkipsTakenPorts fills holes: a port already
// owned by an installed manifest daemon is skipped, the next free one wins.
func TestAllocateSingleGlobalPort_SkipsTakenPorts(t *testing.T) {
	origAvail := portAvailable
	defer func() { portAvailable = origAvail }()
	portAvailable = func(int) bool { return true }
	taken := map[int]bool{globalDaemonBandStart: true, globalDaemonBandStart + 1: true}
	got, err := AllocateSingleGlobalPort(taken)
	if err != nil {
		t.Fatalf("AllocateSingleGlobalPort: %v", err)
	}
	if got != globalDaemonBandStart+2 {
		t.Errorf("got %d, want %d (first free after two taken)", got, globalDaemonBandStart+2)
	}
}

// TestAllocateSingleGlobalPort_SkipsOSBoundPorts honors the OS-level bind
// probe so a port held by an unrelated process is never handed out.
func TestAllocateSingleGlobalPort_SkipsOSBoundPorts(t *testing.T) {
	origAvail := portAvailable
	defer func() { portAvailable = origAvail }()
	// band-start/+1 bound by an unrelated process; first OS-free is band-start+2.
	portAvailable = func(port int) bool { return port >= globalDaemonBandStart+2 }
	got, err := AllocateSingleGlobalPort(nil)
	if err != nil {
		t.Fatalf("AllocateSingleGlobalPort: %v", err)
	}
	if got != globalDaemonBandStart+2 {
		t.Errorf("got %d, want %d (two OS-bound skipped)", got, globalDaemonBandStart+2)
	}
}

// TestAllocateSingleGlobalPort_ExhaustedWhenEntireBandUnavailable returns a
// wrapped ErrPortPoolExhausted rather than a bad port when nothing is free.
func TestAllocateSingleGlobalPort_ExhaustedWhenEntireBandUnavailable(t *testing.T) {
	origAvail := portAvailable
	defer func() { portAvailable = origAvail }()
	portAvailable = func(int) bool { return false }
	_, err := AllocateSingleGlobalPort(nil)
	if err == nil {
		t.Fatal("expected ErrPortPoolExhausted when every band port is OS-bound")
	}
	if !errors.Is(err, ErrPortPoolExhausted) {
		t.Errorf("error should unwrap to ErrPortPoolExhausted; got: %v", err)
	}
}

// TestPortInGlobalDaemonBand pins the band predicate used to validate an
// operator-supplied ?port override.
func TestPortInGlobalDaemonBand(t *testing.T) {
	cases := []struct {
		port int
		want bool
	}{
		{globalDaemonBandStart - 1, false},
		{globalDaemonBandStart, true},
		{globalDaemonBandStart + 50, true},
		{globalDaemonBandEnd, true},
		{globalDaemonBandEnd + 1, false},
		{0, false},
		{9125, false}, // the GUI port — must never be in band
	}
	for _, c := range cases {
		if got := PortInGlobalDaemonBand(c.port); got != c.want {
			t.Errorf("PortInGlobalDaemonBand(%d) = %v, want %v", c.port, got, c.want)
		}
	}
}

// TestGlobalDaemonBandDisjointFromOtherAllocators pins the invariant that the
// marketplace single-daemon band does NOT overlap any other allocator's range
// — specifically the workspace-scoped LSP daemon port_pool (9200–9299), the
// serena dynamic pool (9150–9199), or the GUI port (9125). A shared band would
// let the marketplace allocator hand out a port another allocator later binds
// (the batch-2 review finding this band move fixes). The band is carved ABOVE
// the LSP pool ceiling, so band-start must clear 9299.
func TestGlobalDaemonBandDisjointFromOtherAllocators(t *testing.T) {
	const lspPoolEnd = 9299 // servers/mcp-language-server/manifest.yaml port_pool ceiling
	const serenaPoolEnd = 9199
	const guiPort = 9125
	if globalDaemonBandStart <= lspPoolEnd {
		t.Errorf("marketplace band start %d overlaps the LSP workspace pool (≤%d) — collision risk",
			globalDaemonBandStart, lspPoolEnd)
	}
	if globalDaemonBandStart <= serenaPoolEnd {
		t.Errorf("marketplace band start %d overlaps the serena pool (≤%d)", globalDaemonBandStart, serenaPoolEnd)
	}
	if PortInGlobalDaemonBand(guiPort) {
		t.Errorf("GUI port %d must never fall inside the marketplace band", guiPort)
	}
	if globalDaemonBandEnd < globalDaemonBandStart {
		t.Errorf("band end %d precedes band start %d", globalDaemonBandEnd, globalDaemonBandStart)
	}
}
