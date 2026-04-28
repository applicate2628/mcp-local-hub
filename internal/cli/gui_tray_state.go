// internal/cli/gui_tray_state.go
package cli

import (
	"context"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/tray"
)

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
func aggregateTrayState(ctx context.Context, snapshots <-chan []api.DaemonStatus, trayCh chan<- tray.TrayState) {
	const sentinel = tray.TrayState(-1)
	last := sentinel
	for {
		select {
		case <-ctx.Done():
			return
		case rows, ok := <-snapshots:
			if !ok {
				return
			}
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
