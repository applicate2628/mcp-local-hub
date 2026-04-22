import { useEffect, useRef } from "preact/hooks";

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
export function useEventSource(
  url: string | null,
  handlers: Record<string, (ev: MessageEvent) => void>,
) {
  const handlersRef = useRef(handlers);
  handlersRef.current = handlers;

  useEffect(() => {
    if (!url) return;
    const es = new EventSource(url);
    const attached: Array<[string, (ev: MessageEvent) => void]> = [];
    for (const name of Object.keys(handlersRef.current)) {
      const listener = (ev: MessageEvent) => handlersRef.current[name]?.(ev);
      es.addEventListener(name, listener as EventListener);
      attached.push([name, listener]);
    }
    return () => {
      for (const [name, listener] of attached) {
        es.removeEventListener(name, listener as EventListener);
      }
      es.close();
    };
  }, [url]);
}
