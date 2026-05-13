// hub_mcp_bind.go — Phase 4 Task 4.4 (G4 unified hub MCP).
//
// BindHubMcpListener holds hub-mcp.lock across the full bind
// transaction (spec §"Bind ordering" steps 3-7) so two concurrent
// gui-server starts can't both bind separate OS-assigned ports and
// race to publish endpoint state. codex deep-sec P1 closure on
// PR #158.
//
// The pre-r2 implementation issued EnsureHubTokens (acquires +
// releases flock), LoadHubEndpoint (no flock), validateManifests
// (no flock), bind (no flock), EnsureHubEndpoint (acquires +
// releases flock). The lock-hold windows did NOT cover the bind
// itself, so on first start with port=0 two processes could both
// pass "load endpoint" with the same persisted port (= 0), call
// the listener factory in parallel (both succeed with distinct
// OS-assigned ports), and then race the endpoint persist — the
// loser overwrites the winner's port + pid, breaking the
// `endpoint.json describes the live listener` invariant.
//
// BindHubMcpListener acquires the lock once at step 2 (per spec)
// and holds it through step 7 (endpoint persist). The serving
// goroutine starts AFTER the lock is released (step 8 → step 9),
// matching the spec's "unlock-before-serve" rule.

package api

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
)

// HubMcpBindResult bundles the listener and the post-write endpoint
// record from a successful BindHubMcpListener call. The caller owns
// the listener and is responsible for serving + Close.
type HubMcpBindResult struct {
	Listener net.Listener
	Endpoint HubEndpoint
}

// BindHubMcpListener executes spec §"Bind ordering" steps 3-7 under
// a single hub-mcp.lock acquisition. Returns the bound listener and
// the post-write endpoint record.
//
//   - clients is the set of client names that get a per-client token
//     generated when not already present (step 3, EnsureHubTokens).
//     Pass the full supported set; existing tokens are preserved.
//   - validateManifests is the strict-mode pre-bind gate (step 5).
//     The hub design requires this to run INSIDE the lock so two
//     racing starts can't both pass validation against state that
//     would be mutated by the OTHER start's endpoint persist. Pass
//     nil to skip (e.g. for unit tests that don't have a manifest
//     surface).
//
// On bind failure (step 6) the spec requires emitting the
// credential-rotation warning via LogHubMcpEvent — done inside this
// helper.
//
// On step-7 (endpoint persist) failure the listener is closed
// before returning the error, per spec §"Bind ordering" rollback
// rule, so no traffic is accepted without a published endpoint.
//
// Concurrency invariant (codex deep-sec P1 closure on PR #158):
// two concurrent callers of BindHubMcpListener serialize on
// hub-mcp.lock. The second caller observes the first caller's
// persisted endpoint port and re-binds to that exact port — and
// the listener factory's SO_EXCLUSIVEADDRUSE on Windows ensures
// the second caller fails fast rather than silently sharing the
// port. POSIX equivalent: only one process binds at a time per
// (addr, port).
//
// ctx (codex bot phase4 r10 P1 closure on PR #158): the lock is
// acquired via acquireHubMcpLockContext so a sibling process holding
// hub-mcp.lock cannot freeze gui-server startup past the caller's
// shutdown budget. Callers from short CLI paths that genuinely want
// to wait can pass context.Background(); callers from gui-server
// startup (internal/gui/server.go) MUST pass the Server.Start ctx so
// ctx cancellation tears down the goroutine promptly.
func BindHubMcpListener(ctx context.Context, clients []string, validateManifests func() error) (*HubMcpBindResult, error) {
	// codex bot phase4 r19 P2 closure on PR #158: normalize a nil
	// ctx to context.Background() so the subsequent ctx.Err() checks
	// between bind steps (r13) don't nil-deref. acquireHubMcpLockContext
	// already handles nil via fallback to the blocking lock, but the
	// post-lock cancellation checks would panic on a nil interface.
	// Future CLI/test callers that pass nil get the legacy
	// blocking-lock semantics.
	if ctx == nil {
		ctx = context.Background()
	}
	lk, err := acquireHubMcpLockContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire hub-mcp.lock: %w", err)
	}
	// Lock is released either by the explicit Unlock below (success
	// path) or by the deferred Unlock (any error path). The `locked`
	// flag prevents a double-unlock.
	locked := true
	defer func() {
		if locked {
			_ = lk.Unlock()
		}
	}()

	// codex bot phase4 r13 P1 closure on PR #158: re-check ctx
	// between in-lock steps so a slow filesystem (manifest scan on
	// a stalled disk, hanging DACL stat on a network mount) cannot
	// keep the goroutine running past Server.Start's 2s post-cancel
	// budget and then publish a live listener after Start returned.
	// The acquireHubMcpLockContext call already gated flock
	// contention; these checks gate post-lock work too.
	if cerr := ctx.Err(); cerr != nil {
		return nil, fmt.Errorf("hub-mcp bind canceled before tokens: %w", cerr)
	}

	// Step 3 — tokens. Locked variant; caller already holds flock.
	if _, terr := ensureHubTokensLocked(clients); terr != nil {
		return nil, fmt.Errorf("ensure tokens: %w", terr)
	}

	if cerr := ctx.Err(); cerr != nil {
		return nil, fmt.Errorf("hub-mcp bind canceled before endpoint load: %w", cerr)
	}

	// Step 4 — load endpoint (port + instance_id). Missing file is
	// acceptable (first-start path); other errors surface (DACL /
	// parse / partial-write recovery).
	ep, lerr := loadHubEndpointLocked()
	if lerr != nil && !isMissingEndpointErr(lerr) {
		return nil, fmt.Errorf("load endpoint: %w", lerr)
	}

	if cerr := ctx.Err(); cerr != nil {
		return nil, fmt.Errorf("hub-mcp bind canceled before manifest validation: %w", cerr)
	}

	// Step 5 — strict-mode manifest pre-gate.
	if validateManifests != nil {
		if verr := validateManifests(); verr != nil {
			return nil, fmt.Errorf("bind refused: %w", verr)
		}
	}

	if cerr := ctx.Err(); cerr != nil {
		return nil, fmt.Errorf("hub-mcp bind canceled before listener bind: %w", cerr)
	}

	// Step 6 — bind. Pass port=0 if the persisted record had none
	// (first-start path).
	//
	// Issue #159 concurrency lane #3 closure: thread ctx into the
	// listener so a non-responsive syscall does not block sibling
	// flock waiters for the whole process lifetime. Caller's ctx
	// usually carries the gui-server's startup deadline; cancel
	// propagates here.
	bindAddr := fmt.Sprintf("127.0.0.1:%d", ep.Port)
	ln, lnErr := NewListenerWithSOExclusiveContext(ctx, bindAddr)
	if lnErr != nil {
		_ = LogHubMcpEvent("error", "hub-bind-failed", map[string]any{
			"port": ep.Port,
			"err":  lnErr.Error(),
		})
		// Spec §"Pre-bind handling": port-in-use on the persisted port
		// is indistinguishable from a credential-harvest attack. The
		// operator-facing recovery is the rotation chain documented
		// in §"Bind ordering". One log line; caller surfaces error.
		//
		// Bot r1 P2 closure (PR #167): with ctx now threaded into
		// Listen (issue #159 concurrency lane #3), lnErr can be
		// context.Canceled / DeadlineExceeded when startup is
		// canceled mid-listen. Those are NOT port-hijack signals —
		// emitting credential-rotation-required on cancellation
		// would drive operators through an unnecessary token+
		// instance-id rotation runbook. Skip the warning when the
		// error is ctx-shaped.
		if !errors.Is(lnErr, context.Canceled) && !errors.Is(lnErr, context.DeadlineExceeded) {
			_ = LogHubMcpEvent("warn", "credential-rotation-required", map[string]any{
				"reason": "pre-bind window — credentials may have leaked to pre-binding process",
			})
		}
		return nil, fmt.Errorf("bind %s: %w", bindAddr, lnErr)
	}

	// codex bot phase4 r13 P1 closure on PR #158: if ctx was canceled
	// AFTER step 6 succeeded but BEFORE we publish endpoint.json,
	// close the listener and bail. Otherwise the caller's 2s post-
	// cancel wait could expire while we're still in
	// ensureHubEndpointLocked, leaving a published listener + dead
	// goroutine after Server.Start returned.
	if cerr := ctx.Err(); cerr != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("hub-mcp bind canceled before endpoint persist: %w", cerr)
	}

	// Step 7 — persist endpoint.json with the OS-assigned port.
	port := ln.Addr().(*net.TCPAddr).Port
	persistedEp, perr := ensureHubEndpointLocked(port, os.Getpid())
	if perr != nil {
		// Step-7 failure with the listener already open: close the
		// listener so no traffic is accepted without a published
		// endpoint file (spec §"Bind ordering" rollback rule).
		_ = ln.Close()
		return nil, fmt.Errorf("write endpoint.json: %w", perr)
	}

	// codex bot phase4 r14 P1 closure on PR #158: post-persist ctx
	// check. ensureHubEndpointLocked is NOT ctx-aware
	// (writeHubMcpStateFile → SecureWriteClientConfig is a blocking
	// pipeline). If ctx was canceled DURING the write (a slow disk
	// keeping the goroutine alive past Server.Start's 2 s post-cancel
	// budget), close the listener and abort so we never return a
	// live listener after the shutdown path already gave up. The
	// on-disk endpoint.json is left mutated (the operator can
	// `mcphub gui --reset-port` to clear it).
	if cerr := ctx.Err(); cerr != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("hub-mcp bind canceled after endpoint persist: %w", cerr)
	}

	// Step 8 — release the lock BEFORE starting to serve so
	// concurrent /internal/reload-tokens (which acquires this same
	// lock to fsync the new tokens) doesn't block on us.
	//
	// Issue #159 (deep-sec leaks lane #5) closure: flip `locked =
	// false` AFTER Unlock succeeds. Previous order set `locked =
	// false` first, so if Unlock returned an error the deferred
	// Unlock (above) was short-circuited and the lock-file handle
	// persisted until GC. Now: try Unlock first; on success, clear
	// the flag; on failure, leave the defer armed so the lock is
	// released on function return regardless.
	if uerr := lk.Unlock(); uerr != nil {
		// Failed to release the lock cleanly: close the listener
		// so we don't serve in an undefined locking state. The
		// deferred Unlock stays armed (locked is still true) so
		// the function-return cleanup releases it on the second
		// attempt.
		_ = ln.Close()
		return nil, fmt.Errorf("release hub-mcp.lock: %w", uerr)
	}
	locked = false

	return &HubMcpBindResult{Listener: ln, Endpoint: persistedEp}, nil
}
