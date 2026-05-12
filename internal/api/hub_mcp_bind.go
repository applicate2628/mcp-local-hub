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
func BindHubMcpListener(clients []string, validateManifests func() error) (*HubMcpBindResult, error) {
	lk, err := acquireHubMcpLock()
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

	// Step 3 — tokens. Locked variant; caller already holds flock.
	if _, terr := ensureHubTokensLocked(clients); terr != nil {
		return nil, fmt.Errorf("ensure tokens: %w", terr)
	}

	// Step 4 — load endpoint (port + instance_id). Missing file is
	// acceptable (first-start path); other errors surface (DACL /
	// parse / partial-write recovery).
	ep, lerr := loadHubEndpointLocked()
	if lerr != nil && !isMissingEndpointErr(lerr) {
		return nil, fmt.Errorf("load endpoint: %w", lerr)
	}

	// Step 5 — strict-mode manifest pre-gate.
	if validateManifests != nil {
		if verr := validateManifests(); verr != nil {
			return nil, fmt.Errorf("bind refused: %w", verr)
		}
	}

	// Step 6 — bind. Pass port=0 if the persisted record had none
	// (first-start path).
	bindAddr := fmt.Sprintf("127.0.0.1:%d", ep.Port)
	ln, lnErr := NewListenerWithSOExclusive(bindAddr)
	if lnErr != nil {
		_ = LogHubMcpEvent("error", "hub-bind-failed", map[string]any{
			"port": ep.Port,
			"err":  lnErr.Error(),
		})
		// Spec §"Pre-bind handling": port-in-use on the persisted port
		// is indistinguishable from a credential-harvest attack. The
		// operator-facing recovery is the rotation chain documented
		// in §"Bind ordering". One log line; caller surfaces error.
		_ = LogHubMcpEvent("warn", "credential-rotation-required", map[string]any{
			"reason": "pre-bind window — credentials may have leaked to pre-binding process",
		})
		return nil, fmt.Errorf("bind %s: %w", bindAddr, lnErr)
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

	// Step 8 — release the lock BEFORE starting to serve so
	// concurrent /internal/reload-tokens (which acquires this same
	// lock to fsync the new tokens) doesn't block on us. The
	// deferred Unlock above is short-circuited by `locked = false`.
	locked = false
	if uerr := lk.Unlock(); uerr != nil {
		// Failed to release the lock cleanly: close the listener
		// so we don't serve in an undefined locking state.
		_ = ln.Close()
		return nil, fmt.Errorf("release hub-mcp.lock: %w", uerr)
	}

	return &HubMcpBindResult{Listener: ln, Endpoint: persistedEp}, nil
}
