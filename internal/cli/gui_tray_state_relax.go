// internal/cli/gui_tray_state_relax.go
//
// Parent-side helpers that bridge the tray's "Allow strict-DACL
// relax" menu item to the GUI server's /api/settings/state-read-relax
// endpoint. Kept in a separate file so the tray-config wiring in
// gui.go stays one-line per callback.
//
// The tray child has no HTTP client of its own — by design, since
// the child's only Win32 thread is the message pump. The parent
// (this GUI process) owns the HTTP transport AND the registry-write
// path. Lifecycle:
//
//   - Tray click → child emits {event:"toggle-state-relax"} on stdout.
//   - Parent dispatcher (tray.Run scanner loop) calls
//     cfg.ToggleStateReadRelax which fires postToggleStateRelax.
//   - postToggleStateRelax GETs the current value, POSTs the inverse,
//     then pushes the new value back into stateRelaxCh so the tray
//     child's MF_CHECKED glyph updates on the next menu render.
//
// pollStateReadRelaxForTray gives the child a fresh value at startup
// (initial paint) and re-polls every 30 seconds so an out-of-band
// change (operator sets the env var via PowerShell while the GUI is
// running) eventually reaches the menu without a click.

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// stateRelaxBody mirrors gui.stateRelaxSettingResponse on the wire.
// Duplicated here because internal/cli importing internal/gui would
// create a cycle (gui.go already imports many internal/cli symbols
// via test seams).
type stateRelaxBody struct {
	Enabled         bool `json:"enabled"`
	RestartRequired bool `json:"restart_required"`
}

// pollStateReadRelaxForTray pushes the current state into stateRelaxCh
// once at startup and then every pollEvery interval. Exits on ctx
// cancel. Errors from /api are silently swallowed (best-effort —
// tray menu remains usable even if the endpoint is briefly
// unreachable; the check glyph just stays at its prior value).
//
// Buffered channel send is non-blocking via select+default so a slow
// child consumer doesn't stall the poller.
func pollStateReadRelaxForTray(ctx context.Context, port int, ch chan<- bool) {
	const pollEvery = 30 * time.Second
	push := func() {
		val, err := getStateReadRelax(ctx, port)
		if err != nil {
			return
		}
		select {
		case ch <- val:
		default:
			// channel full; child will see the next push.
		}
	}
	push()
	t := time.NewTicker(pollEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			push()
		}
	}
}

// postToggleStateRelax flips the env-var on the GUI server. Reads
// the current value, POSTs the inverse, then pushes the new value
// into ch so the tray's MF_CHECKED glyph updates on the next render.
//
// Non-blocking channel send: if the tray child is mid-render and
// the channel buffer is full, the next 30 s poll will pick up the
// new value anyway.
func postToggleStateRelax(ctx context.Context, port int, ch chan<- bool) error {
	current, err := getStateReadRelax(ctx, port)
	if err != nil {
		return fmt.Errorf("get current: %w", err)
	}
	next := !current
	body := fmt.Sprintf(`{"enabled":%t}`, next)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("http://127.0.0.1:%d/api/settings/state-read-relax", port),
		strings.NewReader(body))
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	// requireSameOrigin accepts requests with no Origin header from
	// non-browser clients, but adding the loopback Origin makes the
	// request indistinguishable from a Settings-screen fetch on the
	// wire. Same precedent as the postBulk helper in gui.go.
	req.Header.Set("Origin", fmt.Sprintf("http://127.0.0.1:%d", port))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("POST: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var out stateRelaxBody
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4*1024)).Decode(&out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	select {
	case ch <- out.Enabled:
	default:
	}
	return nil
}

// getStateReadRelax returns the current HKCU value via the GUI's
// /api/settings/state-read-relax GET. On POSIX hosts the endpoint
// returns 501; we treat that as "feature not supported" → false
// without error so the toggle UX degrades gracefully (the menu item
// still appears but is always unchecked + a click is a no-op).
func getStateReadRelax(ctx context.Context, port int) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("http://127.0.0.1:%d/api/settings/state-read-relax", port), nil)
	if err != nil {
		return false, fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Origin", fmt.Sprintf("http://127.0.0.1:%d", port))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, fmt.Errorf("GET: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotImplemented {
		return false, nil
	}
	if resp.StatusCode >= 400 {
		return false, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var out stateRelaxBody
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4*1024)).Decode(&out); err != nil {
		return false, fmt.Errorf("decode: %w", err)
	}
	return out.Enabled, nil
}
