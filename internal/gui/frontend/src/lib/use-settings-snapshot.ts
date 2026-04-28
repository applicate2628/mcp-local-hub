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

  const refresh = useCallback(async () => {
    const myGen = ++generation.current;
    // Stale-while-revalidate: keep previous data if we already have ok.
    setState((prev) => (prev.status === "ok" ? prev : { status: "loading", data: null, error: null }));
    try {
      const data = await getSettings();
      if (myGen !== generation.current) return;
      setState({ status: "ok", data, error: null });
    } catch (e) {
      if (myGen !== generation.current) return;
      setState({ status: "error", data: null, error: e as Error });
    }
  }, []);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  return { ...state, refresh };
}
