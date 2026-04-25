import { useEffect, useState, useCallback, useRef } from "preact/hooks";
import { getSecrets, type SecretsEnvelope, type APIError } from "./secrets-api";

export type SnapshotState =
  | { status: "loading"; data: null; error: null }
  | { status: "ok"; data: SecretsEnvelope; error: null }
  | { status: "error"; data: null; error: APIError | Error };

export function useSecretsSnapshot(): SnapshotState & { refresh: () => Promise<void> } {
  const [state, setState] = useState<SnapshotState>({ status: "loading", data: null, error: null });
  // Codex Task-4 quality review F3: monotonically incrementing
  // generation. Each refresh captures its own generation; if the
  // generation advances before the await resolves, the result is
  // stale and we drop the setState call.
  const generation = useRef(0);

  const refresh = useCallback(async () => {
    const myGen = ++generation.current;
    // Stale-while-revalidate: only show "loading" on initial fetch.
    // Background refreshes keep the previous data visible so dependent
    // local state (e.g., InitKeyedView's CTA/banner) is not unmounted.
    setState((prev) => {
      if (prev.status === "ok") {
        return prev; // keep last-known data; no loading flash
      }
      return { status: "loading", data: null, error: null };
    });
    try {
      const data = await getSecrets();
      if (myGen !== generation.current) return; // a newer refresh started
      setState({ status: "ok", data, error: null });
    } catch (e) {
      if (myGen !== generation.current) return;
      setState({ status: "error", data: null, error: e as Error });
    }
  }, []);

  useEffect(() => {
    refresh();
  }, [refresh]);

  // Codex plan-R1 P2: refetch on window focus so a vault edit from a
  // separate tab/CLI surfaces in the registry view without a manual
  // reload. This matches memo §3.1 frontend item #8 ("polls on focus").
  useEffect(() => {
    const onFocus = () => { void refresh(); };
    window.addEventListener("focus", onFocus);
    return () => window.removeEventListener("focus", onFocus);
  }, [refresh]);

  return { ...state, refresh };
}
