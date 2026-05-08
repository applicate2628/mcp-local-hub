---
title: Tray icon shows "error" instead of "down" after `mcphub stop --server <name>` for daemons that exit with code 1
severity: medium (UX confusion — operator sees red error icon for what is actually a clean user-initiated stop)
found-by: D2.1 manual smoke during preview-tag verification on master HEAD d99af40
found-on: 2026-05-07
project: mcp-local-hub
related-pr: pre-existing — present before PR #131/#132/#133 (G2 didn't touch internal/tray/state.go)
---

# Tray "error" state misclassification after `mcphub stop`

## What happens

1. Operator runs `mcphub stop --server <name>`. The daemon receives kill signal (TerminateProcess via `schtasks /End` or equivalent).
2. The daemon process (e.g. Node-based MCP servers like `@modelcontextprotocol/server-memory`) exits with code `1` because that's the Node process's normal "stdin closed → exit non-zero" behavior.
3. Task Scheduler records `LastResult = 1`.
4. `internal/tray/state.go::isRealFailure(1)` returns `true` (1 != 0, 1 != -1, 1 < 0x41300).
5. `internal/tray/state.go::Aggregate` returns `StateError`.
6. Tray icon switches to red "error" variant.
7. Tooltip shows `mcp-local-hub: error`.

The operator sees a red error icon for what was actually a clean user-initiated stop. There's nothing actionable on the dashboard — the daemon is just Stopped — but the icon implies a failure occurred.

## Reproduction (verified 2026-05-07)

```text
$ mcphub status                 # all daemons Stopped → tooltip "down"
$ mcphub restart --server memory  # one running → tooltip "partial" ✓
$ mcphub stop --server memory   # back to all Stopped
$ # tooltip now: "error" instead of "down"
$ curl /api/status
{"server": "memory", "state": "Stopped", "last_result": 1}
```

## Root cause

`isRealFailure(1)` is correct in the general case — exit code 1 from a daemon usually IS a failure (panic, crash, error path). But for Node MCP servers, exit 1 is the response to graceful stdin-close. There's no signal in `LastResult` alone that distinguishes "user stopped" from "daemon crashed with exit 1".

## Fix candidates

### Option A — Track user-initiated stops separately

When `mcphub stop` is called, write a marker in the workspace registry / scheduler-tag metadata indicating "stop was user-initiated at <timestamp>". `Aggregate()` consults this marker before classifying as error.

- Pro: 100% correct semantic.
- Con: Adds state to track + needs cleanup if process reboots.

### Option B — Heuristic: exit code 1 within N seconds of mcphub stop is "user-stopped"

Track timestamps of recent `mcphub stop` calls in memory; if `LastResult == 1` AND the stop was within ~10s, classify as `StateDown` not `StateError`.

- Pro: No persistent state.
- Con: Heuristic edge cases (fast crash after stop request).

### Option C — Map `LastResult == 1` to "graceful stop" universally

Treat exit code 1 the same as -1 (placeholder) in `isRealFailure`.

- Pro: Simplest.
- Con: WRONG — many daemons exit 1 on real failure (panic/crash). This would mask real errors.

### Option D — Add /api/cleanup-result-cache or similar to manually clear

Operator who knows it's a clean stop can clear the error state explicitly via a CLI command or GUI button.

- Pro: Explicit operator control.
- Con: Requires manual action, defeats "at-a-glance" tray UX.

**Recommended: Option A** — explicit user-stop marker. Cleanest semantic, fits with existing scheduler-tag metadata pattern.

## Workaround (operator-side, until fix)

Restart the GUI to reset tray state: `mcphub gui --force --kill --yes` (it auto-respawns clean).

Or run `mcphub restart --server <name>` immediately after `mcphub stop` and stop again — the second stop sometimes results in a different LastResult depending on process timing.

Or accept the false-positive error tooltip; the dashboard shows the actual Stopped state correctly.

## Test gap

`internal/tray/state_test.go` covers `isRealFailure` in isolation but doesn't simulate the full lifecycle of "daemon was Running, user stopped it, exit was 1". Add a fixture test that walks this scenario.

## Plan

- Defer to Cleanup-6 or a dedicated tray-state PR post-preview-tag
- Recommended fix: Option A
- Add a regression test in the same PR

## Resolution

**Status:** closed (Option A shipped + 5 review rounds of refinement)
**Date:** 2026-05-08
**PR:** #142 merged as `d8e3999` (squash across 7 commits / 6 review rounds)

Implemented Option A using the existing `daemon-intent.json` user-stop marker (already shipped with watchdog feature in PR #134 commit `982c366`). Added `tray.AggregateWithIntent(rows, intent, now)` that suppresses `StateError` classification when the daemon row's intent is actively stopped (user-stop within TTL, user-disabled, uninstalled — chronic-failure carve-out keeps StateError so the watchdog quarantine remains visible).

Final shape after rounds 1-6 of bot + codex deep-sec review:

- **Suppression hides red** for non-Running rows with active stop intent (regardless of LastResult shape — operator intent overrides exit-code shape).
- **Suppression excludes from ratio** so "2 Running + 1 user-stopped" classifies as StateHealthy, not StatePartial.
- **Running rows ignore intent** — `r.State != "Running"` guard preserves `Running_IgnoresIntent` semantic.
- **All-suppressed → StateDown**: `total==0 + suppressedCount>0` returns StateDown ("operator stopped everything; nothing running"), not StateHealthy. No green icon over an entirely-stopped system.
- **Tray hot path bounded**: `(*api.API).TryReadDaemonIntent(timeout)` uses `flock.TryLockContext` with a 250ms cap so a held lock cannot freeze the snapshot loop. Aggregator carries an in-process intent cache (5-min TTL) so transient flock contention does NOT regress suppression.

Files:
- `internal/tray/state.go` — added `AggregateWithIntent` with intent-aware suppression + `suppressedCount` for the `total==0` fork
- `internal/tray/state_intent_test.go` — 21 tests covering all suppression branches, ratio rules, all-suppressed→StateDown, Plain+UserStop cross-product
- `internal/api/daemon_intent.go` — added `TryReadDaemonIntent(timeout)` for non-blocking tray hot-path; existing blocking `ReadDaemonIntent` unchanged for one-shot install/stop/uninstall
- `internal/api/daemon_intent_test.go` — 5 new tests covering contention, missing/corrupt paths, zero/negative timeout boundaries, errors.Is(DeadlineExceeded)
- `internal/cli/gui_tray_state.go` — `aggregateTrayStateWithToast` consults intent each tick + holds in-process cache; `defaultIntentReader` calls `TryReadDaemonIntent(250ms)`
- `internal/cli/gui_tray_state_test.go` — 12 tests including LockContentionPreservesUserStopSuppression (cache contract regression guard)
