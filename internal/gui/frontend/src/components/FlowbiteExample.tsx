import { useEffect, useRef } from "preact/hooks";
import { initFlowbite } from "flowbite";

// FlowbiteExample — the PROOF-OF-INTEGRATION reference component for using
// Flowbite inside this Preact app. It is NOT wired into any production screen;
// it exists so the next design lanes have a copy-paste-ready, working pattern.
// See docs/frontend-flowbite.md for the full narrative.
//
// ── Two kinds of Flowbite components ───────────────────────────────────────
//
//  1. STATIC (class-only) components — Badge, Button, Card, Alert, etc.
//     These are pure Tailwind utility classes baked into the markup. They need
//     NO JavaScript: just paste the classes from https://flowbite.com/docs and
//     they render. The Badge + Button below are this kind.
//
//  2. INTERACTIVE (data-attribute) components — Tooltip, Dropdown, Modal,
//     Drawer, Toast, Accordion, etc. These are driven by Flowbite's vanilla
//     JS, which it wires up by SCANNING THE DOM for `data-*` attributes
//     (e.g. `data-tooltip-target`, `data-modal-toggle`). That scan runs once,
//     when you call `initFlowbite()`. In a framework that renders the DOM
//     itself (Preact here), you must call `initFlowbite()` AFTER your markup
//     has mounted — exactly like Flowbite's documented Vue `onMounted` hook,
//     whose Preact equivalent is `useEffect(() => { initFlowbite(); }, [])`.
//     The tooltip below is this kind.
//
// ── The initFlowbite pattern for Preact ────────────────────────────────────
//
//   import { initFlowbite } from "flowbite";
//   useEffect(() => { initFlowbite(); }, []);   // run once after first mount
//
// If a screen RE-RENDERS new data-attribute markup later (e.g. a list of
// modals built from fetched data), call initFlowbite() again in an effect
// keyed on that data so the freshly-rendered triggers get wired:
//
//   useEffect(() => { initFlowbite(); }, [items]);
export function FlowbiteExample(): preact.JSX.Element {
  const ref = useRef<HTMLDivElement>(null);

  useEffect(() => {
    // initFlowbite scans the document for Flowbite data-attribute triggers and
    // attaches the interactive behavior (here: the tooltip below). It is a
    // no-op for markup that has no such attributes, so calling it is always
    // safe. We guard with typeof because the function is only meaningful in a
    // real DOM; the call is harmless under the happy-dom test environment too.
    if (typeof initFlowbite === "function") {
      initFlowbite();
    }
  }, []);

  return (
    <div ref={ref} class="flex flex-col gap-3" data-testid="flowbite-example">
      {/* STATIC component #1 — Flowbite Badge (class-only, no JS). */}
      <span
        class="bg-blue-100 text-blue-800 text-xs font-medium me-2 px-2.5 py-0.5 rounded-sm dark:bg-blue-900 dark:text-blue-300"
        data-testid="flowbite-badge"
      >
        Flowbite enabled
      </span>

      {/* STATIC component #2 — Flowbite Button (class-only, no JS). */}
      <button
        type="button"
        class="text-white bg-blue-700 hover:bg-blue-800 focus:ring-4 focus:ring-blue-300 font-medium rounded-lg text-sm px-5 py-2.5 focus:outline-none dark:bg-blue-600 dark:hover:bg-blue-700 dark:focus:ring-blue-800"
        data-testid="flowbite-button"
      >
        Flowbite Button
      </button>

      {/* INTERACTIVE component — Flowbite Tooltip (driven by initFlowbite()).
          The trigger declares `data-tooltip-target="<id>"`; the tooltip body
          is a sibling element with that id + `role="tooltip"`. initFlowbite()
          (in the effect above) finds the trigger by its data attribute and
          shows/hides the body on hover/focus. */}
      <button
        type="button"
        data-tooltip-target="flowbite-tooltip"
        class="text-white bg-blue-700 hover:bg-blue-800 focus:ring-4 focus:ring-blue-300 font-medium rounded-lg text-sm px-5 py-2.5 focus:outline-none dark:bg-blue-600 dark:hover:bg-blue-700 dark:focus:ring-blue-800"
        data-testid="flowbite-tooltip-trigger"
      >
        Hover for a Flowbite tooltip
      </button>
      <div
        id="flowbite-tooltip"
        role="tooltip"
        class="absolute z-10 invisible inline-block px-3 py-2 text-sm font-medium text-white bg-gray-900 rounded-lg shadow-xs opacity-0 tooltip dark:bg-gray-700"
      >
        Wired by initFlowbite()
        <div class="tooltip-arrow" data-popper-arrow></div>
      </div>
    </div>
  );
}
