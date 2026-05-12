// hub_mcp_listener_windows.go — Phase 4 Task 4.1 (G4 unified hub MCP).
//
// Windows-specific listener factory used by the hub bind sequence in
// internal/gui/hub_listener.go. The factory sets SO_EXCLUSIVEADDRUSE
// on the underlying socket so a second bind to the same port fails
// synchronously instead of racing — closing the pre-bind credential-
// exfil window described in spec §"Bind ordering".
//
// Local constant `soExclusiveAddrUse`: ws2def.h defines
// SO_EXCLUSIVEADDRUSE as `((u_int)(~SO_REUSEADDR))`. x/sys/windows does
// not export this constant (verified against x/sys@v0.26.0
// types_windows.go which only declares SO_REUSEADDR = 4). We define
// the constant inline using the bitwise-NOT form so intent reads at
// a glance AND stays robust if x/sys ever updates SO_REUSEADDR.
//
// Setsockopt error capture: the Control hook MUST surface a non-nil
// `setErr` to the caller (codex r4 F4 closure). Silently dropping the
// error means the listener binds with default SO_REUSEADDR semantics,
// defeating the entire pre-bind safeguard.
//
// Spec: docs/superpowers/specs/2026-05-12-g4-unified-hub-mcp-design-v3.md
// §"Bind ordering" (steps 6-7 — listener factory). Plan: Task 4.1.

//go:build windows

package api

import (
	"context"
	"net"
	"syscall"

	"golang.org/x/sys/windows"
)

// soExclusiveAddrUse is the Windows-specific SO_EXCLUSIVEADDRUSE
// socket option. Defined locally because x/sys/windows does not
// export it. Value matches ws2def.h's `((u_int)(~SO_REUSEADDR))`
// (= -5 on Windows where SO_REUSEADDR == 4).
//
// Bitwise-NOT form (codex r4 F4 closure): documents intent inline +
// auto-tracks any future x/sys revision of SO_REUSEADDR.
const soExclusiveAddrUse = ^windows.SO_REUSEADDR

// NewListenerWithSOExclusive opens a TCP listener on addr with
// SO_EXCLUSIVEADDRUSE set on the underlying socket. The returned
// listener refuses any second-bind attempt against the same (addr,
// port) tuple — even after the original listener closes, the
// kernel's TIME_WAIT entry remains exclusive to this listener's
// lifetime.
//
// Errors:
//   - Control-hook error (SetsockoptInt failed): returned to the
//     caller via the `setErr` capture. The listener is not opened.
//   - lc.Listen error (port in use, address resolution failure,
//     etc.): returned verbatim.
//
// Spec §"Bind ordering" — this is the bind-step listener factory.
// Caller in internal/gui/hub_listener.go must defer Close() on the
// returned listener; the bind-failure log line is the caller's
// responsibility (this layer only surfaces the error).
func NewListenerWithSOExclusive(addr string) (net.Listener, error) {
	lc := net.ListenConfig{
		Control: func(_, _ string, c syscall.RawConn) error {
			var setErr error
			ctlErr := c.Control(func(fd uintptr) {
				setErr = windows.SetsockoptInt(
					windows.Handle(fd),
					windows.SOL_SOCKET,
					soExclusiveAddrUse,
					1,
				)
			})
			if ctlErr != nil {
				return ctlErr
			}
			return setErr // F4: surface SetsockoptInt error to caller.
		},
	}
	return lc.Listen(context.Background(), "tcp", addr)
}
