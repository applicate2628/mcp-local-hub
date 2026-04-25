import { useEffect, useState, useCallback } from "preact/hooks";
import { getSecrets, type SecretsEnvelope, type APIError } from "./secrets-api";

export type SnapshotState =
  | { status: "loading"; data: null; error: null }
  | { status: "ok"; data: SecretsEnvelope; error: null }
  | { status: "error"; data: null; error: APIError | Error };

export function useSecretsSnapshot(): SnapshotState & { refresh: () => Promise<void> } {
  const [state, setState] = useState<SnapshotState>({ status: "loading", data: null, error: null });

  const refresh = useCallback(async () => {
    setState({ status: "loading", data: null, error: null });
    try {
      const data = await getSecrets();
      setState({ status: "ok", data, error: null });
    } catch (e) {
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
