// Toast + ToastContainer — Flowbite-styled transient notifications.
//
// Markup vocabulary is Flowbite's Toast component (see
// https://flowbite.com/docs/components/toast and the Context7-fetched
// reference): the `flex items-center w-full max-w-xs p-4 ... rounded-lg
// shadow-sm` shell, the colored icon chip (`w-8 h-8 ... rounded-lg`), the
// `ms-3 text-sm font-normal` message, and the `data-dismiss-target` close
// button. We keep ALL the Flowbite Tailwind classes so the toast reads as a
// Flowbite Toast visually.
//
// Dismiss handling: Flowbite's vanilla `data-dismiss-target` + initFlowbite()
// works on static server-rendered markup, but our toasts are a LIVE Preact
// list driven by the toast store — they mount/unmount as the store changes.
// Wiring Flowbite's Dismiss JS to a list whose nodes Preact owns and recycles
// is brittle (the handler would removeChild a node Preact still tracks). So
// the close button uses a Preact onClick that calls the store's
// dismissToast() — the store is the single source of truth, Preact reconciles
// the removal, and the visual stays 100% Flowbite. The `data-dismiss-target`
// attribute is still emitted for parity with the Flowbite contract.

import { useEffect, useState } from "preact/hooks";
import {
  dismissToast,
  subscribeToasts,
  type Toast as ToastModel,
  type ToastVariant,
} from "../lib/toast-store";

// Per-variant icon-chip color classes (Flowbite's documented palette) and
// the inline SVG path for the variant glyph. Kept in one map so the markup
// below stays declarative and a new variant is a one-line addition.
const VARIANT_ICON: Record<
  ToastVariant,
  { chip: string; path: string; label: string }
> = {
  success: {
    chip: "text-green-500 bg-green-100 dark:bg-green-800 dark:text-green-200",
    path: "M10 .5a9.5 9.5 0 1 0 9.5 9.5A9.51 9.51 0 0 0 10 .5Zm3.707 8.207-4 4a1 1 0 0 1-1.414 0l-2-2a1 1 0 0 1 1.414-1.414L9 10.586l3.293-3.293a1 1 0 0 1 1.414 1.414Z",
    label: "Check icon",
  },
  danger: {
    chip: "text-red-500 bg-red-100 dark:bg-red-800 dark:text-red-200",
    path: "M10 .5a9.5 9.5 0 1 0 9.5 9.5A9.51 9.51 0 0 0 10 .5Zm3.707 11.793a1 1 0 1 1-1.414 1.414L10 11.414l-2.293 2.293a1 1 0 0 1-1.414-1.414L8.586 10 6.293 7.707a1 1 0 0 1 1.414-1.414L10 8.586l2.293-2.293a1 1 0 0 1 1.414 1.414L11.414 10l2.293 2.293Z",
    label: "Error icon",
  },
  warning: {
    chip: "text-orange-500 bg-orange-100 dark:bg-orange-700 dark:text-orange-200",
    path: "M10 .5a9.5 9.5 0 1 0 9.5 9.5A9.51 9.51 0 0 0 10 .5ZM10 15a1 1 0 1 1 0-2 1 1 0 0 1 0 2Zm1-4a1 1 0 0 1-2 0V6a1 1 0 0 1 2 0v5Z",
    label: "Warning icon",
  },
  info: {
    chip: "text-blue-500 bg-blue-100 dark:bg-blue-800 dark:text-blue-200",
    path: "M10 .5a9.5 9.5 0 1 0 9.5 9.5A9.51 9.51 0 0 0 10 .5ZM9.5 4a1.5 1.5 0 1 1 0 3 1.5 1.5 0 0 1 0-3ZM12 15H8a1 1 0 0 1 0-2h1v-3H8a1 1 0 0 1 0-2h2a1 1 0 0 1 1 1v4h1a1 1 0 0 1 0 2Z",
    label: "Info icon",
  },
};

// ToastItem renders ONE toast with Flowbite's Toast markup.
export function ToastItem(props: { toast: ToastModel }) {
  const { toast } = props;
  const icon = VARIANT_ICON[toast.variant];
  const domId = `toast-${toast.id}`;
  return (
    <div
      id={domId}
      class="flex items-center w-full max-w-xs p-4 mb-4 text-gray-500 bg-white rounded-lg shadow-sm dark:text-gray-400 dark:bg-gray-800"
      role="alert"
      data-testid="toast"
      data-toast-variant={toast.variant}
    >
      <div
        class={`inline-flex items-center justify-center shrink-0 w-8 h-8 rounded-lg ${icon.chip}`}
      >
        <svg
          class="w-5 h-5"
          aria-hidden="true"
          xmlns="http://www.w3.org/2000/svg"
          fill="currentColor"
          viewBox="0 0 20 20"
        >
          <path d={icon.path} />
        </svg>
        <span class="sr-only">{icon.label}</span>
      </div>
      <div class="ms-3 text-sm font-normal" data-testid="toast-message">
        {toast.message}
      </div>
      <button
        type="button"
        class="ms-auto -mx-1.5 -my-1.5 border-0 bg-white text-gray-400 hover:text-gray-900 rounded-lg focus:ring-2 focus:ring-gray-300 p-1.5 hover:bg-gray-100 inline-flex items-center justify-center h-8 w-8 dark:text-gray-500 dark:hover:text-white dark:bg-gray-800 dark:hover:bg-gray-700"
        data-dismiss-target={`#${domId}`}
        data-testid="toast-dismiss"
        aria-label="Close"
        onClick={() => dismissToast(toast.id)}
      >
        <span class="sr-only">Close</span>
        <svg
          class="w-3 h-3"
          aria-hidden="true"
          xmlns="http://www.w3.org/2000/svg"
          fill="none"
          viewBox="0 0 14 14"
        >
          <path
            stroke="currentColor"
            stroke-linecap="round"
            stroke-linejoin="round"
            stroke-width="2"
            d="m1 1 6 6m0 0 6 6M7 7l6-6M7 7l-6 6"
          />
        </svg>
      </button>
    </div>
  );
}

// ToastContainer subscribes to the toast store and renders the live stack in
// a fixed bottom-right viewport region (Flowbite's documented toast
// positioning). Mounted ONCE in app.tsx so every screen shares one stack.
export function ToastContainer() {
  const [toasts, setToasts] = useState<readonly ToastModel[]>([]);
  useEffect(() => subscribeToasts(setToasts), []);
  if (toasts.length === 0) return null;
  return (
    <div
      class="fixed bottom-5 right-5 z-50 flex flex-col items-end"
      data-testid="toast-container"
      aria-live="polite"
    >
      {toasts.map((t) => (
        <ToastItem key={t.id} toast={t} />
      ))}
    </div>
  );
}
