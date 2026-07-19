// internal/cli/gui_cleanup_ticker.go
//
// Background ticker that periodically POSTs to the GUI's
// /api/cleanup/orphans endpoint to reap orphan mcp-language-server
// processes accumulated from un-migrated agent direct-stdio.
//
// Design notes:
//
//   - The ticker runs in its own goroutine, owned by the parent gui
//     process. It does NOT live inside the tray goroutine because the
//     tray is opt-out via --no-tray, but auto-cleanup should fire even
//     when the tray is suppressed.
//   - The ticker dials the GUI's loopback HTTP surface (same pattern
//     as postBulk and postToggleStateRelax) so the request goes through
//     the same auth/CSRF/SSE pipeline as a manual Dashboard click. This
//     guarantees a single code path: any future change to
//     /api/cleanup/orphans applies to the auto-ticker too.
//   - Opt-out via env var MCPHUB_DISABLE_AUTO_CLEANUP. The env var is
//     consulted PER TICK so an operator can disable the ticker live
//     (set the env var, no restart needed) without killing the GUI.
//   - Tick cadence: 5 minutes. Tunable only via the package constant;
//     not a user-facing setting (operators who want different cadence
//     run cleanup from a scheduled task or the Dashboard button).
//   - Observability: each tick that kills/skips at least one process
//     emits an auto-cleanup-tick event to hub-mcp.log at info level.
//     Quiet ticks (killed=0, skipped=0) downgrade to debug to avoid
//     polluting the log with no-op records. Failed POSTs emit warn.
//
// The handler at internal/gui/cleanup.go already enforces:
//   - method POST,
//   - JSON body parse,
//   - same-origin (Origin: http://127.0.0.1:<port>),
//   - Windows-only (POSIX returns 501 not_supported_on_this_os — the
//     ticker handles that gracefully as a no-op).
//
// So this file is purely the parent-side timer + HTTP client glue.

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"mcp-local-hub/internal/api"
)

// autoCleanupInterval is the cadence between cleanup ticks. 5 min is
// a compromise: short enough that orphan accumulation stays bounded
// (a heavy agent session that spawns 4-5 orphan mcp-language-server
// per minute caps at ~20-25 between ticks), long enough that the
// cleanup overhead is invisible on a quiet machine.
const autoCleanupInterval = 5 * time.Minute

// autoCleanupOptOutEnv is the env var operators set to disable the
// background ticker. Recognized values: "1", "true" (case-insensitive).
// Any other value (including empty/unset) leaves the ticker enabled.
const autoCleanupOptOutEnv = "MCPHUB_DISABLE_AUTO_CLEANUP"

// autoCleanupTickerResponse mirrors the success-path JSON shape of
// /api/cleanup/orphans (cleanupResponse in internal/gui/cleanup.go).
// Duplicated here because internal/cli importing internal/gui would
// create a cycle.
type autoCleanupTickerResponse struct {
	Killed  int `json:"killed"`
	Skipped int `json:"skipped"`
}

// runAutoCleanupTicker fires a tick every autoCleanupInterval until
// ctx is cancelled. Each tick POSTs to the GUI's own
// /api/cleanup/orphans with {"apply": true, "scan_clients": true}
// (scan_clients opts the automatic sweep into the client-config scan
// so config-absent dead-parent orphans of client-direct MCP servers
// are reaped too — Rank-2), then emits an observability event. The
// ticker does NOT fire an immediate startup
// tick — the first tick fires after the interval elapses, so a fresh
// GUI launch doesn't compete with agent-side daemon spawns during the
// first 5 min.
//
// port is the loopback port the GUI server is listening on. The
// caller (gui.go) reads it from s.Port() after server startup.
//
// All write paths are best-effort: a failed POST emits a warn-level
// event to hub-mcp.log but does NOT halt the ticker. The next tick
// retries.
func runAutoCleanupTicker(ctx context.Context, port int) {
	t := time.NewTicker(autoCleanupInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if autoCleanupOptedOut() {
				continue
			}
			fireAutoCleanupTick(ctx, port)
		}
	}
}

// autoCleanupOptedOut reads MCPHUB_DISABLE_AUTO_CLEANUP and returns
// true when the operator has disabled the ticker. Recognized values
// are "1" and "true" (case-insensitive after TrimSpace). Reading the
// env var per-tick (instead of once at start) lets an operator flip
// the toggle without restarting the GUI.
func autoCleanupOptedOut() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(autoCleanupOptOutEnv)))
	return v == "1" || v == "true"
}

// fireAutoCleanupTick POSTs a single cleanup request and emits one
// observability event with the outcome. Split out from the ticker
// loop so tests can drive it directly without waiting on the 5 min
// interval.
//
// Per-call request context with a 30 s timeout so a single hung
// backend response can't stall future ticks — the next ticker fire
// creates a fresh request + deadline.
func fireAutoCleanupTick(ctx context.Context, port int) {
	reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// scan_clients:true opts the automatic sweep INTO the client-config
	// scan (A6) so a config-ABSENT dead-parent orphan of a client-direct
	// MCP server is reaped, not just manifest-nominated orphans (Rank-2,
	// bug 2026-07-19). The shipped config-absence gate + identity-verified
	// kill + 600s age floor still apply, so a currently-configured server
	// is nominated then spared. The ticker never sets server, so the
	// handler's scan_clients+server 400 pre-validate never trips.
	body := `{"apply":true,"scan_clients":true}`
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost,
		fmt.Sprintf("http://127.0.0.1:%d/api/cleanup/orphans", port),
		strings.NewReader(body))
	if err != nil {
		// NewRequestWithContext only errors on a malformed URL or
		// unsupported method — both are programmer errors that would
		// have fired on first startup. Log warn and continue.
		_ = api.LogHubMcpEvent("warn", "auto-cleanup-tick",
			map[string]any{"error": "new request: " + err.Error()})
		return
	}
	req.Header.Set("Content-Type", "application/json")
	// requireSameOrigin in internal/gui accepts requests with no
	// Origin header from non-browser clients, but adding the loopback
	// Origin makes the request indistinguishable from a Maintenance-
	// screen click on the wire. Same precedent as postBulk +
	// postToggleStateRelax.
	req.Header.Set("Origin", fmt.Sprintf("http://127.0.0.1:%d", port))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		_ = api.LogHubMcpEvent("warn", "auto-cleanup-tick",
			map[string]any{"error": "POST: " + err.Error()})
		return
	}
	defer resp.Body.Close()

	// 501 not_supported_on_this_os fires on POSIX hosts where the
	// underlying CleanupOrphans backend is unimplemented. Treat as a
	// no-op (debug-only) rather than warn — operators on POSIX
	// shouldn't see a warning every 5 min about a feature that's not
	// on their roadmap yet.
	if resp.StatusCode == http.StatusNotImplemented {
		_ = api.LogHubMcpEvent("debug", "auto-cleanup-tick",
			map[string]any{"skipped_reason": "not_supported_on_this_os"})
		return
	}
	if resp.StatusCode >= 400 {
		_ = api.LogHubMcpEvent("warn", "auto-cleanup-tick",
			map[string]any{"error": fmt.Sprintf("HTTP %d", resp.StatusCode)})
		return
	}

	var out autoCleanupTickerResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&out); err != nil {
		_ = api.LogHubMcpEvent("warn", "auto-cleanup-tick",
			map[string]any{"error": "decode response: " + err.Error()})
		return
	}

	// Downgrade quiet ticks to debug so an idle machine doesn't
	// accrue an info-level record every 5 min. Active ticks (any
	// kill or skip) emit at info so operators can audit the
	// auto-cleanup activity in mcphub hub-mcp status.
	level := "info"
	if out.Killed == 0 && out.Skipped == 0 {
		level = "debug"
	}
	_ = api.LogHubMcpEvent(level, "auto-cleanup-tick", map[string]any{
		"killed":  out.Killed,
		"skipped": out.Skipped,
	})
}