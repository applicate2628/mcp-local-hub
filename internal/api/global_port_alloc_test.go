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
	// 9200/9201 bound by an unrelated process; first OS-free is 9202.
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
