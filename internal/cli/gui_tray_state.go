// internal/cli/gui_tray_state.go
package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/tray"
)

// intentReaderFn returns the operator-recorded daemon-intent file
// snapshot used by the tray aggregator + toast onset gate. Production
// wires this to (*api.API).ReadDaemonIntent (defaultIntentReader);
// tests inject a deterministic fake to drive the user-stop / chronic-
// failure / TTL paths without touching disk. Returning the empty file
// on read error is the existing "no preference" fallback semantic
// shared with api.IntentStillRunning — a corrupt intent file must not
// brick tray classification.
type intentReaderFn func() api.DaemonIntentFile

// defaultIntentReader reads the on-disk intent via api.ReadDaemonIntent.
// Per IntentReadResult contract:
//   - State="missing"  → empty Tasks map, treat as no preference.
//   - State="corrupt"  → File still has an empty Tasks map (the read
//     auto-quarantined the corrupt file); no preference, no spam.
//   - State="valid"    → Tasks map is authoritative.
//
// We discard the QuarantinePath / Err fields here because the tray's
// only job is "use intent if available, fall back if not". Operators
// see corruption events through the watchdog's audit log, not the tray.
func defaultIntentReader() api.DaemonIntentFile {
	a := api.NewAPI()
	res := a.ReadDaemonIntent()
	if res.File.Tasks == nil {
		return api.DaemonIntentFile{Tasks: map[string]api.DaemonIntent{}}
	}
	return res.File
}

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

// isFailedRow returns true when a daemon row reports a real failure.
// Mirrors the StateError predicates in tray.AggregateWithIntent so
// toast onset matches tray icon onset — the user sees the icon turn
// red and the toast pop at the same transition.
//
// LastResult classification is delegated to api.IsRealFailure
// (internal/api/recovery.go) — the single canonical predicate shared
// with the watchdog and tray icon aggregator (plan §18 single-source-
// of-truth). The state-string "fail" match is kept as a separate
// signal because some daemon paths emit Failed without a matching
// LastResult update; that fallback is local to this consumer and not
// part of the canonical failure predicate.
//
// Bug #3 retained as a regression guard: callers that want intent-
// aware classification (i.e. suppress user-initiated stops that exit
// non-zero) should use isFailedRowWithIntent — toast spam after
// `mcphub stop --server X` was the original symptom alongside the red
// tray icon.
func isFailedRow(r api.DaemonStatus) bool {
	if api.IsRealFailure(r.LastResult) {
		return true
	}
	return strings.Contains(strings.ToLower(r.State), "fail")
}

// isFailedRowWithIntent extends isFailedRow with the same intent
// suppression that tray.AggregateWithIntent applies. Keeps tray icon
// and toast onset on the same gate so the user never sees one without
// the other after a clean user-stop. ChronicFailure is NOT suppressed
// — the watchdog quarantine reason exists precisely so the operator
// gets the alert.
func isFailedRowWithIntent(r api.DaemonStatus, intent api.DaemonIntentFile, now time.Time) bool {
	if !isFailedRow(r) {
		return false
	}
	if r.TaskName == "" || intent.Tasks == nil {
		return true
	}
	entry, ok := intent.Tasks[r.TaskName]
	if !ok {
		return true
	}
	active, reason := entry.IsActiveStop(now)
	if !active {
		return true
	}
	// Chronic-failure must surface; every other active-stop reason is
	// operator-initiated and should hide the row from toast onset.
	return reason == api.IntentReasonChronicFailure
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
//
// Bug #3: each cycle reads the daemon-intent file (PR #134) so a
// row exiting non-zero AFTER the operator ran `mcphub stop` does not
// flash a red icon or fire a toast. Suppression is intent-active +
// non-chronic; chronic-failure stays visible because that is the
// watchdog telling the operator something is wrong.
func aggregateTrayState(ctx context.Context, snapshots <-chan []api.DaemonStatus, trayCh chan<- tray.TrayState) {
	aggregateTrayStateWithToast(ctx, snapshots, trayCh, tray.ShowToast, defaultIntentReader)
}

// aggregateTrayStateWithToast is the testable inner form.
// Production wrappers pass tray.ShowToast + defaultIntentReader; tests
// pass a recorder + a deterministic intent fake.
func aggregateTrayStateWithToast(ctx context.Context, snapshots <-chan []api.DaemonStatus, trayCh chan<- tray.TrayState, showToast toastFn, readIntent intentReaderFn) {
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
			// Snapshot the intent file once per cycle. Reading once per
			// cycle (not once per row) is intentional: the watchdog
			// poll cadence is 5s and the intent file is human-edit-
			// frequency, so a per-row stat would be wasted work. A
			// stale snapshot for one cycle is acceptable — the next
			// cycle picks up any change.
			intent := api.DaemonIntentFile{Tasks: map[string]api.DaemonIntent{}}
			if readIntent != nil {
				intent = readIntent()
			}
			now := time.Now().UTC()

			// Failure-onset diff: for each row failed in this
			// snapshot, fire a toast if it wasn't failed in the
			// prior snapshot. Track currentFailed in a fresh map so
			// rows that disappeared between snapshots aren't kept
			// in prevFailed (a regression that would lose onsets
			// when a daemon flaps off → on with a different state).
			currentFailed := make(map[string]bool, len(rows))
			for _, r := range rows {
				if !isFailedRowWithIntent(r, intent, now) {
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

			// Tray-state coalescing as before — intent-aware variant
			// so a user-stop's exit-code-1 lands at StateDown, not
			// StateError.
			s := tray.AggregateWithIntent(rows, intent, now)
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
