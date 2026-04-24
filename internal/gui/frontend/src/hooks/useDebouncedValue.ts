import { useEffect, useState } from "preact/hooks";

// useDebouncedValue returns a throttled mirror of `value`: when `value`
// changes, the returned `debounced` stays the same for `waitMs` ms, then
// flips to the latest value. Multiple changes inside the window coalesce
// — only the most recent value is ever committed.
//
// Used by AddServer for the YAML preview pane: the form state updates
// on every keystroke, but the preview only re-renders once typing has
// paused for 150ms. This prevents preview scroll/caret churn without
// adding visible typing lag (150ms is below the perceived-lag threshold).
export function useDebouncedValue<T>(value: T, waitMs: number): T {
  const [debounced, setDebounced] = useState(value);
  useEffect(() => {
    const timer = setTimeout(() => setDebounced(value), waitMs);
    return () => clearTimeout(timer);
  }, [value, waitMs]);
  return debounced;
}
