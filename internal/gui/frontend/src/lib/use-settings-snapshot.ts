import { useEffect, useState, useCallback, useRef } from "preact/hooks";
import { getSettings } from "./settings-api";
import type { SettingsSnapshot, SettingsSnapshotState } from "./settings-types";

export function useSettingsSnapshot(): SettingsSnapshot {
  const [state, setState] = useState<SettingsSnapshotState>({
    status: "loading",
    data: null,
    error: null,
  });
  const generation = useRef(0);
  // Track whether a successful load has ever completed. Used to decide
  // whether to propagate a refresh error to the caller (r6 P2) — only when
  // there is stale data to preserve does a caller need to know the refresh
  // failed; during the initial-load path the error is already shown in state
  // and the useEffect void-caller does not need the rejection.
  const hasOkRef = useRef(false);

  const refresh = useCallback(async () => {
    const myGen = ++generation.current;
    // Stale-while-revalidate: keep previous data if we already have ok.
    setState((prev) => (prev.status === "ok" ? prev : { status: "loading", data: null, error: null }));
    try {
      const data = await getSettings();
      if (myGen !== generation.current) return;
      hasOkRef.current = true;
      setState({ status: "ok", data, error: null });
    } catch (e) {
      if (myGen !== generation.current) return;
      // Codex PR #20 r4 P2: preserve last-good data on refresh failure.
      // Without this, a transient GET failure after a section save would
      // unmount section-local state via SettingsScreen's full-page error
      // fallback, dropping the user's retry context for partial failures.
      // Initial-load failures (no prior ok) still surface as the error state.
      setState((prev) => {
        if (prev.status === "ok") {
          return prev; // keep stale data; refresh failure is silent
        }
        return { status: "error", data: null, error: e as Error };
      });
      // Codex PR #20 r6 P2: propagate so callers (useSectionSaveFlow.save)
      // can detect refresh failure and decide whether to clear in-flight edits.
      // Only propagate when we have stale data (hasOkRef=true); initial-load
      // failures are consumed by the error state above and must not surface as
      // an unhandled rejection from the void-discarded useEffect call.
      if (hasOkRef.current) {
        throw e;
      }
    }
  }, []);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  return { ...state, refresh };
}
