import { useCallback, useEffect, useRef, useState } from "preact/hooks";

// SCAN_POLL_MS is the auto-refresh cadence for the scan-driven screens
// (Discovery + Servers). /api/scan is cheap — API.Scan() does NOT set
// WithProcessCount (see internal/api/scan.go), so there's no wmic
// snapshot, just config-file reads + classify (~ms). Polling it on a
// short interval is safe; 10s keeps an open scan view current without
// hammering the backend.
export const SCAN_POLL_MS = 10_000;

// AGO_TICK_MS drives the "updated Ns ago" indicator's own re-render so
// the elapsed-time label advances between scans without forcing a real
// refetch. 1s granularity matches the "Ns ago" wording.
const AGO_TICK_MS = 1_000;

// useAutoScan owns the timer + Page-Visibility lifecycle shared by the
// scan-driven screens, WITHOUT owning any screen-specific fetch state or
// (critically) the Servers dirty/pending model. The screen passes:
//
//   - run: the screen's own refetch (it updates that screen's rendered
//     baseline however it likes — setScan, collectServers, etc.). It is
//     read through a ref so a fresh closure each render does NOT restart
//     the interval.
//   - paused: when true, the interval tick is skipped and the
//     becoming-visible immediate refresh is suppressed. The Servers
//     screen passes `dirty.size > 0` here so auto-refresh never clobbers
//     unsaved matrix edits; Discovery passes a constant false.
//
// The hook returns:
//   - rescan(): fire an immediate refetch and RESET the interval (so the
//     next auto tick is a full period away). The "Rescan now" button
//     calls this.
//   - lastScanAt: epoch-ms of the most recent successful run (null until
//     the first completes). Drives the "updated Ns ago" indicator.
//   - agoSeconds: whole seconds since lastScanAt, re-rendered on a 1s
//     tick so the label advances live. null until the first run.
//
// Timer discipline mirrors Dashboard.tsx exactly: a single resettable
// setInterval, a `cancelled` flag so an in-flight run that resolves after
// unmount does not setState, clearInterval on unmount. The
// visibilitychange listener is registered on `document` and removed on
// unmount.
export function useAutoScan(
  run: () => void | Promise<void>,
  paused: boolean,
): { rescan: () => void; lastScanAt: number | null; agoSeconds: number | null } {
  const [lastScanAt, setLastScanAt] = useState<number | null>(null);
  // `now` is bumped on a 1s tick purely so the derived agoSeconds advances
  // between scans. It never drives a refetch.
  const [now, setNow] = useState<number>(() => Date.now());

  // `run` and `paused` are taken by ref so changing them does NOT tear
  // down and rebuild the interval/listener (which would reset the cadence
  // on every parent render). The single mount-scoped effect below reads
  // the latest values through these refs on each tick.
  const runRef = useRef(run);
  runRef.current = run;
  const pausedRef = useRef(paused);
  pausedRef.current = paused;

  // doRun executes the screen's refetch and stamps lastScanAt on success.
  // `cancelledRef` guards the post-await setState against a resolve after
  // unmount. Mirrors Dashboard's `cancelled` flag.
  const cancelledRef = useRef(false);
  const doRun = useCallback(async () => {
    try {
      await runRef.current();
    } finally {
      if (!cancelledRef.current) setLastScanAt(Date.now());
    }
  }, []);

  // Single poll handle, stored in a ref so rescan() can clear + restart it
  // (reset-the-timer semantic) without rebuilding the mount-scoped effect.
  const pollRef = useRef<ReturnType<typeof setInterval> | null>(null);
  const startPoll = useCallback(() => {
    if (pollRef.current != null) clearInterval(pollRef.current);
    pollRef.current = setInterval(() => {
      // Skip the tick while the tab is hidden (pointless background churn)
      // or while the screen reports unsaved edits (Servers dirty pause).
      if (document.hidden) return;
      if (pausedRef.current) return;
      void doRun();
    }, SCAN_POLL_MS);
  }, [doRun]);

  // Mount-scoped effect: initial fetch, interval poll, visibility pause,
  // 1s ago-ticker. Empty deps → runs once; refs supply current values.
  useEffect(() => {
    cancelledRef.current = false;

    // Initial fetch (mirrors each screen's existing on-mount /api/scan).
    void doRun();
    startPoll();

    // On becoming visible again after being hidden, do one immediate
    // refresh so the operator does not stare at a stale view for up to a
    // full poll period. Suppressed while paused (unsaved edits).
    const onVisibility = () => {
      if (document.hidden) return;
      if (pausedRef.current) return;
      void doRun();
    };
    document.addEventListener("visibilitychange", onVisibility);

    // Independent 1s ticker so "updated Ns ago" advances live.
    const agoTimer = setInterval(() => setNow(Date.now()), AGO_TICK_MS);

    return () => {
      cancelledRef.current = true;
      if (pollRef.current != null) clearInterval(pollRef.current);
      pollRef.current = null;
      clearInterval(agoTimer);
      document.removeEventListener("visibilitychange", onVisibility);
    };
  }, [doRun, startPoll]);

  // rescan: immediate refetch + interval reset. Restarting the single poll
  // makes the next auto tick a full period away (the operator just
  // refreshed — an auto tick 1s later would be wasteful).
  const rescan = useCallback(() => {
    void doRun();
    startPoll();
  }, [doRun, startPoll]);

  const agoSeconds =
    lastScanAt == null ? null : Math.max(0, Math.floor((now - lastScanAt) / 1000));

  return { rescan, lastScanAt, agoSeconds };
}
