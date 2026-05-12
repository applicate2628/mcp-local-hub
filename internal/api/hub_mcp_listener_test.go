// hub_mcp_listener_test.go — Phase 4 Task 4.1 (G4 unified hub MCP).
//
// Verifies the platform-specific listener factory used by the hub
// bind sequence. On Windows the factory MUST set SO_EXCLUSIVEADDRUSE
// so a second bind to the same port fails synchronously (pre-bind
// attack defense — spec §"Bind ordering"). On POSIX no such option
// exists; the test guards that the listener still binds to 127.0.0.1
// and that the second-bind test is skipped instead of failing.
package api

import (
	"fmt"
	"net"
	"runtime"
	"testing"
)

// TestNewListenerWithSOExclusiveBinds asserts the factory returns a
// listener bound to a 127.0.0.1 address. Runs on every platform — the
// loopback-bind requirement is universal (Windows + POSIX).
func TestNewListenerWithSOExclusiveBinds(t *testing.T) {
	ln, err := NewListenerWithSOExclusive("127.0.0.1:0")
	if err != nil {
		t.Fatalf("NewListenerWithSOExclusive: %v", err)
	}
	defer ln.Close()
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener addr not *net.TCPAddr: %T", ln.Addr())
	}
	if !addr.IP.IsLoopback() {
		t.Errorf("listener not on loopback: %v", addr)
	}
	if addr.Port == 0 {
		t.Errorf("OS did not assign a port: %v", addr)
	}
}

// TestNewListenerWithSOExclusiveRejectsSecondBindWindows asserts that
// SO_EXCLUSIVEADDRUSE was actually set on Windows: a second bind to
// the same port MUST fail synchronously. POSIX has no equivalent —
// the test skips there (loopback-bind alone is the available defense).
//
// Spec §"Bind ordering" — codex r4 F4 closure: SetsockoptInt error
// must surface, not silently fall back to SO_REUSEADDR semantics.
func TestNewListenerWithSOExclusiveRejectsSecondBindWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("SO_EXCLUSIVEADDRUSE is windows-only")
	}
	ln1, err := NewListenerWithSOExclusive("127.0.0.1:0")
	if err != nil {
		t.Fatalf("first bind: %v", err)
	}
	defer ln1.Close()
	port := ln1.Addr().(*net.TCPAddr).Port
	ln2, err := NewListenerWithSOExclusive(fmt.Sprintf("127.0.0.1:%d", port))
	if err == nil {
		ln2.Close()
		t.Errorf("second bind to port %d must fail with SO_EXCLUSIVEADDRUSE; got success", port)
	}
}

// TestNewListenerWithSOExclusiveRejectsInvalidAddr asserts that
// upstream net errors (bad address, port-out-of-range) flow through
// the factory without being swallowed. The factory wraps
// SetsockoptInt failures via the Control hook — invalid address strings
// surface from lc.Listen before Control ever runs, so the error type
// is whatever net.ListenConfig.Listen returns. The key property is:
// non-nil error AND nil listener AND no panic.
func TestNewListenerWithSOExclusiveRejectsInvalidAddr(t *testing.T) {
	ln, err := NewListenerWithSOExclusive("not-a-real-address:0")
	if err == nil {
		ln.Close()
		t.Fatalf("expected error from bogus address; got listener bound to %v", ln.Addr())
	}
}
