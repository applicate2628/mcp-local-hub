// hub_mcp_control.go — Phase 4 Task 4.3 (G4 unified hub MCP).
//
// Internal control endpoint for the live-reload mechanism. Listens at
// POST /internal/reload-tokens on the SAME socket as the per-client
// /clients/{id}/mcp endpoints. Authenticated by a separate keyspace
// — `X-Mcphub-Control-Token: <64-hex>` — that rotates per hub start.
//
// Contract (spec §"Control endpoint contract"):
//
//   - HTTP method: POST only. Any other → 405 with `Allow: POST` +
//     empty body (RFC 9110 §15.5.6 compliance).
//   - Loopback-guard runs first; non-loopback Host / cross-site fetch
//     → 403.
//   - Per-client `X-Mcphub-Hub-Token` is IGNORED on this path: the
//     control token is a separate keyspace. A leaked client token
//     cannot reach this endpoint.
//   - Constant-time token compare via subtle.ConstantTimeCompare; the
//     64-hex shape gate precedes the compare to keep the timing path
//     identical between "wrong shape" and "wrong value".
//   - Body is ignored. Reload is global; no parameters.
//   - Rate-limit: a 2nd valid reload within 5s of the previous
//     SUCCESSFUL reload returns 429 with `Retry-After: 5`. Failed-auth
//     attempts do NOT count toward cooldown. Concurrent valid requests
//     serialize on reloadMutex; the 2nd inherits the 1st's outcome.
//
// Observability minimization: successful reloads emit one
// `event:"tokens-reloaded" source:"internal-reload"` log line with
// NO token bytes, NO instance ids, NO source PID. Failed reload
// attempts log only the reason category (unauth | method | loopback).
//
// Shutdown: removes <state-dir>/hub-mcp-control.token under flock so a
// crashed hub does not leave a stale token on disk (the next hub
// start re-generates and overwrites).
//
// Spec: docs/superpowers/specs/2026-05-12-g4-unified-hub-mcp-design-v3.md
// §"Control endpoint contract `/internal/reload-tokens`".
// Plan: docs/superpowers/plans/2026-05-12-g4-unified-hub-mcp.md Task 4.3.

package api

import (
	"context"
	"crypto/subtle"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// internalReloadCooldown is the minimum gap between two SUCCESSFUL
// token reloads. Failed-auth attempts (401/403/405) do not count.
// Concurrent requests serialize on reloadMutex; the 2nd inherits the
// 1st's outcome (204 if window opened, 429 otherwise).
const internalReloadCooldown = 5 * time.Second

// InternalReloadHandler implements POST /internal/reload-tokens. Wired
// onto the hub-mcp listener mux in internal/gui/hub_listener.go.
type InternalReloadHandler struct {
	// reloadMutex serializes the cooldown check + the ReloadHubTokens
	// call. Spec calls this "reloadMutex"; the name is preserved.
	reloadMutex sync.Mutex
	// lastReload records the time of the most recent SUCCESSFUL reload.
	// A zero value means "no reload has happened yet" — the first valid
	// reload always succeeds regardless of the cooldown.
	lastReload time.Time
	// controlTok is the freshly-generated per-hub-start control token.
	// atomic.Pointer so Shutdown can clear it without racing the
	// request handler's reads.
	controlTok atomic.Pointer[string]
}

// NewInternalReloadHandler generates a fresh control token via
// crypto/rand (64-lower-hex) and persists it to
// <state-dir>/hub-mcp-control.token under flock. Returns a handler
// ready to mount on the hub-mcp mux.
//
// The control token is intentionally NEVER written to a log file,
// echoed via `mcphub hub-mcp status`, or copied into client configs
// — it is consumed only by the rotation CLI which reads the file
// directly under the same flock the hub used to write it.
//
// codex bot phase4 r5 P2 closure on PR #158: persistence failure is
// surfaced as a returned error (rather than logged + ignored). An
// empty hub-mcp-control.token on disk means no external caller can
// discover the in-memory token, so /internal/reload-tokens is
// effectively unusable for the lifetime of the process. Listener
// startup callers (internal/gui/hub_listener.go) propagate the
// error so the hub listener refuses to come up rather than coming
// up with a silently-broken reload control plane. Operators can fix
// the state-dir DACL / disk-full / antivirus interference, then
// restart.
//
// codex bot phase4 r11 P1 closure on PR #158: ctx threads through to
// writeHubMcpControlTokenLockedContext so the flock acquisition
// honors caller cancellation. Without this, gui-server startup
// (internal/gui/server.go) could see ctx.Done() fire, give up after
// the 2 s hubInitDone wait with hubMcpComp == nil, AND the goroutine
// would later resume from a blocking acquireHubMcpLock() and bring
// up the hub listener after Start has already returned — exactly the
// race the r10 cycle was supposed to close. Test sites that don't
// need cancellation can pass context.Background().
func NewInternalReloadHandler(ctx context.Context) (*InternalReloadHandler, error) {
	h := &InternalReloadHandler{}
	tok, err := generateHexToken()
	if err != nil {
		_ = LogHubMcpEvent("error", "control-token-generate-failed", map[string]any{
			"err": err.Error(),
		})
		return nil, fmt.Errorf("generate hub-mcp control token: %w", err)
	}
	h.controlTok.Store(&tok)
	// Persist under flock so the CLI reading the file does not see a
	// torn write across an atomic-rename. writeHubMcpStateFile uses
	// SecureWriteClientConfig (handle-relative + DACL-set-before-write
	// + atomic rename), so concurrent reads see either the old token
	// or the new token, never a mix.
	if werr := writeHubMcpControlTokenLockedContext(ctx, tok); werr != nil {
		_ = LogHubMcpEvent("error", "control-token-persist-failed", map[string]any{
			"err": werr.Error(),
		})
		// codex deep-sec phase4 r24 P2 closure on PR #158 (lane #3):
		// SecureWriteClientConfig can fail AFTER the rename has
		// committed (e.g. post-rename DACL verification fails).
		// In that case hub-mcp-control.token contains the
		// generated token but the constructor returns error, so
		// no handler is wired to that token — the file is a
		// dangling sensitive artifact. Best-effort remove under
		// flock; if the remove also fails (state-dir DACL pinned
		// it read-only), log it — the operator can clean up by
		// running the rotation CLI which rewrites the file under
		// the same flock.
		if rerr := removeHubMcpControlTokenLockedContext(ctx); rerr != nil {
			_ = LogHubMcpEvent("warn", "control-token-cleanup-on-persist-fail", map[string]any{
				"err": rerr.Error(),
			})
		}
		// Clear the in-memory copy too — no longer authoritative.
		var empty string
		h.controlTok.Store(&empty)
		return nil, fmt.Errorf("persist hub-mcp control token: %w", werr)
	}
	return h, nil
}

// removeHubMcpControlTokenLockedContext is the cleanup half used by
// NewInternalReloadHandler when persistence fails after rename.
// Idempotent — missing file returns nil. Errors that aren't
// "file not found" surface to the caller for logging.
func removeHubMcpControlTokenLockedContext(ctx context.Context) error {
	lk, err := acquireHubMcpLockContext(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = lk.Unlock() }()
	dir, err := DaemonStateDir()
	if err != nil {
		return err
	}
	path := filepath.Join(dir, hubMcpControlTokenFileLeaf)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// ReadHubMcpControlToken loads the persisted control token from
// <state-dir>/hub-mcp-control.token. Used by the rotation CLI
// (mcphub hub-mcp regenerate-token / regenerate-instance-id) to POST
// /internal/reload-tokens so the live hub picks up the new tokens
// without restart.
//
// Returns the trimmed token (no trailing newline) on success.
// Returns os.ErrNotExist if the hub has not run yet (or
// SecureWriteClientConfig pre-write verify failed at constructor
// time — r24 P2 closure: the file is cleaned up on persist failure).
//
// The read goes through readHubMcpStateFile which gates on
// VerifyHubMcpStateDACL first — refuses symlinks, foreign owners,
// non-allowlist DACL ACEs. Same secure-read pipeline the listener
// uses on startup.
func ReadHubMcpControlToken() (string, error) {
	raw, err := readHubMcpStateFile(hubMcpControlTokenFileLeaf)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(raw)), nil
}

// ServeHTTP implements the POST-only contract with loopback-guard,
// constant-time token compare, rate-limit, and the ReloadHubTokens
// + log-event pipeline. Every failure path emits exactly one
// LogHubMcpEvent log line.
func (h *InternalReloadHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Loopback-guard first. The same Host / Origin / Sec-Fetch-Site
	// rules that protect /clients/{id}/mcp apply here.
	if !isSafeLoopbackHubRequest(r) {
		_ = LogHubMcpEvent("warn", "internal-reload-rejected", map[string]any{
			"reason": "loopback",
		})
		http.Error(w, "forbidden loopback request", http.StatusForbidden)
		return
	}
	// codex bot phase5 r9 P2 closure on PR #160: HEAD is the
	// non-mutating liveness-probe variant. Goes through the same
	// loopback + auth gate, returns 204 on auth pass without
	// touching reloadMutex / lastReload / ReloadHubTokens. POST
	// keeps its existing mutate-and-204 semantics. The probe
	// caller (hubProbeAlive in internal/cli/hubmcp.go) uses HEAD
	// so that running `mcphub hub-mcp regenerate-instance-id` —
	// which probes liveness BEFORE refusing on a live hub — does
	// not consume the 5s reload cooldown that a subsequent
	// `regenerate-token` immediately needs.
	if r.Method != http.MethodPost && r.Method != http.MethodHead {
		// RFC 9110 §15.5.6 — Allow header on 405 enumerates supported
		// methods. Body empty.
		w.Header().Set("Allow", "HEAD, POST")
		w.WriteHeader(http.StatusMethodNotAllowed)
		_ = LogHubMcpEvent("warn", "internal-reload-rejected", map[string]any{
			"reason": "method",
		})
		return
	}

	tok := r.Header.Get("X-Mcphub-Control-Token")
	// Per-client X-Mcphub-Hub-Token is IGNORED on this path — separate
	// keyspace. A request that ONLY carries X-Mcphub-Hub-Token falls
	// through the shape gate below (control token empty) → 401.
	if !isLowerHex64(tok) {
		w.WriteHeader(http.StatusUnauthorized)
		_ = LogHubMcpEvent("warn", "internal-reload-rejected", map[string]any{
			"reason": "unauth",
		})
		return
	}
	storedPtr := h.controlTok.Load()
	if storedPtr == nil || subtle.ConstantTimeCompare([]byte(tok), []byte(*storedPtr)) != 1 {
		w.WriteHeader(http.StatusUnauthorized)
		_ = LogHubMcpEvent("warn", "internal-reload-rejected", map[string]any{
			"reason": "unauth",
		})
		return
	}

	// HEAD = non-mutating liveness probe. Auth passed → return 204
	// immediately. No reloadMutex acquisition, no cooldown bump, no
	// ReloadHubTokens call. The successful HEAD response is the
	// hub-specific signal the probe needs (a stranger HTTP service
	// holding the port would either 405 the unknown route or 401
	// against an arbitrary control token).
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// POST → proceed with cooldown check + reload (existing path).
	// Serialize the cooldown check + reload under
	// reloadMutex so two concurrent valid requests cannot both swap
	// the token table (spec §"Control endpoint contract" — codex r5
	// MED: minimum 5s between consecutive successful reloads enforced
	// via a single timestamp guarded by reloadMutex).
	h.reloadMutex.Lock()
	defer h.reloadMutex.Unlock()

	if !h.lastReload.IsZero() && time.Since(h.lastReload) < internalReloadCooldown {
		w.Header().Set("Retry-After", "5")
		w.WriteHeader(http.StatusTooManyRequests)
		// Cooldown rejections are observable but not logged as
		// security events — they are an expected operator-side
		// outcome of double-clicking the rotation CLI.
		return
	}

	if _, err := ReloadHubTokens(); err != nil {
		_ = LogHubMcpEvent("error", "internal-reload-failed", map[string]any{
			"err": err.Error(),
		})
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	h.lastReload = time.Now()
	// Successful reload: ONE log line, no token bytes / no instance id
	// / no source PID. The fields are constants — RedactToken in
	// LogHubMcpEvent has nothing to scrub but the choke-point still
	// applies as defense-in-depth.
	_ = LogHubMcpEvent("info", "tokens-reloaded", map[string]any{
		"source": "internal-reload",
	})
	w.WriteHeader(http.StatusNoContent)
}

// Shutdown removes <state-dir>/hub-mcp-control.token under flock so
// the next hub start regenerates rather than reusing a stale on-disk
// token. Idempotent — a missing file returns nil. Errors that aren't
// "file not found" surface to the caller for the gui shutdown path
// to log.
//
// Caller (internal/gui/hub_listener.go) invokes Shutdown from the
// hub-listener teardown branch when ctx is canceled. The on-process
// control token in h.controlTok also dies with the process; even if
// the file removal fails, the in-memory token is gone.
//
// codex bot phase4 r10 P1 closure on PR #158: lock acquisition is now
// bounded by ctx (acquireHubMcpLockContext). A sibling process
// holding hub-mcp.lock cannot freeze gui-server teardown past the
// caller's 5s budget — when ctx times out, the lock acquisition
// returns context.DeadlineExceeded and the in-memory controlTok has
// already been cleared. The on-disk hub-mcp-control.token may
// remain, but the next hub start regenerates it under flock so a
// stale file is harmless beyond the rotation-warning telemetry
// surface. Pass context.Background() if cancellation is not needed
// (test sites).
func (h *InternalReloadHandler) Shutdown(ctx context.Context) error {
	// Drop the in-memory reference first so any racing late ServeHTTP
	// gets a 401 (controlTok.Load() returns nil → mismatch). Done
	// BEFORE the lock attempt so the in-memory effect is unconditional
	// — even if the flock acquisition times out, the live process
	// stops accepting reloads with the old token.
	var empty string
	h.controlTok.Store(&empty)

	lk, err := acquireHubMcpLockContext(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = lk.Unlock() }()

	dir, err := DaemonStateDir()
	if err != nil {
		return err
	}
	path := filepath.Join(dir, hubMcpControlTokenFileLeaf)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// writeHubMcpControlTokenLockedContext writes the control token under
// flock via writeHubMcpStateFile (the secure DACL/atomic-rename
// pipeline). The flock is acquired via acquireHubMcpLockContext so
// caller cancellation propagates — without this, NewInternalReloadHandler
// could hang past gui-server's shutdown budget if a sibling CLI is
// holding hub-mcp.lock.
//
// codex bot phase4 r11 P1 closure on PR #158: the pre-r11 variant
// (writeHubMcpControlTokenLocked) used blocking acquireHubMcpLock.
// The startup race that closed in r10 had a symmetric hole here —
// fixed by routing through acquireHubMcpLockContext so the same
// 10 ms cancellation cadence applies. Use context.Background() at
// test sites that don't need cancellation.
//
// We split this out of NewInternalReloadHandler so the in-flock half
// is small enough to audit; the constructor takes the flock once and
// the file write is a single delegated call.
func writeHubMcpControlTokenLockedContext(ctx context.Context, tok string) error {
	lk, err := acquireHubMcpLockContext(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = lk.Unlock() }()
	// Trailing newline matches the spec wording ("64-hex value +
	// newline"). The CLI reader trims whitespace.
	return writeHubMcpStateFile(hubMcpControlTokenFileLeaf, []byte(tok+"\n"))
}
