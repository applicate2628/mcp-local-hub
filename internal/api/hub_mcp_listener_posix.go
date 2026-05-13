// hub_mcp_listener_posix.go — Phase 4 Task 4.1 (G4 unified hub MCP).
//
// POSIX-specific listener factory used by the hub bind sequence. No
// SO_EXCLUSIVEADDRUSE analogue exists on POSIX (Linux/macOS/BSD use
// SO_REUSEADDR + SO_REUSEPORT for different purposes, none of which
// match Windows' single-bind-exclusive semantic).
//
// Loopback bind to 127.0.0.1 is the available defense against
// external-network exposure. Per spec §"Bind ordering" (codex r7 P1
// reclassification): a pre-bind attack by a same-user local process
// has the SAME credential-exfiltration consequences as on Windows;
// the recovery workflow is identical (`mcphub gui --reset-port` +
// `mcphub hub-mcp regenerate-token` + `mcphub hub-mcp
// regenerate-instance-id` + reinstall).
//
// Spec: docs/superpowers/specs/2026-05-12-g4-unified-hub-mcp-design-v3.md
// §"Bind ordering" (step 6, POSIX branch). Plan: Task 4.1.

//go:build !windows

package api

import (
	"context"
	"net"
)

// NewListenerWithSOExclusive opens a TCP listener on addr with no
// extra socket options. The POSIX bind sequence relies on loopback
// binding (`127.0.0.1`) as the only mitigation against external
// exposure; a port collision returns the standard syscall error.
//
// Callers in internal/gui/hub_listener.go must treat a bind failure
// as a credential-rotation event (spec §"Pre-bind handling") because
// a same-user local process may have pre-bound the port to harvest
// the token a future client would have sent.
//
// Backwards-compat shim: delegates to NewListenerWithSOExclusiveContext
// with context.Background(). New code in the hub-mcp bind path uses
// the context-aware form so a hostile syscall hang at Listen no longer
// blocks holders of the hub-mcp.lock for the entire process lifetime
// (issue #159 concurrency lane #3).
//
// Spec §"Bind ordering" — POSIX branch.
func NewListenerWithSOExclusive(addr string) (net.Listener, error) {
	return NewListenerWithSOExclusiveContext(context.Background(), addr)
}

// NewListenerWithSOExclusiveContext is the cancellable form. ctx is
// passed to net.ListenConfig.Listen so callers that hold a flock can
// abort the bind on context cancellation instead of waiting for the
// kernel.
//
// Issue #159 concurrency lane #3 closure: the prior implementation
// used context.Background() unconditionally and was called while the
// hub-mcp.lock was held. A non-cancellable Listen hang would block
// every sibling flock waiter until process exit.
func NewListenerWithSOExclusiveContext(ctx context.Context, addr string) (net.Listener, error) {
	lc := net.ListenConfig{}
	return lc.Listen(ctx, "tcp", addr)
}
