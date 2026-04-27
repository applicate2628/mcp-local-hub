import { useEffect, useState, useCallback, useRef } from "preact/hooks";
import { getSecrets, type SecretsEnvelope, type APIError } from "./secrets-api";

// fetchedAt (A3-b memo D8a): epoch ms of last *successful* fetch. Null
// while initial loading and on errors that have not yet seen a success.
// On a success-then-error transition, fetchedAt is preserved (so the
// picker can show "last loaded N min ago" copy in Retry messaging),
// even though `data` is set to null (existing hook contract — stale
// data is NOT preserved on error transition; classifyRefState falls
// back to "unverified" per memo §5.5 R1).
export type SnapshotState =
  | { status: "loading"; data: null;            error: null;             fetchedAt: null }
  | { status: "ok";      data: SecretsEnvelope; error: null;             fetchedAt: number }
  | { status: "error";   data: null;            error: APIError | Error; fetchedAt: number | null };

export type SecretsSnapshot = SnapshotState & { refresh: () => Promise<void> };

export function useSecretsSnapshot(): SecretsSnapshot {
  const [state, setState] = useState<SnapshotState>({
    status: "loading",
    data: null,
    error: null,
    fetchedAt: null,
  });
  // Codex Task-4 quality review F3 (A3-a): monotonically incrementing
  // generation. Each refresh captures its own generation; if the
  // generation advances before the await resolves, the result is
  // stale and we drop the setState call.
  const generation = useRef(0);
  // Codex plan-R1 P1-3: preserve fetchedAt across the loading→error→retry→error
  // path. The setState transitions can land in "loading" mid-refresh, but the
  // ref always carries the last successful timestamp so the eventual error
  // state can re-attach it. Updated on every successful fetch.
  const lastFetchedAtRef = useRef<number | null>(null);

  const refresh = useCallback(async () => {
    const myGen = ++generation.current;
    // Stale-while-revalidate: only show "loading" on initial fetch.
    // Background refreshes keep the previous data visible so dependent
    // local state (e.g., InitKeyedView's CTA/banner) is not unmounted.
    // Codex plan-R1 P1-3: when transitioning error→loading mid-refresh,
    // carry the last-known fetchedAt forward so a subsequent error retry
    // doesn't lose the original success timestamp.
    setState((prev) => {
      if (prev.status === "ok") {
        return prev; // keep last-known data; no loading flash
      }
      return { status: "loading", data: null, error: null, fetchedAt: null };
    });
    try {
      const data = await getSecrets();
      if (myGen !== generation.current) return;
      const now = Date.now();
      lastFetchedAtRef.current = now;
      setState({ status: "ok", data, error: null, fetchedAt: now });
    } catch (e) {
      if (myGen !== generation.current) return;
      // Preserve last-known fetchedAt across error→error retry cycles via
      // the ref; the ref is the source of truth for "last successful load".
      setState({
        status: "error",
        data: null,
        error: e as Error,
        fetchedAt: lastFetchedAtRef.current,
      });
    }
  }, []);

  useEffect(() => {
    refresh();
  }, [refresh]);

  // Codex plan-R1 P2 (A3-a): refetch on window focus so a vault edit
  // from a separate tab/CLI surfaces in the registry view without a
  // manual reload.
  useEffect(() => {
    const onFocus = () => { void refresh(); };
    window.addEventListener("focus", onFocus);
    return () => window.removeEventListener("focus", onFocus);
  }, [refresh]);

  return { ...state, refresh };
}
