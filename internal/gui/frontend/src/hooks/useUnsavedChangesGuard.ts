import { useEffect } from "preact/hooks";

// useUnsavedChangesGuard installs a window.beforeunload listener that
// fires the browser's native "Leave site?" dialog when `dirty` is true.
// When dirty is false (or toggles back), the listener is removed so the
// user never sees the dialog on a clean form.
//
// Note: modern browsers ignore the CUSTOM message — Chrome 51+ shows
// its own default text. Returning any truthy value is the signal that
// dirty state exists. We return a string for older browsers that may
// still display it.
export function useUnsavedChangesGuard(dirty: boolean): void {
  useEffect(() => {
    if (!dirty) return;
    const handler = (e: BeforeUnloadEvent) => {
      e.preventDefault();
      // Modern browsers ignore this string; returnValue is the signal.
      // Legacy browsers may still display it.
      e.returnValue = "You have unsaved changes.";
      return "You have unsaved changes.";
    };
    window.addEventListener("beforeunload", handler);
    return () => window.removeEventListener("beforeunload", handler);
  }, [dirty]);
}
