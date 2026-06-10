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
// wires this to defaultIntentReader (a thin api.TryReadDaemonIntent
// wrapper); tests inject a deterministic fake to drive the user-stop
// / chronic-failure / TTL paths without touching disk.
//
// Returns the full IntentReadResult so the aggregator can distinguish:
//   - "valid" — fresh authoritative snapshot from disk.
//   - "missing" with Err == nil — file genuinely absent (clean install
//     or operator cleared every entry); the empty-Tasks map IS the
//     authoritative answer, not a fallback.
//   - "missing" with Err != nil — degraded read (lock timeout, parent
//     dir resolution failure, etc.); aggregator falls back to its
//     in-process intentCache so transient flock contention does not
//     regress user-stop suppression (Bug #3 → red icon flash).
//   - "corrupt" — file existed but parse failed; QuarantinePath is set
//     (the read auto-quarantined). Aggregator treats this as "no
//     fresh data" — cache stays, operator sees the rename event in
//     the watchdog audit log.
//
// Round 3 codex finding R1: returning bare DaemonIntentFile collapsed
// the "lock timeout" and "genuinely empty" cases to the same value, so
// the aggregator could not tell whether the empty Tasks map was
// authoritative or a degraded fallback. The richer return type fixes
// the regression where Bug #3 reappeared under flock contention
// (e.g. mid-`mcphub install` audit append) — empty Tasks bypassed
// the user-stop suppression branch in tray.AggregateWithIntent.
type intentReaderFn func() api.IntentReadResult

// defaultIntentReaderTimeout caps how long the tray-side intent read
// is willing to wait on the daemon-intent.json flock per snapshot
// cycle. Sized at 250ms — 5% of the StatusPoller's 5-second snapshot
// cadence. The choice is a deliberate tradeoff:
//
//   - Anything over ~500ms would visibly delay tray icon / toast
//     updates if the lock is held by a slow writer (e.g. the watchdog
//     mid-`set-intent` audit append on a network-mounted state dir).
//   - Anything under ~50ms would race against legitimate writes that
//     hold the lock for tens of milliseconds during atomic
//     temp+rename, so the tray would degrade to "no preference" on
//     normal operation and start surfacing red-icon false positives
//     for user-stopped daemons.
//
// 250ms gives ~25 retry polls at the 10ms retryDelay inside
// (*api.API).TryReadDaemonIntent — ample slack for routine writes
// while still bounding the tray's hot path. Bot finding (PR #142
// round 2 P2): the prior wiring used the blocking ReadDaemonIntent,
// so an extended lock hold (or a stalled writer process) would
// freeze tray updates and prevent ctx.Done() observation.
const defaultIntentReaderTimeout = 250 * time.Millisecond

// intentCacheTTL bounds how long the aggregator's in-process intent
// cache survives without a fresh successful read. 5 minutes is well
// past any realistic mcphub operation window (longest known writer
// hold: install audit-append on a slow disk, ~hundreds of ms;
// watchdog --once with audit-degraded cascade, <30s); a stuck holder
// for 5 full minutes is no longer "transient contention" — it is an
// operator-visible incident that should not let stale stop intents
// continue to suppress error icons indefinitely.
//
// At 5s snapshot cadence the cache covers ~60 cycles before eviction.
const intentCacheTTL = 5 * time.Minute

// defaultIntentReader reads the on-disk intent via api.TryReadDaemonIntent
// with the bounded timeout above so a held lock cannot stall the tray
// snapshot loop. Per IntentReadResult contract:
//   - State="missing" + Err=nil  → file genuinely absent (clean install
//     or operator cleared every entry). Empty Tasks map IS authoritative.
//   - State="missing" + Err!=nil → degraded read (lock timeout etc.).
//     The aggregator's intent cache covers the gap; this avoids the
//     Bug #3 regression where flock contention would flash a red icon
//     even though the operator's user-stop intent was still active.
//   - State="corrupt"            → File has an empty Tasks map (the
//     read auto-quarantined the corrupt file). Aggregator treats it
//     as "no fresh data" — cache stays, no UI noise. Operators see
//     corruption via the watchdog audit log surface, not the tray.
//   - State="valid"              → Tasks map is authoritative.
//
// Round 3 codex finding A2: timeout-induced fallbacks ARE silent BY
// DESIGN at this layer (the tray is a low-noise visual surface, not a
// diagnostic log; corrupt-file events surface separately through the
// audit log). The in-process intent cache added per R1 ensures the
// silent fallback does NOT cause a UX regression — a stale-but-recent
// snapshot keeps user-stop suppression alive across short flock
// contention windows.
func defaultIntentReader() api.IntentReadResult {
	a := api.NewAPI()
	res := a.TryReadDaemonIntent(defaultIntentReaderTimeout)
	if res.File.Tasks == nil {
		// Defensive: every documented IntentReadResult path returns a
		// non-nil Tasks map, but a future regression on the api side
		// must not nil-panic the tray aggregator.
		res.File.Tasks = map[string]api.DaemonIntent{}
	}
	return res
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
// pollerErrors carries the poller's per-cycle fetch errors (PR #281
// round-2 P2). On a down supervisor the poller early-returns before
// fanning a snapshot, so without this channel the aggregator would
// never recompute and the tray icon would freeze at its last value
// (typically green) — a fail-quiet on the operator's primary health
// signal. Receiving an error here drives the tray to StateError.
func aggregateTrayState(ctx context.Context, snapshots <-chan []api.DaemonStatus, pollerErrors <-chan error, trayCh chan<- tray.TrayState) {
	aggregateTrayStateWithToast(ctx, snapshots, pollerErrors, trayCh, tray.ShowToast, defaultIntentReader)
}

// intentCacheState carries the most-recent successfully-read intent
// snapshot so the aggregator can survive transient flock contention
// without dropping user-stop suppression. Owned by the aggregator
// goroutine — never escapes — so no synchronisation is required (the
// aggregator is the sole reader and writer per cycle, single-threaded
// by construction). The value-type fields keep `go vet -race` happy
// even if a future refactor accidentally shares the struct: there is
// no shared pointer aliasing into the cached map (api.DaemonIntentFile
// holds the only reference, and we replace it whole on each refresh).
//
// Round 3 codex finding R1: empty-Tasks fallback under contention
// regressed Bug #3 — a daemon with LastResult=1 (Node MCP graceful
// stdin-close) classified as StateError because the user-stop intent
// was unavailable for that snapshot cycle. The cache holds the last
// known-good snapshot so contention windows shorter than intentCacheTTL
// preserve suppression.
type intentCacheState struct {
	file   api.DaemonIntentFile
	seenAt time.Time
	valid  bool // true once we've successfully read at least once
}

// aggregateTrayStateWithToast is the testable inner form.
// Production wrappers pass tray.ShowToast + defaultIntentReader; tests
// pass a recorder + a deterministic intent fake.
//
// Cache eviction policy (round 3 codex finding R1):
//   - A successful read with State == valid OR (State == missing && Err
//     == nil) replaces the cache. "Missing with nil Err" means the file
//     is genuinely absent on disk (operator cleared every entry or fresh
//     install) — that empty Tasks map IS authoritative and must overwrite
//     stale stop intents.
//   - State == corrupt or State == missing with non-nil Err (lock timeout,
//     parent-dir resolution failure, etc.) leaves the cache untouched —
//     the read produced no fresh information.
//   - Cache age > intentCacheTTL (5 min) evicts the cache regardless;
//     a stuck holder for 5 full minutes is operator-visible and stale
//     suppression is no longer the lesser evil.
//
// The cache is per-aggregator-goroutine; it lives on the stack of this
// function. Tests that exercise contention flow synthesise IntentReadResult
// values via a fake intentReaderFn and assert the cache preserves
// suppression across the contended cycle.
func aggregateTrayStateWithToast(ctx context.Context, snapshots <-chan []api.DaemonStatus, pollerErrors <-chan error, trayCh chan<- tray.TrayState, showToast toastFn, readIntent intentReaderFn) {
	const sentinel = tray.TrayState(-1)
	last := sentinel
	prevFailed := map[string]bool{}
	cache := intentCacheState{}
	// forward coalesces redundant SetIcon calls: it pushes s onto trayCh
	// only when s differs from the last forwarded state, and keeps `last`
	// unchanged on a full buffer so the next cycle re-attempts the same
	// forward. Shared by the snapshot path and the poller-error path so
	// both honor the same coalescing + non-blocking-send discipline.
	forward := func(s tray.TrayState) {
		if s == last {
			return
		}
		select {
		case trayCh <- s:
			last = s
		default:
			// Tray's StateCh buffer full — keep `last` unchanged so we
			// re-attempt forward on the next cycle.
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case err, ok := <-pollerErrors:
			if !ok {
				// Error channel closed (test-driven shutdown). The
				// snapshot channel remains the primary feed; keep
				// looping so a nil errorCh case does not spin.
				pollerErrors = nil
				continue
			}
			_ = err // the error's presence, not its text, drives the icon
			// A poller fetch error means the fail-loud status snapshot
			// could not be obtained — typically ErrSupervisorDown. Drive
			// the tray to StateError (the red high-priority icon) so the
			// operator sees the supervisor is unreachable, instead of the
			// icon freezing at its last (usually green) value because no
			// snapshot was fanned out on the error path (PR #281 round-2
			// P2). StateError, not an empty-snapshot StateHealthy.
			forward(tray.StateError)
		case rows, ok := <-snapshots:
			if !ok {
				return
			}
			now := time.Now().UTC()

			// Snapshot the intent file once per cycle. Reading once per
			// cycle (not once per row) is intentional: the watchdog
			// poll cadence is 5s and the intent file is human-edit-
			// frequency, so a per-row stat would be wasted work.
			//
			// Round 3 codex finding R1: when readIntent returns a
			// degraded result (lock timeout / corrupt-quarantine / etc.)
			// we use the cached snapshot if still within intentCacheTTL,
			// so transient flock contention does not drop user-stop
			// suppression. This is the second layer of the graceful-
			// degrade contract: TryReadDaemonIntent's empty-Tasks
			// fallback covers the lock-timeout error path; this cache
			// covers the resulting blank intent so Bug #3 stays fixed
			// even when the watchdog/install holds the lock past
			// defaultIntentReaderTimeout.
			intent := api.DaemonIntentFile{Tasks: map[string]api.DaemonIntent{}}
			if readIntent != nil {
				res := readIntent()
				usable := res.State == api.IntentStateValid ||
					(res.State == api.IntentStateMissing && res.Err == nil)
				if usable {
					if res.File.Tasks == nil {
						res.File.Tasks = map[string]api.DaemonIntent{}
					}
					intent = res.File
					cache.file = res.File
					cache.seenAt = now
					cache.valid = true
				} else if cache.valid && now.Sub(cache.seenAt) <= intentCacheTTL {
					// Use the last known-good snapshot — survives short
					// flock contention windows. The cache is per-cycle
					// owned by this goroutine; a value-type assignment
					// is sufficient (api.DaemonIntentFile holds the only
					// reference into Tasks).
					intent = cache.file
				}
				// else: cache empty or expired → fall back to the
				// already-initialized empty intent (no preference).
			}

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

			// Tray-state coalescing as before — intent-aware variant so
			// a user-stop's exit-code-1 does not land at StateError.
			// Codex deep-sec round 1 (MED): a suppressed row is also
			// excluded from the running/total denominator, so a Running
			// peer alongside a user-stopped row classifies as
			// StateHealthy (only the wanted-running peer counts toward
			// the ratio). When EVERY non-maintenance row is suppressed
			// (codex bot PR #142 round 4 P2), total==0 + suppressedCount>0
			// classifies as StateDown — operator-stopped systems must
			// surface "nothing running", not green-icon-over-stopped.
			s := tray.AggregateWithIntent(rows, intent, now)
			forward(s)
		}
	}
}
