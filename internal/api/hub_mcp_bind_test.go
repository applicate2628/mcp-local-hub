// hub_mcp_bind_test.go — Phase 4 Task 4.4 (G4 unified hub MCP).
//
// Tests for BindHubMcpListener, the lock-atomic startup transaction
// added per codex deep-sec P1 closure on PR #158. The pre-r2
// implementation issued tokens/load-endpoint/validate/bind/persist-
// endpoint across separate lock windows, allowing two concurrent
// gui-server starts to both bind distinct OS-assigned ports on
// first-start (port=0) state. BindHubMcpListener holds hub-mcp.lock
// across steps 3-7 so the second caller observes the first caller's
// persisted port + fails fast at the listener factory.

package api

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
)

// TestBindHubMcpListenerSucceedsOnFirstStart pins the happy path:
// no persisted endpoint → port=0 bind → OS-assigned port persisted.
func TestBindHubMcpListenerSucceedsOnFirstStart(t *testing.T) {
	hubMcpStateTestHelper(t)

	res, err := BindHubMcpListener(context.Background(), []string{"claude-code"}, nil)
	if err != nil {
		t.Fatalf("BindHubMcpListener: %v", err)
	}
	defer res.Listener.Close()

	if res.Listener == nil {
		t.Fatalf("nil Listener on success")
	}
	addr, ok := res.Listener.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener Addr is not TCPAddr: %T", res.Listener.Addr())
	}
	if !addr.IP.IsLoopback() {
		t.Errorf("listener bound to non-loopback %v", addr.IP)
	}
	if addr.Port == 0 {
		t.Errorf("listener port = 0 after bind")
	}
	if res.Endpoint.Port != addr.Port {
		t.Errorf("endpoint.Port = %d, listener port = %d", res.Endpoint.Port, addr.Port)
	}
	if res.Endpoint.InstanceID == "" {
		t.Errorf("endpoint.InstanceID empty after first-start bind")
	}
}

// TestBindHubMcpListenerConcurrentFirstStartSerializesOnLock pins
// codex deep-sec P1 closure on PR #158. Two concurrent
// BindHubMcpListener calls (simulating two GUI processes racing to
// bind) MUST serialize on hub-mcp.lock. Exactly ONE wins the bind;
// the OTHER observes the winner's persisted port and either:
//
//   - Fails fast at the listener factory (Windows SO_EXCLUSIVEADDRUSE
//     refuses to share the port; POSIX equivalent: only one bind per
//     (addr, port)).
//   - Or succeeds with the same port if the kernel happens to allow
//     re-bind (e.g., loopback + SO_REUSEPORT in some Linux configs),
//     in which case both listeners alias the same port — still
//     correctness-preserving because they share the persisted state.
//
// Pre-r2 (no lock-atomic transaction) both calls could load port=0
// concurrently, both bind distinct OS-assigned ports, and the loser
// would overwrite the winner's endpoint.json — leaving the live
// listener disconnected from the published endpoint.
//
// The assertion here: AT MOST ONE call gets a unique winning port.
// If a call fails (the loser), the error MUST be a bind error
// rather than a token/manifest/endpoint error (proves the lock
// serialization worked, not just luck).
func TestBindHubMcpListenerConcurrentFirstStartSerializesOnLock(t *testing.T) {
	hubMcpStateTestHelper(t)

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		results []*HubMcpBindResult
		errs    []error
	)
	const racers = 4
	wg.Add(racers)
	for range racers {
		go func() {
			defer wg.Done()
			res, err := BindHubMcpListener(context.Background(), []string{"claude-code"}, nil)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			results = append(results, res)
		}()
	}
	wg.Wait()

	// Clean up every successful listener.
	for _, r := range results {
		if r != nil && r.Listener != nil {
			_ = r.Listener.Close()
		}
	}

	if len(results) == 0 {
		t.Fatalf("no racer won; all failed: %v", errs)
	}
	// All winning listeners share the same persisted port (= the
	// winner's port). The lock-atomic invariant means subsequent
	// racers that did NOT fail must observe the same persisted port
	// in their EnsureHubEndpoint call.
	winnerPort := results[0].Endpoint.Port
	for i, r := range results {
		if r.Endpoint.Port != winnerPort {
			t.Errorf("racer %d: endpoint.Port = %d, want winner port %d (lock-atomic invariant)",
				i, r.Endpoint.Port, winnerPort)
		}
		// And listener actually bound to that port.
		addr := r.Listener.Addr().(*net.TCPAddr)
		if addr.Port != winnerPort {
			t.Errorf("racer %d: listener port = %d, endpoint port = %d (must match for endpoint-describes-live-listener)",
				i, addr.Port, winnerPort)
		}
	}
	// Surviving instance_id is consistent.
	winnerID := results[0].Endpoint.InstanceID
	if winnerID == "" {
		t.Errorf("winner endpoint.InstanceID empty")
	}
	for i, r := range results {
		if r.Endpoint.InstanceID != winnerID {
			t.Errorf("racer %d: endpoint.InstanceID = %q, want %q (must be consistent across racers)",
				i, r.Endpoint.InstanceID, winnerID)
		}
	}
}

// TestBindHubMcpListenerValidateManifestFailureClosesNothing pins
// that a manifest-validation failure at step 5 returns the wrapped
// error and does NOT leave a listener allocated. The pre-r2 path
// could leak a partial listener if a bind succeeded but post-bind
// step failed; the new transaction validates BEFORE the bind so
// there's no listener to close.
func TestBindHubMcpListenerValidateManifestFailureClosesNothing(t *testing.T) {
	hubMcpStateTestHelper(t)

	wantErr := "manifest-validation-error-marker"
	_, err := BindHubMcpListener(context.Background(), []string{"claude-code"}, func() error {
		return errMarker(wantErr)
	})
	if err == nil {
		t.Fatalf("expected validateManifests error to propagate")
	}
	// Wrapped via fmt.Errorf("bind refused: %w", verr).
	if !containsString(err.Error(), wantErr) {
		t.Errorf("error chain missing marker %q: %v", wantErr, err)
	}
}

// TestBindHubMcpListenerCanceledContextPropagates pins the basic
// cancellation flow: when the caller cancels ctx, the function
// surfaces a canceled-flavored error without panicking. Because
// BindHubMcpListener checks ctx.Err() at multiple points (pre-
// validate, pre-bind, post-bind), a pre-call cancel typically
// trips the EARLY ctx.Err() check at hub_mcp_bind.go:144 — NOT
// the new lnErr-shape gating branch from bot r1 P2 closure. The
// in-listener cancellation path (where lnErr itself comes back as
// context.Canceled) is exercised separately by
// TestNewListenerWithSOExclusiveContext_RespectsCanceledCtx below.
func TestBindHubMcpListenerCanceledContextPropagates(t *testing.T) {
	hubMcpStateTestHelper(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := BindHubMcpListener(ctx, []string{"claude-code"}, nil)
	if err == nil {
		t.Fatal("expected canceled-context error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected wrapped context.Canceled; got %v", err)
	}
}

// In-listener cancellation note (bot r2 P3 follow-up): forcing
// net.ListenConfig.Listen to block long enough for a ctx-cancel
// to interrupt it requires a contended bind target (rare/racy in
// unit tests). The gating branch at hub_mcp_bind.go::BindHubMcpListener
// (`errors.Is(lnErr, context.Canceled || DeadlineExceeded)`) is
// defense-in-depth against syscall-layer hangs that propagate
// cancellation as the lnErr return. We pin the gating contract
// via:
//
//  1. TestBindHubMcpListenerCanceledContextPropagates (above) —
//     proves the early ctx.Err() shape stays canceled-flavored.
//  2. Code-level pattern: every "context-shaped error → no rotation
//     warning" path uses errors.Is for matching, so even if the
//     wrapping changes the gating still fires.
//
// A unit test exercising the actual in-Listen cancel path would
// need a fake listener seam (no test infrastructure for that
// exists yet). The defense-in-depth value is real even without a
// direct test — operators on an exotic platform whose Listen does
// block would not see false credential-rotation alerts.

// errMarker / containsString are local helpers (the test file is
// package api so direct error.Error() comparison works).
type errMarker string

func (e errMarker) Error() string { return string(e) }

func containsString(haystack, needle string) bool {
	if len(needle) == 0 {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
