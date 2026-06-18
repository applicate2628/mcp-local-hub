import { useEffect, useRef, useState } from "preact/hooks";

// SseConnectionState describes the live transport status of the SSE
// stream, surfaced so screens can show a "stale / reconnecting" cue
// instead of trusting a frozen-but-plausible last snapshot when the
// supervisor/GUI drops:
//   - "connecting"   — initial open, no onopen yet (also the null-url idle).
//   - "open"         — onopen fired; the stream is live.
//   - "reconnecting" — onerror fired. Native EventSource auto-retries,
//                      so this is a transient state that returns to
//                      "open" on the next onopen; we do NOT manually
//                      reconnect.
export type SseConnectionState = "connecting" | "open" | "reconnecting";

// useEventSource subscribes to a Server-Sent Events endpoint for the
// lifetime of the calling component. It replaces the legacy pattern
// where each screen opened `new EventSource(...)` and registered a
// cleanup via window.mcphub.registerCleanup. Here the cleanup is the
// effect's return — Preact/React call it on unmount, on dep change,
// and before re-running the effect, all of which close the old stream
// exactly once.
//
// The handler map is taken by ref so repeated parent renders (new
// handler object identity every render) do not reopen the SSE stream.
// Callers may mutate handlers between renders; changes take effect on
// the next event.
//
// Returns the live connection state. The return is additive — callers
// that ignore it (the long-standing `useEventSource(url, handlers)`
// call form) are unaffected.
export function useEventSource(
  url: string | null,
  handlers: Record<string, (ev: MessageEvent) => void>,
): SseConnectionState {
  const handlersRef = useRef(handlers);
  handlersRef.current = handlers;
  const [connectionState, setConnectionState] =
    useState<SseConnectionState>("connecting");

  useEffect(() => {
    if (!url) return;
    setConnectionState("connecting");
    const es = new EventSource(url);
    // Native EventSource reconnects on its own after an error, so we
    // only surface the state — never call es.close()+new EventSource()
    // here (that would race the browser's own retry).
    es.onopen = () => setConnectionState("open");
    es.onerror = () => setConnectionState("reconnecting");
    const attached: Array<[string, (ev: MessageEvent) => void]> = [];
    for (const name of Object.keys(handlersRef.current)) {
      const listener = (ev: MessageEvent) => handlersRef.current[name]?.(ev);
      es.addEventListener(name, listener as EventListener);
      attached.push([name, listener]);
    }
    return () => {
      es.onopen = null;
      es.onerror = null;
      for (const [name, listener] of attached) {
        es.removeEventListener(name, listener as EventListener);
      }
      es.close();
    };
  }, [url]);

  return connectionState;
}
