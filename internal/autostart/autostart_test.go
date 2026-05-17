// Cross-platform tests for the autostart package's common surface:
//   - State enum String() coverage for every constant
//   - New() returns a non-nil Backend on the current OS
//
// Per-OS backend tests live in autostart_<os>_test.go behind build tags.
package autostart

import "testing"

// TestState_String_AllStates locks in the wire-shape of every State
// constant's String() value. `mcphub autostart status` prints this
// string verbatim to stdout, so changing it is a contract change
// scripts can grep against. New states added later must extend this
// test to keep the surface trapped.
func TestState_String_AllStates(t *testing.T) {
	cases := []struct {
		s    State
		want string
	}{
		{StateAbsent, "absent"},
		{StateEnabledRunning, "enabled-running"},
		{StateEnabledStopped, "enabled-stopped"},
		{StateDrifted, "drifted"},
		{StateStaleResidue, "stale-residue"},
	}
	for _, tc := range cases {
		got := tc.s.String()
		if got != tc.want {
			t.Errorf("State(%d).String() = %q, want %q", int(tc.s), got, tc.want)
		}
	}
}

// TestState_String_Unknown ensures we don't return "" for out-of-range
// values — `mcphub autostart status` should print something operators
// can grep instead of a blank line if a future state slips through.
func TestState_String_Unknown(t *testing.T) {
	got := State(99).String()
	if got == "" {
		t.Errorf("State(99).String() returned empty string; want a non-empty sentinel")
	}
}

// TestNew_ReturnsBackendForCurrentOS verifies the platform dispatcher
// hands back a non-nil Backend on the running OS (Windows/Linux/macOS).
// We do NOT call any of its methods — those exercise real OS primitives
// (schtasks/systemctl/launchctl) and are covered by per-OS tests with
// fakes injected through the package-var seams.
func TestNew_ReturnsBackendForCurrentOS(t *testing.T) {
	b, err := New()
	if err != nil {
		// On Windows we need user.Current() to succeed (it does in CI).
		// On Linux/macOS the constructor is straight struct-init and
		// cannot fail. Any error here is unexpected.
		t.Fatalf("New() returned err=%v on current OS; want nil", err)
	}
	if b == nil {
		t.Fatal("New() returned nil Backend; want non-nil")
	}
}
