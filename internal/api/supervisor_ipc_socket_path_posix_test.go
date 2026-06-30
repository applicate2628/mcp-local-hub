//go:build !windows

package api

import (
	"strings"
	"testing"
)

// TestValidateSupervisorIPCSocketPathLen_WithinLimit confirms a normal-length
// state-dir-derived socket path passes.
func TestValidateSupervisorIPCSocketPathLen_WithinLimit(t *testing.T) {
	short := "/home/user/.local/state/mcp-local-hub/supervisor.sock"
	if err := ValidateSupervisorIPCSocketPathLen(short); err != nil {
		t.Fatalf("expected short path to pass, got: %v", err)
	}
}

// TestValidateSupervisorIPCSocketPathLen_AtLimit confirms a path exactly at
// the usable limit (buffer size minus the NUL terminator) passes.
func TestValidateSupervisorIPCSocketPathLen_AtLimit(t *testing.T) {
	usable := maxUnixSocketPathLen() - 1
	path := "/" + strings.Repeat("a", usable-1)
	if len(path) != usable {
		t.Fatalf("test setup: built path of length %d, want %d", len(path), usable)
	}
	if err := ValidateSupervisorIPCSocketPathLen(path); err != nil {
		t.Fatalf("expected exactly-at-limit path to pass, got: %v", err)
	}
}

// TestValidateSupervisorIPCSocketPathLen_OverLimit confirms a path one byte
// over the usable limit is refused with an actionable error naming both the
// limit and the observed length, instead of letting net.Listen fail
// cryptically.
func TestValidateSupervisorIPCSocketPathLen_OverLimit(t *testing.T) {
	usable := maxUnixSocketPathLen() - 1
	path := "/" + strings.Repeat("a", usable) // one byte over usable
	err := ValidateSupervisorIPCSocketPathLen(path)
	if err == nil {
		t.Fatalf("expected over-limit path to be refused")
	}
	msg := err.Error()
	for _, want := range []string{"too long", "limit", "shorten"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message %q missing expected substring %q", msg, want)
		}
	}
}

// TestMaxUnixSocketPathLen_PlatformValues pins the documented sun_path
// buffer sizes (Linux 108 / Darwin 104) so a future edit can't silently
// drift them without a test failure naming the mismatch.
func TestMaxUnixSocketPathLen_PlatformValues(t *testing.T) {
	got := maxUnixSocketPathLen()
	if got != 104 && got != 108 {
		t.Fatalf("maxUnixSocketPathLen() = %d, want 104 (darwin) or 108 (linux/other posix)", got)
	}
}
