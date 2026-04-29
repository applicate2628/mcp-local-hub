// internal/cli/gui_tray_state.go
package cli

import (
	"context"
	"fmt"
	"strings"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/tray"
)

// rowKey produces the (server, daemon) tuple used for per-daemon
// failure-onset diff between adjacent snapshots. Empty Daemon
// falls back to "default" to match the keyFor convention used by
// StatusPoller.
func rowKey(r api.DaemonStatus) string {
	d := r.Daemon
	if d == "" {
		d = "default"
	}
	return r.Server + "/" + d
}

// Task Scheduler informational LastResult codes — NOT real failures.
// Documented in MS-TSCH and observed in production:
//
//	0x41300 (267008) — task is ready to run
//	0x41301 (267009) — task is currently running
//	0x41303 (267011) — task has not yet run (initial state)
//	0x41304 (267012) — no more runs scheduled
//	0x41306 (267014) — task is disabled
//	0x41307 (267015) — task has not yet run for the user
//
// User-defined exit codes are typically small positive (1, 2, 87,
// 1063) or HRESULT-shaped (high bit set → negative when read as
// int32). Anything in the 0x41300-0x4130F informational range
// must be excluded from the failure predicate or the tray spams
// "daemon failed" toasts every poll for every never-run /
// idle-state task on the host (regression observed in PR #22
// initial push, fixed before merge).
const (
	tsInfoCodeMin = 0x41300
	tsInfoCodeMax = 0x4130F
)

// isFailedRow returns true when a daemon row reports a real failure.
// Mirrors the StateError predicates in tray.Aggregate so toast onset
// matches tray icon onset — the user sees the icon turn red and the
// toast pop at the same transition.
//
// LastResult is treated as a real failure only when it is non-zero,
// not -1 (internal/scheduler/scheduler.go:53 sentinel for "task has
// never run" — Codex PR #22 r2 P2: without this filter the very
// first snapshot post-install would fire "daemon failed" toasts for
// every never-run task, exactly the spam the spam-toast fix set out
// to prevent), and not in the Task Scheduler 2.0 informational range
// (0x41300-0x4130F). State-string "fail" match is kept as a separate
// signal because some daemon paths emit Failed without a matching
// LastResult update.
func isFailedRow(r api.DaemonStatus) bool {
	if r.LastResult != 0 && r.LastResult != -1 &&
		(r.LastResult < tsInfoCodeMin || r.LastResult > tsInfoCodeMax) {
		return true
	}
	return strings.Contains(strings.ToLower(r.State), "fail")
}

// toastFn is the indirection point for testing. tray.ShowToast in
// production; a fake recorder in tests.
type toastFn func(title, body string) error

// aggregateTrayState bridges StatusPoller's snapshot channel and the
// tray icon's state channel. For each snapshot it recomputes the
// aggregate TrayState (tray.Aggregate) and forwards onto trayCh ONLY
// when the aggregate changed since the last forward. The check
// avoids spurious SetIcon calls when individual daemons flap but
// the overall state is steady — Windows redraws on every SetIcon,
// however small, and the icon flickering would be user-visible.
//
// Initial value is a sentinel (-1) so the very first snapshot
// always forwards once: even if the daemon state is the default
// "everything healthy", the tray's onReady already painted Healthy
// so the no-op forward is harmless. The forward acts as a
// confirmation that the tray and the poller are in agreement.
//
// Returns when ctx is canceled OR the snapshot channel is closed.
// Non-blocking forward via buffered trayCh + select-default so a
// stalled tray cannot block the snapshot pump.
//
// C4: also detects daemon failure ONSETS by diffing each snapshot
// against the prior one (per (server, daemon) key). A row is a
// failure-onset when it is failed in this snapshot but was not
// failed (or absent) in the prior one. Each onset fires one toast
// via the injected toast function. Fired in a goroutine so the
// PowerShell launch doesn't stall the aggregator pump.
func aggregateTrayState(ctx context.Context, snapshots <-chan []api.DaemonStatus, trayCh chan<- tray.TrayState) {
	aggregateTrayStateWithToast(ctx, snapshots, trayCh, tray.ShowToast)
}

// aggregateTrayStateWithToast is the testable inner form.
// Production wrappers pass tray.ShowToast; tests pass a recorder.
func aggregateTrayStateWithToast(ctx context.Context, snapshots <-chan []api.DaemonStatus, trayCh chan<- tray.TrayState, showToast toastFn) {
	const sentinel = tray.TrayState(-1)
	last := sentinel
	prevFailed := map[string]bool{}
	for {
		select {
		case <-ctx.Done():
			return
		case rows, ok := <-snapshots:
			if !ok {
				return
			}
			// Failure-onset diff: for each row failed in this
			// snapshot, fire a toast if it wasn't failed in the
			// prior snapshot. Track currentFailed in a fresh map so
			// rows that disappeared between snapshots aren't kept
			// in prevFailed (a regression that would lose onsets
			// when a daemon flaps off → on with a different state).
			currentFailed := make(map[string]bool, len(rows))
			for _, r := range rows {
				if !isFailedRow(r) {
					continue
				}
				k := rowKey(r)
				currentFailed[k] = true
				if prevFailed[k] {
					continue // already failed in prior snapshot
				}
				go func(server, daemon, state string, lastResult int32) {
					title := "mcp-local-hub: daemon failed"
					body := fmt.Sprintf("%s/%s — state=%s, last_result=%d", server, daemon, state, lastResult)
					_ = showToast(title, body) // best-effort; toast errors logged elsewhere
				}(r.Server, r.Daemon, r.State, r.LastResult)
			}
			prevFailed = currentFailed

			// Tray-state coalescing as before.
			s := tray.Aggregate(rows)
			if s == last {
				continue
			}
			select {
			case trayCh <- s:
				last = s
			default:
				// Tray's StateCh buffer full — keep `last` unchanged so
				// we re-attempt forward on the next snapshot. The next
				// snapshot will see the same `s` (state hasn't changed
				// from this one's perspective) and try again.
			}
		}
	}
}
